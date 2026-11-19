//go:build unix

// Package pid1 提供看守进程作为 PID 1 时的子进程回收器。
//
// 看守进程只在进程号是 1 时启用它；本地模式下看守进程不是 PID 1，改用
// Claude Code 驱动提供的 exec.Cmd.Wait 实现。
//
// 构造与启用：
//
//	r := pid1.New(logger) // logger 为 nil 时使用 slog.Default()
//	r.Enable()            // 安装 SIGCHLD 处理并开始回收；重复调用只生效一次
//
// Enable 必须在启动任何子进程之前调用。启用后，回收循环运行到进程退出：
// 它是本进程唯一调用 wait4 的组件。r.Start 满足 agent.ProcessRunner 的方法集，
// 经它启动的进程，退出状态送达对应的通道；其余子进程被回收后丢弃并记日志。
// Start 与登记 pid 在同一把锁内完成，回收循环不会在登记之前收走该进程。
package pid1

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// ErrNotEnabled 表示回收器尚未启用，此时不能启动需要退出状态的子进程。
var ErrNotEnabled = errors.New("pid1: reaper is not enabled")

// ExitError 描述经 Start 启动的进程以非零状态或信号结束。
type ExitError struct {
	// Pid 是进程号。
	Pid int
	// Status 是 wait4 返回的等待状态。
	Status syscall.WaitStatus
}

// Error 返回形如 "pid 123: exit status 7" 或 "pid 123: signal: killed" 的描述。
func (e *ExitError) Error() string {
	return fmt.Sprintf("pid %d: %s", e.Pid, describeStatus(e.Status))
}

// describeStatus 把 wait4 的等待状态写成与 exec.ExitError 相近的文字。
func describeStatus(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return fmt.Sprintf("exit status %d", status.ExitStatus())
	case status.Signaled():
		return fmt.Sprintf("signal: %v", status.Signal())
	default:
		return fmt.Sprintf("wait status %#x", uint32(status))
	}
}

// Reaper 回收本进程的全部子进程，并把经 Start 启动的进程的退出状态交给调用方。
type Reaper struct {
	logger *slog.Logger

	mu      sync.Mutex
	enabled bool
	procs   map[int]chan error
}

// New 构造一个回收器。logger 为 nil 时使用 slog.Default()。
// 回收器在 Enable 之前处于未启用状态。
func New(logger *slog.Logger) *Reaper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reaper{
		logger: logger,
		procs:  make(map[int]chan error),
	}
}

// Enable 安装 SIGCHLD 处理并启动回收循环。它只在第一次调用时生效，
// 回收循环运行到进程退出，因此必须在启动任何子进程之前调用。
func (r *Reaper) Enable() {
	r.mu.Lock()
	if r.enabled {
		r.mu.Unlock()
		return
	}
	r.enabled = true
	r.mu.Unlock()

	// 先注册信号再清扫一次，避免在注册之前退出的子进程一直留作僵尸。
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	go r.loop(signals)
}

// Start 启动 cmd，并返回一个在进程退出时收到一次退出结果的通道。
// 退出结果为 nil 表示正常退出，否则为 *ExitError。
// 调用方不得再调用 cmd.Wait；cmd 的标准输入输出流只能是 *os.File 或 nil。
// 回收器未启用时返回 ErrNotEnabled，且不会启动 cmd。
func (r *Reaper) Start(cmd *exec.Cmd) (<-chan error, error) {
	if cmd == nil {
		return nil, errors.New("pid1: nil command")
	}

	// 启动与登记必须在同一把锁内：回收循环在 wait4 之后也要取得这把锁
	// 才能查表，因此不会在登记之前收走该进程。
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return nil, ErrNotEnabled
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	exited := make(chan error, 1)
	r.procs[cmd.Process.Pid] = exited
	return exited, nil
}

// loop 在收到 SIGCHLD 时清扫全部已退出的子进程。
func (r *Reaper) loop(signals <-chan os.Signal) {
	r.reap()
	for range signals {
		r.reap()
	}
}

// reap 反复调用 wait4 直到没有可回收的子进程。
func (r *Reaper) reap() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return
		case err != nil:
			r.logger.Error("pid1: wait4 failed", "error", err)
			return
		case pid == 0:
			return
		}
		r.finish(pid, status)
	}
}

// finish 把退出状态交给对应的通道；没有登记的进程回收后丢弃并记日志。
func (r *Reaper) finish(pid int, status syscall.WaitStatus) {
	r.mu.Lock()
	exited, registered := r.procs[pid]
	delete(r.procs, pid)
	r.mu.Unlock()

	if !registered {
		r.logger.Info("pid1: reaped unregistered child", "pid", pid, "status", describeStatus(status))
		return
	}

	var result error
	if !status.Exited() || status.ExitStatus() != 0 {
		result = &ExitError{Pid: pid, Status: status}
	}
	exited <- result
	close(exited)
}

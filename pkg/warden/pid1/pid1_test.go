//go:build linux

package pid1_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/warden/pid1"
)

// helperEnv 把重新执行的测试二进制标记为替身 PID 1。
const helperEnv = "CASTWORKS_PID1_TEST_HELPER"

// TestReaper 在普通测试进程里重新执行自己，让子进程以
// PR_SET_CHILD_SUBREAPER 模拟 PID 1，再在子进程里运行全部场景。
// 真实的 PID 1 无法在测试里创建，子进程的回收语义与它一致。
func TestReaper(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		runSubreaperScenarios(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestReaper$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("替身 PID 1 子进程失败: %v\n%s", err, out)
	}
	t.Logf("替身 PID 1 子进程输出:\n%s", out)
}

// TestStartRequiresEnable 确认未启用时 Start 拒绝启动进程。
func TestStartRequiresEnable(t *testing.T) {
	r := pid1.New(nil)
	if _, err := r.Start(exec.Command("/bin/true")); !errors.Is(err, pid1.ErrNotEnabled) {
		t.Fatalf("Start 在启用前返回 %v，期望 ErrNotEnabled", err)
	}
}

func runSubreaperScenarios(t *testing.T) {
	if err := setChildSubreaper(); err != nil {
		t.Fatalf("PR_SET_CHILD_SUBREAPER: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := pid1.New(logger)
	r.Enable()

	t.Run("orphan", func(t *testing.T) {
		testOrphanReaped(t, r)
	})
	t.Run("concurrent", func(t *testing.T) {
		testConcurrentExitStatus(t, r)
	})
}

// setChildSubreaper 让当前进程接收孤儿后代，行为与 PID 1 相同。
func setChildSubreaper() error {
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_PRCTL,
		uintptr(syscall.PR_SET_CHILD_SUBREAPER),
		1, 0, 0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// testOrphanReaped 让孙进程在父进程退出后成为孤儿，确认回收器收走它，
// 不留下僵尸。
func testOrphanReaped(t *testing.T, r *pid1.Reaper) {
	stdout, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = stdout.Close() }()

	cmd := exec.Command("/bin/sh", "-c", "sleep 0.3 & echo $!")
	cmd.Stdout = stdoutWrite
	exited, err := r.Start(cmd)
	_ = stdoutWrite.Close()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("读取孙进程 pid: %v", err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("解析孙进程 pid %q: %v", line, err)
	}

	if err := waitExit(t, exited, 5*time.Second); err != nil {
		t.Fatalf("子进程退出: %v", err)
	}

	// 孙进程在子进程退出后被过继给替身 PID 1。回收器若正常工作，
	// sleep 结束时会连同僵尸一起收走，/proc/<pid> 随之消失。
	deadline := time.Now().Add(10 * time.Second)
	path := fmt.Sprintf("/proc/%d", grandchild)
	for {
		_, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		if time.Now().After(deadline) {
			state := "unknown"
			if raw, readErr := os.ReadFile(path + "/stat"); readErr == nil {
				state = procState(string(raw))
			}
			t.Fatalf("孙进程 %d 仍是 %s，孤儿僵尸没有被回收", grandchild, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// testConcurrentExitStatus 并发启动大量短命进程，确认每个登记进程的退出状态
// 都送到自己的通道，没有一个被回收循环吞掉。
func testConcurrentExitStatus(t *testing.T, r *pid1.Reaper) {
	const rounds = 5
	const procs = 200

	for round := 0; round < rounds; round++ {
		start := time.Now()
		var wg sync.WaitGroup
		errs := make(chan error, procs)

		for i := 0; i < procs; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				want := 0
				if i%2 == 1 {
					want = 7
				}
				cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", want))
				exited, err := r.Start(cmd)
				if err != nil {
					errs <- fmt.Errorf("进程 %d: Start: %w", i, err)
					return
				}
				select {
				case err := <-exited:
					if err := checkExit(cmd.Process.Pid, want, err); err != nil {
						errs <- fmt.Errorf("进程 %d: %w", i, err)
						return
					}
				case <-time.After(30 * time.Second):
					errs <- fmt.Errorf("进程 %d: 30 s 内没有送达退出状态", i)
					return
				}
				select {
				case _, ok := <-exited:
					if ok {
						errs <- fmt.Errorf("进程 %d: 退出状态送达后通道未关闭", i)
					}
				case <-time.After(time.Second):
					errs <- fmt.Errorf("进程 %d: 退出状态送达后通道未关闭", i)
				}
			}(i)
		}

		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if t.Failed() {
			return
		}
		t.Logf("第 %d 轮: %d 个进程的退出状态全部送达，耗时 %s", round, procs, time.Since(start))
	}
}

// checkExit 核对送达的退出结果与进程实际退出码一致。
func checkExit(pid, want int, err error) error {
	if want == 0 {
		if err != nil {
			return fmt.Errorf("期望正常退出，得到 %v", err)
		}
		return nil
	}

	var exitErr *pid1.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("期望 *pid1.ExitError，得到 %v", err)
	}
	if exitErr.Pid != pid {
		return fmt.Errorf("期望 pid %d，得到 %d", pid, exitErr.Pid)
	}
	if !exitErr.Status.Exited() || exitErr.Status.ExitStatus() != want {
		return fmt.Errorf("期望退出码 %d，得到 %v", want, exitErr.Status)
	}
	return nil
}

func waitExit(t *testing.T, exited <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-exited:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("%s 内没有送达退出状态", timeout)
	}
}

// procState 从 /proc/<pid>/stat 中取出状态字母。进程名可能含空格与括号，
// 因此从最后一个右括号之后取。
func procState(stat string) string {
	i := strings.LastIndex(stat, ")")
	if i < 0 || i+2 >= len(stat) {
		return "unknown"
	}
	return string(stat[i+2])
}

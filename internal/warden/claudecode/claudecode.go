// Package claudecode 是看守进程内的 Claude Code 驱动器。它以 stream-json
// 驱动一个跨回合常驻的 Claude Code 进程：把投递写入 stdin，从 stdout 识别
// 回合的开始与结束，并经 agent.Sink 报告事件。
//
// 对外接口：
//
//	func New(cfg Config, sink agent.Sink, runner agent.ProcessRunner) *Driver
//	func (d *Driver) Snapshot(fn func(agent.State))
//	func (d *Driver) Deliver(b agent.Batch, cred agent.Credential) error
//	func (d *Driver) Shutdown(ctx context.Context) error
//	type DefaultProcessRunner struct{} // 默认的 agent.ProcessRunner
//
// Config 至少包括 Claude Code 可执行文件路径、主目录与关停等待时长；其余
// 字段用零值取默认值。New 不启动进程，第一条 Deliver 才启动。
//
// 启动命令为
//
//	claude -p --output-format stream-json --input-format stream-json --verbose --dangerously-skip-permissions
//
// 注入的环境变量只有 warden.md 表中列出的变量与 PATH：actor 模式注入镜像
// 里的固定 PATH（与 images/actor/Dockerfile 的 ENV PATH 逐字一致），本地
// 模式注入看守进程启动时的 PATH。看守进程自己的其他环境变量不传给 Claude
// Code。
//
// 工作目录为 <主目录>/work，stdout 每行原样追加到
// <主目录>/.castworks/stream.jsonl 后再解析。目录不存在时以 0700 创建。
package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wangzw/castworks/internal/warden/agent"
)

// actorPATH 是 actor 镜像里的固定 PATH，必须与 images/actor/Dockerfile 的
// ENV PATH 逐字一致。
const actorPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// maxOutputBytes 是回合输出保留的上限，1 MiB。
const maxOutputBytes = 1 << 20

// stdoutEOFWait 是进程退出后等待 stdout 读到 EOF 的上限。超时说明 stdout
// 仍被别的进程持有，此时关闭读端并照常报告退出。
const stdoutEOFWait = 2 * time.Second

// claudeArgs 是启动 Claude Code 的固定参数。
var claudeArgs = []string{
	"-p",
	"--output-format", "stream-json",
	"--input-format", "stream-json",
	"--verbose",
	"--dangerously-skip-permissions",
}

// Config 是驱动器的配置。ClaudeBin 与 Home 必填。
type Config struct {
	// ClaudeBin 是 Claude Code 可执行文件路径。
	ClaudeBin string
	// Home 是 Claude Code 运行用户的主目录。工作目录为 <Home>/work，
	// stream-json 记录写入 <Home>/.castworks/stream.jsonl。
	Home string
	// ActorMode 为真时注入镜像里的固定 PATH；为假时注入 LocalPATH。
	ActorMode bool
	// LocalPATH 是本地模式注入的 PATH；空值取 New 时的 os.Getenv("PATH")。
	LocalPATH string
	// StdinCloseWait 是关闭 stdin 后等待进程退出的时长，默认 10 s。
	StdinCloseWait time.Duration
	// TerminateWait 是发送 SIGTERM 后等待进程退出的时长，默认 5 s。
	TerminateWait time.Duration
	// Stderr 接收 Claude Code 的 stderr，默认 os.Stderr。
	Stderr *os.File
	// Logger 接收日志，默认 slog.Default()。
	Logger *slog.Logger
}

// pendingStart 是已经生成 turn_id、但尚未取得 session_id 的回合。
type pendingStart struct {
	turnID     string
	deliveryID string
}

// Driver 实现 agent.Agent。
type Driver struct {
	cfg    Config
	sink   agent.Sink
	runner agent.ProcessRunner
	log    *slog.Logger

	mu              sync.Mutex
	phase           agent.Phase
	turnID          string
	lastTurn        *agent.TurnResult
	lastAcceptedSeq int64
	sessionID       string
	pendingStart    *pendingStart

	// 当前回合的边界信号。
	idleSeen      bool
	resultSeen    bool
	resultOutput  string
	resultIsError bool
	resultQueued  int

	// stdinMu 串行化写入，不持状态锁；stdin 指针由 mu 保护，写入本身在锁外
	// 进行，Shutdown 因而不必等一次阻塞的写入就能关闭 stdin。
	stdinMu sync.Mutex
	stdin   *os.File

	// writeInFlight 表示有一批投递正在状态锁外写 stdin。这期间读协程到达
	// 的回合边界与进程退出都不立即处理：边界信号留到 TurnStarted 发出之后
	// 判定，进程退出留到写入结束之后报告，保证每个 TurnEnded 之前都有同一
	// turn_id 的 TurnStarted。Deliver 由调用方串行调用。
	writeInFlight bool
	// pendingExit 是写 stdin 期间观察到的进程退出状态，等写入结束后报告。
	pendingExit *string

	stdout       *os.File
	cmd          *exec.Cmd
	shuttingDown bool
	exitDone     chan struct{}
	// stdoutDone 在 readStdout 读到 EOF 后关闭。
	stdoutDone chan struct{}

	streamMu   sync.Mutex
	streamFile *os.File
}

var (
	_ agent.Agent         = (*Driver)(nil)
	_ agent.ProcessRunner = DefaultProcessRunner{}
)

// New 构造驱动器。runner 为 nil 时使用 DefaultProcessRunner。New 不启动
// Claude Code。
func New(cfg Config, sink agent.Sink, runner agent.ProcessRunner) *Driver {
	if cfg.StdinCloseWait <= 0 {
		cfg.StdinCloseWait = 10 * time.Second
	}
	if cfg.TerminateWait <= 0 {
		cfg.TerminateWait = 5 * time.Second
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.LocalPATH == "" {
		cfg.LocalPATH = os.Getenv("PATH")
	}
	if runner == nil {
		runner = DefaultProcessRunner{}
	}
	return &Driver{
		cfg:        cfg,
		sink:       sink,
		runner:     runner,
		log:        cfg.Logger,
		phase:      agent.PhaseStarting,
		exitDone:   make(chan struct{}),
		stdoutDone: make(chan struct{}),
	}
}

// DefaultProcessRunner 以 cmd.Start 与 cmd.Wait 启动并等待子进程，供本地模式
// 与测试使用。
type DefaultProcessRunner struct{}

// Start 实现 agent.ProcessRunner。
func (DefaultProcessRunner) Start(cmd *exec.Cmd) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
		close(exited)
	}()
	return exited, nil
}

// Snapshot 实现 agent.Agent。
func (d *Driver) Snapshot(fn func(agent.State)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := agent.State{
		Phase:           d.phase,
		TurnID:          d.turnID,
		LastAcceptedSeq: d.lastAcceptedSeq,
	}
	if d.phase != agent.PhaseInTurn {
		state.TurnID = ""
	}
	if d.lastTurn != nil {
		last := *d.lastTurn
		state.LastTurn = &last
	}
	fn(state)
}

// Deliver 实现 agent.Agent。写 stdin 不持状态锁：读 stdout 的协程不会因写入
// 阻塞，写入本身由 stdinMu 串行化。
func (d *Driver) Deliver(b agent.Batch, cred agent.Credential) error {
	d.mu.Lock()

	if d.shuttingDown || d.phase == agent.PhaseExited {
		d.mu.Unlock()
		return agent.ErrExited
	}
	if d.phase == agent.PhaseStarting {
		if err := d.startLocked(cred); err != nil {
			d.exitOutsideTurnLocked(err.Error())
			d.mu.Unlock()
			return fmt.Errorf("claudecode: start: %w", err)
		}
	}

	line, err := encodeUserMessage(b.Texts)
	if err != nil {
		d.exitOutsideTurnLocked(err.Error())
		d.mu.Unlock()
		return err
	}

	newTurn := false
	if d.phase == agent.PhaseStarting || d.phase == agent.PhaseBoundary {
		d.beginTurnLocked()
		newTurn = true
	}
	// 写 stdin 在状态锁之外进行。期间读协程可能收齐回合边界，进程也可能
	// 退出；两者都不在写入进行中处理。
	d.writeInFlight = true
	d.mu.Unlock()

	_, writeErr := d.writeStdin(line)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeInFlight = false
	if writeErr != nil {
		if d.shuttingDown {
			d.phase = agent.PhaseExited
			d.turnID = ""
			d.pendingStart = nil
			d.pendingExit = nil
			return fmt.Errorf("claudecode: write stdin: %w", writeErr)
		}
		if newTurn {
			if pending := d.takePendingExitLocked(); pending != "" {
				// 进程在写入窗口里退出。这批投递没有写入成功，也不会被
				// 接受，因此不能带着它的 delivery_id 开始回合：按回合外
				// 退出补一对 delivery_id 为空的 TurnStarted 与 TurnEnded。
				d.exitOutsideTurnLocked(pending)
			} else {
				// 本批消息是本回合的第一批（turn_started 尚未发出），按
				// 回合外退出补一个回合。
				d.exitOutsideTurnLocked(writeErr.Error())
			}
		} else {
			// 回合已在进行，写入失败使回合以 agent_exited 结束。
			status := writeErr.Error()
			if pending := d.takePendingExitLocked(); pending != "" {
				status = pending
			}
			d.exitInTurnLocked(status)
		}
		return fmt.Errorf("claudecode: write stdin: %w", writeErr)
	}

	if d.shuttingDown || d.phase == agent.PhaseExited {
		// 写入期间开始了关停，或进程已退出、不会回 Accepted：不再调用
		// Sink，也不推进 last_accepted_seq。
		d.pendingStart = nil
		d.turnID = ""
		d.pendingExit = nil
		return agent.ErrExited
	}
	d.lastAcceptedSeq = b.ThroughSeq
	d.sink.Accepted(b.DeliveryID, b.ThroughSeq)
	if newTurn {
		d.announceTurnStartLocked(b.DeliveryID)
	} else if d.phase == agent.PhaseInTurn {
		// 回合进行中投递写入成功，已收到的边界信号作废，必须重新到齐。
		d.clearBoundarySignalsLocked()
	}
	// 写入期间收齐的边界信号与进程退出在 TurnStarted 之后判定。先按边界
	// 结束回合，再报告缓冲的退出：边界已经到齐的回合按正常结果结束，进程
	// 退出只作为回合外退出单独报告。
	d.maybeEndTurnLocked()
	if pending := d.takePendingExitLocked(); pending != "" {
		if d.phase == agent.PhaseInTurn {
			d.exitInTurnLocked(pending)
		} else {
			d.exitOutsideTurnLocked(pending)
		}
	}
	d.log.Info("claudecode: delivered",
		"delivery_id", b.DeliveryID,
		"through_seq", b.ThroughSeq,
		"messages", len(b.Texts),
		"bytes", len(line),
		"new_turn", newTurn)
	return nil
}

// Shutdown 实现 agent.Agent：关闭 stdin，等待配置的时长，仍未退出时依次
// 发送 SIGTERM 与 SIGKILL。
func (d *Driver) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if d.shuttingDown {
		// 并发的第二次调用等进程退出后再返回。
		cmd := d.cmd
		done := d.exitDone
		d.mu.Unlock()
		if cmd == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.shuttingDown = true
	if d.phase == agent.PhaseExited || d.phase == agent.PhaseStarting {
		d.mu.Unlock()
		d.closeStream()
		return nil
	}
	cmd := d.cmd
	done := d.exitDone
	closeWait := d.cfg.StdinCloseWait
	termWait := d.cfg.TerminateWait
	d.mu.Unlock()

	d.closeStdin()

	exited, err := waitForExit(ctx, done, closeWait)
	if err != nil {
		return err
	}
	if !exited {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		exited, err = waitForExit(ctx, done, termWait)
		if err != nil {
			return err
		}
	}
	if !exited {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-ctx.Done():
			d.closeStream()
			return ctx.Err()
		}
	}

	d.mu.Lock()
	d.phase = agent.PhaseExited
	d.turnID = ""
	d.pendingStart = nil
	d.mu.Unlock()
	d.closeStream()
	return nil
}

// startLocked 创建工作目录与管道，经 ProcessRunner 启动 Claude Code，并开始
// 读取 stdout 与等待退出。调用方必须持有 d.mu。
func (d *Driver) startLocked(cred agent.Credential) error {
	workDir := filepath.Join(d.cfg.Home, "work")
	stateDir := filepath.Join(d.cfg.Home, ".castworks")
	for _, dir := range []string{workDir, stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	stream, err := os.OpenFile(filepath.Join(stateDir, "stream.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open stream.jsonl: %w", err)
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		_ = stream.Close()
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stream.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	cmd := exec.Command(d.cfg.ClaudeBin, claudeArgs...)
	cmd.Dir = workDir
	cmd.Env = d.environment(cred)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = d.cfg.Stderr

	exited, err := d.runner.Start(cmd)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stream.Close()
		return err
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()

	d.stdin = stdinW
	d.stdout = stdoutR
	d.cmd = cmd
	d.streamMu.Lock()
	d.streamFile = stream
	d.streamMu.Unlock()

	go d.readStdout(stdoutR)
	go d.watchExit(exited)
	return nil
}

// environment 返回注入 Claude Code 的完整环境变量集合。
func (d *Driver) environment(cred agent.Credential) []string {
	path := d.cfg.LocalPATH
	if d.cfg.ActorMode {
		path = actorPATH
	}
	env := []string{
		"HOME=" + d.cfg.Home,
		"PATH=" + path,
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
	}
	switch cred.Kind {
	case agent.CredentialAPIKey:
		env = append(env, "ANTHROPIC_API_KEY="+cred.Value)
	case agent.CredentialOAuthToken:
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+cred.Value)
	}
	return env
}

// encodeUserMessage 把一批文本以空行连接，编码为一行 stream-json 用户消息。
func encodeUserMessage(texts []string) ([]byte, error) {
	payload := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	payload.Message.Role = "user"
	payload.Message.Content = strings.Join(texts, "\n\n")

	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("claudecode: encode user message: %w", err)
	}
	return []byte(buf.String()), nil
}

// writeStdin 写完整一行，由 stdinMu 串行化，不持状态锁。
func (d *Driver) writeStdin(line []byte) (int, error) {
	d.stdinMu.Lock()
	defer d.stdinMu.Unlock()
	d.mu.Lock()
	stdin := d.stdin
	d.mu.Unlock()
	if stdin == nil {
		return 0, errors.New("claudecode: stdin is not open")
	}
	written := 0
	for written < len(line) {
		n, err := stdin.Write(line[written:])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// closeStdin 关闭 stdin，不等进行中的写入：关闭同一个 *os.File 会让阻塞的
// 写入返回错误，关停因此能继续升级。
func (d *Driver) closeStdin() {
	d.mu.Lock()
	stdin := d.stdin
	d.stdin = nil
	d.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

// beginTurnLocked 生成新的 turn_id，进入 PhaseInTurn，并清除上一回合的边界
// 信号。调用方必须持有 d.mu。
func (d *Driver) beginTurnLocked() {
	d.turnID = newTurnID()
	d.phase = agent.PhaseInTurn
	d.pendingStart = nil
	d.clearBoundarySignalsLocked()
}

// clearBoundarySignalsLocked 清掉当前回合已收到的边界信号。调用方必须持有
// d.mu。
func (d *Driver) clearBoundarySignalsLocked() {
	d.idleSeen = false
	d.resultSeen = false
	d.resultOutput = ""
	d.resultIsError = false
	d.resultQueued = 0
}

// announceTurnStartLocked 发出 turn_started；session_id 尚未取得时延后到取得
// 之后。调用方必须持有 d.mu。
func (d *Driver) announceTurnStartLocked(deliveryID string) {
	if d.sessionID != "" {
		d.sink.TurnStarted(d.turnID, deliveryID, d.sessionID)
		return
	}
	d.pendingStart = &pendingStart{turnID: d.turnID, deliveryID: deliveryID}
}

// flushPendingTurnStartLocked 以当前 session_id（可能为空）补发延后的
// turn_started。调用方必须持有 d.mu。
func (d *Driver) flushPendingTurnStartLocked() {
	if d.pendingStart == nil {
		return
	}
	pending := d.pendingStart
	d.pendingStart = nil
	d.sink.TurnStarted(pending.turnID, pending.deliveryID, d.sessionID)
}

// exitOutsideTurnLocked 按回合外退出补一个回合：turn_started（delivery_id 为
// 空）紧接 turn_ended（agent_exited=true），阶段转 PhaseExited。调用方必须
// 持有 d.mu。
func (d *Driver) exitOutsideTurnLocked(status string) {
	if d.phase == agent.PhaseExited {
		return
	}
	d.pendingStart = nil
	d.turnID = ""
	d.phase = agent.PhaseExited
	if d.shuttingDown {
		return
	}
	turnID := newTurnID()
	d.sink.TurnStarted(turnID, "", d.sessionID)
	d.sink.TurnEnded(turnID, exitOutput(status), true, true)
}

// exitInTurnLocked 结束进行中的回合，agent_exited=true。调用方必须持有 d.mu。
func (d *Driver) exitInTurnLocked(status string) {
	if d.phase == agent.PhaseExited {
		return
	}
	turnID := d.turnID
	if d.shuttingDown {
		d.pendingStart = nil
		d.turnID = ""
		d.phase = agent.PhaseExited
		return
	}
	d.flushPendingTurnStartLocked()
	d.sink.TurnEnded(turnID, exitOutput(status), true, true)
	d.phase = agent.PhaseExited
	d.turnID = ""
}

// watchExit 等待子进程退出，并在未开始关停时报告事件。调用方不得再调用
// cmd.Wait。
func (d *Driver) watchExit(exited <-chan error) {
	err := <-exited
	// 先等 readStdout 读到 EOF，避免已输出完整边界的回合被报成 agent_exited。
	// stdout 被后台子进程持有时最多等 stdoutEOFWait。
	d.waitStdoutEOF()

	d.mu.Lock()
	if !d.shuttingDown {
		// 写 stdin 正在进行时只记下退出，等写入结束后由 Deliver 报告。
		d.handleProcessExitLocked(exitStatus(err))
	}
	d.mu.Unlock()
	// 关闭 stdin 会唤醒可能阻塞的写入；放在持锁判定之后，写入方一定能
	// 看到已经记录的退出状态。
	d.closeStdin()
	close(d.exitDone)
}

// waitStdoutEOF 等待 readStdout 读到 EOF，最多 stdoutEOFWait。超时后关闭
// stdout 读端，不再等待读取协程结束。
func (d *Driver) waitStdoutEOF() {
	timer := time.NewTimer(stdoutEOFWait)
	defer timer.Stop()
	select {
	case <-d.stdoutDone:
		return
	case <-timer.C:
	}
	d.mu.Lock()
	stdout := d.stdout
	d.stdout = nil
	d.mu.Unlock()
	if stdout != nil {
		_ = stdout.Close()
	}
}

// handleProcessExitLocked 处理未经 Shutdown 的子进程退出。写 stdin 期间
// 只记录退出状态，等 Deliver 发完 Accepted 与 TurnStarted 再处理。调用方
// 必须持有 d.mu。
func (d *Driver) handleProcessExitLocked(status string) {
	if d.writeInFlight {
		d.pendingExit = &status
		return
	}
	switch d.phase {
	case agent.PhaseExited:
		return
	case agent.PhaseInTurn:
		d.exitInTurnLocked(status)
	default:
		d.exitOutsideTurnLocked(status)
	}
}

// exitStatus 把 ProcessRunner 的退出结果描述成一段文本。
func exitStatus(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// exitOutput 是退出时 turn_ended 的输出。
func exitOutput(status string) string {
	return "claude code exited: " + status
}

// takePendingExitLocked 取走写 stdin 期间记录的退出状态，没有时返回空串。
// 调用方必须持有 d.mu。
func (d *Driver) takePendingExitLocked() string {
	if d.pendingExit == nil {
		return ""
	}
	status := *d.pendingExit
	d.pendingExit = nil
	return status
}

// waitForExit 等待 done 关闭，最多 d；ctx 取消时返回它的错误。
func waitForExit(ctx context.Context, done <-chan struct{}, d time.Duration) (bool, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true, nil
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// closeStream 关闭 stream.jsonl，之后的写入被丢弃。
func (d *Driver) closeStream() {
	d.streamMu.Lock()
	file := d.streamFile
	d.streamFile = nil
	d.streamMu.Unlock()
	if file != nil {
		_ = file.Close()
	}
}

// newTurnID 返回 16 字节 crypto/rand 的小写十六进制。
func newTurnID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand 不可用时无法生成唯一的回合标识，继续运行只会让控制面
		// 收到重复的 turn_id。
		panic("claudecode: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

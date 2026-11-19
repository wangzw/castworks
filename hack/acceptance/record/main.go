// Command record 直接以 stream-json 驱动真实的 Claude Code，录制
// queued_turn_count 大于 0 的样本。
//
// 控制面只在回合边界投递，经控制面无法在回合中写入 stdin，所以
// queued_turn_count 大于 0 的情形要在回合进行中写入第二条用户消息才能
// 产生。本地模式验收没有产生这种样本时，在开发机上运行本程序补录；它不
// 属于验收测试二进制，也不在镜像里。
//
// 用法：
//
//	go run ./hack/acceptance/record \
//		-claude-bin <install-claude.sh 输出的路径> \
//		-credential-file <凭据文件> \
//		-out <名>.jsonl
//
// 凭据文件只用来取出令牌，放进子进程的 CLAUDE_CODE_OAUTH_TOKEN 环境变量。
// 程序不输出、不落盘、不写日志记录令牌。输出文件是 Claude Code 的 stdout
// 原样行，与 <主目录>/.castworks/stream.jsonl 的格式相同。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultFirst  = "Run `sleep 30` in the foreground with the Bash tool, then reply with the single word done."
	defaultSecond = "When the sleep finishes, reply with the single word queued."

	defaultTimeout = 6 * time.Minute
	stdinCloseWait = 10 * time.Second
	terminateWait  = 5 * time.Second
)

// claudeArgs 与看守进程的 Claude Code 驱动器一致。
var claudeArgs = []string{
	"-p",
	"--output-format", "stream-json",
	"--input-format", "stream-json",
	"--verbose",
	"--dangerously-skip-permissions",
}

type config struct {
	claudeBin      string
	credentialFile string
	out            string
	home           string
	first          string
	second         string
	timeout        time.Duration
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 解析参数并录制，返回进程退出码：成功为 0，参数错误为 2，其余为 1。
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "record：%v\n\n", err)
		usage(stderr)
		return 2
	}
	if err := record(cfg, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "record：%v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "已写入 %s\n", cfg.out)
	return 0
}

func parseConfig(args []string) (*config, error) {
	cfg := &config{}
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.claudeBin, "claude-bin", "", "Claude Code 可执行文件路径（必填）")
	fs.StringVar(&cfg.credentialFile, "credential-file", "", "凭据文件路径（必填）")
	fs.StringVar(&cfg.out, "out", "", "输出文件，Claude Code 的 stdout 原样行（必填）")
	fs.StringVar(&cfg.home, "home", "", "主目录；默认在临时目录下新建")
	fs.StringVar(&cfg.first, "first", defaultFirst, "第一条用户消息")
	fs.StringVar(&cfg.second, "second", defaultSecond, "回合进行中写入的第二条用户消息")
	fs.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "整个录制的上限")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("不接受位置参数，收到 %d 个", fs.NArg())
	}
	switch {
	case cfg.claudeBin == "":
		return nil, errors.New("-claude-bin 必填")
	case cfg.credentialFile == "":
		return nil, errors.New("-credential-file 必填")
	case cfg.out == "":
		return nil, errors.New("-out 必填")
	case cfg.first == "":
		return nil, errors.New("-first 不能为空")
	case cfg.second == "":
		return nil, errors.New("-second 不能为空")
	case cfg.timeout <= 0:
		return nil, errors.New("-timeout 必须大于 0")
	}
	return cfg, nil
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `record 直接以 stream-json 驱动 Claude Code，录制 queued_turn_count 大于 0 的样本。

用法：
  record -claude-bin <路径> -credential-file <路径> -out <文件> [选项]

选项：
  -claude-bin         Claude Code 可执行文件路径（必填）
  -credential-file    凭据文件路径（必填），只读取令牌
  -out                输出文件（必填），Claude Code 的 stdout 原样行
  -home               主目录，默认在临时目录下新建
  -first              第一条用户消息
  -second             回合进行中写入的第二条用户消息
  -timeout            整个录制的上限，默认 6m

程序先写入第一条消息，等回合开始后在回合进行中写入第二条消息，使
Claude Code 把它排队，从而在 result 中得到 queued_turn_count 大于 0。
录制结果写进 -out，格式与 stream.jsonl 相同。
`)
}

// prepareHome 返回录制使用的主目录与清理函数。configured 为空时在临时
// 目录下新建主目录，清理函数删除它；调用者指定目录时清理函数不删除。
func prepareHome(configured string) (string, func(), error) {
	if configured != "" {
		return configured, func() {}, nil
	}
	home, err := os.MkdirTemp("", "castworks-record-")
	if err != nil {
		return "", nil, fmt.Errorf("创建主目录：%w", err)
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}

// record 启动 Claude Code、写入两条用户消息，并把 stdout 原样写进输出文件。
func record(cfg *config, stderr io.Writer) error {
	token, err := readToken(cfg.credentialFile)
	if err != nil {
		return err
	}

	home, cleanupHome, err := prepareHome(cfg.home)
	if err != nil {
		return err
	}
	// 临时主目录里会有 Claude Code 的会话与凭据环境，录制结束就删除；
	// 调用者用 -home 指定的目录不动。killGroup 的 defer 在这之后注册，
	// 按后进先出先结束子进程，再删目录。
	defer cleanupHome()
	workDir := filepath.Join(home, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("创建工作目录：%w", err)
	}

	out, err := os.OpenFile(cfg.out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开输出文件：%w", err)
	}
	defer func() { _ = out.Close() }()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("创建 stdin 管道：%w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return fmt.Errorf("创建 stdout 管道：%w", err)
	}

	cmd := exec.Command(cfg.claudeBin, claudeArgs...)
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_OAUTH_TOKEN=" + token,
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}

	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("启动 Claude Code：%w", err)
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()
	// 正常结束时也清理进程组里可能残留的后台命令。
	defer killGroup(cmd, syscall.SIGKILL)

	// 中断与超时都直接结束整个进程组，stdout 随之关闭，读取循环得以返回。
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-sigCtx.Done():
			killGroup(cmd, syscall.SIGTERM)
		case <-finished:
		}
	}()
	timer := time.AfterFunc(cfg.timeout, func() { killGroup(cmd, syscall.SIGTERM) })
	defer timer.Stop()

	if err := writeUserMessage(stdinW, cfg.first); err != nil {
		_ = stdinW.Close()
		return fmt.Errorf("写入第一条用户消息：%w", err)
	}

	reader := bufio.NewReaderSize(stdoutR, 64<<10)
	secondSent := false
	sawQueued := false
	resultSeen := false
	idleSeen := false
	lastQueued := 0
	var secondErr error

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, err := out.Write(line); err != nil {
				_ = stdinW.Close()
				return fmt.Errorf("写入输出文件：%w", err)
			}
			msg := parseStreamLine(line)
			switch {
			case !secondSent && (msg.running() || msg.Type == "assistant"):
				secondSent = true
				if err := writeUserMessage(stdinW, cfg.second); err != nil {
					secondErr = fmt.Errorf("写入第二条用户消息：%w", err)
					_ = stdinW.Close()
				}
			case msg.Type == "result":
				resultSeen = true
				lastQueued = msg.queued()
				if lastQueued > 0 {
					sawQueued = true
					idleSeen = false
				}
			case msg.Type == "system" && msg.Subtype == "session_state_changed" && msg.State == "idle":
				idleSeen = true
			}
		}
		if readErr != nil {
			break
		}
		// 回合边界到齐就结束读取。第二条消息成功排队时，边界前的 result
		// 带着 queued_turn_count 大于 0，不会在这里提前结束。
		if resultSeen && idleSeen && lastQueued == 0 {
			break
		}
	}

	waitErr := finishProcess(cmd, stdinW, reader, out)
	if secondErr != nil {
		return secondErr
	}
	switch {
	case sigCtx.Err() != nil:
		return errors.New("录制被中断")
	case !secondSent:
		return errors.New("回合没有开始，第二条用户消息没有机会写入；Claude Code 的输出已保留")
	case !sawQueued:
		if waitErr != nil {
			return fmt.Errorf("没有观察到 queued_turn_count 大于 0 的 result，Claude Code 退出：%w", waitErr)
		}
		return errors.New("没有观察到 queued_turn_count 大于 0 的 result；Claude Code 的输出已保留")
	}
	// 已经录到目标样本；此时 Claude Code 的退出状态不影响输出文件。
	return nil
}

// killGroup 向子进程所在的进程组发信号。子进程以 Setpgid 启动，因而
// Claude Code 派生的命令也一起结束。
func killGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		// 尽力而为：进程可能已经退出，信号失败不影响后续步骤。
		return
	}
}

// finishProcess 关闭 stdin，等 Claude Code 退出，并按升级顺序发 SIGTERM 与
// SIGKILL。剩余的 stdout 行继续写进输出文件。
func finishProcess(cmd *exec.Cmd, stdin *os.File, reader *bufio.Reader, out io.Writer) error {
	_ = stdin.Close()

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				_, _ = out.Write(line)
			}
			if err != nil {
				return
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var err error
	select {
	case err = <-waitCh:
	case <-time.After(stdinCloseWait):
		killGroup(cmd, syscall.SIGTERM)
		select {
		case err = <-waitCh:
		case <-time.After(terminateWait):
			killGroup(cmd, syscall.SIGKILL)
			err = <-waitCh
		}
	}
	<-drainDone
	return err
}

// readToken 从凭据文件取出令牌。错误信息只带文件路径，不带内容。
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取凭据文件：%w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("凭据文件为空")
	}
	return token, nil
}

// streamMessage 是录制程序从 stdout 关心的字段。
type streamMessage struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	State           string `json:"state"`
	QueuedTurnCount *int   `json:"queued_turn_count"`
}

func parseStreamLine(line []byte) streamMessage {
	var msg streamMessage
	_ = json.Unmarshal(line, &msg)
	return msg
}

// running 报告这条消息是否表示回合已经开始。
func (m streamMessage) running() bool {
	return m.Type == "system" && m.Subtype == "session_state_changed" && m.State == "running"
}

// queued 返回 result 里的 queued_turn_count，缺失时按 0 处理。
func (m streamMessage) queued() int {
	if m.QueuedTurnCount == nil {
		return 0
	}
	return *m.QueuedTurnCount
}

// writeUserMessage 写一行 stream-json 用户消息，格式与看守进程的
// claudecode 驱动一致。
func writeUserMessage(w io.Writer, text string) error {
	payload := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	payload.Message.Role = "user"
	payload.Message.Content = text

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

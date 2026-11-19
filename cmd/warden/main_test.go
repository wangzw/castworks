package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/agentprotocol"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"google.golang.org/grpc"
)

// TestMain 让重新执行的测试二进制充当 --claude-bin 指向的假 Claude Code。
// 驱动器启动它时固定传入 --input-format；正常测试运行不带该参数。
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if arg == "--input-format" {
			os.Exit(runFakeClaude())
		}
	}
	os.Exit(m.Run())
}

// runFakeClaude 是假 Claude Code：先输出一条带 session_id 的 system 初始化
// 消息，之后每收到一条 stdin 用户消息，就按一个回合结束输出 assistant、
// result 与 session_state_changed:idle。
func runFakeClaude() int {
	writeFakeLine(map[string]any{
		"type": "system", "subtype": "init", "session_id": "sess-fake",
	})
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		writeFakeLine(map[string]any{
			"type": "assistant", "session_id": "sess-fake",
			"message": map[string]any{"role": "assistant", "content": []any{}},
		})
		writeFakeLine(map[string]any{
			"type": "result", "subtype": "success", "result": "fake output",
			"is_error": false, "queued_turn_count": 0, "session_id": "sess-fake",
		})
		writeFakeLine(map[string]any{
			"type": "system", "subtype": "session_state_changed",
			"state": "idle", "session_id": "sess-fake",
		})
	}
	return 0
}

// writeFakeLine 把一条消息写为一行 stream-json。
func writeFakeLine(msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = os.Stdout.Write(data)
}

// fakeControlPlane 是进程内的假 AgentStream 服务端。它接受 Hello，回
// HelloAck 与一条 Deliver，记录看守进程的回帧，在收到 turn_ended 后发送
// Shutdown。
type fakeControlPlane struct {
	agentprotocolv1.UnimplementedAgentStreamServer

	runID string
	addr  string

	mu       sync.Mutex
	hellos   []*agentprotocolv1.Hello
	accepted []*agentprotocolv1.Accepted
	started  []*agentprotocolv1.TurnStarted
	ended    []*agentprotocolv1.TurnEnded
}

func newFakeControlPlane(t *testing.T, runID string) *fakeControlPlane {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeControlPlane{runID: runID, addr: ln.Addr().String()}
	gs := grpc.NewServer()
	agentprotocolv1.RegisterAgentStreamServer(gs, s)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	return s
}

func (s *fakeControlPlane) Connect(stream grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame]) error {
	frame, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := agentprotocol.ValidateWardenFrame(frame); err != nil {
		return fmt.Errorf("hello validation: %w", err)
	}
	hello := frame.GetHello()
	if hello == nil {
		return errors.New("first frame is not hello")
	}
	if got := frame.GetRunId(); got != s.runID {
		return fmt.Errorf("run_id %q, want %q", got, s.runID)
	}
	s.mu.Lock()
	s.hellos = append(s.hellos, hello)
	s.mu.Unlock()

	if err := stream.Send(helloAckFrame(s.runID)); err != nil {
		return err
	}
	if err := stream.Send(deliverFrame(s.runID)); err != nil {
		return err
	}

	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		switch {
		case frame.GetAccepted() != nil:
			s.mu.Lock()
			s.accepted = append(s.accepted, frame.GetAccepted())
			s.mu.Unlock()
		case frame.GetEvent() != nil:
			event := frame.GetEvent()
			if started := event.GetTurnStarted(); started != nil {
				s.mu.Lock()
				s.started = append(s.started, started)
				s.mu.Unlock()
			}
			if ended := event.GetTurnEnded(); ended != nil {
				s.mu.Lock()
				s.ended = append(s.ended, ended)
				s.mu.Unlock()
				return stream.Send(shutdownFrame(s.runID))
			}
		default:
			return fmt.Errorf("unexpected warden frame: %T", frame.GetBody())
		}
	}
}

func helloAckFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_HelloAck{HelloAck: &agentprotocolv1.HelloAck{
			LlmCredential: &agentprotocolv1.LLMCredential{
				Kind:  agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY,
				Value: "test-key",
			},
		}},
	}
}

func deliverFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body: &agentprotocolv1.ControlFrame_Deliver{Deliver: &agentprotocolv1.Deliver{
			DeliveryId: "d-1",
			Batch:      []*agentprotocolv1.InboxMessage{{InboxSeq: 1, Text: "hello"}},
			ThroughSeq: 1,
		}},
	}
}

func shutdownFrame(runID string) *agentprotocolv1.ControlFrame {
	return &agentprotocolv1.ControlFrame{
		RunId: runID,
		Body:  &agentprotocolv1.ControlFrame_Shutdown{Shutdown: &agentprotocolv1.Shutdown{Reason: "ended"}},
	}
}

// TestLocalModeDeliversTurnAndExitsZero 以本地模式启动编译出的 warden，连接
// 进程内的假服务端，用重新执行的测试二进制充当 Claude Code，完成一次投递
// 与回合，收到 Shutdown 后以 0 退出。
func TestLocalModeDeliversTurnAndExitsZero(t *testing.T) {
	bin := buildWarden(t)
	server := newFakeControlPlane(t, "run-1")
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--run-id", "run-1",
		"--control-plane", server.addr,
		"--home", home,
		"--claude-bin", fakeClaudePath(t),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("warden exited with %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	if len(server.hellos) != 1 {
		t.Fatalf("hello frames = %d, want 1", len(server.hellos))
	}
	if phase := server.hellos[0].GetAgentState().GetPhase(); phase != agentprotocolv1.Phase_PHASE_STARTING {
		t.Fatalf("hello phase = %v, want starting", phase)
	}
	if len(server.accepted) != 1 {
		t.Fatalf("accepted frames = %d, want 1", len(server.accepted))
	}
	if got := server.accepted[0]; got.GetDeliveryId() != "d-1" || got.GetThroughSeq() != 1 {
		t.Fatalf("accepted = %v, want delivery d-1 through 1", got)
	}
	if len(server.started) != 1 {
		t.Fatalf("turn_started frames = %d, want 1", len(server.started))
	}
	if got := server.started[0]; got.GetDeliveryId() != "d-1" || got.GetSessionId() != "sess-fake" {
		t.Fatalf("turn_started = %v, want delivery d-1 session sess-fake", got)
	}
	if len(server.ended) != 1 {
		t.Fatalf("turn_ended frames = %d, want 1", len(server.ended))
	}
	if got := server.ended[0]; got.GetOutput() != "fake output" || got.GetIsError() || got.GetAgentExited() {
		t.Fatalf("turn_ended = %v, want output %q with no error and no agent exit", got, "fake output")
	}
}

// TestConfigErrorsExitOne 覆盖参数与模式判定的组合。每种组合都以退出码 1
// 结束，且错误文本指出缺失或冲突的项。
func TestConfigErrorsExitOne(t *testing.T) {
	bin := buildWarden(t)
	envWithoutControlPlane := envWithout(os.Environ(), envControlPlane)
	envWithControlPlane := append(append([]string(nil), envWithoutControlPlane...), envControlPlane+"=127.0.0.1:1")

	cases := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{
			name: "actor mode requires the control plane env",
			env:  envWithoutControlPlane,
			want: envControlPlane,
		},
		{
			name: "actor mode rejects local flags",
			args: []string{"--home", t.TempDir()},
			env:  envWithControlPlane,
			want: "--home",
		},
		{
			name: "local mode requires home",
			args: []string{"--run-id", "run-1", "--control-plane", "127.0.0.1:1"},
			env:  envWithoutControlPlane,
			want: "--home",
		},
		{
			name: "local mode requires control plane",
			args: []string{"--run-id", "run-1", "--home", t.TempDir()},
			env:  envWithoutControlPlane,
			want: "--control-plane",
		},
		{
			name: "local mode rejects an empty run id",
			args: []string{"--run-id", "", "--control-plane", "127.0.0.1:1", "--home", t.TempDir()},
			env:  envWithoutControlPlane,
			want: "--run-id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, tc.args...)
			cmd.Env = tc.env
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("expected a nonzero exit\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run: %v", err)
			}
			if exitErr.ExitCode() != 1 {
				t.Fatalf("exit code = %d, want 1", exitErr.ExitCode())
			}
			combined := stdout.String() + stderr.String()
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("output does not mention %q\nstdout:\n%s\nstderr:\n%s", tc.want, stdout.String(), stderr.String())
			}
		})
	}
}

// buildWarden 在临时目录中编译当前包，返回可执行文件路径。
func buildWarden(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "warden")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = wd
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// fakeClaudePath 返回当前测试二进制的绝对路径。warden 以固定参数启动它，
// TestMain 据此进入假 Claude Code 分支。
func fakeClaudePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("absolute test binary path: %v", err)
	}
	return path
}

// envWithout 返回去掉 key 的环境变量列表。
func envWithout(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

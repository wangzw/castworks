package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	recordFakeLaunch()
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

// recordFakeLaunch 把启动参数与环境变量写入 $HOME/.fake，并在给出
// --mcp-config 时连接其中的地址，记录端点是否可达。测试据此核对看守进程
// 传给 Claude Code 的内容。
func recordFakeLaunch() {
	base := filepath.Join(os.Getenv("HOME"), ".fake")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return
	}
	args := os.Args[1:]
	if data, err := json.Marshal(args); err == nil {
		_ = os.WriteFile(filepath.Join(base, "args.json"), data, 0o600)
	}
	if data, err := json.Marshal(os.Environ()); err == nil {
		_ = os.WriteFile(filepath.Join(base, "env.json"), data, 0o600)
	}
	endpoint := fakeMCPURL(args)
	if endpoint == "" {
		return
	}
	reachable := false
	if parsed, err := url.Parse(endpoint); err == nil {
		if conn, err := net.DialTimeout("tcp", parsed.Host, 2*time.Second); err == nil {
			reachable = true
			_ = conn.Close()
		}
	}
	record, err := json.Marshal(map[string]any{"url": endpoint, "reachable": reachable})
	if err == nil {
		_ = os.WriteFile(filepath.Join(base, "mcp.json"), record, 0o600)
	}
}

// fakeMCPURL 返回 --mcp-config 中 castworks 服务器的 URL，没给出时返回空串。
func fakeMCPURL(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--mcp-config" {
			continue
		}
		var config struct {
			Servers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		if json.Unmarshal([]byte(args[i+1]), &config) == nil {
			return config.Servers["castworks"].URL
		}
	}
	return ""
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
			Batch: []*agentprotocolv1.InboxMessage{{
				InboxSeq: 1,
				Content:  &agentprotocolv1.InboxMessage_Text{Text: "hello"},
			}},
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

// TestLocalModePassesMCPConfig 核对本地模式给出 connector 地址时：Claude
// Code 收到 --mcp-config 与 --strict-mcp-config，配置里的端口就是平台 MCP
// 服务器实际监听的端口（假 Claude Code 启动时连得上），且环境变量不包含
// 看守进程的 EXTSIM_URL。
func TestLocalModePassesMCPConfig(t *testing.T) {
	bin := buildWarden(t)
	server := newFakeControlPlane(t, "run-mcp")
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--run-id", "run-mcp",
		"--control-plane", server.addr,
		"--home", home,
		"--claude-bin", fakeClaudePath(t),
		"--connector", "127.0.0.1:9091",
		"--mcp-addr", "127.0.0.1:0",
	)
	cmd.Env = append(os.Environ(), "EXTSIM_URL=http://127.0.0.1:9100")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("warden exited with %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	args := fakeArgs(t, home)
	raw, ok := flagValue(args, "--mcp-config")
	if !ok {
		t.Fatalf("start args have no --mcp-config: %q", args)
	}
	if !containsArg(args, "--strict-mcp-config") {
		t.Fatalf("start args have no --strict-mcp-config: %q", args)
	}

	var config struct {
		Servers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("decode --mcp-config %q: %v", raw, err)
	}
	mcpServer, ok := config.Servers["castworks"]
	if !ok {
		t.Fatalf("--mcp-config has no castworks server: %q", raw)
	}
	if mcpServer.Type != "http" {
		t.Fatalf("castworks server type = %q, want http", mcpServer.Type)
	}
	parsed, err := url.Parse(mcpServer.URL)
	if err != nil {
		t.Fatalf("parse MCP URL %q: %v", mcpServer.URL, err)
	}
	if parsed.Scheme != "http" || parsed.Path != "/mcp" {
		t.Fatalf("MCP URL = %q, want http://<addr>/mcp", mcpServer.URL)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split MCP host %q: %v", parsed.Host, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("MCP host = %q, want 127.0.0.1", host)
	}
	if port == "" || port == "0" {
		t.Fatalf("MCP port = %q, want an assigned port", port)
	}
	if !strings.Contains(stdout.String(), "mcp listening") {
		t.Fatalf("stdout has no MCP listening log:\n%s", stdout.String())
	}

	// 假 Claude Code 在启动时连上了这个地址：配置里的端口就是服务器实际
	// 监听的端口。
	data, err := os.ReadFile(filepath.Join(home, ".fake", "mcp.json"))
	if err != nil {
		t.Fatalf("read fake MCP check: %v", err)
	}
	var check struct {
		URL       string `json:"url"`
		Reachable bool   `json:"reachable"`
	}
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("decode fake MCP check: %v", err)
	}
	if check.URL != mcpServer.URL || !check.Reachable {
		t.Fatalf("fake Claude Code reached %q (reachable %v), want %q reachable", check.URL, check.Reachable, mcpServer.URL)
	}

	for _, entry := range fakeEnv(t, home) {
		if strings.HasPrefix(entry, "EXTSIM_URL=") {
			t.Fatalf("Claude Code environment carries EXTSIM_URL: %q", entry)
		}
	}
}

// TestLocalModeWithoutConnectorKeepsV010Args 核对没给出 connector 地址时启动
// 参数与 v0.1.0 逐字相同，不启动平台 MCP 服务器。
func TestLocalModeWithoutConnectorKeepsV010Args(t *testing.T) {
	bin := buildWarden(t)
	server := newFakeControlPlane(t, "run-nomcp")
	home := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--run-id", "run-nomcp",
		"--control-plane", server.addr,
		"--home", home,
		"--claude-bin", fakeClaudePath(t),
		"--mcp-addr", "127.0.0.1:0",
	)
	cmd.Env = append(os.Environ(), "EXTSIM_URL=http://127.0.0.1:9100")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("warden exited with %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	want := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if args := fakeArgs(t, home); !reflect.DeepEqual(args, want) {
		t.Fatalf("start args = %q, want %q", args, want)
	}
	if strings.Contains(stdout.String(), "mcp listening") {
		t.Fatalf("platform MCP server started without a connector address:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".fake", "mcp.json")); err == nil {
		t.Fatalf("fake Claude Code was given --mcp-config without a connector address")
	}
	for _, entry := range fakeEnv(t, home) {
		if strings.HasPrefix(entry, "EXTSIM_URL=") {
			t.Fatalf("Claude Code environment carries EXTSIM_URL: %q", entry)
		}
	}
}

// TestParseConfigPlatformMCP 核对两种模式下 connector 地址与 MCP 监听地址的
// 来源：actor 模式取环境变量并固定监听地址，本地模式取命令行参数。
func TestParseConfigPlatformMCP(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		env           map[string]string
		wantConnector string
		wantMCPAddr   string
	}{
		{
			name:          "actor mode reads CASTWORKS_CONNECTOR",
			env:           map[string]string{envControlPlane: "127.0.0.1:1", envConnector: "127.0.0.1:9091"},
			wantConnector: "127.0.0.1:9091",
			wantMCPAddr:   actorMCPAddr,
		},
		{
			name:        "actor mode without CASTWORKS_CONNECTOR",
			env:         map[string]string{envControlPlane: "127.0.0.1:1"},
			wantMCPAddr: actorMCPAddr,
		},
		{
			name:        "local mode defaults the MCP address",
			args:        []string{"--run-id", "run-1", "--control-plane", "127.0.0.1:1", "--home", "/tmp/home"},
			wantMCPAddr: localMCPAddr,
		},
		{
			name: "local mode takes both addresses from flags",
			args: []string{
				"--run-id", "run-1", "--control-plane", "127.0.0.1:1", "--home", "/tmp/home",
				"--connector", "127.0.0.1:9091", "--mcp-addr", "127.0.0.1:8082",
			},
			wantConnector: "127.0.0.1:9091",
			wantMCPAddr:   "127.0.0.1:8082",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			cfg, err := parseConfigWithOutput(tc.args, getenv, io.Discard)
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if cfg.ConnectorAddr != tc.wantConnector {
				t.Fatalf("ConnectorAddr = %q, want %q", cfg.ConnectorAddr, tc.wantConnector)
			}
			if cfg.MCPAddr != tc.wantMCPAddr {
				t.Fatalf("MCPAddr = %q, want %q", cfg.MCPAddr, tc.wantMCPAddr)
			}
		})
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
			name: "actor mode rejects --connector",
			args: []string{"--connector", "127.0.0.1:9091"},
			env:  envWithControlPlane,
			want: "--connector",
		},
		{
			name: "actor mode rejects --mcp-addr",
			args: []string{"--mcp-addr", "127.0.0.1:8082"},
			env:  envWithControlPlane,
			want: "--mcp-addr",
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

// fakeArgs 读取假 Claude Code 记录的启动参数。
func fakeArgs(t *testing.T, home string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".fake", "args.json"))
	if err != nil {
		t.Fatalf("read fake args: %v", err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatalf("decode fake args: %v", err)
	}
	return args
}

// fakeEnv 读取假 Claude Code 记录的环境变量。
func fakeEnv(t *testing.T, home string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".fake", "env.json"))
	if err != nil {
		t.Fatalf("read fake env: %v", err)
	}
	var env []string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode fake env: %v", err)
	}
	return env
}

// flagValue 返回 name 之后紧跟的参数值；参数不存在时返回 false。
func flagValue(args []string, name string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1], true
		}
	}
	return "", false
}

// containsArg 报告 args 中是否有 name 这一项。
func containsArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

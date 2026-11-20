package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolCallTimeout 是单次平台 MCP 工具调用的上限，与看守进程转发到
// connector 端点的 10 s 一致。
const toolCallTimeout = 10 * time.Second

// execRunner 用 /bin/sh 执行脚本命令，工作目录是假 Claude Code 的工作目录。
type execRunner struct {
	dir string
}

// run 在前台执行命令。
func (r execRunner) Run(ctx context.Context, command string) commandResult {
	cmd := shellCommand(ctx, r.dir, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	applyExitError(&result, err)
	return result
}

// Start 在后台执行命令，输出写进 outputPath。
func (r execRunner) Start(ctx context.Context, command, outputPath string) backgroundHandle {
	done := make(chan commandResult, 1)
	handle := backgroundHandle{Done: done}
	fail := func(err error) backgroundHandle {
		done <- commandResult{Err: err}
		close(done)
		return handle
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return fail(err)
	}
	output, err := os.Create(outputPath)
	if err != nil {
		return fail(err)
	}
	cmd := shellCommand(ctx, r.dir, command)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		_ = output.Close()
		return fail(err)
	}
	go func() {
		err := cmd.Wait()
		_ = output.Close()
		result := commandResult{}
		applyExitError(&result, err)
		done <- result
		close(done)
	}()
	return handle
}

// shellCommand 构造一条 /bin/sh 命令。取消时结束整个进程组，避免只结束
// shell 而留下后台运行的子进程。
func shellCommand(ctx context.Context, dir, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// applyExitError 把 exec 的错误换算成退出码；拿不到退出码时保留 err。
func applyExitError(result *commandResult, err error) {
	if err == nil {
		return
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return
	}
	result.Err = err
}

// mcpCaller 是平台 MCP 服务器的客户端。第一次调用时才连接，连接后复用。
type mcpCaller struct {
	endpoint string

	mu      sync.Mutex
	session *mcp.ClientSession
}

func newMCPCaller(endpoint string) toolCaller {
	if endpoint == "" {
		return nil
	}
	return &mcpCaller{endpoint: endpoint}
}

// CallTool 调用一个工具，返回结果中的文本与错误标记。
func (c *mcpCaller) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	session, err := c.connect(ctx)
	if err != nil {
		return "", false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", false, err
	}
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n"), result.IsError, nil
}

// connect 建立并缓存 MCP 会话。
func (c *mcpCaller) connect(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return c.session, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fakeclaude", Version: "v0.2.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", c.endpoint, err)
	}
	c.session = session
	return session, nil
}

// Close 关闭 MCP 会话。
func (c *mcpCaller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil
	}
	err := c.session.Close()
	c.session = nil
	return err
}

// mcpEndpoint 从看守进程传入的参数里取出平台 MCP 服务器的地址。没有给出
// --mcp-config 时返回空串，表示不启动 MCP 客户端。
func mcpEndpoint(args []string) (string, error) {
	raw := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--mcp-config":
			if i+1 >= len(args) {
				return "", errors.New("--mcp-config has no value")
			}
			i++
			raw = args[i]
		case strings.HasPrefix(args[i], "--mcp-config="):
			raw = strings.TrimPrefix(args[i], "--mcp-config=")
		}
	}
	if raw == "" {
		return "", nil
	}

	var config struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return "", fmt.Errorf("parse --mcp-config: %w", err)
	}
	server, ok := config.MCPServers["castworks"]
	if !ok {
		return "", errors.New("--mcp-config has no castworks server")
	}
	if server.URL == "" {
		return "", errors.New("castworks MCP server has no url")
	}
	if err := checkLoopbackURL(server.URL); err != nil {
		return "", err
	}
	return server.URL, nil
}

// checkLoopbackURL 只接受回环地址，保证假 Claude Code 不向外联网。
func checkLoopbackURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse MCP url %q: %w", raw, err)
	}
	if parsed.Scheme != "http" {
		return fmt.Errorf("MCP url %q must use http", raw)
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("MCP url %q is not a loopback address", raw)
	}
	return nil
}

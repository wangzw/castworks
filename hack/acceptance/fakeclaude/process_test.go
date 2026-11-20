package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPEndpoint 检查 --mcp-config 的两种写法与错误情形。
func TestMCPEndpoint(t *testing.T) {
	config := `{"mcpServers":{"castworks":{"type":"http","url":"http://127.0.0.1:8082/mcp"}}}`

	got, err := mcpEndpoint([]string{"-p", "--mcp-config", config, "--strict-mcp-config"})
	if err != nil {
		t.Fatalf("mcpEndpoint: %v", err)
	}
	if want := "http://127.0.0.1:8082/mcp"; got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}

	got, err = mcpEndpoint([]string{"--mcp-config=" + config})
	if err != nil || got != "http://127.0.0.1:8082/mcp" {
		t.Fatalf("mcpEndpoint with = form = %q, %v", got, err)
	}

	got, err = mcpEndpoint([]string{"-p", "--output-format", "stream-json"})
	if err != nil || got != "" {
		t.Fatalf("mcpEndpoint without --mcp-config = %q, %v", got, err)
	}

	cases := map[string][]string{
		"no value":      {"--mcp-config"},
		"bad json":      {"--mcp-config", "{"},
		"no castworks":  {"--mcp-config", `{"mcpServers":{}}`},
		"no url":        {"--mcp-config", `{"mcpServers":{"castworks":{"type":"http"}}}`},
		"not loopback":  {"--mcp-config", `{"mcpServers":{"castworks":{"url":"http://api.anthropic.com/mcp"}}}`},
		"https refused": {"--mcp-config", `{"mcpServers":{"castworks":{"url":"https://127.0.0.1/mcp"}}}`},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := mcpEndpoint(args); err == nil {
				t.Fatalf("mcpEndpoint(%v) accepted", args)
			}
		})
	}
}

// TestCheckLoopbackURL 检查回环地址的判定。
func TestCheckLoopbackURL(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8082/mcp",
		"http://localhost:8082/mcp",
		"http://[::1]:8082/mcp",
	} {
		if err := checkLoopbackURL(raw); err != nil {
			t.Errorf("checkLoopbackURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://192.168.1.1:8082/mcp",
		"http://example.com/mcp",
		"ftp://127.0.0.1/mcp",
	} {
		if err := checkLoopbackURL(raw); err == nil {
			t.Errorf("checkLoopbackURL(%q) accepted", raw)
		}
	}
}

// TestExecRunner 检查命令的退出码、输出与后台输出文件。
func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	runner := execRunner{dir: dir}

	result := runner.Run(context.Background(), `printf '{"id":"job-1"}'`)
	if result.failed() {
		t.Fatalf("Run failed: %+v", result)
	}
	if result.Stdout != `{"id":"job-1"}` {
		t.Fatalf("stdout = %q", result.Stdout)
	}

	result = runner.Run(context.Background(), "exit 3")
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", result.ExitCode)
	}

	outputPath := filepath.Join(dir, "task.output")
	handle := runner.Start(context.Background(), "echo background; exit 4", outputPath)
	select {
	case result = <-handle.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("background command did not finish")
	}
	if result.ExitCode != 4 {
		t.Fatalf("background exit code = %d, want 4", result.ExitCode)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !strings.Contains(string(data), "background") {
		t.Fatalf("output file = %q", data)
	}
}

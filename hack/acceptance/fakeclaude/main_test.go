package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScriptPathUsesWorkingDirectory 检查脚本路径固定为工作目录下的固定
// 文件名。
func TestScriptPathUsesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got, err := scriptPath()
	if err != nil {
		t.Fatalf("scriptPath: %v", err)
	}
	if want := filepath.Join(dir, defaultScriptName); got != want {
		t.Fatalf("scriptPath = %q, want %q", got, want)
	}
}

// TestRunRepliesToUserMessage 检查主循环读一行用户消息、按脚本回复并退出 0。
func TestRunRepliesToUserMessage(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	script := `{"rules":[{"name":"hello","match":"hello","steps":[{"type":"reply","text":"pong"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, defaultScriptName), []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run(nil,
		strings.NewReader(`{"type":"user","message":{"role":"user","content":"hello"}}`+"\n"),
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}

	var result string
	var sawRunning, sawIdle bool
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		switch {
		case message["type"] == "result":
			result, _ = message["result"].(string)
		case message["subtype"] == "session_state_changed" && message["state"] == "running":
			sawRunning = true
		case message["subtype"] == "session_state_changed" && message["state"] == "idle":
			sawIdle = true
		}
	}
	if result != "pong" {
		t.Fatalf("result = %q, want pong", result)
	}
	if !sawRunning || !sawIdle {
		t.Fatalf("running = %v, idle = %v", sawRunning, sawIdle)
	}
}

// TestRunRejectsInvalidScript 检查脚本错误时退出 2。
func TestRunRejectsInvalidScript(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, defaultScriptName), []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no rules") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

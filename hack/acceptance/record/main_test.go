package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteUserMessage 检查写进 stdin 的一行是 stream-json 用户消息。
func TestWriteUserMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := writeUserMessage(&buf, "hello\nworld"); err != nil {
		t.Fatalf("writeUserMessage：%v", err)
	}

	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON：%v", err)
	}
	if payload.Type != "user" || payload.Message.Role != "user" {
		t.Fatalf("类型为 %q/%q，想要 user/user", payload.Type, payload.Message.Role)
	}
	if payload.Message.Content != "hello\nworld" {
		t.Fatalf("内容为 %q，想要 %q", payload.Message.Content, "hello\nworld")
	}
	if got := buf.Bytes()[buf.Len()-1]; got != '\n' {
		t.Fatalf("最后一个是 %q，想要换行", got)
	}
}

// TestStreamSignals 检查回合开始与排队计数的解析。
func TestStreamSignals(t *testing.T) {
	running := parseStreamLine([]byte(
		`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	if !running.running() {
		t.Fatal("running 消息没有被识别为回合开始")
	}

	queuedLine := []byte(`{"type":"result","is_error":false,"result":"done","queued_turn_count":2}`)
	if got := parseStreamLine(queuedLine).queued(); got != 2 {
		t.Fatalf("queued_turn_count = %d，想要 2", got)
	}
	plain := []byte(`{"type":"result","is_error":false,"result":"done"}`)
	if got := parseStreamLine(plain).queued(); got != 0 {
		t.Fatalf("缺省 queued_turn_count = %d，想要 0", got)
	}
}

// TestParseConfigRequiresFlags 检查必填参数与默认值。
func TestParseConfigRequiresFlags(t *testing.T) {
	if _, err := parseConfig(nil); err == nil {
		t.Fatal("缺必填参数时应当报错")
	}

	cfg, err := parseConfig([]string{
		"-claude-bin", "/bin/claude",
		"-credential-file", "/tmp/token",
		"-out", "/tmp/out.jsonl",
	})
	if err != nil {
		t.Fatalf("parseConfig：%v", err)
	}
	if cfg.first != defaultFirst || cfg.second != defaultSecond {
		t.Fatalf("默认消息为 %q/%q", cfg.first, cfg.second)
	}
	if cfg.timeout != defaultTimeout {
		t.Fatalf("默认超时为 %s，想要 %s", cfg.timeout, defaultTimeout)
	}

	if _, err := parseConfig([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h 返回 %v，想要 flag.ErrHelp", err)
	}
}

// TestPrepareHomeRemovesTemporaryDirectory 检查未指定 -home 时新建的临时
// 主目录会在清理函数执行后删除，而调用者指定的目录保留。
func TestPrepareHomeRemovesTemporaryDirectory(t *testing.T) {
	home, cleanup, err := prepareHome("")
	if err != nil {
		t.Fatalf("prepareHome：%v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("临时主目录 %s 不可用：%v", home, err)
	}
	cleanup()
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("清理后临时主目录 %s 仍存在：%v", home, err)
	}

	configured := t.TempDir()
	got, keep, err := prepareHome(configured)
	if err != nil {
		t.Fatalf("prepareHome：%v", err)
	}
	if got != configured {
		t.Fatalf("主目录为 %q，想要 %q", got, configured)
	}
	keep()
	if _, err := os.Stat(configured); err != nil {
		t.Fatalf("指定目录不应被删除：%v", err)
	}
}

// TestReadTokenTrimsWhitespace 检查凭据文件末尾的换行被去掉。
func TestReadTokenTrimsWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatalf("写入凭据文件：%v", err)
	}
	token, err := readToken(path)
	if err != nil {
		t.Fatalf("readToken：%v", err)
	}
	if token != "secret-token" {
		t.Fatalf("令牌为 %q，想要 %q", token, "secret-token")
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatalf("写入空凭据文件：%v", err)
	}
	if _, err := readToken(empty); err == nil {
		t.Fatal("空凭据文件应当报错")
	}
}

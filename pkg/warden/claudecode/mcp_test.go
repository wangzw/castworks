package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

// TestArgsWithoutMCPURL 核对没有平台 MCP 服务器时启动参数与 v0.1.0 逐字相同。
func TestArgsWithoutMCPURL(t *testing.T) {
	driver := New(Config{}, nil, nil)
	args, err := driver.args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if !reflect.DeepEqual(args, claudeArgs) {
		t.Fatalf("args = %q, want %q", args, claudeArgs)
	}
}

// TestArgsWithMCPConfig 核对给出 MCP 服务器地址时追加的两个参数与 JSON 内容。
func TestArgsWithMCPConfig(t *testing.T) {
	const url = "http://127.0.0.1:43512/mcp"
	driver := New(Config{MCPURL: url}, nil, nil)
	args, err := driver.args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}

	want := append(append([]string(nil), claudeArgs...),
		"--mcp-config",
		`{"mcpServers":{"castworks":{"type":"http","url":"`+url+`"}}}`,
		"--strict-mcp-config",
	)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

// TestStartCommandWithMCPConfig 以假 Claude Code 核对实际启动参数：MCP 配置
// 在固定参数之后，且只有 castworks 一个 http 服务器。
func TestStartCommandWithMCPConfig(t *testing.T) {
	const url = "http://127.0.0.1:43512/mcp"
	cfg := testConfig(t)
	cfg.MCPURL = url
	writeFakeMode(t, cfg.Home, "record")
	driver, _ := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hi"}},
		apiKeyCredential(),
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	argsPath := filepath.Join(cfg.Home, ".fake", "args.json")
	var args []string
	waitFor(t, func() bool {
		data, err := os.ReadFile(argsPath)
		if err != nil {
			return false
		}
		return json.Unmarshal(data, &args) == nil
	})

	want := append(append([]string(nil), claudeArgs...),
		"--mcp-config",
		`{"mcpServers":{"castworks":{"type":"http","url":"`+url+`"}}}`,
		"--strict-mcp-config",
	)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("start args = %q, want %q", args, want)
	}
}

package connectorhub

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestDevConfig 装载 deploy/dev/connectorhub/config.yaml，核对开发环境控制面
// 配置示例的项目、外部系统名与绑定值。定义按配置文件所在目录解析，签名密钥
// 文件只在 webhook 请求时读取，装载时不必存在。
func TestDevConfig(t *testing.T) {
	// 指向一个空的主目录，证明装载不要求签名密钥文件已经存在。
	home := t.TempDir()
	t.Setenv("HOME", home)

	devDir, err := filepath.Abs(filepath.Join("..", "..", "deploy", "dev", "connectorhub"))
	if err != nil {
		t.Fatalf("resolve dev directory: %v", err)
	}
	config, err := LoadConfig(filepath.Join(devDir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if config.Project != "default" {
		t.Errorf("Project = %q, want default", config.Project)
	}
	if len(config.Systems) != 1 {
		t.Fatalf("Systems = %d, want 1", len(config.Systems))
	}
	system := config.Systems[0]
	if system.Name != "extsim" {
		t.Errorf("Name = %q, want extsim", system.Name)
	}
	if got, want := system.DefinitionPath, filepath.Join(devDir, "extsim.yaml"); got != want {
		t.Errorf("DefinitionPath = %q, want %q", got, want)
	}
	wantKey := filepath.Join(home, ".config", "castworks", "extsim-signing-key")
	if got := system.SigningKeyFile; got != wantKey {
		t.Errorf("SigningKeyFile = %q, want %q", got, wantKey)
	}
	if !slices.Equal(config.Bindings["extsim"], []string{"demo"}) {
		t.Errorf("Bindings[extsim] = %v, want [demo]", config.Bindings["extsim"])
	}
	if !slices.Equal(system.BindingValues, []string{"demo"}) {
		t.Errorf("BindingValues = %v, want [demo]", system.BindingValues)
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// devRepoRoot 从当前工作目录向上找到包含 go.mod 的模块根目录。go test 把
// 工作目录设为被测包的源目录，因此不能直接按模块根写相对路径。
func devRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到包含 go.mod 的模块根目录")
		}
		dir = parent
	}
}

// TestDevConfigProjectConsistency 核对开发环境的三个项目值一致：
// connectorhub 配置里的 bindings、actor 镜像 /etc/extsim.json 的 project，
// 以及 extsim 命令行的默认项目。三者漂移时，actor 提交的作业会被
// connectorhub 判为项目不归属。
func TestDevConfigProjectConsistency(t *testing.T) {
	root := devRepoRoot(t)

	// connectorhub 配置示例中的 extsim bindings。
	configPath := filepath.Join(root, "deploy", "dev", "connectorhub", "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读取 %s: %v", configPath, err)
	}
	bindingsRE := regexp.MustCompile(`(?m)^\s*extsim:\s*\[([^\]]*)\]`)
	match := bindingsRE.FindSubmatch(configData)
	if match == nil {
		t.Fatalf("%s 中找不到 extsim 的 bindings", configPath)
	}
	var bindingProjects []string
	for _, item := range strings.Split(string(match[1]), ",") {
		item = strings.TrimSpace(strings.Trim(item, `"'`))
		if item != "" {
			bindingProjects = append(bindingProjects, item)
		}
	}
	if len(bindingProjects) == 0 {
		t.Fatalf("%s 中 extsim 的 bindings 为空", configPath)
	}

	// actor 镜像的 /etc/extsim.json 由 Dockerfile 在构建时写入，仓库里没有
	// 独立文件，这里从写入它的那一行取出 JSON。
	dockerfilePath := filepath.Join(root, "images", "actor", "Dockerfile")
	dockerfileData, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("读取 %s: %v", dockerfilePath, err)
	}
	writeLineRE := regexp.MustCompile(`(?m)^.*extsim\.json.*$`)
	writeLine := writeLineRE.Find(dockerfileData)
	if writeLine == nil {
		t.Fatalf("%s 中找不到写入 /etc/extsim.json 的行", dockerfilePath)
	}
	jsonRE := regexp.MustCompile(`'(\{[^']*\})'`)
	jsonMatch := jsonRE.FindSubmatch(writeLine)
	if jsonMatch == nil {
		t.Fatalf("%s 中写入 /etc/extsim.json 的行没有 JSON 字面量", dockerfilePath)
	}
	var imageConfig cliConfigFile
	if err := json.Unmarshal(jsonMatch[1], &imageConfig); err != nil {
		t.Fatalf("解析 %s 中的 /etc/extsim.json: %v", dockerfilePath, err)
	}
	if imageConfig.Project == "" {
		t.Fatalf("%s 中的 /etc/extsim.json 没有 project", dockerfilePath)
	}

	if imageConfig.Project != cliDefaultProject {
		t.Errorf("actor 镜像 /etc/extsim.json 的 project = %q，extsim 命令行默认项目 = %q",
			imageConfig.Project, cliDefaultProject)
	}
	for _, project := range bindingProjects {
		if project != cliDefaultProject {
			t.Errorf("connectorhub bindings 的项目 = %q，extsim 命令行默认项目 = %q",
				project, cliDefaultProject)
		}
	}
}

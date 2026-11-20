// Command fakeclaude 是 v0.2.0 验收用的脚本化 Claude Code。
//
// 看守进程以 --claude-bin 启动它，用 stream-json 驱动它：每条用户消息一行
// JSON 从 stdin 进入，它把 assistant、工具结果、后台任务与回合边界等消息
// 逐行写到 stdout。它不联网，不读凭据，只做脚本写明的动作：
//
//   - 解析看守进程传入的 --mcp-config，用 go-sdk 客户端真实调用平台 MCP
//     服务器上的 list_events 与 subscribe_event，不伪造工具结果；
//   - 执行命令（如 extsim job submit --file <f>）并可从输出中取作业标识；
//   - 以后台方式执行命令，按 local_turn03_deliver.jsonl 与
//     local_turn04_spontaneous.jsonl 的字段与次序写后台任务消息；
//   - 收到订阅完成事件文本时按 outcome 行分支。
//
// 行为由脚本文件决定。脚本固定取工作目录下的 fakeclaude.json，由验收框架
// 在启动看守进程前写入。脚本格式见 hack/acceptance/fakeclaude/
// testdata/subscription.json 与包内各类型的注释。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	// defaultScriptName 是工作目录里的脚本文件名。
	defaultScriptName = "fakeclaude.json"
	// shutdownWait 是 stdin 关闭后等待后台命令收尾的上限。
	shutdownWait = 5 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run 解析参数与脚本，读 stdin 直到关闭。退出码：正常为 0，参数与脚本错误
// 为 2，运行中某个动作失败为 1。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	endpoint, err := mcpEndpoint(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fakeclaude: %v\n", err)
		return 2
	}

	path, err := scriptPath()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fakeclaude: %v\n", err)
		return 2
	}
	script, err := LoadScript(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fakeclaude: %v\n", err)
		return 2
	}

	workDir, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fakeclaude: working directory: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caller := newMCPCaller(endpoint)
	if caller != nil {
		defer func() { _ = caller.Close() }()
	}
	sess := newSession(script, newEmitter(stdout, workDir), workDir, caller, execRunner{dir: workDir}, ctx, stderr)

	scanner := newLineScanner(stdin)
	for scanner.Scan() {
		if err := sess.HandleLine(scanner.Bytes()); err != nil {
			_, _ = fmt.Fprintf(stderr, "fakeclaude: %v\n", err)
			return 1
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprintf(stderr, "fakeclaude: read stdin: %v\n", err)
		return 1
	}

	// stdin 关闭表示看守进程开始关停，结束还在运行的后台命令。
	cancel()
	sess.Wait(shutdownWait)
	return 0
}

// scriptPath 返回工作目录下的脚本路径。
func scriptPath() (string, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	return filepath.Join(workDir, defaultScriptName), nil
}

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	// envControlPlane 是 actor 模式下控制面地址的环境变量。
	envControlPlane = "CASTWORKS_CONTROL_PLANE"
	// envConnector 是 actor 模式下 connector 端点地址的环境变量。
	envConnector = "CASTWORKS_CONNECTOR"
	// actorNamePath 是 actor 模式下读取 run_id 的文件，由 systemInfo 卷投射。
	actorNamePath = "/run/ate/actor-name"

	// actor 模式固定的主目录、Claude Code 路径、就绪探针地址与平台 MCP
	// 服务器的监听地址。
	actorHome       = "/home/agent"
	actorClaudeBin  = "/usr/local/bin/claude"
	actorReadyZAddr = ":8081"
	actorMCPAddr    = "127.0.0.1:8082"

	// localClaudeBin 是本地模式 --claude-bin 的默认值，从 PATH 中查找。
	localClaudeBin = "claude"
	// localMCPAddr 是本地模式 --mcp-addr 的默认值，端口由系统分配。
	localMCPAddr = "127.0.0.1:0"
)

// runMode 是看守进程的运行方式。
type runMode int

const (
	modeActor runMode = iota + 1
	modeLocal
)

// String 返回日志中使用的运行方式名称。
func (m runMode) String() string {
	switch m {
	case modeActor:
		return "actor"
	case modeLocal:
		return "local"
	default:
		return "unknown"
	}
}

// Config 是看守进程的命令行配置。解析只读取参数与环境变量，不读文件、
// 不监听端口、不启动子进程。
type Config struct {
	// Mode 是运行方式。
	Mode runMode
	// RunID 是运行实例标识。本地模式下来自 --run-id；actor 模式下由调用方
	// 从 actor-name 文件填入。
	RunID string
	// ControlPlane 是控制面地址 host:port。
	ControlPlane string
	// ConnectorAddr 是 connector 端点地址 host:port；为空时不启动平台 MCP
	// 服务器，Claude Code 的启动参数与 v0.1.0 相同。
	ConnectorAddr string
	// MCPAddr 是平台 MCP 服务器的监听地址；只在 ConnectorAddr 非空时使用。
	MCPAddr string
	// Home 是 Claude Code 运行用户的主目录。
	Home string
	// ClaudeBin 是 Claude Code 可执行文件路径。
	ClaudeBin string
	// ReadyZAddr 是就绪探针监听地址；为空时不监听。
	ReadyZAddr string
}

// localOnlyFlags 是 actor 模式下不接受的参数。--run-id 是模式判定的依据，
// 因此不在其中。
var localOnlyFlags = []string{"control-plane", "home", "claude-bin", "readyz-addr", "connector", "mcp-addr"}

// ParseConfig 解析 args（不含程序名）并校验。getenv 用于读取 actor 模式
// 的控制面地址，可为 nil，此时视为环境变量为空。
func ParseConfig(args []string, getenv func(string) string) (*Config, error) {
	return parseConfigWithOutput(args, getenv, os.Stderr)
}

// parseConfigWithOutput 是 ParseConfig 的实现。output 接收 flag 包的用法与
// 解析错误；测试传入 io.Discard 以免污染输出。
func parseConfigWithOutput(args []string, getenv func(string) string, output io.Writer) (*Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	var (
		runID        string
		controlPlane string
		home         string
		claudeBin    string
		readyzAddr   string
		connector    string
		mcpAddr      string
	)

	fs := flag.NewFlagSet("warden", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&runID, "run-id", "", "运行实例标识；给出即为本地模式")
	fs.StringVar(&controlPlane, "control-plane", "", "控制面地址 host:port；本地模式必填")
	fs.StringVar(&home, "home", "", "Claude Code 运行用户的主目录；本地模式必填")
	fs.StringVar(&claudeBin, "claude-bin", localClaudeBin, "Claude Code 可执行文件；本地模式默认在 PATH 中查找 claude")
	fs.StringVar(&readyzAddr, "readyz-addr", "", "就绪探针监听地址；本地模式为空时不监听")
	fs.StringVar(&connector, "connector", "", "connector 端点地址 host:port；给出时启动平台 MCP 服务器")
	fs.StringVar(&mcpAddr, "mcp-addr", localMCPAddr, "平台 MCP 服务器监听地址")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		// 只报告个数：位置参数可能携带任意文本，原样回显会把它写进日志。
		return nil, fmt.Errorf("unexpected positional arguments: got %d", fs.NArg())
	}

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	if !visited["run-id"] {
		// actor 模式：身份与地址都不来自命令行参数。
		for _, name := range localOnlyFlags {
			if visited[name] {
				return nil, fmt.Errorf("--%s: must not be set in actor mode", name)
			}
		}
		address := getenv(envControlPlane)
		if address == "" {
			return nil, fmt.Errorf("%s: required in actor mode", envControlPlane)
		}
		return &Config{
			Mode:          modeActor,
			ControlPlane:  address,
			ConnectorAddr: getenv(envConnector),
			MCPAddr:       actorMCPAddr,
			Home:          actorHome,
			ClaudeBin:     actorClaudeBin,
			ReadyZAddr:    actorReadyZAddr,
		}, nil
	}

	// 本地模式：--run-id 已给出，其余来源都取命令行参数。
	if runID == "" {
		return nil, errors.New("--run-id: must not be empty")
	}
	if controlPlane == "" {
		return nil, errors.New("--control-plane: required in local mode")
	}
	if home == "" {
		return nil, errors.New("--home: required in local mode")
	}
	if claudeBin == "" {
		return nil, errors.New("--claude-bin: must not be empty")
	}
	return &Config{
		Mode:          modeLocal,
		RunID:         runID,
		ControlPlane:  controlPlane,
		ConnectorAddr: connector,
		MCPAddr:       mcpAddr,
		Home:          home,
		ClaudeBin:     claudeBin,
		ReadyZAddr:    readyzAddr,
	}, nil
}

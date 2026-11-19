// Command warden 是 castworks 的看守进程。它作为沙箱的 PID 1 运行，或作为
// 开发机上的普通进程运行：确定所属的运行实例，在 /readyz 上应答就绪探针，
// 维持到控制面的出站流，并以 stream-json 驱动一个跨回合常驻的 Claude Code。
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wangzw/castworks/internal/warden/claudecode"
	"github.com/wangzw/castworks/internal/warden/controlstream"
	"github.com/wangzw/castworks/internal/warden/identity"
	"github.com/wangzw/castworks/internal/warden/readyz"
)

// shutdownTimeout 是入口关停 Claude Code 的上限。关停升级本身最长 15 s
// （10 s 等 stdin 关闭、5 s 等 SIGTERM），再加 5 s 余量。
const shutdownTimeout = 20 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Getenv))
}

// run 装配并运行看守进程，返回进程退出码：正常关停为 0，参数、身份与地址
// 错误为 1。它把日志器的选择、就绪探针、回收器与驱动的构造都放在这里，
// 便于测试以参数与环境变量驱动整个进程。
func run(args []string, getenv func(string) string) int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := ParseConfig(args, getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}

	// SIGTERM 总是触发关停；本地模式下另加 SIGINT，便于在开发机上用 Ctrl-C
	// 结束。信号取消 Run 的上下文，随后仍走一遍 Agent.Shutdown。信号处理先于
	// 其他组件安装，启动窗口内到达的 SIGTERM 也走优雅关停。
	signals := []os.Signal{syscall.SIGTERM}
	if cfg.Mode == modeLocal {
		signals = append(signals, os.Interrupt)
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()

	// 就绪探针先于其余组件启动：Substrate 的 wakeupProbe 只等看守进程起来，
	// 不以连上控制面为条件。
	var probe *readyz.Server
	if cfg.ReadyZAddr != "" {
		probe, err = readyz.Start(cfg.ReadyZAddr)
		if err != nil {
			logger.Error("start readyz", "error", err)
			return 1
		}
		defer func() { _ = probe.Close() }()
		logger.Info("readyz listening", "addr", probe.Addr().String())
	}

	// 身份：本地模式取 --run-id，actor 模式取 systemInfo 卷投射的 actor 名。
	runID := cfg.RunID
	if cfg.Mode == modeActor {
		runID, err = identity.Read(actorNamePath)
		if err != nil {
			logger.Error("read actor identity", "error", err)
			return 1
		}
	}

	// 是否启用 PID 1 回收器只看进程号是否为 1，与运行方式无关。
	isPID1 := os.Getpid() == 1
	runner := selectRunner(isPID1, logger)

	client := controlstream.New(controlstream.Config{
		Address: cfg.ControlPlane,
		RunID:   runID,
	})
	driver := claudecode.New(claudecode.Config{
		ClaudeBin: cfg.ClaudeBin,
		Home:      cfg.Home,
		ActorMode: cfg.Mode == modeActor,
		Logger:    logger,
	}, client.Sink(), runner)

	// SIGTERM 与本地模式的 SIGINT 已在上文安装。

	logger.Info("warden starting",
		"mode", cfg.Mode.String(),
		"run_id", runID,
		"control_plane", cfg.ControlPlane,
		"home", cfg.Home,
		"pid1", isPID1)

	runErr := client.Run(ctx, driver)
	exitCode := 0
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		logger.Error("control stream stopped", "error", runErr)
		exitCode = 1
	}

	// 关停使用独立的上下文：信号上下文此时已取消，不能再用它限制关停步骤。
	// 上下文有期限，关停升级与并发的第二次 Shutdown 都不会无限等待。
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := driver.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown Claude Code", "error", err)
	}
	logger.Info("warden stopped")
	return exitCode
}

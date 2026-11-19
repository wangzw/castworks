package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// 退出码：正常信号退出 0，启动失败或运行中服务意外退出 1，命令行参数错误 2，
// --help 为 0。
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(runMain())
}

// runMain 设置一次 slog 默认日志器、处理 SIGINT 与 SIGTERM，然后调用 run。
func runMain() int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// One channel receives the signals from the very beginning. The first one
	// starts a graceful shutdown bounded by --shutdown-timeout; the second
	// exits immediately with a failure status. Registering the channel here,
	// rather than from a callback that runs after the first signal, keeps a
	// second signal from being lost while that callback is being scheduled.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
		case <-done:
			return
		}
		cancel()
		select {
		case <-sigs:
			os.Exit(exitFailure)
		case <-done:
		}
	}()

	err := run(ctx, os.Args[1:], nil)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		// flag already printed the usage text.
		return exitOK
	case isUsageError(err):
		// Flag syntax errors have already been printed by the flag package;
		// validation errors have not. Printing again is harmless and keeps the
		// latter visible.
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	default:
		slog.Default().Error("control plane exited", "error", err)
		return exitFailure
	}
}

// isUsageError reports whether err came from parsing or validating the
// command line.
func isUsageError(err error) bool {
	var ue *usageError
	return errors.As(err, &ue)
}

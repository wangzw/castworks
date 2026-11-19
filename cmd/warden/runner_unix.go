//go:build unix

package main

import (
	"log/slog"

	"github.com/wangzw/castworks/internal/warden/agent"
	"github.com/wangzw/castworks/internal/warden/claudecode"
	"github.com/wangzw/castworks/internal/warden/pid1"
)

// 编译期断言：PID 1 回收器与默认的 cmd.Wait 实现都满足 agent.ProcessRunner。
var (
	_ agent.ProcessRunner = (*pid1.Reaper)(nil)
	_ agent.ProcessRunner = claudecode.DefaultProcessRunner{}
)

// selectRunner 在进程号是 1 时启用 PID 1 回收器并返回它，否则返回以
// cmd.Wait 等待子进程的默认实现。回收器必须在启动任何子进程之前启用。
func selectRunner(isPID1 bool, logger *slog.Logger) agent.ProcessRunner {
	if isPID1 {
		reaper := pid1.New(logger)
		reaper.Enable()
		return reaper
	}
	return claudecode.DefaultProcessRunner{}
}

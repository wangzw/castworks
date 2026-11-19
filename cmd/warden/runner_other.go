//go:build !unix

package main

import (
	"log/slog"

	"github.com/wangzw/castworks/pkg/warden/agent"
	"github.com/wangzw/castworks/pkg/warden/claudecode"
)

// 编译期断言：默认的 cmd.Wait 实现满足 agent.ProcessRunner。
var _ agent.ProcessRunner = claudecode.DefaultProcessRunner{}

// selectRunner 返回以 cmd.Wait 等待子进程的默认实现。非 unix 平台没有
// PID 1 回收器，进程号是 1 时只能退回默认实现。
func selectRunner(isPID1 bool, logger *slog.Logger) agent.ProcessRunner {
	if isPID1 {
		logger.Warn("pid1 reaper is unavailable on this platform; using the default process runner")
	}
	return claudecode.DefaultProcessRunner{}
}

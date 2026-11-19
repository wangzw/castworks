//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

// 本地模式验收：README“实现顺序”第 4 步与 warden.md“测试”一节的
// “本地模式端到端”。
//
// 控制面以 --provider local 运行，看守进程以 --run-id 连接它，并通过
// --claude-bin 指向一个 strace 包装脚本，顺带得到 Claude Code 访问的主机
// 清单。测试发四组消息：写文件的第一个回合、只引用“刚才那个文件”的第二
// 个回合、产生自发回合的后台命令，以及让后台通知在回合进行中到达的一组。
// 结束后检查看守进程以 0 退出，并把 stream.jsonl 复制到工作目录，作为真实
// 样本的原料。
//
// 这一项消耗 LLM 额度，只在 make acceptance-local 下运行。
//
// 一次完整运行大约 6 个回合：四组消息加上后台命令的自发回合与排队通知。
// 某一步失败后修好，同一步最多重跑一次，避免反复消耗额度。

const (
	localEnvCredentialFile = "CASTWORKS_ACC_CREDENTIAL_FILE"
	localEnvClaudeBin      = "CASTWORKS_ACC_CLAUDE_BIN"

	// localRunTimeout 比 make acceptance-local 的 -timeout 30m 短 5 分钟。
	// 到期时测试失败并走 t.Cleanup，而不是让 go test 在 -timeout 上直接 panic。
	localRunTimeout = 25 * time.Minute
	// localTurnTimeout 是单个普通回合的上限，后台命令一组放宽到这里。
	localTurnTimeout = 6 * time.Minute
	// localSpontaneousTimeout 是等待后台命令触发自发回合的上限。
	localSpontaneousTimeout = 3 * time.Minute
	// localWardenExitWait 是 End 之后等看守进程退出的上限。
	localWardenExitWait = time.Minute
	// localQuiet 是判定回合边界之后等待“不再有新回合”的时长。
	localQuiet = 5 * time.Second
)

// TestLocal 运行本地模式的端到端验收。
func TestLocal(t *testing.T) {
	ctx := testContext(t, localRunTimeout)

	requireEnv(t, envDatabaseURL)
	credentialFile := requireEnv(t, localEnvCredentialFile)
	claudeBin := requireEnv(t, localEnvClaudeBin)
	// 只检查文件存在，不打开凭据文件：它的内容只由控制面读取。
	for name, path := range map[string]string{
		"凭据文件": credentialFile, "Claude Code": claudeBin,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s %s 不可用：%v", name, path, err)
		}
	}

	pool := pgtest.NewPool(t)

	// 观察步骤放在 t.Cleanup 里，并在 newHarness 之前注册。Cleanup 按后
	// 进先出执行，因此它们会在 harness 的清理（结束运行、停掉子进程）之后
	// 运行；即使前面某一步 Fatal，主机清单与样本原料也不会丢。
	var (
		h         *harness
		tracePath string
	)
	t.Cleanup(func() {
		if h == nil || h.warden == nil || tracePath == "" {
			return
		}
		localSaveStream(t, h)
		localCheckHosts(t, h, tracePath)
	})

	h = newHarness(t, harnessOptions{
		DatabaseURL: pool.Config().ConnString(),
		Provider:    "local",
	})
	h.buildBinaries()

	h.startControlPlane(ctx,
		"--provider", "local",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialFile,
	)

	nonce := "castworks-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	markerName := "marker-" + nonce + ".txt"
	runID := h.createRun(ctx, fmt.Sprintf(
		"Create a file named %s in the current working directory. "+
			"Its contents must be exactly %s and nothing else. Then reply with the single word ready.",
		markerName, nonce))

	tracePath = filepath.Join(h.workDir, "hostwatch.strace")
	wrapper := writeHostwatchWrapper(t, h.workDir, claudeBin, tracePath)
	h.startWardenLocal(runID, wrapper)

	// 第一个回合：写文件。结束时先确认文件真的落在 <home>/work 下。
	turnCtx, cancel := context.WithTimeout(ctx, localTurnTimeout)
	first := h.waitTurnBoundary(turnCtx, runID, 1)
	cancel()
	localRequireTurn(t, first.Turns[0], 1)
	localRequireMarker(t, filepath.Join(h.workDir, "home", "work", markerName), nonce)

	// 第二回合前在工作目录里放一个内容不同的诱饵文件。提示只说“刚才那个
	// 文件”，判定仍看随机标记；诱饵内容不含标记，读到它就无法通过。
	decoyContent := "decoy-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	decoyPath := filepath.Join(h.workDir, "home", "work", "decoy.txt")
	if err := os.WriteFile(decoyPath, []byte(decoyContent+"\n"), 0o600); err != nil {
		t.Fatalf("写入诱饵文件 %s：%v", decoyPath, err)
	}

	// 第二个回合：只引用“刚才那个文件”，不重复文件名与标记。
	h.send(ctx, runID, "Read the file you just wrote and reply with its exact contents, and nothing else.")
	turnCtx, cancel = context.WithTimeout(ctx, localTurnTimeout)
	second := h.waitTurnBoundary(turnCtx, runID, 2)
	cancel()
	localRequireTurn(t, second.Turns[0], 1)
	localRequireTurn(t, second.Turns[1], 2)
	if !strings.Contains(second.Turns[1].Output, nonce) {
		t.Fatalf("第二个回合的输出没有包含标记 %q：%s", nonce, localExcerpt(second.Turns[1].Output, 400))
	}

	// 第三个回合：后台跑约 20 s 的命令并立即结束回合，等自发回合出现。
	// 自发回合是观察项：localSpontaneousTimeout 内没有出现只记录现象，
	// 继续走后面的组，不判失败。
	h.send(ctx, runID, "Start a command in the background that sleeps for about 20 seconds and then exits. "+
		"Reply with the single word started as soon as the background command is running, and end the turn without waiting for it to finish.")
	spontaneousCtx, cancel := context.WithTimeout(ctx, localSpontaneousTimeout)
	spontaneous, spontaneousErr := localWaitQuietBoundaryErr(spontaneousCtx, h, runID, 4)
	cancel()
	if spontaneousErr != nil {
		t.Logf("自发回合没有在 %s 内出现：%v；只记录现象，继续后面的组", localSpontaneousTimeout, spontaneousErr)
	}
	for i := 2; i < len(spontaneous.Turns); i++ {
		localLogTurn(t, fmt.Sprintf("第 %d 个回合", i+1), spontaneous.Turns[i])
	}

	// 第四个回合：后台 5 s 通知在 30 s 前台命令进行中到达，产生排队。
	baseTurns := len(spontaneous.Turns)
	h.send(ctx, runID, "Start a command in the background that sleeps for about 5 seconds and then exits. "+
		"Then run a foreground command that sleeps for about 30 seconds. "+
		"Reply with the single word finished when the foreground command ends.")
	turnCtx, cancel = context.WithTimeout(ctx, localTurnTimeout)
	queued := localWaitQuietBoundary(t, h, turnCtx, runID, baseTurns+1)
	cancel()
	for i := baseTurns; i < len(queued.Turns); i++ {
		localLogTurn(t, fmt.Sprintf("第 %d 个回合", i+1), queued.Turns[i])
	}

	// End；看守进程应正常以 0 退出。
	if err := h.endRun(ctx, runID); err != nil {
		t.Fatalf("结束运行实例：%v", err)
	}
	if err := h.waitChild(h.warden, localWardenExitWait); err != nil {
		t.Fatalf("看守进程没有以 0 退出：%v", err)
	}

	localSaveStream(t, h)
	localCheckHosts(t, h, tracePath)
}

// localRequireTurn 断言一个回合正常结束且没有错误。
func localRequireTurn(t *testing.T, tv turn, number int) {
	t.Helper()
	if tv.EndedAt == nil {
		t.Fatalf("第 %d 个回合没有 ended_at", number)
	}
	if tv.IsError {
		t.Fatalf("第 %d 个回合 is_error 为 true：%s", number, localExcerpt(tv.Output, 400))
	}
}

// localRequireMarker 确认第一个回合在工作目录里写下了内容为标记的文件。
func localRequireMarker(t *testing.T, path, nonce string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("第一个回合没有在工作目录里留下 %s：%v", filepath.Base(path), err)
	}
	if got := strings.TrimSpace(string(content)); got != nonce {
		t.Fatalf("%s 的内容为 %q，想要 %q", filepath.Base(path), got, nonce)
	}
}

// localLogTurn 记录一个回合的结果。后台命令与排队通知两组只作观察，不因
// is_error 判定失败，失败情形留给样本录制与架构决策。
func localLogTurn(t *testing.T, label string, tv turn) {
	t.Helper()
	t.Logf("%s：is_error=%t，输出=%s", label, tv.IsError, localExcerpt(tv.Output, 240))
}

// localExcerpt 截断一段输出，避免日志过长。
func localExcerpt(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "……"
}

// localWaitQuietBoundary 先等第 atLeast 个回合结束并回到 boundary，再观察
// 一段安静时间；期间若又出现新回合（例如背景通知又触发一次），继续等它
// 结束，安静时间重新计算。等待失败时判测试失败。
func localWaitQuietBoundary(t *testing.T, h *harness, ctx context.Context, runID string, atLeast int) runDetail {
	t.Helper()
	detail, err := localWaitQuietBoundaryErr(ctx, h, runID, atLeast)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return detail
}

// localWaitQuietBoundaryErr 是 localWaitQuietBoundary 的错误返回版本，
// 供只观察、不判失败的步骤使用。超时时返回最近一次读到的详情。
func localWaitQuietBoundaryErr(ctx context.Context, h *harness, runID string, atLeast int) (runDetail, error) {
	detail, err := h.waitTurnBoundaryErr(ctx, runID, atLeast)
	if err != nil {
		return detail, err
	}
	deadline := time.Now().Add(localQuiet)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return detail, fmt.Errorf("等待回合边界稳定：%w", ctx.Err())
		case <-time.After(pollInterval):
		}
		current, err := h.getRun(ctx, runID)
		if err != nil {
			return detail, fmt.Errorf("读取运行实例：%w", err)
		}
		if current.State == "failed" {
			return detail, fmt.Errorf("运行实例失败：%s", current.FailureReason)
		}
		if len(current.Turns) == len(detail.Turns) && current.Substate == "boundary" {
			continue
		}
		detail, err = h.waitTurnBoundaryErr(ctx, runID, len(current.Turns))
		if err != nil {
			return detail, err
		}
		deadline = time.Now().Add(localQuiet)
	}
	return detail, nil
}

// localSaveStream 把 <home>/.castworks/stream.jsonl 复制到工作目录，作为
// 真实样本的原料，并记录其中是否已有 queued_turn_count 大于 0 的 result。
// 它从 t.Cleanup 运行，失败用 Errorf 记下，不再中断后续清理。
func localSaveStream(t *testing.T, h *harness) {
	t.Helper()
	streamPath := filepath.Join(h.workDir, "home", ".castworks", "stream.jsonl")
	stream, err := os.ReadFile(streamPath)
	if err != nil {
		t.Errorf("读取 %s：%v", streamPath, err)
		return
	}
	if len(bytes.TrimSpace(stream)) == 0 {
		t.Errorf("%s 为空", streamPath)
		return
	}
	samplePath := filepath.Join(h.workDir, "stream.jsonl")
	if err := os.WriteFile(samplePath, stream, 0o600); err != nil {
		t.Errorf("写入 %s：%v", samplePath, err)
		return
	}
	t.Logf("样本原料已复制到 %s（%d 字节）", samplePath, len(stream))
	if localStreamHasQueuedTurn(stream) {
		t.Logf("stream.jsonl 中有 queued_turn_count 大于 0 的 result，可直接用于样本录制")
	} else {
		t.Logf("stream.jsonl 中没有 queued_turn_count 大于 0 的 result，按计划改用 hack/acceptance/record 录制")
	}
}

// localStreamHasQueuedTurn 判断 stream.jsonl 中是否出现了排队未清的 result。
func localStreamHasQueuedTurn(stream []byte) bool {
	for _, line := range strings.Split(string(stream), "\n") {
		var msg struct {
			Type            string `json:"type"`
			QueuedTurnCount *int   `json:"queued_turn_count"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Type == "result" && msg.QueuedTurnCount != nil && *msg.QueuedTurnCount > 0 {
			return true
		}
	}
	return false
}

// localCheckHosts 解析 strace 文件，写出主机清单，并按计划只在清单为
// api.anthropic.com:443 一种时通过。它从 t.Cleanup 运行，失败用 Errorf
// 记下，不再中断后续清理。
func localCheckHosts(t *testing.T, h *harness, tracePath string) {
	t.Helper()
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Errorf("读取 strace 文件 %s：%v", tracePath, err)
		return
	}
	obs := parseHostwatch(trace, h.grpcAddr)
	reportPath := filepath.Join(h.workDir, "hosts.txt")
	if err := os.WriteFile(reportPath, []byte(obs.report()), 0o644); err != nil {
		t.Errorf("写入主机清单 %s：%v", reportPath, err)
		return
	}
	t.Logf("主机清单（%s）：\n%s", reportPath, obs.report())
	if !obs.allowed() {
		t.Errorf("主机清单不是只有 api.anthropic.com:443。按计划停下，把 %s 发给 castworks-architect，"+
			"确认是否加入出口策略后再开始 Substrate 两回合验收。", reportPath)
	}
}

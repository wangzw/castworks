//go:build acceptance

package acceptance

// 本地模式订阅验收与样本录制：README“实现顺序”第 3 步与
// warden.md“测试”一节的“本地模式端到端”。
//
// 控制面以 --provider local 运行并接入 connectorhub，extsim 固定监听
// 127.0.0.1:9100，看守进程以本地模式驱动真实的 Claude Code。Claude Code
// 先提交一份缺少 owner 的内容，收到 failed 的订阅完成事件后改正重交，再
// 收到 succeeded 并报告结果；随后在同一个运行实例里把 extsim job watch
// 放到后台运行，录制由空变非空再变空的后台作业集合与自发回合。
//
// 判定只用 collectCriteria 与 judgeRoundTrip。本运行不打开挂起（本地模式
// 强制关闭），因此 criteriaCase.SuspensionDisabled 让判定跳过与挂起有关的
// 检查，其余检查仍跑在真实记录上。这一项消耗 LLM 额度，只在
// make acceptance-local-subscription 下运行。

import (
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

const (
	localSubscriptionRunTimeout = 22 * time.Minute
	// localSubscriptionRoundTripTimeout 是两次往返加报告回合的上限，计划
	// 要求 15 分钟。
	localSubscriptionRoundTripTimeout = 15 * time.Minute
	// localSubscriptionBackgroundTimeout 是后台回合加自发回合的上限。
	localSubscriptionBackgroundTimeout = 3 * time.Minute
	localSubscriptionExitWait          = time.Minute
	localSubscriptionJobDuration       = 20 * time.Second

	localSubscriptionCompletionPrefix  = "[castworks] Subscription "
	localSubscriptionToolListEvents    = "mcp__castworks__list_events"
	localSubscriptionToolSubscribe     = "mcp__castworks__subscribe_event"
	localSubscriptionBackgroundSummary = "background_tasks.txt"
)

// TestLocalSubscription 跑通本地模式下的两轮订阅与后台作业录制。
func TestLocalSubscription(t *testing.T) {
	ctx := testContext(t, localSubscriptionRunTimeout)

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

	// 真实运行必须从一个干净提交开始，证据才对得上代码。
	repoHead := subscriptionRequireCleanRepo(t)

	pool := pgtest.NewPool(t)

	// 观察步骤放在 t.Cleanup 里，并在 newHarness 之前注册。Cleanup 按后
	// 进先出执行，因此它们在 harness 的清理之后运行；即使前面某一步 Fatal，
	// 主机清单与样本原料也不会丢。
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
	if err := os.WriteFile(filepath.Join(h.workDir, "repo-head"), []byte(repoHead+"\n"), 0o644); err != nil {
		t.Fatalf("写入运行目录的 repo-head：%v", err)
	}

	signingKey := h.generateSigningKey()
	configPath := h.writeConnectorhubConfig("", signingKey, "http://"+localExtsimJobsAddr)

	h.startControlPlane(ctx,
		"--provider", "local",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--connectorhub-config", configPath,
		"--connector-addr", "127.0.0.1:0",
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialFile,
	)
	h.startExtsim(ctx, extsimOptions{
		Listen:      localExtsimJobsAddr,
		JobDuration: localSubscriptionJobDuration,
	})

	marker := "castworks-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	runID := h.createRun(ctx, localSubscriptionPrompt(marker))
	m := newSuspendScenario("LocalSubscription")
	m.RunID = runID
	m.RepoHead = repoHead
	m.RepoCommit = repoHead
	m.addNote("tested repo HEAD: %s", repoHead)
	// 判定失败时也把已经采集到的记录写到 measurements.json。
	t.Cleanup(func() {
		if m.Written {
			return
		}
		m.finish(t, h.workDir)
	})

	tracePath = filepath.Join(h.workDir, "hostwatch.strace")
	wrapper := writeHostwatchWrapper(t, h.workDir, claudeBin, tracePath)
	h.startWardenLocal(runID, wrapper)

	// 第一段：两次往返与报告回合。
	roundCtx, cancel := context.WithTimeout(ctx, localSubscriptionRoundTripTimeout)
	detail := h.waitLocalSubscriptionReport(roundCtx, runID)
	cancel()
	for i, tv := range detail.Turns {
		localLogTurn(t, fmt.Sprintf("第 %d 个回合", i+1), tv)
	}

	c := h.collectCriteria(t, ctx, runID)
	c.Expect.InitialMarker = marker
	// 本地模式强制关闭挂起，没有挂起记录可查；其余判定照常。
	c.SuspensionDisabled = true
	recordCriteriaRecords(m, c, "")
	m.addCriteria("initial_marker", marker)
	m.addCriteria("turns", turnRecordEvidenceOf(detail.Turns))
	m.addCriteria("suspension_disabled", true)
	assertJudge(t, "round trip", judgeRoundTrip(c))

	stream := h.readStreamJSONL(t)
	if mismatch := localSubscriptionCheckRoundTripStream(stream); mismatch != "" {
		t.Fatalf("stream.jsonl：%s", mismatch)
	}
	localRequireSubscriptionCompletions(t, h, 2)

	// 在花额度跑后台回合之前先看一次主机清单：出现 api.anthropic.com:443
	// 之外的主机就停下，不再继续。
	if !localSubscriptionHostsAllowed(t, h, tracePath) {
		t.Fatalf("主机清单不是只有 api.anthropic.com:443，按计划停下回问，"+
			"不再跑后台回合。清单见 %s", filepath.Join(h.workDir, "hosts.txt"))
	}

	// 第二段：后台作业与自发回合。
	observer, err := h.startBGJobsObserver(ctx, runID)
	if err != nil {
		t.Fatalf("启动 bg_jobs 观察器：%v", err)
	}
	h.send(ctx, runID, localSubscriptionBackgroundPrompt())

	bgCtx, bgCancel := context.WithTimeout(ctx, localSubscriptionBackgroundTimeout)
	localWaitQuietBoundary(t, h, bgCtx, runID, len(detail.Turns)+2)
	if err := waitFor(bgCtx, "bg_jobs 由空变非空再变空", func() (bool, error) {
		return localSubscriptionBGJobsSawEmptyAfterNonEmpty(observer.Changes()), nil
	}); err != nil {
		bgCancel()
		observer.stop()
		t.Fatalf("%v；观察记录：%v", err, localSubscriptionFormatBGJobs(observer.Changes()))
	}
	bgCancel()
	observer.stop()
	if err := observer.Err(); err != nil {
		t.Fatalf("bg_jobs 观察器停止：%v", err)
	}
	turns, err := h.readTurns(ctx, runID)
	if err != nil {
		t.Fatalf("读取回合记录：%v", err)
	}
	changes := observer.Changes()
	m.addCriteria("bg_jobs", changes)
	m.addCriteria("background_turns", turnRecordEvidenceOf(turns))
	background := map[string]any{}
	if emptiedAt, ok := criteriaFirstEmptyAfterNonEmpty(changes); ok {
		background["emptied_at"] = emptiedAt
		if spontaneous, ok := criteriaFirstTurnStartedAfter(turns, emptiedAt.Add(-defaultBGJobsInterval)); ok {
			background["spontaneous_turn_id"] = spontaneous.TurnID
			background["spontaneous_turn_delivery_id"] = spontaneous.DeliveryID
			background["spontaneous_turn_started_at"] = spontaneous.StartedAt
		}
	}
	m.addCriteria("background_job", background)
	if mismatch := localSubscriptionJudgeBGJobs(observer.Changes()); mismatch != "" {
		t.Fatalf("后台作业判定：%s\n观察记录：%s", mismatch, localSubscriptionFormatBGJobs(observer.Changes()))
	}
	for _, change := range observer.Changes() {
		t.Logf("bg_jobs 变化于 %s：%v", change.At.Format(time.RFC3339Nano), change.Jobs)
	}

	// 记录 background_tasks_changed 是否出现，以及任务是否带 ambient 字段。
	stream = h.readStreamJSONL(t)
	obs := summarizeLocalSubscriptionBackgroundTasks(stream)
	if err := os.WriteFile(filepath.Join(h.workDir, localSubscriptionBackgroundSummary),
		[]byte(obs.report()), 0o600); err != nil {
		t.Fatalf("写入 %s：%v", localSubscriptionBackgroundSummary, err)
	}
	t.Logf("后台任务观察：%s", obs.report())

	// End；看守进程应正常以 0 退出。
	if err := h.endRun(ctx, runID); err != nil {
		t.Fatalf("结束运行实例：%v", err)
	}
	if err := h.waitChild(h.warden, localSubscriptionExitWait); err != nil {
		t.Fatalf("看守进程没有以 0 退出：%v", err)
	}

	for i := range turns {
		m.addTurn(turns[i], i+1, false, nil, nil)
	}
	m.finish(t, h.workDir)
	localSaveStream(t, h)
	localCheckHosts(t, h, tracePath)
}

// localSubscriptionPrompt 按演示场景第 1 步写提示：初始内容含随机标记、
// 不含 owner；不通过就按原因改正重交，直到通过；用平台订阅工具等待结果；
// 最后按判定第 1 条要求的固定格式报告。
func localSubscriptionPrompt(marker string) string {
	return fmt.Sprintf(`Use the extsim command line to validate this JSON content:
{"note":%q}

The content intentionally has no owner field. Submit it with "extsim job submit" (write it to a file and pass --file). If the job fails validation, read the reason, correct the content, and submit it again until it passes.

After every submission, call the platform MCP tool %q with system "extsim", then wait for the job with %q, using event "job.completed" and correlation {"job.id": "<job id>"}. End the turn right after subscribing; the completion event will arrive as your next input.

When the second job has succeeded, end your final turn with a report in exactly this format, copying each value from the jobs:
first job: <first job id>
first reason: <the first job's failure reason, exactly as extsim reported it>
marker: %s
second job: <second job id>
second outcome: <the second job's conclusion>

Do not add anything after the report.`, marker, localSubscriptionToolListEvents,
		localSubscriptionToolSubscribe, marker)
}

// localSubscriptionBackgroundPrompt 让 Claude Code 再提交一次作业，以后台
// 方式运行 extsim job watch，并立即结束回合。
func localSubscriptionBackgroundPrompt() string {
	marker := "castworks-bg-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return fmt.Sprintf(`Submit this JSON content to extsim for validation and make sure it passes:
{"note":%q,"owner":"castworks"}

Then start "extsim job watch <job id>" as a background Bash task (use the run_in_background option). As soon as the background task is running, reply with the single word started and end the turn without waiting for it to finish. Do not subscribe to this job.`, marker)
}

// waitLocalSubscriptionReport 等到 2 条 subscription_completed 收件箱行之后
// 又出现一个已经结束的回合，并回到 boundary。
func (h *harness) waitLocalSubscriptionReport(ctx context.Context, runID string) runDetail {
	h.t.Helper()
	var detail runDetail
	err := waitFor(ctx, "两次往返完成并等到报告回合结束", func() (bool, error) {
		current, err := h.getRun(ctx, runID)
		if err != nil {
			return false, err
		}
		detail = current
		if current.State == "failed" {
			return false, fmt.Errorf("运行实例失败：%s", current.FailureReason)
		}
		inbox, err := h.readInbox(ctx, runID)
		if err != nil {
			return false, err
		}
		rows := criteriaCompletionInbox(inbox)
		if len(rows) < 2 {
			return false, nil
		}
		if current.Substate != "boundary" {
			return false, nil
		}
		for _, tv := range current.Turns {
			if tv.EndedAt != nil && !tv.StartedAt.Before(rows[1].CreatedAt) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		h.t.Fatalf("acceptance: %v", err)
	}
	return detail
}

// readStreamJSONL 读取看守进程录下的 stream.jsonl。进程运行期间也可以读，
// 已写入的行就是当前可用的全部记录。
func (h *harness) readStreamJSONL(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(h.workDir, "home", ".castworks", "stream.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s：%v", path, err)
	}
	return data
}

// localSubscriptionCheckRoundTripStream 检查 stream.jsonl 中有两个平台 MCP
// 工具的 tool_use，以及执行 extsim job submit 的 Bash 调用。
func localSubscriptionCheckRoundTripStream(stream []byte) string {
	var (
		listEvents bool
		subscribe  bool
		submit     bool
		tools      []string
	)
	for _, line := range strings.Split(string(stream), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Command string `json:"command"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Type != "assistant" {
			continue
		}
		for _, block := range msg.Message.Content {
			if block.Type != "tool_use" {
				continue
			}
			tools = append(tools, block.Name)
			switch block.Name {
			case localSubscriptionToolListEvents:
				listEvents = true
			case localSubscriptionToolSubscribe:
				subscribe = true
			case "Bash":
				if strings.Contains(block.Input.Command, "extsim job submit") {
					submit = true
				}
			}
		}
	}
	var missing []string
	if !listEvents {
		missing = append(missing, localSubscriptionToolListEvents)
	}
	if !subscribe {
		missing = append(missing, localSubscriptionToolSubscribe)
	}
	if !submit {
		missing = append(missing, "执行 extsim job submit 的 Bash 调用")
	}
	if len(missing) > 0 {
		return fmt.Sprintf("缺少 %s；已有的 tool_use 为 %v", strings.Join(missing, "、"), tools)
	}
	return ""
}

// localRequireSubscriptionCompletions 在 Claude Code 的会话记录里数投递给
// 它的订阅完成事件文本，要求恰好 want 次。会话记录在 HOME/.claude/projects
// 下，用户消息是 type=user 且 message.content 为字符串的项。
func localRequireSubscriptionCompletions(t *testing.T, h *harness, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var last int
	for {
		got, err := countLocalSubscriptionCompletionMessages(h)
		if err != nil {
			t.Fatalf("读取 Claude Code 会话记录：%v", err)
		}
		last = got
		if got == want {
			t.Logf("投递给 Claude Code 的订阅完成事件文本：%d 次", got)
			return
		}
		if got > want {
			t.Fatalf("投递给 Claude Code 的订阅完成事件文本出现了 %d 次，想要 %d 次", got, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待订阅完成事件文本达到 %d 次超时，最后一次计数为 %d", want, last)
		}
		time.Sleep(pollInterval)
	}
}

// countLocalSubscriptionCompletionMessages 统计会话记录中用户消息里的订阅
// 完成事件文本出现次数。
func countLocalSubscriptionCompletionMessages(h *harness) (int, error) {
	paths, err := filepath.Glob(filepath.Join(h.workDir, "home", ".claude", "projects", "*", "*.jsonl"))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var msg struct {
				Type    string `json:"type"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if msg.Type != "user" {
				continue
			}
			count += strings.Count(localUserMessageText(msg.Message.Content),
				localSubscriptionCompletionPrefix)
		}
	}
	return count, nil
}

// localUserMessageText 取出用户消息的文本；tool_result 与其他块不计入。
func localUserMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// localSubscriptionHostsAllowed 读一次 strace 文件，写出主机清单，返回清单
// 是否只有 api.anthropic.com:443。
func localSubscriptionHostsAllowed(t *testing.T, h *harness, tracePath string) bool {
	t.Helper()
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("读取 strace 文件 %s：%v", tracePath, err)
	}
	obs := parseHostwatch(trace, h.grpcAddr)
	reportPath := filepath.Join(h.workDir, "hosts.txt")
	if err := os.WriteFile(reportPath, []byte(obs.report()), 0o600); err != nil {
		t.Fatalf("写入主机清单 %s：%v", reportPath, err)
	}
	t.Logf("主机清单（%s）：\n%s", reportPath, obs.report())
	return obs.allowed()
}

// localSubscriptionBGJobsSawEmptyAfterNonEmpty 判断观察记录里是否已经出现
// 先非空、后变空的变化。
func localSubscriptionBGJobsSawEmptyAfterNonEmpty(changes []bgJobsChange) bool {
	return localSubscriptionJudgeBGJobs(changes) == ""
}

// localSubscriptionJudgeBGJobs 判定 bg_jobs 是否由空变非空再变空。
func localSubscriptionJudgeBGJobs(changes []bgJobsChange) string {
	if len(changes) == 0 {
		return "没有 bg_jobs 观察记录"
	}
	if len(changes[0].Jobs) != 0 {
		return fmt.Sprintf("第一次观察到的 bg_jobs 为 %v，想要空集", changes[0].Jobs)
	}
	sawNonEmpty := false
	for _, change := range changes[1:] {
		if len(change.Jobs) > 0 {
			sawNonEmpty = true
			continue
		}
		if sawNonEmpty {
			return ""
		}
	}
	if !sawNonEmpty {
		return "bg_jobs 从未变为非空"
	}
	return "bg_jobs 变为非空之后没有再变回空集"
}

// localSubscriptionFormatBGJobs 把观察记录写成一行，供失败日志使用。
func localSubscriptionFormatBGJobs(changes []bgJobsChange) string {
	var parts []string
	for _, change := range changes {
		parts = append(parts, fmt.Sprintf("%s=%v", change.At.Format(time.RFC3339Nano), change.Jobs))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// localBackgroundTaskObservation 记录 background_tasks_changed 与任务上
// ambient 字段的观察结果。
type localBackgroundTaskObservation struct {
	changedMessages  int
	startedMessages  int
	notificationMsgs int
	tasks            []localBackgroundTaskField
}

type localBackgroundTaskField struct {
	source  string
	taskID  string
	present bool
	value   bool
}

func (o localBackgroundTaskObservation) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "background_tasks_changed messages: %d\n", o.changedMessages)
	fmt.Fprintf(&b, "task_started messages: %d\n", o.startedMessages)
	fmt.Fprintf(&b, "task_notification messages: %d\n", o.notificationMsgs)
	for _, task := range o.tasks {
		fmt.Fprintf(&b, "%s task %s: ambient present=%t value=%t\n",
			task.source, task.taskID, task.present, task.value)
	}
	if len(o.tasks) == 0 {
		b.WriteString("no task carried an ambient field in either message\n")
	}
	return b.String()
}

// summarizeLocalSubscriptionBackgroundTasks 从 stream.jsonl 统计后台任务
// 消息，并记录每个任务是否带 ambient 字段及取值。
func summarizeLocalSubscriptionBackgroundTasks(stream []byte) localBackgroundTaskObservation {
	var obs localBackgroundTaskObservation
	for _, line := range strings.Split(string(stream), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			TaskID  string `json:"task_id"`
			Tasks   []struct {
				TaskID  string `json:"task_id"`
				Ambient *bool  `json:"ambient"`
			} `json:"tasks"`
			Ambient *bool `json:"ambient"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Type != "system" {
			continue
		}
		switch msg.Subtype {
		case "background_tasks_changed":
			obs.changedMessages++
			for _, task := range msg.Tasks {
				obs.tasks = append(obs.tasks, localBackgroundTaskField{
					source:  "background_tasks_changed",
					taskID:  task.TaskID,
					present: task.Ambient != nil,
					value:   task.Ambient != nil && *task.Ambient,
				})
			}
		case "task_started":
			obs.startedMessages++
			obs.tasks = append(obs.tasks, localBackgroundTaskField{
				source:  "task_started",
				taskID:  msg.TaskID,
				present: msg.Ambient != nil,
				value:   msg.Ambient != nil && *msg.Ambient,
			})
		case "task_notification":
			obs.notificationMsgs++
		}
	}
	return obs
}

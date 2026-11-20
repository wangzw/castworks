package claudecode

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

// feedAll 把样本的全部行交给驱动器的处理路径。
func feedAll(t *testing.T, driver *Driver, lines []string) {
	t.Helper()
	for _, line := range lines {
		feed(t, driver, line)
	}
}

// bgJobEvents 返回事件序列中的 BgJobsChanged 事件。
func bgJobEvents(events []sinkEvent) []sinkEvent {
	var out []sinkEvent
	for _, event := range events {
		if event.Kind == eventBgJobsChanged {
			out = append(out, event)
		}
	}
	return out
}

// currentBgJobs 返回 Snapshot 中的后台作业集合。
func currentBgJobs(driver *Driver) []string {
	var state agent.State
	driver.Snapshot(func(s agent.State) { state = s })
	return state.BgJobs
}

func taskStartedLine(taskID string, backgrounded, ambient bool) string {
	return jsonLine(map[string]any{
		"type": "system", "subtype": "task_started", "task_id": taskID,
		"is_backgrounded": backgrounded, "ambient": ambient,
		"session_id": "ses-bg", "uuid": "u-task-started",
	})
}

func taskNotificationLine(taskID, status string) string {
	return jsonLine(map[string]any{
		"type": "system", "subtype": "task_notification", "task_id": taskID,
		"status": status, "session_id": "ses-bg", "uuid": "u-task-notification",
	})
}

// TestBgJobsRecordedSequence 顺序回放 local_turn03_deliver 与
// local_turn04_spontaneous：turn03 启动的后台任务进入集合，turn04 开头的终态
// 通知与空集推送之后集合为空，且 BgJobsChanged 的调用次数与集合变化次数一致。
func TestBgJobsRecordedSequence(t *testing.T) {
	turn03 := readRecordedRawLines(t, filepath.Join(recordedSampleDir, "local_turn03_deliver.jsonl"))
	turn04 := readRecordedRawLines(t, filepath.Join(recordedSampleDir, "local_turn04_spontaneous.jsonl"))

	driver, rec := newBareDriver(t)
	if got := currentBgJobs(driver); len(got) != 0 {
		t.Fatalf("bg jobs before start = %v, want empty", got)
	}
	simulateDeliver(driver, "delivery-turn03")
	feedAll(t, driver, turn03)

	if got, want := currentBgJobs(driver), []string{"bw18fqiul"}; !slices.Equal(got, want) {
		t.Fatalf("after turn03 bg jobs = %v, want %v", got, want)
	}
	events := bgJobEvents(rec.snapshot())
	// 第 4 行的集合推送加入任务；第 5 行的 task_started 是同一个任务，不产生
	// 变化。
	if len(events) != 1 {
		t.Fatalf("after turn03 BgJobsChanged called %d times, want 1: %+v", len(events), events)
	}
	if want := []string{"bw18fqiul"}; !slices.Equal(events[0].JobIDs, want) {
		t.Fatalf("turn03 BgJobsChanged job ids = %v, want %v", events[0].JobIDs, want)
	}
	if events[0].TurnID == "" {
		t.Fatalf("turn03 BgJobsChanged has no turn id")
	}

	// 前 3 行是 task_updated、终态 task_notification 与空集推送。
	feedAll(t, driver, turn04[:3])
	if got := currentBgJobs(driver); len(got) != 0 {
		t.Fatalf("after turn04 task notification bg jobs = %v, want empty", got)
	}
	events = bgJobEvents(rec.snapshot())
	if len(events) != 2 {
		t.Fatalf("BgJobsChanged called %d times after the empty push, want 2: %+v", len(events), events)
	}
	if len(events[1].JobIDs) != 0 {
		t.Fatalf("turn04 removal reported job ids %v, want empty", events[1].JobIDs)
	}
	if events[1].TurnID != "" {
		t.Fatalf("boundary removal reported turn id %q, want empty", events[1].TurnID)
	}

	// 其余行（自发回合）不改变集合，也不产生额外调用。
	feedAll(t, driver, turn04[3:])
	if got := currentBgJobs(driver); len(got) != 0 {
		t.Fatalf("after turn04 bg jobs = %v, want empty", got)
	}
	if got := len(bgJobEvents(rec.snapshot())); got != 2 {
		t.Fatalf("BgJobsChanged called %d times after turn04, want 2", got)
	}
}

// TestBgJobsEmptyPushClearsNonEmptySet 用手写样本验证集合非空时收到空集的
// background_tasks_changed：集合被清空，且恰好报告一次。
func TestBgJobsEmptyPushClearsNonEmptySet(t *testing.T) {
	lines := readRecordedRawLines(t, filepath.Join("testdata", "handwritten", "bg_jobs_empty_push.jsonl"))

	driver, rec := newBareDriver(t)
	simulateDeliver(driver, "delivery-empty-push")
	feedAll(t, driver, lines[:2])
	if got, want := currentBgJobs(driver), []string{"bg-1"}; !slices.Equal(got, want) {
		t.Fatalf("before the empty push bg jobs = %v, want %v", got, want)
	}
	before := len(bgJobEvents(rec.snapshot()))

	feed(t, driver, lines[2])

	if got := currentBgJobs(driver); len(got) != 0 {
		t.Fatalf("after the empty push bg jobs = %v, want empty", got)
	}
	events := bgJobEvents(rec.snapshot())
	if got := len(events) - before; got != 1 {
		t.Fatalf("the empty push produced %d BgJobsChanged calls, want 1: %+v", got, events)
	}
	if last := events[len(events)-1]; len(last.JobIDs) != 0 {
		t.Fatalf("the empty push reported job ids %v, want empty", last.JobIDs)
	}

	feedAll(t, driver, lines[3:])
}

// TestBgJobsHandwrittenAmbient 用手写样本验证 ambient 任务不计入、非后台任务
// 不计入，以及集合推送的整体替换语义。
func TestBgJobsHandwrittenAmbient(t *testing.T) {
	lines := readRecordedRawLines(t, filepath.Join("testdata", "handwritten", "bg_jobs_ambient.jsonl"))

	driver, rec := newBareDriver(t)
	simulateDeliver(driver, "delivery-1")
	feedAll(t, driver, lines)

	if got, want := currentBgJobs(driver), []string{"bg-2", "bg-3"}; !slices.Equal(got, want) {
		t.Fatalf("bg jobs = %v, want %v", got, want)
	}
	events := bgJobEvents(rec.snapshot())
	want := [][]string{
		{"bg-1"},         // 第一次推送：amb-1 是 ambient，不计入
		{"bg-1", "bg-2"}, // task_started：amb-2 是 ambient，fg-1 不是后台任务
		{"bg-2", "bg-3"}, // 第二次推送整体替换
	}
	if len(events) != len(want) {
		t.Fatalf("BgJobsChanged called %d times, want %d: %+v", len(events), len(want), events)
	}
	for i := range want {
		if !slices.Equal(events[i].JobIDs, want[i]) {
			t.Fatalf("BgJobsChanged %d job ids = %v, want %v", i, events[i].JobIDs, want[i])
		}
	}
}

// TestBgJobsTaskNotificationStatuses 验证只有终态通知移除任务：未知或非终态
// 的 status 不移除。
func TestBgJobsTaskNotificationStatuses(t *testing.T) {
	driver, rec := newBareDriver(t)
	feed(t, driver, initLine("ses-bg"))
	feed(t, driver, taskStartedLine("job-a", true, false), taskStartedLine("job-b", true, false))
	if got, want := currentBgJobs(driver), []string{"job-a", "job-b"}; !slices.Equal(got, want) {
		t.Fatalf("bg jobs = %v, want %v", got, want)
	}

	before := len(bgJobEvents(rec.snapshot()))
	feed(t, driver, taskNotificationLine("job-a", "running"))
	if got, want := currentBgJobs(driver), []string{"job-a", "job-b"}; !slices.Equal(got, want) {
		t.Fatalf("non-terminal notification changed bg jobs to %v, want %v", got, want)
	}
	if got := len(bgJobEvents(rec.snapshot())); got != before {
		t.Fatalf("non-terminal notification produced a BgJobsChanged call")
	}

	feed(t, driver, taskNotificationLine("job-a", "failed"))
	if got, want := currentBgJobs(driver), []string{"job-b"}; !slices.Equal(got, want) {
		t.Fatalf("after failed notification bg jobs = %v, want %v", got, want)
	}
	feed(t, driver, taskNotificationLine("job-b", "stopped"))
	if got := currentBgJobs(driver); len(got) != 0 {
		t.Fatalf("after stopped notification bg jobs = %v, want empty", got)
	}
}

// TestBgJobsClearedOnProcessExit 验证 Claude Code 退出后集合为空，并在退出
// 事件之前报告一次空集。
func TestBgJobsClearedOnProcessExit(t *testing.T) {
	t.Run("during a turn", func(t *testing.T) {
		driver, rec := newBareDriver(t)
		feed(t, driver, initLine("ses-exit"))
		simulateDeliver(driver, "delivery-1")
		feed(t, driver, taskStartedLine("bg-exit", true, false))

		var turnID string
		driver.Snapshot(func(s agent.State) { turnID = s.TurnID })
		before := rec.count()

		driver.mu.Lock()
		driver.handleProcessExitLocked("exit status 1")
		driver.mu.Unlock()

		events := since(rec.snapshot(), before)
		assertKinds(t, events, eventBgJobsChanged, eventTurnEnded)
		if len(events[0].JobIDs) != 0 {
			t.Fatalf("exit reported job ids %v, want empty", events[0].JobIDs)
		}
		if events[0].TurnID != turnID {
			t.Fatalf("exit BgJobsChanged turn id = %q, want %q", events[0].TurnID, turnID)
		}
		if !events[1].AgentExited || events[1].TurnID != turnID {
			t.Fatalf("turn_ended = %+v, want agent_exited on turn %s", events[1], turnID)
		}
		if got := currentBgJobs(driver); len(got) != 0 {
			t.Fatalf("bg jobs after exit = %v, want empty", got)
		}
	})

	t.Run("at the boundary", func(t *testing.T) {
		driver, rec := newBareDriver(t)
		feed(t, driver, initLine("ses-exit"))
		feed(t, driver, taskStartedLine("bg-exit", true, false))
		before := rec.count()

		driver.mu.Lock()
		driver.handleProcessExitLocked("exit status 0")
		driver.mu.Unlock()

		events := since(rec.snapshot(), before)
		assertKinds(t, events, eventBgJobsChanged, eventTurnStarted, eventTurnEnded)
		if len(events[0].JobIDs) != 0 || events[0].TurnID != "" {
			t.Fatalf("boundary exit BgJobsChanged = %+v, want an empty set without a turn id", events[0])
		}
		if got := currentBgJobs(driver); len(got) != 0 {
			t.Fatalf("bg jobs after exit = %v, want empty", got)
		}
	})
}

// TestBgJobsClearedOnShutdown 验证正常关停后集合为空，且关停期间不再调用
// Sink。
func TestBgJobsClearedOnShutdown(t *testing.T) {
	cfg := testConfig(t)
	writeFakeMode(t, cfg.Home, "silent")
	driver, rec := newTestDriver(t, cfg)

	if err := driver.Deliver(
		agent.Batch{DeliveryID: "d-1", ThroughSeq: 1, Texts: []string{"hello"}},
		agent.Credential{Kind: agent.CredentialAPIKey, Value: "sk-test"},
	); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	feed(t, driver, initLine("ses-shutdown-bg"))
	feed(t, driver, taskStartedLine("bg-shutdown", true, false))
	if got, want := currentBgJobs(driver), []string{"bg-shutdown"}; !slices.Equal(got, want) {
		t.Fatalf("bg jobs = %v, want %v", got, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := currentBgJobs(driver); len(got) != 0 {
		t.Fatalf("bg jobs after shutdown = %v, want empty", got)
	}
	if got := len(bgJobEvents(rec.snapshot())); got != 1 {
		t.Fatalf("BgJobsChanged called %d times, want 1 (the join only)", got)
	}
}

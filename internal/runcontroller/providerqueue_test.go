package runcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// providerQueueWait 是队列测试等待事件的上限。测试只在证明“某件事没有
// 发生”时使用短超时，其余等待都以它为兜底。
const (
	providerQueueWait   = 5 * time.Second
	providerQueueAbsent = 150 * time.Millisecond
)

func waitProviderQueue(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(providerQueueWait):
		t.Fatalf("等待%s超时", what)
	}
}

func assertProviderQueueNotClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s不应发生", what)
	case <-time.After(providerQueueAbsent):
	}
}

// TestProviderQueueSerializesPerRun 验证同一运行实例的任务按 reserve 顺序
// 执行且从不重叠。
func TestProviderQueueSerializesPerRun(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	defer q.shutdown()

	const n = 3
	var running, maxRunning atomic.Int32
	started := make([]chan struct{}, n)
	release := make([]chan struct{}, n)
	finished := make([]chan struct{}, n)
	for i := range started {
		started[i] = make(chan struct{})
		release[i] = make(chan struct{})
		finished[i] = make(chan struct{})
	}

	var orderMu sync.Mutex
	var order []int
	for i := 0; i < n; i++ {
		i := i
		slot := q.reserve("run-1")
		slot.start(func(ctx context.Context) {
			cur := running.Add(1)
			for {
				prev := maxRunning.Load()
				if cur <= prev || maxRunning.CompareAndSwap(prev, cur) {
					break
				}
			}
			orderMu.Lock()
			order = append(order, i)
			orderMu.Unlock()
			close(started[i])
			<-release[i]
			running.Add(-1)
			close(finished[i])
		})
	}

	waitProviderQueue(t, started[0], "第一个任务开始")
	assertProviderQueueNotClosed(t, started[1], "第一个任务未完成时第二个任务开始")
	close(release[0])

	waitProviderQueue(t, started[1], "第二个任务开始")
	assertProviderQueueNotClosed(t, started[2], "第二个任务未完成时第三个任务开始")
	close(release[1])

	waitProviderQueue(t, started[2], "第三个任务开始")
	close(release[2])

	for i := 0; i < n; i++ {
		waitProviderQueue(t, finished[i], "任务完成")
	}
	if got := maxRunning.Load(); got != 1 {
		t.Fatalf("同一运行实例同时执行的任务数 = %d，期望 1", got)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	for i, got := range order {
		if got != i {
			t.Fatalf("执行顺序 = %v，期望 0..%d", order, n-1)
		}
	}
}

// TestProviderQueueWaitsForEarlierStart 验证前一个位置尚未 start 或
// abandon 时，后一个位置即使已 start 也不执行。
func TestProviderQueueWaitsForEarlierStart(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	defer q.shutdown()

	first := q.reserve("run-1")
	second := q.reserve("run-1")
	ran := make(chan struct{})
	second.start(func(ctx context.Context) {
		close(ran)
	})

	assertProviderQueueNotClosed(t, ran, "前一个位置未处理时后一个任务执行")

	first.abandon()
	waitProviderQueue(t, ran, "abandon 后后一个任务开始")
}

// TestProviderQueueAbandonSkipsMiddle 验证 abandon 的中间位置被跳过，前后
// 任务仍按预留顺序执行。
func TestProviderQueueAbandonSkipsMiddle(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	defer q.shutdown()

	first := q.reserve("run-1")
	middle := q.reserve("run-1")
	last := q.reserve("run-1")

	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	done := make(chan struct{})

	first.start(func(ctx context.Context) { record("first") })
	middle.abandon()
	last.start(func(ctx context.Context) {
		record("last")
		close(done)
	})

	waitProviderQueue(t, done, "最后一个任务完成")
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "last" {
		t.Fatalf("执行顺序 = %v，期望 [first last]", order)
	}
}

// TestProviderQueueRunsConcurrentlyAcrossRuns 验证不同运行实例的任务并发
// 执行，互不等待。
func TestProviderQueueRunsConcurrentlyAcrossRuns(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	defer q.shutdown()

	startedA := make(chan struct{})
	startedB := make(chan struct{})
	release := make(chan struct{})
	doneA := make(chan struct{})
	doneB := make(chan struct{})

	a := q.reserve("run-a")
	b := q.reserve("run-b")
	a.start(func(ctx context.Context) {
		close(startedA)
		<-release
		close(doneA)
	})
	b.start(func(ctx context.Context) {
		close(startedB)
		<-release
		close(doneB)
	})

	// 两个任务必须同时处于已开始状态：先等到 A，再等到 B，期间不释放。
	waitProviderQueue(t, startedA, "运行实例 A 的任务开始")
	waitProviderQueue(t, startedB, "运行实例 B 的任务开始")

	close(release)
	waitProviderQueue(t, doneA, "运行实例 A 的任务完成")
	waitProviderQueue(t, doneB, "运行实例 B 的任务完成")
}

// TestProviderQueueReserveWithinJob 验证任务内再为同一运行实例 reserve 并
// start 不死锁：新位置排在当前任务之后，等当前任务返回再执行。
func TestProviderQueueReserveWithinJob(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	defer q.shutdown()

	outer := q.reserve("run-1")
	inner := make(chan struct{})
	outer.start(func(ctx context.Context) {
		slot := q.reserve("run-1")
		slot.start(func(ctx context.Context) {
			close(inner)
		})
	})

	waitProviderQueue(t, inner, "任务内预留的任务执行")
}

// TestProviderQueueShutdownCancelsAndWaits 验证 shutdown 取消任务的 ctx，
// 等任务返回后才返回，并丢弃此后的预留位置。
func TestProviderQueueShutdownCancelsAndWaits(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	slot := q.reserve("run-1")
	started := make(chan struct{})
	jobReturned := make(chan struct{})
	var jobCtxErr error
	slot.start(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		jobCtxErr = ctx.Err()
		close(jobReturned)
	})
	waitProviderQueue(t, started, "任务开始")

	shutdownReturned := make(chan struct{})
	go func() {
		q.shutdown()
		close(shutdownReturned)
	}()

	waitProviderQueue(t, jobReturned, "任务因 ctx 取消返回")
	waitProviderQueue(t, shutdownReturned, "shutdown 返回")

	if !errors.Is(jobCtxErr, context.Canceled) {
		t.Fatalf("任务的 ctx 错误 = %v，期望 context.Canceled", jobCtxErr)
	}

	// shutdown 之后预留的位置直接丢弃，start 不执行任务。
	ran := make(chan struct{})
	late := q.reserve("run-late")
	late.start(func(ctx context.Context) {
		close(ran)
	})
	assertProviderQueueNotClosed(t, ran, "shutdown 之后预留的任务执行")
}

// TestProviderQueueStartAfterShutdown 验证先预留、后 shutdown、再 start 的
// 位置被丢弃而不执行任务，shutdown 也不为它等待。
func TestProviderQueueStartAfterShutdown(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	slot := q.reserve("run-1")
	q.shutdown()

	ran := make(chan struct{})
	slot.start(func(ctx context.Context) {
		close(ran)
	})
	assertProviderQueueNotClosed(t, ran, "shutdown 之后 start 的任务执行")
}

// TestProviderQueueAbandonAfterShutdownDoesNotCount 验证 shutdown 之后
// abandon 不再对 WaitGroup 计数，而是立即放行排在它后面的位置。
func TestProviderQueueAbandonAfterShutdownDoesNotCount(t *testing.T) {
	// 不并行：GOMAXPROCS=1 让“立即放行”的断言不受新 goroutine 抢先调度
	// 的影响。未修复的实现会启动一个 goroutine 才关闭 done，它在本次
	// goroutine 检查之前没有机会运行。
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)

	q := newProviderQueue()
	first := q.reserve("run-1")
	second := q.reserve("run-1")
	q.shutdown()

	second.abandon()

	q.mu.Lock()
	select {
	case <-second.done:
	default:
		q.mu.Unlock()
		t.Fatal("shutdown 之后 abandon 仍对 WaitGroup 计数，没有立即放行后继位置")
	}
	q.mu.Unlock()

	first.abandon()
}

// TestProviderQueueIgnoresCallerContext 验证调用方所持 ctx 被取消不影响
// 任务，任务拿到的 ctx 来自队列且未被取消。
func TestProviderQueueIgnoresCallerContext(t *testing.T) {
	t.Parallel()

	q := newProviderQueue()
	defer q.shutdown()

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(callerCtx.Err(), context.Canceled) {
		t.Fatalf("调用方 ctx 未被取消：%v", callerCtx.Err())
	}

	slot := q.reserve("run-1")
	got := make(chan error, 1)
	slot.start(func(ctx context.Context) {
		got <- ctx.Err()
	})

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("任务 ctx 错误 = %v，期望 nil", err)
		}
	case <-time.After(providerQueueWait):
		t.Fatal("等待任务执行超时")
	}
}

// TestProviderQueueProviderCallLog 验证每次提供者调用返回时写一行
// “provider call”日志，成功与失败各一行且属性齐全。
func TestProviderQueueProviderCallLog(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	q := newProviderQueue()
	defer q.shutdown()

	success := q.reserve("run-ok")
	success.start(providerCallJob(opProvision, "run-ok", func(ctx context.Context) error {
		return nil
	}))
	waitProviderQueue(t, success.done, "成功的提供者调用返回")

	boom := errors.New("snapshot boom")
	failure := q.reserve("run-bad")
	failure.start(providerCallJob(opSnapshot, "run-bad", func(ctx context.Context) error {
		return boom
	}))
	waitProviderQueue(t, failure.done, "失败的提供者调用返回")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("日志行数 = %d，期望 2，输出 %q", len(lines), buf.String())
	}

	first := decodeProviderCallLog(t, lines[0])
	if first.msg != "provider call" {
		t.Fatalf("日志消息 = %q，期望 provider call", first.msg)
	}
	if first.op != "provision" || first.runID != "run-ok" || first.errorText != "" {
		t.Fatalf("成功的调用日志属性不对：%+v", first)
	}
	if !first.integerDuration {
		t.Fatalf("duration_ms 不是整数：%v", first.duration)
	}

	second := decodeProviderCallLog(t, lines[1])
	if second.op != "snapshot" || second.runID != "run-bad" || second.errorText != boom.Error() {
		t.Fatalf("失败的调用日志属性不对：%+v", second)
	}
	if !second.integerDuration {
		t.Fatalf("duration_ms 不是整数：%v", second.duration)
	}
}

type providerCallLog struct {
	msg             string
	op              string
	runID           string
	duration        float64
	integerDuration bool
	errorText       string
}

func decodeProviderCallLog(t *testing.T, line string) providerCallLog {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("解析日志行失败：%v，行 %q", err, line)
	}
	getString := func(key string) string {
		v, ok := raw[key].(string)
		if !ok {
			t.Fatalf("日志缺少字符串属性 %q：%q", key, line)
		}
		return v
	}
	duration, ok := raw["duration_ms"].(float64)
	if !ok {
		t.Fatalf("日志缺少数值属性 duration_ms：%q", line)
	}
	return providerCallLog{
		msg:             getString("msg"),
		op:              getString("op"),
		runID:           getString("run_id"),
		duration:        duration,
		integerDuration: duration >= 0 && duration == math.Trunc(duration),
		errorText:       getString("error"),
	}
}

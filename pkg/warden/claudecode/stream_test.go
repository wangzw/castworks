package claudecode

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

// rateLimitEventLine 是 testdata/recorded/local_turn01_deliver.jsonl 第 4 行的
// 真实 rate_limit_event 消息。
const rateLimitEventLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1795132800,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"group_zero_credit_limit","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.03,"resetsAt":1795132800,"status":"allowed"},"seven_day":{"utilization":0.56,"resetsAt":1795136400,"status":"allowed"}}},"uuid":"52b0f98b-fb79-471e-b95b-4fe35e480656","session_id":"e89e59ed-657e-4c5d-8a03-8627fe5bb5ad"}`

// logRecorder 记录经过 Logger 的全部记录，供测试核对级别。
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *logRecorder) WithGroup(string) slog.Handler { return r }

// warnRecords 返回 Warn 及以上级别的记录。
func (r *logRecorder) warnRecords() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, record := range r.records {
		if record.Level >= slog.LevelWarn {
			out = append(out, record)
		}
	}
	return out
}

// TestRateLimitEventIsInformational 断言 rate_limit_event 是信息类消息：
// 不产生 Sink 事件，不开始自发回合，不改变阶段，也不记 Warn 日志。
func TestRateLimitEventIsInformational(t *testing.T) {
	t.Run("at turn boundary", func(t *testing.T) {
		logs := &logRecorder{}
		rec := &recorder{}
		driver := New(Config{Home: t.TempDir(), Logger: slog.New(logs)}, rec, nil)
		driver.phase = agent.PhaseBoundary

		feed(t, driver, rateLimitEventLine)

		if got := rec.snapshot(); len(got) != 0 {
			t.Fatalf("rate_limit_event produced Sink events: %+v", got)
		}
		if driver.phase != agent.PhaseBoundary {
			t.Fatalf("phase = %v, want PhaseBoundary", driver.phase)
		}
		if warnings := logs.warnRecords(); len(warnings) != 0 {
			t.Fatalf("rate_limit_event logged at Warn or above: %+v", warnings)
		}
	})

	t.Run("during a turn", func(t *testing.T) {
		logs := &logRecorder{}
		rec := &recorder{}
		driver := New(Config{Home: t.TempDir(), Logger: slog.New(logs)}, rec, nil)
		driver.phase = agent.PhaseBoundary
		// 先取得 session_id，避免本条消息的 session_id 补发 TurnStarted，
		// 让断言只观察 rate_limit_event 本身的效果。
		driver.sessionID = "sess-existing"
		simulateDeliver(driver, "delivery-1")
		before := rec.count()
		phase := driver.phase

		feed(t, driver, rateLimitEventLine)

		if got := rec.count(); got != before {
			t.Fatalf("rate_limit_event produced %d Sink events, want none", got-before)
		}
		if driver.phase != phase {
			t.Fatalf("phase changed from %v to %v", phase, driver.phase)
		}
		if warnings := logs.warnRecords(); len(warnings) != 0 {
			t.Fatalf("rate_limit_event logged at Warn or above: %+v", warnings)
		}
	})
}

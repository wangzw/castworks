package runcontroller_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// syncBuffer 是并发安全的日志缓冲：流的关闭发生在服务端 goroutine，测试线程
// 会同时轮询读取。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDefaultLog 把进程级 slog 默认输出换到 sb 并返回它，测试结束时恢复。
// 因为 slog 默认输出是进程级的，用它的用例不能并行运行。
func captureDefaultLog(t *testing.T) *syncBuffer {
	t.Helper()
	sb := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sb, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return sb
}

// controlStreamClosedLines 从 s 中取出 runID 的控制流关闭日志行。其他日志行
// （例如提供者调用）不参与断言。
func controlStreamClosedLines(t *testing.T, s, runID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] != "runcontroller: control stream closed" {
			continue
		}
		if rec["run_id"] != runID {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// TestStreamCloseLogsDisconnect 验证当前流关闭时恰好写一行 Info 日志，属性
// run_id 与 disconnected_at 齐全，且时刻与记入断线记录的是同一个假时钟值。
func TestStreamCloseLogsDisconnect(t *testing.T) {
	sb := captureDefaultLog(t)
	h := newHarness(t, crashTestConfig())
	runID, _, cancel := stepToBoundaryCancelable(t, h)

	// 把假时钟推进到带纳秒的时刻，让 RFC 3339 纳秒格式可被观察到。
	h.clock.Advance(123456789 * time.Nanosecond)
	want := h.clock.Now()
	if want.Nanosecond() == 0 {
		t.Fatalf("假时钟时刻 %s 没有纳秒", want)
	}

	cancel()
	eventually(t, "控制流关闭日志", func() bool {
		return len(controlStreamClosedLines(t, sb.String(), runID)) > 0
	})

	since, ok := runcontroller.DisconnectedSinceForTest(h.ctrl, runID)
	if !ok {
		t.Fatal("断线时刻没有记入断线记录")
	}
	if !since.Equal(want) {
		t.Fatalf("断线记录的时刻 = %s，想要 %s", since.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}

	lines := controlStreamClosedLines(t, sb.String(), runID)
	if len(lines) != 1 {
		t.Fatalf("控制流关闭日志行数 = %d，想要 1，输出 %q", len(lines), sb.String())
	}
	rec := lines[0]
	if rec["level"] != "INFO" {
		t.Fatalf("日志级别 = %v，想要 INFO", rec["level"])
	}
	if got := rec["disconnected_at"]; got != want.Format(time.RFC3339Nano) {
		t.Fatalf("disconnected_at = %v，想要 %s", got, want.Format(time.RFC3339Nano))
	}
}

// TestStreamCloseStaleStreamLogsNothing 验证被新流取代的旧流关闭时不写这一行。
func TestStreamCloseStaleStreamLogsNothing(t *testing.T) {
	sb := captureDefaultLog(t)
	h := newHarness(t, crashTestConfig())
	const runID = "run-stale-log"

	first := runcontroller.NewStreamForTest(h.ctrl, runID)
	runcontroller.NewStreamForTest(h.ctrl, runID)
	runcontroller.CloseStaleStreamForTest(h.ctrl, first)

	if lines := controlStreamClosedLines(t, sb.String(), runID); len(lines) != 0 {
		t.Fatalf("旧流关闭写了 %d 行日志，想要 0 行，输出 %q", len(lines), sb.String())
	}
}

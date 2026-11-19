package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type expectedTurn struct {
	Source      string `json:"source"`
	Output      string `json:"output"`
	IsError     bool   `json:"is_error"`
	AgentExited bool   `json:"agent_exited"`
	// EndLine 是结束该回合的行号（从 1 起）。
	EndLine int `json:"end_line"`
}

// TestReplaySamples 回放 testdata/handwritten 与 testdata/recorded 下的全部
// 样本。每个样本是一对文件：<名>.jsonl 是 stdout 原样行，<名>.expect.json
// 按顺序列出回合。recorded/ 为空时照常通过。
func TestReplaySamples(t *testing.T) {
	patterns := []string{
		filepath.Join("testdata", "handwritten", "*.jsonl"),
		filepath.Join("testdata", "recorded", "*.jsonl"),
	}
	found := false
	for _, pattern := range patterns {
		samples, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, sample := range samples {
			found = true
			sample := sample
			t.Run(filepath.Base(sample), func(t *testing.T) {
				replaySample(t, sample)
			})
		}
	}
	if !found {
		t.Fatalf("no samples under testdata/handwritten or testdata/recorded")
	}
}

func replaySample(t *testing.T, sample string) {
	t.Helper()
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	lines := splitSampleLines(string(data))

	expectData, err := os.ReadFile(strings.TrimSuffix(sample, ".jsonl") + ".expect.json")
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var expects []expectedTurn
	if err := json.Unmarshal(expectData, &expects); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	if len(expects) == 0 {
		t.Fatalf("expectations are empty")
	}

	driver, rec := newBareDriver(t)
	lineIdx := 0
	eventIdx := 0
	for i, want := range expects {
		switch want.Source {
		case "deliver":
			simulateDeliver(driver, fmt.Sprintf("delivery-%d", i))
		case "spontaneous":
			for !hasKindSince(rec.snapshot(), eventIdx, eventTurnStarted) {
				if lineIdx >= len(lines) {
					t.Fatalf("turn %d: sample ended before a spontaneous turn started", i)
				}
				feed(t, driver, lines[lineIdx])
				lineIdx++
			}
		default:
			t.Fatalf("turn %d: unknown source %q", i, want.Source)
		}

		endLine := 0
		for !hasKindSince(rec.snapshot(), eventIdx, eventTurnEnded) {
			if lineIdx >= len(lines) {
				t.Fatalf("turn %d: sample ended before turn_ended", i)
			}
			feed(t, driver, lines[lineIdx])
			lineIdx++
			if hasKindSince(rec.snapshot(), eventIdx, eventTurnEnded) {
				endLine = lineIdx
			}
		}
		if want.EndLine <= 0 {
			t.Fatalf("turn %d: expectation has no end_line", i)
		}
		if endLine != want.EndLine {
			t.Fatalf("turn %d: turn ended at line %d, want line %d", i, endLine, want.EndLine)
		}

		events := since(rec.snapshot(), eventIdx)
		started, ok := firstKind(events, eventTurnStarted)
		if !ok {
			t.Fatalf("turn %d: no turn_started", i)
		}
		ended, ok := firstKind(events, eventTurnEnded)
		if !ok {
			t.Fatalf("turn %d: no turn_ended", i)
		}
		if started.TurnID != ended.TurnID {
			t.Fatalf("turn %d: turn ids differ: started %+v, ended %+v", i, started, ended)
		}
		if want.Source == "deliver" && started.DeliveryID == "" {
			t.Fatalf("turn %d: delivered turn has an empty delivery id", i)
		}
		if want.Source == "spontaneous" && started.DeliveryID != "" {
			t.Fatalf("turn %d: spontaneous turn has delivery id %q", i, started.DeliveryID)
		}
		if ended.Output != want.Output || ended.IsError != want.IsError || ended.AgentExited != want.AgentExited {
			t.Fatalf("turn %d: got output=%q is_error=%v agent_exited=%v, want output=%q is_error=%v agent_exited=%v",
				i, ended.Output, ended.IsError, ended.AgentExited,
				want.Output, want.IsError, want.AgentExited)
		}
		eventIdx = rec.count()
	}

	// 样本末尾不应还有回合事件。
	before := rec.count()
	for ; lineIdx < len(lines); lineIdx++ {
		feed(t, driver, lines[lineIdx])
	}
	for _, event := range since(rec.snapshot(), before) {
		if event.Kind == eventTurnStarted || event.Kind == eventTurnEnded {
			t.Fatalf("unexpected turn event after the expected turns: %+v", event)
		}
	}
}

func splitSampleLines(data string) []string {
	data = strings.TrimSuffix(data, "\n")
	if data == "" {
		return nil
	}
	return strings.Split(data, "\n")
}

func hasKindSince(events []sinkEvent, from int, kind string) bool {
	_, ok := firstKind(since(events, from), kind)
	return ok
}

func since(events []sinkEvent, from int) []sinkEvent {
	if from >= len(events) {
		return nil
	}
	return events[from:]
}

func firstKind(events []sinkEvent, kind string) (sinkEvent, bool) {
	for _, event := range events {
		if event.Kind == kind {
			return event, true
		}
	}
	return sinkEvent{}, false
}

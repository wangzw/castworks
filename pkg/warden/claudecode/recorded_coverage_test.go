package claudecode

// 真实录制样本说明
//
// 来源：
//   - testdata/recorded/local_turn*.jsonl 取自本地模式验收
//     （2026-11-19T19:40:56Z，run_id 3ocoujavzjia，Claude Code 2.1.328）的
//     home/.castworks/stream.jsonl。
//   - testdata/recorded/record_attempt_deliver.jsonl 取自
//     hack/acceptance/record 的补录尝试（2026-11-19T19:44:13Z）。该次尝试
//     没有录到 queued_turn_count 大于 0 的 result，Claude Code 被结束后以
//     143 退出；样本只保留第一个完整回合。
//
// 切分（本地模式验收计划的“样本脱敏”一节）：
//   - 每个样本从上一回合 idle 之后的第一行切到本回合的 idle，行序原样保留，
//     不删行。第一个回合没有上一回合，从流的第 1 行开始。
//   - 每个样本因此包含回合开头的 session_state_changed running；两个自发回合
//     的样本还包含开始信号之前的 task_updated、task_notification 与
//     background_tasks_changed。
//   - 本地流共 54 行，恰好切成 6 个样本。补录流只取第一个回合的前 19 行，
//     回合结束后的三行任务通知不属于任何回合，没有收入。
//
// 脱敏：
//   - 主目录路径替换为 /home/agent，Claude Code 项目目录名里的路径编码
//     一并替换为 -home-agent-work。
//   - thinking 块里的 signature 是不透明的服务端数据，回放不使用，统一替换
//     为固定占位串 REDACTED，避免样本携带无法核对的长串。
//   - system 初始化消息里没有邮箱、组织或账号 UUID 字段，因此没有账号标识
//     需要替换；键全部保留。
//   - 其余内容原样保留，包括 session_id、request_id、模型输出，以及主目录
//     之外的消息套接字与任务输出路径。
//
// expect.json 的 output 依据控制面记录的 turns 写出，即 turn_ended 携带的
// output，等于 result 消息的 result 文本；这些文本都远小于 1 MiB，没有
// 触发截断，两者逐字一致。
//
// 已知缺失：真实录制没有出现 idle 先于 result、queued_turn_count 大于 0
// 两种情形。覆盖性测试把它们记为已知缺失并写明录制事实，不手写伪造样本。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const recordedSampleDir = "testdata/recorded"

// recordedKnownMissing 记录真实录制中没有出现的情形及原因。某项出现后应
// 删除对应条目，覆盖性测试会在条目过时时失败。
var recordedKnownMissing = map[string]string{
	"idle_before_result":         "2.1.328 在本地验收的 6 个回合与补录尝试的 1 个回合里都先发 result 再发 session_state_changed: idle，没有录到 idle 先到的回合。",
	"queued_turn_count_positive": "控制面只在回合边界投递；hack/acceptance/record 在回合进行中写入第二条用户消息后，result 的 queued_turn_count 仍为 0，Claude Code 随后以 143 退出，没有录到大于 0 的 result。",
}

// recordedLine 是覆盖性测试关心的 stream-json 字段。
type recordedLine struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	State           string `json:"state"`
	QueuedTurnCount *int   `json:"queued_turn_count"`
}

// TestRecordedCoverage 扫描真实录制样本，断言计划要求的几种情形都有覆盖：
// result 先于 idle、idle 先于 result、queued_turn_count 大于 0 且边界推迟
// 到清零之后、自发回合由 session_state_changed running 开始，以及回合边界
// 上的任务通知不会开始回合。样本里没有出现的情形必须写入 recordedKnownMissing
// 并给出原因，不能手写伪造。
func TestRecordedCoverage(t *testing.T) {
	samples, err := filepath.Glob(filepath.Join(recordedSampleDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob recorded samples: %v", err)
	}
	if len(samples) == 0 {
		t.Fatalf("no recorded samples under %s", recordedSampleDir)
	}

	var (
		resultBeforeIdle         bool
		idleBeforeResult         bool
		queuedDrained            bool
		spontaneousStart         bool
		boundaryNoiseBeforeStart bool
	)

	for _, sample := range samples {
		sample := sample
		t.Run(filepath.Base(sample), func(t *testing.T) {
			raw := readRecordedRawLines(t, sample)
			lines := parseRecordedLines(t, sample, raw)
			expects := readRecordedExpects(t, sample)

			offset := 0
			for i, want := range expects {
				if want.EndLine <= offset || want.EndLine > len(lines) {
					t.Fatalf("turn %d: end_line %d is outside the sample after line %d of %d",
						i, want.EndLine, offset, len(lines))
				}
				segment := lines[offset:want.EndLine]
				rawSegment := raw[offset:want.EndLine]
				base := offset
				offset = want.EndLine

				firstResult, firstIdle := -1, -1
				firstAssistant, runningAt := -1, -1
				lastQueuedPositive, lastQueued := -1, 0
				noiseBeforeStart := 0
				for j, line := range segment {
					switch {
					case line.Type == "result":
						if firstResult < 0 {
							firstResult = j
						}
						queued := 0
						if line.QueuedTurnCount != nil {
							queued = *line.QueuedTurnCount
						}
						if queued > 0 {
							lastQueuedPositive = j
						}
						lastQueued = queued
					case line.Type == "system" && line.Subtype == "session_state_changed" && line.State == "idle":
						if firstIdle < 0 {
							firstIdle = j
						}
					case line.Type == "system" && line.Subtype == "session_state_changed" && line.State == "running":
						if runningAt < 0 {
							runningAt = j
						}
					case line.Type == "assistant":
						if firstAssistant < 0 {
							firstAssistant = j
						}
					case isBoundaryNoise(line):
						if runningAt < 0 {
							noiseBeforeStart++
						}
					}
				}

				if firstResult < 0 || firstIdle < 0 {
					t.Fatalf("turn %d: sample has no result or no idle boundary", i)
				}
				if firstResult < firstIdle {
					resultBeforeIdle = true
				} else {
					idleBeforeResult = true
				}
				// 边界推迟到清零之后：先有 queued_turn_count 大于 0 的
				// result，回合结束前最后一个 result 已清零。
				if lastQueuedPositive >= 0 && lastQueued == 0 {
					queuedDrained = true
				}

				// 回合按真实开始信号判定：session_state_changed running
				// 必须存在，且早于 result 与任何 assistant 消息。
				if runningAt < 0 {
					t.Fatalf("turn %d: sample has no session_state_changed running start signal", i)
				}
				if runningAt >= firstResult {
					t.Fatalf("turn %d: start signal at line %d is not before the result at line %d",
						i, base+runningAt+1, base+firstResult+1)
				}
				if firstAssistant >= 0 && firstAssistant < runningAt {
					t.Fatalf("turn %d: assistant at line %d precedes the running start at line %d",
						i, base+firstAssistant+1, base+runningAt+1)
				}

				if want.Source == "spontaneous" {
					spontaneousStart = true
					// 开始信号之前的行是回合边界上的任务通知。用真实
					// 前缀走一遍驱动，确认它们不开始回合，只有 running 开始。
					if runningAt > 0 {
						driver, rec := newBareDriver(t)
						for _, line := range rawSegment[:runningAt] {
							feed(t, driver, line)
						}
						if events := rec.snapshot(); len(events) != 0 {
							t.Fatalf("turn %d: boundary noise before the start signal produced events: %+v", i, events)
						}
						feed(t, driver, rawSegment[runningAt])
						if _, ok := firstKind(rec.snapshot(), eventTurnStarted); !ok {
							t.Fatalf("turn %d: the running start signal did not start a turn", i)
						}
						if noiseBeforeStart > 0 {
							boundaryNoiseBeforeStart = true
						}
					}
				}
			}
			if offset != len(lines) {
				t.Fatalf("sample has %d trailing line(s) after the last expected turn", len(lines)-offset)
			}
		})
	}

	cases := []struct {
		name     string
		observed bool
	}{
		{"result_before_idle", resultBeforeIdle},
		{"idle_before_result", idleBeforeResult},
		{"queued_turn_count_positive", queuedDrained},
		{"spontaneous_turn_start", spontaneousStart},
		{"boundary_noise_before_start", boundaryNoiseBeforeStart},
	}
	for _, c := range cases {
		reason, known := recordedKnownMissing[c.name]
		switch {
		case c.observed && known:
			t.Errorf("%s is covered now; remove its stale recordedKnownMissing entry (%s)", c.name, reason)
		case c.observed:
			t.Logf("covered: %s", c.name)
		case known:
			t.Logf("missing from the real recordings: %s; recorded fact: %s", c.name, reason)
		default:
			t.Errorf("the real recordings do not cover %s and no reason is recorded; record a new sample instead of hand-writing one", c.name)
		}
	}
}

// isBoundaryNoise 判断一行是否是回合边界上的任务通知。这类消息不改变
// 回合位置，不应开始回合。
func isBoundaryNoise(line recordedLine) bool {
	if line.Type != "system" {
		return false
	}
	switch line.Subtype {
	case "task_updated", "task_notification", "background_tasks_changed":
		return true
	}
	return false
}

func readRecordedRawLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample %s: %v", path, err)
	}
	return splitSampleLines(string(data))
}

func parseRecordedLines(t *testing.T, path string, raw []string) []recordedLine {
	t.Helper()
	lines := make([]recordedLine, 0, len(raw))
	for i, text := range raw {
		var line recordedLine
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("parse sample %s line %d: %v", path, i+1, err)
		}
		lines = append(lines, line)
	}
	return lines
}

func readRecordedExpects(t *testing.T, path string) []expectedTurn {
	t.Helper()
	data, err := os.ReadFile(path[:len(path)-len(".jsonl")] + ".expect.json")
	if err != nil {
		t.Fatalf("read expectations for %s: %v", path, err)
	}
	var expects []expectedTurn
	if err := json.Unmarshal(data, &expects); err != nil {
		t.Fatalf("parse expectations for %s: %v", path, err)
	}
	if len(expects) == 0 {
		t.Fatalf("%s has no expected turns", path)
	}
	return expects
}

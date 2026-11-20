package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner 记录脚本执行的命令，并按顺序返回预设结果。
type fakeRunner struct {
	mu      sync.Mutex
	runs    []string
	started []string
	results []commandResult
	done    chan commandResult
}

func (r *fakeRunner) Run(_ context.Context, command string) commandResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, command)
	if len(r.results) == 0 {
		return commandResult{}
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result
}

func (r *fakeRunner) Start(_ context.Context, command, _ string) backgroundHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, command)
	return backgroundHandle{Done: r.done}
}

// fakeCaller 记录平台 MCP 工具调用。
type fakeCaller struct {
	mu    sync.Mutex
	calls []toolCall
}

type toolCall struct {
	name string
	args map[string]any
}

func (c *fakeCaller) CallTool(_ context.Context, name string, args map[string]any) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, toolCall{name: name, args: args})
	return "tool result for " + name, false, nil
}

func (c *fakeCaller) Close() error { return nil }

func (c *fakeCaller) callList() []toolCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]toolCall(nil), c.calls...)
}

// newTestSession 构造一个写进 buffer 的会话。
func newTestSession(t *testing.T, script *Script, runner commandRunner, caller toolCaller, out io.Writer) *session {
	t.Helper()
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newSession(script, newEmitter(out, workDir), workDir, caller, runner, ctx, io.Discard)
}

// splitLines 把 stream-json 输出按行切开，忽略空行。
func splitLines(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var lines [][]byte
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	return lines
}

// decodeLines 把原始行解码成消息。
func decodeLines(t *testing.T, lines [][]byte) []map[string]any {
	t.Helper()
	messages := make([]map[string]any, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal(line, &messages[i]); err != nil {
			t.Fatalf("decode stream line %q: %v", line, err)
		}
	}
	return messages
}

// resultText 返回最后一条 result 消息的 result 字段。
func resultText(t *testing.T, messages []map[string]any) string {
	t.Helper()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["type"] == "result" {
			text, _ := messages[i]["result"].(string)
			return text
		}
	}
	t.Fatal("no result message")
	return ""
}

// TestSubscriptionFlow 检查失败后补 owner 重交并重新订阅，成功后按格式报告。
func TestSubscriptionFlow(t *testing.T) {
	script, err := LoadScript("testdata/subscription.json")
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	runner := &fakeRunner{results: []commandResult{
		{Stdout: `{"id":"job-1","status":"queued"}`},
		{Stdout: `{"id":"job-2","status":"queued"}`},
	}}
	caller := &fakeCaller{}
	var out bytes.Buffer
	sess := newTestSession(t, script, runner, caller, &out)

	initial := `Submit this content to extsim and wait for the result: {"marker":"m-123"}. Use the platform subscription tools.`
	if err := sess.HandleText(initial); err != nil {
		t.Fatalf("initial turn: %v", err)
	}

	jobFile := filepath.Join(sess.workDir, "job.json")
	data, err := os.ReadFile(jobFile)
	if err != nil {
		t.Fatalf("read job.json: %v", err)
	}
	if string(data) != `{"marker":"m-123"}` {
		t.Fatalf("initial content = %q", data)
	}
	if len(runner.runs) != 1 || runner.runs[0] != "extsim job submit --file job.json" {
		t.Fatalf("commands = %v", runner.runs)
	}
	calls := caller.callList()
	if len(calls) != 2 || calls[0].name != "list_events" || calls[1].name != "subscribe_event" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if got := correlationJobID(calls[1].args); got != "job-1" {
		t.Fatalf("first subscription correlates %q, want job-1", got)
	}

	failed := `[castworks] Subscription sub-abc completed.
system: extsim
event: job.completed
outcome: failed
correlation: job.id=job-1, job.project=demo
occurred_at: 2026-11-20T01:00:00Z
source: webhook
detail:
missing required field "owner"`
	if err := sess.HandleText(failed); err != nil {
		t.Fatalf("failed completion turn: %v", err)
	}
	data, err = os.ReadFile(jobFile)
	if err != nil {
		t.Fatalf("read corrected job.json: %v", err)
	}
	if !strings.Contains(string(data), `"owner":"fakeclaude"`) {
		t.Fatalf("corrected content = %q", data)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("commands after correction = %v", runner.runs)
	}
	calls = caller.callList()
	if len(calls) != 3 || calls[2].name != "subscribe_event" {
		t.Fatalf("tool calls after correction = %+v", calls)
	}
	if got := correlationJobID(calls[2].args); got != "job-2" {
		t.Fatalf("second subscription correlates %q, want job-2", got)
	}

	succeeded := `[castworks] Subscription sub-def completed.
system: extsim
event: job.completed
outcome: succeeded
correlation: job.id=job-2, job.project=demo
occurred_at: 2026-11-20T01:00:00Z
source: webhook
detail:
content accepted`
	if err := sess.HandleText(succeeded); err != nil {
		t.Fatalf("succeeded completion turn: %v", err)
	}

	text := resultText(t, decodeLines(t, splitLines(t, out.Bytes())))
	for _, want := range []string{"job-1", `missing required field "owner"`, "m-123", "job-2", "succeeded"} {
		if !strings.Contains(text, want) {
			t.Errorf("report %q does not mention %q", text, want)
		}
	}
}

func correlationJobID(args map[string]any) string {
	correlation, _ := args["correlation"].(map[string]any)
	if correlation == nil {
		return ""
	}
	value, _ := correlation["job.id"].(string)
	return value
}

// TestBackgroundMessagesMatchSamples 检查后台任务消息的次序与字段和已入库
// 的两份样本一致。
func TestBackgroundMessagesMatchSamples(t *testing.T) {
	script, err := parseScript("background", []byte(`{
		"rules":[{"name":"background","match":"watch","steps":[
			{"type":"background","command":"extsim job watch job-1","description":"Watch the job",
			 "reply":"The watched job finished with exit code ${exit_code}."},
			{"type":"reply","text":"started"}
		]}]
	}`))
	if err != nil {
		t.Fatalf("parseScript: %v", err)
	}
	done := make(chan commandResult, 1)
	runner := &fakeRunner{done: done}
	var out bytes.Buffer
	sess := newTestSession(t, script, runner, nil, &out)

	if err := sess.HandleText("watch the job in the background"); err != nil {
		t.Fatalf("background turn: %v", err)
	}
	done <- commandResult{ExitCode: 0}
	close(done)
	sess.Wait(2 * time.Second)

	got := splitLines(t, out.Bytes())
	messages := decodeLines(t, got)
	split := -1
	for i, message := range messages {
		if message["type"] == "system" && message["subtype"] == "task_updated" {
			split = i
			break
		}
	}
	if split < 0 {
		t.Fatal("no task_updated message")
	}
	deliverLines, deliverMessages := readRecordedSample(t, "local_turn03_deliver.jsonl")
	spontaneousLines, spontaneousMessages := readRecordedSample(t, "local_turn04_spontaneous.jsonl")

	assertSignatures(t, messages[:split], deliverMessages)
	assertSignatures(t, messages[split:], spontaneousMessages)
	assertTaskMessageShapes(t, got[:split], deliverLines)
	assertTaskMessageShapes(t, got[split:], spontaneousLines)
	assertBackgroundSemantics(t, messages)
}

// TestUserText 检查字符串与内容块数组两种用户消息。
func TestUserText(t *testing.T) {
	stringLine := []byte(`{"type":"user","message":{"role":"user","content":"hello"}}`)
	text, ok, err := userText(stringLine)
	if err != nil || !ok || text != "hello" {
		t.Fatalf("userText(string) = %q, %v, %v", text, ok, err)
	}

	blockLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"a"},{"type":"tool_result","content":"x"},{"type":"text","text":"b"}]}}`)
	text, ok, err = userText(blockLine)
	if err != nil || !ok || text != "a\nb" {
		t.Fatalf("userText(blocks) = %q, %v, %v", text, ok, err)
	}

	otherLine := []byte(`{"type":"assistant","message":{"role":"assistant","content":"x"}}`)
	if _, ok, err := userText(otherLine); err != nil || ok {
		t.Fatalf("userText(assistant) = %v, %v", ok, err)
	}
}

// readRecordedSample 读取已入库的录制样本，返回原始行与解码后的消息。
func readRecordedSample(t *testing.T, name string) ([][]byte, []map[string]any) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "pkg", "warden", "claudecode", "testdata", "recorded", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample %s: %v", path, err)
	}
	lines := splitLines(t, data)
	return lines, decodeLines(t, lines)
}

// assertSignatures 比较两条消息序列的 type/subtype。
func assertSignatures(t *testing.T, got, want []map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("message count = %d, want %d\ngot:  %v\nwant: %v",
			len(got), len(want), messageSignatures(got), messageSignatures(want))
	}
	for i := range got {
		if gotSig, wantSig := signature(got[i]), signature(want[i]); gotSig != wantSig {
			t.Fatalf("message %d = %s, want %s\ngot:  %v\nwant: %v",
				i, gotSig, wantSig, messageSignatures(got), messageSignatures(want))
		}
	}
}

func signature(message map[string]any) string {
	kind, _ := message["type"].(string)
	subtype, _ := message["subtype"].(string)
	return kind + "/" + subtype
}

func messageSignatures(messages []map[string]any) []string {
	signatures := make([]string, len(messages))
	for i, message := range messages {
		signatures[i] = signature(message)
	}
	return signatures
}

// assertTaskMessageShapes 比较后台任务消息的顶层字段与嵌套字段，字段次序
// 取原始 JSON 中的顺序。
func assertTaskMessageShapes(t *testing.T, got, want [][]byte) {
	t.Helper()
	bySubtype := func(lines [][]byte, subtype string) [][]byte {
		var found [][]byte
		for _, line := range lines {
			var message struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
			}
			if err := json.Unmarshal(line, &message); err != nil {
				t.Fatalf("decode message: %v", err)
			}
			if message.Type == "system" && message.Subtype == subtype {
				found = append(found, line)
			}
		}
		return found
	}

	for _, subtype := range []string{"background_tasks_changed", "task_started", "task_updated", "task_notification"} {
		gotMessages := bySubtype(got, subtype)
		if len(gotMessages) == 0 {
			continue
		}
		wantMessages := bySubtype(want, subtype)
		if len(wantMessages) == 0 {
			t.Fatalf("no %s message in the sample", subtype)
		}
		for i := range gotMessages {
			if i >= len(wantMessages) {
				t.Fatalf("more %s messages than the sample", subtype)
			}
			assertSameKeys(t, subtype, gotMessages[i], wantMessages[i])
		}
	}
}

// assertSameKeys 比较两条消息的字段名与字段次序。
func assertSameKeys(t *testing.T, subtype string, got, want []byte) {
	t.Helper()
	gotKeys := orderedKeys(t, got)
	wantKeys := orderedKeys(t, want)
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("%s keys = %v, want %v", subtype, gotKeys, wantKeys)
	}
	switch subtype {
	case "background_tasks_changed":
		gotTask := firstTask(t, got)
		wantTask := firstTask(t, want)
		if (gotTask == nil) != (wantTask == nil) {
			t.Fatalf("%s task presence differs", subtype)
		}
		if gotTask != nil {
			if keys := orderedKeys(t, gotTask); strings.Join(keys, ",") != "task_id,run_id,task_type,description" {
				t.Fatalf("%s task keys = %v", subtype, keys)
			}
			if keys := orderedKeys(t, wantTask); strings.Join(keys, ",") != "task_id,run_id,task_type,description" {
				t.Fatalf("sample %s task keys = %v", subtype, keys)
			}
		}
	case "task_updated":
		gotPatch := rawField(t, got, "patch")
		wantPatch := rawField(t, want, "patch")
		if keys := orderedKeys(t, gotPatch); strings.Join(keys, ",") != "status,end_time" {
			t.Fatalf("%s patch keys = %v", subtype, keys)
		}
		if keys := orderedKeys(t, wantPatch); strings.Join(keys, ",") != "status,end_time" {
			t.Fatalf("sample %s patch keys = %v", subtype, keys)
		}
	}
}

// firstTask 返回 tasks 数组的第一个对象，空数组返回 nil。
func firstTask(t *testing.T, raw []byte) []byte {
	t.Helper()
	tasks := rawField(t, raw, "tasks")
	var objects []json.RawMessage
	if err := json.Unmarshal(tasks, &objects); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(objects) == 0 {
		return nil
	}
	return objects[0]
}

// rawField 返回对象中某个字段的原始 JSON。
func rawField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	value, ok := object[field]
	if !ok {
		t.Fatalf("field %q is missing", field)
	}
	return value
}

// orderedKeys 按 JSON 中的出现顺序返回对象的一层键。
func orderedKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		t.Fatalf("object does not start with {: %v", err)
	}
	var keys []string
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			t.Fatalf("read object key: %v", err)
		}
		keys = append(keys, keyToken.(string))
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("skip object value: %v", err)
		}
	}
	return keys
}

// assertBackgroundSemantics 检查后台任务消息的关键取值。
func assertBackgroundSemantics(t *testing.T, messages []map[string]any) {
	t.Helper()
	var changed [][]any
	for _, message := range messages {
		switch message["subtype"] {
		case "background_tasks_changed":
			tasks, _ := message["tasks"].([]any)
			changed = append(changed, tasks)
		case "task_started":
			if message["is_backgrounded"] != true {
				t.Fatalf("task_started is_backgrounded = %v", message["is_backgrounded"])
			}
		case "task_updated":
			patch := message["patch"].(map[string]any)
			if patch["status"] != "completed" {
				t.Fatalf("task_updated status = %v", patch["status"])
			}
			if _, ok := patch["end_time"].(float64); !ok {
				t.Fatalf("task_updated end_time = %v", patch["end_time"])
			}
		case "task_notification":
			if message["status"] != "completed" {
				t.Fatalf("task_notification status = %v", message["status"])
			}
			if message["summary"] == "" {
				t.Fatal("task_notification summary is empty")
			}
		}
	}
	if len(changed) != 2 {
		t.Fatalf("background_tasks_changed count = %d, want 2", len(changed))
	}
	if len(changed[0]) != 1 || len(changed[1]) != 0 {
		t.Fatalf("task sets = %v then %v, want one task then none", changed[0], changed[1])
	}
}

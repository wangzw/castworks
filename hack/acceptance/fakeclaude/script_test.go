package main

import (
	"strings"
	"testing"
)

// TestLoadSubscriptionScript 检查演示脚本能装载，规则按顺序匹配。
func TestLoadSubscriptionScript(t *testing.T) {
	script, err := LoadScript("testdata/subscription.json")
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	if got := script.OwnerName(); got != "fakeclaude" {
		t.Fatalf("owner = %q, want fakeclaude", got)
	}
	if len(script.Rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(script.Rules))
	}

	cases := []struct {
		message string
		rule    string
	}{
		{"[castworks] Subscription sub-abc completed.\noutcome: failed", "subscription-completion"},
		{"Please submit the content to extsim and subscribe.", "submit-and-subscribe"},
		{"Run the job in the background and report.", "background-watch"},
		{"Submit a job to extsim and watch it in the background.", "background-watch"},
		{"hello", ""},
	}
	for _, tc := range cases {
		rule := script.Select(tc.message)
		got := ""
		if rule != nil {
			got = rule.Name
		}
		if got != tc.rule {
			t.Errorf("Select(%q) = %q, want %q", tc.message, got, tc.rule)
		}
	}
}

// TestParseScriptRejectsBadInput 检查脚本校验会拒绝缺字段与写错的名字。
func TestParseScriptRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"no rules":           `{"rules":[]}`,
		"missing match":      `{"rules":[{"steps":[{"type":"reply","text":"hi"}]}]}`,
		"bad regexp":         `{"rules":[{"match":"(","steps":[{"type":"reply","text":"hi"}]}]}`,
		"unknown step":       `{"rules":[{"match":"x","steps":[{"type":"teleport"}]}]}`,
		"command without":    `{"rules":[{"match":"x","steps":[{"type":"command"}]}]}`,
		"bad capture regexp": `{"rules":[{"match":"x","steps":[{"type":"command","command":"true","capture_regex":"("}]}]}`,
		"write without path": `{"rules":[{"match":"x","steps":[{"type":"write","content":"hi"}]}]}`,
		"mcp without tool":   `{"rules":[{"match":"x","steps":[{"type":"mcp"}]}]}`,
		"reply without text": `{"rules":[{"match":"x","steps":[{"type":"reply"}]}]}`,
		"unknown field":      `{"rules":[{"match":"x","steps":[{"type":"reply","text":"hi"}],"extra":1}]}`,
		"empty outcome":      `{"rules":[{"match":"x","outcomes":{"":[{"type":"reply","text":"hi"}]}}]}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseScript(name, []byte(data)); err == nil {
				t.Fatalf("parseScript accepted %s", data)
			}
		})
	}
}

// TestApplyCaptures 检查捕获组与整段匹配两种取值方式。
func TestApplyCaptures(t *testing.T) {
	script, err := parseScript("capture", []byte(`{
		"rules":[{"name":"r","match":"marker","capture":{
			"marker":"marker[^A-Za-z0-9_-]+([A-Za-z0-9_-]+)",
			"content":"\\{[^\\n]*\\}",
			"whole":"marker"
		},"steps":[{"type":"reply","text":"ok"}]}]
	}`))
	if err != nil {
		t.Fatalf("parseScript: %v", err)
	}
	vars := map[string]string{}
	script.Rules[0].ApplyCaptures(`Use {"name":"x"} with marker: abc-123.`, vars)
	if vars["marker"] != "abc-123" {
		t.Errorf("marker = %q, want abc-123", vars["marker"])
	}
	if vars["content"] != `{"name":"x"}` {
		t.Errorf("content = %q", vars["content"])
	}
	if vars["whole"] != "marker" {
		t.Errorf("whole = %q, want marker", vars["whole"])
	}
}

// TestExpand 检查模板替换与缺变量报错。
func TestExpand(t *testing.T) {
	vars := map[string]string{"job_id": "job-1", "reason": `missing "owner"`}
	got, err := Expand("submit ${job_id}: ${reason}", vars)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if want := `submit job-1: missing "owner"`; got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
	if _, err := Expand("${missing}", vars); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("Expand with a missing variable = %v, want an error naming it", err)
	}
}

// TestExpandValue 检查嵌套 JSON 值里的模板替换。
func TestExpandValue(t *testing.T) {
	vars := map[string]string{"job_id": "job-1"}
	input := map[string]any{
		"correlation": map[string]any{"job.id": "${job_id}"},
		"labels":      []any{"id=${job_id}", 3},
	}
	got, err := ExpandValue(input, vars)
	if err != nil {
		t.Fatalf("ExpandValue: %v", err)
	}
	object := got.(map[string]any)
	correlation := object["correlation"].(map[string]any)
	if correlation["job.id"] != "job-1" {
		t.Errorf("correlation job.id = %v", correlation["job.id"])
	}
	labels := object["labels"].([]any)
	if labels[0] != "id=job-1" || labels[1] != 3 {
		t.Errorf("labels = %v", labels)
	}
}

// TestCorrectContent 检查失败后补 owner，非 JSON 内容原样返回。
func TestCorrectContent(t *testing.T) {
	got := CorrectContent(`{"marker":"m1"}`, "fakeclaude")
	if !strings.Contains(got, `"owner":"fakeclaude"`) || !strings.Contains(got, `"marker":"m1"`) {
		t.Fatalf("CorrectContent = %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("CorrectContent kept a trailing newline: %q", got)
	}
	if got := CorrectContent("not json", "fakeclaude"); got != "not json" {
		t.Fatalf("CorrectContent changed non-JSON content: %q", got)
	}
	if got := CorrectContent(`["array"]`, "fakeclaude"); got != `["array"]` {
		t.Fatalf("CorrectContent changed a non-object: %q", got)
	}
}

// TestParseCompletion 检查订阅完成事件文本的解析。
func TestParseCompletion(t *testing.T) {
	failed := `Some human message.

[castworks] Subscription sub-abc completed.
system: extsim
event: job.completed
outcome: failed
correlation: job.id=job-1, job.project=demo
occurred_at: 2026-11-20T01:00:00Z
source: webhook
detail:
missing required field "owner"`

	completion, ok := ParseCompletion(failed)
	if !ok {
		t.Fatal("ParseCompletion did not find the completion text")
	}
	if completion.SubID != "sub-abc" || completion.Outcome != "failed" || completion.Source != "webhook" {
		t.Fatalf("completion = %+v", completion)
	}
	if completion.Detail != `missing required field "owner"` {
		t.Fatalf("detail = %q", completion.Detail)
	}
	if completion.Correlation["job.id"] != "job-1" || completion.Correlation["job.project"] != "demo" {
		t.Fatalf("correlation = %v", completion.Correlation)
	}
	vars := completion.Vars()
	if vars["reason"] != completion.Detail {
		t.Errorf("reason = %q", vars["reason"])
	}
	if vars["job_id"] != "job-1" {
		t.Errorf("job_id variable = %q", vars["job_id"])
	}
	if want := "job.id=job-1, job.project=demo"; vars["correlation"] != want {
		t.Errorf("correlation variable = %q, want %q", vars["correlation"], want)
	}

	truncated := `[castworks] Subscription sub-def completed.
system: extsim
event: job.completed
outcome: succeeded
correlation: job.id=job-2
occurred_at: 2026-11-20T01:00:00Z
source: verify
detail:
long detail
(detail truncated)`
	completion, ok = ParseCompletion(truncated)
	if !ok {
		t.Fatal("ParseCompletion did not find the truncated completion text")
	}
	if completion.Outcome != "succeeded" || completion.Source != "verify" {
		t.Fatalf("completion = %+v", completion)
	}
	if completion.Detail != "long detail" || !completion.DetailTruncated {
		t.Fatalf("detail = %q, truncated = %v", completion.Detail, completion.DetailTruncated)
	}

	if _, ok := ParseCompletion("no completion here"); ok {
		t.Fatal("ParseCompletion matched a message without a completion")
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// 动作类型。脚本按收到的用户消息匹配规则，规则由一串动作组成。
const (
	stepWrite      = "write"
	stepCommand    = "command"
	stepBackground = "background"
	stepMCP        = "mcp"
	stepReply      = "reply"
)

// defaultOwner 是订阅结论为 failed 时补进内容的 owner 默认值。
const defaultOwner = "fakeclaude"

// defaultIDPattern 从 extsim job submit 输出的作业对象中取作业标识。
var defaultIDPattern = regexp.MustCompile(`"id"\s*:\s*"([^"]+)"`)

// templatePattern 匹配脚本里的 ${变量} 占位符。
var templatePattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// completionFirstLine 匹配订阅完成事件文本的首行。
var completionFirstLine = regexp.MustCompile(`^\[castworks\] Subscription (\S+) completed\.$`)

// Script 是假 Claude Code 的行为脚本。
type Script struct {
	// Owner 是订阅失败后补进内容的 owner；空值取 defaultOwner。
	Owner string `json:"owner"`
	// DefaultReply 是没有任何规则匹配时回复的文本。
	DefaultReply string `json:"default_reply"`
	// Rules 按顺序匹配，第一条匹配的规则决定本回合的动作。
	Rules []Rule `json:"rules"`
}

// OwnerName 返回补进内容的 owner。
func (s *Script) OwnerName() string {
	if s.Owner == "" {
		return defaultOwner
	}
	return s.Owner
}

// Rule 是一条“消息匹配什么就做什么”的规则。
type Rule struct {
	// Name 只用于报错与阅读。
	Name string `json:"name"`
	// Match 是匹配用户消息的正则表达式。
	Match string `json:"match"`
	// Capture 把消息中匹配到的子串存进变量，键是变量名，值是一个正则
	// 表达式；有捕获组时取第一组，否则取整个匹配。
	Capture map[string]string `json:"capture"`
	// Steps 是本回合依次执行的动作。
	Steps []Step `json:"steps"`
	// Outcomes 按订阅完成事件的 outcome 选择动作；给出时优先于 Steps。
	Outcomes map[string][]Step `json:"outcomes"`

	match    *regexp.Regexp
	captures map[string]*regexp.Regexp
}

// Step 是一个动作。
type Step struct {
	// Type 取 write、command、background、mcp、reply。
	Type string `json:"type"`
	// Path 是 write 动作的目标文件，相对工作目录。
	Path string `json:"path,omitempty"`
	// Content 是 write 动作写入的内容，可以含 ${变量}。
	Content string `json:"content,omitempty"`
	// Command 是 command 与 background 动作执行的 shell 命令。
	Command string `json:"command,omitempty"`
	// Description 是 Bash 工具调用的说明，也是后台任务的描述。
	Description string `json:"description,omitempty"`
	// Capture 是 command 动作把捕获结果存进的变量名，默认 job_id。
	Capture string `json:"capture,omitempty"`
	// CaptureRegex 是 command 动作从标准输出捕获的表达式，默认取作业对象
	// 的 id。
	CaptureRegex string `json:"capture_regex,omitempty"`
	// Tool 是 mcp 动作调用的工具名，可以是 list_events、subscribe_event，
	// 也可以带 mcp__castworks__ 前缀。
	Tool string `json:"tool,omitempty"`
	// Arguments 是 mcp 动作的参数，字符串值可以含 ${变量}。
	Arguments map[string]any `json:"arguments,omitempty"`
	// Text 是 reply 动作回复的文本，可以含 ${变量}。
	Text string `json:"text,omitempty"`
	// Reply 是 background 动作完成后自发回合的回复，可以含 ${变量}。
	Reply string `json:"reply,omitempty"`
}

// LoadScript 读取并校验脚本文件。
func LoadScript(path string) (*Script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read script: %w", err)
	}
	return parseScript(path, data)
}

// parseScript 解析并校验脚本内容。
func parseScript(name string, data []byte) (*Script, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var script Script
	if err := dec.Decode(&script); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if err := script.prepare(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &script, nil
}

// prepare 编译正则表达式并校验每种动作的必填字段。
func (s *Script) prepare() error {
	if len(s.Rules) == 0 {
		return fmt.Errorf("script has no rules")
	}
	for i := range s.Rules {
		rule := &s.Rules[i]
		label := rule.Name
		if label == "" {
			label = fmt.Sprintf("rule %d", i)
		}
		if rule.Match == "" {
			return fmt.Errorf("%s: match is empty", label)
		}
		re, err := regexp.Compile(rule.Match)
		if err != nil {
			return fmt.Errorf("%s: compile match: %w", label, err)
		}
		rule.match = re
		rule.captures = make(map[string]*regexp.Regexp, len(rule.Capture))
		for name, pattern := range rule.Capture {
			if name == "" {
				return fmt.Errorf("%s: capture name is empty", label)
			}
			cre, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("%s: compile capture %s: %w", label, name, err)
			}
			rule.captures[name] = cre
		}
		if len(rule.Steps) == 0 && len(rule.Outcomes) == 0 {
			return fmt.Errorf("%s: no steps", label)
		}
		if err := validateSteps(label, rule.Steps); err != nil {
			return err
		}
		for outcome, steps := range rule.Outcomes {
			if outcome == "" {
				return fmt.Errorf("%s: outcome is empty", label)
			}
			if err := validateSteps(label+" outcome "+outcome, steps); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSteps 校验一组动作的必填字段。
func validateSteps(label string, steps []Step) error {
	for i, step := range steps {
		where := fmt.Sprintf("%s step %d", label, i)
		switch step.Type {
		case stepWrite:
			if step.Path == "" {
				return fmt.Errorf("%s: write has no path", where)
			}
		case stepCommand, stepBackground:
			if step.Command == "" {
				return fmt.Errorf("%s: %s has no command", where, step.Type)
			}
			if step.CaptureRegex != "" {
				if _, err := regexp.Compile(step.CaptureRegex); err != nil {
					return fmt.Errorf("%s: compile capture_regex: %w", where, err)
				}
			}
		case stepMCP:
			if step.Tool == "" {
				return fmt.Errorf("%s: mcp has no tool", where)
			}
		case stepReply:
			if step.Text == "" {
				return fmt.Errorf("%s: reply has no text", where)
			}
		default:
			return fmt.Errorf("%s: unknown type %q", where, step.Type)
		}
	}
	return nil
}

// Select 返回第一条匹配消息的规则，没有时返回 nil。
func (s *Script) Select(text string) *Rule {
	for i := range s.Rules {
		if s.Rules[i].match.MatchString(text) {
			return &s.Rules[i]
		}
	}
	return nil
}

// ApplyCaptures 把规则捕获的变量存进 vars。变量名排序后依次应用，结果与
// 遍历 map 的顺序无关。
func (r *Rule) ApplyCaptures(text string, vars map[string]string) {
	names := make([]string, 0, len(r.captures))
	for name := range r.captures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		match := r.captures[name].FindStringSubmatch(text)
		if match == nil {
			continue
		}
		if len(match) > 1 {
			vars[name] = match[1]
		} else {
			vars[name] = match[0]
		}
	}
}

// Expand 替换文本中的 ${变量}。变量不存在时报错，避免命令里留下空值。
func Expand(text string, vars map[string]string) (string, error) {
	var missing string
	expanded := templatePattern.ReplaceAllStringFunc(text, func(placeholder string) string {
		name := placeholder[2 : len(placeholder)-1]
		value, ok := vars[name]
		if !ok {
			if missing == "" {
				missing = name
			}
			return placeholder
		}
		return value
	})
	if missing != "" {
		return "", fmt.Errorf("template variable %q is not set", missing)
	}
	return expanded, nil
}

// ExpandValue 递归替换 JSON 值里的模板变量。
func ExpandValue(value any, vars map[string]string) (any, error) {
	switch v := value.(type) {
	case string:
		return Expand(v, vars)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			expanded, err := ExpandValue(item, vars)
			if err != nil {
				return nil, err
			}
			out[i] = expanded
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			expanded, err := ExpandValue(item, vars)
			if err != nil {
				return nil, err
			}
			out[key] = expanded
		}
		return out, nil
	default:
		return value, nil
	}
}

// Completion 是订阅完成事件文本解析后的内容。
type Completion struct {
	SubID           string
	System          string
	Event           string
	Outcome         string
	Correlation     map[string]string
	OccurredAt      string
	Source          string
	Detail          string
	DetailTruncated bool
}

// ParseCompletion 从用户消息里解析订阅完成事件文本。消息里没有这段文本时
// 返回 false。
func ParseCompletion(text string) (*Completion, bool) {
	index := strings.Index(text, "[castworks] Subscription ")
	if index < 0 {
		return nil, false
	}
	lines := strings.Split(trimTrailingNewline(text[index:]), "\n")
	first := completionFirstLine.FindStringSubmatch(lines[0])
	if first == nil {
		return nil, false
	}
	completion := &Completion{SubID: first[1], Correlation: map[string]string{}}
	detailAt := -1
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if line == "detail:" {
			detailAt = i + 1
			break
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "system":
			completion.System = value
		case "event":
			completion.Event = value
		case "outcome":
			completion.Outcome = value
		case "occurred_at":
			completion.OccurredAt = value
		case "source":
			completion.Source = value
		case "correlation":
			completion.Correlation = parseCorrelation(value)
		}
	}
	if detailAt >= 0 {
		detailLines := lines[detailAt:]
		if n := len(detailLines); n > 0 && detailLines[n-1] == "(detail truncated)" {
			completion.DetailTruncated = true
			detailLines = detailLines[:n-1]
		}
		completion.Detail = strings.Join(detailLines, "\n")
	}
	return completion, true
}

// parseCorrelation 解析 “键=值, 键=值” 形式的关联键。
func parseCorrelation(value string) map[string]string {
	correlation := map[string]string{}
	for _, part := range strings.Split(value, ", ") {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		correlation[key] = val
	}
	return correlation
}

// Vars 把订阅完成事件转换成模板变量。
func (c *Completion) Vars() map[string]string {
	vars := map[string]string{
		"sub_id":      c.SubID,
		"system":      c.System,
		"event":       c.Event,
		"outcome":     c.Outcome,
		"occurred_at": c.OccurredAt,
		"source":      c.Source,
		"detail":      c.Detail,
		"reason":      c.Detail,
	}
	keys := make([]string, 0, len(c.Correlation))
	for key := range c.Correlation {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+c.Correlation[key])
		vars[sanitizeVarName(key)] = c.Correlation[key]
	}
	vars["correlation"] = strings.Join(parts, ", ")
	return vars
}

// sanitizeVarName 把关联键里的点换成下划线，使它可以作为模板变量名。
func sanitizeVarName(key string) string {
	return strings.ReplaceAll(key, ".", "_")
}

// FailureReason 从订阅完成事件的 detail 中取出作业失败原因。extsim 的 detail
// 是回调负载，原因嵌在 job.reason；不是这种形状时原样返回 detail。
func FailureReason(detail string) string {
	var payload struct {
		Job struct {
			Reason string `json:"reason"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(detail), &payload); err == nil && payload.Job.Reason != "" {
		return payload.Job.Reason
	}
	return detail
}

// CorrectContent 在内容是 JSON 对象时补上 owner，其余情况原样返回。
func CorrectContent(content, owner string) string {
	var object map[string]any
	if err := json.Unmarshal([]byte(content), &object); err != nil || object == nil {
		return content
	}
	object["owner"] = owner
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(object); err != nil {
		return content
	}
	return trimTrailingNewline(buf.String())
}

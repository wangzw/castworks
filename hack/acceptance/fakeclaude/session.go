package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// commandResult 是一条命令的执行结果。
type commandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// failed 表示命令没有正常成功退出。
func (r commandResult) failed() bool {
	return r.Err != nil || r.ExitCode != 0
}

// output 返回命令的全部输出，标准输出在前。
func (r commandResult) output() string {
	if r.Err != nil {
		if r.Stderr != "" {
			return r.Stdout + r.Stderr + "\n" + r.Err.Error()
		}
		return r.Stdout + r.Err.Error()
	}
	return r.Stdout + r.Stderr
}

// backgroundHandle 是一个已启动的后台命令。
type backgroundHandle struct {
	Done <-chan commandResult
}

// commandRunner 执行脚本里的命令。真实实现启动 /bin/sh，测试用替身。
type commandRunner interface {
	// Run 在前台执行命令并等待结束。
	Run(ctx context.Context, command string) commandResult
	// Start 在后台执行命令，标准输出与标准错误写进 outputPath。
	Start(ctx context.Context, command, outputPath string) backgroundHandle
}

// toolCaller 调用平台 MCP 服务器上的工具。真实实现使用 go-sdk，测试用替身。
type toolCaller interface {
	CallTool(ctx context.Context, name string, args map[string]any) (text string, isError bool, err error)
	Close() error
}

// session 是一个常驻的假 Claude Code 会话：读 stdin 的用户消息，按脚本
// 写出 stream-json 消息。
type session struct {
	script  *Script
	out     *emitter
	workDir string
	caller  toolCaller
	runner  commandRunner
	ctx     context.Context
	errw    io.Writer

	runID string
	// vars 是脚本变量，只在持有 turnMu 时读写。
	vars map[string]string
	// alive 是当前存活的后台任务集合，只在持有 turnMu 时读写。
	alive map[string]bgTask

	turnMu sync.Mutex
	bg     sync.WaitGroup
}

func newSession(script *Script, out *emitter, workDir string, caller toolCaller, runner commandRunner, ctx context.Context, errw io.Writer) *session {
	return &session{
		script:  script,
		out:     out,
		workDir: workDir,
		caller:  caller,
		runner:  runner,
		ctx:     ctx,
		errw:    errw,
		runID:   newRunID(),
		vars:    map[string]string{},
		alive:   map[string]bgTask{},
	}
}

// HandleLine 解析一行 stdin 消息。不是用户消息时忽略，返回成功。
func (s *session) HandleLine(line []byte) error {
	text, ok, err := userText(line)
	if err != nil {
		return err
	}
	if !ok || text == "" {
		return nil
	}
	return s.HandleText(text)
}

// userText 从 stream-json 用户消息中取出文本。content 可以是字符串，也可以
// 是内容块数组。
func userText(line []byte) (string, bool, error) {
	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &payload); err != nil {
		return "", false, fmt.Errorf("decode stdin line: %w", err)
	}
	if payload.Type != "user" {
		return "", false, nil
	}
	content := bytes.TrimSpace(payload.Message.Content)
	if len(content) == 0 {
		return "", false, nil
	}
	if content[0] == '"' {
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return "", false, fmt.Errorf("decode user content: %w", err)
		}
		return text, true, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", false, fmt.Errorf("decode user content blocks: %w", err)
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n"), true, nil
}

// HandleText 处理一条用户消息，串行执行一个回合。
func (s *session) HandleText(text string) error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return s.turn(text)
}

// turn 写出一回合的完整消息序列。调用方必须持有 turnMu。
func (s *session) turn(text string) error {
	if err := s.out.sessionState("running"); err != nil {
		return err
	}
	if err := s.out.init(); err != nil {
		return err
	}

	var replies []string
	steps := []Step{}
	rule := s.script.Select(text)
	if rule == nil {
		if s.script.DefaultReply != "" {
			replies = append(replies, s.script.DefaultReply)
		}
	} else {
		rule.ApplyCaptures(text, s.vars)
		steps = rule.Steps
		if completion, ok := ParseCompletion(text); ok {
			for name, value := range completion.Vars() {
				s.vars[name] = value
			}
			if completion.Outcome == "failed" {
				// 失败原因要在后续回合的报告里继续可用，单独保留。
				s.vars["failure_reason"] = completion.Detail
				// 第一次提交不通过时，按 extsim 的规则补上 owner 再重交。
				s.vars["content"] = CorrectContent(s.vars["content"], s.script.OwnerName())
			}
			if rule.Outcomes != nil {
				outcomeSteps, ok := rule.Outcomes[completion.Outcome]
				if !ok || len(outcomeSteps) == 0 {
					return s.failTurn(fmt.Errorf("no steps for outcome %q", completion.Outcome))
				}
				steps = outcomeSteps
			}
		}
	}

	for _, step := range steps {
		reply, err := s.execute(step)
		if err != nil {
			return s.failTurn(err)
		}
		if step.Type == stepReply {
			replies = append(replies, reply)
		}
	}

	output := strings.Join(replies, "\n")
	if output != "" {
		if err := s.out.assistantText(output); err != nil {
			return err
		}
	}
	if err := s.out.result(output, false); err != nil {
		return err
	}
	return s.out.sessionState("idle")
}

// failTurn 以错误结束当前回合。
func (s *session) failTurn(err error) error {
	text := "fakeclaude: " + err.Error()
	if writeErr := s.out.assistantText(text); writeErr != nil {
		return writeErr
	}
	if writeErr := s.out.result(text, true); writeErr != nil {
		return writeErr
	}
	if writeErr := s.out.sessionState("idle"); writeErr != nil {
		return writeErr
	}
	return err
}

// execute 执行一个动作。reply 动作返回要回复的文本，其余返回空串。
func (s *session) execute(step Step) (string, error) {
	switch step.Type {
	case stepWrite:
		return "", s.writeFile(step)
	case stepCommand:
		return "", s.runCommand(step)
	case stepBackground:
		return "", s.startBackground(step)
	case stepMCP:
		return "", s.callTool(step)
	case stepReply:
		return Expand(step.Text, s.vars)
	default:
		return "", fmt.Errorf("unknown step type %q", step.Type)
	}
}

// writeFile 把模板展开后的内容写进工作目录下的文件。
func (s *session) writeFile(step Step) error {
	path, err := s.resolvePath(step.Path)
	if err != nil {
		return err
	}
	content, err := Expand(step.Content, s.vars)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// resolvePath 把脚本里的相对路径限制在工作目录内。
func (s *session) resolvePath(name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("path %q must be relative to the working directory", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the working directory", name)
	}
	return filepath.Join(s.workDir, clean), nil
}

// runCommand 以前台方式执行命令，写出工具调用与工具结果，并捕获标识。
func (s *session) runCommand(step Step) error {
	command, err := Expand(step.Command, s.vars)
	if err != nil {
		return err
	}
	description := step.Description
	if description == "" {
		description = command
	}
	toolUseID := newToolUseID()
	if err := s.out.assistantToolUse(toolUseID, "Bash", map[string]any{
		"command":     command,
		"description": description,
	}); err != nil {
		return err
	}

	result := s.runner.Run(s.ctx, command)
	if result.failed() {
		output := result.output()
		if err := s.out.toolResult(toolUseID, output, true); err != nil {
			return err
		}
		return fmt.Errorf("command %q failed: %s", command, strings.TrimSpace(output))
	}
	if err := s.out.toolResult(toolUseID, result.Stdout, false); err != nil {
		return err
	}

	name := step.Capture
	if name == "" {
		name = "job_id"
	}
	pattern := defaultIDPattern
	if step.CaptureRegex != "" {
		pattern, err = regexp.Compile(step.CaptureRegex)
		if err != nil {
			return fmt.Errorf("compile capture regexp: %w", err)
		}
	}
	match := pattern.FindStringSubmatch(result.Stdout)
	if match == nil {
		return fmt.Errorf("command %q output does not match %s", command, pattern)
	}
	value := match[0]
	if len(match) > 1 {
		value = match[1]
	}
	s.vars[name] = value
	return nil
}

// callTool 调用平台 MCP 服务器上的工具，写出工具调用与工具结果。
func (s *session) callTool(step Step) error {
	name := strings.TrimPrefix(step.Tool, "mcp__castworks__")
	arguments := make(map[string]any, len(step.Arguments))
	for key, value := range step.Arguments {
		expanded, err := ExpandValue(value, s.vars)
		if err != nil {
			return err
		}
		arguments[key] = expanded
	}

	toolUseID := newToolUseID()
	if err := s.out.assistantToolUse(toolUseID, "mcp__castworks__"+name, arguments); err != nil {
		return err
	}
	if s.caller == nil {
		text := "no platform MCP server is configured; start warden with --connector"
		if err := s.out.toolResult(toolUseID, text, true); err != nil {
			return err
		}
		return fmt.Errorf("call %s: %s", name, text)
	}
	text, isError, err := s.caller.CallTool(s.ctx, name, arguments)
	if err != nil {
		failure := "platform MCP server is unavailable: " + err.Error()
		if writeErr := s.out.toolResult(toolUseID, failure, true); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("call %s: %w", name, err)
	}
	if err := s.out.toolResult(toolUseID, text, isError); err != nil {
		return err
	}
	if isError {
		return fmt.Errorf("tool %s returned an error: %s", name, text)
	}
	s.vars["tool_result"] = text
	return nil
}

// startBackground 以后台方式执行命令并写出后台任务消息。命令结束时由
// finishBackground 写出结束消息并开始一个自发回合。
func (s *session) startBackground(step Step) error {
	command, err := Expand(step.Command, s.vars)
	if err != nil {
		return err
	}
	description := step.Description
	if description == "" {
		description = command
	}
	task := bgTask{
		TaskID:      newTaskID(),
		RunID:       s.runID,
		TaskType:    "local_bash",
		Description: description,
	}
	toolUseID := newToolUseID()
	if err := s.out.assistantToolUse(toolUseID, "Bash", map[string]any{
		"command":           command,
		"description":       description,
		"run_in_background": true,
	}); err != nil {
		return err
	}
	// 样本中 background_tasks_changed 先于 task_started 出现。
	s.alive[task.TaskID] = task
	if err := s.out.backgroundTasksChanged(s.taskList()); err != nil {
		return err
	}
	if err := s.out.taskStarted(task, toolUseID); err != nil {
		return err
	}

	outputDir := filepath.Join(s.workDir, ".fakeclaude", "tasks")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return fmt.Errorf("create task output directory: %w", err)
	}
	outputPath := filepath.Join(outputDir, task.TaskID+".output")
	result := fmt.Sprintf(
		"Command running in background with ID: %s. Output is being written to: %s. You will be notified when it completes. To check interim output, use Read on that file path.",
		task.TaskID, outputPath)
	if err := s.out.toolResult(toolUseID, result, false); err != nil {
		return err
	}

	handle := s.runner.Start(s.ctx, command, outputPath)
	s.bg.Add(1)
	go s.finishBackground(task, toolUseID, outputPath, step.Reply, handle)
	return nil
}

// finishBackground 在一个后台命令结束时写出 task_updated、task_notification
// 与空集的 background_tasks_changed，然后开始一个自发回合。
func (s *session) finishBackground(task bgTask, toolUseID, outputPath, replyTemplate string, handle backgroundHandle) {
	defer s.bg.Done()
	result := <-handle.Done
	if s.ctx.Err() != nil {
		return
	}

	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.vars["exit_code"] = strconv.Itoa(result.ExitCode)
	status := "completed"
	if result.failed() {
		status = "failed"
	}
	if err := s.out.taskUpdated(task.TaskID, task.RunID, status, nowMillis()); err != nil {
		s.logf("write task_updated: %v", err)
		return
	}
	summary := fmt.Sprintf("Background command %q completed (exit code %d)", task.Description, result.ExitCode)
	if status == "failed" {
		summary = fmt.Sprintf("Background command %q failed (exit code %d)", task.Description, result.ExitCode)
	}
	if err := s.out.taskNotification(task, toolUseID, status, outputPath, summary); err != nil {
		s.logf("write task_notification: %v", err)
		return
	}
	delete(s.alive, task.TaskID)
	if err := s.out.backgroundTasksChanged(s.taskList()); err != nil {
		s.logf("write background_tasks_changed: %v", err)
		return
	}

	reply := replyTemplate
	if reply == "" {
		reply = "Background command finished with exit code " + s.vars["exit_code"] + "."
	} else {
		expanded, err := Expand(reply, s.vars)
		if err != nil {
			reply = "fakeclaude: " + err.Error()
		} else {
			reply = expanded
		}
	}
	if err := s.out.sessionState("running"); err != nil {
		s.logf("write session_state_changed: %v", err)
		return
	}
	if err := s.out.init(); err != nil {
		s.logf("write init: %v", err)
		return
	}
	if err := s.out.assistantText(reply); err != nil {
		s.logf("write assistant: %v", err)
		return
	}
	if err := s.out.result(reply, false); err != nil {
		s.logf("write result: %v", err)
		return
	}
	if err := s.out.sessionState("idle"); err != nil {
		s.logf("write session_state_changed: %v", err)
	}
}

// taskList 返回按任务标识排序的存活任务集合。调用方必须持有 turnMu。
func (s *session) taskList() []bgTask {
	tasks := make([]bgTask, 0, len(s.alive))
	for _, task := range s.alive {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })
	return tasks
}

// Wait 等待后台命令的收尾，最多等 timeout。
func (s *session) Wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (s *session) logf(format string, args ...any) {
	if s.errw == nil {
		return
	}
	_, _ = fmt.Fprintf(s.errw, "fakeclaude: "+format+"\n", args...)
}

// newLineScanner 返回读取 stdin 的扫描器。用户消息可能接近 1 MiB，缓冲区
// 上限取 4 MiB。
func newLineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return scanner
}

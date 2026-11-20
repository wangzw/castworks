package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// emitter 把 stream-json 消息逐行写到 stdout。消息类型与字段次序照抄
// pkg/warden/claudecode/testdata/recorded 下已入库的样本。
type emitter struct {
	mu        sync.Mutex
	w         io.Writer
	cwd       string
	sessionID string
}

func newEmitter(w io.Writer, cwd string) *emitter {
	return &emitter{w: w, cwd: cwd, sessionID: newUUID()}
}

// write 编码一条消息并写出一行。
func (e *emitter) write(message any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(message); err != nil {
		return fmt.Errorf("encode stream message: %w", err)
	}
	if _, err := e.w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write stream message: %w", err)
	}
	return nil
}

type sessionStateWire struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	State     string `json:"state"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// sessionState 写出会话状态变化。状态变化的消息形状与样本一致。
func (e *emitter) sessionState(state string) error {
	return e.write(sessionStateWire{
		Type:      "system",
		Subtype:   "session_state_changed",
		State:     state,
		UUID:      newUUID(),
		SessionID: e.sessionID,
	})
}

type initWire struct {
	Type              string   `json:"type"`
	Subtype           string   `json:"subtype"`
	Cwd               string   `json:"cwd"`
	SessionID         string   `json:"session_id"`
	Tools             []string `json:"tools"`
	MCPServers        []any    `json:"mcp_servers"`
	Model             string   `json:"model"`
	PermissionMode    string   `json:"permissionMode"`
	ClaudeCodeVersion string   `json:"claude_code_version"`
	UUID              string   `json:"uuid"`
}

// init 写出会话初始化消息。看守进程从它取得 session_id。
func (e *emitter) init() error {
	return e.write(initWire{
		Type:              "system",
		Subtype:           "init",
		Cwd:               e.cwd,
		SessionID:         e.sessionID,
		Tools:             []string{"Bash", "Read", "Write"},
		MCPServers:        []any{},
		Model:             "fake-claude",
		PermissionMode:    "bypassPermissions",
		ClaudeCodeVersion: "fakeclaude",
		UUID:              newUUID(),
	})
}

type assistantContentWire struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type assistantMessageBody struct {
	Model   string                 `json:"model"`
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Role    string                 `json:"role"`
	Content []assistantContentWire `json:"content"`
}

type assistantWire struct {
	Type            string               `json:"type"`
	Message         assistantMessageBody `json:"message"`
	ParentToolUseID *string              `json:"parent_tool_use_id"`
	SessionID       string               `json:"session_id"`
	UUID            string               `json:"uuid"`
}

// assistantText 写出一条只含文本的 assistant 消息。
func (e *emitter) assistantText(text string) error {
	return e.write(assistantWire{
		Type: "assistant",
		Message: assistantMessageBody{
			Model:   "fake-claude",
			ID:      "msg_" + randomHex(10),
			Type:    "message",
			Role:    "assistant",
			Content: []assistantContentWire{{Type: "text", Text: text}},
		},
		SessionID: e.sessionID,
		UUID:      newUUID(),
	})
}

// assistantToolUse 写出一条含工具调用的 assistant 消息。
func (e *emitter) assistantToolUse(toolUseID, name string, input any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode tool input: %w", err)
	}
	return e.write(assistantWire{
		Type: "assistant",
		Message: assistantMessageBody{
			Model: "fake-claude",
			ID:    "msg_" + randomHex(10),
			Type:  "message",
			Role:  "assistant",
			Content: []assistantContentWire{{
				Type:  "tool_use",
				ID:    toolUseID,
				Name:  name,
				Input: encoded,
			}},
		},
		SessionID: e.sessionID,
		UUID:      newUUID(),
	})
}

type toolResultContentWire struct {
	ToolUseID string `json:"tool_use_id"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

type toolResultMessageBody struct {
	Role    string                  `json:"role"`
	Content []toolResultContentWire `json:"content"`
}

type toolResultWire struct {
	Type            string                `json:"type"`
	Message         toolResultMessageBody `json:"message"`
	ParentToolUseID *string               `json:"parent_tool_use_id"`
	SessionID       string                `json:"session_id"`
	UUID            string                `json:"uuid"`
}

// toolResult 写出一条工具结果消息。
func (e *emitter) toolResult(toolUseID, content string, isError bool) error {
	return e.write(toolResultWire{
		Type: "user",
		Message: toolResultMessageBody{
			Role: "user",
			Content: []toolResultContentWire{{
				ToolUseID: toolUseID,
				Type:      "tool_result",
				Content:   content,
				IsError:   isError,
			}},
		},
		SessionID: e.sessionID,
		UUID:      newUUID(),
	})
}

type resultWire struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	IsError         bool   `json:"is_error"`
	Result          string `json:"result"`
	QueuedTurnCount int    `json:"queued_turn_count"`
	SessionID       string `json:"session_id"`
	UUID            string `json:"uuid"`
}

// result 写出回合结果。
func (e *emitter) result(text string, isError bool) error {
	subtype := "success"
	if isError {
		subtype = "error"
	}
	return e.write(resultWire{
		Type:            "result",
		Subtype:         subtype,
		IsError:         isError,
		Result:          text,
		QueuedTurnCount: 0,
		SessionID:       e.sessionID,
		UUID:            newUUID(),
	})
}

// bgTask 是 background_tasks_changed 里的任务对象。
type bgTask struct {
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id"`
	TaskType    string `json:"task_type"`
	Description string `json:"description"`
}

type backgroundTasksChangedWire struct {
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	Tasks     []bgTask `json:"tasks"`
	UUID      string   `json:"uuid"`
	SessionID string   `json:"session_id"`
}

// backgroundTasksChanged 写出完整的存活任务集合。空集合写成 []，不是 null。
func (e *emitter) backgroundTasksChanged(tasks []bgTask) error {
	if tasks == nil {
		tasks = []bgTask{}
	}
	return e.write(backgroundTasksChangedWire{
		Type:      "system",
		Subtype:   "background_tasks_changed",
		Tasks:     tasks,
		UUID:      newUUID(),
		SessionID: e.sessionID,
	})
}

type taskStartedWire struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	TaskID         string `json:"task_id"`
	RunID          string `json:"run_id"`
	ToolUseID      string `json:"tool_use_id"`
	Description    string `json:"description"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	TaskType       string `json:"task_type"`
	UUID           string `json:"uuid"`
	SessionID      string `json:"session_id"`
}

// taskStarted 写出后台任务开始消息，is_backgrounded 为真。
func (e *emitter) taskStarted(task bgTask, toolUseID string) error {
	return e.write(taskStartedWire{
		Type:           "system",
		Subtype:        "task_started",
		TaskID:         task.TaskID,
		RunID:          task.RunID,
		ToolUseID:      toolUseID,
		Description:    task.Description,
		IsBackgrounded: true,
		TaskType:       task.TaskType,
		UUID:           newUUID(),
		SessionID:      e.sessionID,
	})
}

type taskPatch struct {
	Status  string `json:"status"`
	EndTime int64  `json:"end_time"`
}

type taskUpdatedWire struct {
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype"`
	TaskID    string    `json:"task_id"`
	RunID     string    `json:"run_id"`
	Patch     taskPatch `json:"patch"`
	UUID      string    `json:"uuid"`
	SessionID string    `json:"session_id"`
}

// taskUpdated 写出后台任务状态变化。
func (e *emitter) taskUpdated(taskID, runID, status string, endTime int64) error {
	return e.write(taskUpdatedWire{
		Type:      "system",
		Subtype:   "task_updated",
		TaskID:    taskID,
		RunID:     runID,
		Patch:     taskPatch{Status: status, EndTime: endTime},
		UUID:      newUUID(),
		SessionID: e.sessionID,
	})
}

type taskNotificationWire struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id"`
	ToolUseID  string `json:"tool_use_id"`
	Status     string `json:"status"`
	OutputFile string `json:"output_file"`
	Summary    string `json:"summary"`
	UUID       string `json:"uuid"`
	SessionID  string `json:"session_id"`
}

// taskNotification 写出后台任务结束消息。
func (e *emitter) taskNotification(task bgTask, toolUseID, status, outputFile, summary string) error {
	return e.write(taskNotificationWire{
		Type:       "system",
		Subtype:    "task_notification",
		TaskID:     task.TaskID,
		RunID:      task.RunID,
		ToolUseID:  toolUseID,
		Status:     status,
		OutputFile: outputFile,
		Summary:    summary,
		UUID:       newUUID(),
		SessionID:  e.sessionID,
	})
}

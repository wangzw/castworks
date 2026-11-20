package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"

	"github.com/wangzw/castworks/pkg/warden/agent"
)

// streamTask 是 background_tasks_changed 推送中的一个后台任务。
type streamTask struct {
	TaskID  string `json:"task_id"`
	Ambient bool   `json:"ambient"`
}

// streamMessage 是驱动器从 stdout 关心的字段。其余字段不影响判定。
type streamMessage struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	SessionID       string `json:"session_id"`
	State           string `json:"state"`
	Result          string `json:"result"`
	IsError         bool   `json:"is_error"`
	QueuedTurnCount *int   `json:"queued_turn_count"`

	// 后台任务消息的字段。background_tasks_changed 用 Tasks；
	// task_started 用 TaskID、IsBackgrounded 与 Ambient；
	// task_notification 用 TaskID 与 Status。
	Tasks          []streamTask `json:"tasks"`
	TaskID         string       `json:"task_id"`
	IsBackgrounded bool         `json:"is_backgrounded"`
	Status         string       `json:"status"`
	Ambient        bool         `json:"ambient"`
}

// readStdout 逐行读取 Claude Code 的 stdout：每行先原样追加到 stream.jsonl，
// 再解析并处理。读取在子进程关闭 stdout 时结束。
func (d *Driver) readStdout(r *os.File) {
	defer func() {
		_ = r.Close()
		close(d.stdoutDone)
	}()
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			d.handleStdoutLine(line)
		}
		if err != nil {
			return
		}
	}
}

// handleStdoutLine 追加一行到 stream.jsonl，解析后交给状态机。无法解析的行
// 与未知类型的消息记日志后忽略。
func (d *Driver) handleStdoutLine(line []byte) {
	d.appendStream(line)

	trimmed := bytes.TrimSuffix(line, []byte("\n"))
	trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
	if len(bytes.TrimSpace(trimmed)) == 0 {
		d.log.Debug("claudecode: ignored blank stdout line")
		return
	}

	var msg streamMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		d.log.Warn("claudecode: ignored non-JSON stdout line", "bytes", len(trimmed))
		return
	}

	d.mu.Lock()
	d.handleMessageLocked(&msg)
	d.mu.Unlock()
}

// appendStream 把一行原样写入 stream.jsonl。
func (d *Driver) appendStream(line []byte) {
	d.streamMu.Lock()
	defer d.streamMu.Unlock()
	if d.streamFile == nil {
		return
	}
	if _, err := d.streamFile.Write(line); err != nil {
		d.log.Warn("claudecode: write stream.jsonl", "err", err)
	}
}

// handleMessageLocked 把一条 stream-json 消息翻译为状态变化与 Sink 回调。
// 调用方必须持有 d.mu。
func (d *Driver) handleMessageLocked(msg *streamMessage) {
	if d.shuttingDown {
		// 开始关停之后不再调用 Sink。
		return
	}
	if msg.SessionID != "" {
		d.noteSessionIDLocked(msg.SessionID)
	}

	switch msg.Type {
	case "system":
		switch msg.Subtype {
		case "session_state_changed":
			d.handleSessionStateLocked(msg.State)
		case "background_tasks_changed":
			d.replaceBgJobsFromPushLocked(msg.Tasks)
		case "task_started":
			d.noteTaskStartedLocked(msg)
		case "task_notification":
			d.noteTaskNotificationLocked(msg)
		}
		// 其余 system 消息（init、task_updated 等）不改变回合位置与集合。
	case "assistant":
		// 回合边界上读到 assistant 消息，表示 Claude Code 自行开始了回合。
		if d.phase == agent.PhaseBoundary {
			d.startSpontaneousTurnLocked()
		}
	case "result":
		d.handleResultLocked(msg)
	case "user":
		// 工具结果等用户消息不改变回合位置。
	case "rate_limit_event":
		// 限流状态是信息类消息，不构成回合边界，也不改变会话阶段。
		d.log.Debug("claudecode: rate limit event")
	default:
		d.log.Warn("claudecode: ignored stdout message", "type", msg.Type)
	}
}

// noteSessionIDLocked 缓存首个顶层 session_id，并补发等待它的 turn_started。
// 调用方必须持有 d.mu。
func (d *Driver) noteSessionIDLocked(sessionID string) {
	if d.sessionID != "" {
		return
	}
	d.sessionID = sessionID
	if d.pendingStart == nil {
		return
	}
	pending := d.pendingStart
	d.pendingStart = nil
	d.sink.TurnStarted(pending.turnID, pending.deliveryID, d.sessionID)
}

// handleSessionStateLocked 处理会话状态变化。调用方必须持有 d.mu。
func (d *Driver) handleSessionStateLocked(state string) {
	switch state {
	case "running":
		// 回合边界上转为 running，表示 Claude Code 自行开始了回合。
		if d.phase == agent.PhaseBoundary {
			d.startSpontaneousTurnLocked()
			return
		}
		// 回合进行中转为 running，已收到的边界信号作废。
		if d.phase == agent.PhaseInTurn {
			d.clearBoundarySignalsLocked()
		}
	case "idle":
		if d.phase != agent.PhaseInTurn {
			return
		}
		d.idleSeen = true
		d.maybeEndTurnLocked()
	case "requires_action":
		// 权限询问等阻塞状态，不构成回合边界。
	}
}

// startSpontaneousTurnLocked 在回合边界上开始一个自发回合。调用方必须持有
// d.mu。
func (d *Driver) startSpontaneousTurnLocked() {
	d.beginTurnLocked()
	d.announceTurnStartLocked("")
}

// handleResultLocked 记录 result 消息的边界信号。调用方必须持有 d.mu。
func (d *Driver) handleResultLocked(msg *streamMessage) {
	if d.phase != agent.PhaseInTurn {
		return
	}
	queued := 0
	if msg.QueuedTurnCount != nil {
		queued = *msg.QueuedTurnCount
	}
	if queued > 0 {
		// 还有排队的回合，已到的 idle 不再属于本回合的边界。
		d.idleSeen = false
	}
	d.resultSeen = true
	d.resultOutput = msg.Result
	d.resultIsError = msg.IsError
	d.resultQueued = queued
	d.maybeEndTurnLocked()
}

// replaceBgJobsFromPushLocked 以 background_tasks_changed 推送的完整集合替换
// 当前集合；ambient 为真的任务与缺少任务标识的条目不计入。调用方必须持有
// d.mu。
func (d *Driver) replaceBgJobsFromPushLocked(tasks []streamTask) {
	next := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if task.TaskID == "" || task.Ambient {
			continue
		}
		next[task.TaskID] = struct{}{}
	}
	d.replaceBgJobsLocked(next)
}

// noteTaskStartedLocked 在两次集合推送之间加入后台任务：只有 is_backgrounded
// 为真的任务计入，ambient 为真时不计。调用方必须持有 d.mu。
func (d *Driver) noteTaskStartedLocked(msg *streamMessage) {
	if !msg.IsBackgrounded || msg.Ambient {
		return
	}
	d.addBgJobLocked(msg.TaskID)
}

// noteTaskNotificationLocked 在两次集合推送之间移除报告终态的任务。调用方
// 必须持有 d.mu。
func (d *Driver) noteTaskNotificationLocked(msg *streamMessage) {
	if !isTerminalTaskStatus(msg.Status) {
		return
	}
	d.removeBgJobLocked(msg.TaskID)
}

// isTerminalTaskStatus 判断 task_notification 的报告是否表示任务已结束。
// Claude Code 的 task_notification.status 是闭集 completed、failed、stopped。
func isTerminalTaskStatus(status string) bool {
	switch status {
	case "completed", "failed", "stopped":
		return true
	default:
		return false
	}
}

// maybeEndTurnLocked 在 idle 与 queued_turn_count=0 的 result 都到齐时结束
// 回合。调用方必须持有 d.mu。
func (d *Driver) maybeEndTurnLocked() {
	if d.writeInFlight {
		// 写 stdin 期间先把边界信号留住，等 Deliver 发出 TurnStarted 后再
		// 判定，避免结束一个还没有 TurnStarted 的回合。
		return
	}
	if d.phase != agent.PhaseInTurn || !d.idleSeen || !d.resultSeen || d.resultQueued > 0 {
		return
	}
	d.flushPendingTurnStartLocked()

	output := truncateOutput(d.resultOutput)
	turnID := d.turnID
	d.lastTurn = &agent.TurnResult{
		TurnID:  turnID,
		Output:  output,
		IsError: d.resultIsError,
	}
	d.phase = agent.PhaseBoundary
	d.turnID = ""
	d.pendingStart = nil
	d.idleSeen = false
	d.resultSeen = false
	d.resultOutput = ""
	d.resultIsError = false
	d.resultQueued = 0

	d.sink.TurnEnded(turnID, output, d.lastTurn.IsError, false)
}

// userMessageText 返回编码后的用户消息中承载的文本，供测试核对。
func userMessageText(line []byte) (string, error) {
	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &payload); err != nil {
		return "", err
	}
	return payload.Message.Content, nil
}

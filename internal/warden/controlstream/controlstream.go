// Package controlstream 维持看守进程到控制面的出站流。
//
// 一个 Client 对应一个运行实例的一条出站连接。它实现 agent.Sink，供
// Claude Code 驱动器上报事件；Run 负责连接、对账与重连：
//
//	c := controlstream.New(controlstream.Config{Address: addr, RunID: runID})
//	driver := claudecode.New(..., c.Sink())
//	if err := c.Run(ctx, driver); err != nil { ... }
//
// 每条流的首帧是 Hello，在一个 Agent.Snapshot 的回调里构造并切换当前流，
// 因此驱动的每次状态变化要么体现在 Hello 里，要么作为事件跟在 Hello 之后
// 发出。Sink 回调把帧排入当前流的无界发送队列；没有当前流时直接丢弃，由
// 下一次 Hello 完成对账。
//
// Run 在控制面结束运行实例时返回 nil（收到 Shutdown，或流以
// FailedPrecondition 关闭），在 ctx 取消时返回 ctx.Err()。拨号失败、流以
// 其他状态关闭、收到不合法的控制帧或 run_id 不一致的控制帧时，关闭当前流，
// 按退避序列重连。
package controlstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
	"github.com/wangzw/castworks/internal/agentprotocol"
	"github.com/wangzw/castworks/internal/warden/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

const (
	// keepaliveTime 与 keepaliveTimeout 是客户端 keepalive 的取值，见
	// agentprotocol.md“断线检测与重连”。
	keepaliveTime    = 10 * time.Second
	keepaliveTimeout = 5 * time.Second
)

// DefaultBackoff 是重连退避序列：1 s、2 s、4 s，之后保持 5 s。收到
// HelloAck 后从第一个值重新开始。
var DefaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}

// Config 是 Client 的配置。
type Config struct {
	// Address 是控制面地址，形如 host:port。它原样用作 gRPC 的拨号目标，
	// 因此也是连接的 authority。
	Address string
	// RunID 是运行实例标识，写在每一帧的 run_id 上。
	RunID string
	// Backoff 是重连退避序列，最后一个值在之后的每次重连中重复。收到
	// HelloAck 后从第一个值重新开始。为 nil 或空时使用 DefaultBackoff。
	Backoff []time.Duration
}

// Client 是看守进程一侧的出站流客户端。它实现 agent.Sink；连接由 Run 独占。
type Client struct {
	address     string
	runID       string
	backoff     []time.Duration
	dialOptions []grpc.DialOption
	// sleep 是测试可替换的等待函数；生产实现等待 d 或被 ctx 取消。
	sleep func(context.Context, time.Duration) error

	mu   sync.Mutex
	cur  *stream
	cred *agent.Credential
}

// New 返回一个 Client。Address 与 RunID 由调用方保证非空；地址无法解析时
// 每次连接尝试都会失败，Run 按退避一直重试。
func New(cfg Config) *Client {
	backoff := cfg.Backoff
	if len(backoff) == 0 {
		backoff = DefaultBackoff
	}
	backoff = append([]time.Duration(nil), backoff...)
	return &Client{
		address: cfg.Address,
		runID:   cfg.RunID,
		backoff: backoff,
		dialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                keepaliveTime,
				Timeout:             keepaliveTimeout,
				PermitWithoutStream: true,
			}),
		},
		sleep: sleepContext,
	}
}

// Sink 返回驱动器上报事件所用的 agent.Sink。Sink 的方法把帧排入当前流的
// 发送队列；没有当前流时丢弃，由下一次 Hello 对账。
func (c *Client) Sink() agent.Sink { return c }

// Accepted 实现 agent.Sink。
func (c *Client) Accepted(deliveryID string, throughSeq int64) {
	c.enqueue(&agentprotocolv1.WardenFrame{
		RunId: c.runID,
		Body: &agentprotocolv1.WardenFrame_Accepted{Accepted: &agentprotocolv1.Accepted{
			DeliveryId: deliveryID,
			ThroughSeq: throughSeq,
		}},
	})
}

// TurnStarted 实现 agent.Sink。
func (c *Client) TurnStarted(turnID, deliveryID, sessionID string) {
	c.enqueue(&agentprotocolv1.WardenFrame{
		RunId: c.runID,
		Body: &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{
			TurnId: turnID,
			Kind: &agentprotocolv1.AgentEvent_TurnStarted{TurnStarted: &agentprotocolv1.TurnStarted{
				DeliveryId: deliveryID,
				SessionId:  sessionID,
			}},
		}},
	})
}

// TurnEnded 实现 agent.Sink。
func (c *Client) TurnEnded(turnID, output string, isError, agentExited bool) {
	c.enqueue(&agentprotocolv1.WardenFrame{
		RunId: c.runID,
		Body: &agentprotocolv1.WardenFrame_Event{Event: &agentprotocolv1.AgentEvent{
			TurnId: turnID,
			Kind: &agentprotocolv1.AgentEvent_TurnEnded{TurnEnded: &agentprotocolv1.TurnEnded{
				Output:      output,
				IsError:     isError,
				AgentExited: agentExited,
			}},
		}},
	})
}

// enqueue 把帧排入当前流。持有 c.mu 入队，使 Hello 的切换与事件的入队互斥：
// Hello 一定先于切换之后产生的任何事件。
func (c *Client) enqueue(f *agentprotocolv1.WardenFrame) {
	c.mu.Lock()
	if c.cur != nil {
		c.cur.queue.push(f)
	}
	c.mu.Unlock()
}

// Run 维持到控制面的流，直到运行实例结束或 ctx 取消。控制面发送 Shutdown
// 或以 FailedPrecondition 关闭流时返回 nil；ctx 取消时返回 ctx.Err()。其余
// 失败都关闭当前流、退避后重连，不设次数上限。
func (c *Client) Run(ctx context.Context, a agent.Agent) error {
	index := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, acked := c.runOnce(ctx, a)
		if err := ctx.Err(); err != nil {
			return err
		}
		if done {
			return nil
		}
		if acked {
			index = 0
		}
		delay := c.backoffAt(index)
		if index < len(c.backoff)-1 {
			index++
		}
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// runOnce 建立一条流并把它跑到结束。done 表示运行实例已结束，acked 表示
// 这条流收到过 HelloAck（退避需要复位）。
func (c *Client) runOnce(ctx context.Context, a agent.Agent) (done, acked bool) {
	conn, err := grpc.NewClient(c.address, c.dialOptions...)
	if err != nil {
		slog.Warn("control plane dial failed; reconnecting",
			"run_id", c.runID, "address", c.address, "error", err)
		return false, false
	}
	defer func() { _ = conn.Close() }()

	sctx, cancel := context.WithCancel(ctx)
	cs, err := agentprotocolv1.NewAgentStreamClient(conn).Connect(sctx)
	if err != nil {
		cancel()
		slog.Warn("opening control stream failed; reconnecting",
			"run_id", c.runID, "error", err)
		return false, false
	}

	s := newStream(cancel, cs)
	defer func() {
		c.mu.Lock()
		if c.cur == s {
			c.cur = nil
		}
		c.mu.Unlock()
		s.stop()
	}()

	// Hello 与当前流的切换在驱动器的状态锁内完成：fn 返回前不会出现 Sink
	// 回调，之后的回调排在 Hello 之后。
	a.Snapshot(func(st agent.State) {
		hello := c.helloFrame(st)
		c.mu.Lock()
		c.cur = s
		s.queue.push(hello)
		c.mu.Unlock()
	})

	frame, err := cs.Recv()
	if err != nil {
		return c.handleStreamError(ctx, s, err), false
	}
	if reason := c.checkFrame(frame, true); reason != "" {
		slog.Warn("control frame rejected before hello_ack; reconnecting",
			"run_id", c.runID, "reason", reason)
		return false, false
	}
	c.setCredential(frame.GetHelloAck().GetLlmCredential())
	acked = true

	for {
		frame, err := cs.Recv()
		if err != nil {
			return c.handleStreamError(ctx, s, err), acked
		}
		if reason := c.checkFrame(frame, false); reason != "" {
			slog.Warn("control frame rejected; reconnecting",
				"run_id", c.runID, "reason", reason)
			return false, acked
		}
		switch body := frame.GetBody().(type) {
		case *agentprotocolv1.ControlFrame_Shutdown:
			slog.Info("control plane ended the run",
				"run_id", c.runID, "reason", body.Shutdown.GetReason())
			return true, acked
		case *agentprotocolv1.ControlFrame_Deliver:
			c.handleDeliver(a, body.Deliver)
		case *agentprotocolv1.ControlFrame_HelloAck:
			slog.Warn("unexpected hello_ack after the stream opened; reconnecting",
				"run_id", c.runID)
			return false, acked
		}
	}
}

// checkFrame 校验一帧控制消息；first 为真时还要求它是 HelloAck。返回空串
// 表示通过。
func (c *Client) checkFrame(frame *agentprotocolv1.ControlFrame, first bool) string {
	if err := agentprotocol.ValidateControlFrame(frame); err != nil {
		return err.Error()
	}
	if got := frame.GetRunId(); got != c.runID {
		return fmt.Sprintf("run_id %q does not match %q", got, c.runID)
	}
	if first {
		if frame.GetHelloAck() == nil {
			return "first frame is not hello_ack"
		}
	}
	return ""
}

// handleStreamError 按关闭状态码决定是否结束 Run。recvErr 之外还可能有发送
// 侧先记下的错误，后者优先，因为取消流之后 Recv 只会报 Canceled。
func (c *Client) handleStreamError(ctx context.Context, s *stream, recvErr error) (done bool) {
	if ctx.Err() != nil {
		return false
	}
	err := recvErr
	if stored := s.terminalError(); stored != nil {
		err = stored
	}
	switch code := status.Code(err); code {
	case codes.FailedPrecondition:
		slog.Info("control stream closed with FailedPrecondition; stopping",
			"run_id", c.runID, "error", err)
		return true
	case codes.InvalidArgument:
		// InvalidArgument 说明看守进程有缺陷；记错误日志后照常重连。
		slog.Error("control stream closed with InvalidArgument; reconnecting",
			"run_id", c.runID, "error", err)
		return false
	default:
		slog.Warn("control stream closed; reconnecting",
			"run_id", c.runID, "code", code.String(), "error", err)
		return false
	}
}

// helloFrame 按状态快照构造 Hello。phase=boundary 时携带最近一个回合。
func (c *Client) helloFrame(st agent.State) *agentprotocolv1.WardenFrame {
	h := &agentprotocolv1.Hello{
		AgentState: &agentprotocolv1.AgentState{
			Phase:  toProtoPhase(st.Phase),
			TurnId: st.TurnID,
		},
		LastAcceptedSeq: st.LastAcceptedSeq,
	}
	if st.Phase == agent.PhaseBoundary && st.LastTurn != nil {
		h.LastTurn = &agentprotocolv1.LastTurn{
			TurnId:  st.LastTurn.TurnID,
			Output:  st.LastTurn.Output,
			IsError: st.LastTurn.IsError,
		}
	}
	return &agentprotocolv1.WardenFrame{
		RunId: c.runID,
		Body:  &agentprotocolv1.WardenFrame_Hello{Hello: h},
	}
}

// handleDeliver 过滤掉已写入 stdin 的消息。没有新消息时直接回 Accepted；
// 否则交给驱动器写入，Accepted 由驱动器经 Sink 回调发出。驱动器的 Deliver
// 失败（含 ErrExited）时不补发 Accepted。
func (c *Client) handleDeliver(a agent.Agent, d *agentprotocolv1.Deliver) {
	var batch agent.Batch
	a.Snapshot(func(st agent.State) {
		batch.DeliveryID = d.GetDeliveryId()
		batch.ThroughSeq = d.GetThroughSeq()
		for _, m := range d.GetBatch() {
			if m.GetInboxSeq() > st.LastAcceptedSeq {
				batch.Texts = append(batch.Texts, m.GetText())
			}
		}
	})

	if len(batch.Texts) == 0 {
		c.enqueue(&agentprotocolv1.WardenFrame{
			RunId: c.runID,
			Body: &agentprotocolv1.WardenFrame_Accepted{Accepted: &agentprotocolv1.Accepted{
				DeliveryId: d.GetDeliveryId(),
				ThroughSeq: d.GetThroughSeq(),
			}},
		})
		return
	}

	cred := c.credential()
	if err := a.Deliver(batch, cred); err != nil {
		if errors.Is(err, agent.ErrExited) {
			// ErrExited 是正常的收尾路径：驱动器已按回合外退出补发事件。
			slog.Info("delivery skipped because Claude Code has exited",
				"run_id", c.runID, "delivery_id", batch.DeliveryID)
			return
		}
		// 只记投递标识，不记消息文本。
		slog.Error("delivery failed",
			"run_id", c.runID, "delivery_id", batch.DeliveryID, "error", err)
	}
}

func (c *Client) setCredential(lc *agentprotocolv1.LLMCredential) {
	cred := &agent.Credential{Value: lc.GetValue()}
	switch lc.GetKind() {
	case agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_API_KEY:
		cred.Kind = agent.CredentialAPIKey
	case agentprotocolv1.LLMCredentialKind_LLM_CREDENTIAL_KIND_OAUTH_TOKEN:
		cred.Kind = agent.CredentialOAuthToken
	}
	c.mu.Lock()
	c.cred = cred
	c.mu.Unlock()
}

func (c *Client) credential() agent.Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cred == nil {
		return agent.Credential{}
	}
	return *c.cred
}

func (c *Client) backoffAt(i int) time.Duration {
	if i >= len(c.backoff) {
		i = len(c.backoff) - 1
	}
	return c.backoff[i]
}

func toProtoPhase(p agent.Phase) agentprotocolv1.Phase {
	switch p {
	case agent.PhaseInTurn:
		return agentprotocolv1.Phase_PHASE_IN_TURN
	case agent.PhaseBoundary:
		return agentprotocolv1.Phase_PHASE_BOUNDARY
	case agent.PhaseExited:
		return agentprotocolv1.Phase_PHASE_EXITED
	case agent.PhaseStarting:
		return agentprotocolv1.Phase_PHASE_STARTING
	default:
		return agentprotocolv1.Phase_PHASE_STARTING
	}
}

// sleepContext 等待 d；ctx 取消时返回 ctx.Err()。
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var _ agent.Sink = (*Client)(nil)

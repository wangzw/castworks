package controlstream

import (
	"context"
	"errors"
	"io"
	"sync"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"google.golang.org/grpc"
)

// stream 是一条已建立的出站流。发送由独立协程从无界队列取帧，接收在 Run
// 的协程里串行进行。
type stream struct {
	cs     grpc.BidiStreamingClient[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame]
	cancel context.CancelFunc
	queue  *sendQueue
	done   chan struct{}

	mu  sync.Mutex
	err error
}

func newStream(cancel context.CancelFunc, cs grpc.BidiStreamingClient[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame]) *stream {
	s := &stream{
		cs:     cs,
		cancel: cancel,
		queue:  newSendQueue(),
		done:   make(chan struct{}),
	}
	go s.sendLoop()
	return s
}

// sendLoop 按入队顺序发送帧。发送失败时记下错误并取消流，接收侧据此退出。
// io.EOF 例外：它只表示服务端已结束这条流，真正的关闭状态码在接收侧，记下
// 它并取消流会把 FailedPrecondition 盖成 Canceled。
func (s *stream) sendLoop() {
	defer close(s.done)
	for {
		f, ok := s.queue.pop()
		if !ok {
			return
		}
		if err := s.cs.Send(f); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			s.fail(err)
			return
		}
	}
}

// fail 记下第一个错误并取消流。
func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *stream) terminalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// stop 结束这条流：取消上下文，丢弃队列中尚未发出的帧，等发送协程退出。
func (s *stream) stop() {
	s.cancel()
	s.queue.close()
	<-s.done
}

// sendQueue 是每条流的无界发送队列。push 不阻塞，close 之后入队的帧被丢弃。
type sendQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	frames []*agentprotocolv1.WardenFrame
	closed bool
}

func newSendQueue() *sendQueue {
	q := &sendQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *sendQueue) push(f *agentprotocolv1.WardenFrame) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.frames = append(q.frames, f)
	q.cond.Signal()
}

func (q *sendQueue) pop() (*agentprotocolv1.WardenFrame, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.frames) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.frames) == 0 {
		return nil, false
	}
	f := q.frames[0]
	q.frames[0] = nil
	q.frames = q.frames[1:]
	return f, true
}

func (q *sendQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.frames = nil
	q.cond.Broadcast()
	q.mu.Unlock()
}

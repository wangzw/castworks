package runcontroller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/status"

	"github.com/wangzw/castworks/pkg/agentprotocol"
	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
)

// errStreamClosed is returned by enqueue once the stream has been closed.
var errStreamClosed = errors.New("runcontroller: stream is closed")

// connIDSeq hands out process-wide unique conn IDs. A conn ID is only used
// for identity and logging; it is never sent on the wire.
var connIDSeq atomic.Uint64

// conn is one AgentStream from a warden process. Outbound frames are queued
// in order by reserve/enqueue and sent by pump; both may run concurrently with
// the control plane enqueueing more frames.
type conn struct {
	connID uint64
	run    string

	mu          sync.Mutex
	queue       []*frameSlot
	closed      bool
	closeStatus *status.Status
	wake        chan struct{}
	doneCh      chan struct{}

	// lastDeliverThrough is the through_seq of the most recent Deliver released
	// on this stream. Accepted.through_seq must equal it.
	lastDeliverThrough atomic.Int64
}

// frameSlot is a reserved position in a conn's ordered send queue. A rule
// transaction reserves it while holding the run row lock, then releases it
// after the commit (send=true) or cancels it on rollback (send=false). pump
// sends slots in reservation order and waits for the decision, so a frame
// never leaves before its transaction commits.
type frameSlot struct {
	conn    *conn
	frame   *agentprotocolv1.ControlFrame
	through int64 // Deliver.through_seq; 0 for other frames

	ready chan struct{}
	// send is written before ready is closed and read only after <-ready.
	send bool
	once sync.Once
}

// release allows pump to send the frame. It is called after the transaction
// commits. For a Deliver it records the through sequence the warden must echo
// in Accepted.
func (s *frameSlot) release() {
	s.once.Do(func() {
		s.send = true
		if s.through > 0 {
			s.conn.lastDeliverThrough.Store(s.through)
		}
		close(s.ready)
	})
}

// cancel drops the frame. It is called when the transaction rolls back.
func (s *frameSlot) cancel() {
	s.once.Do(func() { close(s.ready) })
}

func newConn(runID string) *conn {
	return &conn{
		connID: connIDSeq.Add(1),
		run:    runID,
		wake:   make(chan struct{}, 1),
		doneCh: make(chan struct{}),
	}
}

// id returns the process-wide unique identity of this stream.
func (c *conn) id() uint64 { return c.connID }

// runID returns the run this stream belongs to.
func (c *conn) runID() string { return c.run }

// done is closed when the stream is closed with closeWith.
func (c *conn) done() <-chan struct{} { return c.doneCh }

// signal wakes pump. The wake channel has room for one token, so a pending
// wake-up already covers any further queue changes.
func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// enqueue validates f, appends it to the ordered send queue and immediately
// releases it. It never waits for pump; the queue is unbounded. enqueue
// returns an error for an invalid frame and for any frame offered after
// closeWith.
func (c *conn) enqueue(f *agentprotocolv1.ControlFrame) error {
	s, err := c.reserve(f, 0)
	if err != nil {
		return err
	}
	s.release()
	return nil
}

// reserve validates f and appends a slot for it to the ordered send queue.
// The slot starts pending: pump stops at it until release or cancel is called.
// reserve never waits for pump. It returns errStreamClosed for a frame offered
// after closeWith.
func (c *conn) reserve(f *agentprotocolv1.ControlFrame, through int64) (*frameSlot, error) {
	if err := agentprotocol.ValidateControlFrame(f); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errStreamClosed
	}
	s := &frameSlot{conn: c, frame: f, through: through, ready: make(chan struct{})}
	c.queue = append(c.queue, s)
	c.signal()
	return s, nil
}

// closeWith closes the stream with st. It is idempotent and the first call
// wins. Frames already queued are still sent by pump before it returns.
func (c *conn) closeWith(st *status.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeStatus = st
	close(c.doneCh)
	c.signal()
}

// pump sends queued frames in reservation order until the stream is closed or
// ctx ends. A pending slot (its transaction has not committed yet) makes pump
// wait; a canceled slot is skipped. Frames released before closeWith are
// delivered first; if send then fails the error is returned and the rest are
// abandoned. When the queue is drained and the stream was closed, pump returns
// the close status' error, so codes.OK returns nil.
func (c *conn) pump(ctx context.Context, send func(*agentprotocolv1.ControlFrame) error) error {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			s := c.queue[0]
			c.queue[0] = nil
			if len(c.queue) == 1 {
				c.queue = nil
			} else {
				c.queue = c.queue[1:]
			}
			c.mu.Unlock()

			// Prefer a decision that is already available: a released frame
			// queued before closeWith must still be sent.
			select {
			case <-s.ready:
			default:
				select {
				case <-s.ready:
				case <-c.doneCh:
					// The stream closed while this position was still
					// unreleased. A closed stream drops un-released positions
					// and exits; the rule that reserved them will cancel or
					// release them afterwards, and the warden reconciles
					// through its next Hello.
					c.mu.Lock()
					st := c.closeStatus
					c.mu.Unlock()
					return closeStatusError(st)
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if !s.send {
				continue
			}
			if err := send(s.frame); err != nil {
				return err
			}
			continue
		}
		closed, st := c.closed, c.closeStatus
		c.mu.Unlock()

		if closed {
			return closeStatusError(st)
		}

		select {
		case <-c.wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// closeStatusError converts a close status into pump's return value.
func closeStatusError(st *status.Status) error {
	if st == nil {
		return nil
	}
	return st.Err()
}

// streamRegistry holds at most one current conn per run.
type streamRegistry struct {
	mu    sync.Mutex
	conns map[string]*conn
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{conns: make(map[string]*conn)}
}

// replace registers c as the current stream of runID and returns the stream it
// replaced, if any. The replaced conn is not closed here; the caller decides
// how to close it.
func (r *streamRegistry) replace(runID string, c *conn) (old *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old = r.conns[runID]
	r.conns[runID] = c
	return old
}

// current returns the current stream of runID, or nil.
func (r *streamRegistry) current(runID string) *conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[runID]
}

// removeIf unregisters c only if it is still the current stream of runID. It
// returns true when it removed c and false when a newer stream is registered
// (or none is).
func (r *streamRegistry) removeIf(runID string, c *conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[runID] != c {
		return false
	}
	delete(r.conns, runID)
	return true
}

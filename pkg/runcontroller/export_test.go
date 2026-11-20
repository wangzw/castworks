package runcontroller

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
)

// 本文件只把测试需要的内部入口导出给外部测试包 runcontroller_test。
// 它不参与非测试构建。

// SetAfterLockHook installs fn to run after each rule takes the run row lock
// and before it writes. op is the event name; see eventName.
func SetAfterLockHook(c *Controller, fn func(op, runID string)) {
	c.setAfterLock(fn)
}

// SetAfterCommitHook installs fn to run after each rule transaction commits and
// before its reserved frames are released. op is the event name; see
// eventName.
func SetAfterCommitHook(c *Controller, fn func(op, runID string)) {
	c.setAfterCommit(fn)
}

// SetCommitErrorAfterSuccess installs fn to run after a rule transaction
// commits; a non-nil return is reported as a commit error even though the
// transaction committed. Tests use it to reproduce an uncertain commit.
func SetCommitErrorAfterSuccess(c *Controller, fn func() error) {
	c.store.setCommitErrorAfterSuccess(fn)
}

// SetAfterCommitErrorHook installs fn to run after Commit returned an error
// and before the outcome is judged. Tests use it to interleave another
// transaction between the error and the check.
func SetAfterCommitErrorHook(c *Controller, fn func()) {
	c.store.setAfterCommitError(fn)
}

// SetCommitStatusError installs fn to replace the pg_xact_status query. A
// non-nil return makes the outcome check fail, so tests can exhaust its
// retries.
func SetCommitStatusError(c *Controller, fn func() error) {
	c.store.setCommitStatusError(fn)
}

// SetStreamCloseRetryLimit overrides the number of attempts a stream-close
// write makes before giving up. Tests use a small value.
func SetStreamCloseRetryLimit(c *Controller, n int) {
	c.streamCloseAttempts.Store(int32(n))
}

// StreamClosedForTest runs the stream-close event for runID's current stream
// and returns when it finishes. It exercises the bounded retry without a
// running gRPC stream.
func StreamClosedForTest(c *Controller, runID string) {
	conn := c.streams.current(runID)
	if conn == nil {
		return
	}
	c.onStreamClosed(context.Background(), conn)
}

// WaitProviderQueue returns after every provider call reserved for runID
// before the call has finished. Tests use it before asserting a call count so
// a call that appears later cannot be missed.
func WaitProviderQueue(c *Controller, runID string) {
	c.calls.barrier(runID)
}

// TrySuspendNow runs the suspension rule for runID immediately, bypassing the
// scan period.
func TrySuspendNow(c *Controller, runID string) error {
	return c.trySuspend(context.Background(), runID)
}

// ExpireHelloNow runs the T_hello rule for runID immediately, bypassing the
// scan period.
func ExpireHelloNow(c *Controller, runID string) error {
	return c.expireHello(context.Background(), runID)
}

// DeliverFrameForTest builds the Deliver frame for runID's inbox rows in
// [from, through] through the same conversion and validation path that
// delivery uses.
func DeliverFrameForTest(c *Controller, runID string, from, through int64) (*agentprotocolv1.ControlFrame, error) {
	var messages []inboxMessage
	if err := c.store.inTx(context.Background(), func(tx *storeTx) error {
		var err error
		messages, err = tx.inboxRange(runID, from, through, maxDeliverBytes)
		return err
	}); err != nil {
		return nil, err
	}
	return c.deliverFrame(runID, messages)
}

// RegisterClosedStreamForTest registers a stream that is already closed but
// still current for runID. It reproduces the "closed but still registered"
// state End must tolerate when its Shutdown frame cannot be sent.
func RegisterClosedStreamForTest(c *Controller, runID string) {
	conn := newConn(runID)
	c.streams.replace(runID, conn)
	conn.closeWith(status.New(codes.FailedPrecondition, "test: stream already closed"))
}

// StreamForTest is an opaque handle to a stream registered by NewStreamForTest.
type StreamForTest struct{ conn *conn }

// NewStreamForTest registers a new stream for runID the way a successful Hello
// does: it replaces the current stream and clears the disconnect record. The
// returned handle lets a test close the replaced stream afterwards, modelling
// an old stream whose handler returns after the new stream's Hello.
func NewStreamForTest(c *Controller, runID string) *StreamForTest {
	conn := newConn(runID)
	if old := c.streams.replace(runID, conn); old != nil {
		old.closeWith(status.New(codes.Aborted, "test: stream replaced"))
	}
	c.streams.connectedIfCurrent(runID, conn)
	return &StreamForTest{conn: conn}
}

// CloseStaleStreamForTest runs the close handler of a stream that is no longer
// current, as the handler of a replaced stream does when it finally returns.
func CloseStaleStreamForTest(c *Controller, s *StreamForTest) {
	s.conn.closeWith(status.New(codes.Unavailable, "test: stale stream closed"))
	c.onStreamClosed(context.Background(), s.conn)
}

// DisconnectedSinceForTest reports the in-memory disconnect time of runID's
// outbound stream. ok is false while the stream is connected or was never
// seen. Tests use it to wait until a stream close has been recorded.
func DisconnectedSinceForTest(c *Controller, runID string) (since time.Time, ok bool) {
	since, _, ok = c.streams.disconnectState(runID)
	return since, ok
}

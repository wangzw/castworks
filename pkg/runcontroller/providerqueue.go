package runcontroller

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// providerQueue 让提供者调用在事务提交后执行：同一运行实例的调用按预留
// 顺序串行，不同运行实例的调用可以并发。队列自己持有一个只被 shutdown
// 取消的 ctx，任务拿到的 ctx 与发起调用的 RunService 方法或流处理函数的
// ctx 无关。
type providerQueue struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	runs   map[string]*providerRun

	// closedCh 在 shutdown 时关闭，让正在等待前一个位置的 goroutine 退出。
	closedCh chan struct{}
	// wg 计入每个已 start 的槽位对应的 goroutine；任务返回或槽位被丢弃
	// 时减一。shutdown 等它归零。
	wg sync.WaitGroup
}

// providerRun 是同一运行实例按预留顺序排列的槽位。
type providerRun struct {
	slots []*providerSlot
	// live 是尚未结束（done 未关闭）的槽位数；归零后从队列的 map 中移除。
	live int
}

// providerSlot 是队列中的一个位置。前一个位置（prev）的任务返回或被
// abandon 后，本位置的任务才会执行。
type providerSlot struct {
	q     *providerQueue
	runID string
	chain *providerRun
	prev  *providerSlot
	// done 在本位置的任务返回、被 abandon 或被丢弃时关闭。
	done chan struct{}

	// 以下字段由 q.mu 保护。
	resolved  bool // start 或 abandon 已调用
	discarded bool
	job       func(context.Context)
}

func newProviderQueue() *providerQueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &providerQueue{
		ctx:      ctx,
		cancel:   cancel,
		runs:     make(map[string]*providerRun),
		closedCh: make(chan struct{}),
	}
}

// reserve 在持有运行实例行锁时调用，按调用顺序为该运行实例排一个位置。
// 非阻塞。shutdown 之后预留的位置直接丢弃，start 不会执行任务。
func (q *providerQueue) reserve(runID string) *providerSlot {
	q.mu.Lock()
	defer q.mu.Unlock()

	s := &providerSlot{
		q:     q,
		runID: runID,
		done:  make(chan struct{}),
	}
	if q.closed {
		s.discarded = true
		s.resolved = true
		close(s.done)
		return s
	}

	r := q.runs[runID]
	if r == nil {
		r = &providerRun{}
		q.runs[runID] = r
	}
	if n := len(r.slots); n > 0 {
		s.prev = r.slots[n-1]
	}
	s.chain = r
	r.slots = append(r.slots, s)
	r.live++
	return s
}

// start 在事务提交后调用，安排 job 在本位置轮到执行时运行。它立即返回：
// job 在队列自己的 goroutine 中以队列的 ctx 执行。重复调用无效果。
func (s *providerSlot) start(job func(ctx context.Context)) {
	q := s.q
	q.mu.Lock()
	if s.resolved {
		q.mu.Unlock()
		return
	}
	s.resolved = true
	s.job = job

	if s.discarded || q.closed {
		// 队列已关停：丢弃这个位置，放行排在它后面的槽位。
		s.discarded = true
		s.closeDoneLocked()
		q.mu.Unlock()
		return
	}

	q.wg.Add(1)
	q.mu.Unlock()

	go s.run()
}

// abandon 在事务回滚时调用，放行排在它后面的槽位。它不执行任务，但仍要
// 等前一个位置结束，使后继位置不会与前一个位置的任务重叠。已经 start
// 的槽位不受影响，重复调用无效果。
func (s *providerSlot) abandon() {
	q := s.q
	q.mu.Lock()
	if s.resolved {
		q.mu.Unlock()
		return
	}
	s.resolved = true
	s.discarded = true
	// 队列已关停时不再等前一个位置，也不再对 WaitGroup 计数：shutdown
	// 要么已经等完前一个位置，要么正在等它，这个位置自己没有任何任务
	// 需要被等待。
	if s.prev == nil || q.closed {
		s.closeDoneLocked()
		q.mu.Unlock()
		return
	}
	q.wg.Add(1)
	q.mu.Unlock()

	go func() {
		defer q.wg.Done()
		select {
		case <-s.prev.done:
		case <-q.closedCh:
		}
		q.mu.Lock()
		s.closeDoneLocked()
		q.mu.Unlock()
	}()
}

// run 等待前一个位置结束，再执行本位置的任务。队列关停时尚未开始的任务
// 被丢弃。
func (s *providerSlot) run() {
	defer s.q.wg.Done()

	if s.prev != nil {
		select {
		case <-s.prev.done:
		case <-s.q.closedCh:
			s.finish()
			return
		}
	}

	s.q.mu.Lock()
	if s.q.closed {
		s.q.mu.Unlock()
		s.finish()
		return
	}
	job := s.job
	ctx := s.q.ctx
	s.q.mu.Unlock()

	job(ctx)
	s.finish()
}

// finish 关闭本位置的 done，让后继槽位继续。
func (s *providerSlot) finish() {
	s.q.mu.Lock()
	s.closeDoneLocked()
	s.q.mu.Unlock()
}

// closeDoneLocked 关闭槽位的 done 并在整条槽位链结束后移除运行实例条目。
// 调用方必须持有 q.mu。
func (s *providerSlot) closeDoneLocked() {
	close(s.done)
	if s.chain == nil {
		return
	}
	s.chain.live--
	if s.chain.live > 0 {
		return
	}
	if s.q.runs[s.runID] == s.chain {
		delete(s.q.runs, s.runID)
	}
}

// barrier 为 runID 预留一个位置并等它轮到执行；排在它前面的槽位都已结束。
// 它只在测试里经 export_test.go 使用，用来把“计数”放在队列排空之后。
func (q *providerQueue) barrier(runID string) {
	slot := q.reserve(runID)
	done := make(chan struct{})
	slot.start(func(context.Context) { close(done) })
	select {
	case <-done:
	case <-q.closedCh:
	}
}

// shutdown 取消传给全部任务的 ctx，丢弃此后预留的位置，等已开始的任务
// （含正在等待前一个位置的 goroutine）返回后才返回。重复调用安全。
func (q *providerQueue) shutdown() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.cancel()
		close(q.closedCh)
	}
	q.mu.Unlock()

	q.wg.Wait()
}

// providerOp 是提供者调用日志中 op 属性的取值。
type providerOp string

const (
	opProvision  providerOp = "provision"
	opSnapshot   providerOp = "snapshot"
	opRestore    providerOp = "restore"
	opDestroy    providerOp = "destroy"
	opGetSandbox providerOp = "get_sandbox"
)

// providerCallJob 返回一个供 providerSlot.start 执行的 job。它执行一次
// 提供者调用，并在调用返回时写一行“provider call”日志，格式照接口清单
// “提供者调用日志”一条。fn 的返回值由调用方在闭包里取出。
func providerCallJob(op providerOp, runID string, fn func(ctx context.Context) error) func(context.Context) {
	return func(ctx context.Context) {
		started := time.Now()
		err := fn(ctx)
		logProviderCall(op, runID, started, err)
	}
}

// logProviderCall 按接口清单“提供者调用日志”的格式写一行日志：消息为
// provider call，属性为 op、run_id、duration_ms（整数）与 error（成功时
// 为空串）。
func logProviderCall(op providerOp, runID string, started time.Time, err error) {
	errorText := ""
	if err != nil {
		errorText = err.Error()
	}
	slog.Default().Info("provider call",
		"op", string(op),
		"run_id", runID,
		"duration_ms", time.Since(started).Milliseconds(),
		"error", errorText,
	)
}

package humanchannel

import (
	"context"
	"sync"

	"github.com/wangzw/castworks/internal/runcontroller"
)

// FakeCreateCall 记录一次 Create 调用。
type FakeCreateCall struct {
	Ctx    context.Context
	Prompt string
}

// FakeGetCall 记录一次 Get 调用。
type FakeGetCall struct {
	Ctx   context.Context
	RunID string
}

// FakeListCall 记录一次 List 调用。
type FakeListCall struct {
	Ctx context.Context
}

// FakeEndCall 记录一次 End 调用。
type FakeEndCall struct {
	Ctx   context.Context
	RunID string
}

// FakeAppendCall 记录一次 AppendInbox 调用。
type FakeAppendCall struct {
	Ctx   context.Context
	RunID string
	Text  string
}

// FakeRunService 是测试用的 runcontroller.RunService 替身。它记录每次调用与
// 收到的 ctx；各方法的返回值与错误由对应的 Func 字段配置，Func 为 nil 时返回
// 零值与 nil。它可被并发调用。
type FakeRunService struct {
	CreateFunc      func(ctx context.Context, prompt string) (string, error)
	GetFunc         func(ctx context.Context, runID string) (runcontroller.RunDetail, error)
	ListFunc        func(ctx context.Context) ([]runcontroller.RunSummary, error)
	EndFunc         func(ctx context.Context, runID string) error
	AppendInboxFunc func(ctx context.Context, runID string, text string) (int64, error)

	mu      sync.Mutex
	creates []FakeCreateCall
	gets    []FakeGetCall
	lists   []FakeListCall
	ends    []FakeEndCall
	appends []FakeAppendCall
}

var _ runcontroller.RunService = (*FakeRunService)(nil)

func (f *FakeRunService) Create(ctx context.Context, prompt string) (string, error) {
	f.mu.Lock()
	f.creates = append(f.creates, FakeCreateCall{Ctx: ctx, Prompt: prompt})
	fn := f.CreateFunc
	f.mu.Unlock()
	if fn == nil {
		return "", nil
	}
	return fn(ctx, prompt)
}

func (f *FakeRunService) Get(ctx context.Context, runID string) (runcontroller.RunDetail, error) {
	f.mu.Lock()
	f.gets = append(f.gets, FakeGetCall{Ctx: ctx, RunID: runID})
	fn := f.GetFunc
	f.mu.Unlock()
	if fn == nil {
		return runcontroller.RunDetail{}, nil
	}
	return fn(ctx, runID)
}

func (f *FakeRunService) List(ctx context.Context) ([]runcontroller.RunSummary, error) {
	f.mu.Lock()
	f.lists = append(f.lists, FakeListCall{Ctx: ctx})
	fn := f.ListFunc
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx)
}

func (f *FakeRunService) End(ctx context.Context, runID string) error {
	f.mu.Lock()
	f.ends = append(f.ends, FakeEndCall{Ctx: ctx, RunID: runID})
	fn := f.EndFunc
	f.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, runID)
}

func (f *FakeRunService) AppendInbox(ctx context.Context, runID string, text string) (int64, error) {
	f.mu.Lock()
	f.appends = append(f.appends, FakeAppendCall{Ctx: ctx, RunID: runID, Text: text})
	fn := f.AppendInboxFunc
	f.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, runID, text)
}

// CreateCalls 返回已记录的 Create 调用的副本。
func (f *FakeRunService) CreateCalls() []FakeCreateCall { return f.snapshotCreates() }

// GetCalls 返回已记录的 Get 调用的副本。
func (f *FakeRunService) GetCalls() []FakeGetCall { return f.snapshotGets() }

// ListCalls 返回已记录的 List 调用的副本。
func (f *FakeRunService) ListCalls() []FakeListCall { return f.snapshotLists() }

// EndCalls 返回已记录的 End 调用的副本。
func (f *FakeRunService) EndCalls() []FakeEndCall { return f.snapshotEnds() }

// AppendCalls 返回已记录的 AppendInbox 调用的副本。
func (f *FakeRunService) AppendCalls() []FakeAppendCall { return f.snapshotAppends() }

func (f *FakeRunService) snapshotCreates() []FakeCreateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCreateCall(nil), f.creates...)
}

func (f *FakeRunService) snapshotGets() []FakeGetCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeGetCall(nil), f.gets...)
}

func (f *FakeRunService) snapshotLists() []FakeListCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeListCall(nil), f.lists...)
}

func (f *FakeRunService) snapshotEnds() []FakeEndCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeEndCall(nil), f.ends...)
}

func (f *FakeRunService) snapshotAppends() []FakeAppendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeAppendCall(nil), f.appends...)
}

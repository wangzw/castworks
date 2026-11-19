package localprovider

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wangzw/castworks/internal/sandboxprovider"
)

// 编译期断言：New 的返回值满足 sandboxprovider.Provider。
var _ sandboxprovider.Provider = New()

func TestProvision(t *testing.T) {
	p := New()

	timing, err := p.Provision(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Provision returned error: %v", err)
	}
	if timing != (sandboxprovider.Timing{}) {
		t.Fatalf("Provision returned timing %+v, want zero value", timing)
	}
}

func TestSnapshot(t *testing.T) {
	p := New()

	timing, err := p.Snapshot(context.Background(), "cw-run-1")
	if !errors.Is(err, sandboxprovider.ErrUnsupported) {
		t.Fatalf("Snapshot error = %v, want errors.Is ErrUnsupported", err)
	}
	if timing != (sandboxprovider.Timing{}) {
		t.Fatalf("Snapshot returned timing %+v, want zero value", timing)
	}
}

func TestRestoreSnapshot(t *testing.T) {
	p := New()

	timing, err := p.RestoreSnapshot(context.Background(), "cw-run-1")
	if !errors.Is(err, sandboxprovider.ErrUnsupported) {
		t.Fatalf("RestoreSnapshot error = %v, want errors.Is ErrUnsupported", err)
	}
	if timing != (sandboxprovider.Timing{}) {
		t.Fatalf("RestoreSnapshot returned timing %+v, want zero value", timing)
	}
}

func TestDestroy(t *testing.T) {
	p := New()

	if err := p.Destroy(context.Background(), "cw-run-1"); err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
}

func TestGetSandbox(t *testing.T) {
	p := New()

	for _, actor := range []string{"cw-run-1", "cw-0b9f6e2a-4f1c-4d5e-8a7b-3c2d1e0f9a8b", "", "anything"} {
		status, err := p.GetSandbox(context.Background(), actor)
		if err != nil {
			t.Fatalf("GetSandbox(%q) returned error: %v", actor, err)
		}
		if status.State != sandboxprovider.ActorStateRunning {
			t.Fatalf("GetSandbox(%q) state = %q, want %q", actor, status.State, sandboxprovider.ActorStateRunning)
		}
	}
}

// TestConcurrent 用固定的小数目 goroutine 并发调用各方法，配合 -race 验证无数据竞争。
func TestConcurrent(t *testing.T) {
	const goroutines = 8

	p := New()
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := p.Provision(ctx, "run-1"); err != nil {
				t.Errorf("Provision returned error: %v", err)
			}
			if _, err := p.Snapshot(ctx, "cw-run-1"); !errors.Is(err, sandboxprovider.ErrUnsupported) {
				t.Errorf("Snapshot error = %v, want ErrUnsupported", err)
			}
			if _, err := p.RestoreSnapshot(ctx, "cw-run-1"); !errors.Is(err, sandboxprovider.ErrUnsupported) {
				t.Errorf("RestoreSnapshot error = %v, want ErrUnsupported", err)
			}
			if err := p.Destroy(ctx, "cw-run-1"); err != nil {
				t.Errorf("Destroy returned error: %v", err)
			}
			if status, err := p.GetSandbox(ctx, "cw-run-1"); err != nil {
				t.Errorf("GetSandbox returned error: %v", err)
			} else if status.State != sandboxprovider.ActorStateRunning {
				t.Errorf("GetSandbox state = %q, want %q", status.State, sandboxprovider.ActorStateRunning)
			}
		}()
	}
	wg.Wait()
}

package substrate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

const (
	actorTestAtespace     = "castworks-dev"
	actorTestTemplate     = "cw-0123456789ab"
	actorTestControlPlane = "castworks-dev.castworks-system.svc.cluster.local:9090"
)

// fakeControlClient embeds the ControlClient interface and overrides only the
// methods the actor lifecycle uses. Unoverridden methods keep the nil
// interface and panic, which is what a test wants when it reaches a call it
// did not expect. Every call is recorded in order.
type fakeControlClient struct {
	ateapi.ControlClient

	mu       sync.Mutex
	calls    []string
	requests []any

	createActorFn        func(context.Context, *ateapi.CreateActorRequest) (*ateapi.Actor, error)
	createEgressPolicyFn func(context.Context, *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
	getEgressPolicyFn    func(context.Context, *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
	resumeActorFn        func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error)
	suspendActorFn       func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error)
	deleteActorFn        func(context.Context, *ateapi.DeleteActorRequest) (*ateapi.Actor, error)
	getActorFn           func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error)
}

func (f *fakeControlClient) record(name string, req any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	f.requests = append(f.requests, req)
}

func (f *fakeControlClient) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeControlClient) requestAt(i int) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeControlClient) countCall(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (f *fakeControlClient) CreateActor(ctx context.Context, in *ateapi.CreateActorRequest, _ ...grpc.CallOption) (*ateapi.Actor, error) {
	f.record("CreateActor", in)
	if f.createActorFn != nil {
		return f.createActorFn(ctx, in)
	}
	return &ateapi.Actor{}, nil
}

func (f *fakeControlClient) CreateActorEgressPolicy(ctx context.Context, in *ateapi.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapi.EgressPolicy, error) {
	f.record("CreateActorEgressPolicy", in)
	if f.createEgressPolicyFn != nil {
		return f.createEgressPolicyFn(ctx, in)
	}
	return &ateapi.EgressPolicy{}, nil
}

func (f *fakeControlClient) GetActorEgressPolicy(ctx context.Context, in *ateapi.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapi.EgressPolicy, error) {
	f.record("GetActorEgressPolicy", in)
	if f.getEgressPolicyFn != nil {
		return f.getEgressPolicyFn(ctx, in)
	}
	return &ateapi.EgressPolicy{}, nil
}

func (f *fakeControlClient) ResumeActor(ctx context.Context, in *ateapi.ResumeActorRequest, _ ...grpc.CallOption) (*ateapi.ResumeActorResponse, error) {
	f.record("ResumeActor", in)
	if f.resumeActorFn != nil {
		return f.resumeActorFn(ctx, in)
	}
	return &ateapi.ResumeActorResponse{}, nil
}

func (f *fakeControlClient) SuspendActor(ctx context.Context, in *ateapi.SuspendActorRequest, _ ...grpc.CallOption) (*ateapi.SuspendActorResponse, error) {
	f.record("SuspendActor", in)
	if f.suspendActorFn != nil {
		return f.suspendActorFn(ctx, in)
	}
	return &ateapi.SuspendActorResponse{Actor: actorWithSnapshot(ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED, "gs://bucket/actor/snapshot")}, nil
}

func (f *fakeControlClient) DeleteActor(ctx context.Context, in *ateapi.DeleteActorRequest, _ ...grpc.CallOption) (*ateapi.Actor, error) {
	f.record("DeleteActor", in)
	if f.deleteActorFn != nil {
		return f.deleteActorFn(ctx, in)
	}
	return &ateapi.Actor{}, nil
}

func (f *fakeControlClient) GetActor(ctx context.Context, in *ateapi.GetActorRequest, _ ...grpc.CallOption) (*ateapi.Actor, error) {
	f.record("GetActor", in)
	if f.getActorFn != nil {
		return f.getActorFn(ctx, in)
	}
	return &ateapi.Actor{Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RUNNING}}, nil
}

// actorTestClock advances instantly instead of sleeping, so retry tests never
// wait. Sleep honors cancellation like sleepContext does.
type actorTestClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newActorTestClock() *actorTestClock {
	return &actorTestClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *actorTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *actorTestClock) Sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err() == nil
}

func (c *actorTestClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

func newTestActorManager(client ateapi.ControlClient, clock *actorTestClock) *actorManager {
	m := newActorManager(client, actorTestAtespace, actorTestTemplate, actorTestControlPlane)
	m.now = clock.Now
	m.sleep = clock.Sleep
	return m
}

// snapshotStorageStateFailed stands for a terminal upload failure. Substrate
// v1.0.0's generated enum stops at COMPLETED, so the client treats any state
// outside PENDING and COMPLETED as terminal; this numeric value is what a
// newer server would report for a failed upload.
const snapshotStorageStateFailed = ateapi.SnapshotStorageState(3)

// snapshotGeneration returns one snapshot generation with the given durable
// storage state and object URI.
func snapshotGeneration(generation int32, state ateapi.SnapshotStorageState, uri string) *ateapi.Snapshot {
	return &ateapi.Snapshot{
		Generation: generation,
		DurableSnapshot: &ateapi.SnapshotStorage{
			State:  state,
			Object: &ateapi.ObjectSnapshot{SnapshotUri: uri},
		},
	}
}

// actorWithSnapshots returns a suspended actor holding the given snapshot
// generations.
func actorWithSnapshots(snapshots ...*ateapi.Snapshot) *ateapi.Actor {
	return &ateapi.Actor{Status: &ateapi.ActorStatus{
		State:     ateapi.ActorState_ACTOR_STATE_SUSPENDED,
		Snapshots: snapshots,
	}}
}

// actorWithSnapshot returns a suspended actor whose only snapshot has the
// given durable storage state and object URI.
func actorWithSnapshot(state ateapi.SnapshotStorageState, uri string) *ateapi.Actor {
	return actorWithSnapshots(snapshotGeneration(1, state, uri))
}

// matchingActor returns the actor Provision would create for name.
func matchingActor(name string) *ateapi.Actor {
	return &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: actorTestAtespace, Name: name},
		ActorTemplate: &ateapi.ObjectRef{Atespace: actorTestAtespace, Name: actorTestTemplate},
		Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	}
}

// matchingEgressPolicy returns the policy Provision would create for an actor.
func matchingEgressPolicy() *ateapi.EgressPolicy {
	return &ateapi.EgressPolicy{
		Metadata: &ateapi.ResourceMetadata{Atespace: actorTestAtespace, Name: "default"},
		Rules: []*ateapi.EgressRule{
			{
				TlsPassthrough: &ateapi.TlsPassthroughRule{
					Hostnames: []string{"api.anthropic.com"},
					Ports:     &ateapi.Ports{Numbers: []int32{443}},
				},
			},
			{
				Http: &ateapi.HttpRule{
					Hostnames: []string{"castworks-dev.castworks-system.svc.cluster.local"},
					Ports:     &ateapi.Ports{Numbers: []int32{9090}},
				},
			},
		},
	}
}

func checkActorRef(t *testing.T, ref *ateapi.ObjectRef, name string) {
	t.Helper()
	if ref == nil {
		t.Fatal("actor reference is nil")
	}
	if got := ref.GetAtespace(); got != actorTestAtespace {
		t.Errorf("actor reference atespace = %q, want %q", got, actorTestAtespace)
	}
	if got := ref.GetName(); got != name {
		t.Errorf("actor reference name = %q, want %q", got, name)
	}
}

func TestProvisionCallsInOrderAndBuildsEgressRules(t *testing.T) {
	fake := &fakeControlClient{}
	m := newTestActorManager(fake, newActorTestClock())

	timing, err := m.Provision(context.Background(), "abc123def456")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if timing.Duration < 0 {
		t.Fatalf("Provision timing = %v, want non-negative", timing.Duration)
	}

	if got, want := fake.callList(), []string{"CreateActor", "CreateActorEgressPolicy", "ResumeActor"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	create := fake.requestAt(0).(*ateapi.CreateActorRequest)
	a := create.GetActor()
	if got, want := a.GetMetadata().GetAtespace(), actorTestAtespace; got != want {
		t.Errorf("actor atespace = %q, want %q", got, want)
	}
	if got, want := a.GetMetadata().GetName(), "cw-abc123def456"; got != want {
		t.Errorf("actor name = %q, want %q", got, want)
	}
	if got, want := a.GetActorTemplate().GetAtespace(), actorTestAtespace; got != want {
		t.Errorf("template atespace = %q, want %q", got, want)
	}
	if got, want := a.GetActorTemplate().GetName(), actorTestTemplate; got != want {
		t.Errorf("template name = %q, want %q", got, want)
	}

	policyReq := fake.requestAt(1).(*ateapi.CreateActorEgressPolicyRequest)
	checkActorRef(t, policyReq.GetActor(), "cw-abc123def456")
	policy := policyReq.GetEgressPolicy()
	if got, want := policy.GetMetadata().GetName(), "default"; got != want {
		t.Errorf("egress policy name = %q, want %q", got, want)
	}
	if got, want := policy.GetMetadata().GetAtespace(), actorTestAtespace; got != want {
		t.Errorf("egress policy atespace = %q, want %q", got, want)
	}
	rules := policy.GetRules()
	if len(rules) != 2 {
		t.Fatalf("egress rules = %d, want 2", len(rules))
	}
	tlsRule := rules[0].GetTlsPassthrough()
	if tlsRule == nil {
		t.Fatalf("rule 0 = %v, want tls_passthrough", rules[0])
	}
	if got, want := tlsRule.GetHostnames(), []string{"api.anthropic.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tls_passthrough hostnames = %v, want %v", got, want)
	}
	if got, want := tlsRule.GetPorts().GetNumbers(), []int32{443}; !reflect.DeepEqual(got, want) {
		t.Errorf("tls_passthrough ports = %v, want %v", got, want)
	}
	httpRule := rules[1].GetHttp()
	if httpRule == nil {
		t.Fatalf("rule 1 = %v, want http", rules[1])
	}
	if got, want := httpRule.GetHostnames(), []string{"castworks-dev.castworks-system.svc.cluster.local"}; !reflect.DeepEqual(got, want) {
		t.Errorf("http hostnames = %v, want %v", got, want)
	}
	if got, want := httpRule.GetPorts().GetNumbers(), []int32{9090}; !reflect.DeepEqual(got, want) {
		t.Errorf("http ports = %v, want %v", got, want)
	}

	checkActorRef(t, fake.requestAt(2).(*ateapi.ResumeActorRequest).GetActor(), "cw-abc123def456")
	for _, method := range []string{"CreateActor", "CreateActorEgressPolicy", "ResumeActor"} {
		if got := fake.countCall(method); got != 1 {
			t.Errorf("%s calls = %d, want 1", method, got)
		}
	}
}

func TestProvisionRejectsInvalidRunIDBeforeCallingAteAPI(t *testing.T) {
	for _, runID := range []string{"Bad_ID", "Upper", "with.dot", ""} {
		t.Run(fmt.Sprintf("%q", runID), func(t *testing.T) {
			fake := &fakeControlClient{}
			m := newTestActorManager(fake, newActorTestClock())
			if _, err := m.Provision(context.Background(), runID); err == nil {
				t.Fatalf("Provision(%q) = nil error, want rejection", runID)
			}
			if calls := fake.callList(); len(calls) != 0 {
				t.Fatalf("ate-api calls = %v, want none", calls)
			}
		})
	}
}

func TestProvisionTreatsAlreadyExistsAsDone(t *testing.T) {
	alreadyExists := status.Error(codes.AlreadyExists, "exists")
	policy := matchingEgressPolicy()
	// Rules are an unordered set on the server, so return them reversed.
	slices.Reverse(policy.Rules)
	fake := &fakeControlClient{
		createActorFn: func(context.Context, *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
			return nil, alreadyExists
		},
		createEgressPolicyFn: func(context.Context, *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
			return nil, alreadyExists
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return matchingActor("cw-abc123def456"), nil
		},
		getEgressPolicyFn: func(context.Context, *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
			return policy, nil
		},
	}
	m := newTestActorManager(fake, newActorTestClock())

	if _, err := m.Provision(context.Background(), "abc123def456"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := fake.countCall("GetActor"); got != 1 {
		t.Errorf("GetActor calls = %d, want 1 verification", got)
	}
	if got := fake.countCall("GetActorEgressPolicy"); got != 1 {
		t.Errorf("GetActorEgressPolicy calls = %d, want 1 verification", got)
	}
}

func TestProvisionRejectsMismatchedExistingActor(t *testing.T) {
	alreadyExists := status.Error(codes.AlreadyExists, "exists")
	existing := matchingActor("cw-abc123def456")
	existing.ActorTemplate.Name = "cw-fedcba987654"
	fake := &fakeControlClient{
		createActorFn: func(context.Context, *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
			return nil, alreadyExists
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return existing, nil
		},
	}
	m := newTestActorManager(fake, newActorTestClock())

	_, err := m.Provision(context.Background(), "abc123def456")
	if err == nil {
		t.Fatal("Provision = nil error, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "actor_template.name") {
		t.Errorf("error = %v, want it to name actor_template.name", err)
	}
	if got, want := fake.callList(), []string{"CreateActor", "GetActor"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestProvisionRejectsMismatchedExistingEgressPolicy(t *testing.T) {
	alreadyExists := status.Error(codes.AlreadyExists, "exists")
	existing := matchingEgressPolicy()
	existing.Rules[1].GetHttp().Hostnames = []string{"evil.example.com"}
	fake := &fakeControlClient{
		createEgressPolicyFn: func(context.Context, *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
			return nil, alreadyExists
		},
		getEgressPolicyFn: func(context.Context, *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
			return existing, nil
		},
	}
	m := newTestActorManager(fake, newActorTestClock())

	_, err := m.Provision(context.Background(), "abc123def456")
	if err == nil {
		t.Fatal("Provision = nil error, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "egress policy") {
		t.Errorf("error = %v, want it to name the egress policy", err)
	}
	if got, want := fake.callList(), []string{"CreateActor", "CreateActorEgressPolicy", "GetActorEgressPolicy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestProvisionCanBeRetriedAfterInterruption(t *testing.T) {
	var createCalls, policyCalls int
	fake := &fakeControlClient{
		createActorFn: func(context.Context, *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
			createCalls++
			if createCalls == 1 {
				return &ateapi.Actor{}, nil
			}
			return nil, status.Error(codes.AlreadyExists, "exists")
		},
		createEgressPolicyFn: func(context.Context, *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
			policyCalls++
			if policyCalls == 1 {
				return nil, status.Error(codes.Internal, "interrupted")
			}
			return nil, status.Error(codes.AlreadyExists, "exists")
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return matchingActor("cw-abc123def456"), nil
		},
		getEgressPolicyFn: func(context.Context, *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
			return matchingEgressPolicy(), nil
		},
	}
	m := newTestActorManager(fake, newActorTestClock())

	if _, err := m.Provision(context.Background(), "abc123def456"); err == nil {
		t.Fatal("first Provision = nil error, want the interruption")
	}
	if _, err := m.Provision(context.Background(), "abc123def456"); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if createCalls != 2 || policyCalls != 2 {
		t.Fatalf("createCalls = %d, policyCalls = %d, want 2 and 2", createCalls, policyCalls)
	}
}

func TestProvisionRetriesResumeWithBackoff(t *testing.T) {
	for _, code := range []codes.Code{codes.ResourceExhausted, codes.Aborted} {
		t.Run(code.String(), func(t *testing.T) {
			clock := newActorTestClock()
			var resumeCalls int
			fake := &fakeControlClient{
				resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
					resumeCalls++
					if resumeCalls <= 2 {
						return nil, status.Error(code, "try again")
					}
					return &ateapi.ResumeActorResponse{Resumed: true}, nil
				},
			}
			m := newTestActorManager(fake, clock)

			if _, err := m.Provision(context.Background(), "abc123def456"); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			if resumeCalls != 3 {
				t.Fatalf("ResumeActor calls = %d, want 3", resumeCalls)
			}
			if got, want := clock.slept(), []time.Duration{time.Second, 2 * time.Second}; !reflect.DeepEqual(got, want) {
				t.Fatalf("backoff waits = %v, want %v", got, want)
			}
			for i := 0; i < 3; i++ {
				checkActorRef(t, fake.requestAt(2+i).(*ateapi.ResumeActorRequest).GetActor(), "cw-abc123def456")
			}
		})
	}
}

func TestResumeRetryBudgetExhausted(t *testing.T) {
	clock := newActorTestClock()
	fake := &fakeControlClient{
		resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
			return nil, status.Error(codes.ResourceExhausted, "no worker")
		},
	}
	m := newTestActorManager(fake, clock)

	timing, err := m.RestoreSnapshot(context.Background(), "cw-abc123def456")
	if err == nil {
		t.Fatal("RestoreSnapshot = nil error, want budget exhausted")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("status code = %v, want %v; error: %v", got, codes.ResourceExhausted, err)
	}
	if timing.Duration != resumeRetryBudget {
		t.Fatalf("duration = %v, want exactly %v", timing.Duration, resumeRetryBudget)
	}

	// 1, 2, 4, 8, then 10 until the last wait trims the remaining 5 seconds.
	wantSleeps := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second,
		10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second,
		5 * time.Second,
	}
	if got := clock.slept(); !reflect.DeepEqual(got, wantSleeps) {
		t.Fatalf("backoff waits = %v, want %v", got, wantSleeps)
	}
	// One call before each of the 15 waits, plus the call at the deadline.
	if got := fake.countCall("ResumeActor"); got != 16 {
		t.Fatalf("ResumeActor calls = %d, want 16", got)
	}
	if got := fake.countCall("GetActor"); got != 1 {
		t.Fatalf("GetActor calls = %d, want 1 crash check at the deadline", got)
	}
}

func TestResumeBudgetExhaustionChecksForCrash(t *testing.T) {
	clock := newActorTestClock()
	fake := &fakeControlClient{
		resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
			return nil, status.Error(codes.ResourceExhausted, "no worker")
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return &ateapi.Actor{Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_CRASHED}}, nil
		},
	}
	m := newTestActorManager(fake, clock)

	_, err := m.RestoreSnapshot(context.Background(), "cw-abc123def456")
	if !errors.Is(err, sandboxprovider.ErrActorCrashed) {
		t.Fatalf("error = %v, want ErrActorCrashed", err)
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("status code = %v, want %v; error: %v", got, codes.ResourceExhausted, err)
	}
	if got := fake.countCall("GetActor"); got != 1 {
		t.Fatalf("GetActor calls = %d, want 1 crash check at the deadline", got)
	}
	if got := fake.countCall("ResumeActor"); got != 16 {
		t.Fatalf("ResumeActor calls = %d, want 16", got)
	}
}

func TestRestoreSnapshotResumesActor(t *testing.T) {
	fake := &fakeControlClient{}
	m := newTestActorManager(fake, newActorTestClock())

	if _, err := m.RestoreSnapshot(context.Background(), "cw-abc123def456"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	checkActorRef(t, fake.requestAt(0).(*ateapi.ResumeActorRequest).GetActor(), "cw-abc123def456")
	if got, want := fake.callList(), []string{"ResumeActor"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestResumeReturnsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeControlClient{
		resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
			cancel()
			return nil, status.Error(codes.ResourceExhausted, "no worker")
		},
	}
	m := newTestActorManager(fake, newActorTestClock())

	start := time.Now()
	_, err := m.RestoreSnapshot(ctx, "cw-abc123def456")
	if err == nil {
		t.Fatal("RestoreSnapshot = nil error, want cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want wrapped context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RestoreSnapshot took %v after cancellation, want immediate return", elapsed)
	}
}

func TestResumeCrashIsReportedAsErrActorCrashed(t *testing.T) {
	cases := []struct {
		name        string
		resumeCode  codes.Code
		state       ateapi.ActorState
		wantCrashed bool
		provision   bool
	}{
		{name: "failed precondition with crashed actor", resumeCode: codes.FailedPrecondition, state: ateapi.ActorState_ACTOR_STATE_CRASHED, wantCrashed: true},
		{name: "failed precondition with crashed actor through provision", resumeCode: codes.FailedPrecondition, state: ateapi.ActorState_ACTOR_STATE_CRASHED, wantCrashed: true, provision: true},
		{name: "wakeup probe timeout shows as crashed on another error", resumeCode: codes.Internal, state: ateapi.ActorState_ACTOR_STATE_CRASHED, wantCrashed: true},
		{name: "failed precondition with running actor", resumeCode: codes.FailedPrecondition, state: ateapi.ActorState_ACTOR_STATE_RUNNING, wantCrashed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeControlClient{
				resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
					return nil, status.Error(tc.resumeCode, "resume failed")
				},
				getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
					return &ateapi.Actor{Status: &ateapi.ActorStatus{State: tc.state}}, nil
				},
			}
			m := newTestActorManager(fake, newActorTestClock())

			var err error
			if tc.provision {
				_, err = m.Provision(context.Background(), "abc123def456")
			} else {
				_, err = m.RestoreSnapshot(context.Background(), "cw-abc123def456")
			}
			if err == nil {
				t.Fatal("resume = nil error, want failure")
			}
			if got := errors.Is(err, sandboxprovider.ErrActorCrashed); got != tc.wantCrashed {
				t.Fatalf("errors.Is(err, ErrActorCrashed) = %v, want %v; error: %v", got, tc.wantCrashed, err)
			}
			if got := status.Code(err); got != tc.resumeCode {
				t.Fatalf("status code = %v, want %v; error: %v", got, tc.resumeCode, err)
			}
			if fake.countCall("GetActor") != 1 {
				t.Fatalf("GetActor calls = %d, want 1 crash check", fake.countCall("GetActor"))
			}
		})
	}
}

func TestAnyResumeFailureChecksForCrash(t *testing.T) {
	fake := &fakeControlClient{
		resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
			return nil, status.Error(codes.PermissionDenied, "denied")
		},
	}
	m := newTestActorManager(fake, newActorTestClock())
	if _, err := m.RestoreSnapshot(context.Background(), "cw-abc123def456"); err == nil {
		t.Fatal("RestoreSnapshot = nil error, want failure")
	}
	if got := fake.callList(); !reflect.DeepEqual(got, []string{"ResumeActor", "GetActor"}) {
		t.Fatalf("calls = %v, want a GetActor crash check after ResumeActor", got)
	}
}

func TestNotFoundMapping(t *testing.T) {
	notFound := status.Error(codes.NotFound, "gone")

	t.Run("GetSandbox", func(t *testing.T) {
		fake := &fakeControlClient{
			getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
				return nil, notFound
			},
		}
		m := newTestActorManager(fake, newActorTestClock())
		if _, err := m.GetSandbox(context.Background(), "cw-abc123def456"); !errors.Is(err, sandboxprovider.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		} else if got := status.Code(err); got != codes.NotFound {
			t.Fatalf("status code = %v, want NotFound", got)
		}
	})

	t.Run("Snapshot", func(t *testing.T) {
		fake := &fakeControlClient{
			suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
				return nil, notFound
			},
		}
		m := newTestActorManager(fake, newActorTestClock())
		if _, err := m.Snapshot(context.Background(), "cw-abc123def456"); !errors.Is(err, sandboxprovider.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("RestoreSnapshot", func(t *testing.T) {
		fake := &fakeControlClient{
			resumeActorFn: func(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
				return nil, notFound
			},
			getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
				return nil, notFound
			},
		}
		m := newTestActorManager(fake, newActorTestClock())
		if _, err := m.RestoreSnapshot(context.Background(), "cw-abc123def456"); !errors.Is(err, sandboxprovider.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})
}

func TestGetSandboxStateMapping(t *testing.T) {
	cases := []struct {
		state ateapi.ActorState
		want  sandboxprovider.ActorState
	}{
		{ateapi.ActorState_ACTOR_STATE_RUNNING, sandboxprovider.ActorStateRunning},
		{ateapi.ActorState_ACTOR_STATE_RESUMING, sandboxprovider.ActorStateResuming},
		{ateapi.ActorState_ACTOR_STATE_SUSPENDED, sandboxprovider.ActorStateSuspended},
		{ateapi.ActorState_ACTOR_STATE_CRASHED, sandboxprovider.ActorStateCrashed},
		{ateapi.ActorState_ACTOR_STATE_SUSPENDING, sandboxprovider.ActorStateUnknown},
		{ateapi.ActorState_ACTOR_STATE_DELETING, sandboxprovider.ActorStateUnknown},
		{ateapi.ActorState_ACTOR_STATE_REVERTING, sandboxprovider.ActorStateUnknown},
		{ateapi.ActorState_ACTOR_STATE_UNSPECIFIED, sandboxprovider.ActorStateUnknown},
		{ateapi.ActorState(99), sandboxprovider.ActorStateUnknown},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%v", tc.state), func(t *testing.T) {
			fake := &fakeControlClient{
				getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
					return &ateapi.Actor{Status: &ateapi.ActorStatus{State: tc.state}}, nil
				},
			}
			m := newTestActorManager(fake, newActorTestClock())
			status, err := m.GetSandbox(context.Background(), "cw-abc123def456")
			if err != nil {
				t.Fatalf("GetSandbox: %v", err)
			}
			if status.State != tc.want {
				t.Fatalf("state = %q, want %q", status.State, tc.want)
			}
			req := fake.requestAt(0).(*ateapi.GetActorRequest)
			checkActorRef(t, req.GetActor(), "cw-abc123def456")
			if got := fake.countCall("GetActor"); got != 1 {
				t.Fatalf("GetActor calls = %d, want 1", got)
			}
		})
	}
}

func TestSnapshotRequestsSynchronousMode(t *testing.T) {
	fake := &fakeControlClient{}
	m := newTestActorManager(fake, newActorTestClock())

	timing, err := m.Snapshot(context.Background(), "cw-abc123def456")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	req := fake.requestAt(0).(*ateapi.SuspendActorRequest)
	if got, want := req.GetOptions().GetMode(), ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS; got != want {
		t.Fatalf("suspend mode = %v, want %v", got, want)
	}
	if got, want := req.GetActor().GetName(), "cw-abc123def456"; got != want {
		t.Fatalf("actor = %q, want %q", got, want)
	}
	if got := req.GetActor().GetAtespace(); got != actorTestAtespace {
		t.Fatalf("actor atespace = %q, want %q", got, actorTestAtespace)
	}
	if timing.SnapshotBytes != 0 {
		t.Fatalf("SnapshotBytes = %d, want 0", timing.SnapshotBytes)
	}
	if got := fake.countCall("SuspendActor"); got != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", got)
	}
	// The synchronous response already carried a completed durable snapshot,
	// so no polling is needed.
	if got := fake.countCall("GetActor"); got != 0 {
		t.Fatalf("GetActor calls = %d, want 0", got)
	}
}

func TestSnapshotWaitsForDurableSnapshot(t *testing.T) {
	clock := newActorTestClock()
	pending := actorWithSnapshot(ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING, "gs://bucket/actor/1")
	complete := actorWithSnapshot(ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED, "gs://bucket/actor/1")
	var getCalls int
	fake := &fakeControlClient{
		// A retried suspend finds the actor already suspended and returns the
		// node-local snapshot, which is not durable yet.
		suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
			return &ateapi.SuspendActorResponse{Actor: pending}, nil
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			getCalls++
			switch getCalls {
			case 1:
				return nil, status.Error(codes.Unavailable, "forward shut down")
			case 2:
				return pending, nil
			default:
				return complete, nil
			}
		},
	}
	m := newTestActorManager(fake, clock)

	timing, err := m.Snapshot(context.Background(), "cw-abc123def456")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// The second answer had no durable snapshot, so Snapshot must not accept
	// it as success.
	if getCalls != 3 {
		t.Fatalf("GetActor calls = %d, want 3 (one Unavailable, one pending, one durable)", getCalls)
	}
	if got, want := clock.slept(), []time.Duration{time.Second, time.Second, time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("poll waits = %v, want %v", got, want)
	}
	if got, want := timing.Duration, 3*time.Second; got != want {
		t.Fatalf("Snapshot duration = %v, want %v", got, want)
	}
	if got := fake.countCall("SuspendActor"); got != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", got)
	}
}

func TestSnapshotTimesOutWithoutDurableSnapshot(t *testing.T) {
	clock := newActorTestClock()
	pending := actorWithSnapshot(ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING, "gs://bucket/actor/1")
	fake := &fakeControlClient{
		suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
			return &ateapi.SuspendActorResponse{Actor: pending}, nil
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			return pending, nil
		},
	}
	m := newTestActorManager(fake, clock)

	timing, err := m.Snapshot(context.Background(), "cw-abc123def456")
	if err == nil {
		t.Fatal("Snapshot = nil error, want a timeout")
	}
	if !strings.Contains(err.Error(), "durable snapshot") {
		t.Fatalf("error = %v, want it to mention the durable snapshot", err)
	}
	if got, want := timing.Duration, 60*time.Second; got != want {
		t.Fatalf("Snapshot duration = %v, want %v", got, want)
	}
	if got := fake.countCall("GetActor"); got < 2 {
		t.Fatalf("GetActor calls = %d, want repeated polling before the timeout", got)
	}
}

func TestSnapshotFailsFastOnFailedDurableSnapshot(t *testing.T) {
	t.Run("failed in the suspend response", func(t *testing.T) {
		clock := newActorTestClock()
		failed := actorWithSnapshot(snapshotStorageStateFailed, "gs://bucket/actor/1")
		fake := &fakeControlClient{
			suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
				return &ateapi.SuspendActorResponse{Actor: failed}, nil
			},
		}
		m := newTestActorManager(fake, clock)

		timing, err := m.Snapshot(context.Background(), "cw-abc123def456")
		if err == nil {
			t.Fatal("Snapshot = nil error, want the failed durable snapshot reported")
		}
		if !strings.Contains(err.Error(), "durable snapshot") {
			t.Fatalf("error = %v, want it to mention the durable snapshot", err)
		}
		if got := clock.slept(); len(got) != 0 {
			t.Fatalf("Snapshot slept %v, want no polling after a terminal durable state", got)
		}
		if got := fake.countCall("GetActor"); got != 0 {
			t.Fatalf("GetActor calls = %d, want 0 after a terminal durable state", got)
		}
		if got := timing.Duration; got != 0 {
			t.Fatalf("Snapshot duration = %v, want 0", got)
		}
	})

	t.Run("failed on a later poll", func(t *testing.T) {
		clock := newActorTestClock()
		pending := actorWithSnapshot(ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING, "gs://bucket/actor/1")
		failed := actorWithSnapshot(snapshotStorageStateFailed, "gs://bucket/actor/1")
		fake := &fakeControlClient{
			suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
				return &ateapi.SuspendActorResponse{Actor: pending}, nil
			},
			getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
				return failed, nil
			},
		}
		m := newTestActorManager(fake, clock)

		timing, err := m.Snapshot(context.Background(), "cw-abc123def456")
		if err == nil {
			t.Fatal("Snapshot = nil error, want the failed durable snapshot reported")
		}
		if got, want := clock.slept(), []time.Duration{time.Second}; !reflect.DeepEqual(got, want) {
			t.Fatalf("poll waits = %v, want %v", got, want)
		}
		if got := fake.countCall("GetActor"); got != 1 {
			t.Fatalf("GetActor calls = %d, want 1", got)
		}
		if got, want := timing.Duration, time.Second; got != want {
			t.Fatalf("Snapshot duration = %v, want %v", got, want)
		}
	})
}

func TestSnapshotPollsNewestGenerationWhileOlderIsComplete(t *testing.T) {
	clock := newActorTestClock()
	olderComplete := snapshotGeneration(1, ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED, "gs://bucket/actor/1")
	pending := actorWithSnapshots(olderComplete, snapshotGeneration(2, ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING, "gs://bucket/actor/2"))
	complete := actorWithSnapshots(olderComplete, snapshotGeneration(2, ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED, "gs://bucket/actor/2"))
	var getCalls int
	fake := &fakeControlClient{
		suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
			return &ateapi.SuspendActorResponse{Actor: pending}, nil
		},
		getActorFn: func(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error) {
			getCalls++
			if getCalls == 1 {
				return pending, nil
			}
			return complete, nil
		},
	}
	m := newTestActorManager(fake, clock)

	if _, err := m.Snapshot(context.Background(), "cw-abc123def456"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Generation 1 is already complete, but the suspend requested generation 2.
	// Accepting the older snapshot would return before the new upload finishes.
	if getCalls != 2 {
		t.Fatalf("GetActor calls = %d, want 2 while generation 2 is pending", getCalls)
	}
	if got, want := clock.slept(), []time.Duration{time.Second, time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("poll waits = %v, want %v", got, want)
	}
}

func TestSnapshotFailureIsReturned(t *testing.T) {
	fake := &fakeControlClient{
		suspendActorFn: func(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
			return nil, status.Error(codes.Internal, "snapshot failed")
		},
	}
	m := newTestActorManager(fake, newActorTestClock())
	if _, err := m.Snapshot(context.Background(), "cw-abc123def456"); err == nil {
		t.Fatal("Snapshot = nil error, want failure")
	} else if got := status.Code(err); got != codes.Internal {
		t.Fatalf("status code = %v, want Internal", got)
	}
	if got := fake.countCall("GetActor"); got != 0 {
		t.Fatalf("GetActor calls = %d, want 0 after a failed suspend", got)
	}
}

func TestDestroy(t *testing.T) {
	t.Run("missing actor is success", func(t *testing.T) {
		fake := &fakeControlClient{
			deleteActorFn: func(context.Context, *ateapi.DeleteActorRequest) (*ateapi.Actor, error) {
				return nil, status.Error(codes.NotFound, "gone")
			},
		}
		m := newTestActorManager(fake, newActorTestClock())
		if err := m.Destroy(context.Background(), "cw-abc123def456"); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		checkActorRef(t, fake.requestAt(0).(*ateapi.DeleteActorRequest).GetActor(), "cw-abc123def456")
		if got := fake.countCall("DeleteActor"); got != 1 {
			t.Fatalf("DeleteActor calls = %d, want 1", got)
		}
	})

	t.Run("repeated destroy", func(t *testing.T) {
		fake := &fakeControlClient{}
		m := newTestActorManager(fake, newActorTestClock())
		for i := 0; i < 2; i++ {
			if err := m.Destroy(context.Background(), "cw-abc123def456"); err != nil {
				t.Fatalf("Destroy #%d: %v", i+1, err)
			}
			checkActorRef(t, fake.requestAt(i).(*ateapi.DeleteActorRequest).GetActor(), "cw-abc123def456")
		}
		if got := fake.countCall("DeleteActor"); got != 2 {
			t.Fatalf("DeleteActor calls = %d, want 2", got)
		}
	})

	t.Run("other failures are returned", func(t *testing.T) {
		fake := &fakeControlClient{
			deleteActorFn: func(context.Context, *ateapi.DeleteActorRequest) (*ateapi.Actor, error) {
				return nil, status.Error(codes.Internal, "boom")
			},
		}
		m := newTestActorManager(fake, newActorTestClock())
		if err := m.Destroy(context.Background(), "cw-abc123def456"); err == nil {
			t.Fatal("Destroy = nil error, want failure")
		} else if got := status.Code(err); got != codes.Internal {
			t.Fatalf("status code = %v, want Internal", got)
		}
		checkActorRef(t, fake.requestAt(0).(*ateapi.DeleteActorRequest).GetActor(), "cw-abc123def456")
	})
}

func TestConcurrentCallsForDifferentActors(t *testing.T) {
	fake := &fakeControlClient{}
	m := newTestActorManager(fake, newActorTestClock())

	const actors = 16
	var wg sync.WaitGroup
	errs := make(chan error, actors)
	for i := 0; i < actors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("run%09d", i)
			ctx := context.Background()
			if _, err := m.Provision(ctx, runID); err != nil {
				errs <- fmt.Errorf("Provision %s: %w", runID, err)
				return
			}
			actor := sandboxprovider.ActorName(runID)
			if _, err := m.GetSandbox(ctx, actor); err != nil {
				errs <- fmt.Errorf("GetSandbox %s: %w", runID, err)
				return
			}
			if _, err := m.Snapshot(ctx, actor); err != nil {
				errs <- fmt.Errorf("Snapshot %s: %w", runID, err)
				return
			}
			if _, err := m.RestoreSnapshot(ctx, actor); err != nil {
				errs <- fmt.Errorf("RestoreSnapshot %s: %w", runID, err)
				return
			}
			if err := m.Destroy(ctx, actor); err != nil {
				errs <- fmt.Errorf("Destroy %s: %w", runID, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Provision (3) + GetSandbox (1) + Snapshot (1) + RestoreSnapshot (1) +
	// Destroy (1) per actor.
	if got, want := len(fake.callList()), actors*7; got != want {
		t.Fatalf("ate-api calls = %d, want %d", got, want)
	}
}

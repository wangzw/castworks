// The actor lifecycle in this file follows the provider contract in
// internal/sandboxprovider/provider.go and sandboxprovider.md's "接口", "网络"
// and "错误处理" sections. The RPC sequence is modelled on Substrate v1.0.0's
// cmd/ate-setup/internal/steps/substrate.go (Apache License, Version 2.0).

package substrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/wangzw/castworks/internal/sandboxprovider"
)

const (
	// egressPolicyName is the only name a nested EgressPolicy may have.
	egressPolicyName = "default"

	// anthropicHostname is passed through undecrypted; the control-plane host
	// is allowed as cleartext HTTP so the warden can speak h2c to it.
	anthropicHostname = "api.anthropic.com"
	anthropicPort     = int32(443)

	// ResumeActor may return RESOURCE_EXHAUSTED while the scheduler waits for
	// a worker or ABORTED after a conflicting write. Retry such failures with
	// exponential backoff for at most two minutes from the first call.
	resumeRetryInitialDelay = time.Second
	resumeRetryMaxDelay     = 10 * time.Second
	resumeRetryBudget       = 2 * time.Minute

	// A synchronous SuspendActor normally returns after the snapshot reaches
	// durable storage, but a call that finds the actor already suspended (as
	// a retry after Unavailable does) returns the node-local snapshot instead.
	// Snapshot therefore checks for a completed durable snapshot and polls for
	// one for up to snapshotDurableBudget.
	snapshotDurablePollInterval = time.Second
	snapshotDurableBudget       = 60 * time.Second
)

// actorManager implements the five sandboxprovider.Provider methods against
// ate-api. It keeps no per-actor state, so calls for different actors may run
// concurrently; runcontroller never calls one actor concurrently.
type actorManager struct {
	client       ateapi.ControlClient
	atespace     string
	template     string
	controlPlane string // host:port written to the template env

	// now and sleep are seams for the retry tests. Production uses the real
	// clock and sleepContext.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
}

func newActorManager(client ateapi.ControlClient, atespace, template, controlPlane string) *actorManager {
	return &actorManager{
		client:       client,
		atespace:     atespace,
		template:     template,
		controlPlane: controlPlane,
		now:          time.Now,
		sleep:        sleepContext,
	}
}

var _ sandboxprovider.Provider = (*actorManager)(nil)

// Provision creates the actor, its egress policy and resumes it. ALREADY_EXISTS
// from either create step means an earlier attempt got that far, so Provision
// can be retried after an interruption.
func (m *actorManager) Provision(ctx context.Context, runID string) (sandboxprovider.Timing, error) {
	start := m.now()
	actor := sandboxprovider.ActorName(runID)
	if errs := validation.IsDNS1123Label(actor); len(errs) > 0 {
		return sandboxprovider.Timing{Duration: m.now().Sub(start)}, fmt.Errorf(
			"provision: actor name %q derived from run ID %q is not a DNS label: %s",
			actor, runID, strings.Join(errs, "; "))
	}

	if err := m.createActor(ctx, actor); err != nil {
		return sandboxprovider.Timing{Duration: m.now().Sub(start)}, err
	}
	if err := m.createEgressPolicy(ctx, actor); err != nil {
		return sandboxprovider.Timing{Duration: m.now().Sub(start)}, err
	}
	if err := m.resumeActor(ctx, actor); err != nil {
		return sandboxprovider.Timing{Duration: m.now().Sub(start)}, err
	}
	return sandboxprovider.Timing{Duration: m.now().Sub(start)}, nil
}

// Snapshot checkpoints the actor and waits for the snapshot to reach durable
// storage. ate-api v1.0.0 reports no snapshot size, so SnapshotBytes is zero.
func (m *actorManager) Snapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	start := m.now()
	resp, err := m.client.SuspendActor(ctx, &ateapi.SuspendActorRequest{
		Actor: m.actorRef(actor),
		Options: &ateapi.SuspendOptions{
			Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS,
		},
	})
	if err != nil {
		return sandboxprovider.Timing{Duration: m.now().Sub(start)}, m.rpcError("suspend actor", actor, err)
	}
	if err := m.waitForDurableSnapshot(ctx, actor, resp.GetActor()); err != nil {
		return sandboxprovider.Timing{Duration: m.now().Sub(start)}, err
	}
	return sandboxprovider.Timing{Duration: m.now().Sub(start)}, nil
}

// waitForDurableSnapshot confirms that the actor's most recent snapshot is
// complete in durable storage. The actor returned by SuspendActor is checked
// first; when its snapshot is still pending, GetActor is polled until the
// upload completes or snapshotDurableBudget expires. A snapshot that has
// reached any other terminal state fails at once instead of polling for the
// whole budget. An Unavailable from GetActor is retried within the budget,
// because a read is safe to repeat.
func (m *actorManager) waitForDurableSnapshot(ctx context.Context, actor string, a *ateapi.Actor) error {
	if complete, err := durableSnapshotComplete(a); err != nil {
		return fmt.Errorf("suspend actor %s: %w", actor, err)
	} else if complete {
		return nil
	}
	deadline := m.now().Add(snapshotDurableBudget)
	for {
		remaining := deadline.Sub(m.now())
		if remaining <= 0 {
			return fmt.Errorf("suspend actor %s: no durable snapshot after %s", actor, snapshotDurableBudget)
		}
		wait := snapshotDurablePollInterval
		if wait > remaining {
			wait = remaining
		}
		if !m.sleep(ctx, wait) {
			return fmt.Errorf("suspend actor %s: waiting for a durable snapshot: %w", actor, ctx.Err())
		}
		a, err := m.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: m.actorRef(actor)})
		if err != nil {
			if status.Code(err) == codes.Unavailable {
				continue
			}
			return m.rpcError("get actor after suspend", actor, err)
		}
		if complete, err := durableSnapshotComplete(a); err != nil {
			return fmt.Errorf("suspend actor %s: %w", actor, err)
		} else if complete {
			return nil
		}
	}
}

// durableSnapshotComplete reports whether the actor's most recent snapshot is
// complete in durable storage. An older completed snapshot does not count: the
// suspend just requested a new generation, so a PENDING latest snapshot means
// the upload is still running. A missing durable snapshot or PENDING is not an
// error because the upload can still finish. Any other state is terminal:
// Substrate v1.0.0 names only UNSPECIFIED, PENDING and COMPLETED, so a state
// this client does not recognize is returned as an error instead of polled
// until the budget expires.
func durableSnapshotComplete(a *ateapi.Actor) (bool, error) {
	latest := latestSnapshot(a)
	if latest == nil {
		return false, nil
	}
	durable := latest.GetDurableSnapshot()
	if durable == nil {
		return false, nil
	}
	switch state := durable.GetState(); state {
	case ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED:
		if durable.GetObject().GetSnapshotUri() == "" {
			return false, fmt.Errorf("snapshot generation %d: durable snapshot has no object URI", latest.GetGeneration())
		}
		return true, nil
	case ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING:
		return false, nil
	default:
		return false, fmt.Errorf("snapshot generation %d: durable snapshot state is %v, want PENDING or COMPLETED", latest.GetGeneration(), state)
	}
}

// latestSnapshot returns the actor's snapshot with the highest generation.
func latestSnapshot(a *ateapi.Actor) *ateapi.Snapshot {
	var latest *ateapi.Snapshot
	for _, s := range a.GetStatus().GetSnapshots() {
		if latest == nil || s.GetGeneration() > latest.GetGeneration() {
			latest = s
		}
	}
	return latest
}

// RestoreSnapshot resumes the actor from its latest snapshot.
func (m *actorManager) RestoreSnapshot(ctx context.Context, actor string) (sandboxprovider.Timing, error) {
	start := m.now()
	err := m.resumeActor(ctx, actor)
	return sandboxprovider.Timing{Duration: m.now().Sub(start)}, err
}

// Destroy deletes the actor. Its egress policy is deleted with it, and a
// missing actor already counts as destroyed.
func (m *actorManager) Destroy(ctx context.Context, actor string) error {
	_, err := m.client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: m.actorRef(actor)})
	if err == nil || status.Code(err) == codes.NotFound {
		return nil
	}
	return fmt.Errorf("delete actor %s: %w", actor, err)
}

// GetSandbox reports the actor's state. Transitional and unrecognized states
// map to unknown with no error; only crashed is terminal.
func (m *actorManager) GetSandbox(ctx context.Context, actor string) (sandboxprovider.SandboxStatus, error) {
	a, err := m.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: m.actorRef(actor)})
	if err != nil {
		return sandboxprovider.SandboxStatus{}, m.rpcError("get actor", actor, err)
	}
	return sandboxprovider.SandboxStatus{State: mapActorState(a.GetStatus().GetState())}, nil
}

func (m *actorManager) createActor(ctx context.Context, actor string) error {
	_, err := m.client.CreateActor(ctx, &ateapi.CreateActorRequest{
		Actor: &ateapi.Actor{
			Metadata:      &ateapi.ResourceMetadata{Atespace: m.atespace, Name: actor},
			ActorTemplate: &ateapi.ObjectRef{Atespace: m.atespace, Name: m.template},
		},
	})
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.AlreadyExists {
		return m.rpcError("create actor", actor, err)
	}
	// An earlier attempt created the actor. Its shape must match what this
	// provider would create; a different template would run the wrong image.
	existing, err := m.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: m.actorRef(actor)})
	if err != nil {
		return fmt.Errorf("get actor %s after a concurrent create: %w", actor, err)
	}
	return m.checkExistingActor(existing, actor)
}

// checkExistingActor reports the fields of an already existing actor that
// differ from the one Provision would create.
func (m *actorManager) checkExistingActor(a *ateapi.Actor, actor string) error {
	var errs []error
	field := func(path string, want, got any) {
		errs = append(errs, fmt.Errorf("actor %s: %s = %v, want %v", actor, path, got, want))
	}
	if got := a.GetMetadata().GetAtespace(); got != m.atespace {
		field("metadata.atespace", m.atespace, got)
	}
	if got := a.GetMetadata().GetName(); got != actor {
		field("metadata.name", actor, got)
	}
	if got := a.GetActorTemplate().GetAtespace(); got != m.atespace {
		field("actor_template.atespace", m.atespace, got)
	}
	if got := a.GetActorTemplate().GetName(); got != m.template {
		field("actor_template.name", m.template, got)
	}
	return errors.Join(errs...)
}

// createEgressPolicy allows Anthropic over TLS passthrough and the control
// plane as cleartext HTTP/2. The http rule matches the request authority, so
// the warden must dial the same host name.
func (m *actorManager) createEgressPolicy(ctx context.Context, actor string) error {
	want, err := m.buildEgressPolicy()
	if err != nil {
		return fmt.Errorf("create egress policy for actor %s: %w", actor, err)
	}
	_, err = m.client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{
		Actor:        m.actorRef(actor),
		EgressPolicy: want,
	})
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.AlreadyExists {
		return m.rpcError("create egress policy for actor", actor, err)
	}
	// An earlier attempt created the policy. Verify its rules before relying
	// on it: a policy that allows the wrong hosts would silently break the
	// warden's egress.
	existing, err := m.client.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{
		Actor: m.actorRef(actor),
		Name:  egressPolicyName,
	})
	if err != nil {
		return fmt.Errorf("get egress policy for actor %s after a concurrent create: %w", actor, err)
	}
	return checkEgressPolicy(existing, want, actor)
}

// buildEgressPolicy returns the policy Provision creates for an actor.
func (m *actorManager) buildEgressPolicy() (*ateapi.EgressPolicy, error) {
	host, portText, err := net.SplitHostPort(m.controlPlane)
	if err != nil {
		return nil, fmt.Errorf("invalid control-plane address %q: %w", m.controlPlane, err)
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid control-plane address %q: port must be 1-65535", m.controlPlane)
	}
	return &ateapi.EgressPolicy{
		Metadata: &ateapi.ResourceMetadata{Atespace: m.atespace, Name: egressPolicyName},
		Rules: []*ateapi.EgressRule{
			{
				TlsPassthrough: &ateapi.TlsPassthroughRule{
					Hostnames: []string{anthropicHostname},
					Ports:     &ateapi.Ports{Numbers: []int32{anthropicPort}},
				},
			},
			{
				Http: &ateapi.HttpRule{
					Hostnames: []string{host},
					Ports:     &ateapi.Ports{Numbers: []int32{int32(port)}},
				},
			},
		},
	}, nil
}

// checkEgressPolicy compares an already existing policy with the one Provision
// would create. Rules and their hostname lists are sets, so both are sorted
// before comparing.
func checkEgressPolicy(existing, want *ateapi.EgressPolicy, actor string) error {
	var errs []error
	if got, wantAts := existing.GetMetadata().GetAtespace(), want.GetMetadata().GetAtespace(); got != wantAts {
		errs = append(errs, fmt.Errorf("egress policy for actor %s: metadata.atespace = %q, want %q", actor, got, wantAts))
	}
	if got := existing.GetMetadata().GetName(); got != egressPolicyName {
		errs = append(errs, fmt.Errorf("egress policy for actor %s: metadata.name = %q, want %q", actor, got, egressPolicyName))
	}
	if got, wantRules := egressRuleKeys(existing.GetRules()), egressRuleKeys(want.GetRules()); !slices.Equal(got, wantRules) {
		errs = append(errs, fmt.Errorf("egress policy for actor %s: rules = %v, want %v", actor, got, wantRules))
	}
	return errors.Join(errs...)
}

// egressRuleKeys canonicalizes rules into sorted strings so that rule order
// and hostname order do not matter.
func egressRuleKeys(rules []*ateapi.EgressRule) []string {
	keys := make([]string, 0, len(rules))
	for _, rule := range rules {
		switch {
		case rule.GetHttp() != nil:
			keys = append(keys, egressRuleKey("http", rule.GetHttp().GetHostnames(), rule.GetHttp().GetPorts()))
		case rule.GetHttps() != nil:
			keys = append(keys, egressRuleKey("https", rule.GetHttps().GetHostnames(), rule.GetHttps().GetPorts()))
		case rule.GetTlsPassthrough() != nil:
			keys = append(keys, egressRuleKey("tls_passthrough", rule.GetTlsPassthrough().GetHostnames(), rule.GetTlsPassthrough().GetPorts()))
		default:
			keys = append(keys, "invalid")
		}
	}
	slices.Sort(keys)
	return keys
}

func egressRuleKey(protocol string, hostnames []string, ports *ateapi.Ports) string {
	hosts := slices.Clone(hostnames)
	slices.Sort(hosts)
	numbers := slices.Clone(ports.GetNumbers())
	slices.Sort(numbers)
	return fmt.Sprintf("%s hostnames=%s ports=%v", protocol, strings.Join(hosts, ","), numbers)
}

// resumeActor retries RESOURCE_EXHAUSTED and ABORTED with exponential backoff.
// Once the budget from the first call is spent it returns the last error with
// its gRPC code intact. Any other failure is checked against the actor's
// state, because a failed resume may have left it CRASHED.
func (m *actorManager) resumeActor(ctx context.Context, actor string) error {
	start := m.now()
	deadline := start.Add(resumeRetryBudget)
	delay := resumeRetryInitialDelay
	var lastErr error

	for {
		_, err := m.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: m.actorRef(actor)})
		if err == nil {
			return nil
		}
		code := status.Code(err)
		if code != codes.ResourceExhausted && code != codes.Aborted {
			return m.resumeFailure(ctx, actor, err)
		}
		lastErr = err

		now := m.now()
		if !now.Before(deadline) {
			// The retries may have raced with the actor crashing, so make the
			// same crash check as for any other failure before giving up.
			return m.resumeFailure(ctx, actor, fmt.Errorf("still failing after %s: %w", resumeRetryBudget, lastErr))
		}
		wait := delay
		if remaining := deadline.Sub(now); wait > remaining {
			wait = remaining
		}
		slog.Default().WarnContext(ctx, "resume actor failed; retrying",
			"actor", actor, "code", code, "delay", wait, "error", err)
		if !m.sleep(ctx, wait) {
			return fmt.Errorf("resume actor %s: %w", actor, ctx.Err())
		}
		if delay < resumeRetryMaxDelay {
			delay *= 2
			if delay > resumeRetryMaxDelay {
				delay = resumeRetryMaxDelay
			}
		}
	}
}

// resumeFailure inspects the actor after a non-retryable resume error. A
// CRASHED actor, which a wakeup-probe timeout produces, is reported as
// ErrActorCrashed while keeping the original gRPC error reachable through
// errors.Is and status.Code.
func (m *actorManager) resumeFailure(ctx context.Context, actor string, cause error) error {
	state, stateErr := m.actorState(ctx, actor)
	if stateErr == nil && state == ateapi.ActorState_ACTOR_STATE_CRASHED {
		return fmt.Errorf("%w: resume actor %s: %w", sandboxprovider.ErrActorCrashed, actor, cause)
	}
	return m.rpcError("resume actor", actor, cause)
}

func (m *actorManager) actorState(ctx context.Context, actor string) (ateapi.ActorState, error) {
	a, err := m.client.GetActor(ctx, &ateapi.GetActorRequest{Actor: m.actorRef(actor)})
	if err != nil {
		return ateapi.ActorState_ACTOR_STATE_UNSPECIFIED, err
	}
	return a.GetStatus().GetState(), nil
}

func (m *actorManager) actorRef(actor string) *ateapi.ObjectRef {
	return &ateapi.ObjectRef{Atespace: m.atespace, Name: actor}
}

// rpcError wraps an RPC failure. NOT_FOUND becomes ErrNotFound, as the
// provider contract requires outside Destroy; the original gRPC status stays
// reachable.
func (m *actorManager) rpcError(op, actor string, err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %s %s: %w", sandboxprovider.ErrNotFound, op, actor, err)
	}
	return fmt.Errorf("%s %s: %w", op, actor, err)
}

func mapActorState(state ateapi.ActorState) sandboxprovider.ActorState {
	switch state {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		return sandboxprovider.ActorStateRunning
	case ateapi.ActorState_ACTOR_STATE_RESUMING:
		return sandboxprovider.ActorStateResuming
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		return sandboxprovider.ActorStateSuspended
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		return sandboxprovider.ActorStateCrashed
	default:
		// SUSPENDING, DELETING, REVERTING, UNSPECIFIED and any value the
		// client does not know are transitional or unclassifiable.
		return sandboxprovider.ActorStateUnknown
	}
}

package runcontroller

import (
	"errors"
	"reflect"
	"testing"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var rulesNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func rulesTimePtr(t time.Time) *time.Time { return &t }

func rulesInt64Ptr(v int64) *int64 { return &v }

func rulesStringPtr(s string) *string { return &s }

func rulesEnv() machineEnv {
	return machineEnv{Now: rulesNow, StreamConnected: true}
}

func rulesSuspension(restoreReturnedAt time.Time) *machineSuspension {
	return &machineSuspension{RestoreReturnedAt: rulesTimePtr(restoreReturnedAt)}
}

// rulesCase 是各个事件表共用的断言结构。
type rulesCase struct {
	name           string
	cur            machineState
	ev             machineEvent
	env            machineEnv
	want           machineState
	effects        []machineEffect
	wantCode       codes.Code // 非 codes.OK 时要求返回该 gRPC 状态码
	wantTerminated bool       // 为真时要求返回 ErrTerminated
}

func rulesCheck(t *testing.T, cases []rulesCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := transition(tc.cur, tc.ev, tc.env)
			switch {
			case tc.wantTerminated:
				if !errors.Is(err, ErrTerminated) {
					t.Fatalf("error = %v, want ErrTerminated", err)
				}
				if !reflect.DeepEqual(got.State, tc.cur) {
					t.Fatalf("state = %#v, want unchanged %#v", got.State, tc.cur)
				}
			case tc.wantCode != codes.OK:
				if status.Code(err) != tc.wantCode {
					t.Fatalf("error = %v, want code %v", err, tc.wantCode)
				}
				if !reflect.DeepEqual(got.State, tc.cur) {
					t.Fatalf("state = %#v, want unchanged %#v", got.State, tc.cur)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(got.State, tc.want) {
					t.Fatalf("state = %#v, want %#v", got.State, tc.want)
				}
				if !reflect.DeepEqual(got.Effects, tc.effects) {
					t.Fatalf("effects = %#v, want %#v", got.Effects, tc.effects)
				}
			}
		})
	}
}

// TestTransitionAppend 覆盖“新消息与恢复”表的每一行。
func TestTransitionAppend(t *testing.T) {
	env := rulesEnv()
	noStream := machineEnv{Now: rulesNow}

	rulesCheck(t, []rulesCase{
		{
			name: "active/boundary delivers after commit",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2},
			effects: []machineEffect{
				insertInbox{seq: 1, text: "hi"},
				deliver{from: 1, through: 1},
			},
		},
		{
			name: "active/boundary without stream only writes",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			ev:   evAppend{text: "hi"},
			env:  noStream,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			effects: []machineEffect{
				insertInbox{seq: 1, text: "hi"},
			},
		},
		{
			name: "active/in_turn only writes",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3, DeliveredThrough: 1},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 4, DeliveredThrough: 1},
			effects: []machineEffect{
				insertInbox{seq: 3, text: "hi"},
			},
		},
		{
			name: "active/delivering only writes",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2, DeliveredThrough: 0},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, DeliveredThrough: 0},
			effects: []machineEffect{
				insertInbox{seq: 2, text: "hi"},
			},
		},
		{
			name: "suspending only writes",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 2},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateSuspending, NextInboxSeq: 3},
			effects: []machineEffect{
				insertInbox{seq: 2, text: "hi"},
			},
		},
		{
			name: "suspended resumes in the same transaction",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2, DeliveredThrough: 1},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 3, DeliveredThrough: 1},
			effects: []machineEffect{
				insertInbox{seq: 2, text: "hi"},
				updateSuspension{resumeRequestedAt: rulesTimePtr(rulesNow)},
				callRestore{},
			},
		},
		{
			name: "provisioning only writes",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 2},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateProvisioning, NextInboxSeq: 3},
			effects: []machineEffect{
				insertInbox{seq: 2, text: "hi"},
			},
		},
		{
			name: "resuming only writes",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:   evAppend{text: "hi"},
			env:  env,
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 3},
			effects: []machineEffect{
				insertInbox{seq: 2, text: "hi"},
			},
		},
		{
			name:           "ended rejects",
			cur:            machineState{State: StateEnded, NextInboxSeq: 2},
			ev:             evAppend{text: "hi"},
			env:            env,
			wantTerminated: true,
		},
		{
			name:           "failed rejects",
			cur:            machineState{State: StateFailed, NextInboxSeq: 2},
			ev:             evAppend{text: "hi"},
			env:            env,
			wantTerminated: true,
		},
	})
}

func TestTransitionProvisionDone(t *testing.T) {
	boom := errors.New("provision failed")
	rulesCheck(t, []rulesCase{
		{
			name: "success writes provision_returned_at",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1},
			ev:   evProvisionDone{},
			env:  rulesEnv(),
			want: machineState{State: StateProvisioning, NextInboxSeq: 1, ProvisionReturnedAt: rulesTimePtr(rulesNow)},
		},
		{
			name: "success while active keeps state",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2},
			ev:   evProvisionDone{},
			env:  rulesEnv(),
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)},
		},
		{
			name: "failure fails provisioning",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1},
			ev:   evProvisionDone{err: boom},
			env:  rulesEnv(),
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "provision failed",
				EndedAt:       rulesTimePtr(rulesNow),
			},
		},
		{
			name: "failure after hello does nothing",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			ev:   evProvisionDone{err: boom},
			env:  rulesEnv(),
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
		},
		{
			name: "completion after end does nothing",
			cur:  machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evProvisionDone{},
			env:  rulesEnv(),
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
		},
		{
			name: "completion after failure does nothing",
			cur:  machineState{State: StateFailed, NextInboxSeq: 1, FailureReason: "x", EndedAt: rulesTimePtr(rulesNow)},
			ev:   evProvisionDone{err: boom},
			env:  rulesEnv(),
			want: machineState{State: StateFailed, NextInboxSeq: 1, FailureReason: "x", EndedAt: rulesTimePtr(rulesNow)},
		},
	})
}

func TestTransitionHello(t *testing.T) {
	env := rulesEnv()
	restoreAt := rulesNow.Add(-2 * time.Second)

	rulesCheck(t, []rulesCase{
		{
			name: "provisioning starting enters boundary",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_STARTING, lastAcceptedSeq: 1},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "boundary hello records last turn result",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1},
			ev: evHello{
				phase:           agentprotocolv1.Phase_PHASE_BOUNDARY,
				lastAcceptedSeq: 0,
				lastTurn:        &agentprotocolv1.LastTurn{TurnId: "t1", Output: "done"},
			},
			env: env,
			want: machineState{
				State:        StateActive,
				Substate:     SubstateBoundary,
				NextInboxSeq: 1,
				IdleSince:    rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{
				turnEnded{turnID: "t1", output: "done", at: rulesNow},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "in_turn hello records started turn",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_IN_TURN, turnID: "t1", lastAcceptedSeq: 1},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, DeliveredThrough: 1},
			effects: []machineEffect{
				turnStarted{turnID: "t1", at: rulesNow},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "active boundary hello in_turn moves to in_turn",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_IN_TURN, turnID: "t1", lastAcceptedSeq: 0},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, DeliveredThrough: 1},
			effects: []machineEffect{
				turnStarted{turnID: "t1", at: rulesNow},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "delivered_through takes the larger value",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 4, DeliveredThrough: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_IN_TURN, lastAcceptedSeq: 1},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 4, DeliveredThrough: 2},
			effects: []machineEffect{
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "suspended hello only registers the stream",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_IN_TURN, turnID: "t1", lastAcceptedSeq: 1},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2, DeliveredThrough: 1},
			effects: []machineEffect{
				turnStarted{turnID: "t1", at: rulesNow},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "suspending hello only registers the stream",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 1},
			env:  env,
			want: machineState{State: StateSuspending, NextInboxSeq: 2, DeliveredThrough: 1},
			effects: []machineEffect{
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "resuming hello writes hello timing",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 1},
			env:  machineEnv{Now: rulesNow, StreamConnected: true, Suspension: rulesSuspension(restoreAt)},
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{helloAt: rulesTimePtr(rulesNow), helloMS: rulesInt64Ptr(2000)},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "resuming hello before restore returns leaves hello_ms unmeasured",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 1},
			env:  machineEnv{Now: rulesNow, StreamConnected: true, Suspension: rulesSuspension(rulesNow.Add(2 * time.Second))},
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{helloAt: rulesTimePtr(rulesNow)},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "resuming hello without suspension leaves hello_ms unmeasured",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 1},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{helloAt: rulesTimePtr(rulesNow)},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "resuming hello also records last turn",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev: evHello{
				phase:           agentprotocolv1.Phase_PHASE_BOUNDARY,
				lastAcceptedSeq: 1,
				lastTurn:        &agentprotocolv1.LastTurn{TurnId: "t9", Output: "x"},
			},
			env:  machineEnv{Now: rulesNow, StreamConnected: true, Suspension: rulesSuspension(restoreAt)},
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				turnEnded{turnID: "t9", output: "x", at: rulesNow},
				updateSuspension{helloAt: rulesTimePtr(rulesNow), helloMS: rulesInt64Ptr(2000)},
				registerStream{},
				sendHelloAck{},
			},
		},
		{
			name: "boundary hello delivers undelivered messages",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 0},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				registerStream{},
				sendHelloAck{},
				deliver{from: 1, through: 2},
			},
		},
		{
			name: "provisioning hello delivers without a registered stream",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 0},
			env:  machineEnv{Now: rulesNow},
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				registerStream{},
				sendHelloAck{},
				deliver{from: 1, through: 1},
			},
		},
		{
			name: "active boundary hello delivers after reconnecting",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 0},
			env:  machineEnv{Now: rulesNow},
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				registerStream{},
				sendHelloAck{},
				deliver{from: 1, through: 2},
			},
		},
		{
			name: "exited phase fails and closes the stream",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_EXITED, lastAcceptedSeq: 0},
			env:  env,
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "agent exited",
				EndedAt:       rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{
				closeStream{code: codes.FailedPrecondition},
			},
		},
		{
			name: "exited phase fails an active run",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2},
			ev:   evHello{phase: agentprotocolv1.Phase_PHASE_EXITED, lastAcceptedSeq: 1},
			env:  env,
			want: machineState{
				State:            StateFailed,
				NextInboxSeq:     2,
				DeliveredThrough: 1,
				FailureReason:    "agent exited",
				EndedAt:          rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{
				closeStream{code: codes.FailedPrecondition},
			},
		},
		{
			name:     "negative last_accepted_seq is a protocol violation",
			cur:      machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			ev:       evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: -1},
			env:      env,
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "last_accepted_seq beyond allocated is a protocol violation",
			cur:      machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			ev:       evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 2},
			env:      env,
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "unspecified phase is a protocol violation",
			cur:      machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			ev:       evHello{phase: agentprotocolv1.Phase_PHASE_UNSPECIFIED},
			env:      env,
			wantCode: codes.InvalidArgument,
		},
		{
			name:           "ended rejects hello",
			cur:            machineState{State: StateEnded, NextInboxSeq: 2},
			ev:             evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY},
			env:            env,
			wantTerminated: true,
		},
		{
			name:           "failed rejects hello",
			cur:            machineState{State: StateFailed, NextInboxSeq: 2},
			ev:             evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY},
			env:            env,
			wantTerminated: true,
		},
	})
}

func TestTransitionAccepted(t *testing.T) {
	env := rulesEnv()
	rulesCheck(t, []rulesCase{
		{
			name: "delivering becomes in_turn",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, DeliveredThrough: 1},
			ev:   evAccepted{throughSeq: 2},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3, DeliveredThrough: 2},
		},
		{
			name: "boundary keeps its substate",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, DeliveredThrough: 1},
			ev:   evAccepted{throughSeq: 2},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, DeliveredThrough: 2},
		},
		{
			name: "in_turn keeps its substate",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3, DeliveredThrough: 1},
			ev:   evAccepted{throughSeq: 2},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3, DeliveredThrough: 2},
		},
		{
			name: "non active state still advances the position",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:   evAccepted{throughSeq: 1},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2, DeliveredThrough: 1},
		},
		{
			name: "delivered_through takes the larger value",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 4, DeliveredThrough: 3},
			ev:   evAccepted{throughSeq: 1},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 4, DeliveredThrough: 3},
		},
		{
			name:     "zero through_seq is a protocol violation",
			cur:      machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3},
			ev:       evAccepted{throughSeq: 0},
			env:      env,
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "through_seq beyond allocated is a protocol violation",
			cur:      machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3},
			ev:       evAccepted{throughSeq: 3},
			env:      env,
			wantCode: codes.InvalidArgument,
		},
		{
			name:           "ended rejects accepted",
			cur:            machineState{State: StateEnded, NextInboxSeq: 2},
			ev:             evAccepted{throughSeq: 1},
			env:            env,
			wantTerminated: true,
		},
	})
}

func TestTransitionTurnStarted(t *testing.T) {
	env := rulesEnv()
	started := []machineEffect{turnStarted{turnID: "t1", deliveryID: "", at: rulesNow}}

	rulesCheck(t, []rulesCase{
		{
			name:    "boundary starts a turn",
			cur:     machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			ev:      evTurnStarted{turnID: "t1"},
			env:     env,
			want:    machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2},
			effects: started,
		},
		{
			name:    "delivering only records the turn",
			cur:     machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2},
			ev:      evTurnStarted{turnID: "t1"},
			env:     env,
			want:    machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2},
			effects: started,
		},
		{
			name:    "in_turn keeps its substate",
			cur:     machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2},
			ev:      evTurnStarted{turnID: "t1"},
			env:     env,
			want:    machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2},
			effects: started,
		},
		{
			name:    "non active state only records the turn",
			cur:     machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			ev:      evTurnStarted{turnID: "t1"},
			env:     env,
			want:    machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			effects: started,
		},
		{
			name:           "ended rejects turn_started",
			cur:            machineState{State: StateEnded, NextInboxSeq: 2},
			ev:             evTurnStarted{turnID: "t1"},
			env:            env,
			wantTerminated: true,
		},
	})
}

func TestTransitionTurnEnded(t *testing.T) {
	env := rulesEnv()
	noStream := machineEnv{Now: rulesNow}
	endedEffect := turnEnded{turnID: "t1", output: "done", at: rulesNow}

	rulesCheck(t, []rulesCase{
		{
			name: "in_turn returns to the boundary",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 1},
			ev:   evTurnEnded{turnID: "t1", output: "done"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				endedEffect,
			},
		},
		{
			name: "in_turn with undelivered messages delivers",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3},
			ev:   evTurnEnded{turnID: "t1", output: "done"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				endedEffect,
				deliver{from: 1, through: 2},
			},
		},
		{
			name: "in_turn without a stream stays at the boundary",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3},
			ev:   evTurnEnded{turnID: "t1", output: "done"},
			env:  noStream,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				endedEffect,
			},
		},
		{
			name: "delivering waits for accepted",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3},
			ev:   evTurnEnded{turnID: "t1", output: "done"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3},
			effects: []machineEffect{
				endedEffect,
			},
		},
		{
			name: "boundary keeps its substate",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			ev:   evTurnEnded{turnID: "t1", output: "done"},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			effects: []machineEffect{
				endedEffect,
			},
		},
		{
			name: "agent exit in in_turn fails",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 1},
			ev:   evTurnEnded{turnID: "t1", output: "done", agentExited: true},
			env:  env,
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "agent exited",
				EndedAt:       rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{endedEffect},
		},
		{
			name: "agent exit in delivering fails",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 1},
			ev:   evTurnEnded{turnID: "t1", output: "done", agentExited: true},
			env:  env,
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "agent exited",
				EndedAt:       rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{endedEffect},
		},
		{
			name: "agent exit in boundary fails",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			ev:   evTurnEnded{turnID: "t1", output: "done", agentExited: true},
			env:  env,
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "agent exited",
				EndedAt:       rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{endedEffect},
		},
		{
			name: "non active state only records the result",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evTurnEnded{turnID: "t1", output: "done"},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			effects: []machineEffect{
				endedEffect,
			},
		},
		{
			name:           "ended rejects turn_ended",
			cur:            machineState{State: StateEnded, NextInboxSeq: 1},
			ev:             evTurnEnded{turnID: "t1", output: "done"},
			env:            env,
			wantTerminated: true,
		},
	})
}

// TestTransitionSpontaneousTurn 覆盖接口清单“自发回合”一节的帧序列。
func TestTransitionSpontaneousTurn(t *testing.T) {
	env := rulesEnv()

	// 边界上收到空 delivery_id 的 turn_started 直接进入回合。
	boundary := machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1}
	got, err := transition(boundary, evTurnStarted{turnID: "t1"}, env)
	if err != nil {
		t.Fatalf("turn_started: unexpected error: %v", err)
	}
	if got.State.Substate != SubstateInTurn {
		t.Fatalf("substate = %v, want in_turn", got.State.Substate)
	}

	// delivering 下依次收到 turn_started（空 delivery_id）、Accepted、turn_ended。
	cur := machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, DeliveredThrough: 2}
	startEffects := 0

	got, err = transition(cur, evTurnStarted{turnID: "t1", deliveryID: ""}, env)
	if err != nil {
		t.Fatalf("turn_started: unexpected error: %v", err)
	}
	if got.State.Substate != SubstateDelivering {
		t.Fatalf("after spontaneous turn_started substate = %v, want delivering", got.State.Substate)
	}
	for _, e := range got.Effects {
		if _, ok := e.(turnStarted); ok {
			startEffects++
		}
	}
	if startEffects != 1 {
		t.Fatalf("turn_started effects = %d, want 1", startEffects)
	}

	got, err = transition(got.State, evAccepted{throughSeq: 2}, env)
	if err != nil {
		t.Fatalf("accepted: unexpected error: %v", err)
	}
	if got.State.Substate != SubstateInTurn {
		t.Fatalf("after accepted substate = %v, want in_turn", got.State.Substate)
	}

	got, err = transition(got.State, evTurnEnded{turnID: "t1"}, env)
	if err != nil {
		t.Fatalf("turn_ended: unexpected error: %v", err)
	}
	if got.State.State != StateActive || got.State.Substate != SubstateBoundary {
		t.Fatalf("after turn_ended state = %v/%v, want active/boundary", got.State.State, got.State.Substate)
	}
	if len(got.Effects) != 1 {
		t.Fatalf("turn_ended effects = %#v, want a single turnEnded", got.Effects)
	}
	if _, ok := got.Effects[0].(turnEnded); !ok {
		t.Fatalf("turn_ended effect = %T, want turnEnded", got.Effects[0])
	}
}

func TestTransitionStreamClosed(t *testing.T) {
	env := rulesEnv()
	rulesCheck(t, []rulesCase{
		{
			name: "delivering falls back to boundary",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3},
			ev:   evStreamClosed{},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				unregisterStream{},
			},
		},
		{
			name: "boundary is unchanged",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3},
			ev:   evStreamClosed{},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3},
			effects: []machineEffect{
				unregisterStream{},
			},
		},
		{
			name: "in_turn with undelivered messages is unchanged",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3},
			ev:   evStreamClosed{},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3},
			effects: []machineEffect{
				unregisterStream{},
			},
		},
		{
			name: "suspended is unchanged",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 3},
			ev:   evStreamClosed{},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 3},
			effects: []machineEffect{
				unregisterStream{},
			},
		},
		{
			name: "ended only unregisters",
			cur:  machineState{State: StateEnded, NextInboxSeq: 3, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evStreamClosed{},
			env:  env,
			want: machineState{State: StateEnded, NextInboxSeq: 3, EndedAt: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				unregisterStream{},
			},
		},
	})
}

func TestTransitionEnd(t *testing.T) {
	env := rulesEnv()
	noStream := machineEnv{Now: rulesNow}
	endEffects := []machineEffect{
		sendShutdown{},
		closeStream{code: codes.FailedPrecondition},
		callDestroy{},
	}

	rulesCheck(t, []rulesCase{
		{
			name:    "active boundary ends and closes the stream",
			cur:     machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2},
			ev:      evEnd{},
			env:     env,
			want:    machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)},
			effects: endEffects,
		},
		{
			name: "active in_turn without a stream only destroys",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2},
			ev:   evEnd{},
			env:  noStream,
			want: machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				callDestroy{},
			},
		},
		{
			name:    "provisioning ends",
			cur:     machineState{State: StateProvisioning, NextInboxSeq: 1},
			ev:      evEnd{},
			env:     env,
			want:    machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			effects: endEffects,
		},
		{
			name: "suspended ends",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evEnd{},
			env:  noStream,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				callDestroy{},
			},
		},
		{
			name:    "suspending ends",
			cur:     machineState{State: StateSuspending, NextInboxSeq: 1},
			ev:      evEnd{},
			env:     env,
			want:    machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			effects: endEffects,
		},
		{
			name: "resuming ends",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evEnd{},
			env:  noStream,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				callDestroy{},
			},
		},
		{
			name: "failed keeps its state and destroys the actor",
			cur:  machineState{State: StateFailed, NextInboxSeq: 1, FailureReason: "boom", EndedAt: rulesTimePtr(rulesNow)},
			ev:   evEnd{},
			env:  env,
			want: machineState{State: StateFailed, NextInboxSeq: 1, FailureReason: "boom", EndedAt: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				callDestroy{},
			},
		},
		{
			name: "ended does nothing",
			cur:  machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evEnd{},
			env:  env,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
		},
	})
}

func TestTransitionTrySuspend(t *testing.T) {
	env := rulesEnv()
	idleBefore := rulesNow.Add(-30 * time.Second)
	before := idleBefore.Add(-time.Second)
	atBoundary := idleBefore
	after := idleBefore.Add(time.Second)

	rulesCheck(t, []rulesCase{
		{
			name: "idle boundary suspends",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: &before},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateSuspending, NextInboxSeq: 1, IdleSince: &before},
			effects: []machineEffect{
				openSuspension{at: rulesNow},
				callSnapshot{},
			},
		},
		{
			name: "undelivered messages prevent suspension",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, DeliveredThrough: 1, IdleSince: &before},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, DeliveredThrough: 1, IdleSince: &before},
		},
		{
			name: "in_turn does not suspend",
			cur:  machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 1, IdleSince: &before},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 1, IdleSince: &before},
		},
		{
			name: "delivering does not suspend",
			cur:  machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 1, IdleSince: &before},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 1, IdleSince: &before},
		},
		{
			name: "exactly at the deadline does not suspend",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: &atBoundary},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: &atBoundary},
		},
		{
			name: "not idle long enough does not suspend",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: &after},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: &after},
		},
		{
			name: "missing idle_since does not suspend",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
		},
		{
			name: "suspended does not suspend again",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "ended is a no-op",
			cur:  machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evTrySuspend{idleBefore: idleBefore},
			env:  env,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
		},
	})
}

func TestTransitionSnapshotDone(t *testing.T) {
	env := rulesEnv()
	noStream := machineEnv{Now: rulesNow}
	boom := errors.New("snapshot failed")
	timing := sandboxprovider.Timing{Duration: 1500 * time.Millisecond, SnapshotBytes: 4096}

	rulesCheck(t, []rulesCase{
		{
			name: "success without undelivered messages suspends",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 1},
			ev:   evSnapshotDone{timing: timing},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			effects: []machineEffect{
				updateSuspension{snapshotMS: rulesInt64Ptr(1500), snapshotBytes: rulesInt64Ptr(4096)},
			},
		},
		{
			name: "success with undelivered messages resumes immediately",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 3},
			ev:   evSnapshotDone{timing: timing},
			env:  env,
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 3},
			effects: []machineEffect{
				updateSuspension{
					snapshotMS:        rulesInt64Ptr(1500),
					snapshotBytes:     rulesInt64Ptr(4096),
					resumeRequestedAt: rulesTimePtr(rulesNow),
				},
				callRestore{},
			},
		},
		{
			name: "failure returns to the boundary",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 1},
			ev:   evSnapshotDone{err: boom},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{err: rulesStringPtr("snapshot failed")},
			},
		},
		{
			name: "failure with undelivered messages delivers",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 3},
			ev:   evSnapshotDone{err: boom},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{err: rulesStringPtr("snapshot failed")},
				deliver{from: 1, through: 2},
			},
		},
		{
			name: "failure without a stream stays at the boundary",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 3},
			ev:   evSnapshotDone{err: boom},
			env:  noStream,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{err: rulesStringPtr("snapshot failed")},
			},
		},
		{
			name: "crashed actor fails the run",
			cur:  machineState{State: StateSuspending, NextInboxSeq: 1},
			ev:   evSnapshotDone{err: boom, crashed: true},
			env:  env,
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "snapshot failed",
				EndedAt:       rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{
				updateSuspension{err: rulesStringPtr("snapshot failed")},
			},
		},
		{
			name: "stale completion outside suspending does nothing",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
			ev:   evSnapshotDone{timing: timing},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1},
		},
		{
			name: "completion after end does nothing",
			cur:  machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evSnapshotDone{err: boom},
			env:  env,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
		},
	})
}

func TestTransitionRestoreDone(t *testing.T) {
	env := rulesEnv()
	boom := errors.New("restore failed")
	timing := sandboxprovider.Timing{Duration: 2500 * time.Millisecond}

	rulesCheck(t, []rulesCase{
		{
			name: "success records restore timing",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evRestoreDone{timing: timing},
			env:  env,
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			effects: []machineEffect{
				updateSuspension{restoreMS: rulesInt64Ptr(2500), restoreReturnedAt: rulesTimePtr(rulesNow)},
			},
		},
		{
			name: "success after hello keeps active",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: rulesTimePtr(rulesNow)},
			ev:   evRestoreDone{timing: timing},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: rulesTimePtr(rulesNow)},
			effects: []machineEffect{
				updateSuspension{restoreMS: rulesInt64Ptr(2500), restoreReturnedAt: rulesTimePtr(rulesNow)},
			},
		},
		{
			name: "failure fails the run",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evRestoreDone{err: boom},
			env:  env,
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "restore failed",
				EndedAt:       rulesTimePtr(rulesNow),
			},
			effects: []machineEffect{
				updateSuspension{err: rulesStringPtr("restore failed")},
			},
		},
		{
			name: "stale completion outside resuming does nothing",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evRestoreDone{timing: timing},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "completion after end does nothing",
			cur:  machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evRestoreDone{err: boom},
			env:  env,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
		},
	})
}

func TestTransitionHelloExpire(t *testing.T) {
	env := rulesEnv()
	returnedBefore := rulesNow.Add(-60 * time.Second)
	expired := returnedBefore.Add(-time.Second)
	notExpired := returnedBefore
	expiredSuspension := &machineSuspension{RestoreReturnedAt: &expired}
	helloReceivedAt := expired.Add(time.Second)

	rulesCheck(t, []rulesCase{
		{
			name: "provisioning with an expired provision fails",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1, ProvisionReturnedAt: &expired},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{
				State:               StateFailed,
				NextInboxSeq:        1,
				ProvisionReturnedAt: &expired,
				FailureReason:       "hello timeout",
				EndedAt:             rulesTimePtr(rulesNow),
			},
		},
		{
			name: "provisioning exactly at the deadline does not fail",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1, ProvisionReturnedAt: &notExpired},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{State: StateProvisioning, NextInboxSeq: 1, ProvisionReturnedAt: &notExpired},
		},
		{
			name: "provisioning without a returned time does not fail",
			cur:  machineState{State: StateProvisioning, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{State: StateProvisioning, NextInboxSeq: 1},
		},
		{
			name: "resuming with an expired restore fails",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  machineEnv{Now: rulesNow, StreamConnected: true, Suspension: expiredSuspension},
			want: machineState{
				State:         StateFailed,
				NextInboxSeq:  1,
				FailureReason: "hello timeout",
				EndedAt:       rulesTimePtr(rulesNow),
			},
		},
		{
			name: "resuming without a suspension record does not fail",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "resuming without a restore returned time does not fail",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  machineEnv{Now: rulesNow, StreamConnected: true, Suspension: &machineSuspension{}},
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "resuming exactly at the deadline does not fail",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  machineEnv{Now: rulesNow, StreamConnected: true, Suspension: &machineSuspension{RestoreReturnedAt: &notExpired}},
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "resuming that already received hello does not fail",
			cur:  machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env: machineEnv{Now: rulesNow, StreamConnected: true, Suspension: &machineSuspension{
				RestoreReturnedAt: &expired,
				HelloAt:           &helloReceivedAt,
			}},
			want: machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "active does nothing",
			cur:  machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: rulesTimePtr(rulesNow)},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 1, IdleSince: rulesTimePtr(rulesNow)},
		},
		{
			name: "suspended does nothing",
			cur:  machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 1},
		},
		{
			name: "ended does nothing",
			cur:  machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
			ev:   evHelloExpire{returnedBefore: returnedBefore},
			env:  env,
			want: machineState{State: StateEnded, NextInboxSeq: 1, EndedAt: rulesTimePtr(rulesNow)},
		},
	})
}

// TestTransitionTerminated 检查终态上各事件的约定。
func TestTransitionTerminated(t *testing.T) {
	for _, st := range []State{StateEnded, StateFailed} {
		t.Run(string(st), func(t *testing.T) {
			cur := machineState{State: st, NextInboxSeq: 2, FailureReason: "x", EndedAt: rulesTimePtr(rulesNow)}
			env := rulesEnv()

			rejected := []machineEvent{
				evAppend{text: "hi"},
				evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 1},
				evAccepted{throughSeq: 1},
				evTurnStarted{turnID: "t1"},
				evTurnEnded{turnID: "t1"},
			}
			for _, ev := range rejected {
				got, err := transition(cur, ev, env)
				if !errors.Is(err, ErrTerminated) {
					t.Fatalf("%T: error = %v, want ErrTerminated", ev, err)
				}
				if !reflect.DeepEqual(got.State, cur) {
					t.Fatalf("%T: state changed to %#v", ev, got.State)
				}
			}

			ignored := []machineEvent{
				evProvisionDone{},
				evProvisionDone{err: errors.New("late")},
				evSnapshotDone{},
				evRestoreDone{},
				evStreamClosed{},
				evTrySuspend{idleBefore: rulesNow},
				evHelloExpire{returnedBefore: rulesNow},
			}
			for _, ev := range ignored {
				got, err := transition(cur, ev, env)
				if err != nil {
					t.Fatalf("%T: unexpected error: %v", ev, err)
				}
				if !reflect.DeepEqual(got.State, cur) {
					t.Fatalf("%T: state changed to %#v", ev, got.State)
				}
			}
		})
	}
}

// TestTransitionStateMatrix 对每个（状态，子状态，事件）组合断言与状态机图
// 一致的确切结果：新的状态与按执行顺序排列的效果。不在迁移图上的转移要么
// 在当前状态下无事可做（状态不变、无效果），要么在终态上返回 ErrTerminated。
func TestTransitionStateMatrix(t *testing.T) {
	restoreAt := rulesNow.Add(-time.Second)
	env := machineEnv{
		Now:             rulesNow,
		StreamConnected: true,
		Suspension:      rulesSuspension(restoreAt),
	}

	prov := machineState{State: StateProvisioning, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)}
	inTurn := machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2}
	delivering := machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2}
	boundary := machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, IdleSince: rulesTimePtr(rulesNow)}
	suspending := machineState{State: StateSuspending, NextInboxSeq: 2}
	suspended := machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2}
	resuming := machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2}
	ended := machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}
	failed := machineState{State: StateFailed, NextInboxSeq: 2, FailureReason: "x", EndedAt: rulesTimePtr(rulesNow)}

	states := []struct {
		name string
		cur  machineState
	}{
		{"provisioning", prov},
		{"active/in_turn", inTurn},
		{"active/delivering", delivering},
		{"active/boundary", boundary},
		{"suspending", suspending},
		{"suspended", suspended},
		{"resuming", resuming},
		{"ended", ended},
		{"failed", failed},
	}

	events := []struct {
		name string
		ev   machineEvent
	}{
		{"append", evAppend{text: "hi"}},
		{"provision_done", evProvisionDone{}},
		{"hello", evHello{phase: agentprotocolv1.Phase_PHASE_BOUNDARY, lastAcceptedSeq: 1}},
		{"accepted", evAccepted{throughSeq: 1}},
		{"turn_started", evTurnStarted{turnID: "t1"}},
		{"turn_ended", evTurnEnded{turnID: "t1"}},
		{"stream_closed", evStreamClosed{}},
		{"end", evEnd{}},
		{"try_suspend", evTrySuspend{idleBefore: rulesNow.Add(-30 * time.Second)}},
		{"snapshot_done", evSnapshotDone{timing: sandboxprovider.Timing{Duration: time.Second}}},
		{"restore_done", evRestoreDone{timing: sandboxprovider.Timing{Duration: time.Second}}},
		{"hello_expire", evHelloExpire{returnedBefore: rulesNow.Add(-60 * time.Second)}},
	}

	type matrixKey struct {
		state    State
		substate Substate
		event    string
	}
	type matrixWant struct {
		state    machineState
		effects  []machineEffect
		rejected bool
	}
	mk := func(s machineState, event string) matrixKey {
		return matrixKey{state: s.State, substate: s.Substate, event: event}
	}
	w := func(s machineState, effects ...machineEffect) matrixWant {
		return matrixWant{state: s, effects: effects}
	}
	rejected := matrixWant{rejected: true}

	endEffects := []machineEffect{
		sendShutdown{},
		closeStream{code: codes.FailedPrecondition},
		callDestroy{},
	}
	helloAck := []machineEffect{registerStream{}, sendHelloAck{}}
	eventEffects := func(effects ...machineEffect) []machineEffect { return effects }

	want := map[matrixKey]matrixWant{
		// provisioning
		mk(prov, "append"):         w(machineState{State: StateProvisioning, NextInboxSeq: 3, ProvisionReturnedAt: rulesTimePtr(rulesNow)}, insertInbox{seq: 2, text: "hi"}),
		mk(prov, "provision_done"): w(prov),
		mk(prov, "hello"):          w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, ProvisionReturnedAt: rulesTimePtr(rulesNow), IdleSince: rulesTimePtr(rulesNow)}, helloAck...),
		mk(prov, "accepted"):       w(machineState{State: StateProvisioning, NextInboxSeq: 2, DeliveredThrough: 1, ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(prov, "turn_started"):   w(prov, turnStarted{turnID: "t1", at: rulesNow}),
		mk(prov, "turn_ended"):     w(prov, turnEnded{turnID: "t1", at: rulesNow}),
		mk(prov, "stream_closed"):  w(prov, unregisterStream{}),
		mk(prov, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow), EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(prov, "try_suspend"):    w(prov),
		mk(prov, "snapshot_done"):  w(prov),
		mk(prov, "restore_done"):   w(prov),
		mk(prov, "hello_expire"):   w(prov),

		// active/in_turn
		mk(inTurn, "append"):         w(machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 3}, insertInbox{seq: 2, text: "hi"}),
		mk(inTurn, "provision_done"): w(machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(inTurn, "hello"):          w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)}, helloAck...),
		mk(inTurn, "accepted"):       w(machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, DeliveredThrough: 1}),
		mk(inTurn, "turn_started"):   w(inTurn, turnStarted{turnID: "t1", at: rulesNow}),
		mk(inTurn, "turn_ended"):     w(machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2, IdleSince: rulesTimePtr(rulesNow)}, turnEnded{turnID: "t1", at: rulesNow}, deliver{from: 1, through: 1}),
		mk(inTurn, "stream_closed"):  w(inTurn, unregisterStream{}),
		mk(inTurn, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(inTurn, "try_suspend"):    w(inTurn),
		mk(inTurn, "snapshot_done"):  w(inTurn),
		mk(inTurn, "restore_done"):   w(inTurn, updateSuspension{restoreMS: rulesInt64Ptr(1000), restoreReturnedAt: rulesTimePtr(rulesNow)}),
		mk(inTurn, "hello_expire"):   w(inTurn),

		// active/delivering
		mk(delivering, "append"):         w(machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3}, insertInbox{seq: 2, text: "hi"}),
		mk(delivering, "provision_done"): w(machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(delivering, "hello"):          w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)}, helloAck...),
		mk(delivering, "accepted"):       w(machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, DeliveredThrough: 1}),
		mk(delivering, "turn_started"):   w(delivering, turnStarted{turnID: "t1", at: rulesNow}),
		mk(delivering, "turn_ended"):     w(delivering, turnEnded{turnID: "t1", at: rulesNow}),
		mk(delivering, "stream_closed"):  w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, IdleSince: rulesTimePtr(rulesNow)}, unregisterStream{}),
		mk(delivering, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(delivering, "try_suspend"):    w(delivering),
		mk(delivering, "snapshot_done"):  w(delivering),
		mk(delivering, "restore_done"):   w(delivering, updateSuspension{restoreMS: rulesInt64Ptr(1000), restoreReturnedAt: rulesTimePtr(rulesNow)}),
		mk(delivering, "hello_expire"):   w(delivering),

		// active/boundary
		mk(boundary, "append"):         w(machineState{State: StateActive, Substate: SubstateDelivering, NextInboxSeq: 3, IdleSince: rulesTimePtr(rulesNow)}, insertInbox{seq: 2, text: "hi"}, deliver{from: 1, through: 2}),
		mk(boundary, "provision_done"): w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, IdleSince: rulesTimePtr(rulesNow), ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(boundary, "hello"):          w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)}, helloAck...),
		mk(boundary, "accepted"):       w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)}),
		mk(boundary, "turn_started"):   w(machineState{State: StateActive, Substate: SubstateInTurn, NextInboxSeq: 2, IdleSince: rulesTimePtr(rulesNow)}, turnStarted{turnID: "t1", at: rulesNow}),
		mk(boundary, "turn_ended"):     w(boundary, turnEnded{turnID: "t1", at: rulesNow}),
		mk(boundary, "stream_closed"):  w(boundary, unregisterStream{}),
		mk(boundary, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(boundary, "try_suspend"):    w(boundary),
		mk(boundary, "snapshot_done"):  w(boundary),
		mk(boundary, "restore_done"):   w(boundary, updateSuspension{restoreMS: rulesInt64Ptr(1000), restoreReturnedAt: rulesTimePtr(rulesNow)}),
		mk(boundary, "hello_expire"):   w(boundary),

		// suspending
		mk(suspending, "append"):         w(machineState{State: StateSuspending, NextInboxSeq: 3}, insertInbox{seq: 2, text: "hi"}),
		mk(suspending, "provision_done"): w(machineState{State: StateSuspending, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(suspending, "hello"):          w(machineState{State: StateSuspending, NextInboxSeq: 2, DeliveredThrough: 1}, helloAck...),
		mk(suspending, "accepted"):       w(machineState{State: StateSuspending, NextInboxSeq: 2, DeliveredThrough: 1}),
		mk(suspending, "turn_started"):   w(suspending, turnStarted{turnID: "t1", at: rulesNow}),
		mk(suspending, "turn_ended"):     w(suspending, turnEnded{turnID: "t1", at: rulesNow}),
		mk(suspending, "stream_closed"):  w(suspending, unregisterStream{}),
		mk(suspending, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(suspending, "try_suspend"):    w(suspending),
		mk(suspending, "snapshot_done"): w(machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2},
			updateSuspension{snapshotMS: rulesInt64Ptr(1000), snapshotBytes: rulesInt64Ptr(0), resumeRequestedAt: rulesTimePtr(rulesNow)},
			callRestore{}),
		mk(suspending, "restore_done"): w(suspending),
		mk(suspending, "hello_expire"): w(suspending),

		// suspended
		mk(suspended, "append"): w(machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 3},
			insertInbox{seq: 2, text: "hi"},
			updateSuspension{resumeRequestedAt: rulesTimePtr(rulesNow)},
			callRestore{}),
		mk(suspended, "provision_done"): w(machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(suspended, "hello"):          w(machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2, DeliveredThrough: 1}, helloAck...),
		mk(suspended, "accepted"):       w(machineState{State: StateSuspended, Substate: SubstateSnapshotted, NextInboxSeq: 2, DeliveredThrough: 1}),
		mk(suspended, "turn_started"):   w(suspended, turnStarted{turnID: "t1", at: rulesNow}),
		mk(suspended, "turn_ended"):     w(suspended, turnEnded{turnID: "t1", at: rulesNow}),
		mk(suspended, "stream_closed"):  w(suspended, unregisterStream{}),
		mk(suspended, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(suspended, "try_suspend"):    w(suspended),
		mk(suspended, "snapshot_done"):  w(suspended),
		mk(suspended, "restore_done"):   w(suspended),
		mk(suspended, "hello_expire"):   w(suspended),

		// resuming
		mk(resuming, "append"):         w(machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 3}, insertInbox{seq: 2, text: "hi"}),
		mk(resuming, "provision_done"): w(machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2, ProvisionReturnedAt: rulesTimePtr(rulesNow)}),
		mk(resuming, "hello"):          w(machineState{State: StateActive, Substate: SubstateBoundary, NextInboxSeq: 2, DeliveredThrough: 1, IdleSince: rulesTimePtr(rulesNow)}, eventEffects(updateSuspension{helloAt: rulesTimePtr(rulesNow), helloMS: rulesInt64Ptr(1000)}, registerStream{}, sendHelloAck{})...),
		mk(resuming, "accepted"):       w(machineState{State: StateResuming, Substate: SubstateSnapshotted, NextInboxSeq: 2, DeliveredThrough: 1}),
		mk(resuming, "turn_started"):   w(resuming, turnStarted{turnID: "t1", at: rulesNow}),
		mk(resuming, "turn_ended"):     w(resuming, turnEnded{turnID: "t1", at: rulesNow}),
		mk(resuming, "stream_closed"):  w(resuming, unregisterStream{}),
		mk(resuming, "end"):            w(machineState{State: StateEnded, NextInboxSeq: 2, EndedAt: rulesTimePtr(rulesNow)}, endEffects...),
		mk(resuming, "try_suspend"):    w(resuming),
		mk(resuming, "snapshot_done"):  w(resuming),
		mk(resuming, "restore_done"):   w(resuming, updateSuspension{restoreMS: rulesInt64Ptr(1000), restoreReturnedAt: rulesTimePtr(rulesNow)}),
		mk(resuming, "hello_expire"):   w(resuming),

		// ended
		mk(ended, "append"):         rejected,
		mk(ended, "provision_done"): w(ended),
		mk(ended, "hello"):          rejected,
		mk(ended, "accepted"):       rejected,
		mk(ended, "turn_started"):   rejected,
		mk(ended, "turn_ended"):     rejected,
		mk(ended, "stream_closed"):  w(ended, unregisterStream{}),
		mk(ended, "end"):            w(ended),
		mk(ended, "try_suspend"):    w(ended),
		mk(ended, "snapshot_done"):  w(ended),
		mk(ended, "restore_done"):   w(ended),
		mk(ended, "hello_expire"):   w(ended),

		// failed
		mk(failed, "append"):         rejected,
		mk(failed, "provision_done"): w(failed),
		mk(failed, "hello"):          rejected,
		mk(failed, "accepted"):       rejected,
		mk(failed, "turn_started"):   rejected,
		mk(failed, "turn_ended"):     rejected,
		mk(failed, "stream_closed"):  w(failed, unregisterStream{}),
		mk(failed, "end"):            w(failed, callDestroy{}),
		mk(failed, "try_suspend"):    w(failed),
		mk(failed, "snapshot_done"):  w(failed),
		mk(failed, "restore_done"):   w(failed),
		mk(failed, "hello_expire"):   w(failed),
	}

	if len(want) != len(states)*len(events) {
		t.Fatalf("expectation table has %d entries, want %d", len(want), len(states)*len(events))
	}

	for _, st := range states {
		for _, ev := range events {
			t.Run(st.name+"/"+ev.name, func(t *testing.T) {
				got, err := transition(st.cur, ev.ev, env)
				exp := want[mk(st.cur, ev.name)]
				if exp.rejected {
					if !errors.Is(err, ErrTerminated) {
						t.Fatalf("error = %v, want ErrTerminated", err)
					}
					if !reflect.DeepEqual(got.State, st.cur) {
						t.Fatalf("state = %#v, want unchanged %#v", got.State, st.cur)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(got.State, exp.state) {
					t.Fatalf("state = %#v, want %#v", got.State, exp.state)
				}
				if !reflect.DeepEqual(got.Effects, exp.effects) {
					t.Fatalf("effects = %#v, want %#v", got.Effects, exp.effects)
				}
			})
		}
	}
}

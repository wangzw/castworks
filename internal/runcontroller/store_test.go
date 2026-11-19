package runcontroller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newTestStore(t *testing.T) (*store, *pgxpool.Pool) {
	t.Helper()
	pool := templatePool(t)
	return newStore(pool), pool
}

func seedRun(t *testing.T, st *store, runID string, createdAt time.Time) runRow {
	t.Helper()
	r := runRow{
		RunID:        runID,
		State:        StateProvisioning,
		Substate:     SubstateNone,
		ActorName:    "actor-" + runID,
		NextInboxSeq: 2,
		CreatedAt:    createdAt,
	}
	if err := st.inTx(t.Context(), func(tx *storeTx) error {
		return tx.insertRun(r, "first message", createdAt)
	}); err != nil {
		t.Fatalf("insert run %s: %v", runID, err)
	}
	return r
}

func updateRunState(t *testing.T, st *store, runID string, mutate func(*runRow)) runRow {
	t.Helper()
	var updated runRow
	if err := st.inTx(t.Context(), func(tx *storeTx) error {
		r, err := tx.lockRun(runID)
		if err != nil {
			return err
		}
		mutate(&r)
		if err := tx.updateRun(r); err != nil {
			return err
		}
		updated = r
		return nil
	}); err != nil {
		t.Fatalf("update run %s: %v", runID, err)
	}
	return updated
}

func openAndUpdateSuspension(t *testing.T, st *store, runID string, at time.Time, u suspensionUpdate) {
	t.Helper()
	if err := st.inTx(t.Context(), func(tx *storeTx) error {
		if _, err := tx.lockRun(runID); err != nil {
			return err
		}
		if err := tx.openSuspension(runID, at); err != nil {
			return err
		}
		return tx.updateLatestSuspension(runID, u)
	}); err != nil {
		t.Fatalf("open suspension for %s: %v", runID, err)
	}
}

func inboxSeqs(messages []inboxMessage) []int64 {
	seqs := make([]int64, len(messages))
	for i, m := range messages {
		seqs[i] = m.Seq
	}
	return seqs
}

func equalSeqs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestStoreInsertRunAndGetRun(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	r := seedRun(t, st, "run-insert", now)

	detail, err := st.getRun(ctx, r.RunID)
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if detail.RunID != r.RunID || detail.State != StateProvisioning || detail.Substate != SubstateNone {
		t.Fatalf("summary = %+v, want run_id %q provisioning with empty substate", detail.RunSummary, r.RunID)
	}
	if !detail.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", detail.CreatedAt, now)
	}
	if detail.FailureReason != "" {
		t.Fatalf("FailureReason = %q, want empty", detail.FailureReason)
	}
	if len(detail.Turns) != 0 || len(detail.Suspensions) != 0 {
		t.Fatalf("turns/suspensions = %d/%d, want 0/0", len(detail.Turns), len(detail.Suspensions))
	}

	err = st.inTx(ctx, func(tx *storeTx) error {
		messages, err := tx.inboxRange(r.RunID, 1, 10, 1<<20)
		if err != nil {
			return err
		}
		if len(messages) != 1 || messages[0].Seq != 1 || messages[0].Text != "first message" {
			return fmt.Errorf("first inbox message = %+v, want seq 1 %q", messages, "first message")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoreGetRunNotFound(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)

	_, err := st.getRun(t.Context(), "run-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("getRun missing = %v, want ErrNotFound", err)
	}
}

func TestStoreLockRunNotFound(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)

	err := st.inTx(t.Context(), func(tx *storeTx) error {
		_, err := tx.lockRun("run-missing")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("lockRun missing = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdateRun(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-update", now.Add(-time.Hour))

	idleSince := now.Add(-time.Minute)
	provisionReturnedAt := now.Add(-2 * time.Minute)
	endedAt := now

	updated := updateRunState(t, st, "run-update", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
		r.NextInboxSeq = 5
		r.DeliveredThrough = 3
		r.IdleSince = &idleSince
		r.ProvisionReturnedAt = &provisionReturnedAt
		r.FailureReason = "boom"
		r.EndedAt = &endedAt
	})
	if updated.State != StateActive {
		t.Fatalf("updated row state = %q, want active", updated.State)
	}

	detail, err := st.getRun(ctx, "run-update")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if detail.State != StateActive || detail.Substate != SubstateBoundary {
		t.Fatalf("state/substate = %q/%q, want active/boundary", detail.State, detail.Substate)
	}
	if detail.FailureReason != "boom" {
		t.Fatalf("FailureReason = %q, want boom", detail.FailureReason)
	}

	err = st.inTx(ctx, func(tx *storeTx) error {
		r, err := tx.lockRun("run-update")
		if err != nil {
			return err
		}
		switch {
		case r.NextInboxSeq != 5:
			t.Errorf("NextInboxSeq = %d, want 5", r.NextInboxSeq)
		case r.DeliveredThrough != 3:
			t.Errorf("DeliveredThrough = %d, want 3", r.DeliveredThrough)
		case r.IdleSince == nil || !r.IdleSince.Equal(idleSince):
			t.Errorf("IdleSince = %v, want %v", r.IdleSince, idleSince)
		case r.ProvisionReturnedAt == nil || !r.ProvisionReturnedAt.Equal(provisionReturnedAt):
			t.Errorf("ProvisionReturnedAt = %v, want %v", r.ProvisionReturnedAt, provisionReturnedAt)
		case r.EndedAt == nil || !r.EndedAt.Equal(endedAt):
			t.Errorf("EndedAt = %v, want %v", r.EndedAt, endedAt)
		case !r.CreatedAt.Equal(now.Add(-time.Hour)):
			t.Errorf("CreatedAt = %v, want %v", r.CreatedAt, now.Add(-time.Hour))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = st.inTx(ctx, func(tx *storeTx) error {
		return tx.updateRun(runRow{RunID: "run-missing", State: StateEnded})
	})
	if !errors.Is(err, errRunNotLocked) {
		t.Fatalf("updateRun without a lock = %v, want errRunNotLocked", err)
	}
}

func TestStoreInboxRange(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-inbox", now)
	for seq, text := range map[int64]string{
		2: "abcdefghij",
		3: "klmnopqrst",
	} {
		if err := st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertInbox("run-inbox", seq, text, now.Add(time.Duration(seq)*time.Second))
		}); err != nil {
			t.Fatalf("insert inbox %d: %v", seq, err)
		}
	}

	cases := []struct {
		name     string
		from     int64
		through  int64
		maxBytes int
		want     []int64
	}{
		{name: "all messages", from: 1, through: 3, maxBytes: 1 << 20, want: []int64{1, 2, 3}},
		{name: "subset", from: 2, through: 3, maxBytes: 1 << 20, want: []int64{2, 3}},
		{name: "byte cap stops before second", from: 1, through: 3, maxBytes: 10, want: []int64{1}},
		{name: "exactly two messages", from: 1, through: 3, maxBytes: 23, want: []int64{1, 2}},
		{name: "zero cap still returns one", from: 1, through: 3, maxBytes: 0, want: []int64{1}},
		{name: "empty range", from: 7, through: 9, maxBytes: 1 << 20, want: []int64{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var messages []inboxMessage
			err := st.inTx(ctx, func(tx *storeTx) error {
				var err error
				messages, err = tx.inboxRange("run-inbox", tc.from, tc.through, tc.maxBytes)
				return err
			})
			if err != nil {
				t.Fatalf("inboxRange: %v", err)
			}
			if got := inboxSeqs(messages); !equalSeqs(got, tc.want) {
				t.Fatalf("seqs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStoreUpsertTurnStarted(t *testing.T) {
	t.Parallel()

	st, pool := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-turn-start", now)

	first := now.Add(time.Minute)
	second := now.Add(2 * time.Minute)
	if err := st.inTx(ctx, func(tx *storeTx) error {
		return tx.upsertTurnStarted("run-turn-start", "turn-1", "delivery-1", first)
	}); err != nil {
		t.Fatalf("first upsertTurnStarted: %v", err)
	}
	if err := st.inTx(ctx, func(tx *storeTx) error {
		return tx.upsertTurnStarted("run-turn-start", "turn-1", "delivery-2", second)
	}); err != nil {
		t.Fatalf("second upsertTurnStarted: %v", err)
	}

	detail, err := st.getRun(ctx, "run-turn-start")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if len(detail.Turns) != 1 {
		t.Fatalf("turn count = %d, want 1", len(detail.Turns))
	}
	if !detail.Turns[0].StartedAt.Equal(first) {
		t.Fatalf("turn StartedAt = %v, want %v (second upsert must not change it)", detail.Turns[0].StartedAt, first)
	}
	if detail.Turns[0].EndedAt != nil {
		t.Fatalf("turn EndedAt = %v, want nil", detail.Turns[0].EndedAt)
	}

	var deliveryID string
	if err := pool.QueryRow(ctx,
		`SELECT delivery_id FROM turn WHERE run_id = $1 AND turn_id = $2`, "run-turn-start", "turn-1",
	).Scan(&deliveryID); err != nil {
		t.Fatalf("query delivery_id: %v", err)
	}
	if deliveryID != "delivery-1" {
		t.Fatalf("delivery_id = %q, want delivery-1 (second upsert must not change it)", deliveryID)
	}
}

func TestStoreUpsertTurnEnded(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-turn-end", now)

	endedAt := now.Add(time.Minute)
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-turn-end"); err != nil {
			return err
		}
		return tx.upsertTurnEnded("run-turn-end", "turn-new", endedAt, "output", true)
	}); err != nil {
		t.Fatalf("upsertTurnEnded without start: %v", err)
	}

	detail, err := st.getRun(ctx, "run-turn-end")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if len(detail.Turns) != 1 {
		t.Fatalf("turn count = %d, want 1", len(detail.Turns))
	}
	turn := detail.Turns[0]
	if turn.TurnID != "turn-new" || !turn.StartedAt.Equal(endedAt) {
		t.Fatalf("turn = %+v, want turn-new started at %v", turn, endedAt)
	}
	if turn.EndedAt == nil || !turn.EndedAt.Equal(endedAt) {
		t.Fatalf("EndedAt = %v, want %v", turn.EndedAt, endedAt)
	}
	if turn.Output != "output" || !turn.IsError {
		t.Fatalf("output/is_error = %q/%v, want output/true", turn.Output, turn.IsError)
	}

	later := now.Add(2 * time.Minute)
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-turn-end"); err != nil {
			return err
		}
		return tx.upsertTurnEnded("run-turn-end", "turn-new", later, "changed", false)
	}); err != nil {
		t.Fatalf("second upsertTurnEnded: %v", err)
	}
	detail, err = st.getRun(ctx, "run-turn-end")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if !detail.Turns[0].EndedAt.Equal(endedAt) || detail.Turns[0].Output != "output" || !detail.Turns[0].IsError {
		t.Fatalf("turn after second end = %+v, want unchanged", detail.Turns[0])
	}

	startedAt := now.Add(3 * time.Minute)
	if err := st.inTx(ctx, func(tx *storeTx) error {
		return tx.upsertTurnStarted("run-turn-end", "turn-started", "delivery", startedAt)
	}); err != nil {
		t.Fatalf("upsertTurnStarted: %v", err)
	}
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-turn-end"); err != nil {
			return err
		}
		return tx.upsertTurnEnded("run-turn-end", "turn-started", later, "done", false)
	}); err != nil {
		t.Fatalf("upsertTurnEnded after start: %v", err)
	}
	detail, err = st.getRun(ctx, "run-turn-end")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if len(detail.Turns) != 2 {
		t.Fatalf("turn count = %d, want 2", len(detail.Turns))
	}
	started := detail.Turns[1]
	if started.TurnID != "turn-started" || !started.StartedAt.Equal(startedAt) {
		t.Fatalf("turn = %+v, want turn-started started at %v", started, startedAt)
	}
	if started.EndedAt == nil || !started.EndedAt.Equal(later) {
		t.Fatalf("EndedAt = %v, want %v", started.EndedAt, later)
	}
}

func TestStoreSuspensions(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-suspend", now)

	err := st.inTx(ctx, func(tx *storeTx) error {
		_, ok, err := tx.latestSuspension("run-suspend")
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("latestSuspension reported a row on a fresh run")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	firstStarted := now.Add(time.Minute)
	secondStarted := now.Add(2 * time.Minute)
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-suspend"); err != nil {
			return err
		}
		return tx.openSuspension("run-suspend", firstStarted)
	}); err != nil {
		t.Fatalf("first openSuspension: %v", err)
	}
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-suspend"); err != nil {
			return err
		}
		return tx.openSuspension("run-suspend", secondStarted)
	}); err != nil {
		t.Fatalf("second openSuspension: %v", err)
	}

	snapshotMS := int64(120)
	snapshotBytes := int64(4096)
	resumeRequested := now.Add(3 * time.Minute)
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-suspend"); err != nil {
			return err
		}
		return tx.updateLatestSuspension("run-suspend", suspensionUpdate{
			SnapshotMS:        &snapshotMS,
			SnapshotBytes:     &snapshotBytes,
			ResumeRequestedAt: &resumeRequested,
		})
	}); err != nil {
		t.Fatalf("update snapshotted fields: %v", err)
	}

	err = st.inTx(ctx, func(tx *storeTx) error {
		row, ok, err := tx.latestSuspension("run-suspend")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("latestSuspension found no row")
		}
		if row.Seq != 2 || !row.StartedAt.Equal(secondStarted) {
			t.Errorf("latest row = seq %d started %v, want seq 2 started %v", row.Seq, row.StartedAt, secondStarted)
		}
		if row.SnapshotMS == nil || *row.SnapshotMS != snapshotMS {
			t.Errorf("SnapshotMS = %v, want %d", row.SnapshotMS, snapshotMS)
		}
		if row.SnapshotBytes == nil || *row.SnapshotBytes != snapshotBytes {
			t.Errorf("SnapshotBytes = %v, want %d", row.SnapshotBytes, snapshotBytes)
		}
		if row.ResumeRequestedAt == nil || !row.ResumeRequestedAt.Equal(resumeRequested) {
			t.Errorf("ResumeRequestedAt = %v, want %v", row.ResumeRequestedAt, resumeRequested)
		}
		if row.RestoreMS != nil || row.RestoreReturnedAt != nil || row.HelloAt != nil || row.HelloMS != nil {
			t.Errorf("unset fields were written: %+v", row)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	restoreMS := int64(250)
	restoreReturned := now.Add(4 * time.Minute)
	helloAt := now.Add(4*time.Minute + 500*time.Millisecond)
	helloMS := int64(500)
	errText := "restore warning"
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if _, err := tx.lockRun("run-suspend"); err != nil {
			return err
		}
		return tx.updateLatestSuspension("run-suspend", suspensionUpdate{
			RestoreMS:         &restoreMS,
			RestoreReturnedAt: &restoreReturned,
			HelloAt:           &helloAt,
			HelloMS:           &helloMS,
			Error:             &errText,
		})
	}); err != nil {
		t.Fatalf("update restored fields: %v", err)
	}

	detail, err := st.getRun(ctx, "run-suspend")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if len(detail.Suspensions) != 2 {
		t.Fatalf("suspension count = %d, want 2", len(detail.Suspensions))
	}
	first := detail.Suspensions[0]
	if !first.StartedAt.Equal(firstStarted) {
		t.Fatalf("first suspension StartedAt = %v, want %v", first.StartedAt, firstStarted)
	}
	if first.SnapshotMS != nil || first.RestoreMS != nil || first.HelloMS != nil || first.Error != "" {
		t.Fatalf("first suspension = %+v, want all optional fields empty", first)
	}
	second := detail.Suspensions[1]
	if !second.StartedAt.Equal(secondStarted) {
		t.Fatalf("second suspension StartedAt = %v, want %v", second.StartedAt, secondStarted)
	}
	if second.SnapshotMS == nil || *second.SnapshotMS != snapshotMS {
		t.Fatalf("SnapshotMS = %v, want %d", second.SnapshotMS, snapshotMS)
	}
	if second.RestoreMS == nil || *second.RestoreMS != restoreMS {
		t.Fatalf("RestoreMS = %v, want %d", second.RestoreMS, restoreMS)
	}
	if second.HelloMS == nil || *second.HelloMS != helloMS {
		t.Fatalf("HelloMS = %v, want %d", second.HelloMS, helloMS)
	}
	if second.Error != errText {
		t.Fatalf("Error = %q, want %q", second.Error, errText)
	}
}

func TestStoreGetRunOrdering(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-order", now)

	for _, turn := range []struct {
		id  string
		at  time.Time
		out string
	}{
		{id: "turn-b", at: now.Add(2 * time.Minute), out: "b"},
		{id: "turn-a", at: now.Add(time.Minute), out: "a"},
		{id: "turn-c", at: now.Add(time.Minute), out: "c"},
	} {
		if err := st.inTx(ctx, func(tx *storeTx) error {
			if _, err := tx.lockRun("run-order"); err != nil {
				return err
			}
			return tx.upsertTurnEnded("run-order", turn.id, turn.at, turn.out, false)
		}); err != nil {
			t.Fatalf("insert turn %s: %v", turn.id, err)
		}
	}

	openAndUpdateSuspension(t, st, "run-order", now.Add(3*time.Minute), suspensionUpdate{})
	openAndUpdateSuspension(t, st, "run-order", now.Add(time.Minute), suspensionUpdate{})

	detail, err := st.getRun(ctx, "run-order")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	wantTurns := []string{"turn-a", "turn-c", "turn-b"}
	if len(detail.Turns) != len(wantTurns) {
		t.Fatalf("turn count = %d, want %d", len(detail.Turns), len(wantTurns))
	}
	for i, want := range wantTurns {
		if detail.Turns[i].TurnID != want {
			t.Fatalf("turns = %v, want %v", turnIDs(detail.Turns), wantTurns)
		}
	}
	if len(detail.Suspensions) != 2 {
		t.Fatalf("suspension count = %d, want 2", len(detail.Suspensions))
	}
	if !detail.Suspensions[0].StartedAt.Before(detail.Suspensions[1].StartedAt) {
		t.Fatalf("suspensions are not in StartedAt order: %v then %v",
			detail.Suspensions[0].StartedAt, detail.Suspensions[1].StartedAt)
	}
}

func turnIDs(turns []Turn) []string {
	ids := make([]string, len(turns))
	for i, turn := range turns {
		ids[i] = turn.TurnID
	}
	return ids
}

func TestStoreListRuns(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-aaa", now.Add(-2*time.Hour))
	seedRun(t, st, "run-bbb", now)
	seedRun(t, st, "run-ccc", now)

	runs, err := st.listRuns(ctx)
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	want := []string{"run-ccc", "run-bbb", "run-aaa"}
	if len(runs) != len(want) {
		t.Fatalf("run count = %d, want %d", len(runs), len(want))
	}
	for i, wantID := range want {
		if runs[i].RunID != wantID {
			t.Fatalf("run order = %v, want %v", runSummaryIDs(runs), want)
		}
		if runs[i].State != StateProvisioning || runs[i].Substate != SubstateNone {
			t.Fatalf("run %s state/substate = %q/%q, want provisioning/empty",
				runs[i].RunID, runs[i].State, runs[i].Substate)
		}
	}
}

func runSummaryIDs(runs []RunSummary) []string {
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.RunID
	}
	return ids
}

func TestStoreSuspendCandidates(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	idleBefore := testTimestamp()

	longAgo := idleBefore.Add(-time.Minute)
	after := idleBefore.Add(time.Minute)

	seedRun(t, st, "run-ready-1", idleBefore.Add(-time.Hour))
	seedRun(t, st, "run-ready-2", idleBefore.Add(-time.Hour))
	seedRun(t, st, "run-equal", idleBefore.Add(-time.Hour))
	seedRun(t, st, "run-after", idleBefore.Add(-time.Hour))
	seedRun(t, st, "run-in-turn", idleBefore.Add(-time.Hour))
	seedRun(t, st, "run-provisioning", idleBefore.Add(-time.Hour))
	seedRun(t, st, "run-no-idle", idleBefore.Add(-time.Hour))

	updateRunState(t, st, "run-ready-1", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
		r.IdleSince = &longAgo
	})
	updateRunState(t, st, "run-ready-2", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
		r.IdleSince = &longAgo
	})
	updateRunState(t, st, "run-equal", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
		r.IdleSince = &idleBefore
	})
	updateRunState(t, st, "run-after", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
		r.IdleSince = &after
	})
	updateRunState(t, st, "run-in-turn", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateInTurn
		r.IdleSince = &longAgo
	})
	updateRunState(t, st, "run-provisioning", func(r *runRow) {
		r.IdleSince = &longAgo
	})
	updateRunState(t, st, "run-no-idle", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
	})

	got, err := st.suspendCandidates(ctx, idleBefore)
	if err != nil {
		t.Fatalf("suspendCandidates: %v", err)
	}
	want := []string{"run-ready-1", "run-ready-2"}
	if !equalStrings(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestStoreHelloTimeoutCandidates(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	ctx := t.Context()
	returnedBefore := testTimestamp()

	longAgo := returnedBefore.Add(-time.Minute)
	after := returnedBefore.Add(time.Minute)

	seedRun(t, st, "run-prov-old", returnedBefore.Add(-time.Hour))
	seedRun(t, st, "run-prov-equal", returnedBefore.Add(-time.Hour))
	seedRun(t, st, "run-prov-nil", returnedBefore.Add(-time.Hour))
	seedRun(t, st, "run-resume-old", returnedBefore.Add(-time.Hour))
	seedRun(t, st, "run-resume-hello", returnedBefore.Add(-time.Hour))
	seedRun(t, st, "run-resume-latest-after", returnedBefore.Add(-time.Hour))
	seedRun(t, st, "run-active-suspended", returnedBefore.Add(-time.Hour))

	updateRunState(t, st, "run-prov-old", func(r *runRow) {
		r.ProvisionReturnedAt = &longAgo
	})
	updateRunState(t, st, "run-prov-equal", func(r *runRow) {
		r.ProvisionReturnedAt = &returnedBefore
	})
	// run-prov-nil keeps provision_returned_at unset.

	updateRunState(t, st, "run-resume-old", func(r *runRow) {
		r.State = StateResuming
		r.Substate = SubstateSnapshotted
	})
	openAndUpdateSuspension(t, st, "run-resume-old", longAgo, suspensionUpdate{
		RestoreReturnedAt: &longAgo,
	})

	helloAt := longAgo.Add(time.Second)
	updateRunState(t, st, "run-resume-hello", func(r *runRow) {
		r.State = StateResuming
		r.Substate = SubstateSnapshotted
	})
	openAndUpdateSuspension(t, st, "run-resume-hello", longAgo, suspensionUpdate{
		RestoreReturnedAt: &longAgo,
		HelloAt:           &helloAt,
	})

	updateRunState(t, st, "run-resume-latest-after", func(r *runRow) {
		r.State = StateResuming
		r.Substate = SubstateSnapshotted
	})
	openAndUpdateSuspension(t, st, "run-resume-latest-after", longAgo, suspensionUpdate{
		RestoreReturnedAt: &longAgo,
	})
	openAndUpdateSuspension(t, st, "run-resume-latest-after", longAgo.Add(time.Second), suspensionUpdate{
		RestoreReturnedAt: &after,
	})

	updateRunState(t, st, "run-active-suspended", func(r *runRow) {
		r.State = StateActive
		r.Substate = SubstateBoundary
	})
	openAndUpdateSuspension(t, st, "run-active-suspended", longAgo, suspensionUpdate{
		RestoreReturnedAt: &longAgo,
	})

	got, err := st.helloTimeoutCandidates(ctx, returnedBefore)
	if err != nil {
		t.Fatalf("helloTimeoutCandidates: %v", err)
	}
	want := []string{"run-prov-old", "run-resume-old"}
	if !equalStrings(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestStoreWritesRequireLock 验证会读取并回写已有行的方法必须先 lockRun。
// 未锁先写在这里必须报错，而不是照常写库。
func TestStoreWritesRequireLock(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	now := testTimestamp()
	r := seedRun(t, st, "run-require-lock", now)

	writes := []struct {
		name  string
		write func(tx *storeTx) error
	}{
		{
			name: "updateRun",
			write: func(tx *storeTx) error {
				updated := r
				updated.State = StateActive
				updated.Substate = SubstateBoundary
				return tx.updateRun(updated)
			},
		},
		{
			name: "upsertTurnEnded",
			write: func(tx *storeTx) error {
				return tx.upsertTurnEnded("run-require-lock", "turn-1", now, "output", false)
			},
		},
		{
			name: "openSuspension",
			write: func(tx *storeTx) error {
				return tx.openSuspension("run-require-lock", now)
			},
		},
		{
			name: "updateLatestSuspension",
			write: func(tx *storeTx) error {
				return tx.updateLatestSuspension("run-require-lock", suspensionUpdate{})
			},
		},
	}

	for _, tc := range writes {
		t.Run(tc.name, func(t *testing.T) {
			err := st.inTx(t.Context(), func(tx *storeTx) error {
				return tc.write(tx)
			})
			if !errors.Is(err, errRunNotLocked) {
				t.Fatalf("%s without lockRun = %v, want errRunNotLocked", tc.name, err)
			}
		})
	}
}

// TestStoreUpdateLatestSuspensionWithoutSuspension 验证没有挂起记录时
// updateLatestSuspension 报错，而不是静默地更新零行。
func TestStoreUpdateLatestSuspensionWithoutSuspension(t *testing.T) {
	t.Parallel()

	st, _ := newTestStore(t)
	now := testTimestamp()
	seedRun(t, st, "run-no-suspension", now)

	snapshotMS := int64(1)
	err := st.inTx(t.Context(), func(tx *storeTx) error {
		if _, err := tx.lockRun("run-no-suspension"); err != nil {
			return err
		}
		return tx.updateLatestSuspension("run-no-suspension", suspensionUpdate{
			SnapshotMS: &snapshotMS,
		})
	})
	if !errors.Is(err, errNoSuspension) {
		t.Fatalf("updateLatestSuspension without a suspension row = %v, want errNoSuspension", err)
	}
}

// TestStoreLockRunHoldsRowLock 验证 lockRun 的 SELECT ... FOR UPDATE 真的加了行锁：
// 持锁事务未提交时，另一个事务的 lockRun 会在 pg_stat_activity 中表现为
// wait_event_type='Lock'。查询用连接池之外的独立连接，判定不靠睡眠。
func TestStoreLockRunHoldsRowLock(t *testing.T) {
	t.Parallel()

	st, pool := newTestStore(t)
	ctx := t.Context()
	now := testTimestamp()

	seedRun(t, st, "run-lock", now)

	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin locker transaction: %v", err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()

	if _, err := (&storeTx{ctx: ctx, tx: locker}).lockRun("run-lock"); err != nil {
		t.Fatalf("locker lockRun: %v", err)
	}

	observer, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("open independent connection: %v", err)
	}
	defer func() { _ = observer.Close(context.Background()) }()

	type waiterResult struct {
		tx  pgx.Tx
		err error
	}
	waiterTxC := make(chan pgx.Tx, 1)
	waiterDone := make(chan waiterResult, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			waiterDone <- waiterResult{err: err}
			return
		}
		waiterTxC <- tx
		_, err = (&storeTx{ctx: ctx, tx: tx}).lockRun("run-lock")
		waiterDone <- waiterResult{tx: tx, err: err}
	}()

	// 测试在等待发生前失败时，等待方可能还握着第二个连接；关池会因此阻塞，
	// 所以无论走哪条路径都回滚它。
	t.Cleanup(func() {
		select {
		case tx := <-waiterTxC:
			_ = tx.Rollback(context.Background())
		default:
		}
	})

	if err := waitForLockWait(ctx, observer); err != nil {
		t.Fatalf("second lockRun did not block: %v", err)
	}

	if err := locker.Commit(ctx); err != nil {
		t.Fatalf("commit locker transaction: %v", err)
	}

	result := <-waiterDone
	if result.err != nil {
		t.Fatalf("second lockRun: %v", result.err)
	}
	if result.tx == nil {
		t.Fatal("second lockRun returned no transaction")
	}
	<-waiterTxC
	if err := result.tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback waiter transaction: %v", err)
	}
}

func waitForLockWait(ctx context.Context, conn *pgx.Conn) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no backend is waiting on a lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

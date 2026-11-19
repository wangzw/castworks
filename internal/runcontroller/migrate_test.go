package runcontroller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

// templatePool returns a fresh database copied from the "runcontroller"
// template, which has the migrations applied once per process. Tests that
// exercise Migrate itself use migratedPool instead.
func templatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.NewPoolFromTemplate(t, "runcontroller", Migrate)
}

func testTimestamp() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func TestMigrateCreatesTables(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	for _, table := range []string{"schema_migrations", "run", "inbox", "turn", "suspension"} {
		var exists bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s was not created", table)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	st := newStore(pool)
	now := testTimestamp()

	run := runRow{
		RunID:        "run-preserve",
		State:        StateProvisioning,
		Substate:     SubstateNone,
		ActorName:    "actor-run-preserve",
		NextInboxSeq: 2,
		CreatedAt:    now,
	}
	if err := st.inTx(ctx, func(tx *storeTx) error {
		if err := tx.insertRun(run, "first message", now); err != nil {
			return err
		}
		return tx.upsertTurnStarted(run.RunID, "turn-1", "delivery-1", now)
	}); err != nil {
		t.Fatalf("seed data: %v", err)
	}

	versionsBefore := countMigrations(t, pool)
	if versionsBefore == 0 {
		t.Fatal("no migration versions recorded")
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	if got := countMigrations(t, pool); got != versionsBefore {
		t.Fatalf("schema_migrations row count = %d after second Migrate, want %d", got, versionsBefore)
	}

	detail, err := st.getRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("getRun after second Migrate: %v", err)
	}
	if detail.RunID != run.RunID || detail.State != StateProvisioning {
		t.Fatalf("run after second Migrate = %+v, want run_id %q state %q", detail.RunSummary, run.RunID, StateProvisioning)
	}
	if len(detail.Turns) != 1 || detail.Turns[0].TurnID != "turn-1" {
		t.Fatalf("turns after second Migrate = %+v, want turn-1", detail.Turns)
	}

	var inboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox WHERE run_id = $1`, run.RunID).Scan(&inboxCount); err != nil {
		t.Fatalf("count inbox: %v", err)
	}
	if inboxCount != 1 {
		t.Fatalf("inbox count = %d after second Migrate, want 1", inboxCount)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	t.Parallel()

	pool := pgtest.NewPool(t)
	ctx := t.Context()

	const callers = 2
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = Migrate(ctx, pool)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate %d: %v", i, err)
		}
	}

	expected := len(mustLoadMigrations(t))
	if got := countMigrations(t, pool); got != expected {
		t.Fatalf("schema_migrations row count = %d, want %d", got, expected)
	}
}

func TestMigrateConstraints(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	st := newStore(pool)
	now := testTimestamp()

	base := func(runID string) runRow {
		return runRow{
			RunID:        runID,
			State:        StateProvisioning,
			Substate:     SubstateNone,
			ActorName:    "actor-" + runID,
			NextInboxSeq: 2,
			CreatedAt:    now,
		}
	}

	insert := func(r runRow, firstText string) error {
		return st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertRun(r, firstText, now)
		})
	}

	runCases := []struct {
		name    string
		state   State
		sub     Substate
		wantErr bool
	}{
		{name: "provisioning with empty substate", state: StateProvisioning, sub: SubstateNone},
		{name: "active in_turn", state: StateActive, sub: SubstateInTurn},
		{name: "active delivering", state: StateActive, sub: SubstateDelivering},
		{name: "active boundary", state: StateActive, sub: SubstateBoundary},
		{name: "suspended snapshotted", state: StateSuspended, sub: SubstateSnapshotted},
		{name: "resuming snapshotted", state: StateResuming, sub: SubstateSnapshotted},
		{name: "active with empty substate", state: StateActive, sub: SubstateNone, wantErr: true},
		{name: "provisioning with boundary", state: StateProvisioning, sub: SubstateBoundary, wantErr: true},
		{name: "suspended with in_turn", state: StateSuspended, sub: SubstateInTurn, wantErr: true},
		{name: "unknown state", state: State("bogus"), sub: SubstateNone, wantErr: true},
	}

	for i, tc := range runCases {
		t.Run(tc.name, func(t *testing.T) {
			r := base(fmt.Sprintf("run-constraint-%d", i))
			r.State = tc.state
			r.Substate = tc.sub
			err := insert(r, "hello")
			if tc.wantErr && err == nil {
				t.Fatal("insert succeeded, want a CHECK constraint error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("insert failed: %v", err)
			}
		})
	}

	t.Run("inbox text bounds", func(t *testing.T) {
		runID := "run-inbox-bounds"
		if err := insert(base(runID), "hello"); err != nil {
			t.Fatalf("insert run: %v", err)
		}

		err := st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertInbox(runID, 2, "", now)
		})
		if err == nil {
			t.Fatal("empty inbox text succeeded, want a CHECK constraint error")
		}

		err = st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertInbox(runID, 3, strings.Repeat("x", MaxTextBytes+1), now)
		})
		if err == nil {
			t.Fatal("oversized inbox text succeeded, want a CHECK constraint error")
		}

		err = st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertInbox(runID, 4, strings.Repeat("x", MaxTextBytes), now)
		})
		if err != nil {
			t.Fatalf("inbox text of exactly MaxTextBytes failed: %v", err)
		}
	})
}

func countMigrations(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return count
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	return migrations
}

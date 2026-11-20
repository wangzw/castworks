package runcontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
			return tx.insertInbox(runID, inboxInsert{Seq: 2, Kind: KindHumanMessage, Text: ""}, now)
		})
		if err == nil {
			t.Fatal("empty inbox text succeeded, want a CHECK constraint error")
		}

		err = st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertInbox(runID, inboxInsert{Seq: 3, Kind: KindHumanMessage, Text: strings.Repeat("x", MaxTextBytes+1)}, now)
		})
		if err == nil {
			t.Fatal("oversized inbox text succeeded, want a CHECK constraint error")
		}

		err = st.inTx(ctx, func(tx *storeTx) error {
			return tx.insertInbox(runID, inboxInsert{Seq: 4, Kind: KindHumanMessage, Text: strings.Repeat("x", MaxTextBytes)}, now)
		})
		if err != nil {
			t.Fatalf("inbox text of exactly MaxTextBytes failed: %v", err)
		}
	})
}

// v010LastMigrationVersion 是 v0.1.0 的最后一个迁移版本。v0.1.0 的库结构
// 等于只应用版本 1 与 2 的结构。
const v010LastMigrationVersion = 2

// seedV010Schema 把空库恢复成 v0.1.0 的结构，并按 Migrate 的写法记录版本，
// 让随后的 Migrate 只应用 v0.2.0 新增的迁移。
func seedV010Schema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()

	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range mustLoadMigrations(t) {
		if m.version > v010LastMigrationVersion {
			continue
		}
		if _, err := pool.Exec(ctx, m.sql); err != nil {
			t.Fatalf("apply v0.1.0 migration %s: %v", m.name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name,
		); err != nil {
			t.Fatalf("record v0.1.0 migration %s: %v", m.name, err)
		}
	}
}

// TestMigrateFromV010InboxRows 在 v0.1.0 的结构与数据上应用 v0.2.0 的迁移：
// 已有收件箱行取 human_message，运行实例获得空的 bg_jobs 集合。
func TestMigrateFromV010InboxRows(t *testing.T) {
	t.Parallel()

	pool := pgtest.NewPool(t)
	ctx := t.Context()
	seedV010Schema(t, pool)

	now := testTimestamp()
	if _, err := pool.Exec(ctx, `INSERT INTO run (
		run_id, state, substate, actor_name, next_inbox_seq, created_at
	) VALUES ($1, $2, $3, $4, 2, $5)`,
		"run-v010", StateActive, SubstateInTurn, "actor-v010", now,
	); err != nil {
		t.Fatalf("insert v0.1.0 run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO inbox (run_id, inbox_seq, text, created_at)
		VALUES ($1, 1, $2, $3)`, "run-v010", "legacy message", now,
	); err != nil {
		t.Fatalf("insert v0.1.0 inbox row: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate over v0.1.0 schema: %v", err)
	}

	var kind, eventID, dedupKey string
	var payloadNull bool
	if err := pool.QueryRow(ctx, `SELECT kind, event_id, dedup_key, payload IS NULL
		FROM inbox WHERE run_id = $1 AND inbox_seq = 1`, "run-v010",
	).Scan(&kind, &eventID, &dedupKey, &payloadNull); err != nil {
		t.Fatalf("read migrated inbox row: %v", err)
	}
	if kind != "human_message" {
		t.Errorf("migrated inbox kind = %q, want %q", kind, "human_message")
	}
	if eventID != "" || dedupKey != "" || !payloadNull {
		t.Errorf("migrated inbox event_id = %q, dedup_key = %q, payload null = %t; want empty strings and null payload",
			eventID, dedupKey, payloadNull)
	}

	var bgJobs []string
	var bgJobsNull bool
	if err := pool.QueryRow(ctx,
		`SELECT bg_jobs, bg_jobs IS NULL FROM run WHERE run_id = $1`, "run-v010",
	).Scan(&bgJobs, &bgJobsNull); err != nil {
		t.Fatalf("read migrated run bg_jobs: %v", err)
	}
	if bgJobsNull || len(bgJobs) != 0 {
		t.Errorf("migrated run bg_jobs = %v (null = %t), want an empty array", bgJobs, bgJobsNull)
	}
}

// TestMigrateInboxDedupKeyUnique 覆盖 (run_id, dedup_key) 的部分唯一索引：
// dedup_key 非空时同一运行实例内重复被拒，空串可以重复，不同运行实例互不影响。
func TestMigrateInboxDedupKeyUnique(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testTimestamp()

	insertRun := func(runID string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO run (
			run_id, state, substate, actor_name, next_inbox_seq, created_at
		) VALUES ($1, $2, $3, $4, 2, $5)`,
			runID, StateActive, SubstateInTurn, "actor-"+runID, now,
		); err != nil {
			t.Fatalf("insert run %s: %v", runID, err)
		}
	}
	insertSubscription := func(runID string, seq int64, eventID, dedupKey string) error {
		_, err := pool.Exec(ctx, `INSERT INTO inbox (
			run_id, inbox_seq, kind, text, event_id, dedup_key, payload, created_at
		) VALUES ($1, $2, 'subscription_completed', '', $3, $4, '{}'::jsonb, $5)`,
			runID, seq, eventID, dedupKey, now)
		return err
	}

	insertRun("run-dedup-a")
	insertRun("run-dedup-b")

	if err := insertSubscription("run-dedup-a", 1, "event-1", "dedup-1"); err != nil {
		t.Fatalf("first subscription_completed insert: %v", err)
	}
	err := insertSubscription("run-dedup-a", 2, "event-2", "dedup-1")
	if err == nil {
		t.Fatal("duplicate (run_id, dedup_key) insert succeeded, want a unique violation")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate insert error = %v, want SQLSTATE 23505", err)
	}

	for seq := int64(3); seq <= 4; seq++ {
		if _, err := pool.Exec(ctx, `INSERT INTO inbox (run_id, inbox_seq, text, created_at)
			VALUES ($1, $2, $3, $4)`, "run-dedup-a", seq, fmt.Sprintf("human %d", seq), now,
		); err != nil {
			t.Fatalf("insert human_message with empty dedup_key at seq %d: %v", seq, err)
		}
	}

	if err := insertSubscription("run-dedup-b", 1, "event-1", "dedup-1"); err != nil {
		t.Fatalf("same dedup_key on another run: %v", err)
	}
}

// TestMigrateInboxEventShape 覆盖 kind 与其余列的取值约束。
func TestMigrateInboxEventShape(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()
	now := testTimestamp()

	if _, err := pool.Exec(ctx, `INSERT INTO run (
		run_id, state, substate, actor_name, next_inbox_seq, created_at
	) VALUES ($1, $2, $3, $4, 2, $5)`,
		"run-shape", StateActive, SubstateInTurn, "actor-shape", now,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	insert := func(seq int64, kind, text, eventID, dedupKey string, payload any) error {
		_, err := pool.Exec(ctx, `INSERT INTO inbox (
			run_id, inbox_seq, kind, text, event_id, dedup_key, payload, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			"run-shape", seq, kind, text, eventID, dedupKey, payload, now)
		return err
	}

	cases := []struct {
		name     string
		seq      int64
		kind     string
		text     string
		eventID  string
		dedupKey string
		payload  any
		wantErr  bool
	}{
		{name: "subscription_completed", seq: 1, kind: "subscription_completed",
			eventID: "event-1", dedupKey: "dedup-1", payload: `{}`},
		{name: "subscription_completed with text", seq: 2, kind: "subscription_completed",
			text: "not empty", eventID: "event-2", dedupKey: "dedup-2", payload: `{}`, wantErr: true},
		{name: "subscription_completed without event_id", seq: 3, kind: "subscription_completed",
			dedupKey: "dedup-3", payload: `{}`, wantErr: true},
		{name: "subscription_completed without dedup_key", seq: 4, kind: "subscription_completed",
			eventID: "event-4", payload: `{}`, wantErr: true},
		{name: "subscription_completed without payload", seq: 5, kind: "subscription_completed",
			eventID: "event-5", dedupKey: "dedup-5", wantErr: true},
		{name: "human_message", seq: 6, kind: "human_message", text: "hello"},
		{name: "human_message with dedup_key", seq: 7, kind: "human_message",
			text: "hello", dedupKey: "dedup-7", wantErr: true},
		{name: "unknown kind", seq: 8, kind: "bogus", text: "hello", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(tc.seq, tc.kind, tc.text, tc.eventID, tc.dedupKey, tc.payload)
			if tc.wantErr {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("insert error = %v, want SQLSTATE 23514", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("insert failed: %v", err)
			}
		})
	}
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

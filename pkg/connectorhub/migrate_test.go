package connectorhub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

func TestMigrateCreatesTables(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	for _, table := range []string{
		"connectorhub_schema_migrations",
		"connector_event",
		"connector_event_counter",
		"subscription",
		"subscription_claim",
		"completion_delivery",
	} {
		if !tableExists(t, ctx, pool, table) {
			t.Errorf("table %s was not created", table)
		}
	}

	var indexDef string
	err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = 'connector_event'
		AND indexname = 'connector_event_correlation_gin'`).Scan(&indexDef)
	if err != nil {
		t.Fatalf("query correlation GIN index: %v", err)
	}
	if !strings.Contains(indexDef, "USING gin") || !strings.Contains(indexDef, "(correlation)") {
		t.Fatalf("correlation index definition = %q, want a GIN index on correlation", indexDef)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()

	pool := pgtest.NewPool(t)
	ctx := t.Context()

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	columnsBefore := schemaColumns(t, ctx, pool)
	indexesBefore := schemaIndexes(t, ctx, pool)
	versionsBefore := countMigrations(t, ctx, pool)

	counterRows, nextSeq := counterState(t, ctx, pool)
	if counterRows != 1 || nextSeq != 1 {
		t.Fatalf("connector_event_counter = %d rows, next_seq %d, want one row with next_seq 1", counterRows, nextSeq)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	if got := countMigrations(t, ctx, pool); got != versionsBefore {
		t.Fatalf("connectorhub_schema_migrations row count = %d after second Migrate, want %d", got, versionsBefore)
	}
	if got := schemaColumns(t, ctx, pool); !reflect.DeepEqual(got, columnsBefore) {
		t.Fatalf("columns changed after second Migrate:\nbefore: %v\nafter:  %v", columnsBefore, got)
	}
	if got := schemaIndexes(t, ctx, pool); !reflect.DeepEqual(got, indexesBefore) {
		t.Fatalf("indexes changed after second Migrate:\nbefore: %v\nafter:  %v", indexesBefore, got)
	}

	counterRows, nextSeq = counterState(t, ctx, pool)
	if counterRows != 1 || nextSeq != 1 {
		t.Fatalf("connector_event_counter = %d rows, next_seq %d after second Migrate, want one row with next_seq 1", counterRows, nextSeq)
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
	if got := countMigrations(t, ctx, pool); got != expected {
		t.Fatalf("connectorhub_schema_migrations row count = %d, want %d", got, expected)
	}
}

func TestMigrateLeavesRuncontrollerTablesAlone(t *testing.T) {
	t.Parallel()

	t.Run("connectorhub only", func(t *testing.T) {
		t.Parallel()

		pool := migratedPool(t)
		ctx := t.Context()

		for _, table := range []string{"schema_migrations", "run", "inbox", "turn", "suspension"} {
			if tableExists(t, ctx, pool, table) {
				t.Errorf("table %s exists after connectorhub.Migrate only", table)
			}
		}
	})

	t.Run("runcontroller only", func(t *testing.T) {
		t.Parallel()

		pool := pgtest.NewPool(t)
		ctx := t.Context()
		if err := runcontroller.Migrate(ctx, pool); err != nil {
			t.Fatalf("runcontroller.Migrate: %v", err)
		}

		for _, table := range []string{
			"connectorhub_schema_migrations",
			"connector_event",
			"connector_event_counter",
			"subscription",
			"subscription_claim",
			"completion_delivery",
		} {
			if tableExists(t, ctx, pool, table) {
				t.Errorf("table %s exists after runcontroller.Migrate only", table)
			}
		}
	})
}

func TestMigrateConcurrentWithRuncontroller(t *testing.T) {
	t.Parallel()

	pool := pgtest.NewPool(t)
	ctx := t.Context()

	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = Migrate(ctx, pool)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = runcontroller.Migrate(ctx, pool)
	}()
	close(start)
	wg.Wait()

	if errs[0] != nil {
		t.Fatalf("connectorhub.Migrate: %v", errs[0])
	}
	if errs[1] != nil {
		t.Fatalf("runcontroller.Migrate: %v", errs[1])
	}

	if got, want := countMigrations(t, ctx, pool), len(mustLoadMigrations(t)); got != want {
		t.Fatalf("connectorhub_schema_migrations row count = %d, want %d", got, want)
	}

	var runcontrollerVersions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&runcontrollerVersions); err != nil {
		t.Fatalf("count runcontroller schema_migrations: %v", err)
	}
	if runcontrollerVersions == 0 {
		t.Fatal("runcontroller recorded no migration versions")
	}

	for _, table := range []string{"connector_event", "run", "inbox", "subscription", "completion_delivery"} {
		if !tableExists(t, ctx, pool, table) {
			t.Errorf("table %s is missing after both migrations", table)
		}
	}
}

// TestMigrateLockIndependentOfRuncontroller 在持有本模块迁移锁的事务里执行
// runcontroller.Migrate。两者使用不同的 advisory lock 键，后者不应等待前者。
func TestMigrateLockIndependentOfRuncontroller(t *testing.T) {
	t.Parallel()

	pool := pgtest.NewPool(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryLock); err != nil {
		t.Fatalf("acquire connectorhub migration lock: %v", err)
	}

	lockedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := runcontroller.Migrate(lockedCtx, pool); err != nil {
		t.Fatalf("runcontroller.Migrate while connectorhub migration lock is held: %v", err)
	}
}

func TestMigrateConstraints(t *testing.T) {
	t.Parallel()

	pool := migratedPool(t)
	ctx := t.Context()

	insertEvent := func(seq int64, system, delivery, correlation string) error {
		_, err := pool.Exec(ctx, `INSERT INTO connector_event
			(append_seq, system, delivery_id, event_type, project_ids, correlation, occurred_at, payload)
			VALUES ($1, $2, $3, 'job.completed', $4, $5::jsonb, now(), $6)`,
			seq, system, delivery, []string{"default"}, correlation, []byte(`{"job":{"id":"j-1"}}`))
		return err
	}

	t.Run("append_seq and delivery uniqueness", func(t *testing.T) {
		if err := insertEvent(1, "extsim", "d-1", `{"job.id":"j-1"}`); err != nil {
			t.Fatalf("insert first event: %v", err)
		}
		if err := insertEvent(2, "extsim", "d-1", `{"job.id":"j-1"}`); !hasPgCode(err, "23505") {
			t.Fatalf("duplicate (system, delivery_id) error = %v, want unique violation 23505", err)
		}
		if err := insertEvent(2, "other", "d-1", `{"job.id":"j-1"}`); err != nil {
			t.Fatalf("same delivery_id under another system: %v", err)
		}
		if err := insertEvent(3, "extsim", "d-2", `{"job.id":"j-2"}`); err != nil {
			t.Fatalf("insert third event: %v", err)
		}
		if err := insertEvent(0, "extsim", "d-0", `{}`); !hasPgCode(err, "23514") {
			t.Fatalf("append_seq 0 error = %v, want check violation 23514", err)
		}
	})

	t.Run("correlation containment", func(t *testing.T) {
		var j1, j2 int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_event WHERE correlation @> $1::jsonb`,
			`{"job.id":"j-1"}`).Scan(&j1); err != nil {
			t.Fatalf("query correlation j-1: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_event WHERE correlation @> $1::jsonb`,
			`{"job.id":"j-2"}`).Scan(&j2); err != nil {
			t.Fatalf("query correlation j-2: %v", err)
		}
		if j1 != 2 || j2 != 1 {
			t.Fatalf("correlation counts = j-1 %d, j-2 %d, want 2 and 1", j1, j2)
		}
	})

	t.Run("counter is a single row", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO connector_event_counter (id, next_seq) VALUES (true, 2)`); !hasPgCode(err, "23505") {
			t.Fatalf("second counter row error = %v, want unique violation 23505", err)
		}
	})

	insertSubscription := func(subID, status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO subscription
			(sub_id, run_id, project_id, system, event, correlation, status, since_cursor, dedup_spec, created_at)
			VALUES ($1, 'run-1', 'default', 'extsim', 'job.completed', $2::jsonb, $3, 1, $4::jsonb, now())`,
			subID, `{"job.id":"j-1"}`, status, `["job.id"]`)
		return err
	}

	t.Run("subscription status", func(t *testing.T) {
		if err := insertSubscription("sub-valid", "active"); err != nil {
			t.Fatalf("insert active subscription: %v", err)
		}
		if err := insertSubscription("sub-bogus", "bogus"); !hasPgCode(err, "23514") {
			t.Fatalf("invalid subscription status error = %v, want check violation 23514", err)
		}
	})

	t.Run("claim uniqueness", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO subscription_claim (sub_id, delivery_id) VALUES ('sub-valid', 'd-1')`); err != nil {
			t.Fatalf("insert claim: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO subscription_claim (sub_id, delivery_id) VALUES ('sub-valid', 'd-1')`); !hasPgCode(err, "23505") {
			t.Fatalf("duplicate claim error = %v, want unique violation 23505", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO subscription_claim (sub_id, delivery_id) VALUES ('sub-valid', 'd-2')`); err != nil {
			t.Fatalf("claim another delivery for the same subscription: %v", err)
		}
	})

	insertDelivery := func(subID, dedupKey, status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO completion_delivery
			(sub_id, run_id, event_id, dedup_key, event, status)
			VALUES ($1, 'run-1', $2, $3, $4::jsonb, $5)`,
			subID, "("+subID+",success)", dedupKey, `{"sub_id":"`+subID+`"}`, status)
		return err
	}

	t.Run("completion delivery uniqueness and status", func(t *testing.T) {
		if err := insertDelivery("sub-valid", "dedup-1", "pending"); err != nil {
			t.Fatalf("insert pending delivery: %v", err)
		}
		if err := insertDelivery("sub-valid", "dedup-1", "pending"); !hasPgCode(err, "23505") {
			t.Fatalf("duplicate (sub_id, dedup_key) error = %v, want unique violation 23505", err)
		}
		if err := insertDelivery("sub-valid", "dedup-2", "bogus"); !hasPgCode(err, "23514") {
			t.Fatalf("invalid delivery status error = %v, want check violation 23514", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO completion_delivery
			(sub_id, run_id, event_id, dedup_key, event, attempts)
			VALUES ('sub-valid', 'run-1', 'e', 'dedup-3', '{}'::jsonb, -1)`); !hasPgCode(err, "23514") {
			t.Fatalf("negative attempts error = %v, want check violation 23514", err)
		}
	})
}

func tableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}

func countMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM connectorhub_schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count connectorhub_schema_migrations: %v", err)
	}
	return count
}

func counterState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (rows int, nextSeq int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_event_counter`).Scan(&rows); err != nil {
		t.Fatalf("count connector_event_counter: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT next_seq FROM connector_event_counter`).Scan(&nextSeq); err != nil {
		t.Fatalf("read connector_event_counter.next_seq: %v", err)
	}
	return rows, nextSeq
}

func schemaColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT table_name || '.' || column_name || ':' || data_type
		|| ':' || is_nullable || ':' || coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public'
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	return columns
}

func schemaIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT indexname || '=' || indexdef
		FROM pg_indexes WHERE schemaname = 'public' ORDER BY indexname`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()

	var indexes []string
	for rows.Next() {
		var index string
		if err := rows.Scan(&index); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	return indexes
}

func hasPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	return migrations
}

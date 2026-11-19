package pgtest_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

// cleanupHelperEnv marks the re-executed test binary that checks the cleanup
// Main performs. Only the helper test runs in that child process, and its
// TestMain verifies the template is gone after Main returns.
const cleanupHelperEnv = "PGTEST_TEST_MAIN_CLEANUP_HELPER"

// cleanupHelperTemplate records the template database the helper created, for
// the check TestMain performs after Main returns.
var cleanupHelperTemplate string

func TestMain(m *testing.M) {
	code := pgtest.Main(m)
	if os.Getenv(cleanupHelperEnv) == "1" && code == 0 {
		if cleanupHelperTemplate == "" {
			fmt.Fprintln(os.Stderr, "pgtest cleanup helper did not record a template database")
			code = 1
		} else if exists, err := templateDatabaseExists(cleanupHelperTemplate); err != nil {
			fmt.Fprintf(os.Stderr, "check template database %s: %v\n", cleanupHelperTemplate, err)
			code = 1
		} else if exists {
			fmt.Fprintf(os.Stderr, "template database %s still exists after Main returned\n", cleanupHelperTemplate)
			code = 1
		}
	}
	os.Exit(code)
}

// TestMainDropsProcessTemplates runs the helper test in a separate process so
// its TestMain can assert, after Main returns, that the template database the
// helper created is gone.
func TestMainDropsProcessTemplates(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTemplateCleanupHelper$")
	cmd.Env = append(os.Environ(), cleanupHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process failed: %v\n%s", err, out)
	}
}

func TestTemplateCleanupHelper(t *testing.T) {
	if os.Getenv(cleanupHelperEnv) != "1" {
		t.Skip("only runs in the re-executed process for TestMainDropsProcessTemplates")
	}

	pool := pgtest.NewPoolFromTemplate(t, uniqueTemplateName("cleanup"), markerSetup("cleanup_marker"))
	cleanupHelperTemplate = findTemplateDatabase(t, pool, "cleanup")
}

func templateDatabaseExists(name string) (bool, error) {
	adminURL := strings.TrimSpace(os.Getenv(pgtest.DatabaseURLEnv))
	if adminURL == "" {
		return false, fmt.Errorf("%s is not set", pgtest.DatabaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// TestParallelDatabasesAreDistinct runs two parallel tests that each create a
// database through pgtest and checks that the two databases have different
// names.
func TestParallelDatabasesAreDistinct(t *testing.T) {
	const parallel = 2

	var (
		mu    sync.Mutex
		names []string
	)

	for i := range parallel {
		t.Run(fmt.Sprintf("test-%d", i), func(t *testing.T) {
			t.Parallel()

			pool := pgtest.NewPool(t)

			var name string
			if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
				t.Fatalf("query current_database: %v", err)
			}

			mu.Lock()
			names = append(names, name)
			mu.Unlock()
		})
	}

	// Cleanup on the parent runs after both subtests, including their own
	// cleanups, have finished.
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()

		if len(names) != parallel {
			t.Errorf("got %d database names from %d parallel tests", len(names), parallel)
			return
		}
		if names[0] == names[1] {
			t.Errorf("both parallel tests used database %q", names[0])
		}
	})
}

// TestPoolMaxConnsIsFixed checks that the pool returned by NewPool opens a
// fixed, small number of server connections instead of following the pgxpool
// default of max(4, NumCPU). A per-test pool whose size tracks the CPU
// count multiplies connection use by the number of parallel tests. The wanted
// value is below the default's floor of 4, so a pool that follows the default
// fails this test on every machine, including ones with four cores or fewer.
func TestPoolMaxConnsIsFixed(t *testing.T) {
	pool := pgtest.NewPool(t)

	const wantMaxConns = int32(2)
	if got := pool.Config().MaxConns; got != wantMaxConns {
		t.Errorf("pool MaxConns = %d, want fixed %d independent of the CPU count", got, wantMaxConns)
	}
}

// TestPoolDoesNotHoldAdminConnection checks that NewPool does not keep an
// admin connection to the server's configured database open for the lifetime
// of the test. Only the pool for the per-test database should remain.
//
// The test tags its admin connections with a unique application_name so the
// count is not confused by concurrently running test packages, which use the
// same server.
func TestPoolDoesNotHoldAdminConnection(t *testing.T) {
	adminURL := strings.TrimSpace(os.Getenv(pgtest.DatabaseURLEnv))
	if adminURL == "" {
		t.Fatalf("%s is not set; run make db-up and export %s", pgtest.DatabaseURLEnv, pgtest.DatabaseURLEnv)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse %s: %v", pgtest.DatabaseURLEnv, err)
	}
	adminDatabase := strings.TrimPrefix(u.Path, "/")
	if adminDatabase == "" {
		t.Fatalf("%s must name a database", pgtest.DatabaseURLEnv)
	}

	appName := fmt.Sprintf("pgtest-admin-residency-%d", os.Getpid())
	query := u.Query()
	query.Set("application_name", appName)
	u.RawQuery = query.Encode()
	t.Setenv(pgtest.DatabaseURLEnv, u.String())

	pool := pgtest.NewPool(t)

	// pg_stat_activity is updated asynchronously, so a closed admin connection
	// can remain visible for a moment. Poll briefly with a fixed upper bound
	// instead of failing on the first observation.
	const (
		maxPolls  = 10
		pollDelay = 100 * time.Millisecond
	)

	var count int
	for attempt := 1; ; attempt++ {
		err = pool.QueryRow(t.Context(), `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = $1 AND application_name = $2`, adminDatabase, appName).Scan(&count)
		if err != nil {
			t.Fatalf("query pg_stat_activity: %v", err)
		}
		if count == 0 {
			break
		}
		if attempt == maxPolls {
			t.Errorf("NewPool still holds %d admin connection(s) to %s after %d polls over %s, want 0",
				count, adminDatabase, maxPolls, time.Duration(maxPolls)*pollDelay)
			break
		}
		time.Sleep(pollDelay)
	}
}

// templateNameSeq gives every test invocation in this process a distinct
// template name, so a run with -count>1 does not reuse the template and its
// per-invocation setup counter.
var templateNameSeq atomic.Int64

func uniqueTemplateName(base string) string {
	return fmt.Sprintf("%s_%d", base, templateNameSeq.Add(1))
}

// markerSetup returns a setup function that records that it ran by creating a
// table and inserting one row.
func markerSetup(table string) func(context.Context, *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (value text NOT NULL)"); err != nil {
			return err
		}
		_, err := pool.Exec(ctx, "INSERT INTO "+table+" VALUES ('ok')")
		return err
	}
}

// TestParallelTemplatePoolsAreDistinct runs two parallel tests that copy the
// same template and checks that they get different databases, that both see
// the setup applied, and that setup ran only once in the process.
func TestParallelTemplatePoolsAreDistinct(t *testing.T) {
	const parallel = 2

	name := uniqueTemplateName("parallel_template")
	var setups atomic.Int32
	setup := func(ctx context.Context, pool *pgxpool.Pool) error {
		setups.Add(1)
		return markerSetup("setup_marker")(ctx, pool)
	}

	var (
		mu    sync.Mutex
		names []string
	)

	for i := range parallel {
		t.Run(fmt.Sprintf("test-%d", i), func(t *testing.T) {
			t.Parallel()

			pool := pgtest.NewPoolFromTemplate(t, name, setup)

			var dbName string
			if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&dbName); err != nil {
				t.Fatalf("query current_database: %v", err)
			}

			var value string
			if err := pool.QueryRow(t.Context(), "SELECT value FROM setup_marker").Scan(&value); err != nil {
				t.Fatalf("query setup_marker: %v", err)
			}
			if value != "ok" {
				t.Errorf("setup_marker value = %q, want %q", value, "ok")
			}

			mu.Lock()
			names = append(names, dbName)
			mu.Unlock()
		})
	}

	// Cleanup on the parent runs after both subtests, including their own
	// cleanups, have finished.
	t.Cleanup(func() {
		if got := setups.Load(); got != 1 {
			t.Errorf("setup ran %d times in the process, want 1", got)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(names) != parallel {
			t.Errorf("got %d database names from %d parallel tests", len(names), parallel)
			return
		}
		if names[0] == names[1] {
			t.Errorf("both parallel tests used database %q", names[0])
		}
	})
}

// TestTemplateNameHasProcessSuffix checks that a template database name ends
// with the process suffix: an underscore followed by eight hexadecimal
// digits. The suffix keeps templates of concurrent go test processes apart
// without coordination between them.
func TestTemplateNameHasProcessSuffix(t *testing.T) {
	name := uniqueTemplateName("suffix")
	pool := pgtest.NewPoolFromTemplate(t, name, markerSetup("suffix_marker"))
	template := findTemplateDatabase(t, pool, name)

	suffix := regexp.MustCompile(`_[0-9a-f]{8}$`)
	if !suffix.MatchString(template) {
		t.Errorf("template database name %q does not end with an underscore and eight hex digits", template)
	}
}

// TestTemplateHasNoResidualConnections checks that the template database has
// no sessions while copies are made from it. A template with a live session
// cannot be copied, so a residual connection would either fail the copy or
// force it to wait.
func TestTemplateHasNoResidualConnections(t *testing.T) {
	name := uniqueTemplateName(fmt.Sprintf("resconn_%d", os.Getpid()))
	first := pgtest.NewPoolFromTemplate(t, name, markerSetup("conn_marker"))
	template := findTemplateDatabase(t, first, name)

	// The template pool is closed after setup, but the server may take a
	// moment to reap the session. Wait for zero before sampling copies.
	waitForTemplateConnections(t, first, template, 0)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var sampled atomic.Int32
	samplerErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				samplerErr <- nil
				return
			case <-ticker.C:
				var count int32
				err := first.QueryRow(ctx, `
					SELECT count(*)
					FROM pg_stat_activity
					WHERE datname = $1 AND backend_type = 'client backend'`, template).Scan(&count)
				if err != nil {
					if ctx.Err() != nil {
						samplerErr <- nil
						return
					}
					samplerErr <- fmt.Errorf("sample template connections: %w", err)
					return
				}
				if count > sampled.Load() {
					sampled.Store(count)
				}
			}
		}
	}()

	const copies = 4
	names := make(map[string]struct{}, copies)
	for range copies {
		pool := pgtest.NewPoolFromTemplate(t, name, markerSetup("conn_marker"))

		var dbName string
		if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&dbName); err != nil {
			t.Fatalf("query current_database: %v", err)
		}
		if _, ok := names[dbName]; ok {
			t.Errorf("copy used database %q twice", dbName)
		}
		names[dbName] = struct{}{}
	}

	cancel()
	if err := <-samplerErr; err != nil {
		t.Fatal(err)
	}
	if got := sampled.Load(); got != 0 {
		t.Errorf("template database %s had up to %d client connection(s) while copies were created, want 0", template, got)
	}
}

// TestCopyRetriesWhileTemplateIsInUse holds a session on the template open
// while a copy starts. The server rejects the first CREATE DATABASE with
// SQLSTATE 55006; the copy must retry until the session closes.
func TestCopyRetriesWhileTemplateIsInUse(t *testing.T) {
	name := uniqueTemplateName(fmt.Sprintf("retrybusy_%d", os.Getpid()))
	setup := func(ctx context.Context, pool *pgxpool.Pool) error {
		return markerSetup("retry_marker")(ctx, pool)
	}

	first := pgtest.NewPoolFromTemplate(t, name, setup)
	template := findTemplateDatabase(t, first, name)

	conn, err := pgx.Connect(t.Context(), databaseURLWithName(t, template))
	if err != nil {
		t.Fatalf("connect to template database %s: %v", template, err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(1500 * time.Millisecond)
		_ = conn.Close(context.Background())
	}()
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	copied := pgtest.NewPoolFromTemplate(t, name, setup)

	var dbName string
	if err := copied.QueryRow(t.Context(), "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatalf("query current_database: %v", err)
	}
	if dbName == template {
		t.Errorf("copy used the template database %q itself", template)
	}
	<-released
}

func findTemplateDatabase(t *testing.T, pool *pgxpool.Pool, marker string) string {
	t.Helper()

	var name string
	err := pool.QueryRow(t.Context(), `
		SELECT datname
		FROM pg_database
		WHERE datname LIKE 'pgtest_tpl_%' AND datname LIKE '%' || $1 || '%'
		ORDER BY oid DESC
		LIMIT 1`, marker).Scan(&name)
	if err != nil {
		t.Fatalf("find template database for %q: %v", marker, err)
	}
	return name
}

func waitForTemplateConnections(t *testing.T, pool *pgxpool.Pool, template string, want int32) {
	t.Helper()

	const (
		polls = 50
		delay = 20 * time.Millisecond
	)

	var count int32
	for attempt := 1; ; attempt++ {
		err := pool.QueryRow(t.Context(), `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = $1 AND backend_type = 'client backend'`, template).Scan(&count)
		if err != nil {
			t.Fatalf("query template connections: %v", err)
		}
		if count == want {
			return
		}
		if attempt == polls {
			t.Fatalf("template database %s has %d client connection(s), want %d after %s",
				template, count, want, time.Duration(polls)*delay)
		}
		time.Sleep(delay)
	}
}

func databaseURLWithName(t *testing.T, name string) string {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(pgtest.DatabaseURLEnv))
	if raw == "" {
		t.Fatalf("%s is not set; run make db-up and export %s", pgtest.DatabaseURLEnv, pgtest.DatabaseURLEnv)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", pgtest.DatabaseURLEnv, err)
	}
	u.Path = "/" + name
	u.RawPath = ""
	return u.String()
}

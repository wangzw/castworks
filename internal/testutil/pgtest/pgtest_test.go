package pgtest_test

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

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

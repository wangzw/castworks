//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

// testRunCtx is the parent of every test context. SIGINT and SIGTERM cancel
// it, so a test that is waiting gives up and its t.Cleanup functions run
// normally.
var (
	testRunCtx    context.Context
	testRunCancel context.CancelFunc
)

// TestMain installs the signal handling that turns an interrupt into context
// cancellation instead of an immediate process exit. Without it, go test
// terminates on SIGINT and the cleanup that stops child processes and drops
// the test database never runs. It also runs the tests through pgtest.Main, so
// the pgtest_tpl_* template databases NewPoolFromTemplate created are dropped
// when the tests finish.
func TestMain(m *testing.M) {
	testRunCtx, testRunCancel = context.WithCancel(context.Background())

	// The channel holds two signals: the first cancels the tests so their
	// cleanups run in order, the second exits immediately because the user is
	// asking to stop now.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-signals
		_, _ = fmt.Fprintf(os.Stderr, "acceptance: received %s; cancelling tests so cleanups run (signal again to exit immediately)\n", sig)
		testRunCancel()
		sig = <-signals
		_, _ = fmt.Fprintf(os.Stderr, "acceptance: received %s again; exiting without waiting for cleanup\n", sig)
		os.Exit(1)
	}()

	code := pgtest.Main(m)
	signal.Stop(signals)
	testRunCancel()
	os.Exit(code)
}

// requireEnv returns the value of the named environment variable. An empty
// value is an error, never a skip: a missing variable means the make target
// was invoked wrongly, and silently passing would claim an acceptance result
// that did not happen.
func requireEnv(t testing.TB, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("acceptance: %s is not set; run the make target with the variable set", name)
	}
	return value
}

// optionalEnv returns the trimmed value of the named environment variable, or
// the empty string when it is unset.
func optionalEnv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// testContext returns a context that is cancelled when the test's deadline
// passes or when TestMain observes SIGINT or SIGTERM. The deadline is shorter
// than go test's -timeout: when the timeout fires, go test panics and skips
// Cleanup, whereas a context deadline lets the test fail and clean up first.
func testContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(testRunCtx, timeout)
	t.Cleanup(cancel)
	return ctx
}

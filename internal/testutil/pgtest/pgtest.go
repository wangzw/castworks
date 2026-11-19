// Package pgtest creates isolated PostgreSQL databases for tests.
//
// It connects to the server named by the environment variable
// CASTWORKS_TEST_DATABASE_URL, which deploy/dev/compose.yaml provides:
//
//	postgres://castworks:castworks@127.0.0.1:55433/castworks_test?sslmode=disable
//
// The package exposes these functions:
//
//	func Main(m *testing.M) int
//	func NewPool(t testing.TB) *pgxpool.Pool
//	func NewPoolFromTemplate(t testing.TB, name string, setup func(ctx context.Context, pool *pgxpool.Pool) error) *pgxpool.Pool
//
// NewPool creates a new, empty database on that server, opens a connection
// pool against it and returns the pool. It registers a t.Cleanup that closes
// the pool and drops the database. Every call uses a unique database name, so
// tests, including ones marked t.Parallel, may call it concurrently. When
// CASTWORKS_TEST_DATABASE_URL is unset or empty, NewPool calls t.Fatalf
// instead of skipping.
//
// NewPoolFromTemplate creates each named template database once per process,
// runs setup on it once, and then creates a fresh copy for every call with
// CREATE DATABASE ... TEMPLATE. The first call with a given name runs setup;
// later calls in the same process ignore their setup argument.
//
// Template databases created by a process are dropped by Main after its tests
// finish, so a normal run leaves none behind. Only an abnormal exit leaves a
// template database behind; its name carries the fixed prefix pgtest_tpl_ so
// the leftover can be identified. A package that uses NewPoolFromTemplate
// calls Main from TestMain to get that cleanup:
//
//	func TestMain(m *testing.M) {
//		os.Exit(pgtest.Main(m))
//	}
//
// Copies can fail while the server still considers the template database in
// use by a session that is closing. A copy retries that error (SQLSTATE
// 55006, "is being accessed by other users") up to 8 attempts, waiting
// 50 ms, 100 ms, 200 ms, 400 ms, 800 ms and then 1 s per attempt. Any other
// error fails the test immediately.
//
// Both returned pools have a fixed size: MaxConns is 2, independent of the
// CPU count, so a test run's connection use is predictable. The admin
// connection used to create a database, and the one used to drop it, are
// opened when needed and closed immediately; neither stays open for the
// test's lifetime.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseURLEnv is the environment variable holding the URL of the
// PostgreSQL server that hosts the test databases.
const DatabaseURLEnv = "CASTWORKS_TEST_DATABASE_URL"

const (
	setupTimeout  = 30 * time.Second
	maxNameLength = 63 // PostgreSQL identifier limit in bytes

	// testPoolMaxConns is the fixed maximum number of connections in the pool
	// returned by NewPool and NewPoolFromTemplate. It is deliberately small
	// and does not depend on the CPU count, so parallel tests do not multiply
	// into a large connection count on the test server.
	testPoolMaxConns = 2

	// templateNamePrefix marks every database created by NewPoolFromTemplate.
	// It is fixed so leftover template databases can be found and dropped in
	// bulk after an abnormal process exit.
	templateNamePrefix = "pgtest_tpl_"

	// templateSetupTimeout bounds the template database creation and its
	// setup. It uses a background context because the template is shared by
	// every test in the process, so it must not be cancelled when the test
	// that happens to run first finishes.
	templateSetupTimeout = 30 * time.Second

	// Copying a template retries this many times when the server reports the
	// template as still being accessed by other users. The first wait is
	// cloneRetryInitial and each later wait doubles, capped at
	// cloneRetryMaxDelay, so the total wait is about 3.5 s.
	cloneAttempts      = 8
	cloneRetryInitial  = 50 * time.Millisecond
	cloneRetryMaxDelay = time.Second
)

// NewPool creates a fresh database on the server named by
// CASTWORKS_TEST_DATABASE_URL and returns a pool connected to it. The database
// is dropped and the pool closed when the test finishes.
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()

	adminURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if adminURL == "" {
		t.Fatalf("pgtest: %s is not set; run make db-up and export %s", DatabaseURLEnv, DatabaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(t.Context(), setupTimeout)
	defer cancel()

	name, err := newDatabaseName(t.Name())
	if err != nil {
		t.Fatalf("pgtest: generate database name: %v", err)
	}

	if err := createDatabase(ctx, adminURL, name); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}

	testURL, err := databaseURL(adminURL, name)
	if err != nil {
		_ = dropDatabase(ctx, adminURL, name)
		t.Fatalf("pgtest: build database URL for %s: %v", name, err)
	}

	poolConfig, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		_ = dropDatabase(ctx, adminURL, name)
		t.Fatalf("pgtest: parse database URL for %s: %v", name, err)
	}
	poolConfig.MaxConns = testPoolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		_ = dropDatabase(ctx, adminURL, name)
		t.Fatalf("pgtest: create pool for %s: %v", name, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_ = dropDatabase(ctx, adminURL, name)
		t.Fatalf("pgtest: connect to database %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()

		// t.Context() is cancelled before cleanup runs, so use a fresh context.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		if err := dropDatabase(cleanupCtx, adminURL, name); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})

	return pool
}

// NewPoolFromTemplate creates a fresh copy of the template database named by
// name and returns a pool connected to it, behaving like NewPool otherwise.
//
// The first call with a given name in this process creates the template
// database, runs setup on it and closes the template's pool. Later calls with
// the same name reuse the template and ignore setup. Concurrent callers wait
// for the first one to finish. If template creation or setup fails, every
// caller in the process fails with that error.
//
// Each copy is dropped and its pool closed when the test finishes.
func NewPoolFromTemplate(t testing.TB, name string, setup func(context.Context, *pgxpool.Pool) error) *pgxpool.Pool {
	t.Helper()

	adminURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if adminURL == "" {
		t.Fatalf("pgtest: %s is not set; run make db-up and export %s", DatabaseURLEnv, DatabaseURLEnv)
	}
	if setup == nil {
		t.Fatalf("pgtest: NewPoolFromTemplate(%q): setup is nil", name)
	}

	template, err := templateFor(adminURL, name, setup)
	if err != nil {
		t.Fatalf("pgtest: prepare template database %q: %v", name, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), setupTimeout)
	defer cancel()

	dbName, err := newDatabaseName(t.Name())
	if err != nil {
		t.Fatalf("pgtest: generate database name: %v", err)
	}

	if err := cloneDatabase(ctx, adminURL, template, dbName); err != nil {
		t.Fatalf("pgtest: create database %s from template %s: %v", dbName, template, err)
	}

	testURL, err := databaseURL(adminURL, dbName)
	if err != nil {
		_ = dropDatabase(ctx, adminURL, dbName)
		t.Fatalf("pgtest: build database URL for %s: %v", dbName, err)
	}

	poolConfig, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		_ = dropDatabase(ctx, adminURL, dbName)
		t.Fatalf("pgtest: parse database URL for %s: %v", dbName, err)
	}
	poolConfig.MaxConns = testPoolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		_ = dropDatabase(ctx, adminURL, dbName)
		t.Fatalf("pgtest: create pool for %s: %v", dbName, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_ = dropDatabase(ctx, adminURL, dbName)
		t.Fatalf("pgtest: connect to database %s: %v", dbName, err)
	}

	t.Cleanup(func() {
		pool.Close()

		// t.Context() is cancelled before cleanup runs, so use a fresh context.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		if err := dropDatabase(cleanupCtx, adminURL, dbName); err != nil {
			t.Errorf("pgtest: drop database %s: %v", dbName, err)
		}
	})

	return pool
}

// Main runs the tests in m and drops the template databases this process
// created afterwards. A package that uses NewPoolFromTemplate calls it from
// TestMain:
//
//	func TestMain(m *testing.M) {
//		os.Exit(pgtest.Main(m))
//	}
//
// The exit code is the one m.Run returns, made non-zero when a template
// database cannot be dropped.
func Main(m *testing.M) int {
	code := m.Run()
	if err := dropTemplates(); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// dropTemplates drops every template database this process created.
func dropTemplates() error {
	adminURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if adminURL == "" {
		return nil
	}

	templates.mu.Lock()
	created := append([]string(nil), templates.created...)
	templates.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	var errs []error
	for _, name := range created {
		if err := dropDatabase(ctx, adminURL, name); err != nil {
			errs = append(errs, fmt.Errorf("drop template database %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// templates tracks the template database for each name used in this process.
// The entry is stored before creation starts so concurrent callers wait on
// ready instead of creating a second template.
var templates = struct {
	mu      sync.Mutex
	entries map[string]*templateEntry
	created []string // template databases successfully built, for Main to drop
}{entries: make(map[string]*templateEntry)}

type templateEntry struct {
	ready chan struct{}
	name  string
	err   error
}

// templateFor returns the template database name for name, creating it on the
// first call. The returned name is the bare database identifier, not a URL.
func templateFor(adminURL, name string, setup func(context.Context, *pgxpool.Pool) error) (string, error) {
	templates.mu.Lock()
	entry, ok := templates.entries[name]
	if !ok {
		entry = &templateEntry{ready: make(chan struct{})}
		templates.entries[name] = entry
		templates.mu.Unlock()

		// setup may call runtime.Goexit through t.Fatalf or t.Skip. Closing on
		// return would leave every later caller waiting forever, so wake them
		// from a deferred call instead.
		defer close(entry.ready)

		entry.name, entry.err = buildTemplate(adminURL, name, setup)
		if entry.err == nil {
			templates.mu.Lock()
			templates.created = append(templates.created, entry.name)
			templates.mu.Unlock()
		}
		return entry.name, entry.err
	}
	templates.mu.Unlock()

	<-entry.ready
	return entry.name, entry.err
}

// buildTemplate creates the template database and runs setup on it. The
// template's pool is closed before returning, so copying it does not trip the
// server's check for other sessions using the template.
func buildTemplate(adminURL, name string, setup func(context.Context, *pgxpool.Pool) error) (string, error) {
	templateName := newTemplateDatabaseName(name)

	ctx, cancel := context.WithTimeout(context.Background(), templateSetupTimeout)
	defer cancel()

	if err := createDatabase(ctx, adminURL, templateName); err != nil {
		return "", fmt.Errorf("create template database %s: %w", templateName, err)
	}
	created := false
	defer func() {
		if created {
			return
		}
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()
		_ = dropDatabase(dropCtx, adminURL, templateName)
	}()

	templateURL, err := databaseURL(adminURL, templateName)
	if err != nil {
		return "", err
	}
	poolConfig, err := pgxpool.ParseConfig(templateURL)
	if err != nil {
		return "", err
	}
	poolConfig.MaxConns = testPoolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return "", err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return "", err
	}
	if err := setup(ctx, pool); err != nil {
		pool.Close()
		return "", fmt.Errorf("run setup: %w", err)
	}

	// Close waits for every connection to be returned and closed. The server
	// may still report a session for a moment, which cloneDatabase retries.
	pool.Close()

	created = true
	return templateName, nil
}

// cloneDatabase creates name as a copy of the template database. A session
// still closing on the template makes the server reject the copy with
// SQLSTATE 55006; that error is retried with exponential backoff. Every other
// error is returned immediately.
func cloneDatabase(ctx context.Context, adminURL, templateName, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	query := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE " + pgx.Identifier{templateName}.Sanitize()

	delay := cloneRetryInitial
	var lastErr error
	for attempt := 1; attempt <= cloneAttempts; attempt++ {
		_, err := conn.Exec(ctx, query)
		if err == nil {
			return nil
		}
		if !templateInUse(err) {
			return err
		}
		lastErr = err
		if attempt == cloneAttempts {
			break
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("copy template %s: %w (last error: %v)", templateName, ctx.Err(), lastErr)
		case <-timer.C:
		}
		if delay < cloneRetryMaxDelay {
			delay *= 2
			if delay > cloneRetryMaxDelay {
				delay = cloneRetryMaxDelay
			}
		}
	}

	return fmt.Errorf("template %s still in use after %d attempts: %w", templateName, cloneAttempts, lastErr)
}

// templateInUse reports whether err is the server's refusal to copy a
// template database because another session is connected to it.
func templateInUse(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55006" {
		return false
	}
	return strings.Contains(pgErr.Message, "is being accessed by other users")
}

// processSuffix is the random suffix shared by every template database this
// process creates. It keeps template names of concurrent go test processes
// from colliding without needing coordination between them.
var processSuffix = sync.OnceValue(func() string {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("%08x", os.Getpid())
	}
	return hex.EncodeToString(random[:])
})

// newTemplateDatabaseName builds the fixed-prefix name of a template
// database. The sanitized test-visible name is truncated so the process
// suffix always fits within the identifier limit.
func newTemplateDatabaseName(name string) string {
	suffix := processSuffix()
	base := sanitize(name)

	maxBase := maxNameLength - len(templateNamePrefix) - len(suffix) - 1
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	if base == "" {
		return templateNamePrefix + suffix
	}
	return templateNamePrefix + base + "_" + suffix
}

func createDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	return err
}

func dropDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}

func newDatabaseName(testName string) (string, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}

	prefix := sanitize(testName)
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}

	name := "pgtest_" + prefix + "_" + hex.EncodeToString(random[:])
	if len(name) > maxNameLength {
		name = name[:maxNameLength]
	}
	return name, nil
}

func sanitize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func databaseURL(rawURL, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = "/" + name
	u.RawPath = ""
	return u.String(), nil
}

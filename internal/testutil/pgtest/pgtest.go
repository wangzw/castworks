// Package pgtest creates isolated PostgreSQL databases for tests.
//
// It connects to the server named by the environment variable
// CASTWORKS_TEST_DATABASE_URL, which deploy/dev/compose.yaml provides:
//
//	postgres://castworks:castworks@127.0.0.1:55432/castworks_test?sslmode=disable
//
// The package exposes one function:
//
//	func NewPool(t testing.TB) *pgxpool.Pool
//
// NewPool creates a new, empty database on that server, opens a connection
// pool against it and returns the pool. It registers a t.Cleanup that closes
// the pool and drops the database. Every call uses a unique database name, so
// tests, including ones marked t.Parallel, may call it concurrently. When
// CASTWORKS_TEST_DATABASE_URL is unset or empty, NewPool calls t.Fatalf
// instead of skipping.
//
// The returned pool has a fixed size: MaxConns is 2, independent of the CPU
// count, so a test run's connection use is predictable. The admin connection
// used to create the database, and the one used to drop it, are opened when
// needed and closed immediately; neither stays open for the test's lifetime.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseURLEnv is the environment variable holding the URL of the
// PostgreSQL server that hosts the test databases.
const DatabaseURLEnv = "CASTWORKS_TEST_DATABASE_URL"

const (
	setupTimeout  = 30 * time.Second
	maxNameLength = 63 // PostgreSQL identifier limit in bytes

	// testPoolMaxConns is the fixed maximum number of connections in the pool
	// returned by NewPool. It is deliberately small and does not depend on the
	// CPU count, so parallel tests do not multiply into a large connection
	// count on the test server.
	testPoolMaxConns = 2
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

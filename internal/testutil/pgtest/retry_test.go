package pgtest

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTemplateSetupGoexitReleasesWaiters checks that a setup function that
// calls runtime.Goexit, as t.Fatalf and t.Skip do, still releases the callers
// waiting for the same template. Otherwise every later call in the process
// blocks forever.
func TestTemplateSetupGoexitReleasesWaiters(t *testing.T) {
	adminURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if adminURL == "" {
		t.Fatalf("%s is not set; run make db-up and export %s", DatabaseURLEnv, DatabaseURLEnv)
	}

	started := make(chan struct{})
	setup := func(context.Context, *pgxpool.Pool) error {
		close(started)
		runtime.Goexit()
		return nil
	}
	name := "goexit"

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = templateFor(adminURL, name, setup)
	}()

	<-started

	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		_, _ = templateFor(adminURL, name, setup)
	}()

	select {
	case <-waiterDone:
	case <-time.After(10 * time.Second):
		t.Fatal("a caller waiting for the template still blocks after setup called runtime.Goexit")
	}
	<-firstDone
}

// TestCloneDatabaseDoesNotRetryOtherErrors checks that an error other than
// SQLSTATE 55006 is returned from the first attempt instead of being retried
// with backoff. Creating a database that already exists fails with SQLSTATE
// 42P04; a retry loop would spend about 3.5 seconds before returning it.
func TestCloneDatabaseDoesNotRetryOtherErrors(t *testing.T) {
	adminURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if adminURL == "" {
		t.Fatalf("%s is not set; run make db-up and export %s", DatabaseURLEnv, DatabaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	template, err := newDatabaseName("clone_no_retry_template")
	if err != nil {
		t.Fatalf("generate template name: %v", err)
	}
	target, err := newDatabaseName("clone_no_retry_target")
	if err != nil {
		t.Fatalf("generate target name: %v", err)
	}

	if err := createDatabase(ctx, adminURL, template); err != nil {
		t.Fatalf("create template database: %v", err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()
		_ = dropDatabase(dropCtx, adminURL, template)
	}()

	if err := createDatabase(ctx, adminURL, target); err != nil {
		t.Fatalf("create target database: %v", err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()
		_ = dropDatabase(dropCtx, adminURL, target)
	}()

	start := time.Now()
	err = cloneDatabase(ctx, adminURL, template, target)
	elapsed := time.Since(start)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P04" {
		t.Fatalf("cloneDatabase error = %v, want SQLSTATE 42P04 (database already exists)", err)
	}
	// A retried error waits at least cloneRetryInitial and, over all attempts,
	// about 3.5 seconds. A non-retryable error returns from the first attempt.
	if elapsed > 2*time.Second {
		t.Errorf("cloneDatabase took %s for a non-retryable error; it must return it immediately", elapsed)
	}
}

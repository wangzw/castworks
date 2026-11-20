package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakeclock"
)

// assemblyTestStart is the fixed start of the fake clock used by the package
// tests.
var assemblyTestStart = time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC)

// assemblyTestClock adapts fakeclock to the shared Clock interface.
type assemblyTestClock struct {
	*fakeclock.Clock
}

func (c assemblyTestClock) NewTimer(d time.Duration) Timer {
	return c.Clock.NewTimer(d)
}

func newAssemblyTestClock() assemblyTestClock {
	return assemblyTestClock{Clock: fakeclock.New(assemblyTestStart)}
}

// writeTestKey writes an HMAC key file and returns its path.
func writeTestKey(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signing-key")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing signing key: %v", err)
	}
	return path
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// durationPtr returns a pointer to d for tests that pass an explicit job
// duration to JobService.Submit.
func durationPtr(d time.Duration) *time.Duration {
	return &d
}

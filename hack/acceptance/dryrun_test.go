//go:build acceptance

package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

// TestDryRun exercises the orchestration and cleanup of the local acceptance
// path without spending LLM quota: the control plane runs with --provider
// local, a fake Claude Code plays the stream-json side, and the test drives
// two turns and End.
//
// It proves that the harness builds and starts the binaries, listens on the
// right addresses, creates and ends a run, and cleans up. It says nothing
// about the behavior of the real Claude Code; that is TestLocal's job.
func TestDryRun(t *testing.T) {
	ctx := testContext(t, 5*time.Minute)

	requireEnv(t, envDatabaseURL)
	pool := pgtest.NewPool(t)

	h := newHarness(t, harnessOptions{
		DatabaseURL: pool.Config().ConnString(),
		Provider:    "local",
	})
	h.buildBinaries()

	credentialPath := filepath.Join(h.workDir, "llm-credential")
	if err := os.WriteFile(credentialPath, []byte("acceptance-fake-token\n"), 0o600); err != nil {
		t.Fatalf("acceptance: write fake credential: %v", err)
	}

	h.startControlPlane(ctx,
		"--provider", "local",
		"--http-addr", "127.0.0.1:0",
		"--grpc-addr", "127.0.0.1:0",
		"--llm-credential-kind", "oauth_token",
		"--llm-credential-file", credentialPath,
		"--shutdown-timeout", "10s",
	)

	nonce := "dryrun-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	runID := h.createRun(ctx, fmt.Sprintf("Reply with the token %s exactly.", nonce))

	claudeBin := filepath.Join(h.repoRoot, "hack", "acceptance", "testdata", "fake-claude", "fake-claude")
	info, err := os.Stat(claudeBin)
	if err != nil {
		t.Fatalf("acceptance: fake Claude Code: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("acceptance: fake Claude Code %s is not executable", claudeBin)
	}
	h.startWardenLocal(runID, claudeBin)

	first := h.waitTurnBoundary(ctx, runID, 1)
	if len(first.Turns) < 1 {
		t.Fatalf("acceptance: first turn missing: %+v", first)
	}
	if first.Turns[0].IsError {
		t.Fatalf("acceptance: first turn is an error: %q", first.Turns[0].Output)
	}
	if !strings.Contains(first.Turns[0].Output, nonce) {
		t.Fatalf("acceptance: first turn output %q does not contain %q", first.Turns[0].Output, nonce)
	}

	h.send(ctx, runID, fmt.Sprintf("Reply with the token %s again.", nonce))

	second := h.waitTurnBoundary(ctx, runID, 2)
	if len(second.Turns) != 2 {
		t.Fatalf("acceptance: expected 2 turns, got %d", len(second.Turns))
	}
	for i, finished := range second.Turns {
		if finished.IsError {
			t.Fatalf("acceptance: turn %d is an error: %q", i+1, finished.Output)
		}
		if !strings.Contains(finished.Output, nonce) {
			t.Fatalf("acceptance: turn %d output %q does not contain %q", i+1, finished.Output, nonce)
		}
	}

	if err := h.endRun(ctx, runID); err != nil {
		t.Fatalf("acceptance: end run: %v", err)
	}
	if err := h.waitChild(h.warden, 30*time.Second); err != nil {
		t.Fatalf("acceptance: warden exited with an error: %v", err)
	}
}

//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestKubectlAteSeparatesStderrFromStdout reproduces the defect where
// kubectlAte returned stderr warnings as part of its stdout. kubectl ate
// writes a "no --token-file given" warning to stderr on every invocation, so
// a combined capture made every JSON response undecodable.
func TestKubectlAteSeparatesStderrFromStdout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' 'Warning: no --token-file given; acting as the shared identity system:serviceaccount:ate-system:ate-client rather than as yourself.' >&2\n" +
		"printf '%s\\n' '{\"workers\":[]}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := kubectlAte(context.Background(), "", "ate", "get", "workers")
	if err != nil {
		t.Fatalf("kubectlAte: %v", err)
	}
	if strings.Contains(out, "Warning:") {
		t.Fatalf("kubectlAte returned stderr in its output: %q", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("kubectlAte output is not JSON: %q", out)
	}
}

// TestWaitActorGoneUsesSupportedOutputFormat reproduces the defect where the
// actor-gone poll asked kubectl ate for "-o name", a format the plugin does
// not support. Every poll failed with an invalid-format error, which is not a
// missing actor, so the wait retried until its deadline.
func TestWaitActorGoneUsesSupportedOutputFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "name" ]; then
    printf '%s\n' 'Error: invalid output format "name". Must be one of: table, json, yaml' >&2
    exit 1
  fi
done
printf '%s\n' 'Error: failed to get actor "cw-test": rpc error: code = NotFound desc = Actor castworks-dev/cw-test not found' >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := waitActorGone(context.Background(), "", "cw-test", 3*time.Second); err != nil {
		t.Fatalf("waitActorGone did not accept a missing actor: %v", err)
	}
}

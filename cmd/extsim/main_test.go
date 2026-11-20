package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extsimTestBinary is the extsim binary built once for the subprocess tests.
var extsimTestBinary string

// TestMain builds the extsim binary so the serve tests can start it as a real
// subprocess. Building here keeps the binary out of every test's temporary
// directory and keeps the subprocess identical to the shipped command.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "extsim-test-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "extsim tests: create temp dir: %v\n", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "extsim")

	wd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "extsim tests: getwd: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = wd
	if out, err := build.CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "extsim tests: go build: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	extsimTestBinary = bin

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "no command", wantCode: 2, wantErr: "want serve or job"},
		{name: "unknown command", args: []string{"bogus"}, wantCode: 2, wantErr: `unknown command "bogus"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr, func(string) string { return "" })
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantErr)
			}
		})
	}
}

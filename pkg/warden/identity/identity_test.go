package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cases := []struct {
		name      string
		content   string
		wantRunID string
		wantErr   bool
	}{
		{name: "valid", content: "cw-run-1", wantRunID: "run-1"},
		{name: "missing prefix", content: "run-1", wantErr: true},
		{name: "empty run id", content: "cw-", wantErr: true},
		{name: "trailing newline", content: "cw-run-1\n", wantErr: true},
	}

	for _, tc := range cases {
		path := filepath.Join(dir, tc.name+".txt")
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runID, err := Read(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Read(%q) = %q, want error", tc.content, runID)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read(%q): %v", tc.content, err)
			}
			if runID != tc.wantRunID {
				t.Fatalf("Read(%q) = %q, want %q", tc.content, runID, tc.wantRunID)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		if runID, err := Read(filepath.Join(dir, "absent")); err == nil {
			t.Fatalf("Read(absent) = %q, want error", runID)
		}
	})
}

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		actorName string
		wantRunID string
		wantErr   bool
	}{
		{name: "valid", actorName: "cw-0b9f6e2a", wantRunID: "0b9f6e2a"},
		{name: "internal space", actorName: "cw-run 1", wantErr: true},
		{name: "internal tab", actorName: "cw-run\t1", wantErr: true},
		{name: "leading whitespace", actorName: " cw-run-1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runID, err := Parse(tc.actorName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %q, want error", tc.actorName, runID)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.actorName, err)
			}
			if runID != tc.wantRunID {
				t.Fatalf("Parse(%q) = %q, want %q", tc.actorName, runID, tc.wantRunID)
			}
		})
	}
}

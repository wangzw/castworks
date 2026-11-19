package sandboxprovider

import "testing"

func TestActorName(t *testing.T) {
	cases := []struct {
		name  string
		runID string
		want  string
	}{
		{"uuid", "0b9f6e2a-4f1c-4d5e-8a7b-3c2d1e0f9a8b", "cw-0b9f6e2a-4f1c-4d5e-8a7b-3c2d1e0f9a8b"},
		{"simple", "run-1", "cw-run-1"},
		{"empty", "", "cw-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActorName(tc.runID); got != tc.want {
				t.Fatalf("ActorName(%q) = %q, want %q", tc.runID, got, tc.want)
			}
		})
	}
}

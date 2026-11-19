package main

import (
	"strings"
	"testing"
)

// TestWriteOutputTrimsOneTrailingNewline pins the trimming rule from the
// milestone document: one trailing newline is dropped before the two-space
// prefix is added, so a second trailing newline survives as a blank line.
func TestWriteOutputTrimsOneTrailingNewline(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"empty", "", ""},
		{"only a newline", "\n", ""},
		{"no trailing newline", "one", "  one\n"},
		{"one trailing newline", "one\n", "  one\n"},
		{"two trailing newlines", "one\n\n", "  one\n\n"},
		{"blank line between", "one\n\ntwo\n", "  one\n\n  two\n"},
		{"indented lines", "  one\n    two\n", "    one\n      two\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			writeOutput(&b, tc.output)
			if b.String() != tc.want {
				t.Errorf("writeOutput(%q) = %q, want %q", tc.output, b.String(), tc.want)
			}
		})
	}
}

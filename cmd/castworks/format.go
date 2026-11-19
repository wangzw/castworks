package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/wangzw/castworks/pkg/humanchannel/api"
)

// dash stands in for a value that is absent: an empty substate, a turn that
// has not ended, or a suspension phase that has not completed.
const dash = "-"

// substateText renders a substate, showing the dash for the empty string.
func substateText(substate string) string {
	if substate == "" {
		return dash
	}
	return substate
}

// endedAtText renders a turn's end time, showing the dash when it is nil.
func endedAtText(t *api.Time) string {
	if t == nil {
		return dash
	}
	return formatTime(*t)
}

// millisText renders an optional millisecond count, showing the dash when it
// is nil.
func millisText(ms *int64) string {
	if ms == nil {
		return dash
	}
	return fmt.Sprintf("%d", *ms)
}

// writeOutput writes a turn output with each line prefixed by two spaces. One
// trailing newline is dropped first; blank lines stay blank and an empty
// output writes nothing.
func writeOutput(w io.Writer, output string) {
	output = strings.TrimSuffix(output, "\n")
	if output == "" {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			_, _ = fmt.Fprintln(w)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}

// writeDetail writes the multi-line report of "run get".
func writeDetail(w io.Writer, detail api.RunDetail) {
	_, _ = fmt.Fprintf(w, "run_id: %s\n", detail.RunID)
	_, _ = fmt.Fprintf(w, "state: %s\n", detail.State)
	_, _ = fmt.Fprintf(w, "substate: %s\n", substateText(detail.Substate))
	if detail.FailureReason != "" {
		_, _ = fmt.Fprintf(w, "failure_reason: %s\n", detail.FailureReason)
	}
	_, _ = fmt.Fprintf(w, "turns: %d\n", len(detail.Turns))
	for i, turn := range detail.Turns {
		_, _ = fmt.Fprintf(w, "turn %d  %s  started %s  ended %s  is_error %t\n",
			i+1, turn.TurnID, formatTime(turn.StartedAt), endedAtText(turn.EndedAt), turn.IsError)
		writeOutput(w, turn.Output)
	}
	_, _ = fmt.Fprintf(w, "suspensions: %d\n", len(detail.Suspensions))
	for i, s := range detail.Suspensions {
		_, _ = fmt.Fprintf(w, "suspension %d  started %s  snapshot_ms %s  restore_ms %s  hello_ms %s\n",
			i+1, formatTime(s.StartedAt), millisText(s.SnapshotMS), millisText(s.RestoreMS), millisText(s.HelloMS))
		if s.Error != "" {
			_, _ = fmt.Fprintf(w, "  error: %s\n", s.Error)
		}
	}
}

// writeNewTurns prints the turns that have ended and have not been printed
// yet, oldest StartedAt first, recording each one in printed. A turn is only
// ever printed once.
func writeNewTurns(w io.Writer, turns []api.Turn, printed map[string]bool) {
	pending := make([]api.Turn, 0, len(turns))
	for _, turn := range turns {
		if turn.EndedAt != nil && !printed[turn.TurnID] {
			pending = append(pending, turn)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		return pending[i].StartedAt.Before(pending[j].StartedAt.Time)
	})
	for _, turn := range pending {
		_, _ = fmt.Fprintf(w, "turn %s is_error=%t\n", turn.TurnID, turn.IsError)
		writeOutput(w, turn.Output)
		printed[turn.TurnID] = true
	}
}

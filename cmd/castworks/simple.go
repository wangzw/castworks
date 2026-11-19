package main

import (
	"context"
	"fmt"
	"text/tabwriter"
)

// runCreate implements "run create <prompt>".
func runCreate(ctx context.Context, env *environment, args []string) int {
	runID, err := env.client.Create(ctx, args[0])
	if err != nil {
		return reportError(env, err)
	}
	_, _ = fmt.Fprintln(env.stdout, runID)
	return 0
}

// runSend implements "run send <run_id> <text>".
func runSend(ctx context.Context, env *environment, args []string) int {
	seq, err := env.client.Send(ctx, args[0], args[1])
	if err != nil {
		return reportError(env, err)
	}
	_, _ = fmt.Fprintln(env.stdout, seq)
	return 0
}

// runEnd implements "run end <run_id>".
func runEnd(ctx context.Context, env *environment, args []string) int {
	if err := env.client.End(ctx, args[0]); err != nil {
		return reportError(env, err)
	}
	return 0
}

// runList implements "run list".
func runList(ctx context.Context, env *environment, args []string) int {
	runs, err := env.client.List(ctx)
	if err != nil {
		return reportError(env, err)
	}
	tw := tabwriter.NewWriter(env.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RUN_ID\tSTATE\tSUBSTATE\tCREATED_AT")
	for _, r := range runs {
		substate := r.Substate
		if substate == "" {
			substate = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.RunID, r.State, substate, formatTime(r.CreatedAt))
	}
	// The writer is backed by the injected stdout writer; a flush failure
	// cannot be reported as a command error any more usefully than a failed
	// write of the earlier lines.
	_ = tw.Flush()
	return 0
}

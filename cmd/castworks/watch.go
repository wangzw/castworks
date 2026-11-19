package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/wangzw/castworks/internal/humanchannel/client"
)

// watchPollInterval is the delay between two watch polls. It is a variable so
// tests can lower it; the command line does not expose it.
var watchPollInterval = time.Second

func init() {
	registerCommand("watch", 1, runWatch)
}

// runWatch implements "run watch <run_id>": it polls the run every
// watchPollInterval, prints each turn once after it ends and prints a state
// line whenever the state or substate changes. It returns 0 when the run
// ends, 1 when it fails and 130 when ctx is cancelled.
func runWatch(ctx context.Context, env *environment, args []string) int {
	runID := args[0]
	printed := make(map[string]bool)
	lastState, lastSubstate := "", ""
	haveStatus := false
	retrying := false
	for {
		detail, err := env.client.Get(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return 130
			}
			// Only connection errors and 5xx responses are retried, with a
			// single warning per run of consecutive failures. A 4xx, a 2xx
			// response whose body does not decode and every other error is
			// permanent: exit at once.
			if !retryable(err) {
				return reportError(env, err)
			}
			if !retrying {
				_, _ = fmt.Fprintf(env.stderr, "castworks: %s, retrying\n", errorMessage(err))
				retrying = true
			}
			if !pollWait(ctx) {
				return 130
			}
			continue
		}
		retrying = false

		writeNewTurns(env.stdout, detail.Turns, printed)
		if !haveStatus || detail.State != lastState || detail.Substate != lastSubstate {
			_, _ = fmt.Fprintf(env.stdout, "state=%s substate=%s\n", detail.State, substateText(detail.Substate))
			lastState, lastSubstate = detail.State, detail.Substate
			haveStatus = true
		}

		switch detail.State {
		case "ended":
			return 0
		case "failed":
			_, _ = fmt.Fprintf(env.stdout, "failure_reason: %s\n", detail.FailureReason)
			_, _ = fmt.Fprintln(env.stderr, "castworks: run failed")
			return 1
		}

		if !pollWait(ctx) {
			return 130
		}
	}
}

// pollWait sleeps for watchPollInterval and reports whether the wait was
// interrupted by ctx being cancelled.
func pollWait(ctx context.Context) bool {
	timer := time.NewTimer(watchPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryable reports whether a polling error is worth another poll: connection
// errors, which net/http reports as *url.Error, and 5xx responses. A 4xx, a
// 2xx response whose body does not decode and every other error is permanent.
func retryable(err error) bool {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

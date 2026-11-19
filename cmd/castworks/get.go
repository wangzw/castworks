package main

import "context"

func init() {
	registerCommand("get", 1, runGet)
}

// runGet implements "run get <run_id>": it prints the run's state, turns and
// suspensions.
func runGet(ctx context.Context, env *environment, args []string) int {
	detail, err := env.client.Get(ctx, args[0])
	if err != nil {
		return reportError(env, err)
	}
	writeDetail(env.stdout, detail)
	return 0
}

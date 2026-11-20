// Command extsim is a simulated external system for the castworks development
// and acceptance environments. It runs asynchronous jobs and calls back to
// connectorhub when they finish.
//
// The first argument selects the mode: "serve" starts the job and test-control
// interfaces, and "job" runs the command line used by agents inside a sandbox.
package main

import (
	"fmt"
	"io"
	"os"
)

// extsimConfigPath is where the actor image writes the command line defaults.
const extsimConfigPath = "/etc/extsim.json"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run dispatches on the first argument and returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "extsim: missing command; want serve or job")
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "job":
		return runJobCLI(args[1:], stdin, stdout, stderr, getenv, extsimConfigPath)
	default:
		_, _ = fmt.Fprintf(stderr, "extsim: unknown command %q; want serve or job\n", args[0])
		return 2
	}
}

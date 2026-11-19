// Command castworks is the humanchannel command-line client.
//
// Process wiring lives in main: it wires os.Args, the standard streams and a
// context cancelled by os.Interrupt into run, and exits with run's return
// value. run parses the command line, builds the HTTP client and dispatches
// to the command named after "run". The exit codes are:
//
//	0    success
//	1    runtime error (the server returned an error, the server is
//	     unreachable, a run failed, ...)
//	2    usage error
//	130  interrupted
//
// # Adding a command
//
// A command is a function with the shape
//
//	func(ctx context.Context, env *environment, args []string) int
//
// declared in the commands table. env.stdout and env.stderr replace
// os.Stdout and os.Stderr, env.client is the already constructed HTTP
// client and args are the positional arguments after the subcommand name.
// A command writes all of its output through env, never through os.Stdout or
// os.Stderr, never calls os.Exit, and returns the process exit code.
//
// A file added later registers its command by calling registerCommand from
// init. For example:
//
//	func init() {
//		registerCommand("get", 1, runGet)
//	}
//
// errorMessage renders an error the way commands report it; reportError
// writes the one-line "castworks: ..." message to env.stderr and returns exit
// code 1. formatTime renders an api.Time in the format the API uses on the
// wire, converted to UTC so commands print the same instant regardless of the
// timezone the server's timestamp carries.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/wangzw/castworks/internal/humanchannel/api"
	"github.com/wangzw/castworks/internal/humanchannel/client"
)

// environment is what the process gives a command: the injected streams, the
// server address and the already constructed client.
type environment struct {
	stdout io.Writer
	stderr io.Writer
	server string
	client *client.Client
}

// commandFunc implements one subcommand of "castworks run".
type commandFunc func(ctx context.Context, env *environment, args []string) int

// command is a subcommand: how many positional arguments it takes and the
// function that runs it.
type command struct {
	nargs int
	run   commandFunc
}

// commands is the dispatch table. Commands implemented in their own files
// register themselves through registerCommand from init.
var commands = map[string]command{
	"create": {nargs: 1, run: runCreate},
	"send":   {nargs: 2, run: runSend},
	"end":    {nargs: 1, run: runEnd},
	"list":   {nargs: 0, run: runList},
}

// registerCommand installs fn as the command with the given name. Call it from
// init in the file that implements the command.
func registerCommand(name string, nargs int, fn commandFunc) {
	commands[name] = command{nargs: nargs, run: fn}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses argv, builds the client and runs the selected command. It is the
// single entry point tests call; main only wires the process streams into it.
func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	inv, err := parseArgs(argv)
	if err != nil {
		return usageError(stderr, err)
	}
	if inv.help {
		_, _ = fmt.Fprint(stdout, usageText)
		return 0
	}
	c, err := client.New(inv.server, nil)
	if err != nil {
		return usageError(stderr, err)
	}
	cmd := commands[inv.command]
	if len(inv.args) != cmd.nargs {
		return usageError(stderr, fmt.Errorf("run %s: took %d argument(s), want %d", inv.command, len(inv.args), cmd.nargs))
	}
	env := &environment{stdout: stdout, stderr: stderr, server: inv.server, client: c}
	return cmd.run(ctx, env, inv.args)
}

// usageError writes a one-line error and the usage text to stderr and returns
// exit code 2.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "castworks: %v\n", err)
	_, _ = fmt.Fprint(stderr, usageText)
	return 2
}

// reportError writes a one-line "castworks: ..." message to stderr and
// returns exit code 1.
func reportError(env *environment, err error) int {
	_, _ = fmt.Fprintf(env.stderr, "castworks: %s\n", errorMessage(err))
	return 1
}

// errorMessage renders an error for the command line. A server error is
// reported as "server returned <status>: <message>"; any other error is
// printed as is, so a transport error keeps the server address it names.
func errorMessage(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("server returned %d: %s", apiErr.Status, apiErr.Message)
	}
	return err.Error()
}

// timeLayout renders a timestamp the way the API sends it: RFC 3339 with
// fixed millisecond precision. Times are converted to UTC first, so the zone
// is always Z.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// formatTime prints an api.Time in the API's wire format. It converts the
// time to UTC first, then formats it with fixed millisecond precision.
func formatTime(t api.Time) string {
	return t.UTC().Format(timeLayout)
}

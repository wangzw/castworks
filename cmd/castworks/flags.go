package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// defaultServer is the API address used when --server is absent.
const defaultServer = "http://127.0.0.1:8080"

const usageText = `usage: castworks [--server URL] run <command> [--server URL] [--] [args...]

commands:
  create <prompt>        create a run and print its run_id
  send <run_id> <text>   send a message to a run and print its inbox sequence
  get <run_id>           print a run's state, turns and suspensions
  watch <run_id>         follow a run until it ends
  list                   list runs
  end <run_id>           end a run

options:
  --server URL           API server address (default http://127.0.0.1:8080)
  -h, --help             print this help
`

// invocation is the parsed command line.
type invocation struct {
	server  string
	command string
	args    []string
	help    bool
}

// parseArgs parses argv into an invocation. --server may appear before "run"
// or after the subcommand name and before the positional arguments; when both
// appear, the later one wins. Everything after "--" is positional.
func parseArgs(argv []string) (inv invocation, err error) {
	inv.server = defaultServer
	rest, help, err := parseGlobal(argv, &inv)
	if err != nil {
		return inv, err
	}
	if help {
		inv.help = true
		return inv, nil
	}
	if len(rest) == 0 {
		return inv, errors.New("missing command")
	}
	if rest[0] != "run" {
		return inv, fmt.Errorf("unknown command %q", rest[0])
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return inv, errors.New(`missing command after "run"`)
	}
	if rest[0] == "-h" || rest[0] == "--help" {
		inv.help = true
		return inv, nil
	}
	name := rest[0]
	if _, ok := commands[name]; !ok {
		return inv, fmt.Errorf("unknown command %q", name)
	}
	args, help, err := parseCommandArgs(rest[1:], &inv)
	if err != nil {
		return inv, err
	}
	if help {
		inv.help = true
		return inv, nil
	}
	inv.command = name
	inv.args = args
	if err := validateServer(inv.server); err != nil {
		return inv, err
	}
	return inv, nil
}

// parseGlobal consumes the options that may precede the top-level "run"
// command and returns the remaining arguments.
func parseGlobal(argv []string, inv *invocation) (rest []string, help bool, err error) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-h" || arg == "--help":
			return nil, true, nil
		case arg == "--":
			return argv[i+1:], false, nil
		case arg == "--server":
			if i+1 >= len(argv) {
				return nil, false, errors.New("--server needs a value")
			}
			inv.server = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--server="):
			inv.server = strings.TrimPrefix(arg, "--server=")
		case strings.HasPrefix(arg, "-"):
			return nil, false, fmt.Errorf("unknown option %q", arg)
		default:
			return argv[i:], false, nil
		}
	}
	return nil, false, nil
}

// parseCommandArgs consumes the options between the subcommand name and the
// positional arguments. Once the first positional argument is seen, the rest
// are positional; "--" ends option parsing unconditionally.
func parseCommandArgs(argv []string, inv *invocation) (args []string, help bool, err error) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			return append(args, argv[i+1:]...), false, nil
		}
		if len(args) > 0 {
			args = append(args, arg)
			continue
		}
		switch {
		case arg == "-h" || arg == "--help":
			return nil, true, nil
		case arg == "--server":
			if i+1 >= len(argv) {
				return nil, false, errors.New("--server needs a value")
			}
			inv.server = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--server="):
			inv.server = strings.TrimPrefix(arg, "--server=")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return nil, false, fmt.Errorf("unknown option %q", arg)
		default:
			args = append(args, arg)
		}
	}
	return args, false, nil
}

// validateServer accepts only absolute http and https URLs with a host.
func validateServer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid server address %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid server address %q: must be an http or https URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid server address %q: missing host", raw)
	}
	return nil
}

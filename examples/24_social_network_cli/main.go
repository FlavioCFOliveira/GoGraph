package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// usageError signals an invocation problem (unknown subcommand, missing
// or malformed flag, bad positional arguments) so the dispatcher can map
// the failure to exit code 2 instead of the generic runtime exit code 1.
type usageError struct {
	msg string
}

func (e *usageError) Error() string { return e.msg }

func newUsageError(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// usage prints the high-level help for the example CLI to w. Errors
// writing to w (e.g. a closed stderr pipe) are deliberately discarded —
// the binary is about to exit anyway and there is no actionable recovery.
func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `Usage: 24_social_network_cli <subcommand> [flags] [args...]

Subcommands:
  init       Open or create the data directory (writes an empty snapshot).
  seed       Populate the graph with the deterministic social-network fixture.
  query      Run a Cypher query and emit each record as a JSON Lines record.
  snapshot   Force a manual snapshot of the current in-memory graph.
  stats      Print node and edge counts as a single JSON object.

Common flags:
  -d <dir>   Data directory (required for every subcommand).

Run '24_social_network_cli help' to print this help, or
'24_social_network_cli <subcommand> -h' for subcommand-specific options.`)
}

// parseDataDir parses the common '-d <dir>' flag from args and returns
// the validated directory along with any remaining positional arguments.
// The returned error is a *usageError when -d is missing or malformed.
func parseDataDir(subcmd string, args []string) (dir string, rest []string, err error) {
	fs := flag.NewFlagSet(subcmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dir, "d", "", "data directory (required)")
	if perr := fs.Parse(args); perr != nil {
		return "", nil, newUsageError("%s: flag parse: %v", subcmd, perr)
	}
	if dir == "" {
		return "", nil, newUsageError("%s: missing required flag -d <dir>", subcmd)
	}
	return dir, fs.Args(), nil
}

// dispatch routes argv to the matching subcommand. It is split out from
// main() so the round-trip test in T9 can drive it without invoking the
// process. Exit codes 0/1/2 are produced by main, not by dispatch.
func dispatch(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return newUsageError("no subcommand given")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return cmdInit(rest)
	case "seed":
		return cmdSeed(rest)
	case "query":
		return cmdQuery(rest)
	case "snapshot":
		return cmdSnapshot(rest)
	case "stats":
		return cmdStats(rest)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return newUsageError("unknown subcommand %q", sub)
	}
}

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var ue *usageError
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestGraphQueryArgsNameTheClientSubcommand pins the subcommand this tool sends
// statements through.
//
// rmp 1.17.0 rebuilt `rmp graph` as a server/client pair and removed the five
// subcommands of the previous line, `query` among them. This tool was written
// against `query`, so every run after that upgrade exited 3 — the harness
// could not conclude — and because ci-kg-verify is a member of `make ci`, the
// sprint-close gate was red for a reason that had nothing to do with the graph.
// The failure was silent in the worst way: exit 3 is not exit 1, so the gate
// was neither passing nor reporting a fidelity defect.
func TestGraphQueryArgsNameTheClientSubcommand(t *testing.T) {
	args := graphQueryArgs("gograph", "RETURN 1")
	want := []string{"graph", "client", "-r", "gograph", "--query", "RETURN 1"}
	if len(args) != len(want) {
		t.Fatalf("graphQueryArgs returned %d args, want %d: %q", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg %d = %q, want %q (full: %q)", i, args[i], want[i], args)
		}
	}
}

// TestRmpAcceptsTheGraphSubcommandWeUse holds the constant to the binary that
// has to accept it, which is the half a string comparison cannot cover: a
// future rmp line could retire `client` exactly as it retired `query`, and the
// unit test above would keep passing while every run exited 3.
//
// The oracle needs no server and runs no statement. `rmp graph <sub>` with no
// --query exits 127 for an unknown subcommand and 2 for a known one missing its
// query, so the two cases are distinguishable without touching the graph.
// Measured on rmp 1.17.0: `query` gives 127, `client` gives 2.
func TestRmpAcceptsTheGraphSubcommandWeUse(t *testing.T) {
	if _, err := exec.LookPath("rmp"); err != nil {
		t.Skipf("rmp is not on PATH (%v); this check needs the real binary, and the "+
			"unit test above still pins the constant", err)
	}
	const unknownSubcommand = 127
	code := runForExitCode(t, "rmp", "graph", graphSubcommand, "-r", "gograph")
	if code == unknownSubcommand {
		t.Fatalf("rmp rejects `graph %s` as an unknown subcommand (exit %d); "+
			"the graph command has been rebuilt again and graphSubcommand is stale",
			graphSubcommand, code)
	}

	// Liveness: the oracle must be able to say 127 at all, or the assertion
	// above passes because nothing can ever fail it. `query` is the subcommand
	// this tool used before rmp 1.17.0 removed it.
	if got := runForExitCode(t, "rmp", "graph", "query", "-r", "gograph"); got != unknownSubcommand {
		t.Fatalf("control arm: `rmp graph query` exited %d, want %d; the exit code no "+
			"longer distinguishes an unknown subcommand, so this test proves nothing", got, unknownSubcommand)
	}
}

func runForExitCode(t *testing.T, bin string, args ...string) int {
	t.Helper()
	err := exec.Command(bin, args...).Run() //nolint:gosec // fixed binary, arguments composed by this test.
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("running %s %s: %v", bin, strings.Join(args, " "), err)
	return -1
}

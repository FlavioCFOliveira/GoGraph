// Command sim runs the GoGraph deterministic simulation testing (DST) harness.
//
// Usage:
//
//	go run ./cmd/sim [seed] [flags]
//
// With no seed argument a random seed is chosen (from a non-deterministic
// source used solely to pick the seed value) and printed so the run can be
// reproduced. Flags:
//
//	--ticks     number of ticks (operations) to simulate (default 100000)
//	--workload  actor mix: default | write-heavy | read-heavy (default "default")
//	--verbose   print each operation as it runs
//
// On a violation the report (which includes a "Reproduce with:" line) is
// printed to stderr and the process exits 1. On success a one-line summary is
// printed and the process exits 0.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args, executes a simulation, and returns the process exit code. It
// is separated from main so it can be unit-tested with arbitrary writers.
func run(args []string, stdoutRaw, stderrRaw io.Writer) int {
	stdout := &errWriter{w: stdoutRaw}
	stderr := &errWriter{w: stderrRaw}

	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.SetOutput(stderrRaw)
	ticks := fs.Int("ticks", 100000, "number of ticks (operations) to simulate")
	workloadName := fs.String("workload", "default", "actor mix: default | write-heavy | read-heavy")
	verbose := fs.Bool("verbose", false, "print each operation as it runs")

	// Go's flag package stops parsing at the first non-flag token, so the
	// documented usage `sim <seed> --ticks=N` would otherwise leave --ticks
	// unparsed. Split the optional leading positional seed out first, then parse
	// the remaining tokens as flags, so flags work whether they precede or
	// follow the seed.
	seedArgs, flagArgs := splitSeedArg(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(fs.Args()) > 0 {
		stderr.printf("sim: unexpected arguments: %v\n", fs.Args())
		return 2
	}

	seed, ok := resolveSeed(seedArgs, stderr)
	if !ok {
		return 2
	}

	wlFactory, ok := workloadByName(*workloadName, stderr)
	if !ok {
		return 2
	}

	cfg := sim.Config{
		Seed:     seed,
		MaxTicks: *ticks,
		Workload: wlFactory(sim.NewSeed(seed)),
	}
	if *verbose {
		stdout.printf("Running simulation: seed=%d ticks=%d workload=%s\n", seed, *ticks, *workloadName)
		cfg.OnOp = func(tick int64, op sim.Op) {
			stdout.printf("  tick=%d %s %q %v\n", tick, op.Kind, op.Cypher, op.Params)
		}
	}
	sm, err := sim.New(cfg)
	if err != nil {
		stderr.printf("sim: %v\n", err)
		return 2
	}

	report, err := sm.Run(context.Background())
	if err != nil {
		stderr.printf("sim: run error: %v\n", err)
		return 1
	}
	if report != nil {
		stderr.printf("%s", report.String())
		return 1
	}
	stdout.printf("Simulation passed. Seed: %d, Ticks: %d\n", seed, *ticks)
	return 0
}

// errWriter latches the first write error from a sequence of formatted writes so
// each call site need not check it individually. A failed write to the
// process's own stdout/stderr is not separately actionable, but latching the
// error keeps every write checked (errcheck-clean) and would surface a broken
// pipe to anyone who inspects Err.
type errWriter struct {
	w   io.Writer
	err error
}

// printf writes a formatted line, recording the first error encountered.
func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

// splitSeedArg separates an optional leading positional seed token (the first
// argument that does not start with '-') from the flag tokens. It returns
// (seedArgs, flagArgs): seedArgs holds the single seed token when present (else
// empty), and flagArgs holds every remaining token in its original order. Only
// a leading positional is treated as the seed; a non-flag token appearing after
// flags is left in flagArgs so flag.Parse reports it as unexpected.
func splitSeedArg(args []string) (seedArgs, flagArgs []string) {
	for i, a := range args {
		if a == "" || a[0] == '-' {
			continue
		}
		flagArgs = make([]string, 0, len(args)-1)
		flagArgs = append(flagArgs, args[:i]...)
		flagArgs = append(flagArgs, args[i+1:]...)
		return []string{a}, flagArgs
	}
	return nil, args
}

// resolveSeed returns the seed to use: the first positional argument parsed as
// an unsigned integer, or a freshly-chosen random seed when none is given. The
// random source is math/rand/v2's auto-seeded top-level generator, used only to
// pick the value (never inside the deterministic simulation). The chosen seed
// is always reported by the caller so the run can be reproduced.
func resolveSeed(positional []string, stderr *errWriter) (uint64, bool) {
	if len(positional) == 0 {
		//nolint:gosec // G404: this is a test-harness seed selector; a
		// non-cryptographic source is intentional and the chosen value is
		// printed so the run is reproducible.
		return mrand.Uint64(), true
	}
	v, err := strconv.ParseUint(positional[0], 10, 64)
	if err != nil {
		stderr.printf("sim: invalid seed %q: %v\n", positional[0], err)
		return 0, false
	}
	return v, true
}

// workloadFactory builds a workload from a seed.
type workloadFactory func(*sim.Seed) *sim.Workload

// workloadByName maps a workload name to its factory.
func workloadByName(name string, stderr *errWriter) (workloadFactory, bool) {
	switch name {
	case "default":
		return sim.DefaultWorkload, true
	case "write-heavy":
		return sim.WriteHeavyWorkload, true
	case "read-heavy":
		return sim.ReadHeavyWorkload, true
	default:
		stderr.printf("sim: unknown workload %q (want default|write-heavy|read-heavy)\n", name)
		return nil, false
	}
}

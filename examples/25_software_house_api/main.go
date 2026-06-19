package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses flags, opens the persistent store once, optionally seeds it at a
// requested scale, serves HTTP until a termination signal arrives, then shuts
// down gracefully. It returns a process exit code: 0 on a clean run, 1 on a
// runtime failure, 2 on a usage error.
func run(args []string) int {
	fs := flag.NewFlagSet("25_software_house_api", flag.ContinueOnError)
	dir := fs.String("d", "", "data directory holding the WAL and snapshot (required)")
	addr := fs.String("addr", ":8080", "HTTP listen address")
	scaleComponents := fs.Int("scale-components", 0, "extra synthetic :Component nodes to seed at startup (0 = small deterministic fixture only)")
	scaleTasks := fs.Int("scale-tasks", 0, "extra synthetic :Task nodes to seed at startup")
	scaleDevelopers := fs.Int("scale-developers", 0, "extra synthetic :Developer nodes to seed at startup")
	scaleSeed := fs.Int64("scale-seed", 1, "RNG seed fixing the synthetic data shape")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "error: -d <dir> is required")
		return 2
	}
	scale := synthScale{
		components: *scaleComponents,
		tasks:      *scaleTasks,
		developers: *scaleDevelopers,
		seed:       *scaleSeed,
	}
	if err := scale.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	// A context cancelled on SIGINT/SIGTERM governs both startup
	// (recovery honours cancellation) and the serve lifetime.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ds, err := openStore(ctx, *dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open store: %v\n", err)
		return 1
	}

	// When a synthetic scale is requested, seed once at startup so the
	// operator gets the scaled graph without a separate request, and report
	// the build cost and live heap on a "# " telemetry line — the same
	// fact-vs-telemetry convention the non-server examples use on stdout.
	if scale.active() {
		if code := seedAtStartup(ds, scale); code != 0 {
			_ = ds.Close()
			return code
		}
	}

	srv := newServer(ds, *addr)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "listening on %s (data dir %s)\n", *addr, *dir)

	select {
	case err := <-serveErr:
		// The listener failed before any signal (e.g. address in use).
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: serve: %v\n", err)
			_ = ds.snapshotNow()
			_ = ds.Close()
			return 1
		}
		return 0
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "error: shutdown: %v\n", err)
			<-serveErr
			return 1
		}
		<-serveErr // let the serve goroutine finish
		fmt.Fprintln(os.Stderr, "shutdown complete")
		return 0
	}
}

// seedAtStartup loads the fixture at the requested synthetic scale and prints
// the build telemetry on stderr. Deterministic facts (the requested scale) are
// printed as bare lines; volatile telemetry (elapsed, heap) is printed on
// "# "-prefixed lines, matching the examples-standard convention. It returns a
// process exit code: 0 on success, 1 on a seed failure.
func seedAtStartup(ds *dataStore, scale synthScale) int {
	start := time.Now()
	seeded, err := seedFixtureScaled(ds.txnStore, scale)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: seed: %v\n", err)
		return 1
	}
	elapsed := time.Since(start)
	if seeded {
		ds.metrics.recordSeed(elapsed)
	}

	// Facts: the requested scale, reproducible for a fixed seed.
	fmt.Fprintf(os.Stderr, "seed.scale_components=%d\n", scale.components)
	fmt.Fprintf(os.Stderr, "seed.scale_tasks=%d\n", scale.tasks)
	fmt.Fprintf(os.Stderr, "seed.scale_developers=%d\n", scale.developers)
	fmt.Fprintf(os.Stderr, "seed.seed=%d\n", scale.seed)
	fmt.Fprintf(os.Stderr, "seed.applied=%t\n", seeded)
	// Telemetry: varies per run and per machine, never pinned.
	mem := readMem()
	liveNodes := ds.graph.LiveOrder()
	fmt.Fprintf(os.Stderr, "# seed.elapsed=%s\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "# seed.node_rate=%.0f nodes/s\n", rate(liveNodes, elapsed))
	fmt.Fprintf(os.Stderr, "# mem.heap_alloc=%s\n", humanBytes(mem.HeapAlloc))
	fmt.Fprintf(os.Stderr, "# mem.num_gc=%d\n", mem.NumGC)
	return 0
}

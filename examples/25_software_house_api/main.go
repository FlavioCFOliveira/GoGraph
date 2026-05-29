// Command 25_software_house_api is a persistent REST WebAPI over a
// multi-layer Labeled Property Graph that models task management inside a
// software-house. See doc.go for the full package documentation and
// SPEC.md for the data model, REST contract and query catalogue.
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

// run parses flags, opens the persistent store once, serves HTTP until a
// termination signal arrives, then shuts down gracefully. It returns a
// process exit code: 0 on a clean run, 1 on a runtime failure, 2 on a
// usage error.
func run(args []string) int {
	fs := flag.NewFlagSet("25_software_house_api", flag.ContinueOnError)
	dir := fs.String("d", "", "data directory holding the WAL and snapshot (required)")
	addr := fs.String("addr", ":8080", "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "error: -d <dir> is required")
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

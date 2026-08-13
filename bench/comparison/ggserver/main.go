// Command ggserver runs GoGraph behind its own Bolt server as a standalone
// process, so that a comparative benchmark can drive it exactly as it drives
// Neo4j and Memgraph: over a socket, from a client that lives in a different
// process.
//
// This exists for one measurement reason. When the GoGraph server is hosted
// inside the benchmark process, the load generator and the engine share one Go
// runtime and one GOMAXPROCS budget, while the containerised rivals get a CPU
// allocation of their own and are driven by a client that competes with
// nothing. Any throughput compared across that asymmetry measures the harness
// as much as the engine. Running the engine here, under its own GOMAXPROCS,
// removes the asymmetry: every target becomes a CPU-capped server process
// addressed over TCP.
//
// In the comparison this binary was built for it runs CONTAINERISED, with the
// same --cpus cap as the rivals and with the load generator in a container of
// its own on the same network, because reaching a macOS host process through
// the virtual machine's port-forward costs more per round trip than the query
// costs to execute. See docs/concurrency-vs-neo4j-memgraph-2026-08-11.md §2.1.
//
// Usage:
//
//	GOMAXPROCS=4 go run ./bench/comparison/ggserver -addr 127.0.0.1:7689
//
// The server is in-memory only: durability is deliberately out of scope here,
// because the rivals are configured for their own default durability and the
// comparison this binary serves is one of concurrency, not of commit cost.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// serveDebug exposes the Go runtime's own memory accounting so that a
// comparative measurement is not forced to infer the engine's footprint from
// the process's resident set alone.
//
// Resident set size is the only figure that can be compared across three
// engines written in three languages, but on its own it cannot distinguish
// memory the engine is USING from memory the Go allocator is merely HOLDING:
// the runtime returns pages to the operating system lazily, so a process that
// has freed a gigabyte may keep reporting it for minutes. /gc forces the
// collection and the release, so that a subsequent resident reading is a
// steady state rather than a high-water mark, and /memstats reports what the
// runtime itself believes is live — the figure against which the resident
// reading is cross-checked.
//
// The pprof endpoints are what turn a per-node byte figure into an ATTRIBUTED
// one: a resident-set reading says how much an engine spends, and only a heap
// profile says on what. They are registered on this private mux rather than on
// the default one, so that importing net/http/pprof — which registers itself
// globally as a side effect of being imported — cannot expose them on any
// other listener the process may open.
func serveDebug(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.HandleFunc("/gc", func(w http.ResponseWriter, _ *http.Request) {
		// Twice: the first collection makes the finalizable objects
		// unreachable, the second collects what those finalizers released.
		runtime.GC()
		runtime.GC()
		debug.FreeOSMemory()
		_, _ = fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/memstats", func(w http.ResponseWriter, _ *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"heap_alloc": m.HeapAlloc, "heap_sys": m.HeapSys,
			"heap_inuse": m.HeapInuse, "heap_released": m.HeapReleased,
			"heap_objects": m.HeapObjects, "stack_inuse": m.StackInuse,
			"mspan_inuse": m.MSpanInuse, "mcache_inuse": m.MCacheInuse,
			"gc_sys": m.GCSys, "other_sys": m.OtherSys, "sys": m.Sys,
			"total_alloc": m.TotalAlloc, "mallocs": m.Mallocs, "frees": m.Frees,
			"num_gc": m.NumGC,
		})
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("debug listen: %w", err)
	}
	// A read-header timeout is set because a server without one lets an idle
	// peer hold a connection open indefinitely. The other timeouts are left
	// unset deliberately: /debug/pprof/profile and /debug/pprof/trace are
	// long-lived by design, and a write deadline would truncate them.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// main keeps no logic of its own so that run's deferred cleanup — cancelling
// the signal context — always executes before the process exits.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:7689", "listen address")
	debugAddr := flag.String("debug-addr", "0.0.0.0:6060", "memory-introspection listen address; empty disables it")
	maxConns := flag.Int("max-conns", 2048, "maximum concurrent connections")
	maxTx := flag.Int("max-open-tx", 4096, "maximum open transactions per principal")
	// Multigraph mode is the default because it is what openCypher semantics
	// require: two CREATE statements between the same pair of nodes must
	// produce two distinct relationships with independent types and
	// properties. It is exposed as a flag because it is also the single
	// largest structural choice available to a GoGraph embedder — a simple
	// graph leaves the per-instance and per-handle stores empty — so a
	// memory comparison must be able to measure what it costs rather than
	// assume it.
	multigraph := flag.Bool("multigraph", true, "allow parallel edges between the same pair of nodes")
	flag.Parse()

	if *debugAddr != "" {
		if err := serveDebug(*debugAddr); err != nil {
			return err
		}
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: *multigraph})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngine(g)

	// MaxOpenTxPerPrincipal must be raised explicitly. Its zero value applies
	// DefaultMaxOpenTxPerPrincipal = 16, which rejects the sweep's upper
	// concurrency levels with LimitExceeded — the exact defect that made the
	// mandated write-concurrency benchmark unrunnable until rmp #2387.
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections:        *maxConns,
		MaxOpenTxPerPrincipal: *maxTx,
		ConnTimeout:           10 * time.Minute,
		Auth:                  server.NoAuthHandler{},
		Logger:                slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		return fmt.Errorf("NewServer: %w", err)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Printed so the harness can confirm the CPU budget and the graph shape
	// the engine actually got, rather than the ones the environment was meant
	// to impose.
	fmt.Printf("ggserver listening on %s GOMAXPROCS=%d NumCPU=%d multigraph=%t\n",
		ln.Addr(), runtime.GOMAXPROCS(0), runtime.NumCPU(), *multigraph)

	if err := srv.Serve(ctx, ln); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

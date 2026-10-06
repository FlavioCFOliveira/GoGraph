// Command 37_mvcc_write_contention exercises and MEASURES the write side of
// GoGraph's MVCC under concurrent writers.
//
// See README.md for the scenario, the objective, and the six questions the output
// must answer. This file implements that specification; the specification was
// committed first, so the numbers below are not fitted to whatever the code
// happens to produce.
//
// # Output contract
//
// Bare lines carry DETERMINISTIC facts — counts, ratios that must hold, invariant
// verdicts — which a test asserts on. Lines prefixed with "# " carry VOLATILE
// telemetry: throughputs, latencies, heap figures and durations that vary per run
// and per machine, and which a test must not pin.
//
// That split is what lets one program be both the operator's evidence and the
// regression gate. A test that asserted on throughput would fail on a loaded
// machine and teach the next reader to ignore it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// config is the operator-settable shape of the workload.
type config struct {
	storeDir string

	customers  int
	inventory  int
	opsPerProd int
	producers  int
	readers    int
	// hotPct is the percentage of orders that touch the SHARED inventory set
	// rather than only the producer's own customer. It is the contention dial:
	// at 0 nothing collides and conflict detection cannot be demonstrated; at
	// 100 everything collides and independent scaling cannot be. Both halves are
	// needed, which is why this is a fraction and not a flag.
	hotPct int
	seed   uint64
	// ladder is phase 6, the concurrency ladder (ladder.go). Zero levels skip it.
	ladder ladderConfig
	// durability is phase 7, durability across a crash (durability.go). Zero
	// levels and zero kill runs skip it.
	durability durabilityConfig
}

func defaultConfig() config {
	return config{
		customers:  2000,
		inventory:  8,
		opsPerProd: 400,
		producers:  8,
		readers:    4,
		hotPct:     25,
		seed:       1,
		ladder:     defaultLadderConfig(),
		durability: defaultDurabilityConfig(),
	}
}

func (c *config) validate() error {
	switch {
	case c.customers < 1:
		return fmt.Errorf("customers must be >= 1, got %d", c.customers)
	case c.inventory < 1:
		return fmt.Errorf("inventory must be >= 1, got %d", c.inventory)
	case c.opsPerProd < 1:
		return fmt.Errorf("ops-per-producer must be >= 1, got %d", c.opsPerProd)
	case c.producers < 1:
		return fmt.Errorf("producers must be >= 1, got %d", c.producers)
	case c.readers < 0:
		return fmt.Errorf("readers must be >= 0, got %d", c.readers)
	case c.hotPct < 0 || c.hotPct > 100:
		return fmt.Errorf("hot-pct must be within [0, 100], got %d", c.hotPct)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.StringVar(&cfg.storeDir, "store-dir", "",
		"directory for the durable store used by the restart phase (default: a temp dir, removed on exit)")
	flag.IntVar(&cfg.customers, "customers", cfg.customers, "customer nodes per run")
	flag.IntVar(&cfg.inventory, "inventory", cfg.inventory, "SHARED inventory nodes (the contended set)")
	flag.IntVar(&cfg.opsPerProd, "ops-per-producer", cfg.opsPerProd, "orders each producer commits")
	flag.IntVar(&cfg.producers, "producers", cfg.producers, "concurrent producer goroutines")
	flag.IntVar(&cfg.readers, "readers", cfg.readers, "concurrent reader goroutines")
	flag.IntVar(&cfg.hotPct, "hot-pct", cfg.hotPct, "percent of orders that touch the shared inventory (the contention dial)")
	flag.Uint64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	levels := flag.String("ladder-levels", "1,8,64",
		"phase 6: comma-separated goroutine counts of the concurrency ladder; empty skips the phase")
	flag.IntVar(&cfg.ladder.totalOps, "ladder-ops", cfg.ladder.totalOps,
		"phase 6: operations per arm and level, shared among its goroutines; set, it also replaces the smaller default totals of L13 and L15")
	rows := flag.String("ladder-rows", "",
		"phase 6: comma-separated arm ids to run (L01,L04,L05,L06,L08,L09,L10,L11,L13,L15,L17,L18,L19); empty runs all")
	flag.BoolVar(&cfg.ladder.soak, "ladder-soak", false,
		"phase 6: also run the soak arms (horizon capacity cliff, full-size parallel count) and the self-conflict streak gate")
	durLevels := flag.String("durability-levels", "8,64",
		"phase 7: comma-separated writer counts of the in-process crash arms; empty skips them")
	flag.IntVar(&cfg.durability.totalTxns, "durability-txns", cfg.durability.totalTxns,
		"phase 7: transactions per arm and level, shared among its writers; the crash lands at half")
	flag.IntVar(&cfg.durability.killRuns, "durability-kill-runs", 0,
		"phase 7: kill -9 runs of a child process (D02); 0 skips them")
	flag.IntVar(&cfg.durability.killLevel, "durability-kill-level", cfg.durability.killLevel,
		"phase 7: writer count of the kill -9 child")
	childDir := flag.String("durability-child-dir", "",
		"INTERNAL: run as the phase-7 kill child against this store directory, until killed")
	childLevel := flag.Int("durability-child-level", 8, "INTERNAL: writer count of the phase-7 kill child")
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()

	if *childDir != "" {
		if err := runDurabilityChild(context.Background(), *childDir, *childLevel, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "durability child: %v\n", err)
			os.Exit(1)
		}
		return
	}
	dl, err := parseLevels(*durLevels)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	cfg.durability.levels = dl
	cfg.durability.seed = cfg.seed

	lv, err := parseLevels(*levels)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	cfg.ladder.levels = lv
	for _, r := range strings.Split(*rows, ",") {
		if r = strings.TrimSpace(r); r != "" {
			cfg.ladder.rows = append(cfg.ladder.rows, r)
		}
	}
	cfg.ladder.seed = cfg.seed
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "ladder-ops" || f.Name == "ladder-soak" {
			cfg.ladder.rowOps = nil // an explicit size, or the soak shape, applies to every arm
		}
	})
	if err := cfg.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, &cfg)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
}

// run drives every phase and writes the report to w.
func run(ctx context.Context, w io.Writer, cfg *config) error {
	fmt.Fprintf(w, "# config producers=%d readers=%d ops-per-producer=%d customers=%d inventory=%d hot-pct=%d seed=%d\n",
		cfg.producers, cfg.readers, cfg.opsPerProd, cfg.customers, cfg.inventory, cfg.hotPct, cfg.seed)

	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	// PHASE 1 — writer scaling. The number the sprint exists to move.
	if err := phaseWriterScaling(ctx, w, cfg); err != nil {
		return err
	}
	// PHASE 2 — contention: conflicts, retries, reader latency, version retention.
	if err := phaseContention(ctx, w, cfg); err != nil {
		return err
	}
	// PHASE 3 — the self-contradictory check: conservation under concurrent transfers.
	if err := phaseConservation(ctx, w, cfg); err != nil {
		return err
	}
	// PHASE 4 — durability: the data AND the MVCC clock survive a restart.
	if err := phaseRestart(ctx, w, cfg); err != nil {
		return err
	}
	// PHASE 5 — the deterministic scenario catalogue (catalogue.go).
	if err := phaseCatalogue(ctx, w); err != nil {
		return err
	}
	// PHASE 6 — the concurrency ladder (ladder.go).
	if len(cfg.ladder.levels) > 0 {
		out, err := phaseLadder(ctx, w, &cfg.ladder)
		if err != nil {
			return err
		}
		if f := out.failed(); len(f) > 0 {
			return fmt.Errorf("ladder: %d checks failed:\n%s", len(f), strings.Join(f, "\n"))
		}
	}
	// PHASE 7 — durability under concurrent writers across a crash (durability.go).
	if len(cfg.durability.levels) > 0 || cfg.durability.killRuns > 0 {
		out, err := phaseDurability(ctx, w, &cfg.durability)
		if err != nil {
			return err
		}
		if f := out.failed(); len(f) > 0 {
			return fmt.Errorf("durability: %d checks failed:\n%s", len(f), strings.Join(f, "\n"))
		}
	}

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	// #nosec G115 -- heap figures from the runtime, compared as a signed delta for
	// reporting; neither operand is attacker-controlled.
	fmt.Fprintf(w, "# mem.heap_alloc_delta_bytes=%d\n", int64(m1.HeapAlloc)-int64(m0.HeapAlloc))
	fmt.Fprintf(w, "# mem.total_alloc_bytes=%d\n", m1.TotalAlloc-m0.TotalAlloc)
	fmt.Fprintf(w, "# mem.num_gc=%d\n", m1.NumGC-m0.NumGC)
	return nil
}

// parseLevels parses the -ladder-levels list.
func parseLevels(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("ladder level %q: want a positive integer", f)
		}
		out = append(out, n)
	}
	return out, nil
}

// newGraph builds an in-memory graph for one phase.
func newGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

func customerKey(i int) string  { return fmt.Sprintf("cust-%06d", i) }
func inventoryKey(i int) string { return fmt.Sprintf("inv-%03d", i) }

// seedGraph creates the customer and inventory populations.
func seedGraph(g *lpg.Graph[string, float64], cfg *config) error {
	for i := 0; i < cfg.customers; i++ {
		k := customerKey(i)
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			wv := g.Writer(tx)
			if err := wv.AddNode(k); err != nil {
				return err
			}
			return wv.SetNodeProperty(k, "orders", lpg.Int64Value(0))
		}); err != nil {
			return fmt.Errorf("seed customer %d: %w", i, err)
		}
	}
	for i := 0; i < cfg.inventory; i++ {
		k := inventoryKey(i)
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			wv := g.Writer(tx)
			if err := wv.AddNode(k); err != nil {
				return err
			}
			return wv.SetNodeProperty(k, "reserved", lpg.Int64Value(0))
		}); err != nil {
			return fmt.Errorf("seed inventory %d: %w", i, err)
		}
	}
	return nil
}

// retryBudget bounds a retry loop by WALL CLOCK, not by an attempt count.
//
// An attempt count measures this machine's speed: on a fast machine it is generous
// and on a loaded one it gives up early, so the same code reports a different
// conflict-survival rate for reasons that have nothing to do with MVCC. rmp #2330
// made this the project's rule after a fixed count died under coverage.
const retryBudget = 2 * time.Second

// hangBudget bounds one writer phase (one scaling level, or the contention
// phase) by WALL CLOCK, so a writer that stops making progress — a livelock in
// the retry loop, or a write that never returns from the barrier — ends the phase
// with its unfinished orders counted as unrecovered instead of hanging the run.
//
// It is sized to catch a hang, not to measure speed: a healthy phase of the
// documented default shape finishes in milliseconds, and the short-layer shape in
// less, so 30 s is three to four orders of magnitude of margin. A slow machine
// cannot reach it; a stuck writer always does.
const hangBudget = 30 * time.Second

// commitOrder applies one order through sess, retrying a serialization conflict
// until the budget expires. It reports whether it committed, and how many retries
// it took. ctx bounds the barrier acquisition, so a phase whose hang budget has
// expired stops waiting.
func commitOrder(
	ctx context.Context, g *lpg.Graph[string, float64], sess *lpg.Session[string, float64],
	custKey, invKey string, n int,
) (retries int, err error) {
	deadline := time.Now().Add(retryBudget)
	for attempt := 0; ; attempt++ {
		err = sess.ApplyVersionedCtx(ctx, func(tx lpg.WriteTx) error {
			wv := g.Writer(tx)
			// The producer's OWN customer: uncontended by construction.
			if perr := wv.SetNodeProperty(custKey, "orders", lpg.Int64Value(int64(n+1))); perr != nil {
				return perr
			}
			if invKey == "" {
				return nil
			}
			// The SHARED inventory node: this is where writers collide. Read through
			// the TRANSACTION'S OWN VIEW, not the present — a present-time read in a
			// read-modify-write is a lost update, and the conservation invariant in
			// phase 3 caught exactly that mistake in this example's first draft.
			cur, _ := g.WriterViewOf(tx).GetNodeProperty(invKey, "reserved")
			prev, _ := cur.Int64()
			return wv.SetNodeProperty(invKey, "reserved", lpg.Int64Value(prev+1))
		})
		if err == nil {
			return attempt, nil
		}
		if !errors.Is(err, mvcc.ErrSerializationConflict) {
			return attempt, err
		}
		if time.Now().After(deadline) {
			return attempt, err
		}
		retries++
	}
}

// percentiles returns the p50/p95/p99 of ds, which it sorts in place.
func percentiles(ds []time.Duration) (p50, p95, p99 time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(ds)-1))
		return ds[i]
	}
	return at(0.50), at(0.95), at(0.99)
}

// newRand returns a deterministic source for the given seed and stream.
//
// math/rand/v2 is deliberate and not a weakness: these draws pick which customer and
// which inventory node an order touches, so the requirement is REPRODUCIBILITY for a
// fixed seed, which is the opposite of what crypto/rand offers.
//
//nolint:gosec // G404: workload shape, not a security decision; a fixed seed is the point.
func newRand(seed, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, stream))
}

// atomicMax raises dst to v when v is larger.
func atomicMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

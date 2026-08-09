// Example 35 — reader latency under a mixed OLTP-and-analytics workload.
//
// See README.md for the scenario. In one sentence: this is the instrument that
// measures whether a latency-sensitive point query keeps its latency while an
// analytical query and an ingest stream run beside it, which is the property
// the MVCC programme exists to deliver (rmp #2274).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// config is the example's tunable surface.
//
// THE ANALYTICAL QUERY'S COST IS O(nodes²) AND IS CONTROLLED BY -nodes ALONE.
// A first version of this file carried an -analytics-n flag that bounded the
// left-hand side of the self-join with `a.id < N`, on the assumption that it
// would scale the query's cost. It does not: the join is a Cartesian product
// and the predicate is applied to its output, so the scan is nodes × nodes
// whatever N is — the flag measured identically at 8 and at 400 (1m35s both
// times). It was removed rather than documented, because a knob that does
// nothing is worse than no knob.
//
// The default of 3 000 nodes puts the analytical query at roughly two seconds,
// which is long enough to expose the effect and short enough to run unattended.
// THE EFFECT LASTS EXACTLY AS LONG AS THAT QUERY DOES, so raising -nodes raises
// the worst point-query latency in proportion: at 20 000 nodes the analytical
// query runs for about 95 seconds and the worst point read follows it to
// 1m36s.
type config struct {
	nodes       int
	readers     int
	phaseWindow time.Duration
	writeEvery  time.Duration
}

func defaultConfig() config {
	return config{
		nodes:       3000,
		readers:     4,
		phaseWindow: 700 * time.Millisecond,
		writeEvery:  100 * time.Millisecond,
	}
}

func (c config) validate() error {
	switch {
	case c.nodes < 100:
		return fmt.Errorf("nodes must be >= 100, got %d", c.nodes)
	case c.readers < 1:
		return fmt.Errorf("readers must be >= 1, got %d", c.readers)
	case c.phaseWindow <= 0:
		return fmt.Errorf("phase-window must be > 0, got %s", c.phaseWindow)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of :Account nodes")
	flag.IntVar(&cfg.readers, "readers", cfg.readers, "concurrent OLTP reader goroutines")
	flag.DurationVar(&cfg.phaseWindow, "phase-window", cfg.phaseWindow, "measurement window per phase")
	flag.DurationVar(&cfg.writeEvery, "write-every", cfg.writeEvery, "ingest interval")
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()
	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// phase is one measured combination of the three roles.
type phase struct {
	name      string
	analytics bool
	writer    bool
}

// result carries what one phase measured.
type result struct {
	name       string
	throughput float64
	p50, p99   time.Duration
	max        time.Duration
	writes     int64
}

func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.readers=%d\n", cfg.readers)

	dir, err := os.MkdirTemp("", "gograph-ex35-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	eng, g, closeFn, err := openDurable(dir, cfg.nodes)
	if err != nil {
		return err
	}
	defer closeFn()

	// The short read must be a SEEK, not a scan. Without the index it is an
	// unindexed filter over every node, which costs enough that its own barrier
	// hold masks the very effect being measured.
	if _, err := eng.RunInTx(ctx, "CREATE INDEX FOR (n:Account) ON (n.id)", nil); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	fmt.Fprintf(w, "index.created=1\n")

	const shortQ = `MATCH (n:Account {id: $id}) RETURN n.balance AS b`
	// The reporting query: a self-join over every account, which is what an
	// unbounded analytical query looks like and costs O(nodes²).
	const analyticsQ = `MATCH (a:Account), (b:Account) RETURN count(*) AS c`

	// Calibrate. An "analytical" query that is not long measures nothing, so
	// its duration is reported as evidence rather than assumed.
	start := time.Now()
	if err := drain(ctx, eng, analyticsQ, nil); err != nil {
		return fmt.Errorf("calibration: %w", err)
	}
	analyticsDur := time.Since(start)
	fmt.Fprintf(w, "# analytics.duration=%s\n", analyticsDur.Round(time.Millisecond))
	fmt.Fprintf(w, "analytics.is_long=%d\n", boolToInt(analyticsDur >= 50*time.Millisecond))

	phases := []phase{
		{"baseline", false, false},
		{"analytics_only", true, false},
		{"writer_only", false, true},
		{"analytics_and_writer", true, true},
	}
	results := make([]result, 0, len(phases))
	for _, p := range phases {
		r, err := measure(ctx, eng, cfg, p, shortQ, analyticsQ)
		if err != nil {
			return fmt.Errorf("phase %s: %w", p.name, err)
		}
		results = append(results, r)
		fmt.Fprintf(w, "# phase.%s.throughput_ops=%.1f\n", p.name, r.throughput)
		fmt.Fprintf(w, "# phase.%s.p50=%s\n", p.name, r.p50.Round(time.Microsecond))
		fmt.Fprintf(w, "# phase.%s.p99=%s\n", p.name, r.p99.Round(time.Microsecond))
		fmt.Fprintf(w, "# phase.%s.max=%s\n", p.name, r.max.Round(time.Microsecond))
		fmt.Fprintf(w, "# phase.%s.writes=%d\n", p.name, r.writes)
	}

	// The verdict. Each role ALONE should be nearly free; only the combination
	// exposes the defect, which is why all four cells are measured.
	base, both := results[0], results[3]
	collapse := base.throughput / max(both.throughput, 1e-9)
	amplification := float64(both.max) / float64(max64(base.max, 1))
	fmt.Fprintf(w, "# verdict.throughput_collapse=%.1fx\n", collapse)
	fmt.Fprintf(w, "# verdict.max_latency_amplification=%.1fx\n", amplification)
	fmt.Fprintf(w, "# verdict.worst_read_vs_analytics=%.2f\n",
		float64(both.max)/float64(max64(analyticsDur, 1)))

	// readers_starved is the fact the test pins. A reader is starved when the
	// combination costs far more than either role alone AND the worst read has
	// grown to a sizeable fraction of the analytical query — the signature of a
	// point query waiting out an unrelated scan rather than merely sharing CPU.
	starved := collapse >= 4 && float64(both.max) >= 0.5*float64(analyticsDur)
	fmt.Fprintf(w, "readers_starved=%d\n", boolToInt(starved))

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "# heap.alloc_bytes=%d\n", ms.HeapAlloc)
	// MVCC telemetry. Both are zero while the substrate ships inert; they
	// become the memory the design owes a reclamation phase once it is armed,
	// which is why the example reports them from the first day rather than
	// waiting until there is something to show.
	fmt.Fprintf(w, "# mvcc.label_deltas=%d\n", g.LabelDeltaCount())
	fmt.Fprintf(w, "# mvcc.prop_deltas=%d\n", g.PropDeltaCount())
	fmt.Fprintf(w, "phases_measured=%d\n", len(results))
	return nil
}

// measure runs one phase and returns its short-read statistics.
func measure(ctx context.Context, eng *cypher.Engine, cfg config, p phase, shortQ, analyticsQ string) (result, error) {
	stop := make(chan struct{})
	bgCtx, cancelBG := context.WithCancel(ctx)
	defer cancelBG()
	var bg sync.WaitGroup
	var writes atomic.Int64

	if p.analytics {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := drain(bgCtx, eng, analyticsQ, nil); err != nil {
					return
				}
			}
		}()
	}
	if p.writer {
		bg.Add(1)
		go func() {
			defer bg.Done()
			tick := time.NewTicker(cfg.writeEvery)
			defer tick.Stop()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				q := fmt.Sprintf("CREATE (:Ingest {seq:%d})", i)
				if _, err := eng.RunInTx(bgCtx, q, nil); err == nil {
					writes.Add(1)
				}
			}
		}()
	}

	deadline := time.Now().Add(cfg.phaseWindow)
	var mu sync.Mutex
	samples := make([]time.Duration, 0, 4096)
	var rg sync.WaitGroup
	for r := 0; r < cfg.readers; r++ {
		rg.Add(1)
		go func(seed int) {
			defer rg.Done()
			local := make([]time.Duration, 0, 1024)
			for i := 0; time.Now().Before(deadline); i++ {
				params := map[string]expr.Value{"id": expr.IntegerValue(int64((seed*7919 + i) % cfg.nodes))}
				s := time.Now()
				if err := drain(ctx, eng, shortQ, params); err != nil {
					return
				}
				local = append(local, time.Since(s))
			}
			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}(r)
	}
	rg.Wait()
	close(stop)
	cancelBG()
	bg.Wait()

	if len(samples) == 0 {
		return result{}, fmt.Errorf("no short read completed")
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return result{
		name:       p.name,
		throughput: float64(len(samples)) / cfg.phaseWindow.Seconds(),
		p50:        samples[len(samples)*50/100],
		p99:        samples[min(len(samples)*99/100, len(samples)-1)],
		max:        samples[len(samples)-1],
		writes:     writes.Load(),
	}, nil
}

// drain runs q and consumes every row, so the measurement covers the whole
// query rather than only its planning.
func drain(ctx context.Context, eng *cypher.Engine, q string, params map[string]expr.Value) error {
	res, err := eng.Run(ctx, q, params)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // full drain
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// openDurable builds a WAL-backed engine and seeds it, so writes take the
// exclusive visibility barrier and pay a real fsync — the configuration the
// effect under measurement depends on.
func openDurable(dir string, nodes int) (*cypher.Engine, *lpg.Graph[string, float64], func(), error) {
	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wal.Open: %w", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < nodes; i++ {
		k := fmt.Sprintf("a%d", i)
		if err := g.AddNode(k); err != nil {
			return nil, nil, nil, fmt.Errorf("AddNode: %w", err)
		}
		if err := g.SetNodeLabel(k, "Account"); err != nil {
			return nil, nil, nil, fmt.Errorf("SetNodeLabel: %w", err)
		}
		if err := g.SetNodeProperty(k, "id", lpg.Int64Value(int64(i))); err != nil {
			return nil, nil, nil, fmt.Errorf("SetNodeProperty: %w", err)
		}
		if err := g.SetNodeProperty(k, "balance", lpg.Int64Value(int64(1000+i))); err != nil {
			return nil, nil, nil, fmt.Errorf("SetNodeProperty: %w", err)
		}
	}
	st := txn.NewStoreWithOptions[string, float64](g, wlog, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	db := store.New(wlog, store.WithQuiesce(st.RunUnderCommitLock))
	return cypher.NewEngineWithStore(st), g, func() { _ = db.Close() }, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func max64(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

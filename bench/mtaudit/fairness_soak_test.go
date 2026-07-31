//go:build soak

package mtaudit_test

// fairness_soak_test.go — reader starvation under a concurrent long read and a
// concurrent writer (rmp #2274).
//
// Layer: soak. It cannot live in the short layer, and that is a property of the
// defect rather than a convenience: the starvation window is exactly as long as
// the longest concurrent read, so the effect is only visible once that read is
// seconds long. A first version of this harness used an 11.7 ms long read and
// measured −8 %, which would have reported no problem at all.
//
// # What is being measured
//
// `Engine.Run` holds the graph read barrier ([lpg.Graph.View], an RLock on
// visMu) across BUILD and DRAIN, so a long analytical query holds it for its
// whole duration. A write takes the same barrier EXCLUSIVELY
// ([lpg.Graph.ApplyAtomically]). Go's sync.RWMutex prefers a waiting writer, so
// once the writer queues behind the long read, every short reader arriving
// after it parks until the long read finishes and the write completes.
//
// The signature of that mechanism is that each ingredient alone is harmless and
// only the COMBINATION collapses, which is why this test measures all four
// cells rather than just the last one. Measured 2026-07-31 on an Apple M4
// (10 cores) at head f848e854, with a 95.5-second analytical read:
//
//	readers  baseline   +long read  +writer    +BOTH     collapse
//	      1   219 917     222 334    207 107     7 507     −29.3×
//	      8   394 676     353 541    370 924    12 835     −30.8×
//	     64   374 840     365 182    437 150    15 247     −24.6×
//
// with a worst short-read latency of 1m39s, 1m40s and 1m35s respectively — a
// 4.5 µs point query blocked for the entire duration of the analytical read.
//
// # Why prior art says this is a design defect and not a tuning problem
//
// Neither incumbent takes a global exclusive latch for an ordinary write.
// Neo4j's readers take no lock at all and its write locks are per node and per
// relationship (Forseti). Memgraph's ordinary read AND write transactions both
// take `main_lock_` in a SHARED mode; its UNIQUE mode — which does gate new
// shared acquisitions exactly as Go's RWMutex does — is reserved for
// global-state operations such as index creation. Memgraph does not starve
// readers because ordinary writes never take UNIQUE, not because its lock is
// fairer. GoGraph applies writes eagerly to a single unversioned graph, so its
// exclusive barrier is load-bearing, and it therefore applies UNIQUE-grade
// exclusion to its most common operation.
//
// # THIS TEST IS EXPECTED TO FAIL UNTIL rmp #2274 PHASE P4 LANDS
//
// It is red on purpose, and it is red because the module has the defect it
// describes — not because it is broken, not because the machine is loaded, and
// not because someone forgot to finish it. Do not bisect it, do not soften the
// thresholds, and do not skip it. The decided fix is per-object MVCC
// (docs/design-mvcc-delta-chains.md); P4 of that programme retires the read
// barrier from Engine.Run, and this test turning green is that phase's
// acceptance criterion.
//
// Measured red at 51.5× (1 reader) and 61.2× (8 readers) on 2026-07-31, with
// worst short-read latencies of 1m36.7s and 1m36.9s.
//
// It is in the SOAK layer, which `make ci` does not run, so it does not block
// the local gate or a release; the soak layer is a periodic reliability
// exercise. That is deliberate — but it is also exactly how rmp #2256 stayed
// red and unnoticed for roughly 260 sprints, so this failure is registered in
// docs/certification-2026-07-31.md as a KNOWN RED gate with its rmp id rather
// than left to be rediscovered.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// fairnessNodes is the fixture size. It is chosen so the calibration query
// below runs for tens of seconds: the measurement is meaningless if the "long"
// read is short.
const fairnessNodes = 20000

// maxToleratedCollapse is the factor by which short-read throughput may fall
// when a long read and a writer run concurrently. The measured collapse is
// 25–30×, so a fix must bring it well inside this before the gate passes.
// It is deliberately loose: the claim is "does not collapse", not a tuned
// number, and a shared machine must not be able to manufacture a failure.
const maxToleratedCollapse = 4.0

// durableEngine wires a Cypher engine to a real WAL-backed store, so writes
// take the exclusive barrier and pay a genuine fsync.
func durableEngine(t *testing.T, g *lpg.Graph[string, float64]) (*cypher.Engine, func()) {
	t.Helper()
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g.SetIndexManager(index.NewManager())
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	db := store.New(w, store.WithQuiesce(st.RunUnderCommitLock))
	return cypher.NewEngineWithStore(st), func() { _ = db.Close() }
}

// fairnessCell is one measured combination.
type fairnessCell struct {
	name       string
	throughput float64
	worst      time.Duration
}

// TestFairScheduling_LongReadPlusWriterDoesNotStarveReaders is the gate.
func TestFairScheduling_LongReadPlusWriterDoesNotStarveReaders(t *testing.T) {
	testlayers.RequireSoak(t)

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < fairnessNodes; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	eng, closeFn := durableEngine(t, g)
	defer closeFn()

	// The short read must be a genuine point SEEK. Without the index it is an
	// unindexed filter over the whole fixture at 0.68 ms per call, long enough
	// that its own barrier hold masks the starvation being measured.
	if _, err := eng.RunInTx(context.Background(), "CREATE INDEX FOR (n:P) ON (n.w)", nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	const shortQ = `MATCH (n:P {w: $w}) RETURN n.w AS x`
	// A deliberately expensive analytical read: a bounded Cartesian product,
	// which is the shape a reporting query takes and which holds the read
	// barrier for tens of seconds.
	const longQ = `MATCH (a:P), (b:P) WHERE a.w < 60 AND b.w < 20000 RETURN count(*) AS c`
	params := map[string]expr.Value{"w": expr.IntegerValue(7)}

	// Calibrate. A "long read" that is not long makes every number below
	// meaningless, so this is asserted rather than assumed.
	probeStart := time.Now()
	res, err := eng.Run(context.Background(), longQ, nil)
	if err != nil {
		t.Fatalf("calibration run: %v", err)
	}
	for res.Next() { //nolint:revive // full drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("calibration drain: %v", err)
	}
	_ = res.Close()
	probe := time.Since(probeStart)
	t.Logf("calibration: the long read takes %s", probe.Round(time.Millisecond))
	if probe < 5*time.Second {
		t.Fatalf("the long read completes in %s, which is too short to expose reader starvation: "+
			"the stall lasts exactly as long as the long read, so this measurement would report "+
			"no problem whether or not one exists", probe.Round(time.Millisecond))
	}

	measure := func(name string, readers int, withLongRead, withWriter bool) fairnessCell {
		stop := make(chan struct{})
		// The long read is driven under a cancellable context. Without it,
		// teardown waits for the in-flight analytical query to finish, so every
		// cell that uses one costs a further 95 s and the test takes twenty
		// minutes to say nothing extra.
		bgCtx, cancelBG := context.WithCancel(context.Background())
		defer cancelBG()
		var bg sync.WaitGroup
		if withLongRead {
			bg.Add(1)
			go func() {
				defer bg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					r, err := eng.Run(bgCtx, longQ, nil)
					if err != nil {
						return
					}
					for r.Next() { //nolint:revive // full drain
					}
					_ = r.Close()
				}
			}()
		}
		if withWriter {
			bg.Add(1)
			go func() {
				defer bg.Done()
				tick := time.NewTicker(100 * time.Millisecond) // 10 writes/s
				defer tick.Stop()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					case <-tick.C:
					}
					_, _ = eng.RunInTx(context.Background(),
						fmt.Sprintf("CREATE (:W {i:%d})", i), nil)
				}
			}()
		}

		const window = 5 * time.Second
		deadline := time.Now().Add(window)
		var count, worstNs atomic.Int64
		var rg sync.WaitGroup
		for r := 0; r < readers; r++ {
			rg.Add(1)
			go func() {
				defer rg.Done()
				for time.Now().Before(deadline) {
					s := time.Now()
					r, err := eng.Run(context.Background(), shortQ, params)
					if err != nil {
						return
					}
					for r.Next() { //nolint:revive // full drain
					}
					_ = r.Close()
					d := time.Since(s).Nanoseconds()
					for {
						w := worstNs.Load()
						if d <= w || worstNs.CompareAndSwap(w, d) {
							break
						}
					}
					count.Add(1)
				}
			}()
		}
		rg.Wait()
		close(stop)
		cancelBG()
		bg.Wait()
		return fairnessCell{
			name:       name,
			throughput: float64(count.Load()) / window.Seconds(),
			worst:      time.Duration(worstNs.Load()),
		}
	}

	for _, readers := range []int{1, 8} {
		t.Run(fmt.Sprintf("readers=%d", readers), func(t *testing.T) {
			base := measure("baseline (reads only)", readers, false, false)
			longOnly := measure("+ one long read", readers, true, false)
			writerOnly := measure("+ writer 10/s", readers, false, true)
			both := measure("+ long read AND writer", readers, true, true)
			for _, c := range []fairnessCell{base, longOnly, writerOnly, both} {
				t.Logf("%-26s %11.1f op/s   worst %s", c.name, c.throughput, c.worst.Round(time.Microsecond))
			}

			// Each ingredient ALONE must be harmless. Asserting this is what
			// makes the combined failure attributable to the interaction rather
			// than to either component being slow on its own.
			if longOnly.throughput*maxToleratedCollapse < base.throughput {
				t.Errorf("a concurrent long read ALONE cost %.1f× (%.1f → %.1f op/s); readers share "+
					"the barrier, so this should be nearly free",
					base.throughput/longOnly.throughput, base.throughput, longOnly.throughput)
			}
			if writerOnly.throughput*maxToleratedCollapse < base.throughput {
				t.Errorf("a concurrent writer ALONE cost %.1f× (%.1f → %.1f op/s)",
					base.throughput/writerOnly.throughput, base.throughput, writerOnly.throughput)
			}

			// The gate.
			if both.throughput <= 0 {
				t.Fatalf("no short read completed with a long read and a writer running")
			}
			if collapse := base.throughput / both.throughput; collapse > maxToleratedCollapse {
				t.Fatalf("READER STARVATION: a long read plus a writer at 10/s collapsed short-read "+
					"throughput %.1f× (%.1f → %.1f op/s), worst latency %s. Neither ingredient alone "+
					"costs anything, so this is the exclusive write barrier plus Go's RWMutex writer "+
					"preference parking every reader that arrives behind the queued writer, for as "+
					"long as the long read runs (rmp #2274)",
					collapse, base.throughput, both.throughput, both.worst.Round(time.Millisecond))
			}

			// Latency amplification is the sharper statement of the same defect:
			// a point query must not inherit the duration of an unrelated read.
			if both.worst > probe/2 {
				t.Fatalf("READER LATENCY AMPLIFICATION: the worst short read took %s against a long "+
					"read of %s — the point query is waiting out the whole analytical query, so "+
					"reader latency is bounded only by the longest concurrent read (rmp #2274)",
					both.worst.Round(time.Millisecond), probe.Round(time.Millisecond))
			}
		})
	}
}

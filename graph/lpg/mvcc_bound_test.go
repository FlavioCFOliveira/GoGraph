package lpg

// mvcc_bound_test.go — MVCC P6c (rmp #2286): version memory is bounded, and the
// bound is observable.
//
// The point of these tests is that NOTHING in them calls a reclaimer. A
// reclaimer that has to be invoked by a test is not reclamation; what has to be
// true is that an ordinary workload keeps its own memory flat.
//
// # Two bounds since rmp #2308
//
// Reclamation moved onto a background vacuum, so "flat" acquired a shape it did
// not have while the committer swept before returning:
//
//   - INSTANTANEOUSLY, memory is bounded by [MVCCStats.Ceiling] — the debt at
//     which a committer stops signalling the sweeper and waits for it. That is
//     the bound a burst may not exceed, and it is asserted on the PEAK.
//   - IN THE SETTLED STATE, memory returns to [MVCCStats.Bound]. That is
//     asserted after the workload, by POLLING rather than by sweeping: the
//     property is still that the workload settles on its own, and a poll observes
//     settling instead of causing it.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// settleTimeout is how long a test waits for the background vacuum to bring the
// substrate back to the settled bound.
//
// Generous on purpose: the vacuum sweeps at full speed while it is making
// progress, so the real convergence is sub-millisecond, and a long ceiling only
// affects how a genuine failure is reported — never how a pass is timed.
const settleTimeout = 5 * time.Second

// waitWithinBound polls until version memory has settled to the churn bound, and
// fails the test if it has not within [settleTimeout].
//
// It calls no reclaimer. That is the whole point: a substrate that needs a test to
// sweep for it is not bounded, and this asserts that an ordinary workload's own
// vacuum brings it back.
func waitWithinBound(t *testing.T, g *Graph[string, float64]) MVCCStats {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	var s MVCCStats
	for {
		s = g.MVCCStats()
		if s.WithinBound() {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("version memory did not settle within %v: %d records held against a bound of "+
				"%d (ceiling %d), with %d active readers, %d unregistered, oldest reader age %d; "+
				"vacuum %+v", settleTimeout, s.Total, s.Bound, s.Ceiling, s.ActiveSnapshots,
				s.UnregisteredSnapshots, s.OldestSnapshotAge(), g.VacuumStats())
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestMVCCBound_SustainedWritesStayFlat drives a long write workload through the
// ordinary transaction bracket and asserts the substrate does not grow.
//
// Before the driver existed, this left one record per modification for the life
// of the process; the assertion is the difference between a reclaimer and a
// reclaimer that runs.
func TestMVCCBound_SustainedWritesStayFlat(t *testing.T) {
	g := mvccGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	const rounds = reclaimThreshold * 6
	var peak int64
	for i := 0; i < rounds; i++ {
		if err := g.ApplyAtomically(func() error {
			if err := g.SetNodeProperty("a", "w", Int64Value(int64(i))); err != nil {
				return err
			}
			return g.SetNodeLabel("a", fmt.Sprintf("L%d", i%4))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n := g.VersionCount(); n > peak {
			peak = n
		}
	}

	// The INSTANTANEOUS bound. A background sweeper can be outrun, so what may
	// not be exceeded is the ceiling at which the committer waits for it, plus one
	// threshold for the versions charged while the pass that relieved the pressure
	// was still running. Measured at rmp #2308 on this workload: a peak of 8 480
	// against a ceiling of 16 384, and 24 576 modifications would have left 24 576
	// records with no reclamation at all.
	if limit := int64(reclaimDebtCeiling + reclaimThreshold); peak > limit {
		t.Fatalf("peak version count %d over %d modifications exceeds the instantaneous bound of "+
			"%d: the vacuum's backpressure is not holding", peak, rounds, limit)
	}
	// And the SETTLED bound: with the writer stopped and no reader registered,
	// nothing may hold a version back.
	s := waitWithinBound(t, g)
	if s.ActiveSnapshots != 0 {
		t.Errorf("the horizon reports %d active readers after the workload; nothing should be "+
			"holding versions back", s.ActiveSnapshots)
	}
}

// TestMVCCBound_DirectAPIWritesStayFlat is the same claim for a caller that
// never opens a transaction, which is a supported use of this package and the
// one the first version of the driver missed entirely: deltaStamp went straight
// to the clock, so nothing charged the debt and 20 000 writes left 40 000 live
// records with a debt of zero.
func TestMVCCBound_DirectAPIWritesStayFlat(t *testing.T) {
	g := mvccGraph(t)
	const rounds = reclaimThreshold * 6
	var peak int64
	for i := 0; i < rounds; i++ {
		k := fmt.Sprintf("n%d", i%64)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "w", Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		if n := g.VersionCount(); n > peak {
			peak = n
		}
	}
	if limit := int64(reclaimDebtCeiling + reclaimThreshold); peak > limit {
		t.Fatalf("%d direct writes peaked at %d live version records against an instantaneous "+
			"bound of %d", rounds, peak, limit)
	}
	waitWithinBound(t, g)
}

// TestMVCCStats_AttributesGrowthToTheReaderHoldingIt pins the half of the bound
// this package cannot enforce: a reader that has begun is entitled to what it
// can still reach, so the memory it holds must be ATTRIBUTABLE rather than
// merely present.
func TestMVCCStats_AttributesGrowthToTheReaderHoldingIt(t *testing.T) {
	g := mvccGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	snap := g.BeginRead() // the long reader
	for i := 0; i < reclaimThreshold*3; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	s := g.MVCCStats()
	if s.ActiveSnapshots != 1 {
		t.Fatalf("the horizon reports %d active readers, want 1", s.ActiveSnapshots)
	}
	if s.WithinBound() {
		t.Fatal("a reader older than three thresholds of churn is holding nothing back; either " +
			"the reader is not registered or the versions it needs were freed")
	}
	if s.OldestSnapshotAge() == 0 {
		t.Error("the oldest reader's age is zero while it is demonstrably behind: growth cannot " +
			"be attributed to the read that caused it")
	}
	if s.UnregisteredSnapshots != 0 {
		t.Errorf("%d readers failed to register; reclamation is suspended for a different reason "+
			"than this test is measuring", s.UnregisteredSnapshots)
	}

	// And once it leaves, the vacuum takes it all back with NOTHING further being
	// written. That is the drain wake (rmp #2308): the reader's departure is the
	// only event, so if [Graph.wakeVacuumOnRelease] does not fire on it, the
	// records stay for the life of the graph. Measured against the first
	// asynchronous build, whose 1-in-64 tick dropped exactly this release: 16 385
	// records retained with no wake pending.
	g.EndRead(snap)
	waitWithinBound(t, g)
}

// gaugeRecorder captures the gauges the sweep publishes.
//
// It is MUTEX-GUARDED because a metrics backend is a process-global that any
// goroutine may publish to, and since rmp #2308 the publisher is the background
// vacuum rather than the test's own goroutine. The unguarded map died with
// `fatal error: concurrent map writes` the first time the vacuum published while
// the test was reading.
type gaugeRecorder struct {
	mu   sync.Mutex
	seen map[string]float64
}

func (g *gaugeRecorder) IncCounter(string, uint64)            {}
func (g *gaugeRecorder) ObserveLatency(string, time.Duration) {}

func (g *gaugeRecorder) SetGauge(name string, v float64) {
	g.mu.Lock()
	g.seen[name] = v
	g.mu.Unlock()
}

// gauge returns a recorded gauge and whether it was ever published.
func (g *gaugeRecorder) gauge(name string) (float64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.seen[name]
	return v, ok
}

// TestMVCCMetrics_AreExported pins that the utilisation the bounded-resources
// mandate asks for actually reaches a backend, rather than being available only
// to a caller that knows to ask.
func TestMVCCMetrics_AreExported(t *testing.T) {
	rec := &gaugeRecorder{seen: map[string]float64{}}
	metrics.SetBackend(rec)
	defer metrics.SetBackend(nil)

	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// Closed so the vacuum has published and terminated before the gauges are
	// read: the publisher is a background goroutine since rmp #2308, so a test
	// that reads without joining it is asserting on a race rather than on the
	// export.
	defer func() {
		if err := g.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for i := 0; i < reclaimThreshold*2; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	waitWithinBound(t, g)
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := []string{
		"lpg.mvcc.versions.total",
		"lpg.mvcc.versions.bound",
		"lpg.mvcc.versions.ceiling",
		"lpg.mvcc.versions.property",
		"lpg.mvcc.watermark",
		"lpg.mvcc.oldest_snapshot_age",
		"lpg.mvcc.readers.active",
		"lpg.mvcc.snapshots.active",
		"lpg.mvcc.snapshots.unregistered",
		"lpg.mvcc.snapshots.capacity",
		"lpg.mvcc.index_removal_backlog",
		// The WRITE side (rmp #2312). Once MVCC is the module's only concurrency
		// control, a substrate observable only in what it retains is half observable:
		// these are what produces the retention, and the conflict rate is the signal
		// that says whether the workload contends at all.
		"lpg.mvcc.writers.active",
		"lpg.mvcc.commits",
		"lpg.mvcc.aborts",
		"lpg.mvcc.conflicts.total",
		"lpg.mvcc.conflict_rate",
		"lpg.mvcc.conflicts.total.store.node_labels",
		// The RETAINED-chain distribution, which is read cost per object. The buckets
		// must all be published even when empty: a dashboard that has to guess which
		// buckets exist cannot plot a distribution.
		"lpg.mvcc.chain_depth.bucket.1",
		"lpg.mvcc.chain_depth.bucket.128_inf",
		"lpg.mvcc.chain_depth.deepest",
		"lpg.mvcc.chain_depth.chains",
		"lpg.mvcc.chain_depth.deepest.store.node_properties",
		"lpg.mvcc.chain_depth.chains.store.adjacency",
		// The vacuum's own utilisation, which the observability mandate asks of
		// every goroutine and every bounded worker the library owns.
		"lpg.mvcc.vacuum.running",
		"lpg.mvcc.vacuum.passes",
		"lpg.mvcc.vacuum.reclaimed",
		"lpg.mvcc.vacuum.backlog",
		"lpg.mvcc.vacuum.capped_passes",
		"lpg.mvcc.vacuum.records_per_pass",
		"lpg.mvcc.vacuum.starts",
		"lpg.mvcc.vacuum.exits",
		"lpg.mvcc.vacuum.pass_mean_ns",
	}
	for _, name := range want {
		if _, ok := rec.gauge(name); !ok {
			t.Errorf("gauge %q was never published; version memory is bounded but not observable", name)
		}
	}
	if got, _ := rec.gauge("lpg.mvcc.versions.bound"); got != float64(reclaimThreshold) {
		t.Errorf("the exported bound is %v, want %d", got, reclaimThreshold)
	}
	if got, _ := rec.gauge("lpg.mvcc.versions.ceiling"); got != float64(reclaimDebtCeiling) {
		t.Errorf("the exported ceiling is %v, want %d", got, reclaimDebtCeiling)
	}
	// The vacuum must have both STARTED and STOPPED: a goroutine the library owns
	// with no observable end is the leak the mandate forbids.
	if got, _ := rec.gauge("lpg.mvcc.vacuum.running"); got != 0 {
		t.Errorf("the vacuum reports running=%v after Close, want 0", got)
	}
	if vs := g.VacuumStats(); vs.Starts == 0 || vs.Starts != vs.Exits || vs.Running {
		t.Errorf("vacuum lifecycle is not balanced after Close: %+v", vs)
	}
}

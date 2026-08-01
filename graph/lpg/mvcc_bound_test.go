package lpg

// mvcc_bound_test.go — MVCC P6c (rmp #2286): version memory is bounded, and the
// bound is observable.
//
// The point of these tests is that NOTHING in them calls a reclaimer. A
// reclaimer that has to be invoked by a test is not reclamation; what has to be
// true is that an ordinary workload keeps its own memory flat.

import (
	"fmt"
	"testing"

	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

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

	// The churn bound plus one sweep's worth of slack. Two thresholds is
	// generous and still two orders of magnitude below the unbounded behaviour
	// this replaces (12 000 modifications would have left 12 000 records).
	if peak > 2*reclaimThreshold {
		t.Fatalf("peak version count %d over %d modifications exceeds the stated bound of %d: "+
			"reclamation is not keeping up with the write path", peak, rounds, 2*reclaimThreshold)
	}
	s := g.MVCCStats()
	if !s.WithinBound() {
		t.Errorf("after the workload the substrate holds %d records against a bound of %d, with "+
			"%d active readers — nothing should be holding them back", s.Total, s.Bound, s.ActiveReaders)
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
	for i := 0; i < rounds; i++ {
		k := fmt.Sprintf("n%d", i%64)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "w", Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if n := g.VersionCount(); n > 2*reclaimThreshold {
		t.Fatalf("%d direct writes left %d live version records against a bound of %d",
			rounds, n, 2*reclaimThreshold)
	}
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
	if s.ActiveReaders != 1 {
		t.Fatalf("the horizon reports %d active readers, want 1", s.ActiveReaders)
	}
	if s.WithinBound() {
		t.Fatal("a reader older than three thresholds of churn is holding nothing back; either " +
			"the reader is not registered or the versions it needs were freed")
	}
	if s.OldestReaderAge() == 0 {
		t.Error("the oldest reader's age is zero while it is demonstrably behind: growth cannot " +
			"be attributed to the read that caused it")
	}
	if s.UnregisteredReaders != 0 {
		t.Errorf("%d readers failed to register; reclamation is suspended for a different reason "+
			"than this test is measuring", s.UnregisteredReaders)
	}

	// And once it leaves, the next write's sweep takes it all back.
	g.EndRead(snap)
	for i := 0; i < reclaimThreshold+1; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("post-reader write %d: %v", i, err)
		}
	}
	if s := g.MVCCStats(); !s.WithinBound() {
		t.Errorf("after the long reader left, the substrate still holds %d records against a "+
			"bound of %d", s.Total, s.Bound)
	}
}

// gaugeRecorder captures the gauges the sweep publishes.
type gaugeRecorder struct {
	seen map[string]float64
}

func (g *gaugeRecorder) IncCounter(string, uint64)            {}
func (g *gaugeRecorder) ObserveLatency(string, time.Duration) {}
func (g *gaugeRecorder) SetGauge(name string, v float64)      { g.seen[name] = v }

// TestMVCCMetrics_AreExported pins that the utilisation the bounded-resources
// mandate asks for actually reaches a backend, rather than being available only
// to a caller that knows to ask.
func TestMVCCMetrics_AreExported(t *testing.T) {
	rec := &gaugeRecorder{seen: map[string]float64{}}
	metrics.SetBackend(rec)
	defer metrics.SetBackend(nil)

	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
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

	want := []string{
		"lpg.mvcc.versions.total",
		"lpg.mvcc.versions.bound",
		"lpg.mvcc.versions.property",
		"lpg.mvcc.watermark",
		"lpg.mvcc.readers.active",
		"lpg.mvcc.readers.unregistered",
		"lpg.mvcc.index_removal_backlog",
	}
	for _, name := range want {
		if _, ok := rec.seen[name]; !ok {
			t.Errorf("gauge %q was never published; version memory is bounded but not observable", name)
		}
	}
	if got := rec.seen["lpg.mvcc.versions.bound"]; got != float64(reclaimThreshold) {
		t.Errorf("the exported bound is %v, want %d", got, reclaimThreshold)
	}
}

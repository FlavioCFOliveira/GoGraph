package lpg

// mvcc_conflict_metrics_test.go — rmp #2312 AC 5: the conflict series is proved to
// move, by driving a real conflict.
//
// Layer: short.
//
// A counter nobody has watched increment is a counter that does not work. An earlier
// attempt at this test drove the conflict through Graph.SetNodeLabel inside
// ApplyInVersionedTx, which does NOT route through the transaction's writeCtx: the
// write was refused for an unrelated reason, the counter never fired, and the failure
// looked like a broken metric rather than a broken test. The construction below is the
// one the existing conflict suite uses (see TestConflict_NodeExistence), so it is known
// to reach [writeCtx.conflictErr].

import (
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// recordingBackend captures counters so a test can read a series back. The production
// backend is write-only.
type recordingBackend struct {
	mu       sync.Mutex
	counters map[string]uint64
}

func (r *recordingBackend) IncCounter(name string, delta uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counters == nil {
		r.counters = make(map[string]uint64)
	}
	r.counters[name] += delta
}

func (r *recordingBackend) ObserveLatency(string, time.Duration) {}
func (r *recordingBackend) SetGauge(string, float64)             {}

func (r *recordingBackend) get(name string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.counters[name]
	return v, ok
}

// installRecorder swaps in a recording backend for the duration of the test and
// restores the previous one afterwards.
func installRecorder(t *testing.T) *recordingBackend {
	t.Helper()
	rec := &recordingBackend{}
	metrics.SetBackend(rec)
	t.Cleanup(func() { metrics.SetBackend(nil) })
	return rec
}

// TestMVCCMetrics_ConflictSeriesMoves drives a first-updater-wins conflict on the node
// existence chain — A deletes a node and stays in flight, B revives it — and asserts
// both conflict series increment.
//
// It asserts the COUNT and not merely movement, because the two plausible placements of
// this counter differ precisely in the count: one increment per doomed transaction (what
// a contention rate needs) against one per refused write (which scales with transaction
// size and cannot be divided by a commit count).
func TestMVCCMetrics_ConflictSeriesMoves(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	rec := installRecorder(t)

	// A removes the node and stays in flight, so its death record is the chain head B
	// cannot see.
	txA := g.beginLabelTx()
	txA.removeNode("a")

	// B revives it, which records a birth on the same chain. This is the refusal.
	txB := g.beginLabelTx()
	if err := txB.addNode("a"); err != nil {
		t.Fatalf("B addNode: %v", err)
	}
	_, errB := txB.commit()
	if errB == nil {
		t.Fatal("B's overlapping revival was ACCEPTED, so no conflict occurred and this " +
			"test proves nothing about the counter — fix the construction, do not relax it")
	}

	got, ok := rec.get("lpg.mvcc.conflicts")
	if !ok {
		t.Fatalf("B was refused with %v but the series lpg.mvcc.conflicts does not exist in "+
			"the registry at all: the emit site in writeCtx.conflictErr was never reached, so "+
			"the counter cannot report contention", errB)
	}
	if got != 1 {
		t.Fatalf("one doomed transaction produced %d increments of lpg.mvcc.conflicts, want 1. "+
			"More than one means the counter is reporting REFUSED WRITES rather than "+
			"transactions, so it scales with transaction size and cannot be read as a "+
			"contention rate against lpg.mvcc.commits", got)
	}

	// The per-store label is what tells an operator WHICH structure contended.
	perStore, ok := rec.get("lpg.mvcc.conflicts.store.node_existence")
	if !ok || perStore != 1 {
		t.Errorf("the per-store series for the node-existence store did not move as expected "+
			"(present=%v value=%d): without it an operator sees that the workload contends "+
			"but not on what", ok, perStore)
	}

	// A must still be able to commit — it was never the doomed one, and a conflict
	// counter that fired for the winner would overstate contention twofold.
	if _, err := txA.commit(); err != nil {
		t.Fatalf("A was refused too (%v): the conflict count would then describe both sides "+
			"of a first-updater-wins pair", err)
	}
	if got, _ := rec.get("lpg.mvcc.conflicts"); got != 1 {
		t.Fatalf("the winning transaction's commit moved lpg.mvcc.conflicts to %d: only the "+
			"REFUSED transaction may be counted", got)
	}
}

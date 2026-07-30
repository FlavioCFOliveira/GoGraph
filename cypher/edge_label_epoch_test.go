package cypher

// edge_label_epoch_test.go — regression gate for rmp #2255.
//
// # The defect
//
// A committed relationship-type change was invisible to a WARM Engine,
// indefinitely. [edgeTypeFilterFor] keys the edge-type-filter cache on
// [lpg.Graph.TopoGeneration] (cypher/api.go), and the cache serves a hit
// whenever the sampled epoch is unchanged — but none of the three edge-label
// mutators bumped that epoch, because none of them changes edge TOPOLOGY:
// Graph.SetEdgeLabel, Graph.RemoveEdgeLabel and Graph.SetEdgeLabelByHandle.
//
// So a fsynced, applied `Tx.SetEdgeLabel` left the graph correct and the cached
// filter stale, and `MATCH ()-[r:T2]->() RETURN count(r)` answered 0 where 1 was
// correct. The symmetric case is worse: after RemoveEdgeLabel the warm Engine
// still reported the removed type — a PHANTOM type — and because
// cypher/undo_record.go uses RemoveEdgeLabel as the rollback inverse of a label
// SET, an ABORTED transaction's label stayed visible. That is an Atomicity
// violation on top of the Consistency and Isolation ones.
//
// Sprint 313 (fafc50c7) fixed the ORDERING half of this class — bump AFTER the
// last epoch-keyed write. It did not fix the case where there is NO bump at all
// because no topology changed, which is what this file pins.
//
// # Why the green suite missed it
//
// The epoch is a topology counter and every existing test that mutates edge
// labels also adds or removes an edge in the same statement, which bumps the
// epoch for its own reasons and masks the omission. Reaching the defect needs a
// label mutation in ISOLATION against an already-warm cache — the shape below.
//
// # Why the assertions are absolute, not differential
//
// Every expected count here is hand-computed from a fixture small enough to
// enumerate by eye. A differential against a freshly built Engine is included
// only as corroboration that the graph is right and the cache is wrong; on its
// own it would be the weaker gate, because a defect that made BOTH engines agree
// on a wrong answer would pass it.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelEpochScalar runs q, requires exactly one row of one column, and returns
// that value rendered as a string.
func labelEpochScalar(t *testing.T, eng *Engine, q string) string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var rows []string
	for res.Next() {
		var b strings.Builder
		for i := range res.Columns() {
			fmt.Fprintf(&b, "%v", res.ValueAt(i))
		}
		rows = append(rows, b.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	if len(rows) != 1 {
		t.Fatalf("run %q yielded %d rows, want exactly 1", q, len(rows))
	}
	return rows[0]
}

// labelEpochFixture builds the minimal shape that reaches the defect: exactly
// one edge a->b carrying exactly one type, T1.
func labelEpochFixture(t *testing.T) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
	}
	if err := g.AddEdge("a", "b", 1.0); err != nil {
		t.Fatalf("AddEdge(a->b): %v", err)
	}
	g.SetEdgeLabel("a", "b", "T1")
	return g, NewEngine(g)
}

// TestEdgeLabelEpoch_SetEdgeLabelVisibleToWarmEngine_2255 is the primary gate.
// It fails on the pre-fix code with count(r:T2) = 0.
func TestEdgeLabelEpoch_SetEdgeLabelVisibleToWarmEngine_2255(t *testing.T) {
	t.Parallel()
	g, eng := labelEpochFixture(t)
	const q = `MATCH ()-[r:T2]->() RETURN count(r)`

	// Warm the cache under the T2 key. Absolute oracle: no edge carries T2 yet.
	if got := labelEpochScalar(t, eng, q); got != "0" {
		t.Fatalf("before the label was added: count(r:T2) = %s, want 0", got)
	}

	// The mutation the durable apply path performs for OpSetEdgeLabel, and the
	// one the MERGE MATCH branch performs. No topology changes.
	g.SetEdgeLabel("a", "b", "T2")

	// Absolute oracle: the one a->b edge now carries T2, so exactly one row.
	if got := labelEpochScalar(t, eng, q); got != "1" {
		t.Fatalf("WARM engine after a committed SetEdgeLabel: count(r:T2) = %s, want 1 "+
			"(the edge-type filter cache served a stale entry because the epoch did not move)", got)
	}

	// Corroboration only: the graph itself was always right.
	if got := labelEpochScalar(t, NewEngine(g), q); got != "1" {
		t.Fatalf("FRESH engine: count(r:T2) = %s, want 1 — the graph itself is wrong, "+
			"so this is not the cache defect #2255 describes", got)
	}
}

// TestEdgeLabelEpoch_RemoveEdgeLabelLeavesNoPhantomType_2255 covers the
// removal direction, which is also the rollback inverse of a label SET: on the
// pre-fix code the warm Engine still counted the removed type.
func TestEdgeLabelEpoch_RemoveEdgeLabelLeavesNoPhantomType_2255(t *testing.T) {
	t.Parallel()
	g, eng := labelEpochFixture(t)
	const q = `MATCH ()-[r:T1]->() RETURN count(r)`

	// Warm the cache under the T1 key. Absolute oracle: the edge carries T1.
	if got := labelEpochScalar(t, eng, q); got != "1" {
		t.Fatalf("before removal: count(r:T1) = %s, want 1", got)
	}

	g.RemoveEdgeLabel("a", "b", "T1")

	// Absolute oracle: no edge carries T1 any more.
	if got := labelEpochScalar(t, eng, q); got != "0" {
		t.Fatalf("WARM engine after RemoveEdgeLabel: count(r:T1) = %s, want 0 "+
			"(a phantom relationship type survived in the stale filter; as the rollback "+
			"inverse of a label SET this makes an aborted transaction observable)", got)
	}
}

// TestEdgeLabelEpoch_ByHandleVisibleToWarmEngine_2255 covers the per-handle
// mutator, which is the authoritative per-edge type store on a multigraph and is
// what the durable OpSetEdgeLabelByHandle applies.
func TestEdgeLabelEpoch_ByHandleVisibleToWarmEngine_2255(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
	}
	// Two PARALLEL a->b edges, so the per-handle store is the only place that can
	// distinguish their types.
	h1, err := g.AddEdgeH("a", "b", 1.0)
	if err != nil {
		t.Fatalf("AddEdgeH first: %v", err)
	}
	h2, err := g.AddEdgeH("a", "b", 1.0)
	if err != nil {
		t.Fatalf("AddEdgeH second: %v", err)
	}
	if h1 == 0 || h2 == 0 || h1 == h2 {
		t.Fatalf("expected two distinct non-zero handles, got %d and %d", h1, h2)
	}
	g.SetEdgeLabelByHandle("a", "b", h1, "T1")

	eng := NewEngine(g)
	const q = `MATCH ()-[r:T2]->() RETURN count(r)`

	// Warm the cache. Absolute oracle: neither parallel edge carries T2.
	if got := labelEpochScalar(t, eng, q); got != "0" {
		t.Fatalf("before the label was added: count(r:T2) = %s, want 0", got)
	}

	g.SetEdgeLabelByHandle("a", "b", h2, "T2")

	// Absolute oracle: exactly ONE of the two parallel edges carries T2.
	if got := labelEpochScalar(t, eng, q); got != "1" {
		t.Fatalf("WARM engine after a committed SetEdgeLabelByHandle: count(r:T2) = %s, want 1", got)
	}
}

// TestEdgeLabelEpoch_NoOpMutationDoesNotBumpEpoch_2255 is the other half of the
// contract, and the reason the fix bumps on a REAL change rather than on every
// call. The MERGE MATCH branch (cypher/exec/merge_relationship.go) calls
// SetEdgeLabel on every MERGE that binds an existing relationship, guarded by
// edgeHasRequestedType — so that call is always idempotent. Bumping there would
// invalidate the forward/reverse CSR pair cache on a read-mostly MERGE workload
// and force an O(V+E) rebuild for a mutation that changed nothing.
func TestEdgeLabelEpoch_NoOpMutationDoesNotBumpEpoch_2255(t *testing.T) {
	t.Parallel()
	g, _ := labelEpochFixture(t)
	before := g.TopoGeneration()

	g.SetEdgeLabel("a", "b", "T1")        // already present on the slot
	g.RemoveEdgeLabel("a", "b", "ABSENT") // never present
	g.SetEdgeLabel("a", "zz", "T9")       // no such edge — early return

	if after := g.TopoGeneration(); after != before {
		t.Fatalf("no-op edge-label mutations moved the topology epoch %d -> %d; a "+
			"read-mostly MERGE workload would pay a spurious O(V+E) CSR-pair rebuild", before, after)
	}

	// A genuine change must move it exactly once.
	g.SetEdgeLabel("a", "b", "T2")
	if after := g.TopoGeneration(); after != before+1 {
		t.Fatalf("a real edge-label change moved the topology epoch %d -> %d, want exactly one bump to %d",
			before, after, before+1)
	}

	// And removing a label that IS present must move it exactly once more.
	g.RemoveEdgeLabel("a", "b", "T2")
	if after := g.TopoGeneration(); after != before+2 {
		t.Fatalf("a real edge-label removal moved the topology epoch to %d, want exactly %d",
			after, before+2)
	}
}

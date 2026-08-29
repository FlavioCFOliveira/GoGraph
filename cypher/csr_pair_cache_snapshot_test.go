package cypher

// csr_pair_cache_snapshot_test.go — the CSR pair cache key under MVCC
// (rmp #2293).
//
// The cross-query CSR pair cache is keyed on lpg.Graph.TopoGeneration, the
// topology epoch. That key was COMPLETE before MVCC, because the pair was then
// a pure function of the live adjacency and the epoch moved on every change to
// it.
//
// MVCC P4c (rmp #2290) made ReadView.LiveNodeFilter answer as of the reader's
// SNAPSHOT rather than the present, and csrPairFromGraph passes that filter to
// csr.BuildFromAdjListLive. So the pair is now a function of TWO inputs — the
// present adjacency AND the reader's snapshot — while the key still names only
// the first. Two readers at one epoch holding different snapshots therefore
// need different pairs, and the cache hands each of them the other's.
//
// The consequence is not a slow query, it is a wrong answer in both
// directions: a reader whose snapshot predates a node's birth files a pair with
// that node's arcs filtered out, and the next reader at the same epoch — whose
// snapshot is newer, and for whom the edge is committed and visible — is served
// it and cannot see the edge. Symmetrically an old reader can be served a newer
// reader's pair and traverse an arc to a node that did not exist at its
// instant.
//
// Both tests below are deterministic: they hold a snapshot across the write
// rather than racing for the window. The concurrent shape is covered by
// TestCSRPairCache_ConcurrentQueriesAgree, which found this as an intermittent
// "final count = 5, want 6" roughly one run in forty.
//
// Layer: short.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// snapCacheEngine builds the same hub-and-spokes fixture the sibling
// csr_pair_cache_test.go uses — one node 'a' with five outgoing :R arcs — and
// returns the engine and its graph.
func snapCacheEngine(t *testing.T) (*Engine, *lpg.Graph[string, float64], func(string)) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	run := func(q string) {
		t.Helper()
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("RunAny(%q): %v", q, err)
		}
		for res.Next() { // drain: a write's effects are not applied until iterated
		}
		if err := res.Err(); err != nil {
			t.Fatalf("RunAny(%q): %v", q, err)
		}
		res.Close()
	}
	run(`CREATE (a:N {key:'a'})`)
	for i := 0; i < 5; i++ {
		run(fmt.Sprintf(`CREATE (b:N {key:'b%d'})`, i))
		run(fmt.Sprintf(
			`MATCH (a:N {key:'a'}),(b:N {key:'b%d'}) CREATE (a)-[:R]->(b)`, i))
	}
	return eng, g, run
}

// TestCSRPairCache_OldSnapshotMustNotPoisonNewReader is the direction that was
// observed failing: a reader holding a pre-write snapshot must not file its
// narrower pair where a newer reader will be served it.
//
// It asserts on the pair a NEW reader receives, not on cache internals, because
// that is the property that matters — the new reader's snapshot postdates the
// commit, so the sixth arc is committed and visible to it, and a pair that
// omits it is a committed write made invisible.
func TestCSRPairCache_OldSnapshotMustNotPoisonNewReader(t *testing.T) {
	t.Parallel()
	_, g, run := snapCacheEngine(t)

	// Opened BEFORE the write below, and held across it. This is the shape a
	// long analytical read has, and the reason the read barrier was retired.
	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	run(`CREATE (b:N {key:'bx'})`)
	run(`MATCH (a:N {key:'a'}),(b:N {key:'bx'}) CREATE (a)-[:R]->(b)`)

	cache := newCSRPairCache()
	// The old reader goes FIRST, so it is the one that populates the entry.
	oldFwd, _ := csrPairCached(cache, g.ReadAt(old))
	nowFwd, _ := csrPairCached(cache, g.ReadAt(nil))

	// The old reader must not see the arc to a node born after its snapshot.
	if got := oldFwd.Size(); got != 5 {
		t.Errorf("pair for the OLD snapshot has %d arcs, want 5: a reader must not "+
			"observe an edge to a node created after its snapshot", got)
	}
	// And the new reader must see it. This is the assertion that failed.
	if got := nowFwd.Size(); got != 6 {
		t.Errorf("pair for the CURRENT instant has %d arcs, want 6: the cache served "+
			"the old snapshot's narrower pair, making a committed edge invisible", got)
	}
}

// TestCSRPairBuild_EdgeBetweenExistingNodes probes the OTHER dimension of the
// same build, and it is deliberately separate from the cache: no cache is
// involved, so whatever it reports is a property of csrPairFromGraph itself.
//
// The arc added here joins two nodes that BOTH existed at the old snapshot, so
// the liveness filter cannot mask it — if the build reads the present
// adjacency, the old reader sees an edge committed after its instant.
//
// This test records what the build actually does rather than asserting a
// conclusion reached in advance; see the failure message for which way it went.
func TestCSRPairBuild_EdgeBetweenExistingNodes(t *testing.T) {
	t.Parallel()
	_, g, run := snapCacheEngine(t)

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	// A parallel arc between two already-live nodes: no new node, so nothing
	// for the liveness filter to remove.
	run(`MATCH (a:N {key:'a'}),(b:N {key:'b0'}) CREATE (a)-[:R]->(b)`)

	oldFwd, _ := csrPairFromGraph(g.ReadAt(old))
	nowFwd, _ := csrPairFromGraph(g.ReadAt(nil))
	if got := nowFwd.Size(); got != 6 {
		t.Fatalf("pair at the CURRENT instant has %d arcs, want 6: the fixture is wrong", got)
	}
	if got := oldFwd.Size(); got != 5 {
		t.Errorf("pair for the OLD snapshot has %d arcs, want 5: csrPairFromGraph reads "+
			"the PRESENT adjacency, so this read observes an edge committed after its "+
			"snapshot — the CSR path bypasses MVCC on the edge dimension", got)
	}
}

// TestCSRPairBuild_EdgeDeletedAfterSnapshot is the DELETE direction, and it is
// the one an add-only test suite silently omits.
//
// Visibility has to hold both ways round: an arc removed after a read began was
// present at that read's instant and must stay present for it. A build that
// resolved the present would drop it, which is the same class of defect as
// showing an arc added after the snapshot, and it fails on the opposite input —
// so neither test substitutes for the other.
func TestCSRPairBuild_EdgeDeletedAfterSnapshot(t *testing.T) {
	t.Parallel()
	_, g, run := snapCacheEngine(t)

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	// Delete one of the five arcs. Both endpoints survive, so this isolates arc
	// removal from node removal.
	run(`MATCH (:N {key:'a'})-[r:R]->(:N {key:'b0'}) DELETE r`)

	oldFwd, _ := csrPairFromGraph(g.ReadAt(old))
	nowFwd, _ := csrPairFromGraph(g.ReadAt(nil))
	if got := nowFwd.Size(); got != 4 {
		t.Fatalf("pair at the CURRENT instant has %d arcs, want 4: the fixture is wrong", got)
	}
	if got := oldFwd.Size(); got != 5 {
		t.Errorf("pair for the OLD snapshot has %d arcs, want 5: an arc that existed at "+
			"this read's instant was removed from under it", got)
	}
}

// TestCSRPairCache_NewReaderMustNotPoisonOldSnapshot is the mirror direction,
// and it fails for the same missing key rather than for a second cause: the
// current reader populates the entry first, and the old snapshot is then served
// a pair containing an arc to a node that did not exist at its instant.
//
// Worth pinning separately because a fix that only orders the two puts — say,
// preferring the widest pair — would satisfy the test above and still leave a
// reader observing the future here.
func TestCSRPairCache_NewReaderMustNotPoisonOldSnapshot(t *testing.T) {
	t.Parallel()
	_, g, run := snapCacheEngine(t)

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	run(`CREATE (b:N {key:'bx'})`)
	run(`MATCH (a:N {key:'a'}),(b:N {key:'bx'}) CREATE (a)-[:R]->(b)`)

	cache := newCSRPairCache()
	// Reversed order relative to the test above: the CURRENT reader populates.
	nowFwd, _ := csrPairCached(cache, g.ReadAt(nil))
	oldFwd, _ := csrPairCached(cache, g.ReadAt(old))

	if got := nowFwd.Size(); got != 6 {
		t.Errorf("pair for the CURRENT instant has %d arcs, want 6", got)
	}
	if got := oldFwd.Size(); got != 5 {
		t.Errorf("pair for the OLD snapshot has %d arcs, want 5: the cache served a "+
			"newer reader's pair, letting this read observe an edge to a node that "+
			"did not exist at its instant", got)
	}
}

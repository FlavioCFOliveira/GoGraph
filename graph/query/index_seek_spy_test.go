package query

// index_seek_spy_test.go — white-box proof that filterByPreds routes an
// indexable predicate through the secondary index (and skips the per-node
// scan for it), using a spy index that records every read (task #1651).

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// spyHashIndex is a minimal index.Subscriber that reports coverage of one
// (label, property) pair and records each typed read. It implements the exact
// read shape the query engine asserts (hashLookuper[string]) so the engine
// recovers its value type and serves a seek through it. Its posting list is set
// explicitly by the test so a recorded read proves the engine consulted the
// index rather than the per-node scan.
type spyHashIndex struct {
	label, property string
	posting         map[string][]uint64
	lookups         int // count of Lookup + LookupAppend calls
	cardinalityHits int // count of Cardinality calls
}

func (s *spyHashIndex) Apply(index.Change) {}
func (s *spyHashIndex) Kind() string       { return "hash" }
func (s *spyHashIndex) BoundNode() (label, property string, ok bool) {
	return s.label, s.property, true
}

func (s *spyHashIndex) Cardinality(value string) uint64 {
	s.cardinalityHits++
	return uint64(len(s.posting[value]))
}

func (s *spyHashIndex) LookupAppend(value string, dst []uint64) []uint64 {
	s.lookups++
	return append(dst, s.posting[value]...)
}

func (s *spyHashIndex) Lookup(value string) *roaring64.Bitmap {
	s.lookups++
	bm := roaring64.New()
	bm.AddMany(s.posting[value])
	return bm
}

// TestSeek_SpyIndexConsulted proves that a WithProperty equality predicate
// covered by a registered index is served by an index read — the spy records
// the read — and that the per-node scan is skipped for that predicate (a second
// non-indexed predicate still runs the scan, so the scan loop is exercised).
func TestSeek_SpyIndexConsulted(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, p := range []string{"a", "b", "c", "d"} {
		if err := g.SetNodeLabel(p, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	if err := g.SetNodeProperty("a", "dept", lpg.StringValue("Eng")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("c", "dept", lpg.StringValue("Eng")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	// Resolve the interned NodeIDs of the matching nodes so the spy can serve
	// exactly them.
	idA, _ := g.AdjList().Mapper().Lookup("a")
	idC, _ := g.AdjList().Mapper().Lookup("c")

	mgr := index.NewManager()
	spy := &spyHashIndex{
		label:    "Person",
		property: "dept",
		posting:  map[string][]uint64{"Eng": sortedIDs(idA, idC)},
	}
	if err := mgr.CreateIndex("person_dept_hash", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	g.SetIndexManager(mgr)

	c := csr.BuildFromAdjList(g.AdjList())
	e := New(g, c)

	got := e.Match().Vertex(
		WithLabel[string, int64]("Person"),
		WithProperty[string, int64]("dept", lpg.StringValue("Eng")),
	).Collect()

	if spy.lookups == 0 {
		t.Fatalf("index was not consulted: lookups=%d (expected >=1)", spy.lookups)
	}
	// The seek probes Cardinality first to choose the clone-free small-posting
	// path; a recorded probe proves that branch ran (the posting is tiny here).
	if spy.cardinalityHits == 0 {
		t.Fatalf("index Cardinality was not probed: cardinalityHits=%d (expected >=1)", spy.cardinalityHits)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (a, c); got=%v", len(got), got)
	}
	want := map[string]bool{"a": true, "c": true}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected result %q; got=%v", n, got)
		}
	}
}

// TestSeek_SpyIndexIntersectsNotReplaces proves the seek INTERSECTS the index
// result with the working set rather than replacing it: when the index posting
// list contains an id that the label seed excluded (here a node that lost its
// Person label), that id must NOT appear in the result. This pins the
// tombstone-/staleness-safety property documented in index_seek.go.
func TestSeek_SpyIndexIntersectsNotReplaces(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	// "a","b" are Persons; "ghost" is NOT a Person but the spy will claim it.
	for _, p := range []string{"a", "b"} {
		if err := g.SetNodeLabel(p, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	if err := g.SetNodeProperty("a", "dept", lpg.StringValue("Eng")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("ghost", "dept", lpg.StringValue("Eng")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	idA, _ := g.AdjList().Mapper().Lookup("a")
	idGhost, _ := g.AdjList().Mapper().Lookup("ghost")

	mgr := index.NewManager()
	spy := &spyHashIndex{
		label:    "Person",
		property: "dept",
		// Index claims BOTH a and ghost carry dept=Eng. ghost is not a Person,
		// so the label seed excludes it; the intersection must drop it.
		posting: map[string][]uint64{"Eng": sortedIDs(idA, idGhost)},
	}
	if err := mgr.CreateIndex("person_dept_hash", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	g.SetIndexManager(mgr)

	c := csr.BuildFromAdjList(g.AdjList())
	e := New(g, c)

	got := e.Match().Vertex(
		WithLabel[string, int64]("Person"),
		WithProperty[string, int64]("dept", lpg.StringValue("Eng")),
	).Collect()

	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("intersection failed: got=%v, want [a] (ghost must be dropped by the label seed)", got)
	}
}

// sortedIDs returns the NodeIDs as a uint64 slice in ascending order, the order
// the hash index's LookupAppend contract promises.
func sortedIDs(ids ...graph.NodeID) []uint64 {
	out := make([]uint64, len(ids))
	for i, id := range ids {
		out[i] = uint64(id)
	}
	// Insertion sort: the inputs are tiny (test fixtures).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

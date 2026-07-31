package adjlist

// mvcc_adj_test.go — MVCC P3 (rmp #2281): the adjacency reconstructs its past.
//
// Layer: short.
//
// The property under test is that a reader with an older start timestamp sees
// the edge set as it was, while the current reader sees the current one — with
// no lock taken on either path. The awkward cases are the ones that have broken
// versioning schemes elsewhere: a node whose edges are ALL removed (the write
// publishes nil, which would drop the chain), and a transaction that writes
// several edges to one node (which must leave ONE version record, not one per
// edge, or an ordinary statement grows the chain without bound).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func versionedList(t *testing.T) (*AdjList[string, float64], *mvcc.Clock) {
	t.Helper()
	a := New[string, float64](Config{Directed: true, Multigraph: true})
	a.EnableVersioning()
	return a, &mvcc.Clock{}
}

// autoWrite performs fn as a single-statement write committed at a fresh
// timestamp, which is the autocommit shape.
func autoWrite(a *AdjList[string, float64], clk *mvcc.Clock, fn func()) uint64 {
	ts := clk.NextCommitTS()
	a.SetWriteStamp(nil, ts)
	fn()
	a.SetWriteStamp(nil, 0)
	return ts
}

func idOf(t *testing.T, a *AdjList[string, float64], k string) graph.NodeID {
	t.Helper()
	id, ok := a.Mapper().Lookup(k)
	if !ok {
		t.Fatalf("%s not interned", k)
	}
	return id
}

func TestAdjVersion_ReconstructsAnOlderEdgeSet(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	before := clk.ReadTS()
	tsB := autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	tsC := autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "c", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	id := idOf(t, a, "a")

	if got := len(a.EntryNeighboursAsOf(id, before, 0)); got != 0 {
		t.Fatalf("a reader from before any edge sees %d neighbours, want 0", got)
	}
	if got := len(a.EntryNeighboursAsOf(id, tsB, 0)); got != 1 {
		t.Fatalf("a reader at the first commit sees %d neighbours, want 1", got)
	}
	if got := len(a.EntryNeighboursAsOf(id, tsC, 0)); got != 2 {
		t.Fatalf("a reader at the second commit sees %d neighbours, want 2", got)
	}
}

// TestAdjVersion_RemovingEveryEdgeKeepsThePast is the case the naive hook gets
// wrong: removing a node's last edge publishes a nil entry, and a chain hung
// off the entry would vanish with it, making the node look as though it never
// had an edge at all.
func TestAdjVersion_RemovingEveryEdgeKeepsThePast(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	tsAdd := autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	tsDel := autoWrite(a, clk, func() { a.RemoveEdge("a", "b") })
	id := idOf(t, a, "a")

	if got := len(a.EntryNeighboursAsOf(id, tsDel, 0)); got != 0 {
		t.Fatalf("a reader after the removal sees %d neighbours, want 0", got)
	}
	if got := len(a.EntryNeighboursAsOf(id, tsAdd, 0)); got != 1 {
		t.Fatalf("a reader from BEFORE the removal sees %d neighbours, want 1: the version "+
			"chain was dropped when the last edge went and the past became unreachable", got)
	}
}

// TestAdjVersion_OneRecordPerNodePerTransaction pins the chain-growth guard. A
// statement writing several edges to one node replaces its entry once per edge;
// without the same-transaction check that would leave one record per EDGE.
func TestAdjVersion_OneRecordPerNodePerTransaction(t *testing.T) {
	a, clk := versionedList(t)
	if err := a.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for i := 0; i < 8; i++ {
		if err := a.AddNode(string(rune('b' + i))); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	info := mvcc.NewCommitInfo(clk.NextTxID())
	a.SetWriteStamp(info, 0)
	for i := 0; i < 8; i++ {
		if err := a.AddEdge("a", string(rune('b'+i)), 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	a.SetWriteStamp(nil, 0)
	info.Commit(clk.NextCommitTS())

	if n := a.VersionCount(); n != 1 {
		t.Fatalf("one transaction writing 8 edges to one node left %d version records, want 1: "+
			"an ordinary multi-edge statement would grow the chain without bound", n)
	}
}

// TestAdjVersion_UncommittedIsInvisible pins that another transaction's
// in-flight topology change is not visible, and becomes visible atomically.
func TestAdjVersion_UncommittedIsInvisible(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	observer := clk.ReadTS()

	info := mvcc.NewCommitInfo(clk.NextTxID())
	a.SetWriteStamp(info, 0)
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	a.SetWriteStamp(nil, 0)

	if got := len(a.EntryNeighboursAsOf(id, observer, 0)); got != 0 {
		t.Fatalf("an observer sees %d neighbours of an UNCOMMITTED transaction, want 0", got)
	}
	commitTS := clk.NextCommitTS()
	info.Commit(commitTS)
	if got := len(a.EntryNeighboursAsOf(id, commitTS, 0)); got != 2 {
		t.Fatalf("after commit a reader sees %d neighbours, want 2 — both, atomically", got)
	}
	if got := len(a.EntryNeighboursAsOf(id, observer, 0)); got != 0 {
		t.Fatalf("a reader that started before the commit now sees %d neighbours, want 0", got)
	}
}

// TestAdjVersion_InertByDefault pins that nothing is recorded unless armed.
func TestAdjVersion_InertByDefault(t *testing.T) {
	a := New[string, float64](Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	a.RemoveEdge("a", "b")
	if n := a.VersionCount(); n != 0 {
		t.Fatalf("recorded %d versions without being armed; the mechanism must ship inert", n)
	}
}

// BenchmarkAdjVersionWrite measures what retaining a version costs an edge
// append, at two graph sizes an order of magnitude apart.
//
// The claim under test is the same as every earlier phase's: the per-write cost
// must not depend on how many nodes the graph holds. It should be cheaper here
// than for labels or properties, because the entry the version points at is one
// the write already produced — nothing is copied.
func BenchmarkAdjVersionWrite(b *testing.B) {
	for _, size := range []int{10000, 1000000} {
		for _, versioned := range []bool{false, true} {
			name := "nodes=" + itoaBench(size) + "/versioned=" + boolStr(versioned)
			b.Run(name, func(b *testing.B) {
				a := New[string, float64](Config{Directed: true, Multigraph: true})
				if versioned {
					a.EnableVersioning()
				}
				keys := make([]string, size)
				for i := 0; i < size; i++ {
					keys[i] = "n" + itoaBench(i)
					if err := a.AddNode(keys[i]); err != nil {
						b.Fatalf("AddNode: %v", err)
					}
				}
				clk := &mvcc.Clock{}
				// A bounded working set, so the measurement is of the version
				// record and not of a structure growing with the loop.
				work := size
				if work > 1000 {
					work = 1000
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if versioned {
						a.SetWriteStamp(nil, clk.NextCommitTS())
					}
					if err := a.AddEdge(keys[i%work], keys[(i*7+1)%work], 1); err != nil {
						b.Fatalf("AddEdge: %v", err)
					}
				}
				b.StopTimer()
				a.SetWriteStamp(nil, 0)
			})
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

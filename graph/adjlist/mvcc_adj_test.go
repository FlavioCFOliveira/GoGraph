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
	clk := &mvcc.Clock{}
	ws := &mvcc.WriteStamp{}
	ws.SetClock(clk)
	a.SetWriteStamp(ws)
	return a, clk
}

// autoWrite performs fn as a single-statement write committed at a fresh
// timestamp, which is the autocommit shape. With the stamp DISARMED each
// version takes its own commit timestamp inline, which is what a direct write
// outside any transaction does.
func autoWrite(a *AdjList[string, float64], clk *mvcc.Clock, fn func()) uint64 {
	fn()
	return clk.ReadTS()
}

// txWrite performs fn as ONE transaction: every version it creates shares one
// commit record, published at a single timestamp on return.
func txWrite(a *AdjList[string, float64], clk *mvcc.Clock, fn func()) (*mvcc.CommitInfo, uint64) {
	ws := a.WriteStampForTest()
	beginTx(ws)
	fn()
	info, _ := ws.End()
	if info == nil {
		return nil, clk.ReadTS()
	}
	ts := clk.NextCommitTS()
	info.Commit(ts)
	return info, ts
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
	txWrite(a, clk, func() {
		for i := 0; i < 8; i++ {
			if err := a.AddEdge("a", string(rune('b'+i)), 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	})

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

	ws := a.WriteStampForTest()
	beginTx(ws)
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	info, _ := ws.End()

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
				clk := &mvcc.Clock{}
				if versioned {
					a.EnableVersioning()
					ws := &mvcc.WriteStamp{}
					ws.SetClock(clk)
					a.SetWriteStamp(ws)
				}
				keys := make([]string, size)
				for i := 0; i < size; i++ {
					keys[i] = "n" + itoaBench(i)
					if err := a.AddNode(keys[i]); err != nil {
						b.Fatalf("AddNode: %v", err)
					}
				}
				// A bounded working set, so the measurement is of the version
				// record and not of a structure growing with the loop.
				work := size
				if work > 1000 {
					work = 1000
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := a.AddEdge(keys[i%work], keys[(i*7+1)%work], 1); err != nil {
						b.Fatalf("AddEdge: %v", err)
					}
				}
				b.StopTimer()
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

// EntryLabelsAsOf returns the per-slot relationship-type column of id as it was
// at startTS. Test-only accessor: it exists to prove the claim below, not to be
// a public surface before an operator needs one.
func (a *AdjList[N, W]) EntryLabelsAsOf(id graph.NodeID, startTS, txID uint64) []uint32 {
	s := &a.shards[id&shardMask]
	e := a.entryAsOf(s, uint64(id)>>shardBits, startTS, txID)
	if e == nil {
		return nil
	}
	return e.labels
}

// TestAdjVersion_TypesAndPropertiesComeForFree pins something that would
// otherwise be an assumption: versioning the ENTRY also versions the per-slot
// relationship types and the edge-property column, because both live INSIDE the
// entry and both are already copy-on-write.
//
// adjEntry.labels is the type column and adjEntry.aux is the opaque
// edge-property column, whose own contract says "Both lifecycle methods return
// a NEW immutable column (copy-on-write); the receiver is never mutated, so a
// concurrent lock-free reader holding the prior entry (and thus the prior
// column) is unaffected."
//
// If that reasoning is right, P3 needs no separate mechanism for either. It is
// asserted rather than argued because "it should follow" is how a versioning
// hole gets shipped.
func TestAdjVersion_TypesAndPropertiesComeForFree(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	dst := idOf(t, a, "b")

	autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	tsTyped := autoWrite(a, clk, func() {
		if !a.SetEdgeLabelSlot(id, dst, 7) {
			t.Fatal("SetEdgeLabelSlot did not apply")
		}
	})
	tsRetyped := autoWrite(a, clk, func() {
		if !a.SetEdgeLabelSlot(id, dst, 9) {
			t.Fatal("SetEdgeLabelSlot did not apply")
		}
	})

	labelAt := func(ts uint64) uint32 {
		ls := a.EntryLabelsAsOf(id, ts, 0)
		if len(ls) == 0 {
			return 0
		}
		return ls[0]
	}
	if got := labelAt(tsRetyped); got != 9 {
		t.Fatalf("current type is %d, want 9", got)
	}
	if got := labelAt(tsTyped); got != 7 {
		t.Fatalf("the type at the earlier timestamp is %d, want 7: the per-slot type column "+
			"is not being versioned by the entry chain", got)
	}
}

// TestAdjVersion_ReclaimFreesOnlyTheUnreachable is the reclamation contract: a
// version at or before the watermark goes, and one after it stays.
//
// The direction that matters is the second. Freeing too little wastes memory;
// freeing too much makes a live reader unable to find the version it is
// entitled to, which is silent data loss.
func TestAdjVersion_ReclaimFreesOnlyTheUnreachable(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	ts1 := autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	ts2 := autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "c", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	ts3 := autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "d", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	if got := a.VersionCount(); got != 3 {
		t.Fatalf("three writes left %d versions, want 3", got)
	}

	// A watermark of zero means "reclaim nothing", which is what the horizon
	// reports while a reader could not be registered.
	if n := a.Reclaim(0); n != 0 {
		t.Fatalf("Reclaim(0) freed %d records; zero means reclaim NOTHING", n)
	}

	// Watermark at ts2: readers all began at or after ts2, so the versions
	// superseded at ts1 and ts2 are unreachable and the one at ts3 is not.
	freed := a.Reclaim(ts2)
	if freed == 0 {
		t.Fatal("Reclaim freed nothing with a watermark past two versions")
	}
	if got := len(a.EntryNeighboursAsOf(id, ts3, 0)); got != 3 {
		t.Fatalf("the current edge set reads %d after reclamation, want 3", got)
	}
	// A reader at ts2 must still see the state at ts2: two edges. That is the
	// version reclamation was told to preserve.
	if got := len(a.EntryNeighboursAsOf(id, ts2, 0)); got != 2 {
		t.Fatalf("a reader at the watermark sees %d neighbours, want 2 — reclamation freed a "+
			"version a live reader is entitled to", got)
	}
	if a.VersionCount() != int64(3-freed) {
		t.Fatalf("VersionCount is %d after freeing %d of 3", a.VersionCount(), freed)
	}
	_ = ts1
}

// TestAdjVersion_ReclaimIsBoundedByTheHorizon wires the two halves together:
// a registered reader must hold reclamation back.
func TestAdjVersion_ReclaimIsBoundedByTheHorizon(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	old := clk.ReadTS()
	autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})
	autoWrite(a, clk, func() {
		if err := a.AddEdge("a", "c", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})

	var h mvcc.Horizon
	slot := h.Enter(old) // a long reader, started before either write
	a.Reclaim(h.Oldest(clk.ReadTS()))
	if got := len(a.EntryNeighboursAsOf(id, old, 0)); got != 0 {
		t.Fatalf("the long reader sees %d neighbours after reclamation, want 0: the horizon "+
			"did not hold reclamation back", got)
	}
	h.Leave(slot)

	// With the reader gone the watermark advances and the history goes.
	if n := a.Reclaim(h.Oldest(clk.ReadTS())); n == 0 {
		t.Fatal("Reclaim freed nothing once the long reader left")
	}
	if a.VersionCount() != 0 {
		t.Fatalf("%d versions remain with no reader active", a.VersionCount())
	}
}

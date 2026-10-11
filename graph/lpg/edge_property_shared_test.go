package lpg

// edge_property_shared_test.go — the shared append buffer of the fused edge
// property append (rmp #3090): an earlier version never observes a later
// append, a cell is written at most once, a rolled-back append leaves no value,
// and a ReadView pinned before an append (with its per-source slot index) never
// sees the new slot or value.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func fusedAppend(t *testing.T, c *edgePropCols, oldLen int, key PropertyKeyID, v int64) *edgePropCols {
	t.Helper()
	var out auxColumn
	if c == nil {
		out = newEdgePropColsAux(oldLen+1, &edgePropPayload{keyID: key, value: Int64Value(v)})
	} else {
		out = c.GrowSlotWithValue(oldLen, &edgePropPayload{keyID: key, value: Int64Value(v)})
	}
	b, ok := out.(*edgePropCols)
	if !ok || b == nil || b.length != oldLen+1 {
		t.Fatalf("append at %d returned %#v", oldLen, out)
	}
	return b
}

func readSlots(c *edgePropCols, key PropertyKeyID, n int) []string {
	out := make([]string, n)
	for s := 0; s < n; s++ {
		if v, ok := c.get(key, s); ok {
			i, _ := v.Int64()
			out[s] = fmt.Sprint(i)
		} else {
			out[s] = "-"
		}
	}
	return out
}

// TestSharedAppend_EarlierVersionNeverSeesLaterSlot pins every version of a
// fused build and re-reads all of them after the build finished, and after a
// second build branched from an older version.
func TestSharedAppend_EarlierVersionNeverSeesLaterSlot(t *testing.T) {
	const n = 40
	const key PropertyKeyID = 3
	versions := make([]*edgePropCols, n)
	var b *edgePropCols
	for i := 0; i < n; i++ {
		b = fusedAppend(t, b, i, key, int64(100+i))
		versions[i] = b
	}
	if b.shr != 1 {
		t.Fatalf("the fused build did not stay in shared mode (shr=%b); the test proves nothing", b.shr)
	}
	check := func(stage string) {
		t.Helper()
		for i, v := range versions {
			got := readSlots(v, key, n+1)
			for s := 0; s <= n; s++ {
				want := "-"
				if s <= i {
					want = fmt.Sprint(100 + s)
				}
				if got[s] != want {
					t.Fatalf("%s: version %d slot %d reads %s, want %s", stage, i, s, got[s], want)
				}
			}
		}
	}
	check("after build")

	// Branch from version 9 (np 10, behind the cursor): the append must not write
	// cell 10 of the shared buffer, which versions 10.. own.
	before := unsafe.SliceData(versions[9].cols[0].idx) //nolint:gosec // G103: audited — compares backing addresses for identity only, never dereferences them.
	br := fusedAppend(t, versions[9], 10, key, 999)
	if unsafe.SliceData(br.cols[0].idx) == before { //nolint:gosec // G103: audited — compares backing addresses for identity only, never dereferences them.
		t.Fatal("an append from a version behind the write cursor reused the shared buffer")
	}
	if got := readSlots(br, key, 12); got[10] != "999" || got[9] != "109" || got[11] != "-" {
		t.Fatalf("branched version reads %v", got)
	}
	check("after branch")

	// An absent grow on a wholly shared block reuses the header array.
	g := br.GrowSlot(11).(*edgePropCols)
	if got := readSlots(g, key, 13); got[11] != "-" || got[10] != "999" {
		t.Fatalf("absent grow reads %v", got)
	}
	check("after absent grow")

	// The freeze pass materialises an exact, unshared block that reads the same.
	f := b.Compact().(*edgePropCols)
	if f.shr != 0 {
		t.Fatal("Compact left a shared column")
	}
	for s, w := range readSlots(b, key, n) {
		if got := readSlots(f, key, n)[s]; got != w {
			t.Fatalf("Compact changed slot %d: %s -> %s", s, w, got)
		}
	}
}

// TestSharedAppend_RolledBackAppendLeavesNoValue aborts a fused append that
// wrote its cell in place, then checks the present state, a ReadView pinned
// before the append and its slot index, and that the next append does not
// rewrite the aborted cell.
func TestSharedAppend_RolledBackAppendLeavesNoValue(t *testing.T) {
	g := New[string, float64](adjlist.Config{})
	for _, k := range []string{"h", "t0", "t1", "t2"} {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 33; i++ { // degree >= minSourceSlotsDegree; capacity 64, cell 33 spare
		if err := g.AddEdgeLabeledWithProperty("h", "t0", 1, "R", "q", Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	hID, _ := g.adj.Mapper().Lookup("h")
	t1, _ := g.adj.Mapper().Lookup("t1")
	t2, _ := g.adj.Mapper().Lookup("t2")
	pre := asEdgePropCols(g.adj.LoadEntryAux(hID))
	if pre == nil || pre.shr != 1 || pre.np[0] != 33 || len(pre.cols[0].idx) != 64 {
		t.Fatalf("seed block is not a shared buffer with a spare cell: %+v", pre)
	}

	snap := g.BeginRead()
	defer g.EndRead(snap)
	view := g.ReadAt(snap)
	slots := view.buildSourceSlots(hID)
	if slots == nil {
		t.Fatal("hub not indexed")
	}

	boom := errors.New("roll back")
	err := g.ApplyDurable(context.Background(), func(tx WriteTx) error {
		return g.Writer(tx).AddEdgeLabeledWithProperty("h", "t1", 1, "R", "q", Int64Value(777))
	}, func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("ApplyDurable = %v, want the rollback error", err)
	}
	cur := asEdgePropCols(g.adj.LoadEntryAux(hID))
	if cur == nil || cur.np[0] != 33 || cur.cols[0].tail.n != 34 {
		t.Fatalf("the aborted append did not take the in-place path (np=%d tail=%d); the test proves nothing",
			cur.np[0], cur.cols[0].tail.n)
	}
	if _, ok := g.GetEdgeProperty("h", "t1", "q"); ok || g.HasEdgeAsOf("h", "t1", nil) {
		t.Fatal("the rolled-back append is visible at the present instant")
	}

	if err := g.AddEdgeLabeledWithProperty("h", "t2", 1, "R", "q", Int64Value(555)); err != nil {
		t.Fatal(err)
	}
	if got := int64(pre.cols[0].nums[33]); got != 777 {
		t.Fatalf("the aborted cell was rewritten (value %d): a reader of the aborted entry could observe it", got)
	}
	if v, ok := g.GetEdgeProperty("h", "t2", "q"); !ok {
		t.Fatal("the committed append is missing")
	} else if i, _ := v.Int64(); i != 555 {
		t.Fatalf("committed append reads %d, want 555", i)
	}
	if _, ok := g.GetEdgeProperty("h", "t1", "q"); ok {
		t.Fatal("the rolled-back value resurfaced after the next append")
	}

	// The pinned view sees neither append, through the scan and the slot index.
	for _, dst := range []string{"t1", "t2"} {
		if _, ok := view.GetEdgeProperty("h", dst, "q"); ok {
			t.Fatalf("pinned view sees h->%s", dst)
		}
		if p := view.EdgeProperties("h", dst); len(p) != 0 {
			t.Fatalf("pinned view sees properties on h->%s: %v", dst, p)
		}
	}
	pk, _ := g.pkeys.Lookup("q")
	for _, dst := range []graph.NodeID{t1, t2} {
		if _, found, ok := view.edgePropertyBySlots(slots, dst, pk); ok && found {
			t.Fatalf("the pinned slot index answered a value for node %d", dst)
		}
		if has, ok := view.edgeHasPropertyBySlots(slots, dst, pk); ok && has {
			t.Fatalf("the pinned slot index reports presence for node %d", dst)
		}
	}
	if v, ok := view.GetEdgeProperty("h", "t0", "q"); !ok {
		t.Fatal("pinned view lost the seeded property")
	} else if i, _ := v.Int64(); i != 33 {
		t.Fatalf("pinned view reads h->t0 q=%d, want 33", i)
	}
}

// TestSharedAppend_ConcurrentReadersDuringFusedBuild races present-time and
// pinned readers against a fused build at one hub (meaningful under -race).
func TestSharedAppend_ConcurrentReadersDuringFusedBuild(t *testing.T) {
	const n = 256
	g := New[string, float64](adjlist.Config{})
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("t%d", i)
		if err := g.AddNode(names[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddNode("h"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	errc := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for k := r; ; k = (k + 7) % n {
				select {
				case <-done:
					return
				default:
				}
				snap := g.BeginRead()
				view := g.ReadAt(snap)
				for j := 0; j < n; j += 17 {
					if v, ok := view.GetEdgeProperty("h", names[(k+j)%n], "q"); ok {
						if i, _ := v.Int64(); i != int64((k+j)%n) {
							errc <- fmt.Errorf("h->%s reads %d", names[(k+j)%n], i)
							g.EndRead(snap)
							return
						}
					}
				}
				g.EndRead(snap)
			}
		}(r)
	}
	for i := 0; i < n; i++ {
		if err := g.AddEdgeLabeledWithProperty("h", names[i], 1, "R", "q", Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if v, ok := g.GetEdgeProperty("h", names[i], "q"); !ok {
			t.Fatalf("h->%s lost its property", names[i])
		} else if x, _ := v.Int64(); x != int64(i) {
			t.Fatalf("h->%s reads %d", names[i], x)
		}
	}
}

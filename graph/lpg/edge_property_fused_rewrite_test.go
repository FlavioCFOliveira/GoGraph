package lpg

// edge_property_fused_rewrite_test.go — rmp #3093. A fused edge-property append
// that branches from a block a later version already extended (the pre-image a
// rollback restores verbatim) must not write into a cell that later version
// owns: a reader still holding the aborted block would observe the next
// append's value. edge_property_shared.go guards the shared-buffer path with a
// high-water check; these tests cover the fallback path (more than
// maxSharedCols columns, a bit-packed bool column, a FOR-packed date column).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func growWith(t *testing.T, c *edgePropCols, oldLen int, key PropertyKeyID, v PropertyValue) *edgePropCols {
	t.Helper()
	b, ok := c.GrowSlotWithValue(oldLen, &edgePropPayload{keyID: key, value: v}).(*edgePropCols)
	if !ok || b == nil || b.length != oldLen+1 {
		t.Fatalf("append at %d returned %#v", oldLen, b)
	}
	return b
}

// assertBranchKeepsAborted appends 777 (the aborted branch) and then 555 (the
// next append) to the same pre-image, while a goroutine reads the aborted
// block, and fails when the aborted block's new slot changed.
func assertBranchKeepsAborted(t *testing.T, pre *edgePropCols, key PropertyKeyID) {
	t.Helper()
	n := pre.length
	aborted := growWith(t, pre, n, key, Int64Value(777))
	if v, ok := aborted.get(key, n); !ok {
		t.Fatal("aborted branch lost its own value")
	} else if i, _ := v.Int64(); i != 777 {
		t.Fatalf("aborted branch reads %d before the next append", i)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(1)
	go func() { // a lock-free reader still holding the aborted block
		defer wg.Done()
		<-start
		for k := 0; k < 64; k++ {
			_, _ = aborted.get(key, n)
		}
	}()
	close(start)
	next := growWith(t, pre, n, key, Int64Value(555))
	wg.Wait()
	if v, _ := next.get(key, n); func() int64 { i, _ := v.Int64(); return i }() != 555 {
		t.Fatal("the next append lost its value")
	}
	if v, ok := aborted.get(key, n); !ok {
		t.Fatal("the aborted block's slot vanished")
	} else if i, _ := v.Int64(); i != 777 {
		t.Fatalf("the next append rewrote a cell the aborted block owns: it now reads %d, want 777", i)
	}
}

// targetHasSpare reports whether the column for key has spare capacity in its
// idx backing — the precondition for an in-place tail write.
func targetHasSpare(c *edgePropCols, key PropertyKeyID) bool {
	for i := range c.cols {
		if c.cols[i].key == key && (i >= maxSharedCols || c.shr&(1<<uint(i)) == 0) {
			return cap(c.cols[i].idx) > len(c.cols[i].idx)
		}
	}
	return false
}

func TestFusedRewrite_ThreeKeys(t *testing.T) {
	var b *edgePropCols
	b = newEdgePropColsAux(1, &edgePropPayload{keyID: 1, value: Int64Value(0)}).(*edgePropCols)
	for i := 1; i < 9; i++ {
		b = growWith(t, b, i, PropertyKeyID(1+i%3), Int64Value(int64(i)))
	}
	if len(b.cols) != 3 || !targetHasSpare(b, 1) {
		t.Fatalf("precondition: want 3 unshared columns with spare capacity on key 1, got %d cols", len(b.cols))
	}
	assertBranchKeepsAborted(t, b, 1)
}

func TestFusedRewrite_BoolColumn(t *testing.T) {
	var b *edgePropCols
	b = newEdgePropColsAux(1, &edgePropPayload{keyID: 9, value: BoolValue(true)}).(*edgePropCols)
	for i := 1; i < 6; i++ {
		b = growWith(t, b, i, 1, Int64Value(int64(i)))
	}
	if !targetHasSpare(b, 1) {
		t.Fatal("precondition: the int column beside a bool column has no spare capacity")
	}
	assertBranchKeepsAborted(t, b, 1)
}

func TestFusedRewrite_PackedDateColumn(t *testing.T) {
	var b *edgePropCols
	days := benchDays(324)
	for i, d := range days {
		b = b.set(7, i, i+1, dateVal(d))
	}
	b = b.set(1, 0, len(days), Int64Value(0))
	b = b.Compact().(*edgePropCols)
	packed := false
	for i := range b.cols {
		packed = packed || b.cols[i].packedDate
	}
	if !packed {
		t.Fatal("precondition: no FOR-packed date column")
	}
	assertBranchKeepsAborted(t, b, 1)
	// One fallback append converts the packed column; the next branch point is
	// the block after it.
	assertBranchKeepsAborted(t, growWith(t, b, b.length, 1, Int64Value(1)), 1)
}

// TestFusedRewrite_RolledBackAppendThroughApplyDurable is the end-to-end shape:
// the aborted entry's block is captured while it is the stored value, the
// transaction is withdrawn, and another transaction appends.
func TestFusedRewrite_RolledBackAppendThroughApplyDurable(t *testing.T) {
	g := New[string, float64](adjlist.Config{})
	keys := []string{"a", "b", "c"}
	for i := 0; i < 9; i++ {
		if err := g.AddEdgeLabeledWithProperty("h", "t0", 1, "R", keys[i%3], Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	hID, _ := g.adj.Mapper().Lookup("h")
	ka, _ := g.pkeys.Lookup("a")
	pre := asEdgePropCols(g.adj.LoadEntryAux(hID))
	if pre == nil || len(pre.cols) != 3 || !targetHasSpare(pre, ka) {
		t.Fatal("precondition: want a 3-column block with spare capacity on key a")
	}
	slot := pre.length
	var aborted *edgePropCols
	boom := errors.New("roll back")
	err := g.ApplyDurable(context.Background(), func(tx WriteTx) error {
		return g.Writer(tx).AddEdgeLabeledWithProperty("h", "t1", 1, "R", "a", Int64Value(777))
	}, func() error {
		aborted = asEdgePropCols(g.adj.LoadEntryAux(hID))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ApplyDurable = %v, want the rollback error", err)
	}
	if aborted == nil || aborted == pre || aborted.length != slot+1 {
		t.Fatal("precondition: the aborted entry's block was not captured")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < 64; k++ {
			_, _ = aborted.get(ka, slot)
		}
	}()
	if err := g.AddEdgeLabeledWithProperty("h", "t2", 1, "R", "a", Int64Value(555)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if v, ok := aborted.get(ka, slot); !ok {
		t.Fatal("the aborted block's slot vanished")
	} else if i, _ := v.Int64(); i != 777 {
		t.Fatalf("the next append rewrote a cell the aborted entry owns: it reads %d, want 777", i)
	}
	if v, ok := g.GetEdgeProperty("h", "t2", "a"); !ok {
		t.Fatal("the committed append is missing")
	} else if i, _ := v.Int64(); i != 555 {
		t.Fatalf("committed append reads %d", i)
	}
	if _, ok := g.GetEdgeProperty("h", "t1", "a"); ok {
		t.Fatal("the rolled-back append is visible")
	}
}

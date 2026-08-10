package exec

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestCommitDynamicPreSizesBacking pins rmp #2381: the Put that commits a
// dynamic column to a type must give it a backing sized to the Chunk's
// capacity, so filling the chunk to capacity performs NO slice growth.
//
// Before the fix NewDynamicChunk left every backing nil and the committing Put
// appended into it, so the first fill walked append's whole growth series up
// from nil — 128 248 B in 16 allocations for an int64 column filled to the
// default 4096-row capacity, against the 32 960 B in 3 allocations of the
// identically shaped column NewChunk pre-sizes. The assertion below is on
// capacity rather than on an allocation count because capacity is the property
// that makes the growth impossible; TestCommitDynamicFirstFillDoesNotAllocate
// measures the consequence.
func TestCommitDynamicPreSizesBacking(t *testing.T) {
	const capacity = 64

	cases := []struct {
		name  string
		put   func(c *Chunk)
		capOf func(col *column) int
	}{
		{"int64", func(c *Chunk) { c.PutInt64(0, 1) }, func(col *column) int { return cap(col.i64) }},
		{"float64", func(c *Chunk) { c.PutFloat64(0, 1) }, func(col *column) int { return cap(col.f64) }},
		{"string", func(c *Chunk) { c.PutString(0, "a") }, func(col *column) int { return cap(col.str) }},
		{"bool", func(c *Chunk) { c.PutBool(0, true) }, func(col *column) int { return cap(col.b) }},
		{"null-commits-boxed", func(c *Chunk) { c.PutNull(0) }, func(col *column) int { return cap(col.boxed) }},
		{"value-commits-boxed", func(c *Chunk) { c.PutValue(0, expr.ListValue{}) }, func(col *column) int { return cap(col.boxed) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewDynamicChunk(capacity, 1)
			tc.put(c)
			if got := tc.capOf(&c.cols[0]); got < capacity {
				t.Errorf("after the committing Put the backing has capacity %d, want >= the chunk capacity %d: "+
					"the column will grow by doubling for the rest of the fill", got, capacity)
			}
		})
	}
}

// TestCommitDynamicFirstFillDoesNotAllocate is the consequence assertion: with
// the backing committed at capacity, filling a fresh dynamic chunk to exactly
// its capacity must perform ONE allocation for that column's backing, never a
// growth series. testing.AllocsPerRun counts allocations, so the growing fill
// is unmistakable: it measured 12 against 3 for the pre-sized chunk.
func TestCommitDynamicFirstFillDoesNotAllocate(t *testing.T) {
	const capacity = 1024

	dynamic := testing.AllocsPerRun(50, func() {
		c := NewDynamicChunk(capacity, 1)
		for row := 0; row < capacity; row++ {
			c.PutInt64(0, int64(row))
		}
	})
	static := testing.AllocsPerRun(50, func() {
		c := NewChunk(capacity, expr.KindInteger)
		for row := 0; row < capacity; row++ {
			c.AppendInt64(0, int64(row))
		}
	})

	// The dynamic chunk carries one extra allocation over the static one at
	// most (the columns slice is shaped the same; the backing is now a single
	// sized make in both). A doubling fill costs ~11 more than this.
	if dynamic > static+1 {
		t.Errorf("filling a fresh dynamic chunk to capacity performed %.0f allocations against %.0f for the "+
			"identically shaped pre-sized chunk: the committing Put is not sizing the backing, so the first "+
			"fill is growing it by doubling from nil", dynamic, static)
	}
}

// TestCommitDynamicReuseAllocatesNothing pins the guard that makes the fix free
// for pooled chunks: Reset restores the undecided tag but RETAINS the backing,
// so a chunk re-committed to the same kind must find its array already there
// and allocate nothing at all.
func TestCommitDynamicReuseAllocatesNothing(t *testing.T) {
	const capacity = 256
	c := NewDynamicChunk(capacity, 1)
	for row := 0; row < capacity; row++ {
		c.PutInt64(0, int64(row))
	}

	refill := testing.AllocsPerRun(50, func() {
		c.Reset()
		for row := 0; row < capacity; row++ {
			c.PutInt64(0, int64(row))
		}
	})
	if refill != 0 {
		t.Errorf("Reset+refill of a warm dynamic chunk performed %.0f allocations, want 0: "+
			"the retained backing is no longer being reused", refill)
	}
}

// TestPromoteToBoxedPreSizes pins the sibling fix: promotion happens mid-fill,
// so the boxed slice it builds must be sized to the chunk capacity rather than
// to the rows already stored, or the rest of the batch grows it by doubling.
func TestPromoteToBoxedPreSizes(t *testing.T) {
	const capacity = 64
	c := NewDynamicChunk(capacity, 1)
	c.PutInt64(0, 1)    // commits to int64
	c.PutString(0, "x") // conflicting scalar kind -> promotes to boxed

	if got := cap(c.cols[0].boxed); got < capacity {
		t.Errorf("after promotion the boxed backing has capacity %d, want >= the chunk capacity %d", got, capacity)
	}
	// The promotion must preserve what was already stored.
	if c.Len() != 2 {
		t.Fatalf("promoted column holds %d rows, want 2", c.Len())
	}
	if v := c.BoxCell(0, 0); !expr.Equivalent(v, expr.IntegerValue(1)) {
		t.Errorf("row 0 boxed to %v, want the integer 1 the column held before promotion", v)
	}
	if v := c.BoxCell(0, 1); !expr.Equivalent(v, expr.StringValue("x")) {
		t.Errorf("row 1 boxed to %v, want the string that triggered the promotion", v)
	}
}

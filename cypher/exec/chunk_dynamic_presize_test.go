package exec

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestCommitDynamicReservesTheFloorNotTheCapacity pins rmp #2389: the Put that
// commits a dynamic column to a type must reserve dynamicCommitFloor rows, NOT
// the Chunk's whole capacity.
//
// The capacity is a hint, and when the plan exposes no sound RowCountHint —
// which an indexed point lookup correctly does not — NewDynamicChunk falls back
// to DefaultChunkCapacity. rmp #2381 reserved that fallback eagerly on the first
// Put, so a query returning ONE row reserved 32 KB to hold 8 bytes. Measured on
// examples/35_mvcc_mixed_workload over 3 interleaved rounds, that cost 119.6 GB
// of allocation against 43.9 GB and 326k ops/s against 745k.
//
// The capacity below is DefaultChunkCapacity deliberately: it is the exact value
// the regression was measured at, so this test fails against the reverted fix.
func TestCommitDynamicReservesTheFloorNotTheCapacity(t *testing.T) {
	const capacity = DefaultChunkCapacity

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

	// An ABSOLUTE bound, deliberately not written in terms of dynamicCommitFloor:
	// a test that asserts against the constant moves its own goalposts, and would
	// go green again the moment someone raised the floor back to the capacity.
	// 64 rows is far above any sane floor and far below the 4096 that regressed.
	const maxReserve = 64

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewDynamicChunk(capacity, 1)
			tc.put(c)
			got := tc.capOf(&c.cols[0])
			if got > maxReserve {
				t.Errorf("after the committing Put of ONE row the backing reserved %d rows (chunk capacity %d), "+
					"want at most %d: reserving the capacity charges a single-row result for a batch it never receives",
					got, capacity, maxReserve)
			}
			if got < 1 {
				t.Errorf("the committing Put reserved %d rows: a column left at nil walks append's whole growth "+
					"series, which is the defect rmp #2381 removed", got)
			}
		})
	}
}

// TestAppendNullEscalatesTheBacking pins that the NULL path escalates like every
// other append path. A column fed mostly NULLs — an OPTIONAL MATCH that misses, a
// projection over a sparse property — reaches the batch through AppendNull and
// nothing else, so without growTo there it would walk the doubling series up from
// dynamicCommitFloor: the very growth the floor is only safe because growTo
// prevents. Doubling would leave capacity 32 here; the escalation leaves 4096.
func TestAppendNullEscalatesTheBacking(t *testing.T) {
	const capacity = DefaultChunkCapacity

	c := NewDynamicChunk(capacity, 1)
	for row := 0; row <= dynamicCommitFloor; row++ { // one past the floor
		c.PutNull(0)
	}

	if got := cap(c.cols[0].boxed); got < capacity {
		t.Errorf("after outgrowing the %d-row floor the NULL-filled backing has capacity %d, want the chunk's %d: "+
			"AppendNull is growing by doubling instead of escalating in one step", dynamicCommitFloor, got, capacity)
	}
	if c.Len() != dynamicCommitFloor+1 {
		t.Fatalf("column holds %d rows, want %d", c.Len(), dynamicCommitFloor+1)
	}
	// The escalation must not disturb what the column already held: every row is
	// still NULL, and it is the validity bitmap that says so.
	for row := 0; row < c.Len(); row++ {
		if v := c.BoxCell(0, row); !expr.IsNull(v) {
			t.Fatalf("row %d boxed to %v after the escalation, want NULL", row, v)
		}
	}
}

// TestCommitDynamicFloorIsBoundedByTheCapacity pins the other side: a Chunk
// deliberately built narrower than the floor must never be widened past the
// capacity it was asked for.
func TestCommitDynamicFloorIsBoundedByTheCapacity(t *testing.T) {
	const capacity = 4 // narrower than dynamicCommitFloor

	c := NewDynamicChunk(capacity, 1)
	c.PutInt64(0, 1)
	if got := cap(c.cols[0].i64); got != capacity {
		t.Errorf("a %d-row chunk reserved capacity %d, want %d: the floor must not widen a chunk past its capacity",
			capacity, got, capacity)
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

// TestPromoteToBoxedPreSizes pins the sibling behaviour: promotion happens
// mid-fill, so the boxed slice it builds must be sized past the rows already
// stored — otherwise the very next push regrows it — but only to the same floor
// commitDynamic reserves, never to the chunk's whole capacity (rmp #2389).
func TestPromoteToBoxedPreSizes(t *testing.T) {
	const capacity = DefaultChunkCapacity
	c := NewDynamicChunk(capacity, 1)
	c.PutInt64(0, 1)    // commits to int64
	c.PutString(0, "x") // conflicting scalar kind -> promotes to boxed

	// Absolute bounds, for the reason given in
	// TestCommitDynamicReservesTheFloorNotTheCapacity.
	const maxReserve = 64
	if got := cap(c.cols[0].boxed); got > maxReserve {
		t.Errorf("after promotion the boxed backing reserved %d rows (chunk capacity %d), want at most %d: "+
			"a promotion at row 2 must not reserve the chunk's whole capacity", got, capacity, maxReserve)
	} else if got < c.Len() {
		t.Errorf("after promotion the boxed backing reserved %d rows but the column already holds %d", got, c.Len())
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

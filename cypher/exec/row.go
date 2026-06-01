// Package exec implements the Volcano-style executor for the Cypher query
// engine. It defines the [Operator] interface, the [Row]/[RowSlab] data model,
// and the pipeline driver [Drain].
//
// # Data model
//
// A [Row] is a slice of [expr.Value]. A [RowSlab] is a bounded, pooled
// container of pre-allocated rows used to eliminate per-row heap allocations
// in the hot path.
//
// # Concurrency
//
// [RowSlab] is NOT safe for concurrent use. Each goroutine must obtain its own
// slab from [NewRowSlab] or from a [sync.Pool] managed by the caller. The
// exported [SlabPool] provides a ready-to-use pool with default capacity.
package exec

import (
	"errors"
	"fmt"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// DefaultSlabCapacity is the default maximum number of rows a [RowSlab] holds
// before returning [ErrSlabOverflow]. It is sized to keep a typical pipeline
// batch within a few cache lines.
const DefaultSlabCapacity = 4096

// ErrSlabOverflow is returned by [RowSlab.Alloc] when the slab has reached its
// capacity limit. Callers must flush or reset the slab before continuing.
var ErrSlabOverflow = errors.New("exec: row slab overflow")

// ─────────────────────────────────────────────────────────────────────────────
// Row
// ─────────────────────────────────────────────────────────────────────────────

// Row is a single tuple in the pipeline: a slice of [expr.Value] whose
// positions correspond to the operator's output schema. The slice is owned by
// the [RowSlab] that allocated it; callers must not retain it beyond the slab's
// lifetime.
type Row []expr.Value

// ─────────────────────────────────────────────────────────────────────────────
// RowSlab
// ─────────────────────────────────────────────────────────────────────────────

// RowSlab is a bounded arena of pre-allocated rows. It eliminates per-row heap
// allocations by backing all rows in a single flat slice. Each call to [Alloc]
// hands out a sub-slice at zero GC cost after the initial backing allocation.
//
// RowSlab is NOT safe for concurrent use; each pipeline stage owns its own
// instance, typically obtained from [SlabPool].
//
// # Lifecycle
//
//  1. Obtain a slab from [SlabPool.Get] (or call [NewRowSlab]).
//  2. Call [Alloc] for each row needed in the current batch.
//  3. When the batch is fully processed, call [Reset] and return the slab to
//     [SlabPool.Put].
//
// [Reset] zeroes the column values in every allocated row so that no
// [expr.Value] reference is retained past the batch boundary (preventing GC
// nepotism between batches).
type RowSlab struct {
	width     int   // number of columns per row (0 = variable; use explicit width in Alloc)
	cap       int   // maximum number of rows
	rows      []Row // pre-allocated backing slice: len = cap, each element pre-allocated
	allocated int   // number of rows handed out so far
}

// NewRowSlab creates a RowSlab with the given column count and row capacity.
// width is the number of expr.Value slots pre-allocated per row; pass 0 for
// variable-width rows (callers supply their own slice to [AllocRaw]).
// capacity must be ≥ 1; [DefaultSlabCapacity] is a reasonable default.
func NewRowSlab(width, capacity int) *RowSlab {
	if capacity < 1 {
		capacity = DefaultSlabCapacity
	}
	s := &RowSlab{
		width: width,
		cap:   capacity,
		rows:  make([]Row, capacity),
	}
	if width > 0 {
		// Single contiguous backing allocation for all column slots.
		backing := make([]expr.Value, capacity*width)
		for i := range s.rows {
			s.rows[i] = backing[i*width : i*width+width : i*width+width]
		}
	}
	return s
}

// Alloc returns the next available pre-allocated row in the slab.
// It returns [ErrSlabOverflow] if the slab is exhausted.
// The row width matches the width passed to [NewRowSlab]; for variable-width
// slabs (width=0) use [AllocRaw].
func (s *RowSlab) Alloc() (Row, error) {
	if s.width == 0 {
		return nil, fmt.Errorf("exec: RowSlab created with width=0; use AllocRaw")
	}
	if s.allocated >= s.cap {
		return nil, ErrSlabOverflow
	}
	r := s.rows[s.allocated]
	s.allocated++
	return r, nil
}

// AllocRaw returns the next row slot for variable-width slabs (width=0).
// The caller is responsible for setting the returned row to a correctly-sized
// slice before use. For fixed-width slabs, use [Alloc].
func (s *RowSlab) AllocRaw() (int, error) {
	if s.allocated >= s.cap {
		return 0, ErrSlabOverflow
	}
	idx := s.allocated
	s.allocated++
	return idx, nil
}

// SetRow stores row r at index idx. Panics if idx is out of bounds.
// Used with variable-width slabs after [AllocRaw].
func (s *RowSlab) SetRow(idx int, r Row) {
	s.rows[idx] = r
}

// GetRow returns the row at index idx. Panics if idx is out of bounds.
func (s *RowSlab) GetRow(idx int) Row {
	return s.rows[idx]
}

// Len returns the number of rows currently allocated.
func (s *RowSlab) Len() int { return s.allocated }

// Cap returns the maximum number of rows this slab can hold.
func (s *RowSlab) Cap() int { return s.cap }

// Reset resets the slab for reuse. It zeroes every value slot in each
// allocated row to release references held by the GC, then resets the
// allocation counter to zero. The backing memory is retained.
func (s *RowSlab) Reset() {
	for i := range s.allocated {
		row := s.rows[i]
		for j := range row {
			row[j] = nil
		}
	}
	s.allocated = 0
}

// ─────────────────────────────────────────────────────────────────────────────
// SlabPool
// ─────────────────────────────────────────────────────────────────────────────

// SlabPool is a [sync.Pool]-backed pool of [RowSlab] instances with a fixed
// column width and capacity. Operators that process a high volume of rows
// should obtain slabs from a shared pool to reduce GC pressure.
//
// SlabPool is safe for concurrent use.
type SlabPool struct {
	p sync.Pool
}

// NewSlabPool creates a SlabPool that vends RowSlabs with the given column
// width and row capacity.
func NewSlabPool(width, capacity int) *SlabPool {
	sp := &SlabPool{}
	sp.p = sync.Pool{
		New: func() any {
			return NewRowSlab(width, capacity)
		},
	}
	return sp
}

// Get retrieves a reset RowSlab from the pool, or allocates a new one.
func (sp *SlabPool) Get() *RowSlab {
	metrics.IncCounter("cypher.pool.slab.get", 1)
	return sp.p.Get().(*RowSlab) //nolint:forcetypeassert // pool invariant: New always returns *RowSlab
}

// Put resets s and returns it to the pool.
func (sp *SlabPool) Put(s *RowSlab) {
	metrics.IncCounter("cypher.pool.slab.put", 1)
	s.Reset()
	sp.p.Put(s)
}

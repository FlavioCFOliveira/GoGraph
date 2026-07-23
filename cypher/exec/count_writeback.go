package exec

import "github.com/FlavioCFOliveira/GoGraph/graph/index/count"

// CountBuffer collects relationship count-store deltas and dirty markings
// produced by write operators during a single write transaction. It is the
// structural twin of [IndexBuffer] (design docs/count-store-design.md §3.1).
//
// Call EnqueueDelta / MarkDirty for every count-affecting graph mutation. At the
// transaction boundary:
//   - Commit: applies the accumulated deltas then the dirty markings to the
//     count store, then resets.
//   - Rollback: discards everything without touching the store — no undo log is
//     needed because the store is a pure function of graph state and nothing was
//     applied yet.
//
// Both backing slices grow lazily on first use, so a transaction that touches no
// count cell (for example a bare CREATE (:N) over an edgeless graph) allocates
// nothing here. CountBuffer is NOT safe for concurrent use.
type CountBuffer struct {
	deltas []count.Delta
	dirty  []count.DirtyMark
}

// EnqueueDelta appends a cell increment to the buffer.
func (b *CountBuffer) EnqueueDelta(d count.Delta) { b.deltas = append(b.deltas, d) }

// MarkDirty appends an X-scoped dirty marking to the buffer.
func (b *CountBuffer) MarkDirty(m count.DirtyMark) { b.dirty = append(b.dirty, m) }

// Commit applies all buffered deltas and then all buffered dirty markings to cs,
// then resets the buffer. Deltas precede dirty markings so a family the deltas
// just updated exactly is still marked non-exact when the same commit also trips
// the budget on it. A nil cs is safe: the buffer is discarded without panicking.
//
// Commit must run inside the write visibility barrier (visMu.Lock, after the WAL
// fsync succeeds) so the count update becomes visible atomically with the graph
// writes it describes — the durable-then-visible seam the secondary-index
// fan-out uses.
func (b *CountBuffer) Commit(cs *count.Store) {
	if cs != nil {
		for _, d := range b.deltas {
			cs.Apply(d)
		}
		for _, m := range b.dirty {
			cs.MarkDirty(m)
		}
	}
	b.deltas = b.deltas[:0]
	b.dirty = b.dirty[:0]
}

// Rollback discards all buffered deltas and dirty markings without applying them.
func (b *CountBuffer) Rollback() {
	b.deltas = b.deltas[:0]
	b.dirty = b.dirty[:0]
}

// Len returns the number of deltas plus dirty markings currently buffered.
func (b *CountBuffer) Len() int { return len(b.deltas) + len(b.dirty) }

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
//
// # What that requirement is, and what it is NOT (rmp #2303, MVCC B1)
//
// It is a VISIBILITY requirement, not an ORDERING one, and the distinction is what
// lets rmp #2304 remove the barrier without redesigning this. A reader must not
// observe a count that disagrees with the graph it can see; that is atomicity
// between two structures and it needs whatever mechanism replaces the barrier to
// publish both together.
//
// It does NOT rest on the barrier imposing a total order across committers. The
// count store's own ordering basis is COMMUTATIVITY: [count.Store.Apply] is an
// additive delta, and [count.Store.MarkDirty] is a monotone set insert, so any
// interleaving of two transactions' buffers reaches the same state a serial
// schedule would.
//
// That was not free, and it was not true when the claim was first made: Apply
// deleted a cell at zero-or-below, which discarded a transiently-negative cell and
// lost the decrement that produced it, making the aggregate order-sensitive. It
// now deletes at exactly zero. See graph/index/count/commutative_test.go for the
// measurement and the differential.
//
// The intra-transaction order — every delta, THEN every dirty marking — is a
// property of this buffer and survives concurrency unchanged, because one buffer
// belongs to one transaction.
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

// NumDeltas returns the number of buffered cell increments (excluding dirty
// markings). The commit fan-out reads it before [CountBuffer.Commit] resets the
// buffer, to attribute a "deltas applied" observability count to the commit
// (task #2087). It is zero for a transaction that touched no count cell, so the
// bare-CREATE write path emits nothing.
func (b *CountBuffer) NumDeltas() int { return len(b.deltas) }

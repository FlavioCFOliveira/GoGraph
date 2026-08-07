package exec

import "github.com/FlavioCFOliveira/GoGraph/graph/index"

// IndexBuffer collects index.Change events produced by write operators
// during a single write transaction.
//
// Call Enqueue for every graph mutation. At the transaction boundary:
//   - Commit: fans changes to index.Manager.ApplyBatch then resets.
//   - Rollback: discards changes without touching indexes.
//
// IndexBuffer is NOT safe for concurrent use.
type IndexBuffer struct {
	changes []index.Change
	// inline is the array changes starts out on, so a statement that enqueues a
	// couple of changes allocates no slice at all.
	//
	// Measured (rmp #2339, -memprofilerate=1 over 200 000 commits of
	// BenchmarkWriteScaling/mem/writers=1): one `CREATE (n:Account {id: $id})`
	// enqueues exactly two changes — the label and the property — and growing
	// the nil slice through capacities 1 and 2 cost TWO mallocs and 192 B on
	// every commit. index.Change is 64 B, so two entries add 128 B to a buffer
	// that is itself part of the statement's mutator adapter, and the trade is
	// two fewer objects for 64 B less traffic. Past two, append spills to the
	// heap exactly as before.
	inline [2]index.Change
}

// Enqueue appends c to the buffer.
func (b *IndexBuffer) Enqueue(c index.Change) {
	// Start on the inline array rather than on nil. Done here rather than at
	// construction because the zero IndexBuffer must stay usable: the engine
	// builds one with &IndexBuffer{} and rides it inside the adapter, and
	// neither path runs an initialiser.
	if b.changes == nil {
		b.changes = b.inline[:0]
	}
	b.changes = append(b.changes, c)
}

// Commit applies all buffered changes to mgr via ApplyBatch, then resets
// the buffer. A nil mgr is safe: changes are discarded without panicking.
func (b *IndexBuffer) Commit(mgr *index.Manager) {
	if mgr != nil && len(b.changes) > 0 {
		mgr.ApplyBatch(b.changes)
	}
	b.reset()
}

// Rollback discards all buffered changes without applying them.
func (b *IndexBuffer) Rollback() {
	b.reset()
}

// reset truncates the buffer and releases what the drained changes referenced.
// The zeroing is not cosmetic: index.Change carries the old and new property
// values as `any`, and since rmp #2339 the backing array is usually `inline`,
// part of the buffer itself — so truncating the header alone would pin every
// drained value for as long as the buffer lives.
func (b *IndexBuffer) reset() {
	clear(b.changes)
	b.changes = b.changes[:0]
}

// Len returns the number of changes currently buffered.
func (b *IndexBuffer) Len() int { return len(b.changes) }

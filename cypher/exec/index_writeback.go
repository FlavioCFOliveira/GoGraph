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
}

// Enqueue appends c to the buffer.
func (b *IndexBuffer) Enqueue(c index.Change) {
	b.changes = append(b.changes, c)
}

// Commit applies all buffered changes to mgr via ApplyBatch, then resets
// the buffer. A nil mgr is safe: changes are discarded without panicking.
func (b *IndexBuffer) Commit(mgr *index.Manager) {
	if mgr != nil && len(b.changes) > 0 {
		mgr.ApplyBatch(b.changes)
	}
	b.changes = b.changes[:0]
}

// Rollback discards all buffered changes without applying them.
func (b *IndexBuffer) Rollback() {
	b.changes = b.changes[:0]
}

// Len returns the number of changes currently buffered.
func (b *IndexBuffer) Len() int { return len(b.changes) }

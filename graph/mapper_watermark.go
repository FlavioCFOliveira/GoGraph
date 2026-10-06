package graph

// MapperWatermark records how many keys each shard of a [Mapper] had interned
// when it was taken. Interning is append-only within a shard, so the ids a
// shard assigned before the watermark are exactly those whose intra-shard
// index is below the recorded count: [MapperWatermark.Covers] answers "was this
// id interned when the watermark was taken" exactly, from the id alone.
//
// It exists for an MVCC snapshot capture (rmp #2991). A capture must carry
// every id interned at its instant and no id interned later, and the per-id
// birth records cannot answer that once an aborted creation has been withdrawn:
// the withdrawn id then looks as old as a node whose record was reclaimed. The
// watermark taken at the instant answers it without consulting any record.
//
// Concurrency: a MapperWatermark is immutable once returned and safe for
// concurrent reads.
type MapperWatermark struct {
	n [mapperShardCount]uint64
}

// Watermark returns the number of keys each shard has interned, read shard by
// shard under each shard's read lock. Every per-shard count is exact for the
// moment its shard was read; the counts are mutually consistent only when no
// key is interned concurrently, which is the caller's precondition (for a
// checkpoint capture, the commit serialiser's drain provides it).
//
// It allocates one fixed-size value (256 counts) and is O(shards).
//
// Safe for concurrent use.
func (m *Mapper[N]) Watermark() *MapperWatermark {
	w := &MapperWatermark{}
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		w.n[i] = uint64(len(s.reverse))
		s.mu.RUnlock()
	}
	return w
}

// Covers reports whether id had been interned when w was taken: its
// intra-shard index is below the count w recorded for its shard.
//
// Safe for concurrent use.
func (w *MapperWatermark) Covers(id NodeID) bool {
	shard, idx := unpackNodeID(id)
	return idx < w.n[shard]
}

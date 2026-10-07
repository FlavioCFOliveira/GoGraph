package graph

// MapperWatermark records each shard's high-water mark — the intra index its next
// new key would receive — when it was taken. Below it lie every id the shard has
// assigned and its holes (ids reserved and never born, WAL v2 step 1). Interning is append-only within a shard, so the ids a
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

// Watermark returns each shard's high-water mark, read shard by shard under each
// shard's read lock. Every per-shard mark is exact for the moment its shard was
// read; marks of different shards may straddle a concurrent intern. A capture
// reads it just after its instant, so every id assigned at the instant is covered
// (lpg.Graph.BeginCaptureRead).
//
// It allocates one fixed-size value (256 counts) and is O(shards).
//
// Safe for concurrent use.
func (m *Mapper[N]) Watermark() *MapperWatermark {
	w := &MapperWatermark{}
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		w.n[i] = max(s.next, uint64(len(s.reverse)))
		s.mu.RUnlock()
	}
	return w
}

// Covers reports whether id had been assigned when w was taken: its
// intra-shard index is below the high-water mark w recorded for its shard. It
// is true for a hole, which is harmless: a hole names no key, so no walk
// reaches it.
//
// Safe for concurrent use.
func (w *MapperWatermark) Covers(id NodeID) bool {
	shard, idx := unpackNodeID(id)
	return idx < w.n[shard]
}

// Next returns the per-shard high-water marks, indexed by shard, for a
// snapshot's nodeids.bin.
func (w *MapperWatermark) Next() [MapperShards]uint64 { return w.n }

// MapperShards is the number of shards of every [Mapper], as a constant for
// fixed-size per-shard arrays.
const MapperShards = mapperShardCount

package graph

import (
	"fmt"
	"testing"
)

// TestMapperWatermark_CoversExactlyThePrefix pins [MapperWatermark.Covers]: every id
// interned before the watermark is covered and every id interned after it is not,
// in every shard. The fixture interns more keys than there are shards so several
// shards hold more than one id on each side of the watermark.
func TestMapperWatermark_CoversExactlyThePrefix(t *testing.T) {
	t.Parallel()
	const before, after = 3 * mapperShardCount, 2 * mapperShardCount
	m := NewMapper[string]()
	old := make([]NodeID, 0, before)
	for i := range before {
		old = append(old, m.Intern(fmt.Sprintf("before-%05d", i)))
	}
	w := m.Watermark()
	newer := make([]NodeID, 0, after)
	for i := range after {
		newer = append(newer, m.Intern(fmt.Sprintf("after-%05d", i)))
	}
	for _, id := range old {
		if !w.Covers(id) {
			t.Fatalf("id %d interned before the watermark is not covered", uint64(id))
		}
	}
	for _, id := range newer {
		if w.Covers(id) {
			t.Fatalf("id %d interned after the watermark is covered", uint64(id))
		}
	}
	// Re-interning an existing key returns its old id and does not move the
	// watermark's answer.
	if id := m.Intern("before-00000"); !w.Covers(id) {
		t.Fatalf("re-interned key resolved to an uncovered id %d", uint64(id))
	}
}

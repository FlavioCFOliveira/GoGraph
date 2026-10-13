package graph

// mapper_unplace_test.go — rmp #3119: [Mapper.Unplace] gives back the memory a
// released binding held, never lowers a high-water mark, and leaves every read
// path consistent.
//
// Layer: short.

import (
	"fmt"
	"testing"
)

// TestMapperUnplace_ReleasesMemoryAndKeepsIDs releases every key, then interns
// new ones: the reverse tables and forward maps give their memory back, the
// high-water marks do not move, and new keys take ids above every id issued
// before.
func TestMapperUnplace_ReleasesMemoryAndKeepsIDs(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	const n = 80000
	ids := make([]NodeID, n)
	for i := range ids {
		ids[i] = m.Intern(fmt.Sprintf("k%d", i))
	}
	wm := m.Watermark()
	for i, id := range ids {
		if !m.Unplace(fmt.Sprintf("k%d", i), id) {
			t.Fatalf("Unplace(k%d, %d) = false", i, id)
		}
	}
	for i := range m.shards {
		s := &m.shards[i]
		if len(s.reverse) != 0 || cap(s.reverse) > 64 || s.holes != nil || s.holeCount != 0 {
			t.Fatalf("shard %d kept slots: len=%d cap=%d holes=%v holeCount=%d", i, len(s.reverse), cap(s.reverse), s.holes != nil, s.holeCount)
		}
		if len(s.forward) != 0 || s.forwardDeletes >= 64 {
			t.Fatalf("shard %d: forward=%d forwardDeletes=%d, want the map rebuilt", i, len(s.forward), s.forwardDeletes)
		}
		if s.next != wm.n[i] {
			t.Fatalf("shard %d next = %d, want the untouched high-water mark %d", i, s.next, wm.n[i])
		}
	}
	if m.Len() != 0 || m.MaxNodeID() != 0 || m.Watermark().Next() != wm.Next() {
		t.Fatalf("Len=%d MaxNodeID=%d watermark moved=%v", m.Len(), m.MaxNodeID(), m.Watermark().Next() != wm.Next())
	}
	for i := 0; i < 1000; i++ {
		id := m.Intern(fmt.Sprintf("new%d", i))
		if wm.Covers(id) {
			t.Fatalf("new key got id %d below the old high-water mark", id)
		}
		if k, ok := m.Resolve(id); !ok || k != fmt.Sprintf("new%d", i) {
			t.Fatalf("Resolve(%d) = (%q,%v)", id, k, ok)
		}
	}
	if m.Len() != 1000 {
		t.Fatalf("Len = %d, want 1000", m.Len())
	}
}

// TestMapperUnplace_ReadPathsAfterRelease releases a run of keys in the middle
// of every shard and checks that Resolve, Walk and AppendIDs see exactly the
// keys left, in the same order, and that interior holes keep MaxNodeID.
func TestMapperUnplace_ReadPathsAfterRelease(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 64 * mapperShardCount
	ids := make([]NodeID, n)
	for i := range ids {
		ids[i] = m.Intern(i)
	}
	maxID := m.MaxNodeID()
	kept := map[NodeID]int{}
	for i, id := range ids {
		if _, idx := unpackNodeID(id); idx >= 8 && idx < 24 {
			if !m.Unplace(i, id) {
				t.Fatalf("Unplace(%d) = false", i)
			}
			continue
		}
		kept[id] = i
	}
	if m.MaxNodeID() != maxID {
		t.Fatalf("MaxNodeID = %d after interior releases, want %d", m.MaxNodeID(), maxID)
	}
	for i, id := range ids {
		k, ok := m.Resolve(id)
		if want, isKept := kept[id]; isKept != ok || (ok && k != want) {
			t.Fatalf("Resolve(%d) for key %d = (%d,%v), kept=%v", id, i, k, ok, isKept)
		}
	}
	var walked []NodeID
	m.Walk(func(id NodeID, k int) bool {
		if kept[id] != k {
			t.Fatalf("Walk yielded (%d,%d), want key %d", id, k, kept[id])
		}
		walked = append(walked, id)
		return true
	})
	var paged []NodeID
	var cur MapperCursor
	for !cur.Done() {
		paged, cur = m.AppendIDs(paged, cur, 97)
	}
	if len(walked) != len(kept) || len(paged) != len(walked) {
		t.Fatalf("Walk %d ids, AppendIDs %d ids, want %d", len(walked), len(paged), len(kept))
	}
	for i := range walked {
		if walked[i] != paged[i] {
			t.Fatalf("Walk and AppendIDs disagree at %d: %d vs %d", i, walked[i], paged[i])
		}
	}
}

// TestMapperUnplace_PlaceUnbornAfterTrim puts a binding back into a slot that a
// release trimmed off the end of its shard, as recovery and the abort release's
// fallback do.
func TestMapperUnplace_PlaceUnbornAfterTrim(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	id := m.Intern("a")
	if !m.Unplace("a", id) {
		t.Fatal("Unplace = false")
	}
	if placed, err := m.PlaceUnborn("a", id); err != nil || !placed {
		t.Fatalf("PlaceUnborn = (%v,%v), want (true,nil)", placed, err)
	}
	if k, ok := m.Resolve(id); !ok || k != "a" {
		t.Fatalf("Resolve(%d) = (%q,%v), want (\"a\",true)", id, k, ok)
	}
	if m.Len() != 1 {
		t.Fatalf("Len = %d, want 1", m.Len())
	}
}

// TestMapperUnplace_ReadPathsOverRefilledEnd releases every key, interns new
// ones far past the old end, releases some of those, and checks that Resolve,
// Walk and AppendIDs agree on exactly the keys left (rmp #3121).
func TestMapperUnplace_ReadPathsOverRefilledEnd(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 600 * mapperShardCount
	for i := 0; i < n; i++ {
		m.Unplace(i, m.Intern(i))
	}
	kept := map[NodeID]int{}
	for i := n; i < n+3000; i++ {
		id := m.Intern(i)
		if i%3 == 0 {
			if !m.Unplace(i, id) {
				t.Fatalf("Unplace(%d) = false", i)
			}
			continue
		}
		kept[id] = i
	}
	if m.Len() != len(kept) {
		t.Fatalf("Len = %d, want %d", m.Len(), len(kept))
	}
	for id, k := range kept {
		if got, ok := m.Resolve(id); !ok || got != k {
			t.Fatalf("Resolve(%d) = (%d,%v), want (%d,true)", id, got, ok, k)
		}
	}
	var walked []NodeID
	m.Walk(func(id NodeID, k int) bool {
		if kept[id] != k {
			t.Fatalf("Walk yielded (%d,%d), want key %d", id, k, kept[id])
		}
		walked = append(walked, id)
		return true
	})
	var paged []NodeID
	var cur MapperCursor
	for !cur.Done() {
		paged, cur = m.AppendIDs(paged, cur, 97)
	}
	if len(walked) != len(kept) || len(paged) != len(walked) {
		t.Fatalf("Walk %d ids, AppendIDs %d ids, want %d", len(walked), len(paged), len(kept))
	}
	for i := range walked {
		if walked[i] != paged[i] {
			t.Fatalf("Walk and AppendIDs disagree at %d: %d vs %d", i, walked[i], paged[i])
		}
	}
}

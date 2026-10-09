package graph

// mapper_holes_test.go — WAL v2 step 1 (docs/design-wal-v2.md §3.1): a restored
// mapper may carry HOLES, ids below a shard's high-water mark that name no key.
//
// Layer: short.

import (
	"errors"
	"fmt"
	"testing"
)

// sameShardStrings returns n distinct keys that hash to one mapper shard.
func sameShardStrings(t *testing.T, n int) (uint64, []string) {
	t.Helper()
	byShard := map[uint64][]string{}
	for i := 0; i < 1_000_000; i++ {
		k := fmt.Sprintf("hole-%07d", i)
		s := mapperShardFor(k)
		byShard[s] = append(byShard[s], k)
		if len(byShard[s]) == n {
			return s, byShard[s]
		}
	}
	t.Fatalf("no shard received %d probe keys", n)
	return 0, nil
}

// TestMapper_LoadFrom_GapsBecomeHoles restores entries at intra 0 and 3 of one
// shard with no high-water marks: 1 and 2 are holes, every accessor skips them,
// and the next key takes intra 4.
func TestMapper_LoadFrom_GapsBecomeHoles(t *testing.T) {
	t.Parallel()
	shard, keys := sameShardStrings(t, 3)
	m := NewMapper[string]()
	a, b := packNodeID(shard, 0), packNodeID(shard, 3)
	if err := m.LoadFrom([]MapperEntry[string]{{ID: b, Key: keys[1]}, {ID: a, Key: keys[0]}}, nil); err != nil {
		t.Fatalf("LoadFrom with a gap: %v", err)
	}
	for _, idx := range []uint64{1, 2} {
		if k, ok := m.Resolve(packNodeID(shard, idx)); ok {
			t.Errorf("Resolve(hole intra %d) = %q, true; want false", idx, k)
		}
	}
	if k, ok := m.Resolve(b); !ok || k != keys[1] {
		t.Errorf("Resolve(intra 3) = %q, %v; want %q", k, ok, keys[1])
	}
	var walked []NodeID
	m.Walk(func(id NodeID, _ string) bool { walked = append(walked, id); return true })
	if len(walked) != 2 || walked[0] != a || walked[1] != b {
		t.Errorf("Walk = %v, want [%d %d]", walked, a, b)
	}
	if got := m.Len(); got != 2 {
		t.Errorf("Len = %d, want 2 (holes excluded)", got)
	}
	if got, want := m.MaxNodeID(), packNodeID(mapperShardCount-1, 3)+1; got != want {
		t.Errorf("MaxNodeID = %d, want %d (sized by the highest index, holes included)", got, want)
	}
	if !m.Watermark().Covers(packNodeID(shard, 2)) || m.Watermark().Covers(packNodeID(shard, 4)) {
		t.Error("Watermark must cover intra 0..3 (holes included) and not 4")
	}
	if id := m.Intern(keys[2]); id != packNodeID(shard, 4) {
		t.Errorf("next key interned at %d, want intra 4 (%d)", id, packNodeID(shard, 4))
	}
	if id := m.Intern(keys[0]); id != a {
		t.Errorf("restored key re-interned at %d, want %d", id, a)
	}
}

// TestMapper_LoadFrom_NextAboveLen pins the high-water marks: a shard whose next
// is above its last restored index assigns the next key there, and every index
// in between is a hole; a shard with no entries still honours its next.
func TestMapper_LoadFrom_NextAboveLen(t *testing.T) {
	t.Parallel()
	shard, keys := sameShardStrings(t, 2)
	other := (shard + 1) % mapperShardCount
	var next [MapperShards]uint64
	next[shard], next[other] = 10, 7
	m := NewMapper[string]()
	if err := m.LoadFrom([]MapperEntry[string]{{ID: packNodeID(shard, 0), Key: keys[0]}}, &next); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := m.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
	if wm := m.Watermark(); !wm.Covers(packNodeID(shard, 9)) || wm.Covers(packNodeID(shard, 10)) ||
		!wm.Covers(packNodeID(other, 6)) || wm.Covers(packNodeID(other, 7)) {
		t.Error("Watermark must equal the restored high-water marks")
	}
	if id := m.Intern(keys[1]); id != packNodeID(shard, 10) {
		t.Fatalf("new key at %d, want intra 10 (%d)", id, packNodeID(shard, 10))
	}
	for idx := uint64(1); idx < 10; idx++ {
		if _, ok := m.Resolve(packNodeID(shard, idx)); ok {
			t.Errorf("intra %d resolved; want a hole", idx)
		}
	}
	if got := m.Len(); got != 2 {
		t.Errorf("Len after intern = %d, want 2", got)
	}
}

// TestMapper_LoadFrom_RejectsIDAtOrAboveNext pins the one new refusal: an entry at
// or above its shard's recorded high-water mark is corruption.
func TestMapper_LoadFrom_RejectsIDAtOrAboveNext(t *testing.T) {
	t.Parallel()
	shard, keys := sameShardStrings(t, 1)
	for _, idx := range []uint64{2, 3} {
		var next [MapperShards]uint64
		next[shard] = 2
		err := NewMapper[string]().LoadFrom([]MapperEntry[string]{{ID: packNodeID(shard, idx), Key: keys[0]}}, &next)
		if !errors.Is(err, ErrMapperEntryCorrupted) {
			t.Errorf("LoadFrom(intra %d, next 2) = %v, want ErrMapperEntryCorrupted", idx, err)
		}
	}
}

// TestMapper_LoadFrom_RejectsDuplicateIntra pins that two keys on one id are
// refused now that gaps no longer catch it.
func TestMapper_LoadFrom_RejectsDuplicateIntra(t *testing.T) {
	t.Parallel()
	shard, keys := sameShardStrings(t, 2)
	id := packNodeID(shard, 1)
	err := NewMapper[string]().LoadFrom([]MapperEntry[string]{{ID: id, Key: keys[0]}, {ID: id, Key: keys[1]}}, nil)
	if !errors.Is(err, ErrMapperEntryCorrupted) {
		t.Fatalf("LoadFrom(two keys on one id) = %v, want ErrMapperEntryCorrupted", err)
	}
}

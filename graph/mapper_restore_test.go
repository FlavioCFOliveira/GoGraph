package graph

import (
	"errors"
	"testing"
)

// TestMapper_LoadFrom_Roundtrip seeds a fresh mapper from a slice of
// (NodeID, key) pairs produced by Walk over an originally-interned
// mapper. After LoadFrom the seeded mapper must answer Lookup for
// every original key with the original NodeID, and subsequent Intern
// calls must hit the cache without allocating a new slot.
func TestMapper_LoadFrom_Roundtrip(t *testing.T) {
	t.Parallel()
	src := NewMapper[string]()
	keys := []string{"alice", "bob", "carol", "dave", "eve", "frank", "grace"}
	want := make(map[string]NodeID, len(keys))
	for _, k := range keys {
		want[k] = src.Intern(k)
	}

	entries := make([]MapperEntry[string], 0, src.Len())
	src.Walk(func(id NodeID, k string) bool {
		entries = append(entries, MapperEntry[string]{ID: id, Key: k})
		return true
	})

	dst := NewMapper[string]()
	if err := dst.LoadFrom(entries); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := dst.Len(); got != src.Len() {
		t.Fatalf("Len = %d, want %d", got, src.Len())
	}
	for k, expectedID := range want {
		got, ok := dst.Lookup(k)
		if !ok {
			t.Fatalf("Lookup(%q) not found", k)
		}
		if got != expectedID {
			t.Errorf("Lookup(%q) = %d, want %d", k, got, expectedID)
		}
		// Intern must return the same id (cache hit).
		if id := dst.Intern(k); id != expectedID {
			t.Errorf("Intern(%q) after LoadFrom = %d, want %d", k, id, expectedID)
		}
	}
	// A brand-new key must land in the first free slot of its shard;
	// because every shard's reverse slice is exactly its original
	// length, the new key's intra-index equals len(reverse) before
	// the call.
	newID := dst.Intern("zeta")
	if _, ok := dst.Lookup("zeta"); !ok {
		t.Fatal("freshly interned key not found")
	}
	resolved, ok := dst.Resolve(newID)
	if !ok || resolved != "zeta" {
		t.Errorf("Resolve(new) = (%q, %v), want (zeta, true)", resolved, ok)
	}
}

// TestMapper_LoadFrom_RejectsNonEmptyMapper asserts the safety
// pre-condition: LoadFrom on a mapper that already has interned values
// returns ErrMapperNotEmpty without mutating any shard.
func TestMapper_LoadFrom_RejectsNonEmptyMapper(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	originalID := m.Intern("preexisting")
	err := m.LoadFrom([]MapperEntry[string]{{ID: 0, Key: "other"}})
	if !errors.Is(err, ErrMapperNotEmpty) {
		t.Fatalf("LoadFrom on non-empty mapper = %v, want ErrMapperNotEmpty", err)
	}
	// Pre-existing key must still resolve to its original id.
	if got, ok := m.Lookup("preexisting"); !ok || got != originalID {
		t.Errorf("Lookup after rejected LoadFrom = (%d,%v), want (%d,true)", got, ok, originalID)
	}
}

// TestMapper_LoadFrom_RejectsShardMismatch corrupts an entry by
// flipping its NodeID's low byte so the encoded shard index no longer
// agrees with mapperShardFor(key). LoadFrom must detect the
// disagreement and reject the input.
func TestMapper_LoadFrom_RejectsShardMismatch(t *testing.T) {
	t.Parallel()
	src := NewMapper[string]()
	id := src.Intern("alice")
	// Flip the low bit of the shard byte. mapperShardCount is 256, so
	// shard occupies the low 8 bits; XOR with 1 changes shard but not
	// the intra-index, breaking the writer's invariant deliberately.
	corrupted := MapperEntry[string]{ID: id ^ 1, Key: "alice"}

	dst := NewMapper[string]()
	err := dst.LoadFrom([]MapperEntry[string]{corrupted})
	if !errors.Is(err, ErrMapperEntryCorrupted) {
		t.Fatalf("LoadFrom(shard-mismatch) = %v, want ErrMapperEntryCorrupted", err)
	}
}

// TestMapper_LoadFrom_RejectsIntraGap fakes a contiguous-slot
// violation by handing LoadFrom a single entry whose intra-index is
// 2 instead of 0. LoadFrom must reject rather than silently leave
// holes in the reverse slice.
func TestMapper_LoadFrom_RejectsIntraGap(t *testing.T) {
	t.Parallel()
	// Build a key whose hash maps to shard 0, then synthesise an
	// entry at intra-index 2 (gap at 0 and 1).
	shard := mapperShardFor("alice")
	id := packNodeID(shard, 2)
	entry := MapperEntry[string]{ID: id, Key: "alice"}

	dst := NewMapper[string]()
	err := dst.LoadFrom([]MapperEntry[string]{entry})
	if !errors.Is(err, ErrMapperEntryCorrupted) {
		t.Fatalf("LoadFrom(intra-gap) = %v, want ErrMapperEntryCorrupted", err)
	}
}

// TestMapper_LoadFrom_Empty leaves a fresh mapper completely
// untouched: LoadFrom with no entries is a no-op and the post-state
// must remain indistinguishable from a brand-new NewMapper.
func TestMapper_LoadFrom_Empty(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	if err := m.LoadFrom(nil); err != nil {
		t.Fatalf("LoadFrom(nil): %v", err)
	}
	if got := m.Len(); got != 0 {
		t.Errorf("Len = %d, want 0 after LoadFrom(nil)", got)
	}
	// First Intern on the empty mapper still works.
	id := m.Intern("alice")
	if got, ok := m.Lookup("alice"); !ok || got != id {
		t.Errorf("Lookup after empty LoadFrom = (%d,%v), want (%d,true)", got, ok, id)
	}
}

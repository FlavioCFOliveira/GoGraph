package btree

// entry_arena_test.go — the regression gate for the slab-allocated per-key
// payload (task #2684).
//
// The arena exists to cut GC mark cost by cutting the OBJECT count, and it is
// allowed to do that only because it leaves the one property every correctness
// argument in bplus.go rests on untouched: a key's entry has a stable, unique
// address that a path copy duplicates rather than reallocates. An arena bug
// that handed one slot to two keys would silently merge their node-sets — an
// ACID Consistency violation that no existing test targets directly, because
// before this change one key could not possibly share an object with another.
//
// These tests fail on a broken arena and pass on a correct one; they say
// nothing about performance, which bench/entryheap measures.

import (
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestEntrySlabFitsBelowMallocHeaderCutover states the size bound in a form a
// human reads, alongside the constant expression in bplus.go that enforces it
// at compile time. An object holding pointers that exceeds 512 bytes gains an
// 8-byte malloc header and rounds up a size class, which was measured at +30%
// GC mark CPU and +4% resident bytes — the exact opposite of the arena's
// purpose.
func TestEntrySlabFitsBelowMallocHeaderCutover(t *testing.T) {
	const cutover = 512
	got := entrySlabLen * int(unsafe.Sizeof(entry{}))
	if got > cutover {
		t.Fatalf("slab is %d bytes (entrySlabLen=%d x entry=%d); must stay <= %d, see entrySlabLen",
			got, entrySlabLen, unsafe.Sizeof(entry{}), cutover)
	}
}

// TestEntryArenaAllocIsDistinctAndZeroed proves the two properties alloc's
// callers rely on: every call yields an address no previous call yielded, and
// the entry it points at is the zero value — a live, empty, undead entry.
func TestEntryArenaAllocIsDistinctAndZeroed(t *testing.T) {
	var a entryArena
	const n = 4*entrySlabLen + 3 // spans several slabs and ends mid-slab
	seen := make(map[*entry]int, n)
	for i := range n {
		e := a.alloc()
		if prev, dup := seen[e]; dup {
			t.Fatalf("alloc %d returned the same address as alloc %d: %p", i, prev, e)
		}
		seen[e] = i
		if e.dead {
			t.Fatalf("alloc %d returned a dead entry", i)
		}
		if !e.set.IsEmpty() {
			t.Fatalf("alloc %d returned a non-empty entry", i)
		}
		// Write through it, so a later alloc that aliased this slot would be
		// caught by the emptiness check above rather than only by the map.
		e.set.Add(uint64(i)) // G115: i is a bounded non-negative loop index
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct entries, want %d", len(seen), n)
	}
}

// TestEntryPointersAreDistinctPerKey walks the whole tree and proves no two
// keys address the same payload, on BOTH population paths. This is the
// property that a slab allocator could plausibly break and that a functional
// test would not notice until two unrelated keys started returning each
// other's nodes.
func TestEntryPointersAreDistinctPerKey(t *testing.T) {
	// Enough keys to build a multi-level tree and to exhaust many slabs.
	const keys = 5000

	t.Run("insert", func(t *testing.T) {
		ix := New[int64]()
		for i := range keys {
			ix.Insert(int64(i), graph.NodeID(uint64(i))) // G115: bounded non-negative
		}
		assertDistinctEntries(t, ix, keys)
	})

	t.Run("bulkload", func(t *testing.T) {
		values := make([]int64, keys)
		nodes := make([]graph.NodeID, keys)
		for i := range values {
			values[i] = int64(i)
			nodes[i] = graph.NodeID(uint64(i)) // G115: bounded non-negative
		}
		ix := New[int64]()
		if err := ix.BulkLoadSorted(values, nodes); err != nil {
			t.Fatalf("BulkLoadSorted: %v", err)
		}
		assertDistinctEntries(t, ix, keys)
	})
}

// assertDistinctEntries checks that ix maps each of the keys [0, keys) to its
// own entry, and that each entry still holds exactly the node it was given.
func assertDistinctEntries(t *testing.T, ix *Index[int64], keys int) {
	t.Helper()
	tree := ix.tree.Load()
	seen := make(map[*entry]int64, keys)
	for i := range keys {
		k := int64(i)
		e := tree.get(k)
		if e == nil {
			t.Fatalf("key %d missing from the tree", k)
		}
		if prev, dup := seen[e]; dup {
			t.Fatalf("keys %d and %d share entry %p", prev, k, e)
		}
		seen[e] = k
		if got := ix.Cardinality(k); got != 1 {
			t.Fatalf("key %d has cardinality %d, want 1", k, got)
		}
		if bm := ix.Lookup(k); bm == nil || !bm.Contains(uint64(i)) { // G115: bounded non-negative
			t.Fatalf("key %d does not hold node %d", k, i)
		}
	}
}

// TestEntryIdentitySurvivesPathCopy is the load-bearing property the arena must
// not disturb: a structural write copies the root-to-leaf path but must leave
// every OTHER key addressing the very same payload object, so a snapshot and
// its predecessor share one lock and one node-set per key. Slab allocation
// changes where an entry comes from; it must not change that an entry stays
// put.
func TestEntryIdentitySurvivesPathCopy(t *testing.T) {
	ix := New[int64]()
	// Seed a handful of keys and record their payload addresses.
	const seeded = 40
	before := make(map[int64]*entry, seeded)
	for i := range seeded {
		k := int64(i * 1000)
		ix.Insert(k, graph.NodeID(uint64(i))) // G115: bounded non-negative
		before[k] = ix.tree.Load().get(k)
	}
	// Force many splits and several levels of path copying around them.
	for i := range 20000 {
		ix.Insert(int64(i), graph.NodeID(uint64(1_000_000+i))) // G115: bounded non-negative
	}
	for k, want := range before {
		got := ix.tree.Load().get(k)
		if got != want {
			t.Fatalf("key %d moved payload across path copies: had %p, now %p", k, want, got)
		}
	}
}

// TestDetachedEntrySlotIsNeverHandedToANewKey guards the rule that makes the
// arena safe at all: slots are never reused. A detached entry can still be
// reachable from an older snapshot, so re-issuing its slot would resurrect it
// under a different key. Deleting every key and inserting fresh ones must
// therefore produce payload addresses disjoint from the dead ones.
func TestDetachedEntrySlotIsNeverHandedToANewKey(t *testing.T) {
	ix := New[int64]()
	const n = 3 * entrySlabLen
	dead := make(map[*entry]bool, n)
	for i := range n {
		k := int64(i)
		ix.Insert(k, graph.NodeID(uint64(i))) // G115: bounded non-negative
		dead[ix.tree.Load().get(k)] = true
	}
	for i := range n {
		ix.Delete(int64(i), graph.NodeID(uint64(i))) // G115: bounded non-negative
	}
	if got := ix.DistinctValues(); got != 0 {
		t.Fatalf("index still holds %d keys after deleting all of them", got)
	}
	for i := range n {
		k := int64(1000 + i)
		ix.Insert(k, graph.NodeID(uint64(2000+i))) // G115: bounded non-negative
		if e := ix.tree.Load().get(k); dead[e] {
			t.Fatalf("new key %d was given the slot of a detached entry (%p)", k, e)
		}
	}
}

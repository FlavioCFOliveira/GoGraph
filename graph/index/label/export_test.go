package label

import (
	"testing"
	"unsafe"
)

// labelPresent reports whether the spine currently holds an entry for label.
//
// It exists so a test can assert on the spine's STRUCTURE — that an emptied
// label's entry is dropped, not merely emptied — which no exported operation
// can distinguish: Count, Has and Scan all answer the same for an absent label
// and for a present-but-empty one. The pre-#2685 test read the map field
// directly; the geometry that replaced it has no such field, so the assertion is
// re-expressed here rather than weakened.
//
// It takes the spine read lock, so it is safe to call concurrently.
func (i *Index) labelPresent(label uint32) bool {
	i.mu.RLock()
	_, ok := i.spine[label]
	i.mu.RUnlock()
	return ok
}

// markEntryDead force-kills label's entry the way [Index.reap] does, without
// requiring the set to be empty. It is the seam the reaped-entry retry path in
// [Index.mutate] is exercised through: production only reaches a dead entry
// through a race a test cannot schedule deterministically.
//
// It reports whether the label had an entry to kill.
func (i *Index) markEntryDead(label uint32) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.spine[label]
	if !ok {
		return false
	}
	e.mu.Lock()
	e.dead = true
	e.mu.Unlock()
	delete(i.spine, label)
	return true
}

// TestEntryIsCacheLineSized pins the padding [entryPad] applies. The padding is
// a measured performance property — it stops two hot labels' locks sharing a
// cache line — so a change to sync.RWMutex's or index.NodeSet's size must fail
// here rather than silently un-pad every entry.
func TestEntryIsCacheLineSized(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(entry{}); got != cacheLine {
		t.Fatalf("unsafe.Sizeof(entry{}) = %d, want %d; adjust entryPad (currently %d) so the struct fills exactly one %d-byte line",
			got, cacheLine, entryPad, cacheLine)
	}
}

package cypher

// presence_intern_internal_test.go — rmp #2386. The presence path returns one of
// only 2^N maps for a fixed presence-key set, so they are precomputed once on the
// nodeScalarUse and selected per row instead of allocated per row.
//
// The end-to-end semantics (present / absent / mixed / C1 value-also-needed) are
// already pinned by rel_presence_isnull_test.go, and those tests are what prove
// the mask indexing selects the right answer. What is pinned HERE is the table
// itself: its shape, the absent-map-is-nil invariant that box-at-sink depends on,
// the stable key ordering, and the bound that keeps a pathological predicate from
// materialising an exponential table. Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// newPresenceUse builds a nodeScalarUse carrying exactly the given presence keys
// and interns its table, the same way analyseNodeScalarUse does after its C1
// reconciliation.
func newPresenceUse(keys ...string) *nodeScalarUse {
	u := &nodeScalarUse{keys: map[string]struct{}{}, presenceKeys: map[string]struct{}{}}
	for _, k := range keys {
		u.presenceKeys[k] = struct{}{}
	}
	u.internPresenceMaps()
	return u
}

// TestInternPresenceMaps_TableCoversEverySubset pins that the precomputed table
// holds exactly one map per possible present-set, and that each map carries the
// placeholder under exactly the keys its mask names. This is what lets the row
// path answer with a selection instead of an allocation.
func TestInternPresenceMaps_TableCoversEverySubset(t *testing.T) {
	u := newPresenceUse("since", "weight")

	if got, want := len(u.presenceMaps), 1<<2; got != want {
		t.Fatalf("interned %d maps for 2 presence keys, want %d (one per subset)", got, want)
	}
	if got, want := len(u.presenceKeyOrder), 2; got != want {
		t.Fatalf("presenceKeyOrder has %d entries, want %d", got, want)
	}
	// Sorted, so a key's bit position is stable rather than dependent on Go's
	// randomised map iteration order.
	if u.presenceKeyOrder[0] != "since" || u.presenceKeyOrder[1] != "weight" {
		t.Fatalf("presenceKeyOrder = %v, want sorted [since weight]", u.presenceKeyOrder)
	}

	for mask := 1; mask < len(u.presenceMaps); mask++ {
		m := u.presenceMaps[mask]
		want := 0
		for i, k := range u.presenceKeyOrder {
			_, present := m[k]
			wantPresent := mask&(1<<i) != 0
			if present != wantPresent {
				t.Errorf("mask %02b: key %q present=%v, want %v", mask, k, present, wantPresent)
			}
			if wantPresent {
				want++
				if m[k] != relPresencePlaceholder {
					t.Errorf("mask %02b: key %q carries %v, want the presence placeholder", mask, k, m[k])
				}
			}
		}
		if len(m) != want {
			t.Errorf("mask %02b: map holds %d keys, want %d", mask, len(m), want)
		}
	}
}

// TestInternPresenceMaps_EmptySetIsNil pins the invariant box-at-sink depends on:
// when no key is present the answer is an ABSENT map, not an empty one. The
// per-row build returned a nil expr.MapValue in that case and the interned table
// must keep returning exactly that.
func TestInternPresenceMaps_EmptySetIsNil(t *testing.T) {
	u := newPresenceUse("since")
	if u.presenceMaps[0] != nil {
		t.Errorf("presenceMaps[0] = %v, want nil: no key present must stay an ABSENT map, "+
			"because an empty non-nil map is a different value at the sink", u.presenceMaps[0])
	}
}

// TestInternPresenceMaps_BoundedByKeyCount pins the resource bound. A predicate
// naming more presence-only keys than presenceInternMaxKeys must leave the table
// nil so the row path falls back to the per-row build, rather than materialising
// 2^N maps.
func TestInternPresenceMaps_BoundedByKeyCount(t *testing.T) {
	keys := make([]string, 0, presenceInternMaxKeys+1)
	for i := 0; i <= presenceInternMaxKeys; i++ {
		keys = append(keys, string(rune('a'+i)))
	}

	over := newPresenceUse(keys...)
	if over.presenceMaps != nil {
		t.Errorf("interned a table for %d presence keys, want nil above the bound of %d: "+
			"the table is exponential in the key count", len(keys), presenceInternMaxKeys)
	}

	at := newPresenceUse(keys[:presenceInternMaxKeys]...)
	if got, want := len(at.presenceMaps), 1<<presenceInternMaxKeys; got != want {
		t.Errorf("at the bound of %d keys interned %d maps, want %d", presenceInternMaxKeys, got, want)
	}
}

// TestInternPresenceMaps_NoKeysLeavesTableNil pins that a variable with no
// presence-only key interns nothing at all — the overwhelmingly common case,
// which must not pay for a table it will never consult.
func TestInternPresenceMaps_NoKeysLeavesTableNil(t *testing.T) {
	u := newPresenceUse()
	if u.presenceMaps != nil || u.presenceKeyOrder != nil {
		t.Errorf("interned a table for a variable with no presence keys: maps=%v order=%v",
			u.presenceMaps, u.presenceKeyOrder)
	}
}

// TestInternPresenceMaps_SelectionAllocatesNothing is the point of the change:
// selecting an answer from the interned table must perform NO allocation,
// however many rows select it. Before rmp #2386 each row built its own map —
// 2.00 GB over 6 511 214 calls in examples/26, about 330 B to deliver a boolean.
func TestInternPresenceMaps_SelectionAllocatesNothing(t *testing.T) {
	u := newPresenceUse("since")

	var sink expr.MapValue
	allocs := testing.AllocsPerRun(200, func() {
		// The row path's shape: compute a mask from the per-row storage presence
		// checks, then select. Only the selection is under test; the presence
		// checks themselves are unchanged and still run per row.
		mask := 1
		sink = u.presenceMaps[mask]
	})
	if allocs != 0 {
		t.Errorf("selecting an interned presence answer performed %.0f allocations, want 0", allocs)
	}
	if len(sink) != 1 {
		t.Fatalf("selected map holds %d keys, want 1", len(sink))
	}
}

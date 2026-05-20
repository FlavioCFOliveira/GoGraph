package plan_test

import (
	"testing"

	"gograph/cypher/plan"
	"gograph/graph/index"
)

// stubSubscriber is a minimal index.Subscriber used in tests to avoid
// pulling in concrete index implementations for kind-mapping checks.
type stubSubscriber struct{ kind string }

func (s stubSubscriber) Apply(index.Change) {}
func (s stubSubscriber) Kind() string       { return s.kind }

// mustCreate registers sub under name in mgr, failing t on error.
func mustCreate(t *testing.T, mgr *index.Manager, name string, sub index.Subscriber) {
	t.Helper()
	if err := mgr.CreateIndex(name, sub); err != nil {
		t.Fatalf("CreateIndex(%q): %v", name, err)
	}
}

// TestIndexRegistry_Empty verifies that a nil manager produces an empty
// registry whose methods return sane zero-value defaults.
func TestIndexRegistry_Empty(t *testing.T) {
	t.Parallel()

	r := plan.NewIndexRegistry(nil)

	if got := r.All(); got != nil {
		t.Errorf("All() = %v, want nil", got)
	}
	if got := r.ByKind(plan.IndexKindHash); got != nil {
		t.Errorf("ByKind(Hash) = %v, want nil", got)
	}
	if _, ok := r.Lookup("any"); ok {
		t.Error("Lookup on empty registry returned found=true")
	}
	if r.HasHash() {
		t.Error("HasHash() = true on empty registry")
	}
	if r.HasBTree() {
		t.Error("HasBTree() = true on empty registry")
	}
}

// TestIndexRegistry_ByKind verifies that ByKind correctly filters entries
// by their classified kind.
func TestIndexRegistry_ByKind(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	mustCreate(t, mgr, "lbl-a", stubSubscriber{"label"})
	mustCreate(t, mgr, "lbl-b", stubSubscriber{"label"})
	mustCreate(t, mgr, "hash-name", stubSubscriber{"hash"})

	r := plan.NewIndexRegistry(mgr)

	tests := []struct {
		name string
		kind plan.IndexKind
		want int
	}{
		{"label entries", plan.IndexKindLabel, 2},
		{"hash entries", plan.IndexKindHash, 1},
		{"btree entries", plan.IndexKindBTree, 0},
		{"unknown entries", plan.IndexKindUnknown, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := r.ByKind(tc.kind)
			if len(got) != tc.want {
				t.Errorf("ByKind(%v) len = %d, want %d", tc.kind, len(got), tc.want)
			}
		})
	}
}

// TestIndexRegistry_Lookup verifies name-based lookup, both present and absent.
func TestIndexRegistry_Lookup(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	mustCreate(t, mgr, "hash-age", stubSubscriber{"hash"})
	mustCreate(t, mgr, "btree-name", stubSubscriber{"btree"})

	r := plan.NewIndexRegistry(mgr)

	tests := []struct {
		name      string
		lookupKey string
		wantFound bool
		wantKind  plan.IndexKind
	}{
		{"present hash", "hash-age", true, plan.IndexKindHash},
		{"present btree", "btree-name", true, plan.IndexKindBTree},
		{"absent", "nonexistent", false, plan.IndexKindUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, ok := r.Lookup(tc.lookupKey)
			if ok != tc.wantFound {
				t.Fatalf("Lookup(%q) found = %v, want %v", tc.lookupKey, ok, tc.wantFound)
			}
			if ok && entry.Kind != tc.wantKind {
				t.Errorf("Lookup(%q) kind = %v, want %v", tc.lookupKey, entry.Kind, tc.wantKind)
			}
			if ok && entry.Name != tc.lookupKey {
				t.Errorf("Lookup(%q) name = %q, want %q", tc.lookupKey, entry.Name, tc.lookupKey)
			}
		})
	}
}

// TestIndexRegistry_HasHash verifies HasHash returns true only when a hash
// index is present.
func TestIndexRegistry_HasHash(t *testing.T) {
	t.Parallel()

	t.Run("no hash", func(t *testing.T) {
		t.Parallel()
		mgr := index.NewManager()
		mustCreate(t, mgr, "lbl", stubSubscriber{"label"})
		r := plan.NewIndexRegistry(mgr)
		if r.HasHash() {
			t.Error("HasHash() = true, want false when no hash index exists")
		}
	})

	t.Run("with hash", func(t *testing.T) {
		t.Parallel()
		mgr := index.NewManager()
		mustCreate(t, mgr, "h", stubSubscriber{"hash"})
		r := plan.NewIndexRegistry(mgr)
		if !r.HasHash() {
			t.Error("HasHash() = false, want true when hash index exists")
		}
	})
}

// TestIndexRegistry_HasBTree verifies HasBTree returns true only when a btree
// index is present.
func TestIndexRegistry_HasBTree(t *testing.T) {
	t.Parallel()

	t.Run("no btree", func(t *testing.T) {
		t.Parallel()
		mgr := index.NewManager()
		mustCreate(t, mgr, "h", stubSubscriber{"hash"})
		r := plan.NewIndexRegistry(mgr)
		if r.HasBTree() {
			t.Error("HasBTree() = true, want false when no btree index exists")
		}
	})

	t.Run("with btree", func(t *testing.T) {
		t.Parallel()
		mgr := index.NewManager()
		mustCreate(t, mgr, "bt", stubSubscriber{"btree"})
		r := plan.NewIndexRegistry(mgr)
		if !r.HasBTree() {
			t.Error("HasBTree() = false, want true when btree index exists")
		}
	})
}

// TestIndexRegistry_KindMapping verifies the classification of every
// subscriber kind string, including unknown variants.
func TestIndexRegistry_KindMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		subscriberKind string
		wantIndexKind  plan.IndexKind
	}{
		{"label", plan.IndexKindLabel},
		{"hash", plan.IndexKindHash},
		{"btree", plan.IndexKindBTree},
		{"", plan.IndexKindUnknown},
		{"HASH", plan.IndexKindUnknown}, // case-sensitive: uppercase not matched
		{"fulltext", plan.IndexKindUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.subscriberKind, func(t *testing.T) {
			t.Parallel()
			mgr := index.NewManager()
			// Use a unique name derived from the kind string to avoid collisions.
			name := "idx-" + tc.subscriberKind
			if tc.subscriberKind == "" {
				name = "idx-empty"
			}
			mustCreate(t, mgr, name, stubSubscriber{tc.subscriberKind})
			r := plan.NewIndexRegistry(mgr)
			entry, ok := r.Lookup(name)
			if !ok {
				t.Fatalf("Lookup(%q): not found after registration", name)
			}
			if entry.Kind != tc.wantIndexKind {
				t.Errorf("kind %q: got IndexKind %v, want %v",
					tc.subscriberKind, entry.Kind, tc.wantIndexKind)
			}
		})
	}
}

// TestIndexRegistry_All verifies that All returns a copy of all entries
// and that mutations to the returned slice do not affect the registry.
func TestIndexRegistry_All(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	mustCreate(t, mgr, "a", stubSubscriber{"label"})
	mustCreate(t, mgr, "b", stubSubscriber{"hash"})
	mustCreate(t, mgr, "c", stubSubscriber{"btree"})

	r := plan.NewIndexRegistry(mgr)

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("All() len = %d, want 3", len(all))
	}

	// Mutate the returned slice — registry should be unaffected.
	all[0] = plan.IndexEntry{}
	second := r.All()
	if len(second) != 3 {
		t.Errorf("All() after mutation len = %d, want 3", len(second))
	}
	for _, e := range second {
		if e.Name == "" {
			t.Error("registry internal state was mutated by caller modification of All() slice")
		}
	}
}

// TestIndexRegistry_SubscriberPreserved verifies that the Subscriber field
// of a returned IndexEntry is the exact same instance registered with the
// manager, enabling planner rules to type-assert to kind-specific APIs.
func TestIndexRegistry_SubscriberPreserved(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	sub := stubSubscriber{"hash"}
	mustCreate(t, mgr, "prop:email", sub)

	r := plan.NewIndexRegistry(mgr)
	entry, ok := r.Lookup("prop:email")
	if !ok {
		t.Fatal("Lookup: not found")
	}

	got, ok := entry.Subscriber.(stubSubscriber)
	if !ok {
		t.Fatal("Subscriber is not a stubSubscriber")
	}
	if got != sub {
		t.Errorf("Subscriber = %v, want %v", got, sub)
	}
}

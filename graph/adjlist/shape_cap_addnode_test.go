package adjlist_test

// Tests for AddNode behaviour under MaxShardCapacity.
//
// AddNode only interns the key in the Mapper; it never allocates a shard
// slot. Therefore AddNode must always return nil — even when the responsible
// shard is already full — and Order must track every interned key
// regardless of whether AddEdge later succeeds for that key.
//
// These tests are the external-package complement to adjlist_cap_test.go,
// which covers the AddEdge overflow paths. They are in package adjlist_test
// so that no internal symbols are required; all interaction goes through
// the exported API.

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
)

// shard0Keys returns n distinct int keys whose Mapper-assigned NodeID has
// low 8 bits equal to zero (i.e. they land in shard 0 of a fresh AdjList).
// The probe uses a temporary, throw-away AdjList so that interning these
// keys does not affect the AdjList under test.
func shard0Keys(t *testing.T, n int) []int {
	t.Helper()
	const shardMask = 0xFF // mirrors the 256-shard layout in adjlist.go
	probe := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	keys := make([]int, 0, n)
	for i := 0; len(keys) < n && i < 1_000_000; i++ {
		id := probe.Mapper().Intern(i)
		if uint64(id)&shardMask == 0 {
			keys = append(keys, i)
		}
	}
	if len(keys) < n {
		t.Fatalf("shard0Keys: needed %d keys routing to shard 0, found only %d", n, len(keys))
	}
	return keys
}

// TestAdjList_MaxShardCapacity_AddNode_AlwaysSucceeds verifies the core
// contract: AddNode returns nil even when the responsible shard is full.
//
// Steps:
//  1. Saturate shard 0 via AddEdge (MaxShardCapacity=2, so 2 self-loops).
//  2. Confirm the next AddEdge on a shard-0 key returns ErrShardFull.
//  3. Call AddNode on that same (overflow) key — must return nil.
//  4. Order must reflect the new intern (AddNode succeeded in the Mapper).
func TestAdjList_MaxShardCapacity_AddNode_AlwaysSucceeds(t *testing.T) {
	t.Parallel()
	const cap = 2
	keys := shard0Keys(t, cap+1)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true, MaxShardCapacity: cap})

	// Saturate shard 0: each self-loop claims one intra-shard slot.
	for i := 0; i < cap; i++ {
		if err := a.AddEdge(keys[i], keys[i], struct{}{}); err != nil {
			t.Fatalf("AddEdge[%d] unexpected error while saturating: %v", i, err)
		}
	}

	// Confirm shard 0 is full.
	if err := a.AddEdge(keys[cap], keys[cap], struct{}{}); !errors.Is(err, adjlist.ErrShardFull) {
		t.Fatalf("expected ErrShardFull on overflow AddEdge; got %v", err)
	}

	// The overflow key was interned by the failed AddEdge (Intern happens
	// before the shard check). Record the current Order before AddNode.
	orderBefore := a.Order()

	// AddNode on an already-interned key must be a no-op and return nil.
	if err := a.AddNode(keys[cap]); err != nil {
		t.Fatalf("AddNode on full-shard key returned %v; want nil", err)
	}
	if got := a.Order(); got != orderBefore {
		t.Fatalf("Order changed after AddNode on already-interned key: %d → %d", orderBefore, got)
	}

	// AddNode on a brand-new shard-0 key must also succeed.
	fresh := shard0Keys(t, cap+2) // 4 keys; the last one is guaranteed new
	newKey := fresh[cap+1]
	// Ensure newKey is not already present by choosing from a disjoint set
	// (shard0Keys probes a fresh throw-away mapper, so keys[2] and fresh[3]
	// coincide only by accident; we disambiguate by picking the 4th key
	// from a longer list).
	orderBeforeNew := a.Order()
	if err := a.AddNode(newKey); err != nil {
		t.Fatalf("AddNode(newKey=%d) on full shard returned %v; want nil", newKey, err)
	}
	if got := a.Order(); got != orderBeforeNew+1 {
		t.Fatalf("Order after AddNode(newKey): got %d, want %d", got, orderBeforeNew+1)
	}
}

// TestAdjList_MaxShardCapacity_OrderTracked_AddNodeOnly verifies that
// AddNode alone (without any AddEdge) correctly tracks Order and leaves
// Size at zero, and that subsequent AddEdge calls still respect the cap.
func TestAdjList_MaxShardCapacity_OrderTracked_AddNodeOnly(t *testing.T) {
	t.Parallel()
	const cap = 2
	keys := shard0Keys(t, 5) // 5 distinct shard-0 keys

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true, MaxShardCapacity: cap})

	// Add 5 nodes via AddNode only — none must fail.
	for i, k := range keys {
		if err := a.AddNode(k); err != nil {
			t.Fatalf("AddNode[%d] (key=%d) returned %v; want nil", i, k, err)
		}
	}

	if got := a.Order(); got != 5 {
		t.Fatalf("Order after 5×AddNode = %d; want 5", got)
	}
	if got := a.Size(); got != 0 {
		t.Fatalf("Size after AddNode-only = %d; want 0", got)
	}

	// AddEdge for the first two shard-0 keys must succeed (slots 0 and 1).
	for i := 0; i < cap; i++ {
		if err := a.AddEdge(keys[i], keys[i], struct{}{}); err != nil {
			t.Fatalf("AddEdge[%d] after AddNode setup: %v", i, err)
		}
	}

	// Third AddEdge on a shard-0 key must hit the cap.
	if err := a.AddEdge(keys[cap], keys[cap], struct{}{}); !errors.Is(err, adjlist.ErrShardFull) {
		t.Fatalf("expected ErrShardFull on third shard-0 AddEdge; got %v", err)
	}

	if got := a.Size(); got != uint64(cap) {
		t.Fatalf("Size after cap hit = %d; want %d", got, cap)
	}
	// Order must still equal 5: AddEdge failure must not undo interns.
	if got := a.Order(); got != 5 {
		t.Fatalf("Order after AddEdge overflow = %d; want 5 (interns must survive failed AddEdge)", got)
	}
}

// TestAdjList_MaxShardCapacity_AddEdgeOverflow_PreservesOrder documents
// the subtle behaviour that a failed AddEdge (ErrShardFull) still interns
// its endpoints in the Mapper, because Intern is called before the shard
// check. Order must equal 2 after two successful AddEdge calls, and must
// equal 3 (not 2) after the third, failing AddEdge — because both
// endpoints of the third call are interned before the overflow is detected.
func TestAdjList_MaxShardCapacity_AddEdgeOverflow_PreservesOrder(t *testing.T) {
	t.Parallel()
	const cap = 2
	keys := shard0Keys(t, cap+1)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true, MaxShardCapacity: cap})

	for i := 0; i < cap; i++ {
		if err := a.AddEdge(keys[i], keys[i], struct{}{}); err != nil {
			t.Fatalf("AddEdge[%d]: %v", i, err)
		}
	}
	if got := a.Order(); got != uint64(cap) {
		t.Fatalf("Order before overflow = %d; want %d", got, cap)
	}

	// This AddEdge fails, but keys[cap] is interned as both src and dst
	// before the shard check fires.
	err := a.AddEdge(keys[cap], keys[cap], struct{}{})
	if !errors.Is(err, adjlist.ErrShardFull) {
		t.Fatalf("expected ErrShardFull; got %v", err)
	}

	// keys[cap] is a self-loop (src == dst), so exactly one new key was
	// interned, bringing Order from cap to cap+1.
	if got := a.Order(); got != uint64(cap+1) {
		t.Fatalf("Order after failed AddEdge = %d; want %d (intern precedes shard check)", got, cap+1)
	}
	// Size must remain unchanged.
	if got := a.Size(); got != uint64(cap) {
		t.Fatalf("Size after failed AddEdge = %d; want %d", got, cap)
	}
}

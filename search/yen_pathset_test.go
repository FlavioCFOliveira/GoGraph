package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestPathSet_Membership covers the basic add/dedup semantics of the
// hash-with-fallback path set introduced for Yen's #1591 dedup.
func TestPathSet_Membership(t *testing.T) {
	t.Parallel()
	s := newPathSet(4)
	p1 := []graph.NodeID{0, 1, 2}
	p2 := []graph.NodeID{0, 1, 3}
	p3 := []graph.NodeID{0, 1, 2, 4}

	if !s.add(p1) {
		t.Fatal("first add of p1 should be new")
	}
	if s.add(p1) {
		t.Fatal("re-add of p1 should report duplicate")
	}
	// An element-wise-equal but distinct backing array must still dedup.
	if s.add([]graph.NodeID{0, 1, 2}) {
		t.Fatal("equal-but-copied p1 should report duplicate")
	}
	if !s.add(p2) {
		t.Fatal("p2 differs in last element; should be new")
	}
	if !s.add(p3) {
		t.Fatal("p3 is a strict prefix-extension; should be new")
	}
	// Empty sequence is a valid distinct member.
	if !s.add([]graph.NodeID{}) {
		t.Fatal("empty sequence should be new")
	}
	if s.add(nil) {
		t.Fatal("nil sequence equals the empty sequence; should dedup")
	}
}

// TestPathSet_CollisionFallback proves the element-wise fallback keeps two
// DISTINCT sequences distinct even when they land in the same hash bucket.
// Because hashNodes is a real hash, the collision is forced white-box by
// pre-seeding the bucket the second sequence will hash to with a distinct
// decoy sequence; add must then walk the bucket, find no element-wise
// match, and insert (returning true) rather than wrongly reporting a
// duplicate. A bare hash set without the fallback would drop the path.
func TestPathSet_CollisionFallback(t *testing.T) {
	t.Parallel()
	s := newPathSet(4)
	real := []graph.NodeID{5, 6, 7}
	h := hashNodes(real)

	// Simulate a collision: a different sequence already occupies real's
	// hash slot as the inline "first". The decoy is intentionally NOT
	// element-wise equal to real.
	decoy := []graph.NodeID{9, 9, 9, 9}
	s.first[h] = decoy

	if !s.add(real) {
		t.Fatal("colliding-but-distinct sequence must be added (fallback failure)")
	}
	if len(s.overflow[h]) != 1 {
		t.Fatalf("overflow should hold the colliding sequence, got %d", len(s.overflow[h]))
	}
	// The real sequence is now a genuine duplicate even though it shares a
	// hash slot with the decoy: add must check first then overflow, match
	// real element-wise, and report the duplicate (proving the fallback is
	// authoritative — a bare hash set would have dropped real on insert).
	if s.add([]graph.NodeID{5, 6, 7}) {
		t.Fatal("genuine duplicate after collision must dedup")
	}
	// Both co-resident sequences are matchable element-wise within the
	// shared hash slot.
	if !nodesEqual(s.first[h], decoy) {
		t.Fatal("inline first slot must still hold the decoy")
	}
	if !nodesEqual(s.overflow[h][0], real) {
		t.Fatal("overflow must hold the colliding real sequence")
	}
}

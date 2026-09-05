package btree

// height4_corruption_test.go — regression test for the production-readiness
// audit finding [D1] (rmp #2037).
//
// At tree height >= 4, bplus.contains() inspected only a node and its direct
// children (no recursion), so removeChild() mis-routed and no-oped on a leaf
// deeper than a grandchild. An emptied leaf was unlinked from the forward chain
// but left live in the tree; a later reinsert landed in that off-chain leaf,
// visible to Lookup (tree descent) but invisible to Range/Serialize (chain
// walk) — a silent ACID-Consistency violation and a corrupt on-disk image.
//
// This test builds a height-4 tree, deletes a key band, reinserts one key, and
// asserts Range == Lookup, the chain-vs-tree count invariant, and a lossless
// Serialize round trip. It FAILS on the pre-fix code.

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

func TestBPlus_Height4_DeleteReinsert_RangeAndSerialize(t *testing.T) {
	t.Parallel()

	// Fanout is 128, so a height-4 tree needs > ~128^3 (~2.1M) distinct keys.
	const n = 2_300_000
	vals := make([]int, n)
	nodes := make([]graph.NodeID, n)
	for i := 0; i < n; i++ {
		vals[i] = i
		nodes[i] = graph.NodeID(uint64(i))
	}
	idx := New[int]()
	if err := idx.BulkLoadSorted(vals, nodes); err != nil {
		t.Fatalf("BulkLoadSorted: %v", err)
	}
	if h := idx.tree.Load().height; h < 4 {
		t.Fatalf("tree height = %d, want >= 4 to exercise the deep-leaf path; increase n", h)
	}

	// Delete a full band deep in the middle of the tree, then reinsert one key
	// from the band (matches the audit reproduction).
	const base = 1_000_000
	for v := base; v < base+400; v++ {
		idx.Delete(v, graph.NodeID(uint64(v)))
	}
	idx.Insert(base+200, graph.NodeID(uint64(base+200)))

	// Lookup (tree descent) finds it.
	if got := idx.Lookup(base + 200).GetCardinality(); got != 1 {
		t.Fatalf("Lookup(%d) cardinality = %d, want 1", base+200, got)
	}
	// Range (forward-chain walk) must ALSO find it — pre-fix it did not.
	if rng := idx.Range(base+100, base+300); !rng.Contains(uint64(base + 200)) {
		t.Fatalf("Range omits reinserted key %d — forward chain desynced from the tree", base+200)
	}

	// Scan-vs-tree invariant: keys reachable by an in-order forward walk must
	// equal the tree's key count. A leaf that the structural removal failed to
	// detach, or one detached from the scan path but still live in the tree,
	// breaks this. Forward iteration is the descent cursor now that the leaf
	// chain is gone (#2683); the invariant it asserts is unchanged.
	snap := idx.tree.Load()
	scanKeys := 0
	var cur cursor[int]
	for cur.seekFirst(snap); cur.valid(); cur.next() {
		scanKeys++
	}
	if scanKeys != snap.count {
		t.Fatalf("forward-scan key count %d != tree count %d (off-scan leaf present)", scanKeys, snap.count)
	}

	// Serialize header/body consistency: a round trip must be lossless.
	var buf bytes.Buffer
	if err := idx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	idx2 := New[int]()
	if err := idx2.Deserialize(&buf); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if idx2.DistinctValues() != idx.DistinctValues() {
		t.Fatalf("round-trip DistinctValues = %d, want %d (corrupt serialized image)", idx2.DistinctValues(), idx.DistinctValues())
	}
	if !idx2.Range(base+100, base+300).Contains(uint64(base + 200)) {
		t.Fatalf("round-trip Range omits the reinserted key (corrupt serialized image)")
	}
}

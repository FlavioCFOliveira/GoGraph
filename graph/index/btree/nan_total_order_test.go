package btree

// nan_total_order_test.go — regression gate for task #1354.
//
// Before the fix, every comparison in this package used the raw <, ==,
// and >= operators, which form only a PARTIAL order over float64: every
// comparison involving NaN is false. A single Insert(NaN, …) appended an
// entry that broke the monotone-predicate precondition of sort.Search,
// so later inserts of ordinary floats landed at the wrong position and
// destroyed the sorted invariant — Lookup/Range/Delete of real, live
// values then silently missed.
//
// The fix replaces every comparison with the stdlib TOTAL order
// (cmp.Less / cmp.Compare): a NaN key sorts before every other value
// (including -Inf), all NaN bit patterns compare equal to each other,
// and ±0.0 stay one entry. The sorted invariant therefore holds for
// every possible float64 input, with no API change.

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// sortedInvariantHolds reports whether the B+ tree's leaf chain is
// strictly ascending under the [cmp.Compare] total order — the
// precondition every lower-bound search in this package depends on. It
// walks the leaves low→high (the same order Serialize and Range use) and
// checks each key against the previous one across leaf boundaries.
func sortedInvariantHolds[V cmp.Ordered](i *Index[V]) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	var prev V
	hasPrev := false
	for l := i.tree.first; l != nil; l = l.next {
		for k := range l.keys {
			if hasPrev && cmp.Compare(prev, l.keys[k]) >= 0 {
				return false
			}
			prev = l.keys[k]
			hasPrev = true
		}
	}
	return true
}

// TestNaNInsert_FiniteKeysStayQueryable is the audit repro from task
// #1354: an interleaved NaN insert must not corrupt the index for the
// finite keys inserted before or after it.
func TestNaNInsert_FiniteKeysStayQueryable(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	idx.Insert(1, 10)
	idx.Insert(math.NaN(), 99)
	idx.Insert(2, 20)
	idx.Insert(0.5, 5)

	if !sortedInvariantHolds(idx) {
		t.Fatal("sorted invariant broken after interleaved NaN insert")
	}
	for _, tc := range []struct {
		v    float64
		node uint64
	}{{1, 10}, {2, 20}, {0.5, 5}} {
		bm := idx.Lookup(tc.v)
		if bm.GetCardinality() != 1 || !bm.Contains(tc.node) {
			t.Errorf("Lookup(%v) = cardinality %d, want exactly node %d",
				tc.v, bm.GetCardinality(), tc.node)
		}
	}
	got := idx.Range(0, 3)
	for _, n := range []uint64{5, 10, 20} {
		if !got.Contains(n) {
			t.Errorf("Range(0,3): missing node %d", n)
		}
	}
	if got.Contains(99) {
		t.Error("Range(0,3): must not include the NaN-keyed node 99")
	}
}

// TestNaNInsert_InfinitiesRemainValidKeys pins that ±Inf are ordinary,
// fully ordered keys: NaN interleavings must not disturb them, and NaN
// sorts BEFORE -Inf so Range(-Inf, +Inf) excludes NaN-keyed nodes.
func TestNaNInsert_InfinitiesRemainValidKeys(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	idx.Insert(math.Inf(1), 1)
	idx.Insert(math.NaN(), 99)
	idx.Insert(math.Inf(-1), 2)
	idx.Insert(7.5, 3)

	if !sortedInvariantHolds(idx) {
		t.Fatal("sorted invariant broken with ±Inf and NaN keys")
	}
	if bm := idx.Lookup(math.Inf(1)); !bm.Contains(1) {
		t.Error("Lookup(+Inf): missing node 1")
	}
	if bm := idx.Lookup(math.Inf(-1)); !bm.Contains(2) {
		t.Error("Lookup(-Inf): missing node 2")
	}
	full := idx.Range(math.Inf(-1), math.Inf(1))
	for _, n := range []uint64{1, 2, 3} {
		if !full.Contains(n) {
			t.Errorf("Range(-Inf,+Inf): missing node %d", n)
		}
	}
	if full.Contains(99) {
		t.Error("Range(-Inf,+Inf): NaN sorts before -Inf, node 99 must be absent")
	}
	v, n, ok := idx.RangeFirst(math.Inf(-1), math.Inf(1))
	if !ok || !math.IsInf(v, -1) || n != 2 {
		t.Errorf("RangeFirst(-Inf,+Inf) = (%v, %d, %v), want (-Inf, 2, true)", v, n, ok)
	}
}

// TestNaNKey_TotalOrderSemantics pins NaN as a first-class key under
// the total order: all NaN bit patterns collapse into one entry,
// Lookup/Cardinality/Delete address it, and removing it leaves the
// rest of the index intact.
func TestNaNKey_TotalOrderSemantics(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	nanA := math.NaN()
	nanB := math.Float64frombits(0x7FF8000000000001) // distinct NaN payload
	idx.Insert(nanA, 7)
	idx.Insert(nanB, 8) // must deduplicate into the same entry
	idx.Insert(1, 1)

	if got := idx.DistinctValues(); got != 2 {
		t.Fatalf("DistinctValues = %d, want 2 (one merged NaN entry + 1.0)", got)
	}
	bm := idx.Lookup(math.NaN())
	if bm.GetCardinality() != 2 || !bm.Contains(7) || !bm.Contains(8) {
		t.Fatalf("Lookup(NaN) = cardinality %d, want nodes {7, 8}", bm.GetCardinality())
	}
	if got := idx.Cardinality(math.NaN()); got != 2 {
		t.Fatalf("Cardinality(NaN) = %d, want 2", got)
	}

	idx.Delete(math.NaN(), 7)
	if got := idx.Cardinality(math.NaN()); got != 1 {
		t.Fatalf("Cardinality(NaN) after one Delete = %d, want 1", got)
	}
	idx.Delete(math.NaN(), 8)
	if got := idx.DistinctValues(); got != 1 {
		t.Fatalf("DistinctValues after NaN entry drained = %d, want 1", got)
	}
	if bm := idx.Lookup(1.0); !bm.Contains(1) {
		t.Error("Lookup(1.0) lost node 1 after NaN deletions")
	}
}

// TestBulkLoad_NaNInputs_SortedAndQueryable asserts BulkLoad with NaN
// and ±Inf inputs produces a totally ordered index where every key —
// finite, infinite, and the merged NaN entry — stays queryable.
func TestBulkLoad_NaNInputs_SortedAndQueryable(t *testing.T) {
	t.Parallel()
	idx := New[float64]()
	values := []float64{3, math.NaN(), 1, math.Inf(1), math.NaN(), 2, math.Inf(-1)}
	nodes := []graph.NodeID{30, 90, 10, 40, 91, 20, 50}
	if err := idx.BulkLoad(values, nodes); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if !sortedInvariantHolds(idx) {
		t.Fatal("sorted invariant broken after BulkLoad with NaN inputs")
	}
	if got := idx.DistinctValues(); got != 6 {
		t.Fatalf("DistinctValues = %d, want 6 (NaN, -Inf, 1, 2, 3, +Inf)", got)
	}
	for _, tc := range []struct {
		v    float64
		node uint64
	}{{1, 10}, {2, 20}, {3, 30}, {math.Inf(1), 40}, {math.Inf(-1), 50}} {
		if !idx.Lookup(tc.v).Contains(tc.node) {
			t.Errorf("Lookup(%v): missing node %d", tc.v, tc.node)
		}
	}
	nan := idx.Lookup(math.NaN())
	if nan.GetCardinality() != 2 || !nan.Contains(90) || !nan.Contains(91) {
		t.Errorf("Lookup(NaN) = cardinality %d, want nodes {90, 91}", nan.GetCardinality())
	}
}

// TestInsert_SortedInvariant_Rapid drives arbitrary float64 insert
// sequences — finite values mixed with NaN, ±Inf, and ±0.0 — and
// asserts the sorted invariant survives every single insert and that
// every non-NaN key remains addressable afterwards.
func TestInsert_SortedInvariant_Rapid(t *testing.T) {
	t.Parallel()
	specials := []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		math.Copysign(0, -1),
		0,
	}
	rapid.Check(t, func(rt *rapid.T) {
		idx := New[float64]()
		type inserted struct {
			v    float64
			node graph.NodeID
		}
		var ordered []inserted // every non-NaN key inserted
		ops := rapid.IntRange(1, 64).Draw(rt, "ops")
		for op := 0; op < ops; op++ {
			var v float64
			if rapid.Bool().Draw(rt, "special") {
				v = rapid.SampledFrom(specials).Draw(rt, "which")
			} else {
				v = rapid.Float64().Draw(rt, "v")
			}
			node := graph.NodeID(uint64(op)) //nolint:gosec // op ∈ [0, 64)
			idx.Insert(v, node)
			if !sortedInvariantHolds(idx) {
				rt.Fatalf("sorted invariant broken after Insert(%v, %d)", v, node)
			}
			if !math.IsNaN(v) {
				ordered = append(ordered, inserted{v: v, node: node})
			}
		}
		for _, w := range ordered {
			if !idx.Lookup(w.v).Contains(uint64(w.node)) {
				rt.Fatalf("Lookup(%v) lost node %d after arbitrary interleavings", w.v, w.node)
			}
		}
	})
}

// craftFloat64Payload builds a serialised Index[float64] image with the
// given keys in the given on-disk order, one NodeID (i+1) per key. It
// mirrors the writer's layout (magic, version, count, entries, crc32c)
// so the reader's key-order validation is the only check under test.
func craftFloat64Payload(t *testing.T, keys []float64) []byte {
	t.Helper()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, btreeMagic)
	_ = binary.Write(&body, binary.LittleEndian, btreeFormatVersion)
	_ = binary.Write(&body, binary.LittleEndian, uint64(len(keys)))
	for i, k := range keys {
		_ = binary.Write(&body, binary.LittleEndian, uint32(8))
		_ = binary.Write(&body, binary.LittleEndian, math.Float64bits(k))
		_ = binary.Write(&body, binary.LittleEndian, uint64(1))
		_ = binary.Write(&body, binary.LittleEndian, uint64(i+1)) //nolint:gosec // tiny test corpus
	}
	checksum := crc32.Checksum(body.Bytes(), castagnoli)
	var payload bytes.Buffer
	payload.Write(body.Bytes())
	_ = binary.Write(&payload, binary.LittleEndian, checksum)
	return payload.Bytes()
}

// TestDeserialize_NaNKeyOrderPolicy pins the recovery policy for
// persisted NaN keys: a payload whose keys are not STRICTLY ascending
// under the total order — the shape a pre-fix index produced, with a
// NaN entry after a real key or duplicate NaN entries — is rejected
// fail-stop as [index.ErrIndexCorrupted], because its sorted invariant
// is unverifiable and serving it would silently miss live keys.
// Indexes are derived data; the caller rebuilds from the primary graph.
// A payload with the single NaN entry in the leading position — the
// only shape the post-fix writer produces — loads fine.
func TestDeserialize_NaNKeyOrderPolicy(t *testing.T) {
	t.Parallel()

	t.Run("nan-after-real-key-rejected", func(t *testing.T) {
		t.Parallel()
		idx := New[float64]()
		err := idx.Deserialize(bytes.NewReader(craftFloat64Payload(t, []float64{1.0, math.NaN()})))
		if !errors.Is(err, index.ErrIndexCorrupted) {
			t.Fatalf("pre-fix NaN placement = %v, want ErrIndexCorrupted", err)
		}
	})

	t.Run("duplicate-nan-entries-rejected", func(t *testing.T) {
		t.Parallel()
		idx := New[float64]()
		err := idx.Deserialize(bytes.NewReader(craftFloat64Payload(t, []float64{math.NaN(), math.NaN()})))
		if !errors.Is(err, index.ErrIndexCorrupted) {
			t.Fatalf("duplicate NaN entries = %v, want ErrIndexCorrupted", err)
		}
	})

	t.Run("leading-nan-accepted", func(t *testing.T) {
		t.Parallel()
		idx := New[float64]()
		payload := craftFloat64Payload(t, []float64{math.NaN(), math.Inf(-1), 1.0})
		if err := idx.Deserialize(bytes.NewReader(payload)); err != nil {
			t.Fatalf("leading-NaN payload: %v", err)
		}
		if !idx.Lookup(math.NaN()).Contains(1) {
			t.Error("Lookup(NaN): missing node 1")
		}
		if !idx.Lookup(math.Inf(-1)).Contains(2) {
			t.Error("Lookup(-Inf): missing node 2")
		}
		if !idx.Lookup(1.0).Contains(3) {
			t.Error("Lookup(1.0): missing node 3")
		}
	})

	t.Run("nan-round-trip", func(t *testing.T) {
		t.Parallel()
		src := New[float64]()
		src.Insert(math.NaN(), 9)
		src.Insert(2.5, 1)
		src.Insert(math.Inf(1), 2)
		var buf bytes.Buffer
		if err := src.Serialize(&buf); err != nil {
			t.Fatalf("Serialize: %v", err)
		}
		dst := New[float64]()
		if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatalf("Deserialize: %v", err)
		}
		if !dst.Lookup(math.NaN()).Contains(9) || !dst.Lookup(2.5).Contains(1) ||
			!dst.Lookup(math.Inf(1)).Contains(2) {
			t.Fatal("round-trip with NaN key lost entries")
		}
	})
}

// BenchmarkIndex_CardinalityFloat64 probes the comparison-bound point
// search on a float64 index — the path whose comparator changed from
// raw >= / == to cmp.Less / cmp.Compare in task #1354.
func BenchmarkIndex_CardinalityFloat64(b *testing.B) {
	const n = 1_000_000
	values := make([]float64, n)
	nodes := make([]graph.NodeID, n)
	for i := range values {
		values[i] = float64(i)
		nodes[i] = graph.NodeID(uint64(i)) //nolint:gosec // i < 1e6
	}
	idx := New[float64]()
	if err := idx.BulkLoad(values, nodes); err != nil {
		b.Fatalf("BulkLoad: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Cardinality(values[i%n])
	}
}

// BenchmarkIndex_CardinalityString is the same probe for the string
// instantiation — the production key type of every Cypher btree index.
func BenchmarkIndex_CardinalityString(b *testing.B) {
	const n = 100_000
	values := make([]string, n)
	nodes := make([]graph.NodeID, n)
	for i := range values {
		values[i] = "user-" + string(rune('a'+i%26)) + "-" + itoaPad(i)
		nodes[i] = graph.NodeID(uint64(i)) //nolint:gosec // i < 1e5
	}
	idx := New[string]()
	if err := idx.BulkLoad(values, nodes); err != nil {
		b.Fatalf("BulkLoad: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Cardinality(values[i%n])
	}
}

// itoaPad renders i as a fixed-width decimal so benchmark keys sort
// without allocating via fmt.
func itoaPad(i int) string {
	var buf [8]byte
	for k := len(buf) - 1; k >= 0; k-- {
		buf[k] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[:])
}

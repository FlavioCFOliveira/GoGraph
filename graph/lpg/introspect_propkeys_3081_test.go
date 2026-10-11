package lpg

// introspect_propkeys_3081_test.go — rmp #3081: PropertyKeysInUse walks the edge
// property columns column-major and reads presence only. These tests pin that it
// returns exactly the set the slot-major value walk returned, and that its
// STORED-state contract (uncommitted writes are visible) is unchanged.
//
// Layer: short.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// propertyKeysInUseSlotMajor is the pre-#3081 body of PropertyKeysInUse, kept
// verbatim as the reference the column-major walk must agree with.
func propertyKeysInUseSlotMajor[N comparable, W any](g *Graph[N, W]) []string {
	tombstoned := g.tombstoneActive.Load() != 0
	seen := make(map[PropertyKeyID]struct{})
	for i := range g.nodePropShards {
		sh := &g.nodePropShards[i]
		sh.mu.RLock()
		for id, bag := range sh.m {
			if bag.len() == 0 {
				continue
			}
			if tombstoned && g.IsTombstonedStored(id) {
				continue
			}
			bag.forEach(func(pk PropertyKeyID, _ PropertyValue) { seen[pk] = struct{}{} })
		}
		sh.mu.RUnlock()
	}
	maxID := uint64(g.adj.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		srcID := graph.NodeID(id)
		block := asEdgePropCols(g.adj.LoadEntryAux(srcID))
		if block == nil {
			continue
		}
		nbs, _ := g.adj.LoadEntry(srcID)
		n := minInt(len(nbs), block.lenOrZero())
		for i := 0; i < n; i++ {
			if !g.edgeEndpointLive(edgeKey{src: srcID, dst: nbs[i]}) {
				continue
			}
			block.forEachAt(i, func(pk PropertyKeyID, _ PropertyValue) { seen[pk] = struct{}{} })
		}
	}
	out := make([]string, 0, len(seen))
	for pk := range seen {
		if name, ok := g.pkeys.Resolve(pk); ok {
			out = append(out, name)
		}
	}
	return out
}

func sortedKeys(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func assertKeys(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := sortedKeys(got), sortedKeys(want)
	if fmt.Sprint(g) != fmt.Sprint(w) {
		t.Fatalf("%s: PropertyKeysInUse = %v, want %v", label, g, w)
	}
}

// TestPropertyKeysInUse_ColumnMajorMatchesSlotMajor_3081 builds random graphs
// whose edge columns take every physical form — dense with no validity bitmap,
// dense with one, sparse — and whose keys are borne only by edges with a
// tombstoned endpoint, and checks the two walks agree after every phase.
func TestPropertyKeysInUse_ColumnMajorMatchesSlotMajor_3081(t *testing.T) {
	t.Parallel()
	kinds := []func(i int) PropertyValue{
		func(i int) PropertyValue { return Int64Value(int64(i)) },
		func(i int) PropertyValue { return Float64Value(float64(i) / 3) },
		func(i int) PropertyValue { return BoolValue(i%2 == 0) },
		func(i int) PropertyValue { return StringValue(fmt.Sprintf("s%d", i)) },
	}
	for seed := uint64(1); seed <= 6; seed++ {
		r := rand.New(rand.NewPCG(seed, 3081)) //nolint:gosec // G404: math/rand/v2 PCG seeded from the test's own parameter — this test asserts a reproducible sequence, which a CSPRNG would destroy.
		g := New[string, float64](adjlist.Config{})
		const nodes = 120
		name := func(i int) string { return fmt.Sprintf("n%d", i) }
		for i := 0; i < nodes; i++ {
			if err := g.AddNode(name(i)); err != nil {
				t.Fatal(err)
			}
		}
		type pair struct{ s, d string }
		var pairs []pair
		// A hub with a high out-degree gives sparse and validity-bitmap columns.
		for d := 1; d < 100; d++ {
			pairs = append(pairs, pair{name(0), name(d)})
		}
		for e := 0; e < 400; e++ {
			pairs = append(pairs, pair{name(r.IntN(nodes)), name(r.IntN(nodes))})
		}
		for _, p := range pairs {
			if err := g.AddEdge(p.s, p.d, 1); err != nil {
				t.Fatal(err)
			}
		}
		for i, p := range pairs {
			for k := 0; k < 8; k++ {
				// key k is set on a fraction of edges that shrinks with k, so the
				// rare keys land in sparse columns.
				if r.IntN(1<<k) != 0 {
					continue
				}
				key := fmt.Sprintf("e%d", k)
				if err := g.SetEdgeProperty(p.s, p.d, key, kinds[k%len(kinds)](i)); err != nil {
					t.Fatal(err)
				}
			}
			if r.IntN(10) == 0 {
				_ = g.SetNodeProperty(p.s, fmt.Sprintf("v%d", r.IntN(4)), Int64Value(int64(i)))
			}
		}
		check := func(phase string) {
			t.Helper()
			assertKeys(t, fmt.Sprintf("seed %d %s", seed, phase), g.PropertyKeysInUse(), propertyKeysInUseSlotMajor(g))
		}
		check("populated")
		// A key borne by exactly one edge, then that edge's endpoint removed.
		if err := g.SetEdgeProperty(name(0), name(99), "only99", Int64Value(1)); err != nil {
			t.Fatal(err)
		}
		check("only99 set")
		for _, p := range pairs {
			if r.IntN(4) == 0 {
				_ = g.DelEdgeProperty(p.s, p.d, fmt.Sprintf("e%d", r.IntN(8)))
			}
		}
		check("deleted")
		if err := g.RemoveNode(name(99)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 15; i++ {
			_ = g.RemoveNode(name(1 + r.IntN(nodes-1)))
		}
		check("tombstoned")
		for _, k := range sortedKeys(g.PropertyKeysInUse()) {
			if k == "only99" {
				t.Fatalf("seed %d: only99 survived the removal of its edge's endpoint", seed)
			}
		}
	}
}

// TestPropertyKeysInUse_UncommittedWritesVisible_3081 pins the STORED-state
// contract under an open versioned transaction: an uncommitted set adds its key,
// and an uncommitted delete or endpoint removal that leaves a key on no live
// element takes it out — all before the transaction ends.
func TestPropertyKeysInUse_UncommittedWritesVisible_3081(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{})
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("c", "d", 1); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty("a", "base", Int64Value(1)); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "committed base", g.PropertyKeysInUse(), []string{"base"})

	tx := g.BeginVersionedTx()
	defer g.EndVersionedTx(tx)
	step := func(label string, fn func(w WriteView[string, float64]) error, want []string) {
		t.Helper()
		if err := g.ApplyInVersionedTx(context.Background(), tx, func(tx WriteTx) error {
			return fn(g.Writer(tx))
		}); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		assertKeys(t, label, g.PropertyKeysInUse(), want)
	}
	step("uncommitted node+edge set", func(w WriteView[string, float64]) error {
		if err := w.SetNodeProperty("b", "nk", StringValue("x")); err != nil {
			return err
		}
		if err := w.SetEdgeProperty("a", "b", "ek", Int64Value(7)); err != nil {
			return err
		}
		return w.SetEdgeProperty("c", "d", "ek2", BoolValue(true))
	}, []string{"base", "nk", "ek", "ek2"})
	step("uncommitted removes to zero", func(w WriteView[string, float64]) error {
		if err := w.DelNodeProperty("b", "nk"); err != nil {
			return err
		}
		return w.DelEdgeProperty("a", "b", "ek")
	}, []string{"base", "ek2"})
	step("uncommitted endpoint removal", func(w WriteView[string, float64]) error {
		_, err := w.RemoveNode("d")
		return err
	}, []string{"base"})
	step("uncommitted delete of the last base", func(w WriteView[string, float64]) error {
		return w.DelNodeProperty("a", "base")
	}, []string{})
}

// TestPropBag_ForEachKeyMatchesForEach_3081 pins that the key-only walk visits
// exactly forEach's keys, in forEach's order, for every value kind and for both
// the stream and the promoted map form.
func TestPropBag_ForEachKeyMatchesForEach_3081(t *testing.T) {
	t.Parallel()
	vals := []PropertyValue{
		Int64Value(-1), Int64Value(1 << 40), Float64Value(2.5), BoolValue(true), BoolValue(false),
		StringValue(""), StringValue("a longer string value"),
	}
	for n := 0; n <= 80; n += 7 {
		var b propBag
		for i := 0; i < n; i++ {
			b.set(PropertyKeyID(i*3+1), vals[i%len(vals)])
		}
		var want, got []PropertyKeyID
		b.forEach(func(k PropertyKeyID, _ PropertyValue) { want = append(want, k) })
		b.forEachKey(func(k PropertyKeyID) { got = append(got, k) })
		if b.m != nil {
			sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("n=%d (map=%v): forEachKey = %v, forEach keys = %v", n, b.m != nil, got, want)
		}
	}
}

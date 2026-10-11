package cypher

// rel_source_slots_equiv_test.go — the engine-level equivalence gate for rmp
// #3089.
//
// A ReadView reads a pair's type union and coalesced properties through a cached
// per-source slot index instead of scanning the source's slots per read, and the
// relationship builder reads a non-Cypher edge through those per-pair reads. The index must return exactly what the scanning
// per-pair surfaces return. The oracle here is those scanning surfaces, read
// directly from the graph (lpg.Graph.EdgeLabels / EdgeProperties /
// GetEdgeProperty / EdgeHasProperty for a Go-API slot; the by-handle stores for
// a Cypher-created slot), on random multigraphs whose hub is above the index's
// degree floor and mixes Go-API multi-type pairs, per-slot properties, Cypher
// parallel edges and deletes — at the present instant and through a read
// transaction pinned before further writes.

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const slotsEquivTargets = 6

var slotsEquivTypes = []string{"KA", "KB", "KC"}

func slotsEquivFixture(t *testing.T) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	g.SetIndexManager(index.NewManager())
	if err := g.AddNode("h"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel("h", "H"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < slotsEquivTargets; i++ {
		k := fmt.Sprintf("t%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "k", lpg.StringValue(k)); err != nil {
			t.Fatal(err)
		}
	}
	return g, NewEngine(g)
}

// slotsEquivMutate applies n random writes out of the hub. Every Go-API pair is
// typed, so every relationship has a type to compare.
func slotsEquivMutate(t *testing.T, rng *rand.Rand, g *lpg.Graph[string, float64], eng *Engine, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		tk := fmt.Sprintf("t%d", rng.Intn(slotsEquivTargets))
		typ := slotsEquivTypes[rng.Intn(len(slotsEquivTypes))]
		switch op := rng.Intn(10); {
		case op < 3: // Go API: untyped slot, then name the pair's type.
			if err := g.AddEdge("h", tk, 1); err != nil {
				t.Fatal(err)
			}
			if err := g.SetEdgeLabel("h", tk, typ); err != nil {
				t.Fatal(err)
			}
		case op < 5: // Go API: typed slot carrying a per-slot property.
			if err := g.AddEdgeLabeledWithProperty("h", tk, 1, typ, "q", lpg.Int64Value(int64(rng.Intn(100)))); err != nil {
				t.Fatal(err)
			}
		case op < 6: // Go API: per-pair property fan-out (a no-op on an absent pair).
			if err := g.SetEdgeProperty("h", tk, "p", lpg.Int64Value(int64(rng.Intn(100)))); err != nil {
				t.Fatal(err)
			}
		case op < 9: // Cypher parallel edge.
			mustRun(t, eng, fmt.Sprintf(
				"MATCH (h:H), (x:P {k: '%s'}) CREATE (h)-[:%s {p: %d}]->(x)", tk, typ, rng.Intn(100)))
		default: // Cypher delete of one relationship of the pair.
			mustRun(t, eng, fmt.Sprintf(
				"MATCH (:H)-[r]->(:P {k: '%s'}) WITH r LIMIT 1 DELETE r", tk))
		}
	}
}

// slotsEquivOracle renders, from the graph's scanning per-pair surfaces and the
// by-handle stores, the sorted rows each query must return.
func slotsEquivOracle(t *testing.T, g *lpg.Graph[string, float64]) (whole, lazyP, presQ []string) {
	t.Helper()
	view := g.ReadAt(nil)
	m := view.AdjList().Mapper()
	hID, ok := m.Lookup("h")
	if !ok {
		t.Fatal("hub missing")
	}
	ev := g.EntryViewAsOf(hID, nil)
	if len(ev.Handles) != len(ev.Neighbours) {
		t.Fatalf("handle column %d != neighbours %d", len(ev.Handles), len(ev.Neighbours))
	}
	toMap := func(raw map[string]lpg.PropertyValue) expr.MapValue {
		out := expr.MapValue{}
		for k, v := range raw {
			out[k] = lpgPropToExpr(v)
		}
		return out
	}
	for i, nb := range ev.Neighbours {
		tk, ok := m.Resolve(nb)
		if !ok {
			t.Fatalf("neighbour %d unresolved", nb)
		}
		h := ev.Handles[i]
		var typ string
		var props expr.MapValue
		var qPresent bool
		if byH := g.EdgeLabelsByHandle("h", tk, h); len(byH) > 0 {
			typ = pickEdgeType(byH, nil)
			props = toMap(g.EdgePropertiesByHandle("h", tk, h))
			v, has := props["q"]
			qPresent = has && !expr.IsNull(v)
		} else {
			typ = pickEdgeType(g.EdgeLabels("h", tk), nil)
			props = toMap(g.EdgeProperties("h", tk))
			qPresent = g.EdgeHasProperty("h", tk, "q")
		}
		pv, has := props["p"]
		if !has {
			pv = expr.Null
		}
		whole = append(whole, fmt.Sprintf("%q|%q|%s", tk, typ, canonValue(props)))
		lazyP = append(lazyP, fmt.Sprintf("%q|%s", tk, pv.String()))
		presQ = append(presQ, fmt.Sprintf("%q|%t", tk, qPresent))
	}
	sort.Strings(whole)
	sort.Strings(lazyP)
	sort.Strings(presQ)
	return whole, lazyP, presQ
}

const (
	slotsEquivQWhole = "MATCH (:H)-[r]->(x) RETURN x.k, type(r), properties(r)"
	slotsEquivQLazy  = "MATCH (:H)-[r]->(x) RETURN x.k, r.p"
	slotsEquivQPres  = "MATCH (:H)-[r]->(x) RETURN x.k, r.q IS NOT NULL"
)

func slotsEquivRows(t *testing.T, run func(q string) []string) (whole, lazyP, presQ []string) {
	t.Helper()
	whole, lazyP, presQ = run(slotsEquivQWhole), run(slotsEquivQLazy), run(slotsEquivQPres)
	sort.Strings(whole)
	sort.Strings(lazyP)
	sort.Strings(presQ)
	return whole, lazyP, presQ
}

// canonValue renders v with a map's keys sorted, so two equal maps render
// identically whatever their iteration order.
func canonValue(v expr.Value) string {
	m, ok := v.(expr.MapValue)
	if !ok {
		return v.String()
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + m[k].String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// canonRows drains res, rendering each row with [canonValue].
func canonRows(t *testing.T, res *Result, err error, q string) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("run(%s): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		parts := make([]string, len(res.Columns()))
		for i := range parts {
			parts[i] = canonValue(res.ValueAt(i))
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain(%s): %v", q, err)
	}
	return out
}

func txRows(t *testing.T, tx *ExplicitTx, q string) []string {
	t.Helper()
	res, err := tx.Exec(q, nil)
	return canonRows(t, res, err, q)
}

func engRows(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	return canonRows(t, res, err, q)
}

func slotsEquivCompare(t *testing.T, stage string, gotW, gotL, gotP, wantW, wantL, wantP []string) {
	t.Helper()
	for _, c := range []struct {
		name      string
		got, want []string
	}{{"whole", gotW, wantW}, {"lazy r.p", gotL, wantL}, {"presence r.q", gotP, wantP}} {
		if strings.Join(c.got, "\n") != strings.Join(c.want, "\n") {
			t.Fatalf("%s %s:\n got  %v\n want %v", stage, c.name, c.got, c.want)
		}
	}
}

func TestRelSourceSlots_EquivalentToPerPairScan(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed)) //nolint:gosec // G404: math/rand seeded from the test's own parameter — this test asserts a reproducible sequence, which a CSPRNG would destroy.
			g, eng := slotsEquivFixture(t)
			slotsEquivMutate(t, rng, g, eng, 90)

			// The hub must be above the index's degree floor, or the indexed path
			// is never taken and the comparison proves nothing about it.
			// slotsEquivMinDegree mirrors graph/lpg's unexported indexing floor
			// (minSourceSlotsDegree = 32); graph/lpg's own tests assert the index
			// is built at this degree.
			const slotsEquivMinDegree = 32
			hID, _ := g.ReadAt(nil).AdjList().Mapper().Lookup("h")
			if deg := len(g.EntryViewAsOf(hID, nil).Neighbours); deg < slotsEquivMinDegree+2 {
				t.Fatalf("hub degree %d below the index floor %d: fixture proves nothing", deg, slotsEquivMinDegree)
			}

			wantW, wantL, wantP := slotsEquivOracle(t, g)
			gotW, gotL, gotP := slotsEquivRows(t, func(q string) []string { return engRows(t, eng, q) })
			slotsEquivCompare(t, "present", gotW, gotL, gotP, wantW, wantL, wantP)

			// Pin a read transaction, write more, and read through the pinned
			// snapshot: the index must describe the snapshot's entry version.
			tx, err := eng.BeginReadTx(context.Background())
			if err != nil {
				t.Fatalf("BeginReadTx: %v", err)
			}
			slotsEquivMutate(t, rng, g, eng, 30)
			gotW, gotL, gotP = slotsEquivRows(t, func(q string) []string { return txRows(t, tx, q) })
			if err := tx.Rollback(); err != nil {
				t.Fatalf("Rollback: %v", err)
			}
			slotsEquivCompare(t, "pinned snapshot", gotW, gotL, gotP, wantW, wantL, wantP)

			wantW, wantL, wantP = slotsEquivOracle(t, g)
			gotW, gotL, gotP = slotsEquivRows(t, func(q string) []string { return engRows(t, eng, q) })
			slotsEquivCompare(t, "after writes", gotW, gotL, gotP, wantW, wantL, wantP)
		})
	}
}

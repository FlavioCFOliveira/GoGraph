package cypher_test

// scan_index_btree_inclusive_test.go — T616: range query with INCLUSIVE bounds
// on City nodes with a "population" int64 property.
//
// The engine evaluates range predicates via Selection over NodeByLabelScan.
// The btree index is installed to exercise the index infrastructure; the
// engine does not yet rewrite range predicates to NodeByIndexRangeScan, so
// correctness is validated without asserting the plan shape.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// cityEntry holds test data for one City node.
type cityEntry struct {
	name       string
	population int64
}

// buildCityGraph creates a graph with the given cities, all labeled "City",
// with "name" and "population" int64 properties.
func buildCityGraph(t *testing.T, cities []cityEntry) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i, c := range cities {
		key := fmt.Sprintf("city%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "City"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(c.name)); err != nil {
			t.Fatalf("SetNodeProperty name: %v", err)
		}
		if err := g.SetNodeProperty(key, "population", lpg.Int64Value(c.population)); err != nil {
			t.Fatalf("SetNodeProperty population: %v", err)
		}
	}
	return g
}

// installPopulationIndex creates and populates a btree.Index[int64] named
// "city_population_btree" on the "population" property of all City nodes.
func installPopulationIndex(g *lpg.Graph[string, float64]) {
	idx := btree.New[int64]()
	if err := g.IndexManager().CreateIndex("city_population_btree", idx); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			panic(fmt.Sprintf("installPopulationIndex CreateIndex: %v", err))
		}
		return
	}
	g.AdjList().Mapper().Walk(func(id graph.NodeID, nodeKey string) bool {
		if !g.HasNodeLabel(nodeKey, "City") {
			return true
		}
		pv, ok := g.GetNodeProperty(nodeKey, "population")
		if !ok {
			return true
		}
		if iv, ok2 := pv.Int64(); ok2 {
			idx.Insert(iv, id)
		}
		return true
	})
}

// citiesInRange runs the inclusive range query and returns the set of city
// names returned.
func citiesInRange(t *testing.T, eng *cypher.Engine, lo, hi int64) map[string]bool {
	t.Helper()
	q := fmt.Sprintf(
		"MATCH (n:City) WHERE n.population >= %d AND n.population <= %d RETURN n.name",
		lo, hi,
	)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	names := map[string]bool{}
	for res.Next() {
		rec := res.Record()
		sv, ok := rec["n.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("n.name: expected StringValue, got %T", rec["n.name"])
		}
		names[string(sv)] = true
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	return names
}

// testCities is the fixture used by T616 and T625.
var testCities = []cityEntry{
	{"Alfa", 100_000},
	{"Beta", 200_000},
	{"Gamma", 300_000},
	{"Delta", 400_000},
	{"Epsilon", 500_000},
	{"Zeta", 600_000},
	{"Eta", 700_000},
	{"Theta", 800_000},
	{"Iota", 900_000},
	{"Kappa", 1_000_000},
}

// newCityEngine creates a city graph engine with the population btree index
// installed. NewEngine is called first to ensure the index manager is
// initialised before installPopulationIndex runs.
func newCityEngine(t *testing.T) (*lpg.Graph[string, float64], *cypher.Engine) {
	t.Helper()
	g := buildCityGraph(t, testCities)
	eng := cypher.NewEngine(g) // installs index.Manager on g
	installPopulationIndex(g)
	return g, eng
}

// TestBTreeInclusive_MiddleRange verifies that inclusive bounds [500k, 800k]
// return the four cities whose population falls within that range.
func TestBTreeInclusive_MiddleRange(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)
	got := citiesInRange(t, eng, 500_000, 800_000)

	want := []string{"Epsilon", "Zeta", "Eta", "Theta"}
	if len(got) != len(want) {
		t.Fatalf("[500k,800k]: want %v, got %v", want, got)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("[500k,800k]: missing %q; full result: %v", w, got)
		}
	}
}

// TestBTreeInclusive_BoundaryInclusion verifies that boundary values are
// included when using >= and <=.
func TestBTreeInclusive_BoundaryInclusion(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)

	// Exact boundary: only Epsilon (500k) and Theta (800k) are boundaries.
	got := citiesInRange(t, eng, 500_000, 800_000)
	if !got["Epsilon"] {
		t.Errorf("inclusive lower bound 500_000: Epsilon must be included")
	}
	if !got["Theta"] {
		t.Errorf("inclusive upper bound 800_000: Theta must be included")
	}
}

// TestBTreeInclusive_FullRange verifies that [100k, 1M] returns all 10 cities.
func TestBTreeInclusive_FullRange(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)
	got := citiesInRange(t, eng, 100_000, 1_000_000)
	if len(got) != len(testCities) {
		t.Errorf("full range: want %d cities, got %d (%v)", len(testCities), len(got), got)
	}
}

// TestBTreeInclusive_EmptyRange verifies that a range with no matching cities
// returns zero rows.
func TestBTreeInclusive_EmptyRange(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)

	// All cities are between 100k and 1M, so [1_500_000, 2_000_000] is empty.
	got := citiesInRange(t, eng, 1_500_000, 2_000_000)
	if len(got) != 0 {
		t.Errorf("empty range: want 0 cities, got %d (%v)", len(got), got)
	}
}

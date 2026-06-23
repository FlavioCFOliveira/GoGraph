package query_test

// index_seek_test.go — black-box correctness for index-backed property and
// range predicates, using REAL bound indexes (the production maintenance path)
// registered on the graph's index.Manager (task #1651).
//
// Each test asserts the index path returns a set IDENTICAL to the scan path
// (the same query against a graph with no index manager), the central
// acceptance criterion: an index is a transparent optimisation, never a
// different answer. A separate fallback test pins that a property with no
// covering index still scans correctly even while a manager and other indexes
// are present.

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// ----- shared fixture -------------------------------------------------------

const (
	fxLabelPerson = "Person"
	fxPropDept    = "dept"   // string
	fxPropAge     = "age"    // int64
	fxPropSalary  = "salary" // float64
	fxPropActive  = "active" // bool
)

var fxDepts = []string{"Engineering", "Sales", "Marketing", "Finance"}

// buildEmployeeGraph builds a deterministic employee directory of n :Person
// nodes (key "p<i>"), each carrying a string dept, an int64 age, a float64
// salary, and a bool active flag drawn from a seeded RNG. Every value is a
// closed form of i+seed, so the data shape is reproducible. It returns the
// graph and its CSR snapshot; no index manager is attached.
func buildEmployeeGraph(tb testing.TB, n int, seed int64) (*lpg.Graph[string, int64], *csr.CSR[int64]) {
	tb.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	rng := rand.New(rand.NewSource(seed))
	for i := range n {
		key := fmt.Sprintf("p%d", i)
		if err := g.SetNodeLabel(key, fxLabelPerson); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, fxPropDept, lpg.StringValue(fxDepts[rng.Intn(len(fxDepts))])); err != nil {
			tb.Fatalf("SetNodeProperty dept: %v", err)
		}
		if err := g.SetNodeProperty(key, fxPropAge, lpg.Int64Value(int64(21+rng.Intn(45)))); err != nil {
			tb.Fatalf("SetNodeProperty age: %v", err)
		}
		if err := g.SetNodeProperty(key, fxPropSalary, lpg.Float64Value(float64(35000+rng.Intn(145000)))); err != nil {
			tb.Fatalf("SetNodeProperty salary: %v", err)
		}
		if err := g.SetNodeProperty(key, fxPropActive, lpg.BoolValue(rng.Intn(2) == 0)); err != nil {
			tb.Fatalf("SetNodeProperty active: %v", err)
		}
	}
	c := csr.BuildFromAdjList(g.AdjList())
	return g, c
}

// attachHashIndex builds a bound hash.Index[V] over (label, prop), backfills it
// from the live graph, and registers it on a fresh manager set on g. project
// extracts the V key from a node's PropertyValue; toAny is its inverse used by
// the binding to read change payloads. The closures mirror the cypher engine's
// newBoundNodeHashIndex so the test exercises the same maintenance path.
func attachHashIndex[V comparable](
	tb testing.TB,
	g *lpg.Graph[string, int64],
	label, prop string,
	name string,
	project func(lpg.PropertyValue) (V, bool),
) {
	tb.Helper()
	labelID := uint32(g.Registry().Intern(label))
	propID := uint32(g.PropertyKeys().Intern(prop))
	mapper := g.AdjList().Mapper()
	nodeIdx := g.NodeIndex()

	current := func(id graph.NodeID) (V, bool) {
		var zero V
		if g.IsTombstoned(id) {
			return zero, false
		}
		key, ok := mapper.Resolve(id)
		if !ok {
			return zero, false
		}
		pv, ok := g.GetNodeProperty(key, prop)
		if !ok {
			return zero, false
		}
		return project(pv)
	}

	idx, err := indexhash.NewBound(indexhash.Binding[V]{
		PropertyID: propID,
		LabelID:    labelID,
		Label:      label,
		Property:   prop,
		Project: func(v any) (V, bool) {
			var zero V
			pv, ok := v.(lpg.PropertyValue)
			if !ok {
				return zero, false
			}
			return project(pv)
		},
		Eligible: func(id graph.NodeID) bool {
			return !g.IsTombstoned(id) && nodeIdx.Has(labelID, id)
		},
		CurrentValue: current,
	})
	if err != nil {
		tb.Fatalf("NewBound hash %s: %v", name, err)
	}

	// Backfill from the live graph.
	mapper.Walk(func(id graph.NodeID, key string) bool {
		if g.IsTombstoned(id) || !g.HasNodeLabel(key, label) {
			return true
		}
		pv, ok := g.GetNodeProperty(key, prop)
		if !ok {
			return true
		}
		if v, ok := project(pv); ok {
			idx.Insert(v, id)
		}
		return true
	})

	mgr := g.IndexManager()
	if mgr == nil {
		mgr = index.NewManager()
		g.SetIndexManager(mgr)
	}
	if err := mgr.CreateIndex(name, idx); err != nil {
		tb.Fatalf("CreateIndex %s: %v", name, err)
	}
}

// attachBTreeIndex builds a bound btree.Index[V] over (label, prop), backfills
// it via BulkLoad, and registers it on a fresh manager set on g.
func attachBTreeIndex[V interface {
	comparable
	~string | ~int64 | ~float64
}](
	tb testing.TB,
	g *lpg.Graph[string, int64],
	label, prop string,
	name string,
	project func(lpg.PropertyValue) (V, bool),
) {
	tb.Helper()
	labelID := uint32(g.Registry().Intern(label))
	propID := uint32(g.PropertyKeys().Intern(prop))
	mapper := g.AdjList().Mapper()
	nodeIdx := g.NodeIndex()

	idx, err := indexbtree.NewBound(indexbtree.Binding[V]{
		PropertyID: propID,
		LabelID:    labelID,
		Label:      label,
		Property:   prop,
		Project: func(v any) (V, bool) {
			var zero V
			pv, ok := v.(lpg.PropertyValue)
			if !ok {
				return zero, false
			}
			return project(pv)
		},
		Eligible: func(id graph.NodeID) bool {
			return !g.IsTombstoned(id) && nodeIdx.Has(labelID, id)
		},
		CurrentValue: func(id graph.NodeID) (V, bool) {
			var zero V
			if g.IsTombstoned(id) {
				return zero, false
			}
			key, ok := mapper.Resolve(id)
			if !ok {
				return zero, false
			}
			pv, ok := g.GetNodeProperty(key, prop)
			if !ok {
				return zero, false
			}
			return project(pv)
		},
	})
	if err != nil {
		tb.Fatalf("NewBound btree %s: %v", name, err)
	}

	var values []V
	var nodes []graph.NodeID
	mapper.Walk(func(id graph.NodeID, key string) bool {
		if g.IsTombstoned(id) || !g.HasNodeLabel(key, label) {
			return true
		}
		pv, ok := g.GetNodeProperty(key, prop)
		if !ok {
			return true
		}
		if v, ok := project(pv); ok {
			values = append(values, v)
			nodes = append(nodes, id)
		}
		return true
	})
	if err := idx.BulkLoad(values, nodes); err != nil {
		tb.Fatalf("BulkLoad %s: %v", name, err)
	}

	mgr := g.IndexManager()
	if mgr == nil {
		mgr = index.NewManager()
		g.SetIndexManager(mgr)
	}
	if err := mgr.CreateIndex(name, idx); err != nil {
		tb.Fatalf("CreateIndex %s: %v", name, err)
	}
}

// projectors for the four scalar kinds.
func projDeptString(pv lpg.PropertyValue) (string, bool) {
	if pv.Kind() != lpg.PropString {
		return "", false
	}
	return pv.String()
}
func projAgeInt64(pv lpg.PropertyValue) (int64, bool) {
	if pv.Kind() != lpg.PropInt64 {
		return 0, false
	}
	return pv.Int64()
}
func projSalaryFloat64(pv lpg.PropertyValue) (float64, bool) {
	if pv.Kind() != lpg.PropFloat64 {
		return 0, false
	}
	return pv.Float64()
}
func projActiveBool(pv lpg.PropertyValue) (bool, bool) {
	if pv.Kind() != lpg.PropBool {
		return false, false
	}
	return pv.Bool()
}

// collectSorted runs the pattern and returns its results sorted, for a
// set-equality comparison independent of iteration order.
func collectSorted(p *query.Pattern[string, int64]) []string {
	got := p.Collect()
	sort.Strings(got)
	return got
}

// ----- equality identity: index path == scan path --------------------------

func TestSeek_EqualityMatchesScan_AllKinds(t *testing.T) {
	t.Parallel()

	const n, seed = 4000, 7

	cases := []struct {
		name   string
		attach func(tb testing.TB, g *lpg.Graph[string, int64])
		// predFor derives the equality predicate from the (deterministic) graph,
		// so even an exact float equality is pinned to a value a node actually
		// carries and the match set is never vacuous for a fixed seed.
		predFor func(g *lpg.Graph[string, int64]) query.Predicate[string, int64]
	}{
		{
			name: "string dept=Engineering",
			attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
				attachHashIndex(tb, g, fxLabelPerson, fxPropDept, "person_dept_hash", projDeptString)
			},
			predFor: func(*lpg.Graph[string, int64]) query.Predicate[string, int64] {
				return query.WithProperty[string, int64](fxPropDept, lpg.StringValue("Engineering"))
			},
		},
		{
			name: "int64 age=40",
			attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
				attachHashIndex(tb, g, fxLabelPerson, fxPropAge, "person_age_hash", projAgeInt64)
			},
			predFor: func(*lpg.Graph[string, int64]) query.Predicate[string, int64] {
				return query.WithProperty[string, int64](fxPropAge, lpg.Int64Value(40))
			},
		},
		{
			name: "bool active=true",
			attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
				attachHashIndex(tb, g, fxLabelPerson, fxPropActive, "person_active_hash", projActiveBool)
			},
			predFor: func(*lpg.Graph[string, int64]) query.Predicate[string, int64] {
				return query.WithProperty[string, int64](fxPropActive, lpg.BoolValue(true))
			},
		},
		{
			name: "float64 salary (value from p0)",
			attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
				attachHashIndex(tb, g, fxLabelPerson, fxPropSalary, "person_salary_hash", projSalaryFloat64)
			},
			predFor: func(g *lpg.Graph[string, int64]) query.Predicate[string, int64] {
				pv, ok := g.GetNodeProperty("p0", fxPropSalary)
				if !ok {
					t.Fatalf("p0 has no salary")
				}
				return query.WithProperty[string, int64](fxPropSalary, pv)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Scan oracle: no index manager.
			gScan, cScan := buildEmployeeGraph(t, n, seed)
			pred := tc.predFor(gScan)
			want := collectSorted(query.New(gScan, cScan).Match().
				Vertex(query.WithLabel[string, int64](fxLabelPerson), pred))

			// Index path: identical graph, with a covering index.
			gIdx, cIdx := buildEmployeeGraph(t, n, seed)
			tc.attach(t, gIdx)
			got := collectSorted(query.New(gIdx, cIdx).Match().
				Vertex(query.WithLabel[string, int64](fxLabelPerson), pred))

			if !equalStrings(got, want) {
				t.Fatalf("index path != scan path\n got (%d): %v\nwant (%d): %v",
					len(got), trunc(got), len(want), trunc(want))
			}
			if len(want) == 0 {
				t.Fatalf("fixture produced an empty match set; the test would be vacuous")
			}
		})
	}
}

// ----- range identity: btree index path == scan path -----------------------

func TestSeek_RangeMatchesScan(t *testing.T) {
	t.Parallel()

	const n, seed = 4000, 11

	cases := []struct {
		name   string
		lo, hi lpg.PropertyValue
	}{
		{"age [30,40]", lpg.Int64Value(30), lpg.Int64Value(40)},
		{"age [21,21]", lpg.Int64Value(21), lpg.Int64Value(21)},
		{"age [60,65]", lpg.Int64Value(60), lpg.Int64Value(65)},
		{"age [100,200] empty", lpg.Int64Value(100), lpg.Int64Value(200)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gScan, cScan := buildEmployeeGraph(t, n, seed)
			want := collectSorted(query.New(gScan, cScan).Match().
				Vertex(query.WithLabel[string, int64](fxLabelPerson),
					query.WithRange[string, int64](fxPropAge, tc.lo, tc.hi)))

			gIdx, cIdx := buildEmployeeGraph(t, n, seed)
			attachBTreeIndex(t, gIdx, fxLabelPerson, fxPropAge, "person_age_btree", projAgeInt64)
			got := collectSorted(query.New(gIdx, cIdx).Match().
				Vertex(query.WithLabel[string, int64](fxLabelPerson),
					query.WithRange[string, int64](fxPropAge, tc.lo, tc.hi)))

			if !equalStrings(got, want) {
				t.Fatalf("range index path != scan path\n got (%d): %v\nwant (%d): %v",
					len(got), trunc(got), len(want), trunc(want))
			}
		})
	}
}

// ----- index-miss fallback --------------------------------------------------

// TestSeek_MissFallback confirms a property with NO covering index still scans
// correctly even while a manager and an unrelated index are present: the dept
// index is registered, but the query filters on age, so age must fall back to
// the scan and still match the no-index oracle.
func TestSeek_MissFallback(t *testing.T) {
	t.Parallel()

	const n, seed = 2000, 3

	gScan, cScan := buildEmployeeGraph(t, n, seed)
	want := collectSorted(query.New(gScan, cScan).Match().
		Vertex(query.WithLabel[string, int64](fxLabelPerson),
			query.WithProperty[string, int64](fxPropAge, lpg.Int64Value(40))))

	gIdx, cIdx := buildEmployeeGraph(t, n, seed)
	// Register a dept index — irrelevant to the age predicate.
	attachHashIndex(t, gIdx, fxLabelPerson, fxPropDept, "person_dept_hash", projDeptString)
	got := collectSorted(query.New(gIdx, cIdx).Match().
		Vertex(query.WithLabel[string, int64](fxLabelPerson),
			query.WithProperty[string, int64](fxPropAge, lpg.Int64Value(40))))

	if !equalStrings(got, want) {
		t.Fatalf("miss-fallback path != scan path\n got (%d): %v\nwant (%d): %v",
			len(got), trunc(got), len(want), trunc(want))
	}
	if len(want) == 0 {
		t.Fatalf("fixture produced an empty match set; the test would be vacuous")
	}
}

// TestSeek_NoLabelNoSeek confirms that a property predicate with no label
// sibling does NOT use a (label-scoped) index and still matches the scan: the
// second Vertex call filters on dept alone, with no label, so the bound dept
// index cannot cover it and the per-node scan must produce the same set.
func TestSeek_NoLabelNoSeek(t *testing.T) {
	t.Parallel()

	const n, seed = 2000, 5

	build := func(withIdx bool) []string {
		g, c := buildEmployeeGraph(t, n, seed)
		if withIdx {
			attachHashIndex(t, g, fxLabelPerson, fxPropDept, "person_dept_hash", projDeptString)
		}
		// First seed by label, then a SECOND Vertex with only a property
		// predicate (no label) — the label-scoped index must not be used.
		return collectSorted(query.New(g, c).Match().
			Vertex(query.WithLabel[string, int64](fxLabelPerson)).
			Vertex(query.WithProperty[string, int64](fxPropDept, lpg.StringValue("Engineering"))))
	}

	want := build(false)
	got := build(true)
	if !equalStrings(got, want) {
		t.Fatalf("no-label path differs with index present\n got: %v\nwant: %v", trunc(got), trunc(want))
	}
}

// ----- helpers --------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// trunc shortens a slice for failure messages.
func trunc(s []string) []string {
	if len(s) > 10 {
		return append(append([]string{}, s[:10]...), "...")
	}
	return s
}

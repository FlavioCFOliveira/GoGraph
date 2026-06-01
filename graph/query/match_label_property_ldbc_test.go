// Package query_test exercises the fluent MATCH engine with a synthetic
// 1000-node labelled property graph that proxies an LDBC SF1 Person
// workload for the short test layer.
//
// NOTE: A full LDBC SF1 integration test (loading the real Social
// Network Benchmark dataset via a shapegen LDBC loader) would belong
// to the soak layer (//go:build soak) and is deferred until the LDBC
// SF1 dataset loader is added to the shapegen package.
package query_test

import (
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// buildLDBCProxy builds a synthetic 1000-node graph that mimics the
// LDBC Person/Company label distribution used in LDBC SF1 queries:
//
//   - nodes 0..499  carry label "Person" and property firstName=<name>
//   - nodes 500..999 carry label "Company"
//   - nodes 0, 100, 200 have firstName="John" (the query targets)
//
// It returns the graph, the CSR snapshot, and a map from int key to
// NodeID for oracle construction.
func buildLDBCProxy(tb testing.TB) (
	g *lpg.Graph[int, int64],
	c *csr.CSR[int64],
	keyToNodeID map[int]graph.NodeID,
) {
	tb.Helper()

	const n = 1000
	g = lpg.New[int, int64](adjlist.Config{Directed: true})

	for i := range n {
		if err := g.AddNode(i); err != nil {
			tb.Fatalf("AddNode(%d): %v", i, err)
		}
	}

	for i := range 500 {
		if err := g.SetNodeLabel(i, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel(%d, Person): %v", i, err)
		}
		firstName := fmt.Sprintf("Person%d", i)
		if i == 0 || i == 100 || i == 200 {
			firstName = "John"
		}
		if err := g.SetNodeProperty(i, "firstName", lpg.StringValue(firstName)); err != nil {
			tb.Fatalf("SetNodeProperty(%d, firstName): %v", i, err)
		}
	}

	for i := 500; i < n; i++ {
		if err := g.SetNodeLabel(i, "Company"); err != nil {
			tb.Fatalf("SetNodeLabel(%d, Company): %v", i, err)
		}
	}

	c = csr.BuildFromAdjList(g.AdjList())

	keyToNodeID = make(map[int]graph.NodeID, n)
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key int) bool {
		keyToNodeID[key] = id
		return true
	})

	return g, c, keyToNodeID
}

// TestQuery_LabelProperty_LDBC verifies that a fluent
//
//	Match().Vertex(WithLabel("Person"), WithProperty("firstName", "John")).Collect()
//
// returns exactly the three nodes whose firstName is "John", matching
// the oracle built by iterating all nodes and checking both predicates
// independently.
func TestQuery_LabelProperty_LDBC(t *testing.T) {
	t.Parallel()

	g, c, keyToNodeID := buildLDBCProxy(t)
	e := query.New(g, c)

	// Engine query.
	rawResult := e.Match().
		Vertex(
			query.WithLabel[int, int64]("Person"),
			query.WithProperty[int, int64]("firstName", lpg.StringValue("John")),
		).
		Collect()

	// Sort int keys for deterministic comparison.
	sort.Ints(rawResult)

	// Build oracle: all nodes that carry label "Person" AND
	// have firstName == "John".
	var oracle []int
	for i := range 1000 {
		if !g.HasNodeLabel(i, "Person") {
			continue
		}
		v, ok := g.GetNodeProperty(i, "firstName")
		if !ok {
			continue
		}
		s, ok2 := v.String()
		if !ok2 {
			continue
		}
		if s == "John" {
			oracle = append(oracle, i)
		}
	}
	sort.Ints(oracle)

	// Verify cardinality first (fast failure signal).
	if got, want := len(rawResult), len(oracle); got != want {
		t.Fatalf("cardinality mismatch: got %d, want %d (oracle=%v)", got, want, oracle)
	}

	// Verify element equality.
	for idx := range oracle {
		if rawResult[idx] != oracle[idx] {
			t.Fatalf("result[%d] = %d, oracle[%d] = %d; full result=%v oracle=%v",
				idx, rawResult[idx], idx, oracle[idx], rawResult, oracle)
		}
	}

	// Verify the NodeIDs corresponding to the int keys are the expected
	// ones: keys 0, 100, 200 in the mapper.
	expectedKeys := []int{0, 100, 200}
	for _, k := range expectedKeys {
		nid, ok := keyToNodeID[k]
		if !ok {
			t.Fatalf("key %d not found in mapper", k)
		}
		found := false
		for _, res := range rawResult {
			if rid, ok2 := keyToNodeID[res]; ok2 && rid == nid {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected key %d (NodeID=%d) missing from result %v", k, nid, rawResult)
		}
	}

	// Engine cardinality must agree.
	cardPattern := e.Match().
		Vertex(
			query.WithLabel[int, int64]("Person"),
			query.WithProperty[int, int64]("firstName", lpg.StringValue("John")),
		)
	if got, want := cardPattern.Cardinality(), uint64(len(oracle)); got != want {
		t.Fatalf("Pattern.Cardinality() = %d, want %d", got, want)
	}
}

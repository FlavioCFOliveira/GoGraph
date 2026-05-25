package cypher_test

// scan_index_hash_int64_test.go — T611: hash index seek on int64 equality.
//
// Creates 10 Person nodes with integer ages, installs a hash.Index[int64] on
// "age", then queries MATCH (n:Person {age: 30}) RETURN n.name.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/index/hash"
	"gograph/graph/lpg"
)

// personAgeEntry holds test data for a single Person node.
type personAgeEntry struct {
	name string
	age  int64
}

// buildAgeGraph creates a graph with the given persons, all labeled "Person",
// with "name" and "age" properties set on each node.
func buildAgeGraph(t *testing.T, persons []personAgeEntry) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i, p := range persons {
		key := fmt.Sprintf("age_person%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(p.name)); err != nil {
			t.Fatalf("SetNodeProperty name: %v", err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(p.age)); err != nil {
			t.Fatalf("SetNodeProperty age: %v", err)
		}
	}
	return g
}

// installAgeIndex creates and populates a hash.Index[int64] named "age_hash" on
// the "age" property of all Person nodes.
func installAgeIndex(g *lpg.Graph[string, float64]) {
	idx := hash.New[int64]()
	if err := g.IndexManager().CreateIndex("age_hash", idx); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			panic(fmt.Sprintf("installAgeIndex CreateIndex: %v", err))
		}
		return
	}
	g.AdjList().Mapper().Walk(func(id graph.NodeID, nodeKey string) bool {
		if !g.HasNodeLabel(nodeKey, "Person") {
			return true
		}
		pv, ok := g.GetNodeProperty(nodeKey, "age")
		if !ok {
			return true
		}
		if iv, ok2 := pv.Int64(); ok2 {
			idx.Insert(iv, id)
		}
		return true
	})
}

// TestScanIndexHashInt64_Age30Found verifies that querying for age=30 via a
// hash index returns exactly the nodes whose age property is 30.
func TestScanIndexHashInt64_Age30Found(t *testing.T) {
	t.Parallel()

	persons := []personAgeEntry{
		{"Alice", 30},
		{"Bob", 25},
		{"Carol", 35},
		{"Dave", 30},
		{"Eve", 40},
		{"Frank", 20},
		{"Grace", 28},
		{"Heidi", 30},
		{"Ivan", 45},
		{"Judy", 22},
	}

	g := buildAgeGraph(t, persons)
	eng := cypher.NewEngine(g) // installs index.Manager on g
	installAgeIndex(g)

	res, err := eng.Run(context.Background(),
		"MATCH (n:Person {age: 30}) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)

	// Alice, Dave, Heidi all have age 30.
	if len(rows) != 3 {
		t.Fatalf("age=30: want 3 rows, got %d", len(rows))
	}
	names := make(map[string]bool, len(rows))
	for _, row := range rows {
		sv, ok := row["n.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("n.name: expected StringValue, got %T (%v)", row["n.name"], row["n.name"])
		}
		names[string(sv)] = true
	}
	for _, want := range []string{"Alice", "Dave", "Heidi"} {
		if !names[want] {
			t.Errorf("age=30: missing expected name %q; got %v", want, names)
		}
	}
}

// TestScanIndexHashInt64_NoMatch verifies that querying for an age that no node
// carries returns zero rows.
func TestScanIndexHashInt64_NoMatch(t *testing.T) {
	t.Parallel()

	persons := []personAgeEntry{
		{"Alice", 30},
		{"Bob", 25},
	}

	g := buildAgeGraph(t, persons)
	eng := cypher.NewEngine(g) // installs index.Manager on g
	installAgeIndex(g)

	res, err := eng.Run(context.Background(),
		"MATCH (n:Person {age: 99}) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)
	if len(rows) != 0 {
		t.Errorf("age=99: want 0 rows, got %d", len(rows))
	}
}

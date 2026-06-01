package cypher_test

// scan_label_single_test.go — T593: MATCH (n:Label) RETURN n on a
// single-label universe (fast path through NodeByLabelScan).

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildLabeledTestGraph creates a graph with nPerson Person nodes and nMovie
// Movie nodes. Keys are "person0".."personN-1" and "movie0".."movieM-1".
func buildLabeledTestGraph(t *testing.T, nPerson, nMovie int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range nPerson {
		key := fmt.Sprintf("person%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
	}
	for i := range nMovie {
		key := fmt.Sprintf("movie%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Movie"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
	}
	return g
}

// TestScanLabel_PersonNodes verifies that MATCH (n:Person) RETURN n returns
// exactly the Person nodes (5), not the Movie nodes.
func TestScanLabel_PersonNodes(t *testing.T) {
	t.Parallel()

	g := buildLabeledTestGraph(t, 5, 3)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 5 {
		t.Errorf("Person nodes: want 5 rows, got %d", count)
	}
}

// TestScanLabel_MovieNodes verifies that MATCH (n:Movie) RETURN n returns
// exactly the Movie nodes (3), not the Person nodes.
func TestScanLabel_MovieNodes(t *testing.T) {
	t.Parallel()

	g := buildLabeledTestGraph(t, 5, 3)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n:Movie) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 3 {
		t.Errorf("Movie nodes: want 3 rows, got %d", count)
	}
}

// TestScanLabel_MissingLabel verifies that MATCH (n:Missing) RETURN n returns
// zero rows when no node carries that label.
func TestScanLabel_MissingLabel(t *testing.T) {
	t.Parallel()

	g := buildLabeledTestGraph(t, 5, 3)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n:Missing) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if count != 0 {
		t.Errorf("Missing label: want 0 rows, got %d", count)
	}
}

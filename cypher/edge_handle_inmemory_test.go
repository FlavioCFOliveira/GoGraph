package cypher_test

// edge_handle_inmemory_test.go — Stage 1 stable-edge-identity regression
// coverage, in-memory only (no recovery / reopen).
//
// These exercise the new handle-driven read path in buildEdgeTypeFilter and
// buildRelationshipValueFromRow: per-instance relationship TYPE is resolved
// by an explicit per-slot handle read from the CSR, not by positional
// inference from CSR slot order. The delete-survivor case is the invariant
// the handle column exists for — positional inference mis-mapped here.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// inMemTypes runs MATCH (a)-[r]->(b) RETURN type(r) and returns the sorted
// rel-type list. Endpoints are the (key:'x') -> (key:'y') ordered pair.
func inMemTypes(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.Run(context.Background(),
		`MATCH (:N {key:'x'})-[r]->(:N {key:'y'}) RETURN type(r) AS t`, nil)
	if err != nil {
		t.Fatalf("read types: %v", err)
	}
	records := drainRecords(t, res)
	types := make([]string, 0, len(records))
	for _, row := range records {
		s, ok := row["t"].(expr.StringValue)
		if !ok {
			t.Fatalf("type(r) is %T, want StringValue", row["t"])
		}
		types = append(types, string(s))
	}
	sort.Strings(types)
	return types
}

// TestInMemory_ParallelTypedEdges_HandlePath is the task's primary scenario:
// two distinctly-typed parallel edges between the same ordered pair both
// surface their own type via the handle read path (single engine, no reopen).
func TestInMemory_ParallelTypedEdges_HandlePath(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx, `CREATE (a:N {key:'x'}),(b:N {key:'y'})`, nil); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := eng.RunInTx(ctx,
		`MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES]->(b)`, nil); err != nil {
		t.Fatalf("create USES: %v", err)
	}
	if _, err := eng.RunInTx(ctx,
		`MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS]->(b)`, nil); err != nil {
		t.Fatalf("create CALLS: %v", err)
	}

	got := inMemTypes(t, eng)
	want := []string{"CALLS", "USES"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parallel typed edges = %v, want %v", got, want)
	}
}

// TestInMemory_ParallelTypedEdges_SingleCreate covers the same as above but
// with both edges created in ONE statement, so they share the write
// transaction and are appended in immediate succession.
func TestInMemory_ParallelTypedEdges_SingleCreate(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx,
		`CREATE (a:N {key:'x'}),(b:N {key:'y'}),(a)-[:USES]->(b),(a)-[:CALLS]->(b)`, nil); err != nil {
		t.Fatalf("single-statement create: %v", err)
	}

	got := inMemTypes(t, eng)
	want := []string{"CALLS", "USES"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parallel typed edges = %v, want %v", got, want)
	}
}

// TestInMemory_DeleteSibling_SurvivorKeepsType is the delete-stability
// invariant the handle column exists for. Three distinctly-typed parallels;
// delete the FIRST-created one; the remaining two must still report their
// OWN types — the positional read path mis-mapped them after the adjacency
// slot compaction shifted positions.
func TestInMemory_DeleteSibling_SurvivorKeepsType(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx,
		`CREATE (a:N {key:'x'}),(b:N {key:'y'}),`+
			`(a)-[:USES]->(b),(a)-[:CALLS]->(b),(a)-[:READS]->(b)`, nil); err != nil {
		t.Fatalf("seed three parallels: %v", err)
	}

	// Sanity: all three present before delete.
	if got := inMemTypes(t, eng); len(got) != 3 {
		t.Fatalf("before delete: got %v, want 3 types", got)
	}

	// Delete the USES edge (the first-created parallel). This compacts the
	// adjacency slice; the positional read path would re-map the survivors.
	if _, err := eng.RunInTx(ctx,
		`MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) DELETE r`, nil); err != nil {
		t.Fatalf("delete USES: %v", err)
	}

	got := inMemTypes(t, eng)
	want := []string{"CALLS", "READS"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("after deleting USES: survivors = %v, want %v (handle path must not mis-map)", got, want)
	}
}

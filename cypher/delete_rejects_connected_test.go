package cypher_test

// delete_rejects_connected_test.go — DELETE must reject nodes with relationships (T843).
//
// The engine must return exec.ErrDeleteNodeHasRelationships (or a wrapping
// error) when DELETE is attempted on a node that still has incident edges.
// After the error the graph must be unchanged: the node and its edge must
// still exist.
//
// Edges are injected directly via the lpg.Graph API (g.AddEdge) because the
// Cypher cross-product MATCH for two same-label nodes with property predicates
// does not yet produce rows — a known engine limitation.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// synthKeyForLabel returns the synthetic key of the first node that carries
// label in g. It panics when no such node exists.
func synthKeyForLabel(tb testing.TB, g *lpg.Graph[string, float64], label string) string {
	tb.Helper()
	lid, ok := g.Registry().Lookup(label)
	if !ok {
		tb.Fatalf("label %q not registered", label)
	}
	bm := g.NodeIndex().Intersect(uint32(lid))
	if bm.IsEmpty() {
		tb.Fatalf("no node with label %q", label)
	}
	key, _ := g.AdjList().Mapper().Resolve(graph.NodeID(bm.Minimum()))
	return key
}

// TestDelete_RejectsNodeWithRelationships creates a directed edge alice→bob
// using the LPG API, then tries to DELETE alice via Cypher. The engine must
// return ErrDeleteNodeHasRelationships; alice must still exist afterwards.
func TestDelete_RejectsNodeWithRelationships(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (:Src {name: "alice"})`)
	drainRunInTx(t, eng, `CREATE (:Dst {name: "bob"})`)

	aliceKey := synthKeyForLabel(t, g, "Src")
	bobKey := synthKeyForLabel(t, g, "Dst")

	// Inject the edge directly so the adjacency list records it.
	if err := g.AddEdge(aliceKey, bobKey, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// DELETE alice — must fail because she has an outgoing edge.
	res, runErr := eng.RunInTx(ctx, `MATCH (n:Src {name:"alice"}) DELETE n`, nil)
	var iterErr error
	if runErr == nil {
		for res.Next() {
		}
		iterErr = res.Err()
		_ = res.Close()
	}
	gotErr := runErr
	if gotErr == nil {
		gotErr = iterErr
	}
	if gotErr == nil {
		t.Fatal("expected error for DELETE of node with outgoing edge, got nil")
	}
	if !errors.Is(gotErr, exec.ErrDeleteNodeHasRelationships) {
		t.Errorf("expected ErrDeleteNodeHasRelationships, got: %v", gotErr)
	}

	// Graph must be unchanged: alice still exists with label Src.
	aliceRes, err := eng.RunInTx(ctx, `MATCH (n:Src {name:"alice"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("MATCH after failed DELETE: %v", err)
	}
	rows := collectRecords(t, aliceRes)
	if len(rows) != 1 {
		t.Errorf("expected alice to still exist after failed DELETE, got %d rows", len(rows))
		return
	}
	got, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok || string(got) != "alice" {
		t.Errorf("n.name = %v, want StringValue(alice)", rows[0]["n.name"])
	}
}

// TestDelete_RejectsBobWithIncomingEdge verifies that DELETE is also refused
// for a node that has an incoming (not outgoing) edge.
func TestDelete_RejectsBobWithIncomingEdge(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (:Source)`)
	drainRunInTx(t, eng, `CREATE (:Sink)`)

	srcKey := synthKeyForLabel(t, g, "Source")
	dstKey := synthKeyForLabel(t, g, "Sink")

	if err := g.AddEdge(srcKey, dstKey, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// DELETE dst — has an incoming edge from src.
	res, runErr := eng.RunInTx(ctx, `MATCH (n:Sink) DELETE n`, nil)
	var iterErr error
	if runErr == nil {
		for res.Next() {
		}
		iterErr = res.Err()
		_ = res.Close()
	}
	gotErr := runErr
	if gotErr == nil {
		gotErr = iterErr
	}
	if gotErr == nil {
		t.Fatal("expected error for DELETE of node with incoming edge, got nil")
	}
	if !errors.Is(gotErr, exec.ErrDeleteNodeHasRelationships) {
		t.Errorf("expected ErrDeleteNodeHasRelationships, got: %v", gotErr)
	}
}

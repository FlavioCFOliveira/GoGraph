package cypher_test

// match_multigraph_reltype_test.go — rmp #1634 regression: an undirected
// (or reverse) hop over PARALLEL edges with distinct types must report
// each edge's own type, not a single merged type.
//
// Before the fix, exec.Expand.tryRevEdge mapped every reverse edge of a
// (dst -> src) pair onto the FIRST forward-CSR position via
// lookupFwdEdgePos, so two parallel edges A-[:T1]->B and A-[:T2]->B both
// resolved to T1 on the reverse hop. The fix disambiguates by the stable
// per-instance handle that csr.BuildReverse carries on each reverse slot.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func collectRelTypes(t *testing.T, eng *cypher.Engine, query string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var got []string
	for res.Next() {
		rec := res.Record()
		sv, ok := rec["t"].(expr.StringValue)
		if !ok {
			t.Fatalf("column t: expected StringValue, got %T (%v)", rec["t"], rec["t"])
		}
		got = append(got, string(sv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	sort.Strings(got)
	return got
}

// TestMatch_Multigraph_ReverseHop_PerInstanceType pins the #1634 contract:
// the forward hop and the undirected reverse hop over two parallel,
// distinctly-typed edges both yield {T1, T2}.
func TestMatch_Multigraph_ReverseHop_PerInstanceType(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	seed := []string{
		`CREATE (a:A {id: 1})`,
		`CREATE (b:B {id: 2})`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:T1]->(b)`,
		`MATCH (a:A), (b:B) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:T2]->(b)`,
	}
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}

	want := []string{"T1", "T2"}

	// Forward hop (control): was already correct.
	if got := collectRelTypes(t, eng, `MATCH (a:A)-[r]->(b:B) RETURN type(r) AS t`); !equalStrs(got, want) {
		t.Fatalf("forward hop types = %v, want %v", got, want)
	}
	// Undirected reverse hop (the bug): from the :B side, against the edge.
	if got := collectRelTypes(t, eng, `MATCH (b:B)-[r]-(a:A) RETURN type(r) AS t`); !equalStrs(got, want) {
		t.Fatalf("undirected reverse-hop types = %v, want %v (parallel edges collapsed to a merged type)", got, want)
	}
	// Pure reverse hop (incoming): same contract.
	if got := collectRelTypes(t, eng, `MATCH (b:B)<-[r]-(a:A) RETURN type(r) AS t`); !equalStrs(got, want) {
		t.Fatalf("reverse incoming-hop types = %v, want %v", got, want)
	}
}

func equalStrs(a, b []string) bool {
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

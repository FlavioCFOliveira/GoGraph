package cypher_test

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Relationship-isomorphism (cyphermorphism, the "no repeated relationship"
// rule) applies across the ENTIRE MATCH clause, including comma-separated
// path patterns (openCypher 9 §3.2.2; Francis et al. SIGMOD 2018 §3). Two
// comma-separated patterns sharing endpoints must NOT bind the same physical
// edge to two DISTINCT relationship variables. The rule is per-MATCH-clause:
// across SEPARATE MATCH clauses the same edge may bind to different variables.
//
// These tests pin the cross-comma enforcement (rmp #1777). Before the fix,
// `MATCH (a)-[r1]->(b),(a)-[r2]->(b)` over a single edge returned one row with
// r1 and r2 bound to the SAME edge — a relationship-uniqueness violation.

func relUniqEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

func relUniqCount(t *testing.T, eng *cypher.Engine, query string) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("query %q: expected exactly one aggregate row, got %d", query, len(rows))
	}
	iv, ok := rows[0]["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("query %q: count column is %T (%v), want IntegerValue", query, rows[0]["c"], rows[0]["c"])
	}
	return int64(iv)
}

// TestRelUniqueness_CommaOneEdge asserts the headline bug: two comma-separated
// patterns over a graph with a single a->b edge must NOT both bind that edge.
// The result must be 0 rows, not 1.
func TestRelUniqueness_CommaOneEdge(t *testing.T) {
	eng := relUniqEngine(t)
	runSetup(t, eng, "CREATE (a:N{k:0})-[:R]->(b:N{k:1})")

	got := relUniqCount(t, eng,
		"MATCH (a:N{k:0})-[r1]->(b:N{k:1}),(a)-[r2]->(b) RETURN count(*) AS c")
	if got != 0 {
		t.Fatalf("comma form over one edge: got %d rows, want 0 "+
			"(r1 and r2 may not bind the same edge within one MATCH clause)", got)
	}
}

// TestRelUniqueness_CommaConnectedControl is the connected-path control. A
// single connected pattern was always enforced correctly; it must STAY 0.
func TestRelUniqueness_CommaConnectedControl(t *testing.T) {
	eng := relUniqEngine(t)
	runSetup(t, eng, "CREATE (a:N{k:0})-[:R]->(b:N{k:1})")

	got := relUniqCount(t, eng,
		"MATCH (a)-[r1]->(b)<-[r2]-(a) RETURN count(*) AS c")
	if got != 0 {
		t.Fatalf("connected control: got %d rows, want 0 (single connected path "+
			"with two rel vars over one edge)", got)
	}
}

// TestRelUniqueness_CrossClauseControl asserts the rule does NOT leak across
// SEPARATE MATCH clauses: two distinct rel variables in different clauses may
// bind the same edge. This must remain 1 (no regression / not over-restricted).
func TestRelUniqueness_CrossClauseControl(t *testing.T) {
	eng := relUniqEngine(t)
	runSetup(t, eng, "CREATE (a:N{k:0})-[:R]->(b:N{k:1})")

	got := relUniqCount(t, eng,
		"MATCH (a:N{k:0})-[r1]->(b:N{k:1}) MATCH (a)-[r2]->(b) RETURN count(*) AS c")
	if got != 1 {
		t.Fatalf("cross-clause control: got %d rows, want 1 "+
			"(distinct rel vars in SEPARATE MATCH clauses may bind the same edge)", got)
	}
}

// TestRelUniqueness_CommaSharedVar covers the shared-variable comma form, which
// the IR builds via the CorrelatedApply branch (the leading node `a` is shared
// between the two comma patterns). The single edge may not bind both r1 and r2.
func TestRelUniqueness_CommaSharedVar(t *testing.T) {
	eng := relUniqEngine(t)
	runSetup(t, eng, "CREATE (a:N{k:0})-[:R]->(b:N{k:1})")

	got := relUniqCount(t, eng,
		"MATCH (a:N{k:0})-[r1]->(b:N{k:1}),(a)-[r2]->(b:N{k:1}) RETURN count(*) AS c")
	if got != 0 {
		t.Fatalf("shared-var comma form: got %d rows, want 0 "+
			"(CorrelatedApply branch must enforce cross-pattern uniqueness)", got)
	}
}

// TestRelUniqueness_CommaDistinctEdgesKept asserts the fix is NOT over-
// restrictive: when the two comma patterns can bind DIFFERENT edges, those
// matches must survive. Graph: a->b and a->c. The ordered pairs (r1,r2) of
// distinct out-edges of `a` number 2; the two pairs where r1==r2 are excluded
// by uniqueness, leaving exactly 2 rows.
func TestRelUniqueness_CommaDistinctEdgesKept(t *testing.T) {
	eng := relUniqEngine(t)
	runSetup(t, eng,
		"CREATE (a:N{k:0}), (b:N{k:1}), (c:N{k:2}), (a)-[:R]->(b), (a)-[:R]->(c)")

	got := relUniqCount(t, eng,
		"MATCH (a:N{k:0})-[r1]->(x),(a)-[r2]->(y) RETURN count(*) AS c")
	if got != 2 {
		t.Fatalf("distinct-edge comma form: got %d rows, want 2 "+
			"(two distinct out-edges of a form 2 ordered distinct-rel pairs; "+
			"the fix must not reject valid distinct-edge matches)", got)
	}
}

// TestRelUniqueness_CommaVLEExcludesPriorEdge asserts the no-repeat rule reaches
// a variable-length step in a subsequent comma pattern: the VLE must not re-
// traverse the edge already bound to `r` in the preceding single-hop pattern.
// Graph: one a->b edge. `(a)-[r]->(b),(a)-[*1]->(b)` must yield 0 rows because
// the only length-1 path from a to b uses the edge already bound to r.
func TestRelUniqueness_CommaVLEExcludesPriorEdge(t *testing.T) {
	eng := relUniqEngine(t)
	runSetup(t, eng, "CREATE (a:N{k:0})-[:R]->(b:N{k:1})")

	got := relUniqCount(t, eng,
		"MATCH (a:N{k:0})-[r]->(b:N{k:1}),(a)-[*1]->(b) RETURN count(*) AS c")
	if got != 0 {
		t.Fatalf("comma VLE form: got %d rows, want 0 "+
			"(the VLE step must exclude the edge already bound to r in the "+
			"preceding comma pattern)", got)
	}
}

package cypher_test

// patterncomp_parallel_multitype_test.go — regression for rmp #2017.
//
// The key-based pattern-comprehension fallback (cypher.patternEvaluator's
// EvalPatternComp, reached when a pattern comprehension is evaluated inline —
// e.g. inside a WHERE predicate — rather than lowered to RollUpApply) could not
// enumerate an untyped relationship `[r]` per parallel-edge instance over a
// multi-type parallel pair. It produced one candidate per neighbour slot (so
// the list length was right) but resolved every candidate's type from the
// per-pair union via the single deterministic pick, so
//
//	[(a)-[r]->(b) | type(r)]
//
// over (a)-[:T1]->(b) / (a)-[:T2]->(b) yielded ["T1", "T1"] — the
// alphabetically-smallest type twice — and `'T2' IN [...]` was false. The fix
// resolves each candidate's type from its own stable per-slot handle, so the
// two parallel instances report their own distinct types ["T1", "T2"],
// consistent with the primary RollUpApply comprehension path and a plain MATCH.
//
// The comprehension MUST be exercised through the inline EvalPatternComp path:
// a pattern comprehension in a WHERE predicate (evaluated by the Filter
// operator, which wires the pattern evaluator) reaches it, whereas the same
// comprehension in a RETURN projection is lowered to RollUpApply and never
// touches this code. The read path (Engine.Run) is used because it is the path
// that wires the pattern evaluator into expression evaluation.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildParallelMultiTypeEngine builds a directed multigraph with one :A and one
// :B node joined by two parallel relationships of distinct types :T1 and :T2.
func buildParallelMultiTypeEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	ctx := context.Background()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (a:A), (b:B)`,
		`MATCH (a:A),(b:B) CREATE (a)-[:T1]->(b)`,
		`MATCH (a:A),(b:B) CREATE (a)-[:T2]->(b)`,
	} {
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
		for res.Next() { // drain
		}
		if cerr := res.Err(); cerr != nil {
			t.Fatalf("setup %q result error: %v", q, cerr)
		}
		_ = res.Close()
	}
	return eng
}

// countRunRows runs query on the read path (Engine.Run) and returns the number
// of rows produced.
func countRunRows(t *testing.T, eng *cypher.Engine, query string) int {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", query, err)
	}
	defer res.Close() // test cleanup
	var n int
	for res.Next() {
		n++
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("Run %q result error: %v", query, cerr)
	}
	return n
}

// TestPatternCompFallback_UntypedRelPerParallelInstance is the #2017 repro. The
// untyped `[r]` over the multi-type parallel pair must enumerate BOTH distinct
// types via the inline EvalPatternComp path.
func TestPatternCompFallback_UntypedRelPerParallelInstance(t *testing.T) {
	t.Parallel()
	eng := buildParallelMultiTypeEngine(t)

	// Length: the comprehension collects one element per parallel instance.
	if n := countRunRows(t, eng,
		`MATCH (a:A),(b:B) WHERE size([(a)-[r]->(b) | type(r)]) = 2 RETURN a AS c`); n != 1 {
		t.Fatalf("size([(a)-[r]->(b) | type(r)]) = 2: got %d rows, want 1", n)
	}

	// Per-instance type(r): the alphabetically-smallest type T1 was always
	// reported pre-fix; T2 was never enumerated over the parallel pair.
	if n := countRunRows(t, eng,
		`MATCH (a:A),(b:B) WHERE 'T1' IN [(a)-[r]->(b) | type(r)] RETURN a AS c`); n != 1 {
		t.Fatalf("'T1' IN [(a)-[r]->(b) | type(r)]: got %d rows, want 1", n)
	}
	if n := countRunRows(t, eng,
		`MATCH (a:A),(b:B) WHERE 'T2' IN [(a)-[r]->(b) | type(r)] RETURN a AS c`); n != 1 {
		t.Fatalf("'T2' IN [(a)-[r]->(b) | type(r)]: got %d rows, want 1 (T2 must be enumerated per instance)", n)
	}

	// Combined: exactly two elements AND both distinct types present — the
	// full multi-type enumeration.
	if n := countRunRows(t, eng,
		`MATCH (a:A),(b:B) WHERE size([(a)-[r]->(b) | type(r)]) = 2 `+
			`AND 'T1' IN [(a)-[r]->(b) | type(r)] `+
			`AND 'T2' IN [(a)-[r]->(b) | type(r)] RETURN a AS c`); n != 1 {
		t.Fatalf("combined length-and-both-types: got %d rows, want 1", n)
	}
}

// TestPatternCompFallback_UntypedRelIncoming covers the incoming direction of
// the same defect: an untyped `[r]` incoming hop over the parallel pair must
// also enumerate both instances (collectIncomingCandidates previously stopped
// at the first matching slot, collapsing the parallel edges to one candidate
// and one type).
func TestPatternCompFallback_UntypedRelIncoming(t *testing.T) {
	t.Parallel()
	eng := buildParallelMultiTypeEngine(t)

	if n := countRunRows(t, eng,
		`MATCH (b:B) WHERE size([(b)<-[r]-() | type(r)]) = 2 `+
			`AND 'T1' IN [(b)<-[r]-() | type(r)] `+
			`AND 'T2' IN [(b)<-[r]-() | type(r)] RETURN b AS c`); n != 1 {
		t.Fatalf("incoming combined length-and-both-types: got %d rows, want 1", n)
	}
}

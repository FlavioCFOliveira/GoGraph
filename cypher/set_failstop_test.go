package cypher_test

// set_failstop_test.go — regression test for the SET-RHS fail-silent bug
// (2026-07-13 production-readiness audit, cypher finding F1).
//
// The write path evaluated a SET / MERGE-SET right-hand side through a closure
// that swallowed ANY evaluation error into a silent no-op, so a statement whose
// RHS could not be evaluated left the property unset with no diagnostic — while
// the same expression raises loudly in RETURN / WHERE. That violates the
// fail-stop, never fail-silent mandate. The RHS closure now propagates the
// error, so the statement fails and rolls back atomically.
//
// # Why the RHS is an indexing error and not the COUNT { } it once was
//
// The original test drove the closure with `SET n.bad = COUNT { (n)-->() }`,
// which errored for a reason that had nothing to do with F1: COUNT { } was
// simply unwired on the write path, so ANY occurrence of it in a writing
// statement failed. rmp #2660 wired it, and that error source disappeared —
// `SET n.bad = COUNT { (n)-->() }` is now a valid statement that stores the
// count, which is pinned by TestWritePathSubqueryAndPatternPredicate.
//
// The SUBJECT of this test is unchanged and is not about COUNT { }: it is that
// an evaluation error on a SET RHS reaches the caller instead of being
// swallowed. `n.name[0]` — indexing into a String — is a per-row evaluation
// error that is raised by the same closure, and it is derived from the bound
// row, so no constant folding can move it out of the RHS and rob this test of
// its subject.
//
// The atomicity half is unchanged: the co-assigned `n.good` must not survive a
// statement whose other assignment failed.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSet_EvalErrorRHS_FailStop asserts that a runtime evaluation error on a SET
// RHS in the write path surfaces a loud error (never a silent no-op) and that the
// whole statement rolls back atomically — the co-assigned property is not left
// partially applied.
func TestSet_EvalErrorRHS_FailStop(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithRegistry(g, funcs.DefaultRegistry)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx, "CREATE (:P {name:'a'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := eng.RunInTx(ctx,
		"MATCH (n:P) SET n.good = 7, n.bad = n.name[0] RETURN n.good AS g", nil)
	// The eval error may surface from RunInTx or from the lazy result stream.
	gotErr := err
	if gotErr == nil && res != nil {
		for res.Next() { // drain to surface the lazy error
		}
		gotErr = res.Err()
		res.Close()
	}
	if gotErr == nil {
		t.Fatal("SET n.bad = n.name[0] silently succeeded, want a fail-stop error")
	}
	if !strings.Contains(gotErr.Error(), "index") {
		t.Fatalf("error = %v, want the indexing error the RHS raises", gotErr)
	}

	// Atomicity: the co-assigned n.good must NOT have been applied.
	check, err := eng.Run(ctx, "MATCH (n:P) RETURN n.good AS g", nil)
	if err != nil {
		t.Fatalf("verify Run: %v", err)
	}
	defer check.Close()
	if !check.Next() {
		t.Fatal("verify: no rows")
	}
	gv, _ := check.Record()["g"].(expr.Value)
	if check.Record()["g"] != nil && !expr.IsNull(gv) {
		t.Fatalf("n.good = %v after a failed statement, want NULL/unset (atomic rollback)", check.Record()["g"])
	}
}

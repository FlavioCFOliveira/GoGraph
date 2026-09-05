package cypher_test

// set_failstop_test.go — regression test for the SET-RHS fail-silent bug
// (2026-07-13 production-readiness audit, cypher finding F1).
//
// The write path evaluated a SET / MERGE-SET right-hand side through a closure
// that swallowed ANY evaluation error into a silent no-op. `SET n.p = COUNT {
// (n)-->() }` under RunInTx therefore left n.p unset with no diagnostic (COUNT{}
// is unwired in the write path, so it errors — the error was discarded), while
// the same RHS raises loudly in RETURN/WHERE. That violates the fail-stop,
// never fail-silent mandate. The RHS closure now propagates the error, so the
// statement fails and rolls back atomically.

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

// TestSet_CountSubqueryRHS_FailStop asserts that a COUNT{} subquery on a SET RHS
// in the write path surfaces a loud error (never a silent no-op) and that the
// whole statement rolls back atomically — the co-assigned property is not left
// partially applied.
func TestSet_CountSubqueryRHS_FailStop(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithRegistry(g, funcs.DefaultRegistry)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx, "CREATE (:P {name:'a'})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := eng.RunInTx(ctx,
		"MATCH (n:P) SET n.good = 7, n.bad = COUNT { (n)-->() } RETURN n.good AS g", nil)
	// The eval error may surface from RunInTx or from the lazy result stream.
	gotErr := err
	if gotErr == nil && res != nil {
		for res.Next() { // drain to surface the lazy error
		}
		gotErr = res.Err()
		res.Close()
	}
	if gotErr == nil {
		t.Fatal("SET n.bad = COUNT{...} silently succeeded, want a fail-stop error")
	}
	if !strings.Contains(gotErr.Error(), "COUNT") && !strings.Contains(gotErr.Error(), "subquery") {
		t.Fatalf("error = %v, want a COUNT/subquery-not-supported error", gotErr)
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

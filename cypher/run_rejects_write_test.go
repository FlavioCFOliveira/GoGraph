package cypher_test

// run_rejects_write_test.go — regression coverage for a pre-existing defect
// surfaced while testing rmp #1866: Engine.Run had no explicit check for a
// write/DDL clause. buildPlanWithMutatorFull (which wires the write-capable
// physical builder via bopts.writeFallback) is only invoked by RunInTx, so a
// write clause reaching Run always failed — but several calls deeper, with
// an opaque "cypher: unsupported IR node *ir.SomeInternalType" error that
// named an implementation-internal Go type and gave the caller no hint that
// Run itself was the wrong entry point. Confirmed pre-existing for the
// already-shipped MergeRelationship (not something introduced by
// MergePattern): `MATCH (a),(b) MERGE (a)-[r:R]->(b) RETURN r` run via
// Engine.Run failed the identical way before this fix.
//
// Fix: Run now checks ir.ContainsWrite on the already-built plan and
// rejects immediately with a clear, actionable error wrapping
// ErrWriteInReadOnlyTx (the same sentinel a read-only explicit transaction
// uses for the same underlying reason: no writer lock, no visibility
// barrier, no WAL transaction on this call path).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestRun_RejectsWriteClause_ClearError(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	queries := []string{
		`CREATE (:X)`,
		`MATCH (a),(b) MERGE (a)-[:R]->(b)`,
		`MERGE (a:Y {k: 1})-[:R]->(b:Z {k: 2})`,
		`MATCH (n) SET n.x = 1`,
	}
	for _, q := range queries {
		_, err := eng.Run(ctx, q, nil)
		if err == nil {
			t.Fatalf("Run(%q): expected an error, got nil", q)
		}
		if !errors.Is(err, cypher.ErrWriteInReadOnlyTx) {
			t.Fatalf("Run(%q): error = %v, want errors.Is(err, ErrWriteInReadOnlyTx)", q, err)
		}
		if !strings.Contains(err.Error(), "RunInTx") {
			t.Fatalf("Run(%q): error = %q, want it to mention RunInTx", q, err.Error())
		}
	}

	// A pure read must still succeed unaffected.
	res, err := eng.Run(ctx, `MATCH (n) RETURN count(n) AS n`, nil)
	if err != nil {
		t.Fatalf("Run pure read: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Run pure read drain: %v", err)
	}
	res.Close()
}

// TestRun_RejectsWriteClause_NoPartialExecution verifies the rejection
// happens before ANY execution — the graph must be completely untouched.
func TestRun_RejectsWriteClause_NoPartialExecution(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE (:ShouldNeverExist)`, nil); err == nil {
		t.Fatal("expected Run to reject the write")
	}
	assertCount(ctx, t, eng, `MATCH (n:ShouldNeverExist) RETURN count(n) AS n`, 0)
}

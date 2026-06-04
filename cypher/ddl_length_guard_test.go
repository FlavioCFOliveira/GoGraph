package cypher_test

// ddl_length_guard_test.go — regression coverage for #1321: the DDL parse path
// (ir.ParseDDL) must enforce the same 1 MiB byte-length cap the DML parse path
// enforces. Before the fix a multi-MiB CREATE INDEX bypassed the cap because
// DDL never reached the parser's pre-parse guard.
//
// Layer: short. goleak-clean (engine/graph local).

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestDDL_OversizeQueryRejected verifies that an over-length DDL query is
// rejected with a "query too large" error, matching the DML path's message.
func TestDDL_OversizeQueryRejected(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Build a > 1 MiB CREATE INDEX: a valid prefix padded with whitespace so
	// the over-length check trips before any tokenisation work.
	const twoMiB = 2 << 20
	ddl := "CREATE INDEX bigidx FOR (n:Label) ON (n.prop)" + strings.Repeat(" ", twoMiB)

	_, err := eng.Run(ctx, ddl, nil)
	if err == nil {
		t.Fatal("oversize CREATE INDEX accepted; want a 'query too large' error")
	}
	if !strings.Contains(err.Error(), "query too large") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "query too large")
	}

	// The DML path rejects an over-length query with the same phrase; confirm
	// the DDL path now matches it.
	dml := "RETURN 1 AS n" + strings.Repeat(" ", twoMiB)
	_, dmlErr := eng.Run(ctx, dml, nil)
	if dmlErr == nil {
		t.Fatal("oversize DML query accepted; want a 'query too large' error")
	}
	if !strings.Contains(dmlErr.Error(), "query too large") {
		t.Fatalf("DML error = %q, want it to contain %q", dmlErr.Error(), "query too large")
	}
}

// TestDDL_WithinLimitAccepted is the negative control: a normal-sized CREATE
// INDEX is unaffected by the guard.
func TestDDL_WithinLimitAccepted(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.Run(ctx, "CREATE INDEX smallidx FOR (n:Label) ON (n.prop)", nil)
	if err != nil {
		t.Fatalf("normal CREATE INDEX rejected: %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("normal CREATE INDEX iteration error: %v", err)
	}
	_ = res.Close()
}

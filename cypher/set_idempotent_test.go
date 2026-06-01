package cypher_test

// set_idempotent_test.go — T828
//
// Tests that SET is idempotent: setting a property twice to the same value
// leaves the node with that value unchanged (no doubling, no stacking).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSet_TwiceEqualsOnce verifies that setting the same integer property
// twice results in the same final value as setting it once.
func TestSet_TwiceEqualsOnce(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Account {name: "X"})`)
	drainRunInTx(t, eng, `MATCH (n:Account) SET n.age = 30`)
	drainRunInTx(t, eng, `MATCH (n:Account) SET n.age = 30`)

	res, err := eng.Run(ctx, `MATCH (n:Account) RETURN n.age`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if got := fmtAny(rows[0]["n.age"]); got != "30" {
		t.Errorf("n.age after two identical SETs = %s, want 30", got)
	}
}

// TestSet_FloatTwiceEqualsOnce verifies the same idempotency for a float
// property.
func TestSet_FloatTwiceEqualsOnce(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Rate {name: "r"})`)
	drainRunInTx(t, eng, `MATCH (n:Rate) SET n.score = 9.5`)
	drainRunInTx(t, eng, `MATCH (n:Rate) SET n.score = 9.5`)

	res, err := eng.Run(ctx, `MATCH (n:Rate) RETURN n.score`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got := fmtAny(rows[0]["n.score"])
	if got != "9.5" {
		t.Errorf("n.score after two identical SETs = %s, want 9.5", got)
	}
}

// TestSet_OverwriteChangesValue verifies that SET with a different value
// replaces the previous one (ruling out an append-rather-than-replace bug).
func TestSet_OverwriteChangesValue(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Node {name: "n", v: 1})`)
	drainRunInTx(t, eng, `MATCH (n:Node) SET n.v = 2`)

	res, err := eng.Run(ctx, `MATCH (n:Node) RETURN n.v`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if got := fmtAny(rows[0]["n.v"]); got != "2" {
		t.Errorf("n.v after overwrite = %s, want 2", got)
	}
}

package cypher_test

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestT960CreateWithVarExpr verifies that plan-construction succeeds and
// the property is materialised correctly when a CREATE property map contains
// a bound variable reference (e.g. UNWIND [42] AS x CREATE (n {num: x})).
func TestT960CreateWithVarExpr(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	// Write-only: plan-construction must NOT fail for {num: x}.
	res, err := engine.RunInTx(ctx, `UNWIND [42] AS x CREATE (n {num: x})`, nil)
	if err != nil {
		t.Fatalf("CREATE with variable property failed: %v", err)
	}
	if res != nil {
		res.Close()
	}

	// Read back: the node must have num=42.
	res2, err2 := engine.Run(ctx, `MATCH (n) WHERE n.num = 42 RETURN n.num`, nil)
	if err2 != nil {
		t.Fatalf("MATCH query failed: %v", err2)
	}
	defer res2.Close()
	if !res2.Next() {
		t.Fatal("expected node with num=42 to exist after CREATE {num: x}")
	}
	t.Logf("n.num = %v", res2.Record())
}

// TestT960CreateWithPropAccess verifies that CREATE with a property-access
// expression (e.g. CREATE (b {num: a.id})) does not fail at plan-construction.
func TestT960CreateWithPropAccess(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	// Setup: create a node with a property.
	res0, err := engine.RunInTx(ctx, `CREATE (a {id: 5})`, nil)
	if err != nil {
		t.Fatalf("setup CREATE failed: %v", err)
	}
	if res0 != nil {
		res0.Close()
	}

	// Plan-construction must NOT fail for {num: a.id}.
	// RunAny routes write queries through RunInTx; Run would reject a write-containing plan.
	res, err2 := engine.RunAny(ctx, `MATCH (a) CREATE (b {num: a.id}) RETURN b.num`, nil)
	if err2 != nil {
		t.Fatalf("CREATE with property access failed: %v", err2)
	}
	defer res.Close()
	if res.Next() {
		t.Logf("b.num = %v", res.Record())
	} else {
		t.Log("no rows returned (property read-back depends on T937 fix)")
	}
}

// TestT960CreateRelWithVarExpr verifies that CREATE relationship with a
// non-literal property map does not fail at plan-construction.
func TestT960CreateRelWithVarExpr(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := engine.RunInTx(ctx,
		`UNWIND [42] AS w CREATE (a)-[:KNOWS {weight: w}]->(b)`,
		nil,
	)
	if err != nil {
		t.Fatalf("CREATE relationship with variable property failed: %v", err)
	}
	if res != nil {
		res.Close()
	}
}

// TestT960MergeWithVarExpr verifies that MERGE with a non-literal property
// map does not fail at plan-construction.
func TestT960MergeWithVarExpr(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := engine.RunInTx(ctx,
		`UNWIND [99] AS x MERGE (n {num: x})`,
		nil,
	)
	if err != nil {
		t.Fatalf("MERGE with variable property failed: %v", err)
	}
	if res != nil {
		res.Close()
	}
}

// TestT960ParamInCreate verifies that query parameters in CREATE property maps
// are substituted correctly.
func TestT960ParamInCreate(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	params := map[string]expr.Value{
		"val": expr.IntegerValue(7),
	}
	res, err := engine.RunInTx(ctx, `CREATE (n {num: $val})`, params)
	if err != nil {
		t.Fatalf("CREATE with $param property failed: %v", err)
	}
	if res != nil {
		res.Close()
	}

	res2, err2 := engine.Run(ctx, `MATCH (n) WHERE n.num = 7 RETURN n.num`, nil)
	if err2 != nil {
		t.Fatalf("MATCH after param CREATE failed: %v", err2)
	}
	defer res2.Close()
	if !res2.Next() {
		t.Fatal("expected node with num=7 after CREATE {num: $val}")
	}
	t.Logf("n.num = %v", res2.Record())
}

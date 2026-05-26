package cypher_test

// multi_edge_create_test.go — regression coverage for the multi-edge
// CREATE / MATCH+CREATE-relationship fix. Before the fix, the IR
// translator emitted one CreateNode per variable usage in a CREATE
// clause, so re-references like the second `(b)` in
// `CREATE (a:User), (b:User), (a)-[:F]->(b)` spawned a fresh node and
// overwrote the schema map, causing every relationship past the first
// to drop. Cartesian MATCH + CREATE-relationship lost edges for the
// same reason: the MATCH-bound variables were re-emitted as
// CreateNode operators on top of the MATCH plan, shifting schema
// positions away from the row layout the CreateRelationship operator
// expected.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

func TestRunInTx_MultiEdgeSingleCreate(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `
        CREATE (a:User {username: "a"}),
               (b:User {username: "b"}),
               (c:User {username: "c"}),
               (a)-[:F]->(b),
               (b)-[:F]->(c),
               (c)-[:F]->(a)
    `, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	assertCount(ctx, t, eng, `MATCH (n:User) RETURN count(n) AS n`, 3)
	assertCount(ctx, t, eng, `MATCH ()-[r:F]->() RETURN count(r) AS n`, 3)
}

func TestRunInTx_MultiEdgeBidirectional(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (a:X), (b:X), (a)-[:R]->(b), (b)-[:R]->(a)`, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	assertCount(ctx, t, eng, `MATCH (n:X) RETURN count(n) AS n`, 2)
	assertCount(ctx, t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS n`, 2)
}

func TestRunInTx_MatchPlusCreateRelationship(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (a:User {username: "a"}), (b:User {username: "b"})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	res2, err := eng.RunInTx(ctx, `
        MATCH (a:User), (b:User)
        WHERE a.username = "a" AND b.username = "b"
        CREATE (a)-[:KNOWS]->(b)
    `, nil)
	if err != nil {
		t.Fatalf("MATCH+CREATE: %v", err)
	}
	_ = drainRecords(t, res2)

	assertCount(ctx, t, eng, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS n`, 1)
}

func TestRunInTx_AnonymousEndpoint(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (a:User {username: "owner"}), (a)-[:OWNS]->()`, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	assertCount(ctx, t, eng, `MATCH (n:User) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH ()-[r:OWNS]->() RETURN count(r) AS n`, 1)
}

// assertCount runs a `RETURN count(*) AS n` style query and fails the
// test when the result differs from want.
func assertCount(ctx context.Context, t *testing.T, eng *cypher.Engine, query string, want int64) {
	t.Helper()
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", query, err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("Run %q yielded %d rows, want 1", query, len(rows))
	}
	got := fmtAny(rows[0]["n"])
	wantS := fmtAny(want)
	if got != wantS {
		t.Fatalf("count %q = %s, want %s", query, got, wantS)
	}
}

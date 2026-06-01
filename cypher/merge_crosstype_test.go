package cypher_test

// merge_crosstype_test.go — rmp #1240
//
// Regression tests for the openCypher conformance bug where MERGE's match
// phase used STRICT type+value equality instead of the `=` operator's
// cross-type numeric equality. A node stored as integer `(:N {x:1})` failed
// to match `MERGE (n:N {x:1.0})` and MERGE wrongly created a duplicate, even
// though `MATCH (n:N {x:1.0})` and `WHERE n.x = 1.0` matched it correctly
// (1 == 1.0 → true, per Comparison1.feature [9]/[10]/[11]).
//
// The fix routes MERGE's property comparison through the same value-equality
// as the `=` operator (cypher/exec/mergePropValueEquals → expr.Value.Equal),
// so MERGE now matches across the Integer/Float boundary. These tests pin:
//   - the cross-type match (no duplicate created);
//   - the absence of side effects on a pure match (stored kind unchanged);
//   - cross-type list elements;
//   - negative controls that must still create two distinct nodes;
//   - a temporal non-regression check via date().

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// nodePropKind walks the graph and returns the PropertyKind stored for the
// first node carrying the given property key. The boolean reports whether any
// such node was found.
func nodePropKind(t *testing.T, g *lpg.Graph[string, float64], key string) (lpg.PropertyKind, bool) {
	t.Helper()
	var (
		kind  lpg.PropertyKind
		found bool
	)
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, nodeKey string) bool {
		props := g.NodeProperties(nodeKey)
		if pv, ok := props[key]; ok {
			kind = pv.Kind()
			found = true
			return false // stop walking
		}
		return true
	})
	return kind, found
}

// TestMerge_CrossType_IntStoredFloatPattern verifies that a node stored as an
// integer is matched by a MERGE whose pattern uses a float literal of equal
// numeric value, so no duplicate is created and the stored value is NOT
// mutated (MERGE writes nothing on a pure match).
func TestMerge_CrossType_IntStoredFloatPattern(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Store an integer property.
	drainRunInTx(t, eng, `CREATE (n:N {x: 1})`)

	// MERGE with a float literal of equal numeric value must match.
	drainRunInTx(t, eng, `MERGE (n:N {x: 1.0})`)

	assertCount(ctx, t, eng, `MATCH (n:N) RETURN count(n) AS n`, 1)

	// Hard invariant: a pure match mutates nothing — the stored value stays
	// an integer; the float literal never reaches storage.
	kind, ok := nodePropKind(t, g, "x")
	if !ok {
		t.Fatal("expected a node carrying property x")
	}
	if kind != lpg.PropInt64 {
		t.Fatalf("stored x kind = %v, want PropInt64 (MERGE must not mutate on match)", kind)
	}
}

// TestMerge_CrossType_FloatStoredIntPattern is the symmetric case: a node
// stored as a float is matched by a MERGE whose pattern uses an integer
// literal of equal numeric value.
func TestMerge_CrossType_FloatStoredIntPattern(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:N {x: 1.0})`)
	drainRunInTx(t, eng, `MERGE (n:N {x: 1})`)

	assertCount(ctx, t, eng, `MATCH (n:N) RETURN count(n) AS n`, 1)

	kind, ok := nodePropKind(t, g, "x")
	if !ok {
		t.Fatal("expected a node carrying property x")
	}
	if kind != lpg.PropFloat64 {
		t.Fatalf("stored x kind = %v, want PropFloat64 (MERGE must not mutate on match)", kind)
	}
}

// TestMerge_CrossType_Relationship verifies the relationship analogue: an
// edge stored with an integer property is matched by a MERGE whose inline
// relationship predicate uses a float literal of equal numeric value, so no
// duplicate edge is created.
func TestMerge_CrossType_Relationship(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed two endpoints and one edge carrying an integer property.
	drainRunInTx(t, eng, `CREATE (a:A {id: 1}), (b:B {id: 2})`)
	drainRunInTx(t, eng,
		`MATCH (a:A {id: 1}), (b:B {id: 2}) MERGE (a)-[:R {w: 1}]->(b)`)

	assertCount(ctx, t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS n`, 1)

	// MERGE with a float literal of equal numeric value must match the
	// existing edge — no duplicate.
	drainRunInTx(t, eng,
		`MATCH (a:A {id: 1}), (b:B {id: 2}) MERGE (a)-[:R {w: 1.0}]->(b)`)

	assertCount(ctx, t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS n`, 1)
}

// TestMerge_CrossType_ListElements verifies that cross-type numeric list
// elements match: a node stored with t:[1,2] (integers) is matched by a
// MERGE whose pattern uses t:[1.0,2.0] (floats).
func TestMerge_CrossType_ListElements(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (n:L {t: [1, 2]})`)
	drainRunInTx(t, eng, `MERGE (n:L {t: [1.0, 2.0]})`)

	assertCount(ctx, t, eng, `MATCH (n:L) RETURN count(n) AS n`, 1)
}

// TestMerge_CrossType_NegativeControls verifies that genuinely different
// values still create two distinct nodes:
//   - distinct integers (1 vs 2);
//   - distinct strings that look numeric ("1" vs "1.0") — string equality is
//     not numeric, so these must NOT collapse;
//   - distinct booleans (true vs false).
func TestMerge_CrossType_NegativeControls(t *testing.T) {
	t.Parallel()

	t.Run("distinct integers", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)
		ctx := context.Background()

		drainRunInTx(t, eng, `MERGE (n:NI {x: 1})`)
		drainRunInTx(t, eng, `MERGE (n:NI {x: 2})`)

		assertCount(ctx, t, eng, `MATCH (n:NI) RETURN count(n) AS n`, 2)
	})

	t.Run("distinct numeric-looking strings", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)
		ctx := context.Background()

		drainRunInTx(t, eng, `MERGE (n:NS {s: "1"})`)
		drainRunInTx(t, eng, `MERGE (n:NS {s: "1.0"})`)

		assertCount(ctx, t, eng, `MATCH (n:NS) RETURN count(n) AS n`, 2)
	})

	t.Run("distinct booleans", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)
		ctx := context.Background()

		drainRunInTx(t, eng, `MERGE (n:NB {b: true})`)
		drainRunInTx(t, eng, `MERGE (n:NB {b: false})`)

		assertCount(ctx, t, eng, `MATCH (n:NB) RETURN count(n) AS n`, 2)
	})
}

// TestMerge_Temporal_NonRegression verifies the unchanged same-kind temporal
// path: two MERGEs with the same date() match to one node, while a different
// date creates a second node. Temporal values stay on the strict same-kind
// comparison path; the cross-type fallback never converts them.
func TestMerge_Temporal_NonRegression(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (n:E {when: date("2024-05-21")})`)
	drainRunInTx(t, eng, `MERGE (n:E {when: date("2024-05-21")})`)

	assertCount(ctx, t, eng, `MATCH (n:E) RETURN count(n) AS n`, 1)

	// A different date is a distinct value — a second node must be created.
	drainRunInTx(t, eng, `MERGE (n:E {when: date("2024-05-22")})`)

	assertCount(ctx, t, eng, `MATCH (n:E) RETURN count(n) AS n`, 2)
}

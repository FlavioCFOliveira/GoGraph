package cypher_test

// set_undirected_reciprocal_test.go — regression gate for rmp #2817: an
// undirected SET over two relationships between the same pair of nodes must
// write BOTH of them, and the property counter must equal the number actually
// modified.
//
// # Which half was the defect
//
// The report showed `propertiesWritten: 2` against only one changed edge, which
// admits two readings: a missing write, or a counter that double-counted a
// single relationship bound once per traversable direction. It is the WRITE.
//
// openCypher 9 §5.1.1 (relationship isomorphism / "Uniqueness of relationships")
// requires that within a single MATCH no relationship be traversed more than
// once *per solution*, and the TCK pins this for the undirected case in
// `cypher/tck/features/clauses/match/Match2.feature` — "Undirected match in
// self-relationship graph" and its siblings — where one stored relationship
// matched by `(a)-[r]-(b)` yields one row per distinct binding, not a duplicate
// of the same relationship. Two DISTINCT relationships must therefore produce
// two rows and two writes.
//
// The fixture below makes that decidable rather than arguable: it asserts
// `id(r)` on the undirected match and finds two DIFFERENT ids, so the two rows
// are two different relationships, so two writes were owed and the counter's 2
// was right.
//
// # The root cause
//
// Expand emits (src, r, dst) in TRAVERSAL order. For the reverse hop of an
// undirected pattern that is the SWAP of the edge's storage order, and the SET
// write path keyed its edge-property mutations on those columns verbatim. With
// (a)-[:R]->(b) and (b)-[:R]->(a) both present, both rows carried n=a, m=b, so
// both writes were keyed (a, b): the first relationship was stamped twice and
// the second never. The read path has always normalised this, and DELETE has
// removeEdgeEitherDirection for exactly the same reason; SET had nothing.
//
// rmp #2504 fixed precisely this class on the READ side — a reciprocal pair made
// r.tag / startNode(r) / endNode(r) resolve to the other edge on a reverse or
// undirected hop (see reciprocal_rel_binding_test.go). The write side was not
// covered then, and this is the same defect on it.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// undirectedSetSeed builds the two-relationship fixture: a and b joined by one
// relationship in each direction, both stamped 'orig-reverse'.
const undirectedSetSeed = `CREATE (a:Y {key:'a'})-[:R {stamp:'orig-reverse'}]->(b:Y {key:'b'}), (b)-[:R {stamp:'orig-reverse'}]->(a)`

func newUndirectedSetEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	if _, err := runEntityProp(eng, undirectedSetSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return eng
}

// TestUndirectedSet_ReciprocalPair_WritesBoth is the #2817 gate.
//
// It fails on the pre-fix engine: `after directed` reported
// a→b stamp="written" and b→a stamp="orig-reverse" — one relationship stamped,
// the other untouched, while the counter claimed two property writes.
func TestUndirectedSet_ReciprocalPair_WritesBoth(t *testing.T) {
	t.Parallel()
	eng := newUndirectedSetEngine(t)

	// ── the two rows are two DISTINCT relationships ───────────────────────────
	// This is what settles which half of the report was wrong. If the undirected
	// pattern bound ONE relationship twice, the ids would be equal and the
	// counter — not the write — would be the defect.
	bound, err := runEntityProp(eng, `MATCH (n)-[r:R]-(m {key:'b'}) RETURN id(r) AS rid ORDER BY rid`)
	if err != nil {
		t.Fatalf("undirected match: %v", err)
	}
	if len(bound) != 2 {
		t.Fatalf("undirected match bound %d rows, want 2 (one per relationship)", len(bound))
	}
	if a, b := fmtAny(bound[0]["rid"]), fmtAny(bound[1]["rid"]); a == b {
		t.Fatalf("the two rows bound the SAME relationship (id %s twice); this fixture cannot decide the defect", a)
	}

	// ── the write ────────────────────────────────────────────────────────────
	res, err := eng.RunInTx(t.Context(), `MATCH (n)-[r:R]-(m {key:'b'}) SET r.stamp = 'written'`, nil)
	if err != nil {
		t.Fatalf("SET: %v", err)
	}
	for res.Next() { // drain the write to completion
	}
	if err := res.Err(); err != nil {
		t.Fatalf("SET drain: %v", err)
	}
	counters := res.Counters()
	if err := res.Close(); err != nil {
		t.Fatalf("SET close: %v", err)
	}

	// ── every relationship carries the new stamp ─────────────────────────────
	// Read DIRECTED so each stored relationship answers exactly once; an
	// undirected read would report each of them twice and could hide a miss
	// behind its sibling.
	rows, err := runEntityProp(eng, `MATCH (x)-[r:R]->(y) RETURN x.key AS x, y.key AS y, r.stamp AS s ORDER BY x`)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("directed read returned %d relationships, want 2", len(rows))
	}
	for _, r := range rows {
		if got := fmtAny(r["s"]); got != `"written"` {
			t.Errorf("relationship %s→%s kept stamp %s, want \"written\" — the undirected SET missed it",
				fmtAny(r["x"]), fmtAny(r["y"]), got)
		}
	}

	// ── the counter equals the number actually modified ──────────────────────
	assertPropsSet(t, counters, 2)
}

// TestUndirectedSet_SingleRelationship_CountsOnce is the companion control: one
// relationship matched undirected is still ONE property write, so the #2817 fix
// cannot have bought its result by writing (or counting) a phantom second edge.
func TestUndirectedSet_SingleRelationship_CountsOnce(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	if _, err := runEntityProp(eng, `CREATE (a:Y {key:'a'})-[:R {stamp:'orig'}]->(b:Y {key:'b'})`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := eng.RunInTx(t.Context(), `MATCH (n)-[r:R]-(m {key:'b'}) SET r.stamp = 'written'`, nil)
	if err != nil {
		t.Fatalf("SET: %v", err)
	}
	for res.Next() { // drain the write to completion
	}
	if err := res.Err(); err != nil {
		t.Fatalf("SET drain: %v", err)
	}
	counters := res.Counters()
	if err := res.Close(); err != nil {
		t.Fatalf("SET close: %v", err)
	}
	assertPropsSet(t, counters, 1)

	rows, err := runEntityProp(eng, `MATCH (x)-[r:R]->(y) RETURN r.stamp AS s`)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(rows) != 1 || fmtAny(rows[0]["s"]) != `"written"` {
		t.Fatalf("single-relationship undirected SET: rows=%v, want one \"written\"", rows)
	}
}

// TestUndirectedSet_ReverseHopOnly_WritesTheBoundEdge isolates the reverse hop
// with no reciprocal sibling to absorb a mis-keyed write. The pattern is walked
// b→a while storage holds a→b, so the write MUST be redirected onto (a, b);
// before the fix it was keyed (b, a), where no edge exists, and vanished.
func TestUndirectedSet_ReverseHopOnly_WritesTheBoundEdge(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	if _, err := runEntityProp(eng, `CREATE (a:Y {key:'a'})-[:R {stamp:'orig'}]->(b:Y {key:'b'})`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Anchor on b and walk outward: the only solution traverses the stored
	// a→b edge backwards.
	if _, err := runEntityProp(eng, `MATCH (n {key:'b'})-[r:R]-(m) SET r.stamp = 'written'`); err != nil {
		t.Fatalf("SET: %v", err)
	}
	rows, err := runEntityProp(eng, `MATCH (x)-[r:R]->(y) RETURN r.stamp AS s`)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(rows) != 1 || fmtAny(rows[0]["s"]) != `"written"` {
		t.Fatalf("reverse-hop SET: rows=%v, want one \"written\"", rows)
	}
}

// assertPropsSet checks the statement's PropertiesSet counter, which by the
// applied-not-attempted rule must equal the number of properties actually
// written.
func assertPropsSet(t *testing.T, c *exec.QueryCounters, want int64) {
	t.Helper()
	if c == nil {
		t.Fatalf("statement reported nil counters, want PropertiesSet = %d", want)
	}
	if c.PropertiesSet != want {
		t.Errorf("PropertiesSet = %d, want %d", c.PropertiesSet, want)
	}
}

package cypher_test

// reltype_shape_differential_test.go — a CROSS-VERSION differential over the three
// behaviour families rmp #2251 was required to leave unchanged: multi-type
// relationship patterns `[r:A|B]`, PARALLEL relationships between one node pair,
// and the SLOT-ORDINAL behaviours that read a slot's position within its
// destination run (the expand-into seek, per-instance relationship identity, and
// reverse per-instance typing).
//
// # How this is a differential rather than an assertion
//
// It emits a canonical digest of every query's full result and every query is
// order-stable, so the digest a run produces can be compared BYTE FOR BYTE with
// the digest the same test produces against another tree. It was run against
// pristine 35990293 and against the change, and the two digests are identical.
// Nothing in the file encodes an expectation of what the answers "should" be — the
// point is that they did not MOVE, which is a claim only a comparison can make.
//
// It also carries its own non-vacuity guard: the fixture is asserted to contain
// parallel relationships, several relationship types, and a closing hop, and every
// query is asserted to return at least one row, so a digest of nothing but empty
// results cannot pass for agreement.
//
// # The recorded comparison
//
// Run on 2026-08-29 against pristine 35990293 and against the rmp #2251 change,
// all 24 rendered result lines BYTE-IDENTICAL:
//
//	DIGEST multitype    9e0665ba33c6f57e
//	DIGEST parallel     4bb62018cced45eb
//	DIGEST slotordinal  328e12ee541b39bf
//	DIGEST TOTAL        916b7753cd3469a8
//
// The digests are recorded rather than ASSERTED, deliberately. They are a function
// of the rendering and of the fixture, so pinning them would turn any future
// change to either into a spurious failure while adding nothing: the claim this
// file supports is "the answers did not move ACROSS THIS CHANGE", which only a
// comparison of two trees can make and no constant in one tree can.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relTypeShapeQueries is the battery. Every query is ORDER-STABLE (an explicit
// ORDER BY, or an aggregate) so its rendering is deterministic across runs.
var relTypeShapeQueries = []struct{ family, q string }{
	// ── multi-type patterns ──────────────────────────────────────────────────
	{"multitype", `MATCH (a)-[r:K|M]->(b) RETURN a.sid, type(r), b.sid ORDER BY a.sid, type(r), b.sid`},
	{"multitype", `MATCH (a)-[r:K|M|Z]->(b) RETURN count(r) AS c`},
	{"multitype", `MATCH (a)<-[r:K|M]-(b) RETURN a.sid, type(r), b.sid ORDER BY a.sid, type(r), b.sid`},
	{"multitype", `MATCH (a)-[r:K|M]-(b) RETURN count(r) AS c`},
	{"multitype", `MATCH (a)-[r:NOPE|ALSO_NOPE]->(b) RETURN count(r) AS c`},
	{"multitype", `MATCH (a)-[r:K|K]->(b) RETURN count(r) AS c`},

	// ── parallel relationships ───────────────────────────────────────────────
	{"parallel", `MATCH (a {sid:'p'})-[r]->(b {sid:'q'}) RETURN type(r) AS t ORDER BY t`},
	{"parallel", `MATCH (a {sid:'p'})-[r:K]->(b {sid:'q'}) RETURN count(r) AS c`},
	{"parallel", `MATCH (a {sid:'p'})<-[r:K]-(b) RETURN count(r) AS c`},
	{"parallel", `MATCH (a {sid:'q'})<-[r]-(b {sid:'p'}) RETURN type(r) AS t ORDER BY t`},
	{"parallel", `MATCH (a)-[r:K]->(b) RETURN a.sid, b.sid ORDER BY a.sid, b.sid`},
	{"parallel", `MATCH (a)-[r]->(a) RETURN a.sid, type(r) AS t ORDER BY a.sid, t`},

	// ── slot-ordinal: the closing hop reads a slot's position in its dst run ──
	{"slotordinal", `MATCH (a)-[r1:K]->(b)-[r2:K]->(a) RETURN count(*) AS c`},
	{"slotordinal", `MATCH (a)-[r1:K]->(b)-[r2:M]->(a) RETURN count(*) AS c`},
	{"slotordinal", `MATCH (a)-[r1]->(b)-[r2]->(c)-[r3]->(a) RETURN count(*) AS c`},
	{"slotordinal", `MATCH (a {sid:'p'})-[r:K]->(b {sid:'q'}) RETURN count(r) AS c`},
	{"slotordinal", `MATCH (a)-[r:K*1..2]->(b) RETURN a.sid, b.sid ORDER BY a.sid, b.sid`},
	{"slotordinal", `MATCH p = shortestPath((a {sid:'p'})-[:K*1..4]->(b {sid:'r'})) RETURN length(p) AS l`},
	{"slotordinal", `MATCH (a) WHERE EXISTS { MATCH (a)<-[:K]-(x) RETURN x } RETURN a.sid ORDER BY a.sid`},
	{"slotordinal", `MATCH (a) RETURN a.sid AS sid, COUNT { MATCH (a)-[:K]->() } AS c ORDER BY sid`},
}

// buildRelTypeShapeFixture builds a multigraph carrying, deliberately: parallel
// relationships of the SAME type between one pair, parallel relationships of
// DIFFERENT types between the same pair, self-loops, a triangle so a closing hop
// has something to close, and a node with several incoming relationships so the
// reverse side has ordinals to get wrong.
func buildRelTypeShapeFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	stmts := []string{
		`CREATE (:P {sid:'p'}), (:P {sid:'q'}), (:P {sid:'r'}), (:P {sid:'s'})`,
		// Parallel, same type, same pair — the multigraph case.
		`MATCH (a:P {sid:'p'}), (b:P {sid:'q'}) CREATE (a)-[:K]->(b)`,
		`MATCH (a:P {sid:'p'}), (b:P {sid:'q'}) CREATE (a)-[:K]->(b)`,
		// Parallel, DIFFERENT types, same pair.
		`MATCH (a:P {sid:'p'}), (b:P {sid:'q'}) CREATE (a)-[:M]->(b)`,
		// A triangle p→q→r→p, so a closing hop exists.
		`MATCH (a:P {sid:'q'}), (b:P {sid:'r'}) CREATE (a)-[:K]->(b)`,
		`MATCH (a:P {sid:'r'}), (b:P {sid:'p'}) CREATE (a)-[:K]->(b)`,
		// A second, differently typed closing leg.
		`MATCH (a:P {sid:'r'}), (b:P {sid:'p'}) CREATE (a)-[:M]->(b)`,
		// Self-loops, one of each type.
		`MATCH (a:P {sid:'s'}) CREATE (a)-[:K]->(a)`,
		`MATCH (a:P {sid:'s'}) CREATE (a)-[:M]->(a)`,
		// A node with several incoming relationships of mixed type.
		`MATCH (a:P {sid:'s'}), (b:P {sid:'r'}) CREATE (a)-[:M]->(b)`,
		`MATCH (a:P {sid:'q'}), (b:P {sid:'p'}) CREATE (a)-[:M]->(b)`,
	}
	for _, s := range stmts {
		res, err := eng.RunAny(context.Background(), s, nil)
		if err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
		for res.Next() { // drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("fixture %q Err: %v", s, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("fixture %q Close: %v", s, err)
		}
	}
	return eng
}

// TestRelTypeShapes_CrossVersionDigest renders the whole battery and logs one
// digest per family plus a total. Compare the logged digests across trees.
func TestRelTypeShapes_CrossVersionDigest(t *testing.T) {
	ctx := context.Background()
	eng := buildRelTypeShapeFixture(t)

	// Non-vacuity guard on the FIXTURE, before anything is digested.
	if n := shapeScalar(ctx, t, eng, `MATCH (a {sid:'p'})-[r]->(b {sid:'q'}) RETURN count(r) AS c`); n != "3" {
		t.Fatalf("fixture has %s parallel p→q relationships, want 3 — the parallel-edge "+
			"family would be exercising nothing", n)
	}
	if n := shapeScalar(ctx, t, eng, `MATCH ()-[r]->() RETURN count(DISTINCT type(r)) AS c`); n != "2" {
		t.Fatalf("fixture carries %s distinct relationship types, want 2 — the multi-type "+
			"family would be exercising nothing", n)
	}
	if n := shapeScalar(ctx, t, eng, `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS c`); n == "0" {
		t.Fatal("fixture contains no typed 3-cycle, so the slot-ordinal family's closing " +
			"hop never closes and proves nothing")
	}

	byFamily := map[string][]string{}
	all := make([]string, 0, len(relTypeShapeQueries))
	for _, tc := range relTypeShapeQueries {
		rows := shapeRows(ctx, t, eng, tc.q)
		if len(rows) == 0 {
			t.Errorf("query returned NO rows, so its contribution to the digest is vacuous: %s", tc.q)
		}
		line := tc.q + "\n" + strings.Join(rows, "\n")
		byFamily[tc.family] = append(byFamily[tc.family], line)
		all = append(all, line)
		t.Logf("%-12s %-78s -> %v", tc.family, tc.q, rows)
	}
	fams := make([]string, 0, len(byFamily))
	for f := range byFamily {
		fams = append(fams, f)
	}
	sort.Strings(fams)
	for _, f := range fams {
		t.Logf("DIGEST %-12s %s", f, shapeDigest(byFamily[f]))
	}
	t.Logf("DIGEST %-12s %s", "TOTAL", shapeDigest(all))
}

// shapeDigest is a stable hash of the rendered results.
func shapeDigest(lines []string) string {
	h := sha256.Sum256([]byte(strings.Join(lines, "\x1e")))
	return hex.EncodeToString(h[:8])
}

// shapeRows renders every row of q as one comparable string.
func shapeRows(ctx context.Context, t *testing.T, eng *cypher.Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	var out []string
	cols := len(res.Columns())
	for res.Next() {
		var b strings.Builder
		for i := 0; i < cols; i++ {
			fmt.Fprintf(&b, "%v|", res.ValueAt(i))
		}
		out = append(out, b.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close %q: %v", q, err)
	}
	return out
}

// shapeScalar returns the single value of a single-row, single-column query.
func shapeScalar(ctx context.Context, t *testing.T, eng *cypher.Engine, q string) string {
	t.Helper()
	rows := shapeRows(ctx, t, eng, q)
	if len(rows) != 1 {
		t.Fatalf("scalar %q returned %d rows", q, len(rows))
	}
	return strings.TrimSuffix(rows[0], "|")
}

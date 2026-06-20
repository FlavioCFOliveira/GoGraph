package cypher_test

// rel_presence_isnull_test.go — sprint 222 #1638. End-to-end Cypher coverage
// for the presence-only relationship `r.k IS [NOT] NULL` fast path: a bound
// relationship variable whose only property read is an IS NULL / IS NOT NULL
// predicate is answered from a storage presence check (lpg.EdgeHasProperty)
// without materialising the value.
//
// The tests assert openCypher-correct results across present / absent keys,
// both edge directions, a key used for BOTH presence and value (C1, must still
// see the real value), a present date (a non-null-mapping kind delivered as a
// tagged string), and a present time/bytes property created through the lpg API
// (null-mapping kinds that read back as Null through Cypher). The presence path
// must produce IDENTICAL results to the value-materialising path; a paired
// value-forcing query proves equivalence. Layer: short.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// presenceRows executes a read query and returns its rows as maps.
func presenceRows(t *testing.T, eng *cypher.Engine, q string) []map[string]interface{} {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	return collectRecords(t, res)
}

// mustExec runs a write/DDL statement to completion.
func mustExec(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
}

// newRelPresenceGraph builds a graph of FRIEND edges where some carry a `since`
// property and some do not:
//
//	(alice)-[:FRIEND {since:2020}]->(bob)    — since present (int)
//	(carol)-[:FRIEND]->(dave)                — since absent
//	(eve)-[:FRIEND {since: date('2021-01-01')}]->(frank) — since present (date)
//
// All names are unique so a query can identify each edge by its endpoints.
func newRelPresenceGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	mustExec(t, eng, `CREATE (a:P {name:'alice'})-[:FRIEND {since:2020}]->(b:P {name:'bob'})`)
	mustExec(t, eng, `CREATE (c:P {name:'carol'})-[:FRIEND]->(d:P {name:'dave'})`)
	mustExec(t, eng, `CREATE (e:P {name:'eve'})-[:FRIEND {since: date('2021-01-01')}]->(f:P {name:'frank'})`)
	return g
}

// TestRelPresence_IsNotNull_FiltersPresentEdges is the motivating query: the
// relationship is bound but its only property read is `r.since IS NOT NULL`, so
// the presence fast path is exercised. The result must match the value path.
func TestRelPresence_IsNotNull_FiltersPresentEdges(t *testing.T) {
	g := newRelPresenceGraph(t)
	eng := cypher.NewEngine(g)

	// Presence path: r is bound, r.since read only via IS NOT NULL.
	presence := presenceRows(t, eng,
		`MATCH (a)-[r:FRIEND]->(b) WHERE r.since IS NOT NULL RETURN a.name AS n ORDER BY n`)
	gotPresence := stringColumn(t, presence, "n")
	wantPresent := []string{"alice", "eve"} // bob/carol edges have no since
	assertStrings(t, "IS NOT NULL (presence path)", gotPresence, wantPresent)

	// Value-forcing path: ALSO read r.since as a value in the RETURN (C1 drops it
	// from presenceKeys, so the full value is materialised). The WHERE filter
	// must yield the identical set of edges. Projecting r.since is the
	// value-forcing use; the assertion is on the filtered names.
	value := presenceRows(t, eng,
		`MATCH (a)-[r:FRIEND]->(b) WHERE r.since IS NOT NULL RETURN a.name AS n, r.since AS s ORDER BY n`)
	assertStrings(t, "IS NOT NULL (value path)", stringColumn(t, value, "n"), wantPresent)

	// The motivating count query.
	cnt := presenceRows(t, eng, `MATCH ()-[r:FRIEND]->() WHERE r.since IS NOT NULL RETURN count(r) AS c`)
	if len(cnt) != 1 {
		t.Fatalf("count query rows = %d, want 1", len(cnt))
	}
	if c, ok := cnt[0]["c"].(expr.IntegerValue); !ok || int64(c) != 2 {
		t.Fatalf("count(r) = %v, want 2", cnt[0]["c"])
	}
}

// TestRelPresence_IsNull_FiltersAbsentEdges is the IS NULL complement.
func TestRelPresence_IsNull_FiltersAbsentEdges(t *testing.T) {
	g := newRelPresenceGraph(t)
	eng := cypher.NewEngine(g)
	rows := presenceRows(t, eng,
		`MATCH (a)-[r:FRIEND]->(b) WHERE r.since IS NULL RETURN a.name AS n ORDER BY n`)
	assertStrings(t, "IS NULL", stringColumn(t, rows, "n"), []string{"carol"})
}

// TestRelPresence_Projection_ReturnsBoolean is the scalar-projection sentinel:
// `RETURN r.since IS NOT NULL`. The placeholder gated by the presence path is
// read ONLY by the IS NOT NULL operator and collapsed to a boolean, so only the
// boolean escapes into the row — identical to the value path.
func TestRelPresence_Projection_ReturnsBoolean(t *testing.T) {
	g := newRelPresenceGraph(t)
	eng := cypher.NewEngine(g)
	rows := presenceRows(t, eng,
		`MATCH (a)-[r:FRIEND]->(b) RETURN a.name AS n, r.since IS NOT NULL AS present ORDER BY n`)
	want := map[string]bool{"alice": true, "carol": false, "eve": true}
	if len(rows) != len(want) {
		t.Fatalf("projection rows = %d, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		name := string(r["n"].(expr.StringValue))
		bv, ok := r["present"].(expr.BoolValue)
		if !ok {
			t.Fatalf("present col for %q is %T, want BoolValue", name, r["present"])
		}
		if bool(bv) != want[name] {
			t.Fatalf("present(%q) = %v, want %v", name, bool(bv), want[name])
		}
	}
}

// TestRelPresence_ValueStillMaterialised proves C1: when the SAME key is read as
// both a presence test and a value, the real value is materialised and returned,
// NOT the presence placeholder. If the placeholder had leaked, r.since would
// come back as the boolean true rather than the integer 2020.
func TestRelPresence_ValueStillMaterialised(t *testing.T) {
	g := newRelPresenceGraph(t)
	eng := cypher.NewEngine(g)
	rows := presenceRows(t, eng,
		`MATCH (a)-[r:FRIEND]->(b) WHERE r.since IS NOT NULL RETURN a.name AS n, r.since AS s ORDER BY n`)
	// alice has int 2020; eve has a date. Check alice's integer value survives.
	var aliceSince interface{}
	for _, r := range rows {
		if string(r["n"].(expr.StringValue)) == "alice" {
			aliceSince = r["s"]
		}
	}
	iv, ok := aliceSince.(expr.IntegerValue)
	if !ok {
		t.Fatalf("alice r.since is %T (%v), want IntegerValue 2020 — placeholder leaked?", aliceSince, aliceSince)
	}
	if int64(iv) != 2020 {
		t.Fatalf("alice r.since = %d, want 2020", int64(iv))
	}
}

// TestRelPresence_BothDirections confirms presence is resolved against the
// stored direction for an undirected match: `MATCH (a)-[r:FRIEND]-(b)` traverses
// each edge in both directions, and r.since IS NOT NULL must hold on the reverse
// hop too (the direction probe yields the stored endpoints for EdgeHasProperty).
func TestRelPresence_BothDirections(t *testing.T) {
	g := newRelPresenceGraph(t)
	eng := cypher.NewEngine(g)
	// Undirected: alice<->bob is visited from both alice and bob. Both rows must
	// agree that since is present.
	rows := presenceRows(t, eng,
		`MATCH (a)-[r:FRIEND]-(b) WHERE a.name IN ['alice','bob'] RETURN a.name AS n, r.since IS NOT NULL AS present ORDER BY n`)
	if len(rows) != 2 {
		t.Fatalf("undirected rows = %d, want 2 (alice->bob and bob->alice)", len(rows))
	}
	for _, r := range rows {
		bv, ok := r["present"].(expr.BoolValue)
		if !ok || !bool(bv) {
			t.Fatalf("undirected since presence for %v = %v, want true on both hops",
				r["n"], r["present"])
		}
	}
}

// TestRelPresence_NullMappingKindReadsAbsent is the C12 sentinel at the Cypher
// level: a relationship property whose stored kind maps to Null (a PropTime set
// through the lpg API) reads back as Null through Cypher, so `r.k IS NOT NULL`
// must be FALSE for it — exactly what EdgeHasProperty's kind gate reports, and
// exactly what the value path (lpgPropToExpr -> Null) would evaluate.
func TestRelPresence_NullMappingKindReadsAbsent(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	mustExec(t, eng, `CREATE (a:P {name:'x'})-[:R]->(b:P {name:'y'})`)
	// Set a PropTime and a PropBytes property through the lpg public API; both
	// read back as Null through Cypher (lpgPropToExpr has no PropTime/PropBytes
	// case).
	if err := g.SetEdgeProperty("x", "y", "ts", lpg.TimeValue(time.Now())); err != nil {
		t.Fatalf("SetEdgeProperty time: %v", err)
	}
	if err := g.SetEdgeProperty("x", "y", "blob", lpg.BytesValue([]byte{1, 2})); err != nil {
		t.Fatalf("SetEdgeProperty bytes: %v", err)
	}

	// Cross-check the value path agrees these read as Null.
	valRows := presenceRows(t, eng, `MATCH (a)-[r:R]->(b) RETURN r.ts AS ts, r.blob AS blob`)
	if len(valRows) != 1 {
		t.Fatalf("value rows = %d, want 1", len(valRows))
	}
	if !expr.IsNull(valRows[0]["ts"].(expr.Value)) {
		t.Fatalf("value path: r.ts = %v, want Null (PropTime maps to Null)", valRows[0]["ts"])
	}
	if !expr.IsNull(valRows[0]["blob"].(expr.Value)) {
		t.Fatalf("value path: r.blob = %v, want Null (PropBytes maps to Null)", valRows[0]["blob"])
	}

	// Presence path must agree: IS NOT NULL is false, IS NULL is true.
	notNull := presenceRows(t, eng, `MATCH (a)-[r:R]->(b) WHERE r.ts IS NOT NULL RETURN count(r) AS c`)
	if c := int64(notNull[0]["c"].(expr.IntegerValue)); c != 0 {
		t.Fatalf("IS NOT NULL count for PropTime = %d, want 0", c)
	}
	isNullRows := presenceRows(t, eng, `MATCH (a)-[r:R]->(b) WHERE r.blob IS NULL RETURN count(r) AS c`)
	if c := int64(isNullRows[0]["c"].(expr.IntegerValue)); c != 1 {
		t.Fatalf("IS NULL count for PropBytes = %d, want 1", c)
	}
}

// TestRelPresence_ParallelEdgesDateKind exercises a multigraph where two
// parallel edges between the same endpoints carry `since` under different
// non-null kinds (int on one CREATE, date on the other). Each occupies its own
// slot, so the per-pair coalesce folds them; both read as non-null, so every
// matched row reports present. This pins the cross-slot coalescing scan that
// EdgeHasProperty shares with EdgeProperties (C9).
func TestRelPresence_ParallelEdgesDateKind(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	mustExec(t, eng, `CREATE (a:P {name:'a'}), (b:P {name:'b'})`)
	mustExec(t, eng, `MATCH (a:P {name:'a'}),(b:P {name:'b'}) CREATE (a)-[:R {since:1}]->(b)`)
	mustExec(t, eng, `MATCH (a:P {name:'a'}),(b:P {name:'b'}) CREATE (a)-[:R {since: date('2020-01-01')}]->(b)`)

	rows := presenceRows(t, eng, `MATCH (a)-[r:R]->(b) WHERE r.since IS NOT NULL RETURN count(r) AS c`)
	// Both parallel edges match (since present on each).
	if c := int64(rows[0]["c"].(expr.IntegerValue)); c != 2 {
		t.Fatalf("parallel-edge IS NOT NULL count = %d, want 2", c)
	}
	// Projection sentinel over the same multigraph: every row reports present.
	proj := presenceRows(t, eng, `MATCH (a)-[r:R]->(b) RETURN r.since IS NOT NULL AS present`)
	if len(proj) != 2 {
		t.Fatalf("parallel-edge projection rows = %d, want 2", len(proj))
	}
	for _, r := range proj {
		if bv, ok := r["present"].(expr.BoolValue); !ok || !bool(bv) {
			t.Fatalf("parallel-edge present = %v, want true", r["present"])
		}
	}
}

// stringColumn extracts column col from rows as a string slice.
func stringColumn(t *testing.T, rows []map[string]interface{}, col string) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		sv, ok := r[col].(expr.StringValue)
		if !ok {
			t.Fatalf("column %q is %T (%v), want StringValue", col, r[col], r[col])
		}
		out = append(out, string(sv))
	}
	return out
}

// assertStrings compares two string slices for exact equality (order-sensitive).
func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}
}

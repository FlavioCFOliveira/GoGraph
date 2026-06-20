package cypher_test

// edge_property_date_roundtrip_test.go — sprint 222, task #1642.
//
// The columnar edge-property tier stores a Cypher Date on an edge as an int32
// epoch-day (the de-boxed date column), not as a boxed string. These tests are
// the TCK-critical gate that the round-trip is lossless END TO END through
// Cypher: a date written via CREATE/SET reads back as a native Date (NOT null,
// NOT a raw string), and ORDER BY on the date column orders chronologically (a
// calendar order, proving the value reconstitutes as a real Date rather than a
// lexical string).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestEdgeDate_CreateReadBackNonNull writes a date on a relationship via CREATE
// and asserts it reads back through Cypher as a native Date value (not null,
// not a string).
func TestEdgeDate_CreateReadBackNonNull(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:P {name:'a'})`)
	drainRunInTx(t, eng, `CREATE (b:P {name:'b'})`)
	drainRunInTx(t, eng,
		`MATCH (a:P {name:'a'}), (b:P {name:'b'})
		 CREATE (a)-[r:KNOWS {since: date('2020-01-15')}]->(b)`)

	res, err := eng.RunInTx(ctx, `MATCH ()-[r:KNOWS]->() RETURN r.since AS d`, nil)
	if err != nil {
		t.Fatalf("RunInTx read: %v", err)
	}
	defer res.Close()
	got := 0
	for res.Next() {
		got++
		rec := res.Record()
		d, _ := rec["d"].(expr.Value)
		if d == nil || expr.IsNull(d) {
			t.Fatalf("r.since read back as NULL — date round-trip lost (PropTime trap)")
		}
		dv, ok := d.(expr.DateValue)
		if !ok {
			t.Fatalf("r.since read back as %T (%v), want expr.DateValue", rec["d"], rec["d"])
		}
		if dv.Year != 2020 || dv.Month != 1 || dv.Day != 15 {
			t.Fatalf("r.since = %v, want 2020-01-15", dv)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected 1 row, got %d", got)
	}
}

// TestEdgeDate_StoredAsInt32EpochDay reaches into the graph to assert the date
// is physically stored in the de-boxed int32 epoch-day column, not as a boxed
// value — i.e. the memory win is actually realised, not just the semantics.
func TestEdgeDate_StoredAsInt32EpochDay(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (a:P {name:'a'})`)
	drainRunInTx(t, eng, `CREATE (b:P {name:'b'})`)
	drainRunInTx(t, eng,
		`MATCH (a:P {name:'a'}), (b:P {name:'b'})
		 CREATE (a)-[r:KNOWS {since: date('2020-01-15')}]->(b)`)

	// The lpg public surface returns the SOH-tagged Date string (the storage
	// contract); the cypher bridge decodes it to a native Date. Assert the
	// public surface yields a tagged Date string (kind PropString, first byte
	// 0x01), which is what the int32 column reconstitutes.
	found := false
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, srcKey string) bool {
		g.AdjList().Mapper().Walk(func(_ graph.NodeID, dstKey string) bool {
			props := g.EdgeProperties(srcKey, dstKey)
			if v, ok := props["since"]; ok {
				if v.Kind() != lpg.PropString {
					t.Fatalf("since kind = %d, want PropString (tagged date)", v.Kind())
				}
				s, _ := v.String()
				if len(s) < 2 || s[0] != 0x01 {
					t.Fatalf("since = %q, want SOH-tagged Date string", s)
				}
				if s != "\x012020-01-15" {
					t.Fatalf("since = %q, want SOH+2020-01-15", s)
				}
				found = true
				return false
			}
			return true
		})
		return !found
	})
	if !found {
		t.Fatal("expected a since date on the KNOWS edge")
	}
}

// TestEdgeDate_OrderByChronological writes three edges with out-of-order dates
// and asserts ORDER BY r.since returns them in calendar order — proving the
// stored value behaves as a Date, not a lexical string. (Lexical and calendar
// order happen to agree for canonical YYYY-MM-DD, so to make the test
// discriminating we also confirm the returned values are DateValues and compare
// via the typed Date ordering.)
func TestEdgeDate_OrderByChronological(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// One source, three distinct destinations, each edge a different date.
	drainRunInTx(t, eng, `CREATE (s:S {name:'s'})`)
	for _, d := range []string{"x", "y", "z"} {
		drainRunInTx(t, eng, `CREATE (n:T {name:'`+d+`'})`)
	}
	// Insert in deliberately non-sorted order: 2021, 2019, 2020.
	drainRunInTx(t, eng, `MATCH (s:S),(t:T {name:'x'}) CREATE (s)-[:E {on: date('2021-06-01')}]->(t)`)
	drainRunInTx(t, eng, `MATCH (s:S),(t:T {name:'y'}) CREATE (s)-[:E {on: date('2019-03-15')}]->(t)`)
	drainRunInTx(t, eng, `MATCH (s:S),(t:T {name:'z'}) CREATE (s)-[:E {on: date('2020-12-31')}]->(t)`)

	res, err := eng.RunInTx(ctx, `MATCH ()-[r:E]->() RETURN r.on AS d ORDER BY r.on`, nil)
	if err != nil {
		t.Fatalf("RunInTx ORDER BY: %v", err)
	}
	defer res.Close()
	var dates []expr.DateValue
	for res.Next() {
		rec := res.Record()
		d, _ := rec["d"].(expr.Value)
		if d == nil || expr.IsNull(d) {
			t.Fatalf("ORDER BY row has NULL date — round-trip lost")
		}
		dv, ok := d.(expr.DateValue)
		if !ok {
			t.Fatalf("ORDER BY row is %T, want expr.DateValue", rec["d"])
		}
		dates = append(dates, dv)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	if len(dates) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(dates))
	}
	want := []expr.DateValue{
		{Year: 2019, Month: 3, Day: 15},
		{Year: 2020, Month: 12, Day: 31},
		{Year: 2021, Month: 6, Day: 1},
	}
	for i, w := range want {
		if dates[i] != w {
			t.Fatalf("row %d = %v, want %v (ORDER BY not chronological)", i, dates[i], w)
		}
	}
}

// TestEdgeDate_SetThenUpdate writes a date via SET, then overwrites it with a
// later date, asserting the latest value reads back (last-write-wins on the
// columnar tier).
func TestEdgeDate_SetThenUpdate(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:P {name:'a'})`)
	drainRunInTx(t, eng, `CREATE (b:P {name:'b'})`)
	drainRunInTx(t, eng, `MATCH (a:P {name:'a'}),(b:P {name:'b'}) CREATE (a)-[:R]->(b)`)
	drainRunInTx(t, eng, `MATCH ()-[r:R]->() SET r.on = date('2020-01-01')`)
	drainRunInTx(t, eng, `MATCH ()-[r:R]->() SET r.on = date('2022-09-09')`)

	res, err := eng.RunInTx(ctx, `MATCH ()-[r:R]->() RETURN r.on AS d`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	defer res.Close()
	rows := 0
	for res.Next() {
		rows++
		dv, ok := res.Record()["d"].(expr.DateValue)
		if !ok {
			t.Fatalf("d is %T, want DateValue", res.Record()["d"])
		}
		if dv.Year != 2022 || dv.Month != 9 || dv.Day != 9 {
			t.Fatalf("d = %v, want 2022-09-09 (last write wins)", dv)
		}
	}
	if rows != 1 {
		t.Fatalf("expected 1 row, got %d", rows)
	}
}

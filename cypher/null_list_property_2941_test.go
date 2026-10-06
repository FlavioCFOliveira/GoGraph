package cypher_test

// null_list_property_2941_test.go — regression gate for rmp #2941.
//
// A list property value containing null cannot be stored (openCypher 9 restricts
// a property to a primitive or a list of primitives; TCK Set1 [10] classifies an
// unstorable list as InvalidPropertyType; Neo4j documents that a stored list
// cannot contain null). Before the fix:
//
//   - SET x.p = [1, null, 2], SET x += {p: [3, null]} and the MERGE SET forms
//     reported success with no counters and kept the old value;
//   - CREATE/MERGE with a literal map {u: [null, 'x']} stored the list with the
//     nulls removed.
//
// Every clause form, on a node and on a relationship, must now fail with
// InvalidPropertyType (exec.ErrNullListElement), leave the graph exactly as it
// was, and report no counters — in memory and on the WAL-backed engine, where
// the recovered graph must match as well.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const fixture2941 = `CREATE (a:N {id: 1, p: 'old'})-[:R {p: 'old'}]->(b:N {id: 2})`

// fingerprint2941 is the whole observable state the refused statements could
// touch: node and relationship counts, and both entities' full property maps.
const fingerprint2941 = `MATCH (n) WITH count(n) AS nc
OPTIONAL MATCH ()-[r]->() WITH nc, count(r) AS rc
MATCH (a:N {id: 1})-[r:R]->()
RETURN nc, rc, properties(a) AS ap, properties(r) AS rp`

type case2941 struct {
	name   string
	query  string
	params map[string]expr.Value
}

func nullList2941() expr.Value {
	return expr.ListValue{expr.IntegerValue(1), expr.Null}
}

func cases2941() []case2941 {
	onNode := `MATCH (x:N {id: 1}) `
	onRel := `MATCH (:N {id: 1})-[x:R]->() `
	cs := make([]case2941, 0, 32)
	for _, tgt := range []struct{ kind, match string }{{"node", onNode}, {"rel", onRel}} {
		cs = append(cs,
			case2941{tgt.kind + "/SET prop literal", tgt.match + `SET x.p = [1, null, 2]`, nil},
			case2941{tgt.kind + "/SET prop expression", tgt.match + `SET x.p = [1, x.missing]`, nil},
			case2941{tgt.kind + "/SET prop parameter", tgt.match + `SET x.p = $l`, map[string]expr.Value{"l": nullList2941()}},
			case2941{tgt.kind + "/SET += literal", tgt.match + `SET x += {u: [3, null]}`, nil},
			case2941{tgt.kind + "/SET += expression", tgt.match + `SET x += {u: [3, x.missing]}`, nil},
			case2941{tgt.kind + "/SET += parameter", tgt.match + `SET x += $m`, map[string]expr.Value{"m": expr.MapValue{"u": nullList2941()}}},
			case2941{tgt.kind + "/SET = literal", tgt.match + `SET x = {u: [3, null]}`, nil},
			case2941{tgt.kind + "/SET = expression", tgt.match + `SET x = {u: [3, x.missing]}`, nil},
			case2941{tgt.kind + "/SET = parameter", tgt.match + `SET x = $m`, map[string]expr.Value{"m": expr.MapValue{"u": nullList2941()}}},
		)
	}
	cs = append(cs,
		case2941{"node/MERGE ON MATCH SET literal", `MERGE (x:N {id: 1}) ON MATCH SET x.p = [1, null]`, nil},
		case2941{"node/MERGE ON MATCH SET expression", `MERGE (x:N {id: 1}) ON MATCH SET x.p = [1, x.missing]`, nil},
		case2941{"node/MERGE ON MATCH SET +=", `MERGE (x:N {id: 1}) ON MATCH SET x += {u: [1, null]}`, nil},
		case2941{"node/MERGE ON CREATE SET literal", `MERGE (x:N {id: 9}) ON CREATE SET x.p = [1, null]`, nil},
		case2941{"rel/MERGE ON MATCH SET literal", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[x:R]->(b) ON MATCH SET x.p = [1, null]`, nil},
		case2941{"rel/MERGE ON MATCH SET expression", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[x:R]->(b) ON MATCH SET x.p = [1, x.missing]`, nil},
		case2941{"rel/MERGE ON CREATE SET literal", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[x:S]->(b) ON CREATE SET x.p = [1, null]`, nil},
		case2941{"node/CREATE literal", `CREATE (:C {u: [null, 'x']})`, nil},
		case2941{"node/CREATE parameter", `CREATE (:C {u: $l})`, map[string]expr.Value{"l": nullList2941()}},
		case2941{"node/CREATE expression", `MATCH (a:N {id: 1}) CREATE (:C {u: [a.missing, 'x']})`, nil},
		case2941{"rel/CREATE literal", `MATCH (a:N {id: 1}), (b:N {id: 2}) CREATE (a)-[:RC {u: [null, 'x']}]->(b)`, nil},
		case2941{"rel/CREATE parameter", `MATCH (a:N {id: 1}), (b:N {id: 2}) CREATE (a)-[:RC {u: $l}]->(b)`, map[string]expr.Value{"l": nullList2941()}},
		case2941{"node/MERGE literal", `MERGE (:C {u: [null, 'y']})`, nil},
		case2941{"rel/MERGE literal", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[:RM {u: [null, 'y']}]->(b)`, nil},
	)
	return cs
}

// run2941 runs query to completion and returns its counters and error.
func run2941(eng *cypher.Engine, query string, params map[string]expr.Value) (*exec.QueryCounters, error) {
	res, err := eng.RunInTx(context.Background(), query, params)
	if err != nil {
		return nil, err
	}
	for res.Next() { // deliberate full drain
	}
	drainErr := res.Err()
	counters := res.Counters()
	closeErr := res.Close()
	if drainErr != nil {
		return counters, drainErr
	}
	return counters, closeErr
}

func state2941(t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	res, err := eng.Run(context.Background(), fingerprint2941, nil)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	defer res.Close()
	var rows []string
	for res.Next() {
		rec := res.Record()
		cols := make([]string, 0, len(rec))
		for _, col := range []string{"nc", "rc", "ap", "rp"} {
			cols = append(cols, col+"="+canon2941(rec[col]))
		}
		rows = append(rows, strings.Join(cols, " "))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("fingerprint drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("fingerprint returned %d rows, want 1: %q", len(rows), rows)
	}
	return rows[0]
}

// canon2941 renders a result value with map keys in sorted order, so two
// renderings of the same state compare equal (a map's own String form iterates
// in Go map order).
func canon2941(v any) string {
	switch x := v.(type) {
	case expr.MapValue:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+canon2941(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+canon2941(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		return fmt.Sprint(v)
	}
}

func assertRefused2941(t *testing.T, eng *cypher.Engine, c case2941) {
	t.Helper()
	before := state2941(t, eng)
	if !strings.Contains(before, "old") {
		t.Fatalf("fixture state %q does not carry the prior value 'old'", before)
	}
	counters, err := run2941(eng, c.query, c.params)
	if err == nil {
		t.Fatalf("%q reported success; want InvalidPropertyType (counters %+v)", c.query, counters)
	}
	if !errors.Is(err, exec.ErrNullListElement) {
		t.Errorf("%q: error %v does not wrap exec.ErrNullListElement", c.query, err)
	}
	if !strings.Contains(err.Error(), "InvalidPropertyType") {
		t.Errorf("%q: error %q does not name InvalidPropertyType", c.query, err)
	}
	if counters != nil && counters.ContainsUpdates() {
		t.Errorf("%q: counters %+v report an effect for a refused statement", c.query, counters)
	}
	if after := state2941(t, eng); after != before {
		t.Errorf("%q changed the graph although it was refused\n  before: %s\n  after:  %s", c.query, before, after)
	}
}

func TestNullListProperty_RefusedInMemory_2941(t *testing.T) {
	t.Parallel()
	for _, c := range cases2941() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
			if _, err := run2941(eng, fixture2941, nil); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			assertRefused2941(t, eng, c)
		})
	}
}

func TestNullListProperty_RefusedOnWAL_2941(t *testing.T) {
	t.Parallel()
	for _, c := range cases2941() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatalf("wal.Open: %v", err)
			}
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			})
			eng := cypher.NewEngineWithStore(st)
			if _, err := run2941(eng, fixture2941, nil); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			assertRefused2941(t, eng, c)
			live := state2941(t, eng)

			if err := w.Close(); err != nil {
				t.Fatalf("wal.Close: %v", err)
			}
			rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			})
			if err != nil {
				t.Fatalf("recovery.Open: %v", err)
			}
			if got := state2941(t, cypher.NewEngine(rec.Graph)); got != live {
				t.Errorf("recovered state differs from the live state\n  live:      %s\n  recovered: %s", live, got)
			}
		})
	}
}

// TestNullListProperty_ControlsStillStore_2941 pins the neighbours of the
// refusal: a list without null stores, and a null scalar is still a no-op
// removal rather than an error.
func TestNullListProperty_ControlsStillStore_2941(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
	if _, err := run2941(eng, fixture2941, nil); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, q := range []string{
		`MATCH (x:N {id: 1}) SET x.l = [1, 2]`,
		`MATCH (:N {id: 1})-[x:R]->() SET x.l = [1, 2]`,
		`CREATE (:C {u: ['x']})`,
		`MATCH (x:N {id: 1}) SET x.gone = null`,
	} {
		if _, err := run2941(eng, q, nil); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	st := state2941(t, eng)
	if !strings.Contains(st, "l:[1, 2]") {
		t.Errorf("state %q does not carry the stored list [1, 2]", st)
	}
}

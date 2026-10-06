package cypher_test

// property_value_kinds_2958_test.go — regression gate for rmp #2958.
//
// The whole-entity SET forms ingest a runtime map through
// exec.exprMapValueToEntries, which converted each entry with a helper that knew
// only the four primitive kinds and SKIPPED every other entry with a defensive
// `continue`. The same narrow conversion sat behind a parameter inside a
// property-map literal. Measured at f32c5a61, every one of these reported
// success and wrote nothing for the key:
//
//   - SET x += $m / SET x = $m with a temporal value or a list of temporals
//     (the REPLACE form also cleared every key x carried);
//   - SET x += {u: $d} / SET x = {u: $d, …} with a temporal parameter;
//   - MERGE … ON MATCH / ON CREATE SET x += {u: date(…)} and x = {…};
//   - CREATE (:C $m) with a temporal entry, and MERGE (:C {u: $d}), which
//     also MATCHED on the dropped key.
//
// A temporal value IS a property type (openCypher 9 temporal types, CIP2015-08-06;
// `SET x.u = date(…)` and `SET x.u = $d` already stored it), so these must store
// exactly what the single-property form stores. Every value that is NOT a
// property type — a map, a nested list, a list of maps, a list with a null
// element, a node, a relationship or a path, alone or inside a list — must be
// refused with InvalidPropertyType (TCK Set1 [10]), leave the graph unchanged
// and report no counters: in memory, and on the WAL with the recovered graph
// matching.

import (
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// temporals2958 returns one value of every temporal kind.
func temporals2958(t *testing.T) map[string]expr.Value {
	t.Helper()
	d, err := expr.ParseDate("2020-01-02")
	if err != nil {
		t.Fatal(err)
	}
	ldt, err := expr.ParseLocalDateTime("2020-01-02T03:04:05")
	if err != nil {
		t.Fatal(err)
	}
	dt, err := expr.ParseDateTime("2020-01-02T03:04:05+01:00")
	if err != nil {
		t.Fatal(err)
	}
	lt, err := expr.ParseLocalTime("03:04:05")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := expr.ParseTime("03:04:05+01:00")
	if err != nil {
		t.Fatal(err)
	}
	du, err := expr.ParseDuration("P1DT2H")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]expr.Value{
		"date": d, "localdatetime": ldt, "datetime": dt,
		"localtime": lt, "time": tm, "duration": du,
	}
}

type store2958 struct {
	name   string
	query  string
	params map[string]expr.Value
	// read returns the stored value as column u.
	read string
	want expr.Value
}

const (
	read2958Node = `MATCH (x:N {id: 1}) RETURN x.u AS u`
	read2958Rel  = `MATCH (:N {id: 1})-[x:R]->() RETURN x.u AS u`
	read2958C    = `MATCH (c:C) RETURN c.u AS u`
)

func storeCases2958(t *testing.T) []store2958 {
	t.Helper()
	tv := temporals2958(t)
	d := tv["date"]
	dl := expr.ListValue{d, tv["duration"]}
	cs := make([]store2958, 0, 48)
	for kind, v := range tv {
		cs = append(cs,
			store2958{"node/SET += parameter map/" + kind, `MATCH (x:N {id: 1}) SET x += $m`, map[string]expr.Value{"m": expr.MapValue{"u": v}}, read2958Node, v},
			store2958{"rel/SET += parameter map/" + kind, `MATCH (:N {id: 1})-[x:R]->() SET x += $m`, map[string]expr.Value{"m": expr.MapValue{"u": v}}, read2958Rel, v},
		)
	}
	for _, tgt := range []struct{ kind, match, read, keep string }{
		{"node", `MATCH (x:N {id: 1}) `, read2958Node, `id: 1, p: 'old'`},
		{"rel", `MATCH (:N {id: 1})-[x:R]->() `, read2958Rel, `p: 'old'`},
	} {
		cs = append(cs,
			store2958{tgt.kind + "/SET = parameter map", tgt.match + `SET x = $m`, map[string]expr.Value{"m": expr.MapValue{"u": d, "id": expr.IntegerValue(1), "p": expr.StringValue("old")}}, tgt.read, d},
			store2958{tgt.kind + "/SET = parameter map, temporal list", tgt.match + `SET x = $m`, map[string]expr.Value{"m": expr.MapValue{"u": dl, "id": expr.IntegerValue(1), "p": expr.StringValue("old")}}, tgt.read, dl},
			store2958{tgt.kind + "/SET += map literal, parameter", tgt.match + `SET x += {u: $d}`, map[string]expr.Value{"d": d}, tgt.read, d},
			store2958{tgt.kind + "/SET = map literal, parameter list", tgt.match + `SET x = {u: $d, ` + tgt.keep + `}`, map[string]expr.Value{"d": dl}, tgt.read, dl},
		)
	}
	cs = append(cs,
		store2958{"node/MERGE ON MATCH SET +=", `MERGE (x:N {id: 1}) ON MATCH SET x += {u: date('2020-01-02')}`, nil, read2958Node, d},
		store2958{"node/MERGE ON MATCH SET =", `MERGE (x:N {id: 1}) ON MATCH SET x = {u: date('2020-01-02'), id: 1, p: 'old'}`, nil, read2958Node, d},
		store2958{"node/MERGE ON MATCH SET += parameter map", `MERGE (x:N {id: 1}) ON MATCH SET x += $m`, map[string]expr.Value{"m": expr.MapValue{"u": d}}, read2958Node, d},
		store2958{"node/MERGE ON CREATE SET +=", `MERGE (x:C {id: 7}) ON CREATE SET x += {u: date('2020-01-02')}`, nil, read2958C, d},
		store2958{"rel/MERGE ON MATCH SET +=", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[x:R]->(b) ON MATCH SET x += {u: date('2020-01-02')}`, nil, read2958Rel, d},
		store2958{"rel/MERGE pattern ON MATCH SET +=", `MERGE (:N {id: 1})-[x:R]->(:N {id: 2}) ON MATCH SET x += {u: date('2020-01-02')}`, nil, read2958Rel, d},
		store2958{"node/CREATE parameter map", `CREATE (:C $m)`, map[string]expr.Value{"m": expr.MapValue{"u": d}}, read2958C, d},
		store2958{"node/CREATE parameter list", `CREATE (:C {u: $d})`, map[string]expr.Value{"d": dl}, read2958C, dl},
		store2958{"node/MERGE parameter", `MERGE (:C {u: $d})`, map[string]expr.Value{"d": d}, read2958C, d},
		store2958{"node/MERGE parameter list", `MERGE (:C {u: $d})`, map[string]expr.Value{"d": dl}, read2958C, dl},
	)
	return cs
}

type refuse2958 struct {
	name   string
	query  string
	params map[string]expr.Value
	// wantIs is the exec sentinel the refusal must wrap; nil when the refusing
	// path builds its InvalidPropertyType error as plain text.
	wantIs error
}

func refuseCases2958() []refuse2958 {
	node := expr.NodeValue{ID: 0}
	m := func(v expr.Value) map[string]expr.Value { return map[string]expr.Value{"m": expr.MapValue{"u": v}} }
	cs := make([]refuse2958, 0, 48)
	for _, tgt := range []struct{ kind, match string }{
		{"node", `MATCH (x:N {id: 1}), (b:N {id: 2}) `},
		{"rel", `MATCH (:N {id: 1})-[x:R]->(b:N {id: 2}) `},
	} {
		for _, op := range []string{"+=", "="} {
			p := tgt.kind + "/SET " + op + " "
			cs = append(cs,
				refuse2958{p + "parameter map: map", tgt.match + `SET x ` + op + ` $m`, m(expr.MapValue{"a": expr.IntegerValue(1)}), nil},
				refuse2958{p + "parameter map: list of maps", tgt.match + `SET x ` + op + ` $m`, m(expr.ListValue{expr.MapValue{"a": expr.IntegerValue(1)}}), nil},
				refuse2958{p + "parameter map: nested list", tgt.match + `SET x ` + op + ` $m`, m(expr.ListValue{expr.ListValue{expr.IntegerValue(1)}}), exec.ErrNestedPropertyValue},
				refuse2958{p + "parameter map: null element", tgt.match + `SET x ` + op + ` $m`, m(expr.ListValue{expr.IntegerValue(1), expr.Null}), exec.ErrNullListElement},
				refuse2958{p + "parameter map: node", tgt.match + `SET x ` + op + ` $m`, m(node), nil},
				refuse2958{p + "parameter map: list with a node", tgt.match + `SET x ` + op + ` $m`, m(expr.ListValue{node}), nil},
				refuse2958{p + "map literal: node variable", tgt.match + `SET x ` + op + ` {u: b}`, nil, nil},
				refuse2958{p + "map literal: list with a node variable", tgt.match + `SET x ` + op + ` {u: [b]}`, nil, nil},
				refuse2958{p + "map literal: nested map", tgt.match + `SET x ` + op + ` {u: {a: 1}}`, nil, nil},
				refuse2958{p + "map literal: nested list", tgt.match + `SET x ` + op + ` {u: [[1]]}`, nil, exec.ErrNestedPropertyValue},
				refuse2958{p + "map literal: node parameter", tgt.match + `SET x ` + op + ` {u: $n}`, map[string]expr.Value{"n": node}, exec.ErrEntityPropertyValue},
				refuse2958{p + "map literal: parameter list with a node", tgt.match + `SET x ` + op + ` {u: $n}`, map[string]expr.Value{"n": expr.ListValue{node}}, exec.ErrEntityPropertyValue},
			)
		}
	}
	cs = append(cs,
		refuse2958{"path/SET += map literal", `MATCH p = (x:N {id: 1})-[:R]->() SET x += {u: p}`, nil, nil},
		refuse2958{"node/MERGE ON MATCH SET += node variable", `MATCH (b:N {id: 2}) MERGE (x:N {id: 1}) ON MATCH SET x += {u: b}`, nil, nil},
		refuse2958{"node/MERGE ON MATCH SET += node parameter", `MERGE (x:N {id: 1}) ON MATCH SET x += $m`, m(node), nil},
		refuse2958{"node/CREATE parameter map: node", `CREATE (:C $m)`, m(node), nil},
		refuse2958{"node/CREATE node parameter", `CREATE (:C {u: $n})`, map[string]expr.Value{"n": node}, nil},
		refuse2958{"node/MERGE node parameter", `MERGE (:C {u: $n})`, map[string]expr.Value{"n": node}, exec.ErrEntityPropertyValue},
		refuse2958{"node/MERGE parameter list with a node", `MERGE (:C {u: $n})`, map[string]expr.Value{"n": expr.ListValue{node}}, exec.ErrEntityPropertyValue},
		refuse2958{"node/SET prop node parameter", `MATCH (x:N {id: 1}) SET x.u = $n`, map[string]expr.Value{"n": node}, nil},
	)
	return cs
}

func TestPropertyValueKinds_TemporalsStore_2958(t *testing.T) {
	t.Parallel()
	for engName, mk := range enginesB2() {
		for _, c := range storeCases2958(t) {
			t.Run(engName+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				e := mk(t)
				if _, err := run2941(e.eng, fixture2941, nil); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				counters, err := run2941(e.eng, c.query, c.params)
				if err != nil {
					t.Fatalf("%q: %v", c.query, err)
				}
				if counters == nil || !counters.ContainsUpdates() {
					t.Errorf("%q: counters %+v report no effect for a stored value", c.query, counters)
				}
				want := []string{"u=" + canon2941(c.want)}
				got := queryRowsB2(t, e.eng, c.read, "u")
				if strings.Join(got, ";") != strings.Join(want, ";") {
					t.Errorf("%q stored %v, want %v", c.query, got, want)
				}
				// Nothing else the fixture carried may be lost (the REPLACE forms
				// restate id and p).
				live := state2941(t, e.eng)
				if !strings.Contains(live, `p:"old"`) {
					t.Errorf("%q lost a key it did not name: %s", c.query, live)
				}
				if e.recover != nil {
					rec := e.recover(t)
					if got := queryRowsB2(t, rec, c.read, "u"); strings.Join(got, ";") != strings.Join(want, ";") {
						t.Errorf("recovered %q value %v, want %v", c.query, got, want)
					}
					if st := state2941(t, rec); st != live {
						t.Errorf("recovered state differs\n  live:      %s\n  recovered: %s", live, st)
					}
				}
			})
		}
	}
}

func TestPropertyValueKinds_NonPropertyRefused_2958(t *testing.T) {
	t.Parallel()
	for engName, mk := range enginesB2() {
		for _, c := range refuseCases2958() {
			t.Run(engName+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				e := mk(t)
				if _, err := run2941(e.eng, fixture2941, nil); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				before := state2941(t, e.eng)
				beforeC := queryRowsB2(t, e.eng, `MATCH (c:C) RETURN count(c) AS n`, "n")
				counters, err := run2941(e.eng, c.query, c.params)
				if err == nil {
					t.Fatalf("%q reported success; want InvalidPropertyType (counters %+v)", c.query, counters)
				}
				if !strings.Contains(err.Error(), "InvalidPropertyType") {
					t.Errorf("%q: error %q does not name InvalidPropertyType", c.query, err)
				}
				if c.wantIs != nil && !errors.Is(err, c.wantIs) {
					t.Errorf("%q: error %v does not wrap %v", c.query, err, c.wantIs)
				}
				if counters != nil && counters.ContainsUpdates() {
					t.Errorf("%q: counters %+v report an effect for a refused statement", c.query, counters)
				}
				after := state2941(t, e.eng)
				if after != before {
					t.Errorf("%q changed the graph although it was refused\n  before: %s\n  after:  %s", c.query, before, after)
				}
				if got := queryRowsB2(t, e.eng, `MATCH (c:C) RETURN count(c) AS n`, "n"); strings.Join(got, "") != strings.Join(beforeC, "") {
					t.Errorf("%q created a node although it was refused: %v", c.query, got)
				}
				if e.recover != nil {
					if st := state2941(t, e.recover(t)); st != after {
						t.Errorf("recovered state differs\n  live:      %s\n  recovered: %s", after, st)
					}
				}
			})
		}
	}
}

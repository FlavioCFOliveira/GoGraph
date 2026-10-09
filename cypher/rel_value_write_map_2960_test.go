package cypher_test

// rel_value_write_map_2960_test.go — regression gate for rmp #2960.
//
// The per-row evaluators of the write-path maps — SET x = {…}, SET x += {…},
// SET x += <expr>, CREATE / MERGE property maps and MERGE ON CREATE / ON MATCH
// actions — build their row context through buildRowCtxFromMutator, which
// turned a bound node id into a node value but left a bound RELATIONSHIP as the
// raw integer handle of its Expand column. Measured at 25999b58:
//
//   - `SET x = {u: r}`, `SET x += {u: [r]}`, `CREATE (:C {u: r})` and
//     `MERGE … ON MATCH SET x.u = r` stored the handle (`u: 1`) instead of
//     refusing the value with InvalidPropertyType (openCypher 9 §3.2: an entity
//     is not a property type; TCK Set1 [10]);
//   - `r.p` read null there, so `SET x += {u: r.p}` silently wrote nothing and
//     `MERGE (:C {u: r.p})` raised MergeReadOwnWrites;
//   - `type(r)` and `properties(r)` failed with a type error.
//
// Each of those forms must see the relationship the plain `SET x.u = r.p`
// sees: the bound instance (by handle, so a parallel sibling's properties never
// leak), in its stored orientation. Every shape runs in memory and on the WAL,
// where the recovered graph must hold exactly the live outcome.

import (
	"errors"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

const setup2960 = `CREATE (a:A {id: 1})-[:T {p: 7}]->(b:B {id: 2}),
	(a)-[:T {p: 8}]->(b),
	(:X {id: 1})`

// read2960 renders every :X and :C node, so a refused statement can be shown to
// leave the graph exactly as setup2960 built it.
const read2960 = `MATCH (n) WHERE n:X OR n:C
	RETURN labels(n)[0] AS l, properties(n) AS p ORDER BY l, p.u, p.t`

type value2960 struct {
	name  string
	query string
	want  []string
}

func cases2960() (refuse, store []value2960) {
	const m = `MATCH (:A)-[r:T {p: 7}]->(:B), (x:X) `
	refuse = []value2960{
		{name: "SET = map with r", query: m + `SET x = {u: r}`},
		{name: "SET += map with [r]", query: m + `SET x += {u: [r]}`},
		{name: "CREATE map with r", query: `MATCH (:A)-[r:T {p: 7}]->(:B) CREATE (:C {u: r})`},
		{name: "MERGE ON MATCH SET x.u = r", query: `MATCH (:A)-[r:T {p: 7}]->(:B) MERGE (x:X {id: 1}) ON MATCH SET x.u = r`},
		{name: "MERGE ON MATCH SET += map with r", query: `MATCH (:A)-[r:T {p: 7}]->(:B) MERGE (x:X {id: 1}) ON MATCH SET x += {u: r}`},
		{name: "MERGE property map with r", query: `MATCH (:A)-[r:T {p: 7}]->(:B) MERGE (:C {u: r})`},
	}
	store = []value2960{
		{name: "SET += map with r.p", query: m + `SET x += {u: r.p}`,
			want: []string{"l=\"X\" p={id:1,u:7}"}},
		{name: "SET = map with r.p and type(r)", query: m + `SET x = {u: r.p, t: type(r)}`,
			want: []string{"l=\"X\" p={t:\"T\",u:7}"}},
		{name: "SET += properties(r)", query: m + `SET x += properties(r)`,
			want: []string{"l=\"X\" p={id:1,p:7}"}},
		{name: "CREATE map with r.p of each parallel instance", query: `MATCH (:A)-[r:T]->(:B) CREATE (:C {u: r.p, t: type(r)})`,
			want: []string{"l=\"C\" p={t:\"T\",u:7}", "l=\"C\" p={t:\"T\",u:8}", "l=\"X\" p={id:1}"}},
		{name: "MERGE property map with r.p", query: `MATCH (:A)-[r:T]->(:B) MERGE (:C {u: r.p})`,
			want: []string{"l=\"C\" p={u:7}", "l=\"C\" p={u:8}", "l=\"X\" p={id:1}"}},
		{name: "MERGE ON MATCH SET x.u = r.p, x.t = type(r)", query: `MATCH (:A)-[r:T {p: 8}]->(:B) MERGE (x:X {id: 1}) ON MATCH SET x.u = r.p, x.t = type(r)`,
			want: []string{"l=\"X\" p={id:1,t:\"T\",u:8}"}},
		{name: "MERGE ON MATCH SET += map with r.p", query: `MATCH (:A)-[r:T {p: 8}]->(:B) MERGE (x:X {id: 1}) ON MATCH SET x += {u: r.p}`,
			want: []string{"l=\"X\" p={id:1,u:8}"}},
		{name: "MERGE ON CREATE SET from r", query: `MATCH (:A)-[r:T {p: 7}]->(:B) MERGE (c:C {id: 9}) ON CREATE SET c.u = r.p, c += {t: type(r)}`,
			want: []string{"l=\"C\" p={id:9,t:\"T\",u:7}", "l=\"X\" p={id:1}"}},
		// The relationship reached only through a comprehension and a CASE:
		// the evaluator resolves just the variables its expressions reference,
		// so these must still count as references.
		{name: "r inside a comprehension and a CASE", query: m + `SET x += {u: [q IN [r] | q.p][0], t: CASE WHEN type(r) = 'T' THEN 'yes' END}`,
			want: []string{"l=\"X\" p={id:1,t:\"yes\",u:7}"}},
		// The reverse hop of an undirected pattern walks the relationship
		// against its storage order; the value must still be the stored instance.
		{name: "undirected reverse hop", query: `MATCH (b:B)-[r:T {p: 7}]-(a:A), (x:X) SET x += {u: r.p, s: id(startNode(r)) = id(a), e: id(endNode(r)) = id(b)}`,
			want: []string{"l=\"X\" p={e:true,id:1,s:true,u:7}"}},
	}
	return refuse, store
}

func TestRelValueInWriteMap_2960(t *testing.T) {
	refuse, store := cases2960()
	for engName, mk := range enginesB2() {
		for _, c := range refuse {
			t.Run(engName+"/refuse/"+c.name, func(t *testing.T) {
				e := mk(t)
				mustRun2960(t, e, setup2960)
				before := queryRowsB2(t, e.eng, read2960, "l", "p")
				res, err := e.eng.RunInTx(t.Context(), c.query, nil)
				if err == nil {
					for res.Next() {
					}
					err = res.Err()
					res.Close()
				}
				if !errors.Is(err, exec.ErrEntityPropertyValue) {
					t.Fatalf("%q: want exec.ErrEntityPropertyValue, got err=%v; graph now %v",
						c.query, err, queryRowsB2(t, e.eng, read2960, "l", "p"))
				}
				assertGraph2960(t, e, before)
			})
		}
		for _, c := range store {
			t.Run(engName+"/store/"+c.name, func(t *testing.T) {
				e := mk(t)
				mustRun2960(t, e, setup2960)
				mustRun2960(t, e, c.query)
				assertGraph2960(t, e, c.want)
			})
		}
	}
}

func mustRun2960(t *testing.T, e engineB2, query string) {
	t.Helper()
	res, err := e.eng.RunInTx(t.Context(), query, nil)
	if err != nil {
		t.Fatalf("%q: %v", query, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%q drain: %v", query, err)
	}
	res.Close()
}

// assertGraph2960 checks the live graph and, on the WAL engine, the graph
// recovery rebuilds from the log.
func assertGraph2960(t *testing.T, e engineB2, want []string) {
	t.Helper()
	if got := queryRowsB2(t, e.eng, read2960, "l", "p"); !slices.Equal(got, want) {
		t.Fatalf("live graph:\n got %v\nwant %v", got, want)
	}
	if e.recover == nil {
		return
	}
	if got := queryRowsB2(t, e.recover(t), read2960, "l", "p"); !slices.Equal(got, want) {
		t.Fatalf("recovered graph:\n got %v\nwant %v", got, want)
	}
}

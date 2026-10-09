package cypher_test

// set_item_order_2953_test.go — regression gate for rmp #2953.
//
// # The rule
//
// openCypher CIP2015-10-27 "State visibility between clauses" (adopted; read
// at opencypher/openCypher 677cbafa, cip/1.adopted/): "Each clause lives in its
// own state, which includes all the changes of clauses coming before it,
// including changes performed by itself". The TCK Set and Merge features carry
// no scenario where one item reads another's write (Set1 [11] and Merge6/7
// [*] write independent keys), so the CIP is the governing text. The plain SET
// clause already applied its items one after another, each reading every
// earlier item's write — the guard tests below pin that — and an ON CREATE /
// ON MATCH action list is a SET item list, so it must behave the same.
//
// # The defect
//
// Measured at f32c5a61, a MERGE action list broke that rule four ways:
//
//   - the per-property items and the whole-entity items (`x += {…}`,
//     `x = {…}`) were applied as two batches, per-property first, so
//     `x += {a: 1}, x.b = x.a + 1` read a as null and `x += {a: 1}, x.a = 2`
//     ended with a = 1;
//   - MergePattern refreshed a relationship value only after the whole list,
//     so `e.a = 1, e.b = e.a + 1` and `e.a = 1, a.b = e.a + 1` read e.a as null;
//   - an entity bound by a preceding clause and projected (`WITH n`) kept its
//     pre-action snapshot, so `n.a = 1, n.b = n.a + 1` read n.a as null;
//   - the per-row evaluators were keyed on (variable, property), so two items
//     writing the same property shared ONE evaluator: `x.c = x.id + 1,
//     x.c = x.c * 10` ran the second right-hand side twice and stored nothing.

import (
	"strings"
	"testing"
)

type order2953 struct {
	name  string
	query string
	// reads maps a read query (returning column p) to the rows it must return.
	reads map[string][]string
}

const (
	readNode1 = `MATCH (x:N {id: 1}) RETURN properties(x) AS p`
	readNode2 = `MATCH (x:N {id: 2}) RETURN properties(x) AS p`
	readRelR  = `MATCH (:N {id: 1})-[x:R]->() RETURN properties(x) AS p`
	readRelS  = `MATCH ()-[x:S]->() RETURN properties(x) AS p`
	readC     = `MATCH (x:C) RETURN properties(x) AS p`
)

func one2953(read, p string) map[string][]string { return map[string][]string{read: {"p=" + p}} }

func orderCases2953() []order2953 {
	return []order2953{
		// MERGE on a node (exec.Merge).
		{"Merge/same key twice, both expressions", `MERGE (x:N {id: 1}) ON MATCH SET x.c = x.id + 1, x.c = x.c * 10`, one2953(readNode1, `{c:20,id:1,p:"old"}`)},
		{"Merge/+= then a reader", `MERGE (x:N {id: 1}) ON MATCH SET x += {a: 1}, x.b = x.a + 1`, one2953(readNode1, `{a:1,b:2,id:1,p:"old"}`)},
		{"Merge/+= then an overwrite", `MERGE (x:N {id: 1}) ON MATCH SET x += {a: 1}, x.a = 2`, one2953(readNode1, `{a:2,id:1,p:"old"}`)},
		{"Merge/= then a reader", `MERGE (x:N {id: 1}) ON MATCH SET x = {a: 1, id: 1}, x.b = x.a + 1`, one2953(readNode1, `{a:1,b:2,id:1}`)},
		{"Merge/property then += reader", `MERGE (x:N {id: 1}) ON MATCH SET x.a = 1, x += {b: x.a + 1}`, one2953(readNode1, `{a:1,b:2,id:1,p:"old"}`)},
		{"Merge/ON CREATE += then a reader", `MERGE (x:C {id: 9}) ON CREATE SET x += {a: 1}, x.b = x.a + 1`, one2953(readC, `{a:1,b:2,id:9}`)},
		{"Merge/projected relationship", `MATCH ()-[r:R]->() WITH r MERGE (x:N {id: 1}) ON MATCH SET r.p = 'new', r.b = r.p`, one2953(readRelR, `{b:"new",p:"new"}`)},
		// MERGE on a pattern (exec.MergePattern).
		{"MergePattern/relationship reader", `MERGE (:N {id: 1})-[e:R]->(:N {id: 2}) ON MATCH SET e.a = 1, e.b = e.a + 1`, one2953(readRelR, `{a:1,b:2,p:"old"}`)},
		{"MergePattern/relationship overwrite then reader", `MERGE (:N {id: 1})-[e:R]->(:N {id: 2}) ON MATCH SET e.p = 'new', e.b = e.p`, one2953(readRelR, `{b:"new",p:"new"}`)},
		{"MergePattern/node reads relationship", `MERGE (a:N {id: 1})-[e:R]->(:N {id: 2}) ON MATCH SET e.a = 1, a.b = e.a + 1`, map[string][]string{
			readNode1: {`p={b:2,id:1,p:"old"}`}, readRelR: {`p={a:1,p:"old"}`},
		}},
		{"MergePattern/relationship += then a reader", `MERGE (:N {id: 1})-[e:R]->(:N {id: 2}) ON MATCH SET e += {a: 1}, e.b = e.a + 1`, one2953(readRelR, `{a:1,b:2,p:"old"}`)},
		{"MergePattern/bound endpoints, node reads relationship", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[e:R]->(b) ON MATCH SET e.a = 1, a.b = e.a + 1`, map[string][]string{
			readNode1: {`p={b:2,id:1,p:"old"}`}, readRelR: {`p={a:1,p:"old"}`},
		}},
		{"MergePattern/property then = reader", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[e:R]->(b) ON MATCH SET e.a = 1, e = {b: e.a}`, one2953(readRelR, `{b:1}`)},
		{"MergePattern/ON CREATE chain", `MATCH (a:N {id: 1}) MERGE (a)-[e:S]->(c:C) ON CREATE SET e.a = 1, e.b = e.a + 1, c.a = e.b`, map[string][]string{
			readRelS: {`p={a:1,b:2}`}, readC: {`p={a:2}`},
		}},
		{"MergePattern/projected node", `MATCH (n:N {id: 1}) WITH n MERGE (:N {id: 2})-[:R2]->(:C) ON CREATE SET n.a = 1, n.b = n.a + 1`, one2953(readNode1, `{a:1,b:2,id:1,p:"old"}`)},
		// MERGE on a relationship with bound endpoints (exec.MergeRelationship).
		{"MergeRelationship/same key twice, both expressions", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[e:R]->(b) ON MATCH SET e.c = e.p + 'x', e.c = e.c + 'y'`, one2953(readRelR, `{c:"oldxy",p:"old"}`)},
		{"MergeRelationship/reader", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[e:R]->(b) ON MATCH SET e.a = 1, e.b = e.a + 1`, one2953(readRelR, `{a:1,b:2,p:"old"}`)},
		{"MergeRelationship/ON CREATE reader", `MATCH (a:N {id: 1}), (b:N {id: 2}) MERGE (a)-[e:S]->(b) ON CREATE SET e.a = 1, e.b = e.a + 1`, one2953(readRelS, `{a:1,b:2}`)},
		// The plain SET clause — conformant at f32c5a61; pinned as the reference
		// the MERGE action lists now match.
		{"SET/node reader", `MATCH (x:N {id: 1}) SET x.a = 1, x.b = x.a + 1`, one2953(readNode1, `{a:1,b:2,id:1,p:"old"}`)},
		{"SET/+= then an overwrite", `MATCH (x:N {id: 1}) SET x += {a: 1}, x.a = 2`, one2953(readNode1, `{a:2,id:1,p:"old"}`)},
		{"SET/same key twice, both expressions", `MATCH (x:N {id: 1}) SET x.c = x.id + 1, x.c = x.c * 10`, one2953(readNode1, `{c:20,id:1,p:"old"}`)},
		{"SET/relationship = then a reader", `MATCH (:N {id: 1})-[x:R]->() SET x = {a: 1}, x.b = x.a + 1`, one2953(readRelR, `{a:1,b:2}`)},
		{"SET/projected relationship", `MATCH ()-[r:R]->() WITH r SET r.p = 'new', r.b = r.p`, one2953(readRelR, `{b:"new",p:"new"}`)},
		{"SET/two entities, sequential not simultaneous", `MATCH (x:N {id: 1}), (y:N {id: 2}) SET x.p = y.id, y.p = x.p`, map[string][]string{
			readNode1: {`p={id:1,p:2}`}, readNode2: {`p={id:2,p:2}`},
		}},
	}
}

func TestSetItemOrder_EachItemSeesEarlierWrites_2953(t *testing.T) {
	t.Parallel()
	for engName, mk := range enginesB2() {
		for _, c := range orderCases2953() {
			t.Run(engName+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				e := mk(t)
				if _, err := run2941(e.eng, fixture2941, nil); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				counters, err := run2941(e.eng, c.query, nil)
				if err != nil {
					t.Fatalf("%q: %v", c.query, err)
				}
				if counters == nil || !counters.ContainsUpdates() {
					t.Errorf("%q: counters %+v report no effect", c.query, counters)
				}
				check := func(label string, eng engineB2) {
					for read, want := range c.reads {
						if got := queryRowsB2(t, eng.eng, read, "p"); strings.Join(got, ";") != strings.Join(want, ";") {
							t.Errorf("%s %q: %q returned %v, want %v", label, c.query, read, got, want)
						}
					}
				}
				check("live", e)
				if e.recover != nil {
					check("recovered", engineB2{eng: e.recover(t)})
				}
			})
		}
	}
}

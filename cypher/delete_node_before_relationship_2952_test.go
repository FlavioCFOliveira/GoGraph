package cypher_test

// delete_node_before_relationship_2952_test.go — regression gate for rmp #2952.
//
// The items of one DELETE clause run as one operator each, row by row, and the
// node guard ("a node that still has a relationship cannot be DELETEd") fired
// on the spot. `DELETE r, a` therefore succeeded while `DELETE a, r` — the same
// clause with its items listed the other way round — was refused, because the
// node was reached before its relationship was removed.
//
// openCypher decides the guard for the clause as a whole: TCK Delete5 [7]
// deletes two paths whose nodes are each held by the OTHER path's relationship,
// and Delete4 [1] lists the relationship first. The node's guard is now decided
// at the end of the clause, as rmp #2950 already does for a path's nodes. A node
// still held by a relationship the clause does NOT list must still be refused,
// leaving the graph unchanged and the counters empty.

import (
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

const fixture2952 = `CREATE (a:A {id: 1})-[:R]->(b:B {id: 2}), (c:A {id: 3})-[:R]->(d:B {id: 4}), (c)-[:S]->(d), (e:A {id: 5})-[:R]->(f:B {id: 6}), (e)-[:R]->(f)`

const state2952 = `MATCH (n) WITH n ORDER BY n.id
WITH collect(labels(n)[0] + toString(n.id)) AS ns
OPTIONAL MATCH (x)-[r]->(y) WITH ns, x, r, y ORDER BY x.id, type(r), y.id
RETURN ns, collect(toString(x.id) + type(r) + toString(y.id)) AS rs`

func snapshot2952(t *testing.T, e engineB2) string {
	t.Helper()
	return strings.Join(queryRowsB2(t, e.eng, state2952, "ns", "rs"), ";")
}

func TestDeleteNodeBeforeRelationship_2952(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		query        string
		wantNodes    int64
		wantRels     int64
		wantAfterSub []string // fragments the remaining state must NOT contain
	}{
		{"node then relationship", `MATCH (a:A {id: 1})-[r:R]->(b:B) DELETE a, r`, 1, 1, []string{"A1", "1R2"}},
		{"node, relationship, node", `MATCH (a:A {id: 1})-[r:R]->(b:B) DELETE a, r, b`, 2, 1, []string{"A1", "B2", "1R2"}},
		{"relationship then node (Delete4 order)", `MATCH (a:A {id: 1})-[r:R]->(b:B) DELETE r, a`, 1, 1, []string{"A1", "1R2"}},
		{"node held by two listed relationships, two rows", `MATCH (a:A {id: 5})-[r:R]->(b:B) DELETE a, r`, 1, 2, []string{"A5", "5R6"}},
		{"node in a later item than its relationship's row", `MATCH (a:A {id: 5})-[r:R]->(b:B) DELETE b, r, a`, 2, 2, []string{"A5", "B6", "5R6"}},
	}
	for engName, mk := range enginesB2() {
		for _, c := range cases {
			t.Run(engName+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				e := mk(t)
				if _, err := run2941(e.eng, fixture2952, nil); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				counters, err := run2941(e.eng, c.query, nil)
				if err != nil {
					t.Fatalf("%q: %v", c.query, err)
				}
				if counters == nil || counters.NodesDeleted != c.wantNodes || counters.RelationshipsDeleted != c.wantRels {
					t.Errorf("%q: counters %+v, want -nodes %d -relationships %d", c.query, counters, c.wantNodes, c.wantRels)
				}
				live := snapshot2952(t, e)
				for _, frag := range c.wantAfterSub {
					if strings.Contains(live, `"`+frag+`"`) {
						t.Errorf("%q left %s in the graph: %s", c.query, frag, live)
					}
				}
				if !strings.Contains(live, `"A3"`) || !strings.Contains(live, `"3S4"`) {
					t.Errorf("%q touched an entity it does not name: %s", c.query, live)
				}
				if e.recover != nil {
					if got := snapshot2952(t, engineB2{eng: e.recover(t)}); got != live {
						t.Errorf("recovered state differs\n  live:      %s\n  recovered: %s", live, got)
					}
				}
			})
		}
	}
}

func TestDeleteNodeBeforeRelationship_UnlistedStillRefused_2952(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		// c also holds the unlisted :S relationship.
		`MATCH (c:A {id: 3})-[r:R]->(d:B) DELETE c, r`,
		`MATCH (c:A {id: 3})-[r:R]->(d:B) DELETE r, c`,
		// Delete1 [7]: a connected node with no relationship listed at all.
		`MATCH (c:A {id: 3}) DELETE c`,
		// Both endpoints listed, their relationship not.
		`MATCH (a:A {id: 1})-->(b:B) DELETE a, b`,
	} {
		for engName, mk := range enginesB2() {
			t.Run(engName+"/"+q, func(t *testing.T) {
				t.Parallel()
				e := mk(t)
				if _, err := run2941(e.eng, fixture2952, nil); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				before := snapshot2952(t, e)
				counters, err := run2941(e.eng, q, nil)
				if !errors.Is(err, exec.ErrDeleteNodeHasRelationships) {
					t.Fatalf("%q: error %v, want exec.ErrDeleteNodeHasRelationships", q, err)
				}
				if counters != nil && counters.ContainsUpdates() {
					t.Errorf("%q: counters %+v report an effect for a refused statement", q, counters)
				}
				after := snapshot2952(t, e)
				if after != before {
					t.Errorf("%q changed the graph although it was refused\n  before: %s\n  after:  %s", q, before, after)
				}
				if e.recover != nil {
					if got := snapshot2952(t, engineB2{eng: e.recover(t)}); got != after {
						t.Errorf("recovered state differs\n  live:      %s\n  recovered: %s", after, got)
					}
				}
			})
		}
	}
}

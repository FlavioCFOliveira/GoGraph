package cypher_test

// rel_props_per_instance_varlen_size_test.go — rmp #2910 and #2912.
//
//   - #2910. A relationship variable carried across a WITH was re-materialised
//     from its row coordinates with its TYPE resolved per instance (by handle)
//     but its PROPERTIES read from the per-pair store, so each of two parallel
//     edges between the same nodes reported the pair's coalesced map: `MATCH
//     ()-[r]->() WITH r RETURN r.w` gave the edge written with w = 1 the value
//     2. Every relationship is a distinct graph element with its own property
//     map, and WITH forwards the relationship unchanged
//     (clauses/with/With1.feature [3] "Forwarding a relationship variable").
//     Each route below is asserted against absolute per-edge answers, in
//     memory, after WAL replay, and after a snapshot with the WAL truncated.
//   - #2912. A variable-length relationship variable (`[rs*]`) was typed as a
//     single relationship by the static analysis, so `size(rs)` was rejected at
//     compile time with InvalidArgumentType. The variable is bound to a List of
//     relationships (clauses/match/Match4.feature [1] "Handling fixed-length
//     variable length pattern" returns `[[:T]]`; Match9.feature [2]), and size()
//     of a List returns its length (expressions/list/List6.feature [1] "Return
//     list size").
//
// Layer: short. goleak-clean (engines, graphs and WAL writers are local).

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ppiWrites builds two parallel a->b edges with different property maps (the
// first carries an extra key, so keys() differs too) and one a->c edge.
var ppiWrites = []string{
	`CREATE (:N {k:'a'}), (:N {k:'b'}), (:N {k:'c'})`,
	`MATCH (x:N {k:'a'}), (y:N {k:'b'}) CREATE (x)-[:R {w: 1, only1: 'x'}]->(y)`,
	`MATCH (x:N {k:'a'}), (y:N {k:'b'}) CREATE (x)-[:R {w: 2}]->(y)`,
	`MATCH (x:N {k:'a'}), (y:N {k:'c'}) CREATE (x)-[:R {w: 3}]->(y)`,
}

// ppiWant is the per-edge answer every route must return as "w|nkeys": each
// relationship reports its own w and its own key count.
var ppiWant = []string{"1|2", "2|1", "3|1"}

// ppiRoutes returns, for every relationship, "w|nkeys" under a different way of
// reaching the relationship value.
var ppiRoutes = []struct{ name, query string }{
	{"direct", `MATCH ()-[r]->() RETURN r.w AS w, size(keys(r)) AS n`},
	{"with", `MATCH ()-[r]->() WITH r RETURN r.w AS w, size(keys(r)) AS n`},
	{"with_alias", `MATCH ()-[r]->() WITH r AS x RETURN x.w AS w, size(keys(x)) AS n`},
	{"with_and_node", `MATCH (a)-[r]->(b) WITH a, r, b RETURN r.w AS w, size(keys(r)) AS n`},
	{"with_properties", `MATCH ()-[r]->() WITH r RETURN properties(r).w AS w, size(keys(properties(r))) AS n`},
	{"with_map_projection", `MATCH ()-[r]->() WITH r RETURN r{.*}.w AS w, size(keys(r{.*})) AS n`},
	{"with_where", `MATCH ()-[r]->() WITH r WHERE r.w IN [1, 2, 3] RETURN r.w AS w, size(keys(r)) AS n`},
	{"with_order_by", `MATCH ()-[r]->() WITH r ORDER BY r.w RETURN r.w AS w, size(keys(r)) AS n`},
	{"with_distinct", `MATCH ()-[r]->() WITH DISTINCT r RETURN r.w AS w, size(keys(r)) AS n`},
	{"two_withs", `MATCH ()-[r]->() WITH r WITH r RETURN r.w AS w, size(keys(r)) AS n`},
	{"unwind_collect", `MATCH ()-[r]->() WITH collect(r) AS rs UNWIND rs AS r RETURN r.w AS w, size(keys(r)) AS n`},
}

// ppiFilterRoutes are single-row filters over the carried relationship: the
// WHERE must select the edge that owns the value, and the projection must
// report that same edge.
var ppiFilterRoutes = []struct{ name, query string }{
	{"with_where_eq_1", `MATCH ()-[r]->() WITH r WHERE r.w = 1 RETURN r.w AS w, size(keys(r)) AS n`},
	{"with_where_key", `MATCH ()-[r]->() WITH r WHERE r.only1 = 'x' RETURN r.w AS w, size(keys(r)) AS n`},
}

// ppiOrder asserts WITH … ORDER BY r.w orders by each edge's own value.
const ppiOrder = `MATCH ()-[r]->() WITH r ORDER BY r.w RETURN collect(r.w) AS ws, 0 AS z`

func ppiAssert(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, rt := range ppiRoutes {
		if got := rbRows(t, eng, rt.query, "w", "n"); !slices.Equal(got, ppiWant) {
			t.Errorf("%s: %s\n got  %v\n want %v", rt.name, rt.query, got, ppiWant)
		}
	}
	for _, rt := range ppiFilterRoutes {
		if got, want := rbRows(t, eng, rt.query, "w", "n"), []string{"1|2"}; !slices.Equal(got, want) {
			t.Errorf("%s: %s\n got  %v\n want %v", rt.name, rt.query, got, want)
		}
	}
	if got, want := rbRows(t, eng, ppiOrder, "ws", "z"), []string{"[1, 2, 3]|0"}; !slices.Equal(got, want) {
		t.Errorf("order: %s\n got  %v\n want %v", ppiOrder, got, want)
	}
}

// TestRelPropsPerInstance_InMemory is the #2910 regression on the in-memory
// engine.
func TestRelPropsPerInstance_InMemory(t *testing.T) {
	t.Parallel()
	ppiAssert(t, rbEngineFromQueries(t, ppiWrites))
}

// TestRelPropsPerInstance_Durable is the #2910 regression on the WAL-backed
// store, after pure WAL replay and after a snapshot with the WAL truncated.
func TestRelPropsPerInstance_Durable(t *testing.T) {
	t.Parallel()
	for _, snap := range []bool{false, true} {
		name := "wal_replay"
		if snap {
			name = "snapshot_reopen"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bhWriteCycle(t, dir, snap, ppiWrites...)
			ppiAssert(t, ppiReopen(t, dir))
		})
	}
}

// ppiReopen recovers dir and returns a durable engine over it; the WAL writer
// is closed at test cleanup.
func ppiReopen(t *testing.T, dir string) *cypher.Engine {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, bhRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("wal.Close: %v", err)
		}
	})
	return cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](res.Graph, w, bhStoreOpts()))
}

// rbEngineFromQueries builds a multigraph in-memory engine by running writes.
func rbEngineFromQueries(t *testing.T, writes []string) *cypher.Engine {
	t.Helper()
	eng := rbEngine(t, rbFixture{name: "empty"})
	for _, q := range writes[1:] {
		mustRunWrite(t, eng, q)
	}
	return eng
}

// TestSizeOfVarLengthRelList is the #2912 regression: size() of a
// variable-length relationship variable is the length of its List, in MATCH,
// after WITH, and inside a pattern comprehension; the checks that reject a
// relationship-list elsewhere are unchanged.
func TestSizeOfVarLengthRelList(t *testing.T) {
	t.Parallel()
	// chain_with_shortcut: a -[w1]-> b -[w1]-> c, a -[w2]-> c.
	eng := rbEngine(t, rbFixtures[6])
	if rbFixtures[6].name != "chain_with_shortcut" {
		t.Fatalf("fixture 6 is %q, want chain_with_shortcut", rbFixtures[6].name)
	}
	cases := []struct {
		name, query string
		want        []string
	}{
		{"match", `MATCH (a:N {k:'a'})-[rs*]->(b) RETURN b.k AS b, size(rs) AS s`, []string{`"b"|1`, `"c"|1`, `"c"|2`}},
		{"match_typed_props", `MATCH (a:N {k:'a'})-[rs:R* {w: 1}]->(b) RETURN b.k AS b, size(rs) AS s`, []string{`"b"|1`, `"c"|2`}},
		// rs is projected FIRST: at dfa9187f a WITH that projects any item before
		// rs (`WITH b, rs`) forwards rs as an empty list — a separate defect.
		{"after_with", `MATCH (a:N {k:'a'})-[rs*]->(b) WITH rs, b.k AS k RETURN k AS b, size(rs) AS s`, []string{`"b"|1`, `"c"|1`, `"c"|2`}},
		{"comprehension", `MATCH (a:N {k:'a'}) UNWIND [(a)-[r:R* {w: 1}]->(b) | size(r)] AS s RETURN a.k AS b, s AS s`, []string{`"a"|1`, `"a"|2`}},
		{"where", `MATCH (a:N {k:'a'})-[rs*]->(b) WHERE size(rs) > 1 RETURN b.k AS b, size(rs) AS s`, []string{`"c"|2`}},
		{"size_equals_length", `MATCH p = (a:N {k:'a'})-[rs*]->(b) WHERE size(rs) = length(p) RETURN b.k AS b, size(rs) AS s`, []string{`"b"|1`, `"c"|1`, `"c"|2`}},
		// Unchanged neighbours: a relationship list is still rejected where a
		// single relationship is.
		{"length_still_rejected", `MATCH (a)-[rs*]->(b) RETURN length(rs) AS b, 0 AS s`, []string{"ERROR"}},
		{"where_bare_still_rejected", `MATCH (a)-[rs*]->(b) WHERE rs RETURN 1 AS b, 0 AS s`, []string{"ERROR"}},
		// A single relationship stays rejected by size().
		{"size_single_rel_rejected", `MATCH (a)-[r]->(b) RETURN size(r) AS b, 0 AS s`, []string{"ERROR"}},
	}
	for _, c := range cases {
		got := rbRows(t, eng, c.query, "b", "s")
		if len(c.want) == 1 && c.want[0] == "ERROR" {
			if len(got) != 1 || len(got[0]) < 6 || got[0][:6] != "ERROR:" {
				t.Errorf("%s: %s\n got %v, want a compile-time error", c.name, c.query, got)
			}
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %s\n got  %v\n want %v", c.name, c.query, got, c.want)
		}
	}
}

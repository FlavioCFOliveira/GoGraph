package cypher_test

// varlen_list_with_identity_test.go — rmp #2917 and #2915.
//
//   - #2917. A variable-length relationship variable (`[rs*]`) carried through a
//     WITH lost its contents whenever the carried list landed in the column the
//     raw hop list had occupied below the WITH: `MATCH (a:A)-[rs*]->(b) WITH b,
//     rs RETURN size(rs)` returned 0 for every row, `UNWIND rs` returned no row,
//     and a WITH that re-bound the name (`WITH b, [7, 8, 9, 10] AS rs`) returned
//     a fabricated relationship. The variable is a List of relationships
//     (clauses/match/Match9.feature [1] "Variable length relationship variables
//     are lists of relationships") and WITH forwards a variable unchanged
//     (clauses/with/With1.feature [3] "Forwarding a relationship variable",
//     [4] "Forwarding a path variable"); a re-bound name is the new value.
//   - #2915. The post-Apply filter that stops a variable-length hop from
//     re-using a relationship bound before the pattern compared node PAIRS, so on
//     a multigraph it also rejected every PARALLEL relationship between the same
//     two nodes: `MATCH ()-[r]->() WITH r MATCH (a)-[r]->(b)<-[:R*1..1]-(a)
//     RETURN count(*)` returned 0 instead of 2. Relationship uniqueness is about
//     relationship identity (clauses/match/Match4.feature [7] "Matching variable
//     length patterns including a bound relationship"), and two parallel
//     relationships are distinct. The same filter fed a carried relationship
//     LIST to startNode(), which failed the query that re-uses it as a bound
//     list (clauses/match/Match9.feature [6] "Matching relationships into a list
//     and matching variable length using the list, with bound nodes").
//
// Layer: short. goleak-clean (engines and graphs are local).

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// vliEngine returns an in-memory multigraph engine after running writes.
func vliEngine(t *testing.T, writes ...string) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	for _, q := range writes {
		mustRunWrite(t, eng, q)
	}
	return eng
}

type vliCase struct {
	name, query string
	want        []string
}

func vliRun(t *testing.T, eng *cypher.Engine, cases []vliCase) {
	t.Helper()
	for _, c := range cases {
		if got := rbRows(t, eng, c.query, "n"); !slices.Equal(got, c.want) {
			t.Errorf("%s: %s\n got  %v\n want %v", c.name, c.query, got, c.want)
		}
	}
}

// TestVarLengthRelListThroughWith is the #2917 regression. The chain is
// (:A)-[:T {w:1}]->(:B)-[:T {w:2}]->(:C), so `(a:A)-[rs*]->(b)` binds rs to [w1]
// (b = B) and to [w1, w2] (b = C). Every route must report those two lists.
func TestVarLengthRelListThroughWith(t *testing.T) {
	t.Parallel()
	eng := vliEngine(t, `CREATE (:A {k:'a'})-[:T {w:1}]->(:B {k:'b'})-[:T {w:2}]->(:C {k:'c'})`)
	sizes := []string{"1", "2"}
	ws := []string{"[1, 2]", "[1]"}
	const m = `MATCH (a:A)-[rs*]->(b) `
	vliRun(t, eng, []vliCase{
		// Controls: no WITH, and a named path's relationships through WITH.
		{"return_direct", m + `RETURN size(rs) AS n`, sizes},
		{"return_with_node", m + `RETURN b, [r IN rs | r.w] AS n`, ws},
		{"path_control", `MATCH p = (a:A)-[*]->(b) WITH b, p RETURN [r IN relationships(p) | r.w] AS n`, ws},
		// Item order: rs after another item is the order that lost the list.
		{"with_rs_only", m + `WITH rs RETURN size(rs) AS n`, sizes},
		{"with_rs_b", m + `WITH rs, b RETURN size(rs) AS n`, sizes},
		{"with_b_rs", m + `WITH b, rs RETURN size(rs) AS n`, sizes},
		{"with_a_rs", m + `WITH a, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"with_rs_a", m + `WITH rs, a RETURN [r IN rs | r.w] AS n`, ws},
		{"with_a_b_rs", m + `WITH a, b, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"with_expr_rs", m + `WITH b.k AS k, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"with_b_rs_const", m + `WITH b, rs, 1 AS one RETURN [r IN rs | r.w] AS n`, ws},
		{"with_alias", m + `WITH b, rs AS x RETURN size(x) AS n`, sizes},
		{"with_b_rs_mixed", m + `WITH b, rs RETURN b.k + ':' + toString(size(rs)) AS n`, []string{`"b:1"`, `"c:2"`}},
		// Consumers of the carried list.
		{"return_list", m + `WITH b, rs RETURN [r IN rs | type(r)] AS n`, []string{`["T", "T"]`, `["T"]`}},
		{"length", m + `WITH b, rs RETURN length(rs) AS n`, sizes},
		{"unwind", m + `WITH b, rs UNWIND rs AS r RETURN r.w AS n`, []string{"1", "1", "2"}},
		{"where", m + `WITH b, rs WHERE size(rs) = 2 RETURN size(rs) AS n`, []string{"2"}},
		{"order_by", m + `WITH b, rs ORDER BY size(rs) DESC RETURN collect(size(rs)) AS n`, []string{"[2, 1]"}},
		{"distinct", m + `WITH DISTINCT b, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"collect", m + `WITH b, collect(rs) AS c RETURN [x IN c | size(x)] AS n`, []string{"[1]", "[2]"}},
		{"group_key", m + `WITH b, rs, count(*) AS c RETURN [r IN rs | r.w] AS n`, ws},
		{"group_key_a", m + `WITH a, rs, count(*) AS c RETURN [r IN rs | r.w] AS n`, ws},
		{"two_withs", m + `WITH b, rs WITH b, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"with_then_match", m + `WITH b, rs MATCH (b) RETURN size(rs) AS n`, sizes},
		// Bounds, types, OPTIONAL MATCH, named path.
		{"bounded", `MATCH (a:A)-[rs*1..2]->(b) WITH b, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"typed", `MATCH (a:A)-[rs:T*]->(b) WITH b, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"optional", `MATCH (a:A) OPTIONAL MATCH (a)-[rs*]->(b) WITH b, rs RETURN [r IN rs | r.w] AS n`, ws},
		{"named_path", `MATCH p = (a:A)-[rs*]->(b) WITH b, rs RETURN [r IN rs | r.w] AS n`, ws},
		// A re-bound name is the new value, not a decoding of it.
		{"rebind", m + `WITH b, [7, 8, 9, 10] AS rs RETURN rs AS n`, []string{"[7, 8, 9, 10]", "[7, 8, 9, 10]"}},
	})
}

// TestVLENoRepeatRelIdentity is the #2915 regression. Three parallel a->b
// relationships (w = 1, 2, 3): with r bound before the pattern, the
// variable-length hop may cross either OTHER parallel relationship and never r
// itself.
func TestVLENoRepeatRelIdentity(t *testing.T) {
	t.Parallel()
	eng := vliEngine(t, `CREATE (a:A), (b:B), (a)-[:R {w:1}]->(b), (a)-[:R {w:2}]->(b), (a)-[:R {w:3}]->(b)`)
	pairs := []string{"[1, 2]", "[1, 3]", "[2, 1]", "[2, 3]", "[3, 1]", "[3, 2]"}
	const w = `MATCH ()-[r]->() WITH r `
	vliRun(t, eng, []vliCase{
		{"count", w + `MATCH (a)-[r]->(b)<-[:R*1..1]-(a) RETURN count(*) AS n`, []string{"6"}},
		{"count_upto_2", w + `MATCH (a)-[r]->(b)<-[:R*1..2]-(a) RETURN count(*) AS n`, []string{"6"}},
		{"untyped", w + `MATCH (a)-[r]->(b)<-[*1..1]-(a) RETURN count(*) AS n`, []string{"6"}},
		{"undirected", w + `MATCH (a)-[r]->(b)-[:R*1..1]-(a) RETURN count(*) AS n`, []string{"6"}},
		{"which", w + `MATCH (a)-[r]->(b)<-[q:R*1..1]-(a) RETURN [r.w] + [e IN q | e.w] AS n`, pairs},
		{"never_self", w + `MATCH (a)-[r]->(b)<-[q:R*1..1]-(a) RETURN r IN q AS n`, []string{"false", "false", "false", "false", "false", "false"}},
		// Controls: the fixed-length hop, which never used this filter.
		{"fixed_hop", w + `MATCH (a)-[r]->(b)<-[:R]-(a) RETURN count(*) AS n`, []string{"6"}},
	})
}

// TestVarLengthRelListReusedAsBoundList covers the carried relationship list
// re-used as a bound variable-length list after WITH (Match9 [6]); it needs both
// fixes: #2917 keeps the list's contents, #2915 stops the no-repeat filter from
// feeding the list to startNode().
func TestVarLengthRelListReusedAsBoundList(t *testing.T) {
	t.Parallel()
	eng := vliEngine(t, `CREATE (:A {k:'a'})-[:T]->(:B {k:'b'})-[:T]->(:C {k:'c'})`)
	want := []string{`"ab"`, `"ac"`}
	const m = `MATCH (a:A)-[rs*]->(b) `
	vliRun(t, eng, []vliCase{
		{"with_rs", m + `WITH rs MATCH (x)-[rs*]->(y) RETURN x.k + y.k AS n`, want},
		{"with_b_rs", m + `WITH b, rs MATCH (x)-[rs*]->(y) RETURN x.k + y.k AS n`, want},
		{"control_copied_list", m + `WITH b, [r IN rs | r] AS q MATCH (x)-[q*]->(y) RETURN x.k + y.k AS n`, want},
	})
}

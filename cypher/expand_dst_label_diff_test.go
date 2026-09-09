package cypher_test

// expand_dst_label_diff_test.go — the far-endpoint label push (rmp #2629).
//
// Three things are pinned here, and they are different kinds of claim:
//
//  1. THE REWRITE FIRES, and the plan loses the filter. This is the regression
//     witness for the reported defect: `MATCH (:USER)-[:FRIEND]->(:USER) RETURN
//     count(*)` used to plan CountRows → ColumnarFilter → columnarExpand → scan,
//     with the filter removing NOTHING and costing about 70% of the query. The
//     same test drives the DISABLED arm and asserts the filter IS there, so it
//     cannot pass vacuously and it records what the pre-change plan was.
//
//  2. THE ANSWER IS UNCHANGED, differentially against the pre-change plan rather
//     than against a golden file — the same graph, the same queries, two engines
//     differing in exactly one option. The battery covers the four shapes the
//     task named (labelled, unlabelled, mismatched, two DIFFERENT labels) and the
//     shapes that must be REFUSED (an already-bound destination, more than one
//     predicate), plus reverse and undirected hops, a multi-label destination, a
//     never-interned label, row-returning forms, and a graph whose labels were
//     REMOVED after the edges were created — which is where a gate reading the
//     label index instead of the snapshot-aware bag would diverge.
//
//  3. THE ACCOUNTING SURVIVES. The removed filter used to report the rows it
//     dropped; the expansion must now report them, or PROFILE silently loses
//     them.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// dstLabelFixture builds a small mixed-label graph:
//
//	u0..u11   :USER, of which u0, u3, u6, u9 are also :VIP
//	a0..a3    :ARTICLE
//	FRIEND    ui -> u(i+1 mod 12)  and  ui -> u(i+5 mod 12)   (USER -> USER)
//	FOLLOW    u0 -> u2                                        (a second USER type)
//	LIKE      ui -> a(i mod 4)                                (USER -> ARTICLE)
//	SELF      u4 -> u4                                        (a self-loop)
//
// Every node carries a `name` property so a row-returning query has something
// deterministic to order by.
func dstLabelFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 12; i++ {
		k := "u" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "USER"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
		if i%3 == 0 {
			// SetNodeLabel ATTACHES, so a second call adds a second label.
			if err := g.SetNodeLabel(k, "VIP"); err != nil {
				t.Fatalf("SetNodeLabel %s VIP: %v", k, err)
			}
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	for i := 0; i < 4; i++ {
		k := "a" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "ARTICLE"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	edge := func(src, dst, typ string) {
		if err := g.AddEdgeLabeledWithProperty(src, dst, 1, typ, "w", lpg.Int64Value(1)); err != nil {
			t.Fatalf("AddEdgeLabeledWithProperty %s-[%s]->%s: %v", src, typ, dst, err)
		}
	}
	for i := 0; i < 12; i++ {
		u := "u" + strconv.Itoa(i)
		edge(u, "u"+strconv.Itoa((i+1)%12), "FRIEND")
		edge(u, "u"+strconv.Itoa((i+5)%12), "FRIEND")
		edge(u, "a"+strconv.Itoa(i%4), "LIKE")
	}
	edge("u0", "u2", "FOLLOW")
	edge("u4", "u4", "SELF")
	return g
}

// dstLabelQueries is the differential battery. Every entry must return the same
// rows whether or not the push fires; `pushes` records how many times the rewrite
// is EXPECTED to fire, which is what stops the equivalence half from passing
// vacuously on a query the rewrite never touched.
var dstLabelQueries = []struct {
	name   string
	query  string
	pushes uint64
}{
	// The four shapes the task named.
	{"labelled_same", "MATCH (:USER)-[:FRIEND]->(:USER) RETURN count(*) AS c", 1},
	{"unlabelled", "MATCH ()-[:FRIEND]->() RETURN count(*) AS c", 0},
	{"mismatched", "MATCH (:USER)-[:FRIEND]->(:ARTICLE) RETURN count(*) AS c", 1},
	{"different_labels", "MATCH (:USER)-[:LIKE]->(:ARTICLE) RETURN count(*) AS c", 1},

	// Source label only — no destination predicate exists, so nothing to push.
	{"src_label_only", "MATCH (:USER)-[:FRIEND]->() RETURN count(*) AS c", 0},
	// Destination label only, over an all-nodes anchor.
	{"dst_label_only", "MATCH ()-[:FRIEND]->(:USER) RETURN count(*) AS c", 1},

	// A multi-label destination is a single bare conjunctive LabelPredicate, so it
	// pushes as one gate testing every label.
	{"multi_label_dst", "MATCH (:USER)-[:FRIEND]->(:USER:VIP) RETURN count(*) AS c", 1},
	// A label the registry has never interned: carried by no node, so zero.
	{"never_interned_dst", "MATCH (:USER)-[:FRIEND]->(:NOSUCHLABEL) RETURN count(*) AS c", 1},

	// Direction variants: the gate is applied on the emit branch of BOTH cursors.
	{"reverse_hop", "MATCH (:USER)<-[:FRIEND]-(:VIP) RETURN count(*) AS c", 1},
	{"undirected_hop", "MATCH (:VIP)-[:FRIEND]-(:USER) RETURN count(*) AS c", 1},

	// Untyped hop: the type filter is absent, so every adjacency slot reaches the
	// gate.
	{"untyped_hop", "MATCH (:USER)-->(:ARTICLE) RETURN count(*) AS c", 1},

	// REFUSED — an already-bound destination (expand-into). ToVar names a
	// synthetic column an equality Selection also reads, so the label predicate is
	// not the only thing above the Expand and IntoVar vetoes the push outright.
	{"expand_into", "MATCH (a:USER)-[:FRIEND]->(b:USER)-[:FRIEND]->(a) RETURN count(*) AS c", 0},
	// REFUSED — a second predicate above the Expand. Pushing one conjunct earlier
	// than another is what the single-predicate restriction exists to avoid.
	{"two_predicates", "MATCH (:USER)-[:FRIEND]->(n:USER) WHERE n.name > 'u1' RETURN count(*) AS c", 0},

	// Row-returning forms: the push is confined to the aggregate source, so these
	// must be untouched AND identical.
	{"rows_far_name", "MATCH (:USER)-[:FRIEND]->(u:USER) RETURN u.name AS n ORDER BY n", 0},
	{"rows_both_names", "MATCH (a:USER)-[:FRIEND]->(b:VIP) RETURN a.name AS an, b.name AS bn ORDER BY an, bn", 0},
	// A GROUPED aggregate over the same hop. The push serves this too — the source
	// is built the same way — and the grouping key is a RELATIONSHIP property, so
	// the expansion's edge column must still reach the aggregate intact.
	{"rows_rel_prop", "MATCH (:USER)-[r:FRIEND]->(:USER) RETURN r.w AS w, count(*) AS c ORDER BY w", 1},

	// Self-loop: source and destination are the same node, so the gate sees it.
	{"self_loop", "MATCH (:USER)-[:SELF]->(:USER) RETURN count(*) AS c", 1},
	// Two relationship types on one hop.
	{"multi_type", "MATCH (:USER)-[:FRIEND|FOLLOW]->(:USER) RETURN count(*) AS c", 1},
	// A count over a longer pattern: only the LAST hop's destination label stands
	// alone above its Expand.
	{"two_hops", "MATCH (:USER)-[:FRIEND]->(:USER)-[:LIKE]->(:ARTICLE) RETURN count(*) AS c", 0},
}

// dstLabelRows executes q and renders its rows as canonical strings in emission order.
func dstLabelRows(t *testing.T, eng *cypher.Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			v, ok := rec[c]
			switch {
			case !ok:
				parts = append(parts, c+"=<missing>")
			case v == nil:
				parts = append(parts, c+"=<nil>")
			default:
				parts = append(parts, fmt.Sprintf("%s=%T:%v", c, v, v))
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err %q: %v", q, err)
	}
	return out
}

// TestExpandDstLabelPush_AnswerEquivalence is the correctness gate: the same graph
// and the same queries under two engines differing in exactly one option must
// return identical rows, in identical order.
//
// Order is compared as EMITTED, not sorted, because the gate drops slots in place
// and reorders nothing — so equality of the sequence is the honest claim, and a
// sorted comparison would hide a reordering the rewrite must not cause.
func TestExpandDstLabelPush_AnswerEquivalence(t *testing.T) {
	g := dstLabelFixture(t)
	push := cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	nopush := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableExpandLabelPush: true})

	for _, q := range dstLabelQueries {
		t.Run(q.name, func(t *testing.T) {
			before := cypher.ExpandDstLabelPushCount()
			got := dstLabelRows(t, push, q.query)
			fired := cypher.ExpandDstLabelPushCount() - before
			want := dstLabelRows(t, nopush, q.query)

			if len(got) != len(want) {
				t.Fatalf("row count %d with the push, %d without:\n push: %q\n  no: %q",
					len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d differs:\n push: %s\n  no: %s", i, got[i], want[i])
				}
			}
			if fired != q.pushes {
				t.Fatalf("the rewrite fired %d time(s), expected %d — the plan is not the one this case means to compare", fired, q.pushes)
			}
			// A count that came back zero from BOTH arms would satisfy the
			// comparison while proving nothing about a traversal. Guard the shapes
			// that must have found something.
			if q.name == "labelled_same" || q.name == "different_labels" {
				if len(got) != 1 || strings.HasSuffix(got[0], ":0") {
					t.Fatalf("%s produced no matches at all: %q", q.name, got)
				}
			}
		})
	}
}

// TestExpandDstLabelPush_NoPushWithoutTheGate confirms the DISABLED arm never
// fires the rewrite, over the whole battery. Without this the equivalence test
// above could be comparing two identical plans and calling it equivalence.
func TestExpandDstLabelPush_NoPushWithoutTheGate(t *testing.T) {
	g := dstLabelFixture(t)
	nopush := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableExpandLabelPush: true})
	before := cypher.ExpandDstLabelPushCount()
	for _, q := range dstLabelQueries {
		dstLabelRows(t, nopush, q.query)
	}
	if fired := cypher.ExpandDstLabelPushCount() - before; fired != 0 {
		t.Fatalf("DisableExpandLabelPush still pushed %d time(s)", fired)
	}
}

// TestExpandDstLabelPush_PlanDropsTheFilter is the regression witness. The
// reported defect WAS the operator this asserts the absence of.
func TestExpandDstLabelPush_PlanDropsTheFilter(t *testing.T) {
	g := dstLabelFixture(t)
	const q = "MATCH (:USER)-[:FRIEND]->(:USER) RETURN count(*) AS c"

	pushed, err := cypher.NewEngineWithOptions(g, cypher.EngineOptions{}).
		Profile(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Profile (push): %v", err)
	}
	legacy, err := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableExpandLabelPush: true}).
		Profile(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Profile (no push): %v", err)
	}

	// The pre-change plan, recorded so a future reader can see what was removed.
	if !strings.Contains(legacy, "ColumnarFilter") {
		t.Fatalf("the disabled arm no longer plans a filter, so this test has stopped\n"+
			"witnessing the defect it was written for. Plan:\n%s", legacy)
	}
	if strings.Contains(pushed, "Filter") {
		t.Fatalf("the destination label is still enforced by a filter:\n%s", pushed)
	}
	if !strings.Contains(pushed, "columnarExpand") || !strings.Contains(pushed, "NodeByLabelScan") {
		t.Fatalf("expected CountRows over a columnar expansion over a label scan:\n%s", pushed)
	}
}

// TestExpandDstLabelPush_RejectedSlotsStayAccounted pins the accounting. The
// filter that used to sit above the expansion reported the rows it dropped; with
// it gone, the expansion must report them, so a reader of PROFILE still sees
// where the rows went.
//
// The mismatched shape is the sharpest case: every FRIEND slot passes the type
// filter and every one of them fails the label gate, so the expansion emits zero
// rows and must account for EVERY slot it read.
func TestExpandDstLabelPush_RejectedSlotsStayAccounted(t *testing.T) {
	g := dstLabelFixture(t)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	plan, err := eng.Profile(context.Background(),
		"MATCH (:USER)-[:FRIEND]->(:ARTICLE) RETURN count(*) AS c", nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	line := ""
	for _, l := range strings.Split(plan, "\n") {
		if strings.Contains(l, "columnarExpand") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no columnarExpand line in:\n%s", plan)
	}
	hits := planField(t, line, "dbhits=")
	removed := planField(t, line, "removed=")
	rows := planField(t, line, "rows=")
	if rows != 0 {
		t.Fatalf("mismatched labels emitted %d rows, want 0: %s", rows, line)
	}
	// Every slot the expansion consumed was rejected: by the type filter (a LIKE
	// or FOLLOW or SELF slot) or by the endpoint gate (a FRIEND slot landing on a
	// :USER, which is not an :ARTICLE).
	if removed != hits {
		t.Fatalf("expansion read %d slots and accounted for %d removed; the pushed gate's\n"+
			"rejections are not being charged: %s", hits, removed, line)
	}
}

// planField extracts an integer field such as `rows=123` from a rendered plan
// line.
func planField(t *testing.T, line, key string) int {
	t.Helper()
	i := strings.Index(line, key)
	if i < 0 {
		t.Fatalf("no %q in plan line: %s", key, line)
	}
	rest := line[i+len(key):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		t.Fatalf("field %q is not a number in plan line: %s", key, line)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		t.Fatalf("field %q: %v", key, err)
	}
	return n
}

// TestExpandDstLabelPush_SnapshotAgreesAfterLabelRemoval is the case a gate built
// on the label INDEX rather than the snapshot-aware label bag would get wrong.
//
// The labels are removed AFTER the edges exist, so the destination nodes are
// still reachable through live FRIEND edges while no longer carrying :VIP. Both
// arms must agree, and the count must actually drop — otherwise the removal did
// nothing and the case is vacuous.
func TestExpandDstLabelPush_SnapshotAgreesAfterLabelRemoval(t *testing.T) {
	g := dstLabelFixture(t)
	push := cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	nopush := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableExpandLabelPush: true})
	const q = "MATCH (:USER)-[:FRIEND]->(:VIP) RETURN count(*) AS c"

	beforeRows := dstLabelRows(t, push, q)
	if got := dstLabelRows(t, nopush, q); !dstLabelRowsEqual(beforeRows, got) {
		t.Fatalf("before the removal the two arms already disagree: %q vs %q", beforeRows, got)
	}

	res, err := push.RunAny(context.Background(), "MATCH (n:VIP) REMOVE n:VIP RETURN count(*) AS c", nil)
	if err != nil {
		t.Fatalf("REMOVE n:VIP: %v", err)
	}
	for res.Next() { // drain so the write commits before the reads below
	}
	if err := res.Err(); err != nil {
		t.Fatalf("REMOVE n:VIP drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("REMOVE n:VIP close: %v", err)
	}

	afterPush := dstLabelRows(t, push, q)
	afterNoPush := dstLabelRows(t, nopush, q)
	if !dstLabelRowsEqual(afterPush, afterNoPush) {
		t.Fatalf("after removing the label the arms disagree: push %q, no push %q", afterPush, afterNoPush)
	}
	if dstLabelRowsEqual(beforeRows, afterPush) {
		t.Fatalf("removing :VIP from every node changed nothing (%q), so this case proves nothing", afterPush)
	}
	if len(afterPush) != 1 || !strings.HasSuffix(afterPush[0], ":0") {
		t.Fatalf("no node carries :VIP any more, so the count must be 0, got %q", afterPush)
	}
}

func dstLabelRowsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExpandDstLabelPush_MultiLabelDestinationIsAConjunction pins the semantics
// of a multi-label destination: the gate must require EVERY label, so the count
// is invariant under the labels' order and equals the count of the narrower
// label — every :VIP in the fixture is also a :USER — while being strictly
// smaller than the count for :USER alone.
func TestExpandDstLabelPush_MultiLabelDestinationIsAConjunction(t *testing.T) {
	g := dstLabelFixture(t)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	count := func(q string) string {
		rows := dstLabelRows(t, eng, q)
		if len(rows) != 1 {
			t.Fatalf("%q returned %d rows, want 1", q, len(rows))
		}
		return rows[0]
	}
	both := count("MATCH (:USER)-[:FRIEND]->(:USER:VIP) RETURN count(*) AS c")
	swapped := count("MATCH (:USER)-[:FRIEND]->(:VIP:USER) RETURN count(*) AS c")
	if both != swapped {
		t.Fatalf("a label conjunction is commutative: (:USER:VIP)=%s but (:VIP:USER)=%s", both, swapped)
	}
	// Every :VIP is a :USER in this fixture, so the conjunction collapses to :VIP.
	if vip := count("MATCH (:USER)-[:FRIEND]->(:VIP) RETURN count(*) AS c"); both != vip {
		t.Fatalf("(:USER:VIP)=%s but (:VIP)=%s, though every VIP in the fixture is a USER", both, vip)
	}
	// And it must be a real narrowing of :USER alone, or the conjunction was never
	// exercised as a conjunction.
	if user := count("MATCH (:USER)-[:FRIEND]->(:USER) RETURN count(*) AS c"); both == user {
		t.Fatalf("(:USER:VIP) matched as much as (:USER) (%s); the second label removed nothing", both)
	}
}

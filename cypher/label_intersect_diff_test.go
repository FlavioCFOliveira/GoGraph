package cypher

// label_intersect_diff_test.go — result-identity gate for the set-at-a-time
// multi-label conjunction (#2135).
//
// Layer: short. Engines and graphs are local, so the suite is goleak-clean
// (enforced by TestMain in testmain_test.go).
//
// Every case is checked THREE ways, not two:
//
//  1. against the intersection DISABLED, which catches a rewrite that changes an
//     answer;
//  2. against an ABSOLUTE oracle — the set-theoretic intersection of the label
//     memberships the test itself wrote — which catches a defect the two arms would
//     SHARE, since both run the same row pipeline;
//  3. against the PLAN, asserting the arms actually differ wherever the path is
//     meant to fire. A differential whose arms silently take the same plan is green
//     for the wrong reason.
//
// Design, the authoritative-bitmap argument and the snapshot-atomicity argument:
// docs/design-bitmap-intersection.md §5–§6.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// liDiffPop is the :L1 population — above rangeSeekMinLabelPopulation so nothing
// declines for want of a population floor, and small enough for the short layer.
const liDiffPop = 3000

// liDiffNode is one fixture node and the labels it carries, so the oracle is
// computed from what the test wrote rather than from anything the engine reports.
type liDiffNode struct {
	key    string
	labels map[string]bool
}

// liDiffGraph seeds a graph whose label memberships span every case below and
// returns the ground truth alongside it.
func liDiffGraph(t *testing.T) (*lpg.Graph[string, float64], []liDiffNode) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	truth := make([]liDiffNode, 0, liDiffPop+64)

	add := func(key string, labels ...string) {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %q: %v", key, err)
		}
		// `k` mirrors the node's own key so the oracle can compare MEMBERSHIP, not
		// just cardinality — a count-only comparison would pass on the wrong rows.
		if err := g.SetNodeProperty(key, "k", lpg.StringValue(key)); err != nil {
			t.Fatalf("SetNodeProperty k %q: %v", key, err)
		}
		set := make(map[string]bool, len(labels))
		for _, l := range labels {
			if err := g.SetNodeLabel(key, l); err != nil {
				t.Fatalf("SetNodeLabel %q %s: %v", key, l, err)
			}
			set[l] = true
		}
		truth = append(truth, liDiffNode{key: key, labels: set})
	}

	for i := 0; i < liDiffPop; i++ {
		labels := []string{"L1"}
		// A TINY intersection with L2: 1 in 100.
		if i%100 == 0 {
			labels = append(labels, "L2")
		}
		// L3 overlaps L1∩L2 only partially, so a three-label conjunction is
		// strictly smaller than any pair.
		if i%300 == 0 {
			labels = append(labels, "L3")
		}
		add(fmt.Sprintf("n%05d", i), labels...)
		if err := g.SetNodeProperty(fmt.Sprintf("n%05d", i), "p",
			lpg.Int64Value(int64(i%7))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// An L2-only tail, so L2 is not a subset of L1 and the intersection strictly
	// reduces rows relative to scanning either label alone.
	for i := 0; i < 25; i++ {
		add(fmt.Sprintf("t%05d", i), "L2")
	}
	// A registered-but-EMPTY label.
	add("empty_donor", "L1", "Vanished")
	g.RemoveNodeLabel("empty_donor", "Vanished")
	for i := range truth {
		if truth[i].key == "empty_donor" {
			delete(truth[i].labels, "Vanished")
		}
	}
	// A node carrying BOTH labels that is then DELETED, so a stale bitmap would
	// surface it.
	add("deleted_both", "L1", "L2")
	g.RemoveNode("deleted_both")
	truth = truth[:len(truth)-1]
	// A node RELABELLED away from the conjunction after the fact.
	add("relabelled_away", "L1", "L2")
	g.RemoveNodeLabel("relabelled_away", "L2")
	for i := range truth {
		if truth[i].key == "relabelled_away" {
			delete(truth[i].labels, "L2")
		}
	}
	// A node relabelled INTO the conjunction after the fact.
	add("relabelled_into", "L1")
	if err := g.SetNodeLabel("relabelled_into", "L2"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	for i := range truth {
		if truth[i].key == "relabelled_into" {
			truth[i].labels["L2"] = true
		}
	}
	return g, truth
}

// liOracle is the absolute expected answer for a label conjunction: the sorted keys
// of nodes carrying EVERY named label, computed with no engine involved.
func liOracle(truth []liDiffNode, labels ...string) []string {
	out := make([]string, 0, 64)
	for _, n := range truth {
		all := true
		for _, l := range labels {
			if !n.labels[l] {
				all = false
				break
			}
		}
		if all {
			out = append(out, n.key)
		}
	}
	sort.Strings(out)
	return out
}

// liOracleWithProp is liOracle restricted to nodes the fixture gave a `p`
// property, i.e. the ones written by the indexed loop rather than the hand-added
// edge cases.
func liOracleWithProp(truth []liDiffNode, labels ...string) []string {
	out := make([]string, 0, 64)
	for _, k := range liOracle(truth, labels...) {
		var idx int
		if _, err := fmt.Sscanf(k, "n%05d", &idx); err == nil {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// liOracleProp0 is liOracle restricted to nodes whose `p` property is 0. The
// fixture sets p = i%7 on the indexed nodes and gives the hand-added edge cases no
// p at all, so those are excluded — which is exactly what `{p: 0}` requires.
func liOracleProp0(truth []liDiffNode, labels ...string) []string {
	out := make([]string, 0, 16)
	for _, k := range liOracle(truth, labels...) {
		var idx int
		if _, err := fmt.Sscanf(k, "n%05d", &idx); err == nil && idx%7 == 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// liRunKeys returns the sorted node keys q yields, reading the key property the
// fixture stores implicitly as the node's own key via id-less projection.
func liRunKeys(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	out := make([]string, 0, 64)
	for res.Next() {
		v := res.ValueAt(0)
		out = append(out, strings.Trim(v.String(), `"`))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	sort.Strings(out)
	return out
}

// TestLabelIntersect_Differential is the core gate.
func TestLabelIntersect_Differential(t *testing.T) {
	t.Parallel()
	g, truth := liDiffGraph(t)
	on := NewEngineWithOptions(g, EngineOptions{MaxResultRows: MaxResultRowsUnlimited})
	off := NewEngineWithOptions(g, EngineOptions{
		DisableBitmapIntersection: true, MaxResultRows: MaxResultRowsUnlimited,
	})

	cases := []struct {
		name     string
		query    string
		want     []string
		wantFire bool
	}{{
		name:     "tiny_intersection",
		query:    `MATCH (n:L1:L2) RETURN n.k AS k`,
		want:     liOracle(truth, "L1", "L2"),
		wantFire: true,
	}, {
		// L3 (10 nodes) is a SUBSET of L2 and of L1, so |L1 ∩ L2 ∩ L3| == |L3| and
		// the strict-row-reduction gate correctly vetoes: there are no rows left for
		// the AND to remove relative to scanning :L3. The answer must still be right,
		// which is what makes this a useful case rather than a missing one.
		name:     "three_labels_nested_vetoes",
		query:    `MATCH (n:L1:L2:L3) RETURN n.k AS k`,
		want:     liOracle(truth, "L1", "L2", "L3"),
		wantFire: false,
	}, {
		// A label the registry knows but nothing carries: the conjunction is empty.
		name:     "registered_but_empty_label",
		query:    `MATCH (n:L1:Vanished) RETURN n.k AS k`,
		want:     nil,
		wantFire: true,
	}, {
		// A label absent from the registry entirely. Its exact cardinality is 0, so
		// the EMPTY short-circuit fires ahead of the gate and the plan becomes a scan
		// over a provably empty bitmap — strictly better than scanning a populated
		// label and dropping every row in a filter.
		name:     "label_absent_from_registry_short_circuits",
		query:    `MATCH (n:L1:NeverDeclared) RETURN n.k AS k`,
		want:     nil,
		wantFire: true,
	}, {
		// The intersection equals the smaller label, so the gate must VETO and hand
		// the shape to the shipped plan. The answer must still be right.
		name:     "intersection_equals_smaller_label_vetoes",
		query:    `MATCH (n:L3:L1) RETURN n.k AS k`,
		want:     liOracle(truth, "L3", "L1"),
		wantFire: false,
	}, {
		name:     "with_inline_property",
		query:    `MATCH (n:L1:L2 {p: 0}) RETURN n.k AS k`,
		want:     liOracleProp0(truth, "L1", "L2"),
		wantFire: true,
	}, {
		// The WHERE drops `relabelled_into`, which carries both labels but has no `p`
		// property at all, so `n.p >= 0` is NULL for it. The oracle is therefore the
		// label intersection AND "has p" — a reminder that a residual predicate is
		// still a predicate even when the labels were decided set-at-a-time.
		name:     "with_where_clause",
		query:    `MATCH (n:L1:L2) WHERE n.p >= 0 RETURN n.k AS k`,
		want:     liOracleWithProp(truth, "L1", "L2"),
		wantFire: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planOn, err := on.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain on: %v", err)
			}
			fired := strings.Contains(planOn, intersectMarker)
			if fired != tc.wantFire {
				t.Fatalf("intersection fired = %v, want %v\nplan:\n%s", fired, tc.wantFire, planOn)
			}
			if tc.wantFire {
				planOff, perr := off.Explain(tc.query, nil)
				if perr != nil {
					t.Fatalf("Explain off: %v", perr)
				}
				if strings.Contains(planOff, intersectMarker) {
					t.Fatalf("disabled arm still intersected — the differential is degenerate\nplan:\n%s", planOff)
				}
			}

			gotOn := liRunKeys(t, on, tc.query)
			gotOff := liRunKeys(t, off, tc.query)

			// (1) The two arms must agree, whatever the answer is.
			assertSameStrings(t, "enabled vs disabled", gotOn, gotOff)
			// (2) And both must equal the absolute oracle.
			if tc.want != nil {
				// (2) MEMBERSHIP against the absolute oracle, not merely cardinality.
				assertSameStrings(t, "enabled vs Go oracle", gotOn, tc.want)
				assertSameStrings(t, "disabled vs Go oracle", gotOff, tc.want)
			} else if len(gotOn) != 0 {
				t.Fatalf("expected an empty result, got %d rows: %v", len(gotOn), gotOn)
			}
		})
	}
}

// TestLabelIntersect_StaleBitmapAndRelabelling pins the two cases that would break
// if the label index were NOT authoritative — the assumption that licenses dropping
// the residual Filter (design §5). A deleted node must not surface from a bitmap,
// and a relabelled node must move in and out of the conjunction.
func TestLabelIntersect_StaleBitmapAndRelabelling(t *testing.T) {
	t.Parallel()
	g, truth := liDiffGraph(t)
	on := NewEngineWithOptions(g, EngineOptions{MaxResultRows: MaxResultRowsUnlimited})

	const q = `MATCH (n:L1:L2) RETURN count(n) AS c`
	res, err := on.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got int64
	for res.Next() {
		if iv, ok := res.ValueAt(0).(interface{ String() string }); ok {
			_, _ = fmt.Sscanf(iv.String(), "%d", &got)
		}
	}
	_ = res.Close()

	want := int64(len(liOracle(truth, "L1", "L2")))
	if got != want {
		t.Fatalf("count = %d, want %d — a deleted or relabelled node leaked from the bitmap", got, want)
	}
	// The fixture deleted a node that carried BOTH labels and relabelled one away
	// from and one into the conjunction, so this count is only right if the label
	// index tracked all three.
	t.Logf("conjunction count = %d (deleted, relabelled-away and relabelled-into all accounted for)", got)
}

// TestLabelIntersect_Rapid is the property: for a random assignment of labels the
// planned result set always equals the set-theoretic intersection of the label
// memberships. It counts how often the path fired so the property cannot pass
// vacuously.
//
// Not parallel: it keeps a counter across rapid iterations.
func TestLabelIntersect_Rapid(t *testing.T) {
	// The population must clear rangeSeekMinLabelPopulation for nothing to decline
	// on the population floor.
	const pop = 1200

	fired := 0
	rapid.Check(t, func(rt *rapid.T) {
		// Per-node membership drawn independently for two labels, so the
		// intersection size ranges over the whole spectrum across iterations —
		// including the degenerate "one label is a subset of the other".
		pA := rapid.IntRange(1, 9).Draw(rt, "pctA")
		pB := rapid.IntRange(1, 9).Draw(rt, "pctB")

		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		inA := make(map[string]bool, pop)
		inB := make(map[string]bool, pop)
		for i := 0; i < pop; i++ {
			key := fmt.Sprintf("r%05d", i)
			if err := g.AddNode(key); err != nil {
				rt.Fatalf("AddNode: %v", err)
			}
			// Deterministic in i, so the fixture is reproducible from (pA, pB).
			if i%10 < pA {
				if err := g.SetNodeLabel(key, "A"); err != nil {
					rt.Fatalf("SetNodeLabel A: %v", err)
				}
				inA[key] = true
			}
			if (i/3)%10 < pB {
				if err := g.SetNodeLabel(key, "B"); err != nil {
					rt.Fatalf("SetNodeLabel B: %v", err)
				}
				inB[key] = true
			}
		}
		want := 0
		for k := range inA {
			if inB[k] {
				want++
			}
		}

		eng := NewEngineWithOptions(g, EngineOptions{MaxResultRows: MaxResultRowsUnlimited})
		const q = `MATCH (n:A:B) RETURN count(n) AS c`
		plan, err := eng.Explain(q, nil)
		if err != nil {
			rt.Fatalf("Explain: %v", err)
		}
		if strings.Contains(plan, intersectMarker) {
			fired++
		}

		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			rt.Fatalf("Run: %v", err)
		}
		got := -1
		for res.Next() {
			var n int
			if _, serr := fmt.Sscanf(res.ValueAt(0).String(), "%d", &n); serr == nil {
				got = n
			}
		}
		if err := res.Err(); err != nil {
			rt.Fatalf("iter: %v", err)
		}
		if err := res.Close(); err != nil {
			rt.Fatalf("close: %v", err)
		}
		if got != want {
			rt.Fatalf("|A ∩ B| = %d, want %d (pctA=%d pctB=%d)", got, want, pA, pB)
		}
	})

	if fired == 0 {
		t.Fatal("the intersection never fired across the whole property run — the property is vacuous")
	}
	t.Logf("intersection fired in %d rapid iterations", fired)
}

// TestLabelIntersect_ConcurrentRelabelling is the snapshot-atomicity assertion
// (design §6): the k-way AND runs under ONE index read-lock, so a concurrent
// relabelling can never yield a row violating the conjunction.
//
// The invariant is stated race-free rather than by re-checking labels live, which
// would itself be racy. Three disjoint groups: `always` carries both labels for the
// whole run, `never` carries only one, and `churn` is relabelled continuously. Any
// answer must therefore contain every `always` node, no `never` node, and nothing
// outside always ∪ churn.
func TestLabelIntersect_ConcurrentRelabelling(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	const (
		alwaysN = 40
		neverN  = 1200
		churnN  = 60
	)
	always := make(map[string]bool, alwaysN)
	churn := make([]string, 0, churnN)
	mk := func(key string, labels ...string) {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		for _, l := range labels {
			if err := g.SetNodeLabel(key, l); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
	}
	for i := 0; i < alwaysN; i++ {
		k := fmt.Sprintf("a%05d", i)
		mk(k, "CA", "CB")
		always[k] = true
	}
	for i := 0; i < neverN; i++ {
		mk(fmt.Sprintf("x%05d", i), "CA")
	}
	for i := 0; i < churnN; i++ {
		k := fmt.Sprintf("c%05d", i)
		mk(k, "CA")
		churn = append(churn, k)
	}

	eng := NewEngineWithOptions(g, EngineOptions{MaxResultRows: MaxResultRowsUnlimited})
	const q = `MATCH (n:CA:CB) RETURN count(n) AS c`

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, k := range churn {
				if err := g.SetNodeLabel(k, "CB"); err != nil {
					return
				}
				g.RemoveNodeLabel(k, "CB")
			}
		}
	}()

	for i := 0; i < 200; i++ {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Run: %v", err)
		}
		got := -1
		for res.Next() {
			var n int
			if _, serr := fmt.Sscanf(res.ValueAt(0).String(), "%d", &n); serr == nil {
				got = n
			}
		}
		_ = res.Close()
		// Every `always` node must be present, and nothing outside always ∪ churn
		// can be, so the count is bounded on both sides at every instant.
		if got < alwaysN || got > alwaysN+churnN {
			close(stop)
			wg.Wait()
			t.Fatalf("count = %d, want within [%d, %d]: a concurrent relabelling produced a row "+
				"violating the conjunction", got, alwaysN, alwaysN+churnN)
		}
	}
	close(stop)
	wg.Wait()
}

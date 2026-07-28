package cypher

// hash_join_order_test.go — the order-preservation proof for the
// disconnected-equi-join hash join (rmp #2225 part B).
//
// # Why this file exists
//
// [exec.HashJoin]'s own contract claims only MULTISET identity with the
// nested-loop Cartesian product it replaces: "only emission ORDER differs, which
// openCypher leaves unspecified absent ORDER BY". The planner therefore carried
// an order-safety guard ([hashJoinOrderSafe]) that disabled the substitution for
// the whole query whenever a bare LIMIT/SKIP or a collect() could observe the
// order — and the write path refused the substitution outright, because `SET` is
// last-write-wins and a reordering would change the final graph state.
//
// That contract understates what the implementation actually delivers. The
// substitution is order-PRESERVING: the emitted sequence is row-for-row identical
// to the nested loop. [hashJoinBuildOnLeft] states the four-step argument;
// this file is its empirical half, because a documented invariant that nothing
// checks is an invariant waiting to be optimised away — the specific way it would
// break is somebody teaching the operator to self-select the smaller build side.
//
// # What is compared
//
// Not the multiset. The full row SEQUENCE, position by position, between an
// Engine with the hash join enabled and one constructed with
// EngineOptions.DisableHashJoin, over the same fixture. Each case asserts the
// join actually fired on one arm and not the other, so a case can never silently
// degrade into comparing one plan with itself.
//
// Both operators are covered and each case records which one it exercised:
// [exec.ColumnarHashJoin] when both arms are bare scans, the row-mode
// [exec.HashJoin] otherwise (an Expand or a filtered subtree on an arm).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// hashJoinOrderFixture seeds n :P nodes. Keys repeat heavily so buckets hold
// several rows and the within-bucket order is actually exercised; every 7th node
// has NO age property at all (a NULL key, which the join excludes and the nested
// loop filters out), and every 11th carries a float age whose value is integral,
// so cross-type numeric equality (integer 3 = float 3.0) puts unequal-typed but
// equal-valued rows in the same bucket.
func hashJoinOrderFixture(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(id): %v", err)
		}
		switch {
		case i%7 == 0:
			// No age: a NULL join key.
		case i%11 == 0:
			if err := g.SetNodeProperty(key, "age", lpg.Float64Value(float64(i%13))); err != nil {
				t.Fatalf("SetNodeProperty(age float): %v", err)
			}
		default:
			if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i%13))); err != nil {
				t.Fatalf("SetNodeProperty(age int): %v", err)
			}
		}
	}
	// A relationship layer so a case can put an Expand on one arm and force the
	// row-mode operator.
	for i := 0; i+1 < n; i += 3 {
		src, dst := fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1)
		if err := g.AddEdge(src, dst, 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(src, dst, "K")
	}
	return g
}

// hashJoinOrderRun executes q and renders every row as one comparable string,
// preserving emission order.
func hashJoinOrderRun(t *testing.T, eng *Engine, q string, params map[string]any) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		var b strings.Builder
		for i := range res.Columns() {
			fmt.Fprintf(&b, "%v\x1f", res.ValueAt(i))
		}
		out = append(out, b.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	return out
}

// TestHashJoinOrder_SequenceMatchesNestedLoop is the order-preservation gate. Any
// change that lets the operator pick its build side by cardinality fails here.
func TestHashJoinOrder_SequenceMatchesNestedLoop(t *testing.T) {
	unwindRows := make([]any, 0, 40)
	for i := 0; i < 40; i++ {
		unwindRows = append(unwindRows, map[string]any{"a": int64(i % 13)})
	}

	cases := []struct {
		name     string
		q        string
		params   map[string]any
		columnar bool // which operator this shape is expected to build
	}{
		{
			name:     "two bare scans, scalar projection",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id`,
			columnar: true,
		},
		{
			name:     "two bare scans, entity projection",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a, b`,
			columnar: true,
		},
		{
			name:     "residual predicate above the key",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age AND a.id < b.id RETURN a.id, b.id`,
			columnar: true,
		},
		{
			name:     "expand on the outer arm forces the row-mode operator",
			q:        `MATCH (a:P)-[:K]->(x:P), (b:P) WHERE a.age = b.age RETURN a.id, x.id, b.id`,
			columnar: false,
		},
		{
			name:     "expand on the inner arm forces the row-mode operator",
			q:        `MATCH (a:P), (b:P)-[:K]->(y:P) WHERE a.age = b.age RETURN a.id, b.id, y.id`,
			columnar: false,
		},
		{
			name:     "UNWIND-driven probe (the bulk-load read control)",
			q:        `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN r.a, b.id`,
			params:   map[string]any{"rows": unwindRows},
			columnar: false,
		},

		// ── The shapes the read path's order-safety scan used to exclude (rmp #2236's
		// sibling, #2234). Each one is an operator that can OBSERVE row order, so
		// order identity here is the whole justification for retiring that scan. A
		// bare LIMIT is the sharpest case in the set: openCypher 9 §8.4 leaves WHICH
		// rows come back unspecified, so a divergence would be legal — and still a
		// visible behaviour change. It is the one shape where the order argument has
		// to be exactly right rather than merely plausible.
		{
			name:     "bare LIMIT without ORDER BY (order is observable, spec leaves it free)",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id LIMIT 25`,
			columnar: true,
		},
		{
			name:     "bare SKIP without ORDER BY",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id SKIP 40`,
			columnar: true,
		},
		{
			name:     "bare SKIP and LIMIT together",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id SKIP 40 LIMIT 25`,
			columnar: true,
		},
		{
			name:     "collect() materialises rows in arrival order",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, collect(b.id)`,
			columnar: true,
		},
		{
			name:     "collect(DISTINCT) keeps first-occurrence order",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, collect(DISTINCT b.id)`,
			columnar: true,
		},
		{
			name:     "collect() over the whole result, one row, maximal order exposure",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN collect(a.id), collect(b.id)`,
			columnar: true,
		},
		{
			name: "bare LIMIT with an Expand on an arm (row-mode operator)",
			q: `MATCH (a:P)-[:K]->(x:P), (b:P) WHERE a.age = b.age ` +
				`RETURN a.id, x.id, b.id LIMIT 25`,
			columnar: false,
		},
		{
			name: "collect() with an Expand on an arm (row-mode operator)",
			q: `MATCH (a:P)-[:K]->(x:P), (b:P) WHERE a.age = b.age ` +
				`RETURN a.id, collect(b.id)`,
			columnar: false,
		},
		{
			// The CONTROL for the group above: the scan already permitted a LIMIT
			// dominated by an ORDER BY, so this shape fired before and after. It keeps
			// the group honest — if every case above started failing because the join
			// stopped firing at all, this one would too, distinguishing "the guard was
			// retired" from "the substitution broke".
			name:     "LIMIT under ORDER BY (permitted before the scan was retired)",
			q:        `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id ORDER BY a.id, b.id LIMIT 25`,
			columnar: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			on := NewEngine(hashJoinOrderFixture(t, 150))
			off := NewEngineWithOptions(hashJoinOrderFixture(t, 150), EngineOptions{DisableHashJoin: true})

			beforeAll := hashJoinBuildCount.Load()
			beforeCol := hashJoinColumnarBuildCount.Load()
			gotOn := hashJoinOrderRun(t, on, tc.q, tc.params)
			firedAll := hashJoinBuildCount.Load() - beforeAll
			firedCol := hashJoinColumnarBuildCount.Load() - beforeCol
			if firedAll == 0 {
				t.Fatalf("the hash join did not fire; this case is comparing one plan with "+
					"itself and proves nothing. Query: %s", tc.q)
			}
			if gotCol := firedCol > 0; gotCol != tc.columnar {
				t.Fatalf("expected columnar=%v, got columnar=%v — the case no longer covers the "+
					"operator it was written for", tc.columnar, gotCol)
			}

			beforeAll = hashJoinBuildCount.Load()
			gotOff := hashJoinOrderRun(t, off, tc.q, tc.params)
			if hashJoinBuildCount.Load() != beforeAll {
				t.Fatal("the hash join fired on the OFF arm; DisableHashJoin is not taking effect")
			}

			if len(gotOn) != len(gotOff) {
				t.Fatalf("row COUNT differs: hash join %d, nested loop %d", len(gotOn), len(gotOff))
			}
			if len(gotOn) == 0 {
				t.Fatal("the query returned no rows, so order identity is vacuous")
			}
			for i := range gotOn {
				if gotOn[i] != gotOff[i] {
					t.Fatalf("ORDER DIVERGED at row %d of %d:\n  hash join  %q\n  nested loop %q\n"+
						"The hash join is no longer emitting the nested loop's sequence — see "+
						"hashJoinBuildOnLeft. If a build-side self-selection was just added, it "+
						"makes the substitution order-CHANGING: it must then be gated on the write "+
						"path being absent AND on a reinstated read-path order scan.",
						i, len(gotOn), gotOn[i], gotOff[i])
				}
			}
		})
	}
}

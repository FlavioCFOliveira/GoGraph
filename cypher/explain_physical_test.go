package cypher

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedPeople builds a graph of n :P nodes whose age cycles over buckets, which is
// the shape that makes an equi-join on age worth substituting a hash join for.
func seedPeople(t *testing.T, n, buckets int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	for i := 0; i < n; i++ {
		r, err := eng.RunInTx(context.Background(),
			fmt.Sprintf("CREATE (:P {age: %d, name: 'n%d'})", i%buckets, i), nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if cerr := r.Close(); cerr != nil {
			t.Fatalf("seed close %d: %v", i, cerr)
		}
	}
	return eng
}

// TestExplainPhysical_AgreesWithRuntimeCounters is the adversarial gate required
// by rmp #2222 AC 1 and AC 2: the rendering must be verified against the RUNTIME
// COUNTER that records what the planner actually did, never against another
// rendering.
//
// The distinction is the whole point. Before this change Engine.Explain rebuilt
// the planner's decisions a second time against the logical IR, and was wrong in
// both directions — it reported NodeByIndexSeek where a label scan ran, and
// printed CartesianProduct for an equi-join the planner substitutes a hash join
// for. A test that compared one rendering against another would have passed
// throughout. Each case below therefore executes the query, reads the counter the
// planner increments, and requires the rendering to agree with it.
func TestExplainPhysical_AgreesWithRuntimeCounters(t *testing.T) {
	t.Parallel()

	t.Run("hash join substitution is rendered as a join, never as a product", func(t *testing.T) {
		t.Parallel()
		eng := seedPeople(t, 400, 50)
		const q = "MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)"

		before := hashJoinBuildCount.Load()
		runToCompletion(t, eng, q)
		fired := hashJoinBuildCount.Load() - before
		if fired == 0 {
			t.Fatalf("the hash join did not fire, so this case no longer covers the "+
				"substitution it was written for (query: %s)", q)
		}

		plan := explainOK(t, eng, q)
		if !strings.Contains(plan, "HashJoin") {
			t.Errorf("hashJoinBuildCount fired %d time(s) but the plan names no HashJoin:\n%s", fired, plan)
		}
		// The specific inversion round 4 documented: O(n·m) shown where O(n+m) runs.
		if strings.Contains(plan, "CartesianProduct") {
			t.Errorf("the plan reports CartesianProduct while a HashJoin executed — the "+
				"reader is shown O(n*m) where O(n+m) runs:\n%s", plan)
		}
	})

	t.Run("the columnar tier is named when it is engaged", func(t *testing.T) {
		t.Parallel()
		eng := seedPeople(t, 400, 50)
		const q = "MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)"

		before := hashJoinColumnarBuildCount.Load()
		runToCompletion(t, eng, q)
		columnar := hashJoinColumnarBuildCount.Load() - before

		plan := explainOK(t, eng, q)
		named := strings.Contains(plan, "ColumnarHashJoin")
		if columnar > 0 && !named {
			t.Errorf("the columnar hash join was built %d time(s) but the plan does not name "+
				"it, so tier engagement is invisible:\n%s", columnar, plan)
		}
		if columnar == 0 && named {
			t.Errorf("the plan names ColumnarHashJoin but the columnar counter never "+
				"fired:\n%s", plan)
		}
	})

	t.Run("the access path distinguishes a label scan from an index seek", func(t *testing.T) {
		t.Parallel()
		eng := seedPeople(t, 200, 200)

		// No index: an equality predicate must be served by a scan.
		const q = "MATCH (n:P) WHERE n.name = 'n7' RETURN n"
		plan := explainOK(t, eng, q)
		if !strings.Contains(plan, "NodeByLabelScan") {
			t.Errorf("without an index the plan should scan the label:\n%s", plan)
		}
		if strings.Contains(plan, "NodeByIndexSeek") {
			t.Errorf("no index exists, yet the plan claims an index seek — this is round 3's "+
				"inversion (a seek reported where a scan runs):\n%s", plan)
		}

		// With an index the same query must seek, and the rendering must follow.
		mustRun(t, eng, "CREATE INDEX FOR (p:P) ON (p.name)")
		withIdx := explainOK(t, eng, q)
		if !strings.Contains(withIdx, "NodeByIndexSeek") {
			t.Errorf("an index on :P(name) exists but the plan does not seek it:\n%s", withIdx)
		}
	})

	t.Run("a rendered leaf reports what it was built against", func(t *testing.T) {
		t.Parallel()
		eng := seedPeople(t, 20, 20)
		plan := explainOK(t, eng, "MATCH (n:P) RETURN n")
		if !strings.Contains(plan, "NodeByLabelScan [P]") {
			t.Errorf("the label scan should state the label it iterates:\n%s", plan)
		}
	})
}

// TestExplainPhysical_WritingStatementSaysSo asserts a writing statement is
// labelled rather than silently rendered as a different kind of plan. A write's
// physical tree binds to an open transaction, so there is none to walk outside
// one, and claiming otherwise would be the same class of untruth this surface
// removes.
func TestExplainPhysical_WritingStatementSaysSo(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 5, 5)

	plan := explainOK(t, eng, "CREATE (:P {age: 1})")
	if !strings.Contains(plan, "logical plan") {
		t.Errorf("a writing statement's plan must declare that it is the logical one:\n%s", plan)
	}
}

// profileShapes are the plan shapes both profiling gates run over: a columnar
// hash join under a global aggregation, a bare columnar projection, a columnar
// filter, an aggregation over a scan, and a sort-with-limit. Between them they
// cover every composite lowering that builds an intermediate operator, which is
// what TestProfile_EveryOperatorIsMeasured needs and what
// TestProfile_PlanShapeIsIdenticalProfiledOrNot needs to see unchanged.
//
// Shared deliberately: the two properties — every node measured, and the shape
// unchanged by measuring it — must hold over the SAME shapes, or one gate could
// drift onto plans the other never checks.
var profileShapes = []string{
	"MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)",
	"MATCH (n:P) RETURN n.age",
	"MATCH (n:P) WHERE n.age > 10 RETURN n.name",
	"MATCH (n:P) RETURN count(n)",
	"MATCH (n:P) RETURN n.age ORDER BY n.age LIMIT 5",
}

// TestProfile_PlanShapeIsIdenticalProfiledOrNot is the transparency gate on the
// profiling wrapper.
//
// The builder wraps on the way OUT of its recursion, so a parent is constructed
// with its child already wrapped and runs its capability type-assertions against
// the wrapper. A wrapper that failed to preserve ChunkProducer or
// NodeIDColumnProducer would make the parent build a row-mode operator instead of
// a columnar one — profiling would silently change the plan it exists to observe,
// and every measurement would describe a plan the user never runs.
//
// Comparing the two renderings node for node is what catches that, and it is why
// the assertion is on the SHAPE rather than on the measurements.
func TestProfile_PlanShapeIsIdenticalProfiledOrNot(t *testing.T) {
	t.Parallel()

	for _, q := range profileShapes {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			eng := seedPeople(t, 300, 40)

			explained := stripAnnotations(explainOK(t, eng, q))
			profiled, err := eng.Profile(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("Profile(%q): %v", q, err)
			}
			if got := stripAnnotations(profiled); got != explained {
				t.Errorf("profiling changed the plan.\n--- Explain ---\n%s\n--- Profile ---\n%s\n"+
					"A wrapper that hides a capability makes the parent build a different "+
					"operator, so the measurements would describe a plan that never runs.",
					explained, got)
			}
		})
	}
}

// TestProfile_EveryOperatorIsMeasured is the completeness gate on a profile.
//
// buildOperator applies the profiler at ONE point, the value its recursion
// returns, which covers the whole tree exactly once — as long as one logical node
// becomes one physical operator. Where a lowering is COMPOSITE it does not: only
// the outermost operator passes through the wrap point, and the intermediates
// underneath rendered "(not measured)".
//
// Five did, on these same five shapes (rmp #2237): the EagerAggregation beneath a
// GlobalAggregateAdapter, both aggregation pre-projections, a ColumnarFilter
// beneath a ColumnarProject, and the final result projection — which is built
// above buildOperator's recursion entirely and so appeared unmeasured as the plan
// ROOT.
//
// An unmeasured node is worse than a missing one. It is rendered, so a reader
// counts it as part of the plan, and its cost lands in whichever ancestor WAS
// measured — inviting exactly the wrong conclusion about where time goes.
func TestProfile_EveryOperatorIsMeasured(t *testing.T) {
	t.Parallel()

	for _, q := range profileShapes {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			eng := seedPeople(t, 300, 40)

			out, err := eng.Profile(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("Profile(%q): %v", q, err)
			}
			if strings.Contains(out, "(not measured)") {
				t.Errorf("a profiled plan contains an unmeasured operator:\n%s\n"+
					"Every operator must report rows and time. A composite lowering that "+
					"builds an intermediate has to pass it through profileIntermediate — "+
					"buildOperator's single wrap point only reaches the value it returns.", out)
			}
			// The rendering must also be a plan, not an error string: a shape that
			// silently stopped producing a tree would pass the check above vacuously.
			if !strings.Contains(out, "rows=") || !strings.Contains(out, "time=") {
				t.Errorf("profile reports no measurements at all:\n%s", out)
			}
		})
	}
}

// TestProfile_ReportsRowsAndRefusesWrites covers the two contracts of the
// profiling surface: it measures a real execution, and it does not perform writes
// as a side effect of a diagnostic.
func TestProfile_ReportsRowsAndRefusesWrites(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 120, 12)

	out, err := eng.Profile(context.Background(), "MATCH (n:P) RETURN n.age", nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	// The scan really ran, so its measured row count is the seeded node count.
	if !strings.Contains(out, "rows=120") {
		t.Errorf("expected a measured operator reporting the 120 scanned rows:\n%s", out)
	}
	if !strings.Contains(out, "time=") {
		t.Errorf("a profiled plan must report per-operator time:\n%s", out)
	}

	if _, werr := eng.Profile(context.Background(), "CREATE (:P {age: 9})", nil); werr == nil {
		t.Error("Profile must refuse a writing statement rather than execute its writes")
	}
}

// --- helpers ---------------------------------------------------------------

func explainOK(t *testing.T, eng *Engine, q string) string {
	t.Helper()
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain(%q): %v", q, err)
	}
	if plan == "" {
		t.Fatalf("Explain(%q) returned an empty plan", q)
	}
	return plan
}

func runToCompletion(t *testing.T, eng *Engine, q string) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	for res.Next() {
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("Run(%q) drain: %v", q, rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("Run(%q) close: %v", q, cerr)
	}
}

// stripAnnotations removes the per-node measurements so two renderings can be
// compared on structure alone.
func stripAnnotations(plan string) string {
	lines := strings.Split(plan, "\n")
	for i, ln := range lines {
		if k := strings.Index(ln, " (rows="); k >= 0 {
			ln = ln[:k]
		}
		if k := strings.Index(ln, " (not measured)"); k >= 0 {
			ln = ln[:k]
		}
		lines[i] = ln
	}
	return strings.Join(lines, "\n")
}

// TestProfile_DbHitsDistinguishSeekFromScan is the acceptance gate for rmp #2238.
//
// Rows alone cannot tell a selective plan from a wasteful one: an indexed point
// lookup and a label scan followed by a filter can emit a comparable handful of
// rows while touching wildly different amounts of storage. Db-hits are the measure
// that separates them, and this asserts the separation on ONE graph with ONE
// indexed property, so the two plans differ in nothing but their access path.
func TestProfile_DbHitsDistinguishSeekFromScan(t *testing.T) {
	t.Parallel()
	const n = 300
	eng := seedPeople(t, n, 40)
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_name FOR (x:P) ON (x.name)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	// The seek: `name` is indexed, so this is a point lookup.
	seek, err := eng.Profile(context.Background(), "MATCH (n:P) WHERE n.name = 'n7' RETURN n.age", nil)
	if err != nil {
		t.Fatalf("Profile(seek): %v", err)
	}
	// The scan: `age` has no index, so the same predicate shape becomes a label
	// scan with a filter above it.
	scan, err := eng.Profile(context.Background(), "MATCH (n:P) WHERE n.age = 7 RETURN n.age", nil)
	if err != nil {
		t.Fatalf("Profile(scan): %v", err)
	}

	seekHits := totalDbHits(t, seek)
	scanHits := totalDbHits(t, scan)

	if seekHits == 0 {
		t.Fatalf("the seek plan reports no db-hits at all, so the comparison below is "+
			"vacuous — a storage-reading operator must report them:\n%s", seek)
	}
	if scanHits < int64(n) {
		t.Errorf("the scan plan reports %d db-hits over %d nodes; a label scan reads one "+
			"record per node, so it must report at least %d:\n%s", scanHits, n, n, scan)
	}
	if seekHits >= scanHits {
		t.Errorf("db-hits do not distinguish the access paths: seek=%d, scan=%d. That is the "+
			"whole point of the measure — both plans return a similar row count, and only "+
			"db-hits show that one read the entire label.\n--- seek ---\n%s\n--- scan ---\n%s",
			seekHits, scanHits, seek, scan)
	}

	// A pure row transformer touches no storage, so its own figure must be zero
	// rather than inheriting its child's.
	if !strings.Contains(seek, "dbhits=0") {
		t.Errorf("no operator reports zero db-hits; a projection reads no storage and must "+
			"not be charged for its child's reads:\n%s", seek)
	}
}

// totalDbHits sums the dbhits= figures in a rendered profile. It parses the
// rendering rather than reading the tree, so it also proves the numbers are
// actually SURFACED to a user and not merely computed.
func totalDbHits(t *testing.T, profile string) int64 {
	t.Helper()
	var total int64
	found := false
	for _, line := range strings.Split(profile, "\n") {
		i := strings.Index(line, "dbhits=")
		if i < 0 {
			continue
		}
		rest := line[i+len("dbhits="):]
		end := strings.IndexAny(rest, ",)")
		if end < 0 {
			t.Fatalf("malformed dbhits field in %q", line)
		}
		v, err := strconv.ParseInt(rest[:end], 10, 64)
		if err != nil {
			t.Fatalf("unparsable dbhits in %q: %v", line, err)
		}
		total += v
		found = true
	}
	if !found {
		t.Fatalf("a profiled plan reports no db-hits at all:\n%s", profile)
	}
	return total
}

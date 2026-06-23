package cypher

// parallel_scan_project_diff_test.go — differential, determinism, and guard
// tests for the morsel-parallel fused scan→filter→project fast path (#1682).
//
// The differential tests run each representative scan/filter/projection query
// with the parallel fused path ENABLED (low threshold so it engages on a small
// test graph) and DISABLED and assert an IDENTICAL result MULTISET (compared
// sorted, because a full scan is unordered and the parallel path concatenates
// per-worker slices). A diagnostic build counter confirms the parallel path was
// actually engaged for the enabled run, so the test cannot silently pass by never
// triggering. Guard assertions confirm the fused path declines for shapes outside
// its locked scope (aggregation, DISTINCT, ORDER BY between projection and scan,
// label scan, Expand, Unwind) and falls back to the serial pipeline.
//
// These reuse the helpers (buildPSTestGraph, drainSortedPS, engines,
// assertEqualRows, psTestThreshold) defined in parallel_scan_diff_test.go.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestParallelScanProject_Differential proves the fused scan→filter→project path
// returns a result multiset identical to the serial path, and that it engaged.
func TestParallelScanProject_Differential(t *testing.T) {
	// 200 nodes > psTestThreshold (50) so the fused path engages.
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	cases := []struct {
		name string
		q    string
	}{
		{"return-prop", `MATCH (n) RETURN n.v AS v`},
		{"return-node", `MATCH (n) RETURN n AS node`},
		{"return-id", `MATCH (n) RETURN id(n) AS i`},
		{"return-labels", `MATCH (n) RETURN labels(n) AS ls`},
		{"return-arith", `MATCH (n) RETURN n.v * 2 + 1 AS x`},
		{"filter-eq", `MATCH (n) WHERE n.g = 1 RETURN n.v AS v`},
		{"filter-range", `MATCH (n) WHERE n.v > 100 RETURN n.v AS v`},
		{"filter-and", `MATCH (n) WHERE n.v >= 50 AND n.v < 150 RETURN n.v AS v`},
		{"filter-null-drop", `MATCH (n) WHERE n.missing > 0 RETURN n.v AS v`}, // NULL drops every row
		{"filter-multi-col", `MATCH (n) WHERE n.g = 2 RETURN n.v AS v, n.g AS grp`},
		{"case-expr", `MATCH (n) RETURN CASE WHEN n.g = 0 THEN 'a' ELSE 'b' END AS c`},
		{"filter-return-node", `MATCH (n) WHERE n.v < 30 RETURN n AS node`},
		// Argument-bearing temporal constructor is PURE (deterministic): it must
		// still fuse, unlike its zero-argument clock form (see DeclinesOutsideScope).
		{"temporal-arg-pure", `MATCH (n) RETURN date('2020-01-01') AS d`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := parallelScanProjectBuildCount.Load()
			gotOn := drainSortedPS(t, on, tc.q)
			triggered := parallelScanProjectBuildCount.Load() > before
			gotOff := drainSortedPS(t, off, tc.q)
			assertEqualRows(t, tc.q, gotOn, gotOff)
			if !triggered {
				t.Fatalf("expected fused parallel scan to engage for %q, but it did not", tc.q)
			}
		})
	}
}

// TestParallelScanProject_SortAbove proves that an ORDER BY above the fused
// projection (a downstream Sort pipeline-breaker) still orders the full multiset
// correctly: the parallel path concatenates per-worker slices unordered, and the
// Sort above consumes them and produces an ordered result identical to serial.
// The comparison is on the RAW (unsorted-by-the-test) order so it actually checks
// the ORDER BY took effect.
func TestParallelScanProject_SortAbove(t *testing.T) {
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	const q = `MATCH (n) WHERE n.v >= 100 RETURN n.v AS v ORDER BY n.v DESC`
	gotOn := drainOrderedPS(t, on, q)
	gotOff := drainOrderedPS(t, off, q)
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch: parallel=%d serial=%d", len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("ordered row %d differs:\n  parallel = %s\n  serial   = %s", i, gotOn[i], gotOff[i])
		}
	}
	// Spot-check the order is actually descending: the highest n.v (199) sorts
	// first. The projected row may carry extra scope columns (ORDER BY n.v keeps
	// n in scope), so match the v column by prefix rather than exact row text.
	if len(gotOn) == 0 || !strings.HasPrefix(gotOn[0], "v=199") {
		first := ""
		if len(gotOn) > 0 {
			first = gotOn[0]
		}
		t.Fatalf("ORDER BY n.v DESC first row = %q, want it to start with v=199", first)
	}
}

// TestParallelScanProject_LimitAbove proves an unordered LIMIT above the fused
// projection returns the SAME NUMBER of rows on both paths and that every row it
// returns is a member of the full result multiset. Per openCypher, an unordered
// LIMIT is non-deterministic, so the test asserts cardinality and membership —
// NEVER row identity.
func TestParallelScanProject_LimitAbove(t *testing.T) {
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	const q = `MATCH (n) WHERE n.g = 1 RETURN n.v AS v LIMIT 5`
	gotOn := drainOrderedPS(t, on, q)
	gotOff := drainOrderedPS(t, off, q)
	if len(gotOn) != len(gotOff) {
		t.Fatalf("LIMIT cardinality mismatch: parallel=%d serial=%d", len(gotOn), len(gotOff))
	}
	if len(gotOn) != 5 {
		t.Fatalf("LIMIT 5 returned %d rows, want 5", len(gotOn))
	}
	// Every returned row must be in the full (unlimited) result multiset.
	full := map[string]struct{}{}
	for _, r := range drainSortedPS(t, off, `MATCH (n) WHERE n.g = 1 RETURN n.v AS v`) {
		full[r] = struct{}{}
	}
	for _, r := range gotOn {
		if _, ok := full[r]; !ok {
			t.Fatalf("LIMIT returned row %q not present in the full result multiset", r)
		}
	}
}

// TestParallelScanProject_DistinctAbove proves a DISTINCT above the fused
// projection (a downstream Distinct pipeline-breaker) safely fuses the scan leaf:
// the parallel path concatenates per-worker slices and the Distinct above dedups
// the full multiset, yielding a result identical to serial. Distinct ABOVE the
// fused leaf is the sanctioned shape (only a DISTINCT between projection and scan,
// or inside the projection, would disqualify).
func TestParallelScanProject_DistinctAbove(t *testing.T) {
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	const q = `MATCH (n) RETURN DISTINCT n.g AS g`
	before := parallelScanProjectBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	triggered := parallelScanProjectBuildCount.Load() > before
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if !triggered {
		t.Fatalf("expected fused parallel scan to engage beneath DISTINCT for %q", q)
	}
	// g ∈ {0,1,2} → exactly three distinct rows.
	if len(gotOn) != 3 {
		t.Fatalf("DISTINCT n.g returned %d rows, want 3: %v", len(gotOn), gotOn)
	}
}

// TestParallelScanProject_DeclinesOutsideScope proves the fused path does NOT
// engage for shapes outside its locked scope, while results stay correct against
// the serial path.
func TestParallelScanProject_DeclinesOutsideScope(t *testing.T) {
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	// Deterministic decline cases: the fused path must NOT engage AND the result
	// must match serial.
	for _, q := range []string{
		`MATCH (n) RETURN count(*) AS c`,                       // global aggregate (count reduce owns this)
		`MATCH (n) RETURN n.g AS g, count(*) AS c`,             // grouped aggregate
		`MATCH (n:Item) RETURN n.v AS v`,                       // NodeByLabelScan, not AllNodesScan
		`MATCH (n) UNWIND [1,2] AS x RETURN n.v AS v, x`,       // Unwind between
		`MATCH (n) RETURN sum(n.v) AS s`,                       // aggregate item
		`MATCH (n) RETURN size([x IN [1,2,3] WHERE x>1]) AS s`, // list comprehension item
	} {
		before := parallelScanProjectBuildCount.Load()
		gotOn := drainSortedPS(t, on, q)
		if parallelScanProjectBuildCount.Load() != before {
			t.Errorf("fused parallel scan unexpectedly engaged for %q", q)
		}
		gotOff := drainSortedPS(t, off, q)
		assertEqualRows(t, q, gotOn, gotOff)
	}

	// Non-deterministic / clock-dependent items: the fused path must DECLINE
	// (engaging would let per-worker evaluation change the projected VALUE
	// multiset, not just its order — cypher-expert sign-off). Their results are not
	// value-comparable across runs, so only the decline (build-count unchanged) is
	// asserted; the query is still drained to confirm it executes on both paths.
	for _, q := range []string{
		`MATCH (n) RETURN rand() AS r`,             // rand: non-deterministic per call
		`MATCH (n) RETURN n.v AS v, rand() AS r`,   // non-deterministic mixed with a pure item
		`MATCH (n) RETURN datetime() AS d`,         // zero-arg clock constructor
		`MATCH (n) RETURN date() AS d`,             // zero-arg clock constructor
		`MATCH (n) RETURN localdatetime() AS d`,    // zero-arg clock constructor
		`MATCH (n) RETURN n.v AS v, time() AS tod`, // zero-arg clock constructor mixed with pure
	} {
		before := parallelScanProjectBuildCount.Load()
		_ = drainSortedPS(t, on, q)
		if parallelScanProjectBuildCount.Load() != before {
			t.Errorf("fused parallel scan unexpectedly engaged for non-deterministic %q", q)
		}
		_ = drainSortedPS(t, off, q) // drains without error on the serial path
	}
}

// TestParallelScanProject_SmallGraphStaysSerial proves the threshold gate keeps a
// small graph on the serial path, so small-query latency is unaffected.
func TestParallelScanProject_SmallGraphStaysSerial(t *testing.T) {
	g := buildPSTestGraph(t, psTestThreshold) // exactly at threshold → strict > fails
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})

	for _, q := range []string{
		`MATCH (n) RETURN n.v AS v`,
		`MATCH (n) WHERE n.g = 1 RETURN n.v AS v`,
	} {
		before := parallelScanProjectBuildCount.Load()
		_ = drainSortedPS(t, on, q)
		if parallelScanProjectBuildCount.Load() != before {
			t.Errorf("fused parallel scan engaged at-threshold for %q (should stay serial)", q)
		}
	}
}

// TestParallelScanProject_EmptyGraph proves a fused scan over an empty graph
// yields zero rows on both paths (the fused path declines: live count 0 is not >
// threshold).
func TestParallelScanProject_EmptyGraph(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: 0})
	got := drainSortedPS(t, on, `MATCH (n) RETURN n.v AS v`)
	if len(got) != 0 {
		t.Fatalf("scan over empty graph = %v, want []", got)
	}
}

// TestParallelScanProject_DeterministicResult runs the fused path many times and
// asserts the result multiset never varies despite worker interleaving.
func TestParallelScanProject_DeterministicResult(t *testing.T) {
	g := buildPSTestGraph(t, 300)
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
	off := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})

	const q = `MATCH (n) WHERE n.v >= 50 RETURN n.v AS v`
	want := drainSortedPS(t, off, q)
	for run := range 25 {
		got := drainSortedPS(t, on, q)
		assertEqualRows(t, fmt.Sprintf("%s (run %d)", q, run), got, want)
	}
}

// TestParallelScanProject_RaceLazyPredicate drives the fused path over a graph
// large enough to spread across several morsels and workers, with a WHERE
// predicate that exercises the lazy node-materialisation path (n.k scalar access)
// and a projection that reads a property too. Run under `go test -race` it pins
// the per-worker buildOpts isolation (#1682 must-fix #1): without a per-worker
// nodeResolver, two workers writing bopts.nodeResolver concurrently would trip
// the detector. The result is validated against the serial path.
func TestParallelScanProject_RaceLazyPredicate(t *testing.T) {
	const n = 60_000
	g := buildPSTestGraph(t, n)
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
	off := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})

	const q = `MATCH (n) WHERE n.v >= 30000 RETURN n.v AS v`
	before := parallelScanProjectBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	if parallelScanProjectBuildCount.Load() == before {
		t.Fatalf("expected fused parallel scan to engage for the large-graph race query")
	}
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if len(gotOn) != n-30000 {
		t.Fatalf("filtered count = %d, want %d", len(gotOn), n-30000)
	}
}

// drainOrderedPS runs q and returns rows in result order (NOT sorted by the test),
// rendered as canonical strings — used to verify a downstream ORDER BY / LIMIT.
func drainOrderedPS(t *testing.T, e *Engine, q string) []string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		var sb []byte
		for i, c := range cols {
			if i > 0 {
				sb = append(sb, '|')
			}
			sb = append(sb, fmt.Sprintf("%s=%v", c, rec[c])...)
		}
		out = append(out, string(sb))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	return out
}

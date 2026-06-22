package cypher

// parallel_scan_diff_test.go — differential, determinism, and guard tests for
// the morsel-parallel-reduce count fast path (#1672).
//
// The differential test runs each representative count query with the parallel
// count path ENABLED (low threshold so it engages on a small test graph) and
// DISABLED and asserts an IDENTICAL result. The count reduce emits one scalar
// row, which must match exactly. A diagnostic build counter confirms the
// parallel path was actually engaged for the enabled run, so the test cannot
// silently pass by never triggering. Guard assertions confirm the parallel count
// path declines for shapes outside its locked scope.
//
// The full-node scan itself is NOT parallelised in the planner — the
// morsel-parallel full-scan funnel was benchmarked as a regression and only the
// count reduce shipped — so there is no full-scan differential here.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// psTestThreshold engages the parallel count reduce on a small test graph: any
// live node count above it takes the parallel path.
const psTestThreshold = 50

// buildPSTestGraph creates a graph of n :Item nodes, each carrying a distinct
// integer "v" property and a "g" group property in {0,1,2}.
func buildPSTestGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range n {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "Item"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(int64(i%3))); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// drainSortedPS runs q and returns every row rendered as a canonical string,
// sorted so two result sets that differ only in order compare equal.
func drainSortedPS(t *testing.T, e *Engine, q string) []string {
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
	sort.Strings(out)
	return out
}

// engines returns a parallel-ENABLED engine (low threshold) and a
// parallel-DISABLED engine over the same graph.
func engines(g *lpg.Graph[string, float64]) (on, off *Engine) {
	on = NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
	off = NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})
	return on, off
}

// TestParallelScan_Differential proves the parallel count reduce returns results
// identical to the serial path, and that it actually engaged.
func TestParallelScan_Differential(t *testing.T) {
	// 200 nodes > psTestThreshold (50), so the parallel count path engages.
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	t.Run("count-star", func(t *testing.T) {
		const q = `MATCH (n) RETURN count(*) AS c`
		before := parallelCountScanBuildCount.Load()
		gotOn := drainSortedPS(t, on, q)
		triggered := parallelCountScanBuildCount.Load() > before
		gotOff := drainSortedPS(t, off, q)
		assertEqualRows(t, q, gotOn, gotOff)
		if !triggered {
			t.Fatalf("expected parallel count scan to engage for %q, but it did not", q)
		}
		if len(gotOn) != 1 || gotOn[0] != "c=200" {
			t.Fatalf("count(*) = %v, want [c=200]", gotOn)
		}
	})

	t.Run("count-var", func(t *testing.T) {
		const q = `MATCH (n) RETURN count(n) AS c`
		before := parallelCountScanBuildCount.Load()
		gotOn := drainSortedPS(t, on, q)
		triggered := parallelCountScanBuildCount.Load() > before
		gotOff := drainSortedPS(t, off, q)
		assertEqualRows(t, q, gotOn, gotOff)
		if !triggered {
			t.Fatalf("expected parallel count scan to engage for %q, but it did not", q)
		}
		if len(gotOn) != 1 || gotOn[0] != "c=200" {
			t.Fatalf("count(n) = %v, want [c=200]", gotOn)
		}
	})

	t.Run("count-default-alias", func(t *testing.T) {
		const q = `MATCH (n) RETURN count(*)`
		gotOn := drainSortedPS(t, on, q)
		gotOff := drainSortedPS(t, off, q)
		assertEqualRows(t, q, gotOn, gotOff)
	})
}

// TestParallelScan_DeclinesOutsideScope proves the parallel count reduce does
// NOT engage for shapes outside its locked scope (sum/avg, grouped count, a
// label scan, count of a property), while results stay correct.
func TestParallelScan_DeclinesOutsideScope(t *testing.T) {
	g := buildPSTestGraph(t, 200)
	on, off := engines(g)

	for _, q := range []string{
		`MATCH (n) RETURN sum(n.v) AS s`,            // sum: non-associative, declines
		`MATCH (n) RETURN avg(n.v) AS a`,            // avg: declines
		`MATCH (n) RETURN min(n.v) AS m`,            // min: out of scope, declines
		`MATCH (n) RETURN n.g AS g, count(*) AS c`,  // grouped: declines
		`MATCH (n) RETURN count(DISTINCT n.g) AS c`, // distinct: declines
		`MATCH (n) RETURN count(n.v) AS c`,          // count(prop): declines
		`MATCH (n:Item) RETURN count(*) AS c`,       // NodeByLabelScan child: count reduce declines
	} {
		before := parallelCountScanBuildCount.Load()
		gotOn := drainSortedPS(t, on, q)
		if parallelCountScanBuildCount.Load() != before {
			t.Errorf("parallel count reduce unexpectedly engaged for %q", q)
		}
		gotOff := drainSortedPS(t, off, q)
		assertEqualRows(t, q, gotOn, gotOff)
	}
}

// TestParallelScan_SmallGraphStaysSerial proves the threshold gate keeps a small
// graph on the serial path, so small-query latency is unaffected.
func TestParallelScan_SmallGraphStaysSerial(t *testing.T) {
	g := buildPSTestGraph(t, psTestThreshold) // exactly at threshold → strict > fails
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})

	for _, q := range []string{
		`MATCH (n) RETURN count(*) AS c`,
		`MATCH (n) RETURN count(n) AS c`,
	} {
		bCount := parallelCountScanBuildCount.Load()
		_ = drainSortedPS(t, on, q)
		if parallelCountScanBuildCount.Load() != bCount {
			t.Errorf("parallel count reduce engaged at-threshold for %q (should stay serial)", q)
		}
	}
}

// TestParallelScan_EmptyGraph proves count over an empty graph yields 0 on both
// paths and the parallel count reduce declines (live count 0 is not > threshold).
func TestParallelScan_EmptyGraph(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: 0}) // default threshold
	got := drainSortedPS(t, on, `MATCH (n) RETURN count(*) AS c`)
	if len(got) != 1 || got[0] != "c=0" {
		t.Fatalf("count over empty graph = %v, want [c=0]", got)
	}
}

// TestParallelScan_NegativeThresholdAlwaysParallel proves a negative threshold
// (clamped to 0) takes the parallel path for any non-empty graph, and results
// still match serial.
func TestParallelScan_NegativeThresholdAlwaysParallel(t *testing.T) {
	g := buildPSTestGraph(t, 10) // tiny graph
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: -1})
	off := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})

	const q = `MATCH (n) RETURN count(*) AS c`
	before := parallelCountScanBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	if parallelCountScanBuildCount.Load() == before {
		t.Fatalf("expected parallel count reduce to engage with negative (clamped-to-0) threshold")
	}
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if gotOn[0] != "c=10" {
		t.Fatalf("count = %v, want [c=10]", gotOn)
	}
}

// TestParallelScan_DeterministicResult runs the parallel count reduce many times
// and asserts the result never varies despite worker interleaving.
func TestParallelScan_DeterministicResult(t *testing.T) {
	g := buildPSTestGraph(t, 300)
	on := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})

	const countQ = `MATCH (n) RETURN count(*) AS c`
	for run := range 25 {
		c := drainSortedPS(t, on, countQ)
		if len(c) != 1 || c[0] != "c=300" {
			t.Fatalf("run %d: count = %v, want [c=300]", run, c)
		}
	}
}

func assertEqualRows(t *testing.T, q string, gotOn, gotOff []string) {
	t.Helper()
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch for %q: parallel=%d serial=%d", q, len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("row %d differs for %q:\n  parallel = %s\n  serial   = %s", i, q, gotOn[i], gotOff[i])
		}
	}
}

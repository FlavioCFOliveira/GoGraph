package cypher

// parallel_aggregate_null_diff_test.go — NULL-input differential coverage for the
// morsel-parallel aggregate scan (#2111). The existing differential tests
// (parallel_aggregate_diff_test.go) feed a non-NULL "v" on every node, so they
// never exercise the empty-extremum path where a worker partial holds no non-NULL
// value (val == nil ⇒ "no non-NULL seen"). openCypher min/max/count(v) skip NULL:
// an ALL-NULL group or an ALL-NULL global scan must yield min = max = NULL and
// count(v) = 0, while count(*) still counts the rows.
//
// These are the cases where a naive parallel combine could diverge from serial:
// merging two "no value seen" partials, or merging a "no value seen" partial with
// one that has a value, must reproduce EXACTLY what the serial funcs.MinAgg /
// MaxAgg / CountAgg produce. Each query is run parallel-ENABLED and
// parallel-DISABLED and asserted byte-identical, with the build counter proving
// the parallel path engaged.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// addNullVNode inserts an :Item node named k in group grp that carries NO "v"
// property, so n.v reads as NULL. It fixes the group so grouped aggregates can be
// exercised over rows whose aggregated value is entirely absent.
func addNullVNode(t *testing.T, g *lpg.Graph[string, float64], k string, grp int64) {
	t.Helper()
	if err := g.AddNode(k); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel(k, "Item"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty(k, "g", lpg.Int64Value(grp)); err != nil {
		t.Fatal(err)
	}
}

// findRow returns the first sorted row whose canonical string contains needle, or
// "" when none does. Rows render as "col=val|col=val|…" in RETURN-clause order.
func findRow(rows []string, needle string) string {
	for _, r := range rows {
		if strings.Contains(r, needle) {
			return r
		}
	}
	return ""
}

// TestParallelAggregate_AllNullScan_Differential proves min/max/count over a graph
// where NO node carries "v" (every value is NULL) is byte-identical to serial and
// yields min = max = NULL, count(v) = 0, count(*) = N. This is the empty-extremum
// path: every per-worker min/max partial holds val == nil, so the combine must
// merge "no value seen" partials without inventing a value.
func TestParallelAggregate_AllNullScan_Differential(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	const n = 120 // > psTestThreshold (50) so the parallel aggregate path engages
	for i := 0; i < n; i++ {
		addNullVNode(t, g, strconv.Itoa(i), int64(i%3))
	}
	on, off := engines(g)

	// Global (group-by-less) min/max/count over an all-NULL column.
	global := `MATCH (n) RETURN min(n.v) AS lo, max(n.v) AS hi, count(n.v) AS c, count(*) AS total`
	got := assertAggParallelDiff(t, on, off, global)
	if len(got) != 1 {
		t.Fatalf("all-NULL global scan: want 1 row, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "c=0") || !strings.Contains(got[0], "total=120") {
		t.Fatalf("all-NULL global scan row = %q, want count(v)=0 and count(*)=120", got[0])
	}

	// Grouped min/max/count over an all-NULL column: every group present, each with
	// min = max = NULL and count(v) = 0 but count(*) = its membership.
	grouped := `MATCH (n) RETURN n.g AS grp, min(n.v) AS lo, max(n.v) AS hi, count(n.v) AS c, count(*) AS total`
	gr := assertAggParallelDiff(t, on, off, grouped)
	if len(gr) != 3 {
		t.Fatalf("all-NULL grouped scan: want 3 groups, got %d: %v", len(gr), gr)
	}
	for _, row := range gr {
		if !strings.Contains(row, "c=0") {
			t.Fatalf("all-NULL grouped row = %q, want count(v)=0", row)
		}
	}
}

// TestParallelAggregate_MixedNullGroups_Differential proves that groups mixing
// present and absent "v" values are byte-identical to serial. Group 0 is entirely
// NULL (empty extremum), group 1 mixes NULL and Int64 (the extremum must skip the
// NULLs), and group 2 is entirely non-NULL. This exercises the combine merging a
// "no value seen" partial with a value-bearing one and vice versa.
func TestParallelAggregate_MixedNullGroups_Differential(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	// Interleave so the deterministic scan order mixes groups and, within group 1,
	// mixes NULL and non-NULL rows.
	idx := 0
	next := func() string { s := strconv.Itoa(idx); idx++; return s }
	for i := 0; i < 60; i++ {
		addNullVNode(t, g, next(), 0) // group 0: all NULL
		if i%2 == 0 {                 // group 1: half NULL…
			addNullVNode(t, g, next(), 1)
		} else { // …half a real value spanning a wide range
			addAggNode(t, g, next(), lpg.Int64Value(int64(1000-i)), 1)
		}
		addAggNode(t, g, next(), lpg.Int64Value(int64(i)), 2) // group 2: all non-NULL
	}
	on, off := engines(g)

	// Grouped: group 0 → NULL extrema / count(v)=0; group 1 → extrema of its
	// non-NULL members only; group 2 → full extrema.
	grouped := `MATCH (n) RETURN n.g AS grp, min(n.v) AS lo, max(n.v) AS hi, count(n.v) AS c, count(*) AS total`
	gr := assertAggParallelDiff(t, on, off, grouped)
	if len(gr) != 3 {
		t.Fatalf("mixed-NULL grouped scan: want 3 groups, got %d: %v", len(gr), gr)
	}
	// Group 0 (all NULL) must skip every value: count(v)=0.
	if row := findRow(gr, "grp=0"); row == "" || !strings.Contains(row, "c=0") {
		t.Fatalf("mixed-NULL group 0 row = %q, want count(v)=0", row)
	}

	// Global min/max over the whole mix skips NULLs and spans groups 1 and 2.
	global := `MATCH (n) RETURN min(n.v) AS lo, max(n.v) AS hi, count(n.v) AS c`
	assertAggParallelDiff(t, on, off, global)
}

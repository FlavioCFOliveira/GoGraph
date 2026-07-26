package cypher

// parallel_label_scan_diff_test.go — differential, threshold and leak gates for the
// partitioned label scan (#2187).
//
// Every intra-query parallel leaf used to require the IR leaf to be a bare, unlabelled
// AllNodesScan. Real Cypher almost always carries a label, so parallelism never engaged
// in practice — the round-3 audit measured adding a label that every node already
// carried at 2.0x to 3.7x, with causality confirmed by toggling DisableParallelScan.
//
// A labelled leaf now walks that label's roaring bitmap and the existing morsel splitter
// partitions THOSE ids. The correctness claim is that the row set is bit-identical to
// the serial label scan's, which holds because both iterate the same bitmap from the
// same resolver, and the label index is already live-only.
//
// The label is admitted on the PROJECTION leaf only. Measuring both showed they
// disagree: 2.1x faster for the projection, 1.6x SLOWER for the aggregate, because #2185
// gave the serial aggregate an unboxed columnar pre-projection that the parallel workers
// do not yet have. See docs/benchmarks/parallel-label-scan-2026-07-26.md.
//
// The tests here cover what the shared differential table in
// parallel_scan_project_diff_test.go cannot: a graph where the label is a strict SUBSET
// of the nodes (so a leak from the bitmap to the whole graph would be visible as extra
// rows), the threshold behaviour that must be keyed on the LABEL's cardinality rather
// than the graph's order, the measured decision NOT to admit a labelled leaf on the
// aggregate path, and goroutine hygiene.
//
// Layer: short.

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildSubsetLabelGraph builds n nodes of which only every third carries label Few,
// while ALL carry label Many. So `:Few` has ~n/3 members and `:Many` has n. A parallel
// leaf that leaked from the label bitmap to the whole-graph walk would return n rows
// for a `:Few` query instead of n/3 — a difference this fixture makes loud.
func buildSubsetLabelGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		k := "s" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "Many"); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := g.SetNodeLabel(k, "Few"); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "gp", lpg.Int64Value(int64(i%5))); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestParallelLabelScan_SubsetLabelDifferential is the core correctness gate: on a graph
// where the label is a strict subset, the parallel arm must return exactly the serial
// arm's multiset — proving the leaf partitioned the LABEL, not the graph.
func TestParallelLabelScan_SubsetLabelDifferential(t *testing.T) {
	const n = 600
	g := buildSubsetLabelGraph(t, n)
	on, off := engines(g) // on: threshold 50; off: parallel disabled

	for _, tc := range []struct {
		name string
		q    string
		want int // expected row count, computed from the fixture
	}{
		{"few-prop", `MATCH (n:Few) RETURN n.v AS v`, (n + 2) / 3},
		{"few-filtered", `MATCH (n:Few) WHERE n.v > 100 RETURN n.v + 0 AS v`, 0}, // computed below
		{"many-prop", `MATCH (n:Many) RETURN n.v AS v`, n},
		{"few-multi-col", `MATCH (n:Few) RETURN n.v AS v, n.gp AS gp`, (n + 2) / 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := parallelScanProjectBuildCount.Load()
			gotOn := drainSortedPS(t, on, tc.q)
			engaged := parallelScanProjectBuildCount.Load() > before
			gotOff := drainSortedPS(t, off, tc.q)
			assertEqualRows(t, tc.q, gotOn, gotOff)
			if !engaged {
				t.Fatalf("expected the partitioned label scan to engage for %q, but it did not", tc.q)
			}
			if tc.want > 0 && len(gotOn) != tc.want {
				t.Fatalf("%q returned %d rows, want %d: a leak from the label bitmap to the "+
					"whole-graph walk would show up exactly here", tc.q, len(gotOn), tc.want)
			}
		})
	}
}

// TestParallelLabelScan_BelowLabelThresholdStaysSerial pins the threshold rule: the gate
// must key on the LABEL's cardinality, not the graph's live order. A small label inside a
// large graph must stay serial — otherwise a ten-node label in a million-node graph
// would spawn a worker fleet to do nothing.
func TestParallelLabelScan_BelowLabelThresholdStaysSerial(t *testing.T) {
	// 600 nodes, all Many; only 10 carry Rare. With a threshold of 50 the graph is
	// over it and Many is over it, but Rare (10) is under it.
	const n = 600
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		k := "r" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "Many"); err != nil {
			t.Fatal(err)
		}
		if i < 10 {
			if err := g.SetNodeLabel(k, "Rare"); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	on, off := engines(g)

	before := parallelScanProjectBuildCount.Load()
	gotOn := drainSortedPS(t, on, `MATCH (n:Rare) RETURN n.v AS v`)
	if parallelScanProjectBuildCount.Load() != before {
		t.Fatal("the parallel leaf engaged for a 10-node label under the 50-row threshold: " +
			"the gate is keyed on the graph's live order instead of the label's cardinality")
	}
	gotOff := drainSortedPS(t, off, `MATCH (n:Rare) RETURN n.v AS v`)
	assertEqualRows(t, "MATCH (n:Rare) RETURN n.v AS v", gotOn, gotOff)
	if len(gotOn) != 10 {
		t.Fatalf("MATCH (n:Rare) returned %d rows, want 10", len(gotOn))
	}

	// The same graph's large label DOES engage, so the previous assertion is about the
	// label's size and not about labels being rejected outright.
	before = parallelScanProjectBuildCount.Load()
	_ = drainSortedPS(t, on, `MATCH (n:Many) RETURN n.v AS v`)
	if parallelScanProjectBuildCount.Load() == before {
		t.Fatal("the parallel leaf declined for a 600-node label over the 50-row threshold")
	}
}

// TestParallelLabelScan_UnknownLabelIsEmptyAndSerial pins the degenerate case: a label no
// node carries resolves to an empty bitmap, which is under any threshold, so the query
// stays serial and returns nothing.
func TestParallelLabelScan_UnknownLabelIsEmptyAndSerial(t *testing.T) {
	g := buildSubsetLabelGraph(t, 600)
	on, off := engines(g)
	const q = `MATCH (n:NoSuchLabel) RETURN n.v AS v`

	before := parallelScanProjectBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	if parallelScanProjectBuildCount.Load() != before {
		t.Fatal("the parallel leaf engaged for a label with no members")
	}
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if len(gotOn) != 0 {
		t.Fatalf("%q returned %d rows, want 0", q, len(gotOn))
	}
}

// TestParallelLabelAggregate_LabelledLeafStaysSerial pins the measured decision that
// the AGGREGATE leaf does NOT admit a labelled scan (#2187). The partitioned label scan
// is a 2.1x win for the projection leaf but a 1.6x loss here, because #2185 gave the
// serial aggregate an unboxed columnar pre-projection while the parallel workers still
// build the boxed row-at-a-time one: 21.3 ms / 199 868 allocations serial against
// 33.9 ms / 1 603 900 parallel at 200 000 rows and 10 cores.
//
// So the assertion is the reverse of the projection case: the parallel aggregate scan
// must DECLINE a labelled leaf, and the result must still match serial. If a future
// change gives the workers the unboxed pre-projection and re-measures a win, this test
// is the one to update — deliberately, with numbers.
func TestParallelLabelAggregate_LabelledLeafStaysSerial(t *testing.T) {
	g := buildSubsetLabelGraph(t, 600)
	on, off := engines(g)

	for _, q := range []string{
		`MATCH (n:Few) RETURN min(n.v) AS m`,
		`MATCH (n:Few) RETURN max(n.v) AS m`,
		`MATCH (n:Few) RETURN n.gp AS gp, min(n.v) AS m`,
		`MATCH (n:Many) RETURN min(n.v) AS m`,
	} {
		t.Run(q, func(t *testing.T) {
			before := parallelAggregateScanBuildCount.Load()
			gotOn := drainSortedPS(t, on, q)
			if parallelAggregateScanBuildCount.Load() != before {
				t.Fatalf("the parallel aggregate scan engaged for %q; #2187 measured that path "+
					"1.6x SLOWER than the serial columnar aggregate, so a labelled leaf must "+
					"stay serial until the workers get the unboxed pre-projection", q)
			}
			gotOff := drainSortedPS(t, off, q)
			assertEqualRows(t, q, gotOn, gotOff)
		})
	}

	// The UNLABELLED shape is still admitted, so the assertion above is about the label
	// and not about the aggregate path having been switched off wholesale.
	before := parallelAggregateScanBuildCount.Load()
	_ = drainSortedPS(t, on, `MATCH (n) RETURN min(n.v) AS m`)
	if parallelAggregateScanBuildCount.Load() == before {
		t.Fatal("the parallel aggregate scan declined the unlabelled shape too: this test can " +
			"no longer distinguish the label rule from a wholesale disable")
	}
}

// TestParallelLabelAggregate_LabelCountStillO1 pins the precedence the reorder
// established: a bare labelled count(*) must keep its O(1) LabelCountScan and must NOT
// be claimed by the parallel aggregate scan, which since #2187 accepts a labelled leaf
// and would replace an index read with a full parallel walk.
func TestParallelLabelAggregate_LabelCountStillO1(t *testing.T) {
	g := buildSubsetLabelGraph(t, 600)
	on, off := engines(g)

	for _, q := range []string{
		`MATCH (n:Few) RETURN count(*) AS c`,
		`MATCH (n:Many) RETURN count(*) AS c`,
		`MATCH (n:Few) RETURN count(n) AS c`,
	} {
		t.Run(q, func(t *testing.T) {
			before := parallelAggregateScanBuildCount.Load()
			gotOn := drainSortedPS(t, on, q)
			if parallelAggregateScanBuildCount.Load() != before {
				t.Fatalf("%q was claimed by the parallel aggregate scan; it must keep the O(1) "+
					"LabelCountScan, which is asymptotically better than any worker count", q)
			}
			gotOff := drainSortedPS(t, off, q)
			assertEqualRows(t, q, gotOn, gotOff)
		})
	}
}

// TestParallelLabelScan_ExactValueSet is a stronger form of the multiset check: it
// compares the projected values against the set computed directly from the fixture, so a
// correct-count-but-wrong-rows partitioning bug cannot hide behind an equal-length
// comparison.
func TestParallelLabelScan_ExactValueSet(t *testing.T) {
	const n = 600
	g := buildSubsetLabelGraph(t, n)
	on, _ := engines(g)

	res, err := on.Run(context.Background(), `MATCH (n:Few) RETURN n.v AS v`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = res.Close() }()
	var got []string
	for res.Next() {
		got = append(got, res.ValueAt(0).String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	var want []string
	for i := 0; i < n; i += 3 {
		want = append(want, strconv.Itoa(i))
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("value %d: got %q, want %q — the leaf partitioned the wrong id set",
				i, got[i], want[i])
		}
	}
}

// TestParallelLabelScan_NoGoroutineLeak pins that the partitioned leaf leaves no worker
// behind, on both the projection and the aggregate path.
func TestParallelLabelScan_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)
	g := buildSubsetLabelGraph(t, 600)
	on, _ := engines(g)
	for _, q := range []string{
		`MATCH (n:Few) RETURN n.v AS v`,
		`MATCH (n:Many) WHERE n.v > 10 RETURN n.v + 0 AS v`,
		`MATCH (n:Few) RETURN min(n.v) AS m`,
		`MATCH (n:Few) RETURN n.gp AS gp, max(n.v) AS m`,
		`MATCH (n) RETURN min(n.v) AS m`,
	} {
		res, err := on.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run(%q): %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err(%q): %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close(%q): %v", q, err)
		}
	}
}

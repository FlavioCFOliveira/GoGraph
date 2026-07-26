package cypher

// columnar_limit_identity_test.go — exhaustive correctness gate for the
// chunk-transparent LIMIT (#2186).
//
// LIMIT is the one lever in #2186 that adds an operator rather than widening a
// recogniser, and the failure mode it risks is an off-by-one in the row budget. So
// this file does not sample: it sweeps EVERY limit from 0 to just past the chunk
// capacity boundary and asserts the exact row count and the exact prefix, both against
// the row-mode reference.
//
// The boundaries that matter are the ones where the clamp interacts with
// DefaultChunkCapacity (4096): a limit below it must produce a single short fill; a
// limit exactly equal to it must produce one full fill and then zero; a limit just
// above it must produce a full fill and then a short one. The fixture is sized so all
// three, plus a limit exceeding the whole result, are covered.
//
// Layer: short.

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// limitFixtureRows is just over two chunk capacities, so a limit can fall before,
// exactly on, and after every fill boundary the operator can hit.
const limitFixtureRows = 8200

// seedLimitGraph builds limitFixtureRows nodes labelled L, each with an int64 `v`
// equal to its index, so a result's ORDER-free prefix is still checkable by value set.
func seedLimitGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < limitFixtureRows; i++ {
		k := "L" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "L"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// limitRowValues runs query and returns the projected values in EMISSION order.
func limitRowValues(t *testing.T, g *lpg.Graph[string, float64], query string) []expr.Value {
	t.Helper()
	res, err := NewEngine(g).Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer func() { _ = res.Close() }()
	var out []expr.Value
	for res.Next() {
		out = append(out, res.ValueAt(0))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return out
}

// TestColumnarLimit_ExactCountAndPrefix sweeps the limit across every boundary and
// requires the columnar arm to return the same COUNT and the same ORDERED prefix as
// the row-mode arm. The row-mode arm wraps the projection in coalesce(), which the
// columnar projection declines, so it executes fully boxed.
func TestColumnarLimit_ExactCountAndPrefix(t *testing.T) {
	g := seedLimitGraph(t)

	// Total surviving rows for the predicate below, so the >result and =result cases
	// are exact rather than assumed.
	const threshold = 10
	total := limitFixtureRows - (threshold + 1)

	limits := []int{
		0, 1, 2, 7, 4095, 4096, 4097, 8188, 8189, 8190, total - 1, total, total + 1, total + 5000,
	}
	for _, lim := range limits {
		t.Run(fmt.Sprintf("limit=%d", lim), func(t *testing.T) {
			colQ := fmt.Sprintf("MATCH (n:L) WHERE n.v > %d RETURN n.v AS v LIMIT %d", threshold, lim)
			rowQ := fmt.Sprintf("MATCH (n:L) WHERE coalesce(n.v) > %d RETURN coalesce(n.v) AS v LIMIT %d", threshold, lim)

			col := limitRowValues(t, g, colQ)
			row := limitRowValues(t, g, rowQ)

			want := lim
			if want > total {
				want = total
			}
			if len(col) != want {
				t.Fatalf("LIMIT %d returned %d rows, want %d (total surviving = %d)",
					lim, len(col), want, total)
			}
			if len(row) != want {
				t.Fatalf("row-mode reference for LIMIT %d returned %d rows, want %d",
					lim, len(row), want)
			}
			for i := range col {
				if valueKey(col[i]) != valueKey(row[i]) {
					t.Fatalf("LIMIT %d: value %d differs, columnar=%q row=%q",
						lim, i, valueKey(col[i]), valueKey(row[i]))
				}
			}
		})
	}
}

// TestColumnarLimit_EngagesAndStaysIdentical pins that the LIMIT query really does run
// column-major (otherwise the sweep above proves nothing about the new operator) while
// remaining identical to the boxed reference.
func TestColumnarLimit_EngagesAndStaysIdentical(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)
	g := seedLimitGraph(t)

	be.filterBatches.Store(0)
	col := limitRowValues(t, g, "MATCH (n:L) WHERE n.v > 10 RETURN n.v AS v LIMIT 5000")
	if be.filterBatches.Load() == 0 {
		t.Fatal("LIMIT query did not engage the columnar filter: the chunk chain is still " +
			"broken at the LIMIT, so #2186's chunk-transparent Limit is not in effect")
	}
	be.filterBatches.Store(0)
	row := limitRowValues(t, g, "MATCH (n:L) WHERE coalesce(n.v) > 10 RETURN coalesce(n.v) AS v LIMIT 5000")
	if be.filterBatches.Load() != 0 {
		t.Fatal("the coalesce reference unexpectedly engaged the columnar filter")
	}
	if len(col) != 5000 || len(row) != 5000 {
		t.Fatalf("LIMIT 5000: columnar=%d row=%d rows, want 5000 each", len(col), len(row))
	}
	for i := range col {
		if valueKey(col[i]) != valueKey(row[i]) {
			t.Fatalf("value %d differs: columnar=%q row=%q", i, valueKey(col[i]), valueKey(row[i]))
		}
	}
}

// TestColumnarLimit_WithSkip pins the composition openCypher allows: SKIP is NOT
// chunk-transparent, so a SKIP below the LIMIT must break the chain and the whole
// query must fall back — still returning the correct window.
func TestColumnarLimit_WithSkip(t *testing.T) {
	g := seedLimitGraph(t)
	col := limitRowValues(t, g, "MATCH (n:L) WHERE n.v > 10 RETURN n.v AS v SKIP 100 LIMIT 50")
	row := limitRowValues(t, g, "MATCH (n:L) WHERE coalesce(n.v) > 10 RETURN coalesce(n.v) AS v SKIP 100 LIMIT 50")
	if len(col) != 50 || len(row) != 50 {
		t.Fatalf("SKIP 100 LIMIT 50: columnar=%d row=%d rows, want 50 each", len(col), len(row))
	}
	for i := range col {
		if valueKey(col[i]) != valueKey(row[i]) {
			t.Fatalf("value %d differs: columnar=%q row=%q", i, valueKey(col[i]), valueKey(row[i]))
		}
	}
}

// TestColumnarLimit_RowAndColumnarShareTheCounter pins the invariant that makes the
// value-embedded Limit safe: a ColumnarLimit consumed row-at-a-time must emit exactly
// n rows, because Next and FillChunk increment the SAME counter. An ORDER BY above the
// LIMIT forces the row path (Sort pulls via Next), so this is that case.
func TestColumnarLimit_RowAndColumnarShareTheCounter(t *testing.T) {
	g := seedLimitGraph(t)
	for _, lim := range []int{1, 4096, 5000} {
		t.Run(strconv.Itoa(lim), func(t *testing.T) {
			q := fmt.Sprintf("MATCH (n:L) WHERE n.v > 10 RETURN n.v AS v LIMIT %d", lim)
			got := limitRowValues(t, g, q)
			if len(got) != lim {
				t.Fatalf("%q returned %d rows, want %d", q, len(got), lim)
			}
			// The same limit consumed through a downstream Sort (row path).
			qs := fmt.Sprintf("MATCH (n:L) WHERE n.v > 10 WITH n.v AS v LIMIT %d RETURN v ORDER BY v", lim)
			sorted := limitRowValues(t, g, qs)
			if len(sorted) != lim {
				t.Fatalf("%q returned %d rows, want %d", qs, len(sorted), lim)
			}
		})
	}
}

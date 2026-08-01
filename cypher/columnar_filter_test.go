package cypher

// columnar_filter_test.go — byte-identity guard for the late-materialisation
// columnar read chain: scan → ColumnarFilter → chunk-input ColumnarProject
// (#1704 P3, task #1824).
//
// The columnar chain (a typed int64 NodeID column threaded from the scan, through
// a predicate evaluated over the unboxed columns, into a typed projection Chunk
// boxed only at the sink) MUST select exactly the same rows AND produce the
// byte-identical projected values as the row-at-a-time path. These tests prove it
// by running the SAME data two ways over the SAME scan order:
//
//   - columnar:  MATCH (n) WHERE n.v >= 0 RETURN n.v AS v
//   - row path:  MATCH (n) WHERE coalesce(n.v) >= 0 RETURN coalesce(n.v) AS v
//
// coalesce(x) returns x unchanged for a single argument, so the two queries are
// value-equivalent; but the function call disqualifies BOTH the columnar predicate
// (its operand is a function call, not a bare `node.prop`) AND the columnar
// projection, forcing the fully boxed row path (evalRowPooled → evalOrdering /
// lpgPropToExpr) — the exact pre-change behaviour. Any divergence in the surviving
// row set or the projected values is a byte-identity defect. Both scans walk the
// same mapper order, so the surviving rows line up element-wise.
//
// A metrics probe asserts the columnar filter actually engaged for the columnar
// query (and did NOT for the coalesce query), so a silent fallback cannot make the
// comparison vacuous, and matChunk asserts the columnar projection engaged.

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// columnarFilterBatchMetric mirrors the unexported exec counter name; the exec
// package increments it once per columnar FillChunk batch. Kept in sync by intent —
// a mismatch would make the probe silently read zero, which the "engaged" assertion
// below would catch.
const columnarFilterBatchMetric = "cypher.exec.columnar_filter.batch"

// countingBackend records only the columnar-filter batch counter, so a test can
// prove the columnar filter path ran (or did not).
type countingBackend struct{ filterBatches atomic.Uint64 }

func (c *countingBackend) IncCounter(name string, delta uint64) {
	if name == columnarFilterBatchMetric {
		c.filterBatches.Add(delta)
	}
}
func (c *countingBackend) ObserveLatency(string, time.Duration) {}

func (c *countingBackend) SetGauge(string, float64) {}

// mixedFilterGraph builds a graph whose one "v" property spans every kind the
// columnar filter must handle: integers around the small-int cache boundary and
// int64 extremes, floats incl. NaN/±Inf/±0, strings incl. empty/unicode/a
// temporal-tag-lookalike, booleans, a real temporal (stored SOH-tagged), and a
// node with no "v" at all. Node keys are zero-padded so the mapper walk order is
// stable and identical across two builds of the same graph.
func mixedFilterGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	vals := []struct {
		set bool
		v   lpg.PropertyValue
	}{
		{true, lpg.Int64Value(math.MinInt64)},
		{true, lpg.Int64Value(-256)},
		{true, lpg.Int64Value(-1)},
		{true, lpg.Int64Value(0)},
		{true, lpg.Int64Value(1)},
		{true, lpg.Int64Value(255)},
		{true, lpg.Int64Value(256)},
		{true, lpg.Int64Value(1000)},
		{true, lpg.Int64Value(math.MaxInt64)},
		{true, lpg.Float64Value(-1.5)},
		{true, lpg.Float64Value(math.Copysign(0, -1))},
		{true, lpg.Float64Value(0.0)},
		{true, lpg.Float64Value(1.5)},
		{true, lpg.Float64Value(3.14159)},
		{true, lpg.Float64Value(math.NaN())},
		{true, lpg.Float64Value(math.Inf(1))},
		{true, lpg.Float64Value(math.Inf(-1))},
		{true, lpg.StringValue("")},
		{true, lpg.StringValue("a")},
		{true, lpg.StringValue("m")},
		{true, lpg.StringValue("z")},
		{true, lpg.StringValue("héllo")},
		{true, lpg.StringValue("\x01not-a-date")},
		{true, lpg.BoolValue(true)},
		{true, lpg.BoolValue(false)},
		{true, lpg.DateValue(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC))},
		{false, lpg.PropertyValue{}},
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i, cell := range vals {
		key := padKey(i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if cell.set {
			if err := g.SetNodeProperty(key, "v", cell.v); err != nil {
				t.Fatalf("SetNodeProperty(%s): %v", key, err)
			}
		}
	}
	return g
}

// padKey returns a fixed-width decimal key so lexical and insertion order agree.
func padKey(i int) string {
	s := "000" + strconv.Itoa(i)
	return "n" + s[len(s)-3:]
}

// drainFilteredValues runs query (which must project a single column aliased v)
// and returns the *Result plus the ordered surviving values, cross-checking the
// positional and by-name accessors agree for every row.
func drainFilteredValues(t *testing.T, g *lpg.Graph[string, float64], query string) (*Result, []expr.Value) {
	t.Helper()
	eng := NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	var vals []expr.Value
	for res.Next() {
		byPos := res.ValueAt(0)
		byName, _ := res.Record()["v"].(expr.Value)
		if !valuesByteIdentical(byPos, byName) {
			t.Fatalf("ValueAt/Record disagree for %q: pos=%#v name=%#v", query, byPos, byName)
		}
		vals = append(vals, byPos)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return res, vals
}

func TestColumnarFilter_ByteIdentity(t *testing.T) {
	// Each predicate is written with the property as `n.v`; the row-path reference
	// replaces every `n.v` with `coalesce(n.v)`, which is value-equivalent but forces
	// the fully boxed path.
	predicates := []string{
		// Integer constant: int properties fast-path, every other kind falls back.
		"n.v >= 0",
		"n.v > 0",
		"n.v <= 0",
		"n.v < 256",
		"n.v = 0",
		"n.v <> 0",
		"n.v = 256",
		"n.v = 1000",
		// Constant on the left (operator flipped).
		"0 <= n.v",
		"256 > n.v",
		// Float constant: float properties fast-path (incl. NaN/±Inf/±0), ints fall back.
		"n.v >= 0.0",
		"n.v < 1.5",
		"n.v = 1.5",
		"n.v <> 1.5",
		"n.v = 0.0",
		// String constant: string properties fast-path (temporal-tagged strings fall back).
		"n.v >= 'm'",
		"n.v < 'm'",
		"n.v = 'a'",
		"n.v <> 'a'",
		"n.v = ''",
		// Bool constant.
		"n.v = true",
		"n.v <> true",
		"n.v = false",
	}

	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, pred := range predicates {
		t.Run(pred, func(t *testing.T) {
			colQuery := "MATCH (n) WHERE " + pred + " RETURN n.v AS v"
			rowPred := strings.ReplaceAll(pred, "n.v", "coalesce(n.v)")
			rowQuery := "MATCH (n) WHERE " + rowPred + " RETURN coalesce(n.v) AS v"

			be.filterBatches.Store(0)
			colRes, colVals := drainFilteredValues(t, mixedFilterGraph(t), colQuery)
			defer func() { _ = colRes.Close() }()
			colBatches := be.filterBatches.Load()

			be.filterBatches.Store(0)
			rowRes, rowVals := drainFilteredValues(t, mixedFilterGraph(t), rowQuery)
			defer func() { _ = rowRes.Close() }()
			rowBatches := be.filterBatches.Load()

			// The columnar query must engage BOTH the columnar filter (batch counter
			// > 0) and the columnar projection (matChunk != nil); the coalesce query
			// must engage neither, or the comparison proves nothing.
			if colBatches == 0 {
				t.Fatalf("columnar query %q did not engage the columnar filter", colQuery)
			}
			if colRes.matChunk == nil {
				t.Fatalf("columnar query %q did not engage the columnar projection", colQuery)
			}
			if rowBatches != 0 {
				t.Fatalf("coalesce query %q unexpectedly engaged the columnar filter", rowQuery)
			}
			if rowRes.matChunk != nil {
				t.Fatalf("coalesce query %q unexpectedly engaged the columnar projection", rowQuery)
			}

			if len(colVals) != len(rowVals) {
				t.Fatalf("surviving row counts differ: columnar=%d row=%d for %q", len(colVals), len(rowVals), pred)
			}
			for i := range colVals {
				if !valuesByteIdentical(colVals[i], rowVals[i]) {
					t.Fatalf("row %d byte-identity for %q: columnar=%#v row-path=%#v", i, pred, colVals[i], rowVals[i])
				}
			}
		})
	}
}

// TestColumnarFilter_NoWhere_ScanChunkInput proves the no-filter case
// (MATCH (n) RETURN n.v) also unboxes the scan by consuming it column-major, and
// stays byte-identical to the boxed coalesce path. It must NOT engage the columnar
// filter (there is no WHERE) but MUST engage the columnar projection.
func TestColumnarFilter_NoWhere_ScanChunkInput(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	colRes, colVals := drainFilteredValues(t, mixedFilterGraph(t), "MATCH (n) RETURN n.v AS v")
	defer func() { _ = colRes.Close() }()
	if be.filterBatches.Load() != 0 {
		t.Fatalf("no-WHERE query unexpectedly engaged the columnar filter")
	}
	if colRes.matChunk == nil {
		t.Fatalf("no-WHERE query did not engage the columnar projection")
	}

	rowRes, rowVals := drainFilteredValues(t, mixedFilterGraph(t), "MATCH (n) RETURN coalesce(n.v) AS v")
	defer func() { _ = rowRes.Close() }()

	if len(colVals) != len(rowVals) {
		t.Fatalf("row counts differ: columnar=%d row=%d", len(colVals), len(rowVals))
	}
	for i := range colVals {
		if !valuesByteIdentical(colVals[i], rowVals[i]) {
			t.Fatalf("row %d byte-identity: columnar=%#v row-path=%#v", i, colVals[i], rowVals[i])
		}
	}
}

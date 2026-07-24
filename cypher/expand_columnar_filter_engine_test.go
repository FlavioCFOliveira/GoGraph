package cypher

// expand_columnar_filter_engine_test.go — engine byte-identity + engagement guard
// for the columnar read chain over a single-hop traversal (rmp #2106):
// scan → columnar Expand → ColumnarFilter → chunk-input ColumnarProject.
//
// A post-traversal WHERE on the far node (`MATCH (n)-[r]->(p) WHERE p.v CMP c
// RETURN p.v`) must select exactly the same edges AND project the byte-identical
// far-node values as the fully boxed row path. The row-path reference wraps every
// `p.v` in coalesce(), which is value-equivalent but disqualifies both the columnar
// predicate and the columnar projection, forcing the pre-#2106 boxed path. A metrics
// probe asserts the columnar filter engaged for the columnar query and did NOT for
// the coalesce query, so a silent fallback cannot make the comparison vacuous.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// farNodeGraph builds a hub node with one out-edge to each of a set of target
// nodes whose "v" property spans every kind the columnar filter must handle. The
// hub is added first so a MATCH (n)-[r]->(p) enumerates exactly hub→target edges,
// binding p to each target in a stable order.
func farNodeGraph(t *testing.T) *lpg.Graph[string, float64] {
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
		{true, lpg.Float64Value(math.NaN())},
		{true, lpg.Float64Value(math.Inf(1))},
		{true, lpg.Float64Value(math.Inf(-1))},
		{true, lpg.StringValue("")},
		{true, lpg.StringValue("a")},
		{true, lpg.StringValue("m")},
		{true, lpg.StringValue("z")},
		{true, lpg.StringValue("\x01not-a-date")},
		{true, lpg.BoolValue(true)},
		{true, lpg.BoolValue(false)},
		{true, lpg.DateValue(time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC))},
		{false, lpg.PropertyValue{}},
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("hub"); err != nil {
		t.Fatalf("AddNode(hub): %v", err)
	}
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
		if err := g.AddEdge("hub", key, 1.0); err != nil {
			t.Fatalf("AddEdge(hub->%s): %v", key, err)
		}
	}
	return g
}

// sortedValueKeys renders a slice of values to sorted canonical keys (kind + IEEE-754
// bit pattern for floats) so two result multisets compare byte-identically
// regardless of traversal/emission order.
func sortedValueKeys(vals []expr.Value) []string {
	keys := make([]string, len(vals))
	for i, v := range vals {
		keys[i] = valueKey(v)
	}
	sort.Strings(keys)
	return keys
}

func valueKey(v expr.Value) string {
	if v == nil || expr.IsNull(v) {
		return "null"
	}
	switch cv := v.(type) {
	case expr.FloatValue:
		return "Float:" + strconv.FormatUint(math.Float64bits(float64(cv)), 16)
	default:
		return fmt.Sprintf("%s:%v", v.Kind(), v)
	}
}

func TestColumnarExpandFilter_ByteIdentity(t *testing.T) {
	predicates := []string{
		"p.v >= 0",
		"p.v > 0",
		"p.v <= 0",
		"p.v < 256",
		"p.v = 0",
		"p.v <> 0",
		"p.v = 1000",
		"0 <= p.v",
		"256 > p.v",
		"p.v >= 0.0",
		"p.v < 1.5",
		"p.v = 1.5",
		"p.v <> 1.5",
		"p.v >= 'm'",
		"p.v < 'm'",
		"p.v = 'a'",
		"p.v = ''",
		"p.v = true",
		"p.v <> true",
	}

	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, pred := range predicates {
		t.Run(pred, func(t *testing.T) {
			colQuery := "MATCH (n)-[r]->(p) WHERE " + pred + " RETURN p.v AS v"
			rowPred := strings.ReplaceAll(pred, "p.v", "coalesce(p.v)")
			rowQuery := "MATCH (n)-[r]->(p) WHERE " + rowPred + " RETURN coalesce(p.v) AS v"

			be.filterBatches.Store(0)
			colRes, colVals := drainFilteredValues(t, farNodeGraph(t), colQuery)
			defer func() { _ = colRes.Close() }()
			colBatches := be.filterBatches.Load()

			be.filterBatches.Store(0)
			rowRes, rowVals := drainFilteredValues(t, farNodeGraph(t), rowQuery)
			defer func() { _ = rowRes.Close() }()
			rowBatches := be.filterBatches.Load()

			if colBatches == 0 {
				t.Fatalf("columnar query %q did not engage the columnar filter over Expand", colQuery)
			}
			if colRes.matChunk == nil {
				t.Fatalf("columnar query %q did not engage the columnar projection", colQuery)
			}
			if rowBatches != 0 {
				t.Fatalf("coalesce query %q unexpectedly engaged the columnar filter", rowQuery)
			}

			if len(colVals) != len(rowVals) {
				t.Fatalf("surviving row counts differ: columnar=%d row=%d for %q", len(colVals), len(rowVals), pred)
			}
			colKeys := sortedValueKeys(colVals)
			rowKeys := sortedValueKeys(rowVals)
			for i := range colKeys {
				if colKeys[i] != rowKeys[i] {
					t.Fatalf("multiset mismatch at %d for %q: columnar=%q row-path=%q", i, pred, colKeys[i], rowKeys[i])
				}
			}
		})
	}
}

// TestColumnarExpandFilter_ProjectFarAndNear proves a projection that reads both the
// near (source, chunk column 0) and the far (dstID column) node properties over the
// traversal is byte-identical to the boxed path, exercising the generalised
// arbitrary-column chunk-input projection (#2106).
func TestColumnarExpandFilter_ProjectFarAndNear(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	be.filterBatches.Store(0)
	colRes, err := NewEngine(farNodeGraph(t)).Run(context.Background(),
		"MATCH (n)-[r]->(p) WHERE p.v >= 0 RETURN p.v AS pv", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = colRes.Close() }()
	var colVals []expr.Value
	for colRes.Next() {
		colVals = append(colVals, colRes.ValueAt(0))
	}
	if err := colRes.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if be.filterBatches.Load() == 0 {
		t.Fatalf("columnar Expand filter did not engage")
	}

	rowRes, err := NewEngine(farNodeGraph(t)).Run(context.Background(),
		"MATCH (n)-[r]->(p) WHERE coalesce(p.v) >= 0 RETURN coalesce(p.v) AS pv", nil)
	if err != nil {
		t.Fatalf("Run row: %v", err)
	}
	defer func() { _ = rowRes.Close() }()
	var rowVals []expr.Value
	for rowRes.Next() {
		rowVals = append(rowVals, rowRes.ValueAt(0))
	}
	if err := rowRes.Err(); err != nil {
		t.Fatalf("Err row: %v", err)
	}
	if len(colVals) != len(rowVals) {
		t.Fatalf("row counts differ: columnar=%d row=%d", len(colVals), len(rowVals))
	}
	c, r := sortedValueKeys(colVals), sortedValueKeys(rowVals)
	for i := range c {
		if c[i] != r[i] {
			t.Fatalf("mismatch at %d: columnar=%q row=%q", i, c[i], r[i])
		}
	}
}

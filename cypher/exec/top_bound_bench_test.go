package exec

// top_bound_bench_test.go — #2509
//
// The measurement behind the bound-handling design in [Top]: how the cost of the
// bounded operator moves as the bound n approaches the input size M.
//
// [Sort] is the in-build CONTROL. It is not touched by #2509, so its arm is the
// same program before and after the change; any movement in it across two builds
// is layout or environment noise, not the change, and the Top/Sort RATIO is
// therefore the quantity to read rather than either absolute.
//
// Both arms drain exactly n rows. Sort must order all M regardless, so draining
// it to n rather than to M is what a Sort→Limit plan actually does and is the
// fair comparison against Top(n).
//
// The fixture mirrors the #352 audit graph: 120 000 rows over a 65 000-value key
// domain, so roughly half the rows share their key with another row. Ties are
// not an edge case here, they are the common case, exactly as they are in the
// audit fixture's 55 000 duplicate salaries.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

const (
	// boundBenchM matches the audit fixture's node count.
	boundBenchM = 120_000
	// boundBenchKeyDomain matches the audit fixture's salary domain, so the tie
	// density is the same one the profile was taken over.
	boundBenchKeyDomain = 65_000
)

// boundBenchRows builds the shared input. Column 0 is a unique payload; column 1
// is the key source, read through an EVALUATOR rather than by index because that
// is the shape ORDER BY takes over a projected entity and the shape whose key
// materialisation dominates.
func boundBenchRows() []Row {
	rows := make([]Row, boundBenchM)
	for i := range rows {
		rows[i] = Row{
			expr.IntegerValue(int64(1_000_000 + i)),
			expr.IntegerValue(int64(100_000 + i%boundBenchKeyDomain)),
		}
	}
	return rows
}

func boundBenchKey() SortKey {
	return SortKey{Ascending: true, Eval: func(r Row) (expr.Value, error) {
		if len(r) > 1 {
			return r[1], nil
		}
		return expr.Null, nil
	}}
}

// drainN runs op and discards the first n rows it emits, stopping there.
func drainN(b *testing.B, op Operator, n int) {
	b.Helper()
	if err := op.Init(context.Background()); err != nil {
		b.Fatalf("Init: %v", err)
	}
	var row Row
	for i := 0; i < n; i++ {
		ok, err := op.Next(&row)
		if err != nil {
			b.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
	}
	if err := op.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}
}

// boundBenchPoints sweep the bound from far below the input size up to it. The
// interesting region is the right-hand end: a bounded heap whose bound
// approaches M performs a replacement for a large fraction of the input and
// keeps every retained row's key live, which is where the audit predicted the
// bounded operator would lose to an unbounded sort.
var boundBenchPoints = []int{10, 110, 10_010, 60_000, boundBenchM}

// BenchmarkBoundedOrder measures Sort→take(n) against Top(n) over the same rows.
// Read the RATIO between the two arms at each n, not the absolutes.
func BenchmarkBoundedOrder(b *testing.B) {
	rows := boundBenchRows()
	key := boundBenchKey()
	for _, n := range boundBenchPoints {
		n := n
		b.Run(fmt.Sprintf("n=%06d/Sort", n), func(b *testing.B) {
			src := &sliceSource{rows: rows}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				op, err := NewSort(src, []SortKey{key}, 0)
				if err != nil {
					b.Fatalf("NewSort: %v", err)
				}
				drainN(b, op, n)
			}
		})
		b.Run(fmt.Sprintf("n=%06d/Top", n), func(b *testing.B) {
			src := &sliceSource{rows: rows}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				op, err := NewTop(src, []SortKey{key}, n, 0)
				if err != nil {
					b.Fatalf("NewTop: %v", err)
				}
				drainN(b, op, n)
			}
		})
	}
}

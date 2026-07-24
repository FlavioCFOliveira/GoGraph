package exec_test

// agg_column_kernel_bench_test.go — allocs/op de-box benchmark for columnar
// (chunk-input) EagerAggregation (rmp #2104).
//
// The same aggregation over the same 1M-row input is driven two ways:
//
//   - columnar_on:  WithChunkInput → the SoA kernels read the unboxed int64 argument
//     column and scatter-accumulate it; no per-row Row allocation, no per-argument
//     expr.Value box.
//   - row_off:      the row path pulls the source via Next (one boxed Row per row) and
//     Steps a boxed expr.Value per argument into a per-group funcs.Aggregator.
//
// benchstat over the two sub-benchmarks quantifies the O(input) de-box win.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// genAggSource deterministically produces n rows of (key, value), key ∈ [0, groups),
// value = the running index (large enough that boxing it allocates). It serves the
// column-major path (FillChunk, unboxed int64 columns) and the row path (Next, a
// freshly boxed Row per row) from the same generator, so a benchmark can drive one
// operator both ways over identical data.
type genAggSource struct {
	n      int
	groups int
	idx    int
}

func (s *genAggSource) Init(_ context.Context) error { s.idx = 0; return nil }
func (s *genAggSource) Close() error                 { return nil }

func (s *genAggSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= s.n {
		return false, nil
	}
	r := make(exec.Row, 2)
	r[0] = expr.IntegerValue(int64(s.idx % s.groups))
	r[1] = expr.IntegerValue(int64(s.idx))
	s.idx++
	*out = r
	return true, nil
}

func (s *genAggSource) NewOutputChunk(capacity int) *exec.Chunk {
	return exec.NewChunk(capacity, expr.KindInteger, expr.KindInteger)
}

func (s *genAggSource) FillChunk(dst *exec.Chunk, maxRows int) (int, error) {
	n := 0
	for n < maxRows && s.idx < s.n {
		dst.AppendInt64(0, int64(s.idx%s.groups))
		dst.AppendInt64(1, int64(s.idx))
		s.idx++
		n++
	}
	return n, nil
}

// benchDrainAgg drives op to completion, discarding rows.
func benchDrainAgg(b *testing.B, op exec.Operator) {
	b.Helper()
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		b.Fatalf("Init: %v", err)
	}
	for {
		var r exec.Row
		ok, err := op.Next(&r)
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

// BenchmarkColumnarAgg_SumCountGroupBy aggregates SUM(value), COUNT(*) grouped by key
// over 1M rows, columnar ON vs OFF. Compare with:
//
//	go test ./cypher/exec/ -run '^$' -bench BenchmarkColumnarAgg_SumCountGroupBy -benchmem -count=10
func BenchmarkColumnarAgg_SumCountGroupBy(b *testing.B) {
	const n = 1 << 20 // ~1.05M rows
	const groups = 128

	for _, mode := range []struct {
		name  string
		chunk bool
	}{
		{"columnar_on", true},
		{"row_off", false},
	} {
		b.Run(mode.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				src := &genAggSource{n: n, groups: groups}
				op, err := exec.NewEagerAggregation(
					src,
					[]int{0},
					[]funcs.AggregatorFactory{funcs.NewSumAgg(), funcs.NewCountStarAgg()},
					0,
				)
				if err != nil {
					b.Fatalf("NewEagerAggregation: %v", err)
				}
				if mode.chunk {
					if werr := op.WithChunkInput(); werr != nil {
						b.Fatalf("WithChunkInput: %v", werr)
					}
				}
				benchDrainAgg(b, op)
			}
		})
	}
}

// BenchmarkColumnarAgg_MinMaxGroupBy aggregates MIN(value), MAX(value) grouped by key
// over 1M rows, columnar ON vs OFF. It pins that the numeric min/max fast path keeps
// the running best unboxed (per-row comparison via expr.Compare on inline scalars does
// not allocate), so the columnar allocs/op stays O(groups), not O(input).
func BenchmarkColumnarAgg_MinMaxGroupBy(b *testing.B) {
	const n = 1 << 20
	const groups = 128

	for _, mode := range []struct {
		name  string
		chunk bool
	}{
		{"columnar_on", true},
		{"row_off", false},
	} {
		b.Run(mode.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				src := &genAggSource{n: n, groups: groups}
				op, err := exec.NewEagerAggregation(
					src,
					[]int{0},
					[]funcs.AggregatorFactory{funcs.NewMinAgg(), funcs.NewMaxAgg()},
					0,
				)
				if err != nil {
					b.Fatalf("NewEagerAggregation: %v", err)
				}
				if mode.chunk {
					if werr := op.WithChunkInput(); werr != nil {
						b.Fatalf("WithChunkInput: %v", werr)
					}
				}
				benchDrainAgg(b, op)
			}
		})
	}
}

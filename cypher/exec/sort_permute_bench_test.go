package exec

// sort_permute_bench_test.go — #2652
//
// The measurement behind one design choice: how [Sort.sortDecorated] applies the
// permutation it computed. Two strategies produce identical output, so the choice
// is a resource question and is decided here rather than by argument.
//
//   - permuteRows: cycle following, in place, no auxiliary storage.
//   - permuteRowsBuffered: fill a fresh []Row and replace the slice.
//
// The buffered form is shorter and is what most implementations reach for. It
// costs one slice header per row on every execution AND it discards the backing
// array that [Sort.Init] reuses, so a cached plan re-executed over the same input
// pays it again each time. Read B/op and allocs/op, not ns/op: the point is the
// allocation, and at this size both walks are memory-bound anyway.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// permuteRowsBuffered is the rejected alternative, kept only so the benchmark
// below can measure what rejecting it saved. It is never called by the operator.
func permuteRowsBuffered(rows []Row, perm []int) []Row {
	out := make([]Row, len(rows))
	for i, p := range perm {
		out[i] = rows[p]
	}
	return out
}

// permuteBenchN matches the row count of the #352 audit fixture, so the numbers
// are comparable with the profile that motivated #2652.
const permuteBenchN = 120_000

// newPermuteBenchInput builds the rows and a full-reversal permutation, the
// worst case for cycle following (one cycle of length 2 per pair, so every row
// moves).
func newPermuteBenchInput() ([]Row, []int) {
	rows := make([]Row, permuteBenchN)
	for i := range rows {
		rows[i] = Row{expr.IntegerValue(int64(256 + i))}
	}
	perm := make([]int, permuteBenchN)
	for i := range perm {
		perm[i] = permuteBenchN - 1 - i
	}
	return rows, perm
}

func BenchmarkPermuteRowsInPlace(b *testing.B) {
	rows, perm := newPermuteBenchInput()
	scratch := make([]int, permuteBenchN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(scratch, perm) // permuteRows consumes its perm; restoring it is not measured as an allocation
		permuteRows(rows, scratch)
	}
}

func BenchmarkPermuteRowsBuffered(b *testing.B) {
	rows, perm := newPermuteBenchInput()
	var sink []Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink = permuteRowsBuffered(rows, perm)
	}
	if len(sink) != permuteBenchN {
		b.Fatalf("sink length %d", len(sink))
	}
}

package cypher

// result_bytes_cap_bench_test.go — the AC's mandatory performance clause for the
// aggregate-byte budget (#1328). The byte budget adds a per-row size estimate to
// the barrier-held materialise drain, so it must not regress the common
// small-result path. This benchmark drives the full Engine.Run → materialize
// path for a small whole-graph MATCH (the path every ordinary query takes) and
// reports ns/op + allocs/op so a benchstat before/after comparison can prove the
// accounting is within noise.
//
// It lives in the white-box cypher package (not cypher_test) so a follow-up can,
// if needed, also benchmark Result.materialize() in isolation against a
// pre-built ResultSet; the end-to-end form here is the one the AC names ("the
// common small-result path") and is what a consumer actually pays.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newBenchGraph builds an lpg.Graph with n bare nodes (no properties) for the
// small-result drain benchmark. It mirrors the cypher_test.newGraph helper but
// lives in the white-box package so the benchmark can share the cypher scope.
func newBenchGraph(b *testing.B, n int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		if err := g.AddNode(string(rune('A'+i%26)) + string(rune('0'+i%10)) + itoaBench(i)); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
	}
	return g
}

// newBenchGraphWide builds n nodes each carrying a single string property "blob"
// of blobLen bytes, so the per-row size estimate walks a non-trivial string
// value. Used to guard the wide-row path of the estimator against allocation.
func newBenchGraphWide(b *testing.B, n, blobLen int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	blob := strings.Repeat("x", blobLen)
	for i := 0; i < n; i++ {
		key := "K" + itoaBench(i)
		if err := g.SetNodeProperty(key, "blob", lpg.StringValue(blob)); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// itoaBench is a tiny allocation-free-enough decimal formatter used only to make
// node keys unique in the benchmark setup (setup is outside the timed region).
func itoaBench(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// benchSmallResult is the row count for the common small-result path. It is
// small enough that the per-row size accounting, being O(columns) per row, is
// the only thing the byte budget adds — exactly the cost the AC asks us to keep
// within noise.
const benchSmallResult = 100

// BenchmarkResultMaterialize_SmallResult measures the end-to-end cost of running
// and fully draining a small whole-graph MATCH under the default caps. The
// byte-budget accounting runs inside materialize() on this path, so the
// before/after delta of this benchmark is the regression signal the AC's perf
// clause requires.
func BenchmarkResultMaterialize_SmallResult(b *testing.B) {
	g := newBenchGraph(b, benchSmallResult)
	eng := NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		var count int
		for res.Next() {
			count++
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Result.Err: %v", err)
		}
		_ = res.Close()
		if count != benchSmallResult {
			b.Fatalf("drained %d rows, want %d", count, benchSmallResult)
		}
	}
}

// BenchmarkResultMaterialize_ScalarProjection measures the path a typical query
// takes: projecting a small scalar property per row (RETURN n.blob with a tiny
// blob), which is more representative of "the common small-result path" than the
// bare whole-node return above. The per-row drain here does real projection work,
// so the O(1)-column byte estimate is a smaller relative fraction.
func BenchmarkResultMaterialize_ScalarProjection(b *testing.B) {
	g := newBenchGraphWide(b, benchSmallResult, 16)
	eng := NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, "MATCH (n) RETURN n.blob AS blob", nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		var count int
		for res.Next() {
			count++
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Result.Err: %v", err)
		}
		_ = res.Close()
		if count != benchSmallResult {
			b.Fatalf("drained %d rows, want %d", count, benchSmallResult)
		}
	}
}

// BenchmarkResultMaterialize_WideRows measures the same drain when each row
// carries a moderately large string property, so the size accounting walks a
// non-trivial value per row. It guards the wide-row path against an accidental
// allocation in the estimator (the estimate must read the string length, never
// copy the bytes).
func BenchmarkResultMaterialize_WideRows(b *testing.B) {
	g := newBenchGraphWide(b, benchSmallResult, 512)
	eng := NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, "MATCH (n) RETURN n.blob AS blob", nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		var count int
		for res.Next() {
			count++
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Result.Err: %v", err)
		}
		_ = res.Close()
		if count != benchSmallResult {
			b.Fatalf("drained %d rows, want %d", count, benchSmallResult)
		}
	}
}

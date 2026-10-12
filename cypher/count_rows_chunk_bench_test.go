package cypher_test

// count_rows_chunk_bench_test.go — allocation evidence for rmp #3114: the
// column-major count drain (CountRows over a ColumnarFilter over a
// NodeByLabelScan) used to allocate two DefaultChunkCapacity-row integer chunks
// per statement — the drain's destination and the filter's scratch batch — even
// when the label holds a handful of nodes.
//
// The shape is the scan arm of examples/37_mvcc_write_contention's seek = scan
// check (`MATCH (m:L) WHERE m.v + 0 = $v RETURN count(m)`), which runs it once per
// verified value. The Small arm is that example's regime (the fixed per-statement
// cost dominates); the Big arm spans several 4096-row batches.
//
//	go test -run=^$ -bench='BenchmarkCountRowsChunk' -benchmem -count=10 ./cypher/
//
// Every arm disables the parallel scan so the serial CountRows drain is measured.
//
// Layer: short.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedCountRowsChunkGraph builds n nodes labelled Item, each with an int64 `v`.
func seedCountRowsChunkGraph(b *testing.B, n int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Item"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

func benchCountRowsChunk(b *testing.B, n int) {
	silenceBenchLogs(b)
	g := seedCountRowsChunkGraph(b, n)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})
	const q = "MATCH (m:Item) WHERE m.v >= 0 RETURN count(m)"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

func BenchmarkCountRowsChunk_FilteredSmall(b *testing.B) { benchCountRowsChunk(b, 64) }
func BenchmarkCountRowsChunk_FilteredBig(b *testing.B)   { benchCountRowsChunk(b, 20_000) }

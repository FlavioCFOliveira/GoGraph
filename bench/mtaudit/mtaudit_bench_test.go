// Package mtaudit is a throwaway engine-level scaling/profiling harness for the
// 2026-06-24 full performance audit. It bypasses the Bolt network layer to
// measure pure engine read/write scaling and allocation behaviour without
// connection-churn noise. NOT part of the module; safe to delete.
package mtaudit

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seed builds a directed graph of n labelled nodes, each carrying an Int64
// property "v". Labelling exercises the LabelRegistry path; the property
// exercises the PropertyKeyRegistry path.
func seed(n int) *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			panic(err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			panic(err)
		}
		_ = g.SetNodeProperty(id, "v", lpg.Int64Value(int64(i)))
	}
	return g
}

func runRead(b *testing.B, n int, q string) {
	g := seed(n)
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := eng.Run(ctx, q, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// --- Small graph (2000 nodes): contention-sensitive, below parallel threshold ---

// Count fast path (ParallelCountScan eligible only above threshold; here serial count).
func BenchmarkEngReadCount(b *testing.B) { runRead(b, 2000, "MATCH (n) RETURN count(n)") }

// Per-row filter, count result (touches PropertyKeyRegistry per row → R1).
func BenchmarkEngReadFilterCount(b *testing.B) {
	runRead(b, 2000, "MATCH (n) WHERE n.v >= 0 RETURN count(n)")
}

// Per-row filter + projection returning rows (materialises a NodeValue per row).
func BenchmarkEngReadProject(b *testing.B) {
	runRead(b, 2000, "MATCH (n) WHERE n.v >= 0 RETURN n.v")
}

// Label-match path (touches LabelRegistry → R2).
func BenchmarkEngReadLabel(b *testing.B) { runRead(b, 2000, "MATCH (n:N) RETURN count(n)") }

// --- Large graph (60000 nodes): above the 50k parallel-scan threshold ---

func BenchmarkEngReadCountLarge(b *testing.B) { runRead(b, 60000, "MATCH (n) RETURN count(n)") }

// ParallelCountScan with pushed-down filter (#1672).
func BenchmarkEngReadFilterCountLarge(b *testing.B) {
	runRead(b, 60000, "MATCH (n) WHERE n.v >= 0 RETURN count(n)")
}

// ParallelScanProject path (#1682): pushed-down filter + projection, parallel reduce.
func BenchmarkEngReadProjectLarge(b *testing.B) {
	runRead(b, 60000, "MATCH (n) WHERE n.v >= 0 RETURN n.v")
}

// --- Write path ---

// Autocommit write — expected flat (single-writer serialization).
func BenchmarkEngWriteAutocommit(b *testing.B) {
	g := seed(0)
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := eng.RunInTx(ctx, "CREATE (:N)", nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// N readers contend with one steady explicit-tx writer — visMu penalty (M2b).
func BenchmarkEngReadUnderWriter(b *testing.B) {
	g := seed(2000)
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	q := "MATCH (n) RETURN count(n)"

	var stop atomic.Bool
	var writes int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		i := 0
		for !stop.Load() {
			lbl := fmt.Sprintf("w%d", i)
			i++
			if _, err := eng.RunInTx(ctx, "CREATE (:W {id:'"+lbl+"'})", nil); err != nil {
				return
			}
			atomic.AddInt64(&writes, 1)
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := eng.Run(ctx, q, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.StopTimer()
	stop.Store(true)
	<-done
	b.ReportMetric(float64(atomic.LoadInt64(&writes)), "writes")
}

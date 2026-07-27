package txn_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// writerCounts is the concurrency ladder task #2221 acceptance criterion 2
// requires: throughput must be reported at each of these writer counts so the
// scaling curve — not a single point — is the evidence.
var writerCounts = []int{1, 8, 64, 256, 1024}

// BenchmarkWriteScaling_StoreAPI measures durable-commit throughput through the
// Go store API ([txn.Tx.Commit]) as concurrency rises. This path releases the
// single-writer semaphore after the append and fsyncs outside it
// ([txn.Tx.appendOnly] then [wal.Writer.SyncGroup]), so many committers are
// inside SyncGroup at once and one fsync amortises across the whole group.
//
// It is the control arm for BenchmarkWriteScaling_Cypher: both perform one
// durable single-edge transaction per operation against the same WAL and the
// same in-memory engine, so a divergence in their scaling curves isolates the
// commit-path coordination rather than the cost of the write itself.
func BenchmarkWriteScaling_StoreAPI(b *testing.B) {
	for _, writers := range writerCounts {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			dir := b.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				b.Fatalf("wal.Open: %v", err)
			}
			b.Cleanup(func() { _ = w.Close() })
			g := lpg.New[string, int64](adjlist.Config{Directed: true})
			s := txn.NewStoreWithCodec(g, w, txn.NewStringCodec())

			before := w.Stats().Syncs
			runConcurrent(b, writers, func(worker, i int) error {
				tx := s.Begin()
				if aerr := tx.AddEdge(
					fmt.Sprintf("w%d-s%d", worker, i),
					fmt.Sprintf("w%d-d%d", worker, i), 0); aerr != nil {
					_ = tx.Rollback()
					return aerr
				}
				return tx.Commit()
			})
			// The mean group size: commits per leader fsync. Reported so the
			// per-commit cost can be read against the batch size it was
			// achieved at, which is what task #2221 acceptance criterion 3
			// requires — the wake cost must be shown O(1) in batch size by
			// measurement, not asserted.
			if syncs := w.Stats().Syncs - before; syncs > 0 {
				b.ReportMetric(float64(b.N)/float64(syncs), "commits/fsync")
			}
		})
	}
}

// BenchmarkWriteScaling_Cypher measures durable-commit throughput through the
// Cypher engine's write path ([cypher.Engine.RunInTx] → commitUnderBarrier →
// [txn.Tx.CommitWALOnly]) as concurrency rises.
//
// Unlike the store API above, this path performs its WAL fsync while the
// graph's visibility barrier (lpg's visMu) is held in WRITE mode, so writers
// are strictly serialised across the disk sync and never coalesce. The
// benchmark exists to quantify that: a flat curve here against a rising one in
// BenchmarkWriteScaling_StoreAPI localises the ceiling to the barrier, not to
// the WAL.
func BenchmarkWriteScaling_Cypher(b *testing.B) {
	for _, writers := range writerCounts {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			dir := b.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				b.Fatalf("wal.Open: %v", err)
			}
			b.Cleanup(func() { _ = w.Close() })
			g := lpg.New[string, float64](adjlist.Config{Directed: true})
			s := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			})
			eng := cypher.NewEngineWithStore(s)

			// A bare anonymous CREATE: the smallest possible durable write, and
			// unique per operation by construction, so nothing here contends on
			// a shared node. No parameters, so parameter binding does not enter
			// the measurement.
			ctx := context.Background()
			runConcurrent(b, writers, func(worker, i int) error {
				res, rerr := eng.RunInTx(ctx, "CREATE (:N)", nil)
				if rerr != nil {
					return rerr
				}
				return res.Close()
			})
		})
	}
}

// runConcurrent drives exactly b.N durable commits spread over the given number
// of writer goroutines and reports throughput as ops/sec.
//
// Each worker draws its iteration index from one shared atomic counter, so the
// b.N budget is honoured exactly no matter how unevenly the goroutines are
// scheduled — the alternative (b.N/writers per worker) silently truncates the
// budget and, at 1024 writers with a small b.N, would measure goroutine startup
// rather than commits. The (worker, i) pair is still unique per operation, so
// every transaction touches distinct keys and none of them contend on the same
// node.
//
// Timing covers only the commit loop: the WAL, graph, and engine are built
// before the timer starts.
func runConcurrent(b *testing.B, writers int, commit func(worker, i int) error) {
	b.Helper()

	var next atomic.Int64
	var firstErr atomic.Value // error
	var wg sync.WaitGroup

	b.ResetTimer()
	start := b.Elapsed()
	wg.Add(writers)
	for worker := 0; worker < writers; worker++ {
		go func(worker int) {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= b.N {
					return
				}
				if err := commit(worker, i); err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	elapsed := b.Elapsed() - start
	b.StopTimer()

	if e := firstErr.Load(); e != nil {
		b.Fatalf("a concurrent commit failed: %v", e)
	}
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/sec")
	}
}

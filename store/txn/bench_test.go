package txn

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// openBenchStore builds a Store backed by a real on-disk WAL in a temp dir,
// mirroring openStore (the *testing.T helper) but for benchmarks. The WAL is
// real so Commit's single fsync per transaction is exercised: that fsync is
// the cost the upcoming group-commit work (#1507) must amortise, so it must
// not be hidden behind an in-memory writer here. Cleanup runs via b.Cleanup.
func openBenchStore(b *testing.B) *Store[string, int64] {
	b.Helper()
	dir := b.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		b.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithCodec(g, w, NewStringCodec())
	b.Cleanup(func() {
		_ = w.Close()
		_ = os.RemoveAll(dir)
	})
	return s
}

// commitOps drives one transaction that writes ops node labels and commits it,
// returning any error. Node keys are derived from seq so distinct transactions
// touch distinct keys and the graph grows monotonically (no contention on a
// single key, no accidental no-op writes).
func commitOps(s *Store[string, int64], seq, ops int) error {
	tx := s.Begin()
	for k := 0; k < ops; k++ {
		if err := tx.SetNodeLabel(fmt.Sprintf("n-%d-%d", seq, k), "Person"); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// BenchmarkCommit measures the full transaction commit path (buffer ops ->
// encode WAL frames -> append -> single fsync -> apply in memory) for a
// 1-op transaction and a k-op transaction. Each iteration commits a fresh
// transaction against distinct keys.
func BenchmarkCommit(b *testing.B) {
	for _, ops := range []int{1, 16} {
		b.Run(fmt.Sprintf("ops=%d", ops), func(b *testing.B) {
			s := openBenchStore(b)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := commitOps(s, i, ops); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCommitConcurrent measures throughput of 1-op commits driven by a
// fixed number of concurrent committer goroutines. Commits serialise on the
// store's single-writer lock, so this exposes lock-handoff plus per-commit
// fsync cost under contention — the regime group commit must improve. The
// total work is bounded by b.N (shared across goroutines via an atomic
// counter), so wall time scales with b.N and the short layer stays fast.
func BenchmarkCommitConcurrent(b *testing.B) {
	for _, goroutines := range []int{1, 8, 64, 256} {
		b.Run(fmt.Sprintf("goroutines=%d", goroutines), func(b *testing.B) {
			s := openBenchStore(b)
			b.ReportAllocs()
			b.ResetTimer()

			var seq atomic.Int64
			limit := int64(b.N)
			var wg sync.WaitGroup
			wg.Add(goroutines)
			var failed atomic.Bool
			for g := 0; g < goroutines; g++ {
				go func() {
					defer wg.Done()
					for {
						i := seq.Add(1) - 1
						if i >= limit {
							return
						}
						if err := commitOps(s, int(i), 1); err != nil {
							failed.Store(true)
							return
						}
					}
				}()
			}
			wg.Wait()
			b.StopTimer()
			if failed.Load() {
				b.Fatal("a concurrent commit failed")
			}
		})
	}
}

package lpg

// mvcc_vacuum_bench_test.go — MVCC C2 (rmp #2308) acceptance criterion 6: the
// commit-latency measurement that justifies moving reclamation off the commit
// path.
//
// # What is being compared, and how
//
// The change under measurement is WHERE the sweep runs, so the honest comparison
// is one commit's cost with the sweep on it against one commit's cost without.
// Both arms are driven in the SAME process by the same loop, because measuring
// them across two builds on this project's hardware has manufactured phantom
// regressions from a byte-identical control.
//
//   - Async is the shipped path: the committer charges its versions and, once per
//     [reclaimThreshold], wakes the background vacuum.
//   - Sync reproduces the placement rmp #2308 removed: the committer takes the
//     single-sweeper slot and performs a full unbounded sweep itself when the debt
//     is due.
//
// The workload is a long single-writer churn, which is the arm the sweep placement
// affects: it is the only shape where one goroutine both produces all the garbage
// and pays for all of it.
//
// Run:
//
//	go test ./graph/lpg/ -run '^$' -bench 'BenchmarkVacuumCommitLatency' -benchmem -count=5 | tee new.txt
//	benchstat new.txt
//
// Layer: short (benchmarks are not run by `go test`).

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// syncSweepIfDue is the placement rmp #2308 removed, kept HERE — in a test file —
// so the benchmark can measure the counterfactual without the production path
// carrying a mode switch it would never use.
//
// It is a faithful reproduction of the old [Graph.reclaimIfDue]: gated on the
// debt threshold, guarded against a concurrent sweep by the single-sweeper slot,
// and skipping rather than queueing when that slot is busy.
func syncSweepIfDue[N comparable, W any](g *Graph[N, W]) {
	if g.VersionCount() == 0 {
		g.reclaimDebt.Store(0)
		return
	}
	if g.reclaimDebt.Load() < reclaimThreshold {
		return
	}
	g.reclaimDebt.Store(0)
	if !g.vac.tryAcquireSweeper() {
		return
	}
	defer g.vac.releaseSweeper()
	watermark := g.horizon.Oldest(g.mvccClock.ReadTS())
	if watermark == 0 {
		return
	}
	for u := vacuumUnit(0); u < vacuumUnitCount; u++ {
		g.sweepUnit(u, watermark)
	}
	g.publishMVCCMetrics()
}

func benchVacuumCommitLatency(b *testing.B, onCommit bool) {
	b.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	b.Cleanup(func() { _ = g.Close() })
	if err := g.AddNode("a"); err != nil {
		b.Fatalf("AddNode: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			b.Fatalf("write %d: %v", i, err)
		}
		if onCommit {
			syncSweepIfDue(g)
		}
	}
	b.StopTimer()
	// Reported so a run whose two arms did different amounts of reclamation is
	// visible rather than silently comparable.
	b.ReportMetric(float64(g.VersionCount()), "retained")
}

// BenchmarkVacuumCommitLatency is the per-commit mean on a ONE-NODE graph, where
// the sweep the async arm avoids is nearly free — so this is the arm on which
// moving it off the commit path can only cost, and the number to check before
// claiming the move is free everywhere.
//
//	go test ./graph/lpg/ -run '^$' -bench 'BenchmarkVacuumCommitLatency' -benchmem -count=8 | tee mean.txt
//	benchstat -col /arm mean.txt
func BenchmarkVacuumCommitLatency(b *testing.B) {
	for _, arm := range []struct {
		name     string
		onCommit bool
	}{{"async", false}, {"sync", true}} {
		b.Run("arm="+arm.name, func(b *testing.B) { benchVacuumCommitLatency(b, arm.onCommit) })
	}
}

// benchVacuumCommitTail measures the DISTRIBUTION rather than the mean, over a
// graph SHAPE on which the sweep is expensive.
//
// # Why the shape is the whole measurement
//
// The first attempt churned ONE node and found both arms indistinguishable at
// every quantile — p50 125 ns, p99 and max overlapping inside the noise. That was
// not a null result about the placement; it was a null result about the workload.
// A sweep is O(objects carrying history), and a one-node graph has exactly one, so
// there was nothing for the committer to pay for.
//
// So this drives `nodes` distinct nodes round-robin, which is what a real write
// workload does, and the sweep then has that many chains to walk. The mean is
// still the wrong statistic — a sweep amortised over [reclaimThreshold] commits
// barely moves it — because what the placement decides is WHICH commit pays: one
// in every four thousand paying for everybody's garbage, or nobody paying. The
// module's mandate names that property directly ("Fair scheduling. Long-running
// operations yield … to keep latency tails bounded for concurrent short queries").
//
// Per-operation timing costs both arms the same two time.Now calls, so the
// comparison stands even though the absolute numbers carry that overhead.
func benchVacuumCommitTail(b *testing.B, onCommit bool, nodes, writers int) {
	b.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	b.Cleanup(func() { _ = g.Close() })
	keys := make([]string, nodes)
	for i := range keys {
		keys[i] = fmt.Sprintf("n%d", i)
		if err := g.AddNode(keys[i]); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
	}
	// Enough commits that the sweep fires many times over; fewer would leave the
	// sync arm's tail unpopulated and the comparison vacuous.
	perWriter := reclaimThreshold * 8 / writers

	b.ResetTimer()
	var mu sync.Mutex
	all := make([]time.Duration, 0, perWriter*writers)
	for i := 0; i < b.N; i++ {
		all = all[:0]
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				local := make([]time.Duration, 0, perWriter)
				for j := 0; j < perWriter; j++ {
					k := keys[(w*perWriter+j)%nodes]
					t0 := time.Now()
					if err := g.ApplyVersioned(func(WriteTx) error {
						return g.SetNodeProperty(k, "w", Int64Value(int64(j)))
					}); err != nil {
						// A serialization conflict is a legitimate outcome under
						// concurrent writers and is not a latency sample.
						continue
					}
					if onCommit {
						syncSweepIfDue(g)
					}
					local = append(local, time.Since(t0))
				}
				mu.Lock()
				all = append(all, local...)
				mu.Unlock()
			}(w)
		}
		wg.Wait()
	}
	b.StopTimer()
	if len(all) == 0 {
		b.Skip("no samples")
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	at := func(q float64) float64 {
		return float64(all[int(q*float64(len(all)-1))].Nanoseconds())
	}
	b.ReportMetric(at(0.50), "p50-ns")
	b.ReportMetric(at(0.99), "p99-ns")
	b.ReportMetric(at(0.999), "p999-ns")
	b.ReportMetric(float64(all[len(all)-1].Nanoseconds()), "max-ns")
	b.ReportMetric(float64(g.VersionCount()), "retained")
}

const benchTailNodes = 16384

// BenchmarkVacuumCommitTail compares the two placements at each writer count, as
// SUB-BENCHMARKS keyed `arm=` and `writers=` so benchstat can do the comparison
// from one run rather than across two:
//
//	go test ./graph/lpg/ -run '^$' -bench 'BenchmarkVacuumCommitTail' -count=8 | tee tail.txt
//	benchstat -col /arm tail.txt
func BenchmarkVacuumCommitTail(b *testing.B) {
	for _, writers := range []int{1, 8} {
		for _, arm := range []struct {
			name     string
			onCommit bool
		}{{"async", false}, {"sync", true}} {
			b.Run(fmt.Sprintf("writers=%d/arm=%s", writers, arm.name), func(b *testing.B) {
				benchVacuumCommitTail(b, arm.onCommit, benchTailNodes, writers)
			})
		}
	}
}

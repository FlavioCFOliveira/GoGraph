package cypher

// reader_under_write_tx_bench_test.go — characterisation benchmark for audit
// finding F7 (#1836): quantify the reader tail latency caused by the
// engine-wide visibility barrier a write [ExplicitTx] holds for its entire
// lifetime (head-of-line blocking).
//
// The #1671 closure that deferred the copy-on-write cure measured readers under
// a STEADY SMALL writer, not a single write transaction held open across
// simulated round-trips. This benchmark fills that gap: it measures a scanning
// read query's latency (Engine.Run, which reads under lpg.Graph.View -> the
// barrier's RLock) in two modes — no concurrent writer, and a background writer
// that holds a write ExplicitTx open for a fixed think-time each cycle — and
// reports p50/p99. The delta is the head-of-line-blocking tail the ExplicitTx
// operational-contract godoc warns about.
//
// It is a benchmark, so it does NOT run in the gated short test layer; it exists
// to be run on demand (go test -bench=ReaderLatencyUnderHeldWriteTx ./cypher/)
// when characterising the read/write tail before a release.

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func benchBuildReaderGraph(b *testing.B, n int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			b.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	return g
}

func reportReaderPercentiles(b *testing.B, lat []time.Duration) {
	if len(lat) == 0 {
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) float64 {
		idx := int(q * float64(len(lat)-1))
		return float64(lat[idx].Microseconds())
	}
	b.ReportMetric(p(0.50), "p50_us")
	b.ReportMetric(p(0.99), "p99_us")
	b.ReportMetric(float64(lat[len(lat)-1].Microseconds()), "max_us")
}

func BenchmarkReaderLatencyUnderHeldWriteTx(b *testing.B) {
	g := benchBuildReaderGraph(b, 2000)
	e := NewEngine(g)
	const readQ = `MATCH (n) RETURN n.v AS v` // scanning read: runs under the barrier RLock

	run := func(b *testing.B, writerHold time.Duration) {
		var (
			stop chan struct{}
			wg   sync.WaitGroup
		)
		if writerHold > 0 {
			stop = make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					tx, err := e.BeginTx(context.Background())
					if err != nil {
						time.Sleep(time.Millisecond)
						continue
					}
					// Hold the visibility barrier open across a simulated
					// client round-trip / think-time, then commit.
					time.Sleep(writerHold)
					_ = tx.Commit()
				}
			}()
		}

		lat := make([]time.Duration, 0, b.N)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			t0 := time.Now()
			res, err := e.Run(context.Background(), readQ, nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() { //nolint:revive // drain
			}
			if err := res.Err(); err != nil {
				b.Fatal(err)
			}
			_ = res.Close()
			lat = append(lat, time.Since(t0))
		}
		b.StopTimer()
		if stop != nil {
			close(stop)
			wg.Wait()
		}
		reportReaderPercentiles(b, lat)
	}

	b.Run("no-writer", func(b *testing.B) { run(b, 0) })
	b.Run("held-writer-2ms", func(b *testing.B) { run(b, 2*time.Millisecond) })
	b.Run("held-writer-10ms", func(b *testing.B) { run(b, 10*time.Millisecond) })
}

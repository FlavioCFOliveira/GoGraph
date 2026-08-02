package mvcc

// commitlog_bench_test.go — what the contiguous frontier costs, measured
// (rmp #2298, acceptance criterion 4).
//
// The claim being tested is that the READ path does not move: [Clock.ReadTS] is
// still one atomic load and [Visible] is still one comparison, because the whole
// change lives on the publish side. That is a structural claim, but a structural
// claim about performance is still a claim, so it is measured.
//
// The legacy arm is carried in this file rather than reached by checking out the
// previous commit, so both implementations run in ONE process against the same
// allocator, the same CPU state and the same run-to-run conditions. A
// back-to-back A/B across two builds on this project's hardware has manufactured
// phantom regressions from a byte-identical control before, which is why
// graph/lpg carries EnableMVCC/DisableMVCC for exactly this purpose.

import (
	"sync/atomic"
	"testing"
)

// legacyPublish is the single-watermark implementation the commit log replaces:
// a monotone CAS raising the visible instant straight to ts. It is correct only
// while commits are serialised, which is the finding rmp #2298 exists to fix; it
// survives here solely as the benchmark's control arm.
func legacyPublish(visible *atomic.Uint64, ts uint64) {
	for {
		cur := visible.Load()
		if cur >= ts {
			return
		}
		if visible.CompareAndSwap(cur, ts) {
			return
		}
	}
}

// BenchmarkReadTS is the reader's cost: one atomic load, taken once per query
// in [lpg.Graph.BeginRead]. It is the number the change must not move.
func BenchmarkReadTS(b *testing.B) {
	var c Clock
	for i := 0; i < 1000; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink uint64
	for i := 0; i < b.N; i++ {
		sink += c.ReadTS()
	}
	readTSSink.Store(sink)
}

var readTSSink atomic.Uint64

// BenchmarkVisible is the per-version-chain-node cost — the test that runs on
// every versioned read, for every version walked. Unchanged by this task, and
// measured to prove it.
func BenchmarkVisible(b *testing.B) {
	cases := []struct {
		name              string
		ts, startTS, txID uint64
	}{
		{"committed/visible", 100, 500, 0},
		{"committed/invisible", 900, 500, 0},
		{"own-write", TxIDBase + 7, 500, TxIDBase + 7},
		{"other-in-flight", TxIDBase + 9, 500, TxIDBase + 7},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			n := 0
			for i := 0; i < b.N; i++ {
				if Visible(tc.ts, tc.startTS, tc.txID) {
					n++
				}
			}
			visibleSink.Store(int64(n))
		})
	}
}

var visibleSink atomic.Int64

// BenchmarkPublish is where the cost actually went: once per commit, on the
// write path. Both arms run here so the difference is a measurement rather than
// a comparison of two runs.
//
// The frontier arm takes a mutex and sets a bit; the legacy arm is an
// uncontended CAS. In-order publication is the normal case — it is what a
// serialised writer produces today and what a well-behaved concurrent writer
// produces most of the time — and out-of-order is the case the commit log
// exists for, where the frontier arm additionally walks the bitmap.
func BenchmarkPublish(b *testing.B) {
	b.Run("frontier/in-order", func(b *testing.B) {
		var c Clock
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			c.PublishCommitTS(c.NextCommitTS())
		}
	})
	b.Run("legacy/in-order", func(b *testing.B) {
		var (
			commit  atomic.Uint64
			visible atomic.Uint64
		)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			legacyPublish(&visible, commit.Add(1))
		}
	})
	// Out of order by a window of 8 — eight writers inside their commit
	// sections at once, which is the shape this task exists to make safe. The
	// legacy arm is not run here: it does not produce a correct frontier at all
	// under this input, so a number for it would compare different answers.
	b.Run("frontier/out-of-order-window-8", func(b *testing.B) {
		const window = 8
		var c Clock
		pending := make([]uint64, 0, window)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pending = append(pending, c.NextCommitTS())
			if len(pending) == window {
				// Publish the window in reverse, so every publication but the
				// last leaves a hole and only the last advances the frontier.
				for j := len(pending) - 1; j >= 0; j-- {
					c.PublishCommitTS(pending[j])
				}
				pending = pending[:0]
			}
		}
		for _, ts := range pending {
			c.PublishCommitTS(ts)
		}
	})
}

// BenchmarkPublishConcurrent measures the publish path under real contention,
// which is the regime the sprint is moving towards: the frontier arm serialises
// publishers on pubMu, the legacy arm on the CAS retry loop. Neither is free,
// and this is what says by how much they differ.
func BenchmarkPublishConcurrent(b *testing.B) {
	b.Run("frontier", func(b *testing.B) {
		var c Clock
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c.PublishCommitTS(c.NextCommitTS())
			}
		})
	})
	b.Run("legacy", func(b *testing.B) {
		var (
			commit  atomic.Uint64
			visible atomic.Uint64
		)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				legacyPublish(&visible, commit.Add(1))
			}
		})
	})
}

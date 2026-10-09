package server

// txregistry_order_test.go — the oldest-first contract of txRegistry.list and
// what it costs (rmp #2562).
//
// list() used to insertion-sort its result. The map it ranges has a random
// iteration order, so with distinct start instants — which a real clock always
// produces — the input is a genuine permutation and the sort was quadratic:
// 600 µs per call at 512 open transactions. These tests pin the order at a size
// where a regression to a quadratic sort is measurable by the benchmark below,
// and keep the same-instant arrangement in both, because that degenerate case is
// the one that once hid the cost (every comparison false, zero swaps).
//
// Layer: short.

import (
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// fillTxRegistry registers n entries on a fresh registry driven by a fake clock.
// When distinct is set the clock advances between registrations, so every entry
// carries its own StartedAt; otherwise all n share one instant.
func fillTxRegistry(tb testing.TB, n int, distinct bool) *txRegistry {
	tb.Helper()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	r := newTxRegistry(clk)
	for i := range n {
		r.register(newTxEntry(fmt.Sprintf("tx-%d", i), "p", "r", "w", "TX_READY", nil))
		if distinct {
			clk.Advance(time.Millisecond)
		}
	}
	return r
}

// TestTxRegistryList_OldestFirst asserts that a listing holds every open entry
// exactly once and in non-decreasing StartedAt order, for both arrangements.
func TestTxRegistryList_OldestFirst(t *testing.T) {
	const n = 1024
	for _, distinct := range []bool{false, true} {
		t.Run(fmt.Sprintf("distinct=%t", distinct), func(t *testing.T) {
			got := fillTxRegistry(t, n, distinct).list()
			if len(got) != n {
				t.Fatalf("list returned %d entries, want %d", len(got), n)
			}
			ids := make(map[string]bool, n)
			instants := make(map[time.Time]bool, n)
			for i := range got {
				if ids[got[i].ID] {
					t.Fatalf("entry %q listed twice", got[i].ID)
				}
				ids[got[i].ID] = true
				instants[got[i].StartedAt] = true
				if i > 0 && got[i].StartedAt.Before(got[i-1].StartedAt) {
					t.Fatalf("not oldest-first at %d: %v after %v", i, got[i].StartedAt, got[i-1].StartedAt)
				}
			}
			// The arrangement itself, verified: a "distinct" run whose instants had
			// collided would test the degenerate order and nothing else.
			want := 1
			if distinct {
				want = n
			}
			if len(instants) != want {
				t.Fatalf("%d distinct instants, want %d", len(instants), want)
			}
		})
	}
}

// BenchmarkTxRegistryList measures one listing at each size, with distinct and
// with shared start instants. The distinct column is the one that was quadratic;
// the shared column is kept so a future change that is cheap only on degenerate
// input stays visible.
func BenchmarkTxRegistryList(b *testing.B) {
	for _, distinct := range []bool{true, false} {
		arrangement := "same-instant"
		if distinct {
			arrangement = "distinct-instants"
		}
		for _, n := range []int{8, 64, 256, 512, 1024} {
			b.Run(fmt.Sprintf("%s/open=%d", arrangement, n), func(b *testing.B) {
				r := fillTxRegistry(b, n, distinct)
				b.ReportAllocs()
				for b.Loop() {
					if got := len(r.list()); got != n {
						b.Fatalf("list returned %d entries, want %d", got, n)
					}
				}
			})
		}
	}
}

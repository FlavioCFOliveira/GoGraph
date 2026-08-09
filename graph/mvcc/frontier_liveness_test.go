package mvcc

// frontier_liveness_test.go — the frontier must reach the last published instant in
// EVERY publication order (rmp #2362).
//
// # Why this exists before the fast path, not after
//
// rmp #2362 adds a lock-free fast path in front of pubMu: when the frontier is already
// at ts-1, CAS it to ts and skip the lock. Its technical requirements demand a test
// that "mixes in-order and out-of-order publication and asserts the frontier reaches
// the final timestamp in every interleaving" BEFORE the optimisation, because getting
// it wrong stalls visibility permanently rather than slowly — writes keep succeeding
// and readers stop seeing them, which is the rmp #2309 failure mode.
//
// The analysis in docs/mvcc-publish-fast-path.md found that the ONE-condition guard
// the task proposed does exactly that. commitLog.finish with ts == oldest calls
// advance(), which walks over every contiguous set bit and can jump the frontier by
// many; a fast path that advances by one leaves the rest behind:
//
//	ts+1 … ts+5 finish out of order   (bits set, frontier unmoved)
//	ts finishes via the fast path      (frontier -> ts, not ts+5)
//	no further commit arrives          => five acknowledged commits invisible for ever
//
// So the guard needs a second condition (nothing pending above ts). These tests are
// the oracle for that work: they pass against the current locked implementation and
// must keep passing against any fast path, which is what makes them worth having now.
//
// EXHAUSTIVE, not sampled. Every permutation of a small window is enumerated rather
// than a few orders tried at random, because the stalling orders are precisely the
// out-of-order ones and a sampler can miss them.

import (
	"testing"
)

// TestClock_FrontierReachesTheLastInstantInEveryOrder enumerates EVERY permutation of
// the publication order for a window of allocated timestamps and asserts that once all
// of them have finished, the frontier equals the highest.
//
// This is the liveness property the whole substrate rests on: a commit that has
// finished must eventually be visible, whatever order its peers finished in.
func TestClock_FrontierReachesTheLastInstantInEveryOrder(t *testing.T) {
	for _, n := range []int{2, 3, 4, 5} {
		t.Run("window="+windowName(n), func(t *testing.T) {
			perms := permutations(n)
			if len(perms) != factorial(n) {
				t.Fatalf("enumerated %d permutations for n=%d, want %d — the generator is "+
					"wrong and the exhaustiveness claim is false", len(perms), n, factorial(n))
			}
			for _, order := range perms {
				var c Clock
				ts := make([]uint64, n)
				for i := range ts {
					ts[i] = c.NextCommitTS()
				}
				last := ts[n-1]

				for _, idx := range order {
					c.PublishCommitTS(ts[idx])
				}

				if got := c.ReadTS(); got != last {
					t.Fatalf("order %v: frontier = %d after every commit finished, want %d. "+
						"The frontier is STUCK: commits are durable and acknowledged but no "+
						"reader will ever see them", order, got, last)
				}
				if inflight := c.InFlightCommits(); inflight != 0 {
					t.Fatalf("order %v: InFlightCommits = %d after every commit finished, want 0",
						order, inflight)
				}
			}
		})
	}
}

// TestClock_FrontierIsMonotonicUnderOutOfOrderPublication asserts the frontier never
// moves BACKWARDS as commits finish in an arbitrary order.
//
// A fast path that stores instead of comparing could regress it, and a reader that saw
// an instant and then saw an earlier one would observe a state no serial order
// produced. Separate from the liveness test because it fails differently: liveness is
// about the final value, this is about every intermediate one.
func TestClock_FrontierIsMonotonicUnderOutOfOrderPublication(t *testing.T) {
	const n = 5
	for _, order := range permutations(n) {
		var c Clock
		ts := make([]uint64, n)
		for i := range ts {
			ts[i] = c.NextCommitTS()
		}
		prev := c.ReadTS()
		for _, idx := range order {
			c.PublishCommitTS(ts[idx])
			got := c.ReadTS()
			if got < prev {
				t.Fatalf("order %v: frontier went BACKWARDS, %d -> %d after publishing %d",
					order, prev, got, ts[idx])
			}
			prev = got
		}
	}
}

// TestClock_FrontierNeverExceedsTheContiguousPrefix is the safety half, and it is what
// the fast path is most likely to break.
//
// The frontier may only reach an instant when EVERY instant below it has finished.
// Publishing out of order must leave it behind: with 1 and 3 finished but not 2, a
// reader may start at 1 and no higher, or it would see commit 3 while commit 2 is
// still in flight — a state no serial order produced.
func TestClock_FrontierNeverExceedsTheContiguousPrefix(t *testing.T) {
	var c Clock
	one, two, three := c.NextCommitTS(), c.NextCommitTS(), c.NextCommitTS()

	c.PublishCommitTS(three)
	if got := c.ReadTS(); got != 0 {
		t.Fatalf("frontier = %d with only %d finished; 1 and 2 are still in flight so it "+
			"must be 0", got, three)
	}
	c.PublishCommitTS(one)
	if got := c.ReadTS(); got != one {
		t.Fatalf("frontier = %d after finishing %d, want %d — %d is finished but %d is not, "+
			"so the frontier stops at %d", got, one, one, three, two, one)
	}
	// Now the gap closes and the frontier must jump PAST two, all the way to three.
	// A fast path advancing by one would stop at two and leave three invisible.
	c.PublishCommitTS(two)
	if got := c.ReadTS(); got != three {
		t.Fatalf("frontier = %d after the gap closed, want %d. It must JUMP over every "+
			"already-finished instant, not advance by one — advancing by one is exactly the "+
			"stall rmp #2362's one-condition fast path would cause", got, three)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// permutations returns every ordering of [0,n).
func permutations(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	var rec func(k int)
	rec = func(k int) {
		if k == n {
			cp := make([]int, n)
			copy(cp, idx)
			out = append(out, cp)
			return
		}
		for i := k; i < n; i++ {
			idx[k], idx[i] = idx[i], idx[k]
			rec(k + 1)
			idx[k], idx[i] = idx[i], idx[k]
		}
	}
	rec(0)
	return out
}

// windowName formats the window size for a subtest name without colliding with the
// package's existing itoa helper.
func windowName(n int) string { return string(rune('0' + n)) }

func factorial(n int) int {
	f := 1
	for i := 2; i <= n; i++ {
		f *= i
	}
	return f
}

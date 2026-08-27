package mvcc

// frontier_pinning_test.go — the contiguous frontier stalls behind ONE in-flight
// commit while later commits finish above it (rmp #2369).
//
// This is the root cause the task told me to re-verify independently before
// building anything on it, and to verify at the primitive level: seven
// hypotheses had already been refuted in this area, so the mechanism had to be
// asserted on a bare Clock rather than inferred from an engine-level symptom.
//
// It is NOT a defect. Snapshot isolation is exactly what this produces, and
// cypher.Session exists to give a caller read-your-own-writes on top of it. What
// this test establishes is that the HAZARD IS REAL AND REACHABLE — that a
// finished, acknowledged commit can be invisible to a fresh reader — which is
// what makes the Session guarantee worth testing at all. Without it, a Session
// test that observes no stale read would be indistinguishable from one running
// on a graph where the hazard never arises.

import "testing"

// TestClock_FrontierStallsBehindOneInFlightCommit is deterministic: no
// goroutines, no timing, no load. It orders the operations by hand.
func TestClock_FrontierStallsBehindOneInFlightCommit(t *testing.T) {
	var c Clock

	// Three commits are allocated in order. The first stays IN FLIGHT.
	older := c.NextCommitTS()
	mid := c.NextCommitTS()
	newer := c.NextCommitTS()

	base := c.ReadTS()

	// The two later commits finish and are acknowledged to their callers.
	c.PublishCommitTS(mid)
	c.PublishCommitTS(newer)

	if got := c.InFlightCommits(); got == 0 {
		t.Fatalf("InFlightCommits = 0 while commit %d has not finished; the fixture is not in "+
			"the state this test exists to describe", older)
	}

	// The frontier has NOT moved, although two commits above it are finished and
	// acknowledged. Every transaction starting now inherits this value as its
	// startTS and cannot see either of them.
	if got := c.ReadTS(); got != base {
		t.Fatalf("ReadTS advanced from %d to %d while commit %d is still in flight; the "+
			"contiguous frontier must not pass an unfinished commit", base, got, older)
	}
	if c.ReadTS() >= mid {
		t.Fatalf("the frontier %d reached the finished commit %d; that would make a snapshot "+
			"pinned above an unfinished commit, which is the one thing this design refuses to do",
			c.ReadTS(), mid)
	}

	// Finish the straggler: the frontier now jumps past ALL THREE at once, which
	// is what makes the stall a lag rather than a loss.
	c.PublishCommitTS(older)

	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits = %d once every commit finished, want 0", got)
	}
	if got := c.ReadTS(); got < newer {
		t.Fatalf("ReadTS = %d after every commit finished, want at least %d; the frontier must "+
			"catch up to the newest contiguous commit, not merely to the one that was blocking",
			got, newer)
	}
}

// TestClock_FrontierIsNotPinnedWithoutAnInFlightCommit is the CONTROL. Without
// it, the test above is satisfied by a clock whose frontier never advances at
// all — which would be a far worse defect and would look identical.
func TestClock_FrontierIsNotPinnedWithoutAnInFlightCommit(t *testing.T) {
	var c Clock

	first := c.NextCommitTS()
	c.PublishCommitTS(first)
	if got := c.ReadTS(); got < first {
		t.Fatalf("ReadTS = %d after a single commit finished, want at least %d; with nothing in "+
			"flight the frontier must advance immediately", got, first)
	}

	second := c.NextCommitTS()
	c.PublishCommitTS(second)
	if got := c.ReadTS(); got < second {
		t.Fatalf("ReadTS = %d after the second commit finished, want at least %d", got, second)
	}
}

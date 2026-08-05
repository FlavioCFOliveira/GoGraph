package mvcc

// ratchet_test.go — rmp #2309: restoring a derived clock must leave the frontier
// ABLE TO MOVE, not merely positioned correctly.
//
// Layer: short.
//
// # The defect these pin, and why it needed its own test
//
// [Clock.RatchetTo] restores the clock at recovery. Its first version raised the two
// atomics — the allocation counter and the visible frontier — and stopped there. But
// the CONTIGUITY that produces the frontier lives in [commitLog.oldest], and a log
// that still believes timestamp 1 is unfinished computes a frontier of 0 for ever.
//
// Since [Clock.finishCommitTS] only ever RAISES visible, the frontier could then
// never move past the ratcheted value again: every commit after recovery allocated
// a timestamp, set its bit, failed to advance `oldest`, and stayed INVISIBLE for the
// life of the process. Writes kept succeeding; readers never saw them.
//
// internal/sim caught it, but as NODE LOSS against its oracle (21 nodes expected, 15
// present) in a full-stack crash-recovery scenario — a symptom that looks nothing
// like a clock defect and took a bisect over four commits to attribute. These tests
// state the mechanism directly, so the next regression is named rather than
// discovered three layers away.

import "testing"

// TestClock_RatchetKeepsTheFrontierMovable is the direct regression: after a
// ratchet, an ordinary allocate-and-publish must make its instant visible.
func TestClock_RatchetKeepsTheFrontierMovable(t *testing.T) {
	var c Clock
	const floor = 1000

	c.RatchetTo(floor)
	if got := c.ReadTS(); got != floor {
		t.Fatalf("ReadTS = %d immediately after RatchetTo(%d), want %d", got, floor, floor)
	}

	// The ordinary commit cycle, exactly as a writer performs it.
	ts := c.NextCommitTS()
	if ts != floor+1 {
		t.Fatalf("NextCommitTS after RatchetTo(%d) = %d, want %d", floor, ts, floor+1)
	}
	c.PublishCommitTS(ts)

	if got := c.ReadTS(); got != ts {
		t.Fatalf("ReadTS = %d after publishing %d, want %d. The frontier is STUCK: the "+
			"commit log was left believing timestamp 1 is unfinished, so it computes a "+
			"frontier of 0 and finishCommitTS — which only ever raises visible — can "+
			"never move it again. Every commit after recovery is invisible for the "+
			"life of the process, while the writes themselves keep succeeding",
			got, ts, ts)
	}
	if n := c.InFlightCommits(); n != 0 {
		t.Fatalf("InFlightCommits = %d after the commit finished, want 0", n)
	}
}

// TestClock_RatchetThenManyCommitsStayVisible walks the frontier well past the
// ratchet, including across the commit log's block boundary, so a rebase that
// merely papered over the first commit would still fail.
func TestClock_RatchetThenManyCommitsStayVisible(t *testing.T) {
	var c Clock
	const floor = 4090 // just below clIDsPerBlock, so the run crosses a block retire

	c.RatchetTo(floor)
	for i := 0; i < 20; i++ {
		ts := c.NextCommitTS()
		c.PublishCommitTS(ts)
		if got := c.ReadTS(); got != ts {
			t.Fatalf("commit %d: ReadTS = %d after publishing %d — the frontier stopped "+
				"tracking the clock %d commits after the ratchet", i, got, ts, i)
		}
	}
}

// TestClock_RatchetIsMonotone pins that a floor below the clock cannot rewind it,
// including the contiguity: a rebase to a LOWER floor would make already-finished
// timestamps look unfinished and stall the frontier just as badly.
func TestClock_RatchetIsMonotone(t *testing.T) {
	var c Clock
	for i := 0; i < 5; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}
	high := c.ReadTS()

	c.RatchetTo(1)
	if got := c.ReadTS(); got != high {
		t.Fatalf("ReadTS = %d after RatchetTo(1) over a clock at %d, want %d: the "+
			"ratchet must raise and never lower", got, high, high)
	}
	// And the frontier still moves.
	ts := c.NextCommitTS()
	c.PublishCommitTS(ts)
	if got := c.ReadTS(); got != ts {
		t.Fatalf("ReadTS = %d after publishing %d following a no-op ratchet, want %d: "+
			"the no-op path rebased the log backwards and stalled the frontier",
			got, ts, ts)
	}
}

// TestClock_RatchetOnAFreshClockBehavesLikeNoCommits covers the zero value, which is
// the state every reopened graph starts from.
func TestClock_RatchetOnAFreshClockBehavesLikeNoCommits(t *testing.T) {
	var c Clock
	c.RatchetTo(0) // a WAL with no instants: nothing to restore
	if got := c.ReadTS(); got != 0 {
		t.Fatalf("ReadTS = %d after RatchetTo(0), want 0", got)
	}
	ts := c.NextCommitTS()
	if ts != 1 {
		t.Fatalf("first commit after RatchetTo(0) = %d, want 1", ts)
	}
	c.PublishCommitTS(ts)
	if got := c.ReadTS(); got != 1 {
		t.Fatalf("ReadTS = %d after the first commit, want 1", got)
	}
}

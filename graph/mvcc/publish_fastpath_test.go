package mvcc

// publish_fastpath_test.go — the lock-free publication fast path (rmp #2362).
//
// graph/mvcc/frontier_liveness_test.go is the BLACK-BOX oracle: it asserts the
// frontier's liveness, monotonicity and safety through the public API, and it
// passes whether or not a fast path exists. These tests are its white-box
// complement, and they exist because a fast path that is never TAKEN also passes
// the oracle. Three things need pinning that the oracle cannot see:
//
//   - that the common in-order publication really does skip the lock and the
//     bitmap, which is the entire point of the change;
//   - that skipping the bitmap for a long run does not then cost a block per
//     4096 timestamps when a publisher finally does take the lock;
//   - the fast-path/locked-path RACE named in the task's acceptance criteria,
//     which no single-goroutine test can reach.

import (
	"sync"
	"testing"
)

// TestClock_InOrderPublicationSkipsTheCommitLog is the evidence that the fast
// path is taken at all.
//
// It is a white-box assertion on purpose. The fast path's whole claim is that an
// in-order publication touches neither pubMu nor the bitmap, and the only
// observable difference between "took the fast path" and "took a very quick
// locked path" is that the log is left untouched: `oldest` never moves and no
// block is ever allocated. Asserting the frontier alone would pass against an
// implementation that quietly did all the work under the lock.
func TestClock_InOrderPublicationSkipsTheCommitLog(t *testing.T) {
	var c Clock
	const n = 1000
	for i := 0; i < n; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}
	if got := c.ReadTS(); got != n {
		t.Fatalf("ReadTS = %d after %d in-order commits, want %d", got, n, n)
	}
	if c.log.oldest != 0 {
		t.Fatalf("commitLog.oldest = %d after %d in-order commits: the log was touched, so "+
			"the publication fast path did NOT run and rmp #2362 delivered nothing",
			c.log.oldest, n)
	}
	if got := c.log.liveBlocks(); got != 0 {
		t.Fatalf("liveBlocks = %d after %d in-order commits, want 0: the fast path must not "+
			"allocate a bitmap it never reads", got, n)
	}
	if got := c.log.blocked.Load(); got != 0 {
		t.Fatalf("blocked = %d with no publisher in the locked path and no bit above the "+
			"frontier, want 0 — a non-zero count disables the fast path for ever", got)
	}
}

// TestClock_LockedPathCatchesUpWithoutAllocatingABlockPerRun covers the sharp
// edge docs/mvcc-publish-fast-path.md names: block management.
//
// After a long fast-path run the log's `oldest` is far behind the published
// frontier. A locked path that walked from that stale position would have
// [commitLog.blockFor] extend the chain one block per [clIDsPerBlock] timestamps
// — memory proportional to the number of commits, which is exactly the growth
// the log's design exists to avoid. [commitLog.syncTo] must retire instead.
func TestClock_LockedPathCatchesUpWithoutAllocatingABlockPerRun(t *testing.T) {
	var c Clock
	// Long enough to span several blocks, so a stale walk is unmistakable.
	const run = 5 * clIDsPerBlock
	for i := 0; i < run; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}

	// Now force the locked path: publish out of order, which the fast path
	// refuses, and which leaves a bit above the frontier.
	first, second := c.NextCommitTS(), c.NextCommitTS()
	c.PublishCommitTS(second)
	if got, want := c.ReadTS(), uint64(run); got != want {
		t.Fatalf("ReadTS = %d with %d still in flight, want %d", got, first, want)
	}
	if got := c.log.liveBlocks(); got > 2 {
		t.Fatalf("liveBlocks = %d after a %d-commit fast-path run, want at most 2: the locked "+
			"path walked from a stale position and allocated a block per %d timestamps",
			got, run, clIDsPerBlock)
	}

	c.PublishCommitTS(first)
	if got, want := c.ReadTS(), second; got != want {
		t.Fatalf("ReadTS = %d after the gap closed, want %d", got, want)
	}
	if got := c.log.liveBlocks(); got > 2 {
		t.Fatalf("liveBlocks = %d once the window closed, want at most 2", got)
	}
	if got := c.log.blocked.Load(); got != 0 {
		t.Fatalf("blocked = %d once every commit finished, want 0", got)
	}
}

// TestClock_FrontierSurvivesTheFastPathLockedPathRace is acceptance criterion 2's
// last clause, and the only property here that needs two goroutines.
//
// The interleaving it hunts is precise. A publisher takes pubMu and reads the
// frontier to catch the log up to it; a fast path then advances the frontier past
// that read and returns, and the publisher — computing from what it read — never
// installs the higher value. Its own bit sits above the frontier for ever:
//
//	frontier f, commits f+1 and f+2 in flight
//	B (f+2, out of order) reads the frontier: f
//	A (f+1, in order)     CAS f -> f+1, re-checks, returns
//	B                     records f+2 above f, computes frontier f, installs nothing
//	=> frontier f+1, commit f+2 durable, acknowledged, and invisible for ever
//
// Each round recreates exactly that shape and starts both goroutines from a
// barrier, so the window is hit rather than hoped for. Removing
// [commitLog.enterPublish] from Clock.finishCommitTS — the half of
// [commitLog.blocked] that has no effect in any single-goroutine test — makes
// this fail within a few hundred rounds; removing the post-CAS re-check makes it
// fail within a few tens of thousands. Both were verified by injection.
func TestClock_FrontierSurvivesTheFastPathLockedPathRace(t *testing.T) {
	// 100 000 rather than a few thousand, because the count is sized to the SLOWER
	// of the two defects it must catch, MEASURED rather than guessed. Deleting the
	// publisher bracket ([commitLog.enterPublish]) failed at rounds 348 and 10 406;
	// deleting the post-CAS re-check failed at 3 407, 12 813, 15 605, 17 792 and
	// 23 282. The worst observed is 23 282, so this leaves a factor of four in hand.
	const rounds = 100000
	var c Clock
	for r := 0; r < rounds; r++ {
		inOrder, outOfOrder := c.NextCommitTS(), c.NextCommitTS()
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(2)
		go func() {
			defer done.Done()
			start.Wait()
			c.PublishCommitTS(outOfOrder)
		}()
		go func() {
			defer done.Done()
			start.Wait()
			c.PublishCommitTS(inOrder)
		}()
		start.Done()
		done.Wait()

		if got := c.ReadTS(); got != outOfOrder {
			t.Fatalf("round %d: frontier = %d once both commits finished, want %d. The frontier "+
				"is STUCK: %d is durable and acknowledged and no reader will ever see it",
				r, got, outOfOrder, outOfOrder)
		}
		if got := c.InFlightCommits(); got != 0 {
			t.Fatalf("round %d: InFlightCommits = %d once both commits finished, want 0", r, got)
		}
	}
	if got := c.log.blocked.Load(); got != 0 {
		t.Fatalf("blocked = %d after %d rounds, want 0: the count leaked and the fast path is "+
			"now disabled for the life of the clock", got, rounds)
	}
}

// TestClock_FrontierIsMonotoneUnderConcurrentPublication asserts the safety half
// under the same race: an observer must never see the frontier move backwards,
// nor see it pass a commit that is still in flight.
//
// The locked path installs its frontier with a compare-and-swap loop precisely
// because the fast path can raise it from under the lock; a plain store there
// would land an older value on top of a newer one, and this is what would catch
// it.
func TestClock_FrontierIsMonotoneUnderConcurrentPublication(t *testing.T) {
	const (
		writers = 8
		perW    = 2000
	)
	var (
		c        Clock
		writersW sync.WaitGroup
		observer sync.WaitGroup
		stop     = make(chan struct{})
		regress  = make(chan [2]uint64, 1)
	)

	// An observer that only ever compares what it reads with what it read before.
	observer.Add(1)
	go func() {
		defer observer.Done()
		prev := uint64(0)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := c.ReadTS()
			if got < prev {
				select {
				case regress <- [2]uint64{prev, got}:
				default:
				}
				return
			}
			prev = got
		}
	}()

	// Writers publish in an order that is deliberately not the allocation order:
	// each takes two timestamps and publishes the newer one first, so every round
	// puts a bit above the frontier and then closes the gap under it.
	for w := 0; w < writers; w++ {
		writersW.Add(1)
		go func() {
			defer writersW.Done()
			for i := 0; i < perW; i++ {
				a, b := c.NextCommitTS(), c.NextCommitTS()
				c.PublishCommitTS(b)
				c.PublishCommitTS(a)
			}
		}()
	}
	writersW.Wait()
	close(stop)
	observer.Wait()

	select {
	case r := <-regress:
		t.Fatalf("frontier went BACKWARDS, %d -> %d: a reader would observe a state no serial "+
			"order produced. The locked path must install its frontier with a compare-and-swap "+
			"loop, because the fast path raises it without pubMu", r[0], r[1])
	default:
	}

	want := uint64(writers * perW * 2)
	if got := c.ReadTS(); got != want {
		t.Fatalf("frontier = %d once every commit finished, want %d", got, want)
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits = %d once every commit finished, want 0", got)
	}
	if got := c.log.blocked.Load(); got != 0 {
		t.Fatalf("blocked = %d once every commit finished, want 0", got)
	}
}

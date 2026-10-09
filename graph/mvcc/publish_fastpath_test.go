package mvcc

// publish_fastpath_test.go — the in-order publication fast path (rmp #2362) and,
// since rmp #2932, the lock-free commit registry behind it.
//
// graph/mvcc/frontier_liveness_test.go is the BLACK-BOX oracle: it asserts the
// frontier's liveness, monotonicity and safety through the public API, and it
// passes whether or not a fast path exists. These tests are its white-box
// complement, and they exist because a fast path that is never TAKEN also passes
// the oracle. Three things need pinning that the oracle cannot see:
//
//   - that the common in-order publication really does skip the slow path, which
//     is the entire point of the fast path;
//   - that a long in-order run followed by out-of-order publication resumes from
//     the right place;
//   - the race between an in-order publication and an out-of-order one, which no
//     single-goroutine test can reach.

import (
	"sync"
	"testing"
)

// TestClock_InOrderPublicationSkipsTheRing is the evidence that the fast path is
// taken at all.
//
// It is a white-box assertion on purpose. The only observable difference between
// "took the fast path" and "took a very quick slow path" is that no record is
// marked finished and none is published on another's behalf. Asserting the
// frontier alone would pass against an implementation that quietly took the slow
// path for every publication.
func TestClock_InOrderPublicationSkipsTheSlowPath(t *testing.T) {
	var c Clock
	const n = 1000
	for i := 0; i < n; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}
	if got := c.ReadTS(); got != n {
		t.Fatalf("ReadTS = %d after %d in-order commits, want %d", got, n, n)
	}
	if got := c.OutOfOrderPublications(); got != 0 {
		t.Fatalf("OutOfOrderPublications = %d after %d in-order commits, want 0", got, n)
	}
	if got := c.HelpedPublications(); got != 0 {
		t.Fatalf("HelpedPublications = %d after %d in-order commits, want 0", got, n)
	}
}

// TestClock_OutOfOrderAfterALongInOrderRun covers the seam between the two paths:
// after a long fast-path run, the first out-of-order publication must find its
// record and the frontier must resume exactly where the fast path left it.
func TestClock_OutOfOrderAfterALongInOrderRun(t *testing.T) {
	var c Clock
	const run = 5 * registrySlots // several laps' worth of timestamps
	for i := 0; i < run; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}

	first, second := c.NextCommitTS(), c.NextCommitTS()
	c.PublishCommitTS(second)
	if got, want := c.ReadTS(), uint64(run); got != want {
		t.Fatalf("ReadTS = %d with %d still in flight, want %d", got, first, want)
	}
	table := c.reg.slots.Load()

	c.PublishCommitTS(first)
	if got, want := c.ReadTS(), second; got != want {
		t.Fatalf("ReadTS = %d after the gap closed, want %d", got, want)
	}
	if c.reg.slots.Load() != table {
		t.Fatal("the registry was reallocated")
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits = %d once every commit finished, want 0", got)
	}
}

// TestClock_FrontierSurvivesTheFastPathLockedPathRace is acceptance criterion 2's
// last clause, and the only property here that needs two goroutines.
//
// The interleaving it hunts is precise. An out-of-order publication marks its
// record finished while the in-order publication below it advances the frontier; if
// neither then carries the frontier over the recorded timestamp, it sits above
// the frontier for ever:
//
//	frontier f, commits f+1 and f+2 in flight
//	B (f+2, out of order) marks f+2 finished, reads the frontier: f, nothing to carry
//	A (f+1, in order)     CAS f -> f+1, does not look at f+2, returns
//	=> frontier f+1, commit f+2 durable, acknowledged, and invisible for ever
//
// Each round recreates exactly that shape and starts both goroutines from a
// barrier, so the window is hit rather than hoped for. Both halves of the
// pairing are load-bearing; see the injection record in the rmp #2932 report.
func TestClock_FrontierSurvivesTheFastPathLockedPathRace(t *testing.T) {
	// 100 000 rather than a few thousand, because the count is sized to the SLOWER
	// of the defects it must catch, MEASURED rather than guessed: the locked design
	// rmp #2932 replaced had an injected defect that survived to round 23 282, so
	// this keeps a wide margin.
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
			"order produced. Every advance of the frontier must be a compare-and-swap, because "+
			"every publication raises it without a lock", r[0], r[1])
	default:
	}

	want := uint64(writers * perW * 2)
	if got := c.ReadTS(); got != want {
		t.Fatalf("frontier = %d once every commit finished, want %d", got, want)
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits = %d once every commit finished, want 0", got)
	}
}

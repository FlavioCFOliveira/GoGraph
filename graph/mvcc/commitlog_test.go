package mvcc

// commitlog_test.go — the frontier is CONTIGUOUS, so a reader can never be
// handed an instant that includes one commit and excludes an earlier one that
// is still in flight (rmp #2298).
//
// TestClock_OutOfOrderPublicationNeverExposesAGap and
// TestClock_FrontierIsAlwaysTheContiguousMaximum are the acceptance gates, and
// both were verified to FAIL against the single-watermark implementation they
// replace — a monotone CAS of visible to ts — which reports 2 as visible the
// moment commit 2 publishes, while commit 1 has not.

import (
	"math/rand"
	"sync"
	"testing"
)

// TestClock_OutOfOrderPublicationNeverExposesAGap is acceptance criterion 2.
// Two writers allocate in order and publish in reverse; no reader may observe
// the second without the first.
func TestClock_OutOfOrderPublicationNeverExposesAGap(t *testing.T) {
	var c Clock

	first := c.NextCommitTS()  // writer A
	second := c.NextCommitTS() // writer B, allocated later
	if first != 1 || second != 2 {
		t.Fatalf("allocation gave %d, %d; want 1, 2", first, second)
	}
	if got := c.ReadTS(); got != 0 {
		t.Fatalf("ReadTS %d before anything finished, want 0", got)
	}

	// B finishes first. This is the whole point: the highest PUBLISHED
	// timestamp is now 2, but the highest timestamp below which nothing is in
	// flight is still 0.
	c.PublishCommitTS(second)

	startTS := c.ReadTS()
	if startTS >= second {
		t.Fatalf("ReadTS %d with commit %d still in flight: a reader starting here would observe "+
			"commit %d without commit %d, straddling a commit", startTS, first, second, first)
	}
	if Visible(second, startTS, 0) {
		t.Fatalf("commit %d is visible to a reader at %d while commit %d has not finished",
			second, startTS, first)
	}
	if Visible(first, startTS, 0) {
		t.Fatalf("commit %d is visible to a reader at %d before it finished", first, startTS)
	}

	// A finishes. Both are now behind the frontier, together.
	c.PublishCommitTS(first)
	if got := c.ReadTS(); got != second {
		t.Fatalf("ReadTS %d after both finished, want %d", got, second)
	}
	after := c.ReadTS()
	if !Visible(first, after, 0) || !Visible(second, after, 0) {
		t.Fatalf("a reader starting at %d after both commits finished sees first=%v second=%v; want both",
			after, Visible(first, after, 0), Visible(second, after, 0))
	}

	// The reader that started mid-flight keeps its instant: it must not
	// retroactively acquire either commit.
	if Visible(first, startTS, 0) || Visible(second, startTS, 0) {
		t.Fatal("a commit became visible to a snapshot taken before it finished")
	}
}

// TestClock_FrontierIsAlwaysTheContiguousMaximum is the property the whole
// scheme rests on, checked after every single publication across many random
// orders: ReadTS is the largest N such that every timestamp in [1, N] has
// finished. Anything larger is a gap; anything smaller is lost visibility.
func TestClock_FrontierIsAlwaysTheContiguousMaximum(t *testing.T) {
	const n = 300
	rng := rand.New(rand.NewSource(0x2298)) //nolint:gosec // deterministic test input, not crypto
	for trial := 0; trial < 40; trial++ {
		var c Clock
		for i := 0; i < n; i++ {
			c.NextCommitTS()
		}
		order := rng.Perm(n) // publish in a random order
		done := make([]bool, n+2)
		for step, idx := range order {
			ts := uint64(idx + 1)
			c.PublishCommitTS(ts)
			done[ts] = true

			want := uint64(0)
			for want < uint64(n) && done[want+1] {
				want++
			}
			if got := c.ReadTS(); got != want {
				t.Fatalf("trial %d step %d: after publishing %d, ReadTS = %d, want the contiguous "+
					"maximum %d", trial, step, ts, got, want)
			}
		}
		if got := c.ReadTS(); got != n {
			t.Fatalf("trial %d: ReadTS %d after every commit finished, want %d", trial, got, n)
		}
	}
}

// TestClock_AbandonedTimestampDoesNotStallTheFrontier covers the obligation
// [Clock.AbandonCommitTS] exists to discharge. A timestamp that is allocated
// and then neither published nor abandoned holds the frontier forever — every
// later commit stays invisible to new readers and the log grows without bound.
func TestClock_AbandonedTimestampDoesNotStallTheFrontier(t *testing.T) {
	var c Clock
	a := c.NextCommitTS()
	b := c.NextCommitTS()
	d := c.NextCommitTS()

	c.PublishCommitTS(b)
	c.PublishCommitTS(d)
	if got := c.ReadTS(); got != 0 {
		t.Fatalf("ReadTS %d with the oldest timestamp still in flight, want 0", got)
	}
	if got := c.InFlightCommits(); got != 3 {
		t.Fatalf("InFlightCommits %d, want 3", got)
	}

	// A fails after allocating. Nothing it wrote is visible under a, but the
	// frontier must be free to move past it.
	c.AbandonCommitTS(a)
	if got := c.ReadTS(); got != d {
		t.Fatalf("ReadTS %d after the stalled timestamp was abandoned, want %d", got, d)
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits %d once every timestamp finished, want 0", got)
	}
}

// TestClock_ConcurrentPublicationIsMonotone drives many publishers at once and
// asserts that no OBSERVER ever sees the frontier move backwards — the failure
// a per-publisher store outside the lock would produce, where an older
// publisher's frontier lands after a newer one's.
//
// Monotonicity is a property of one observer's successive loads, and it has to
// be asserted that way. A first version of this test compared each goroutine's
// single observation against a shared "highest seen so far" under a mutex and
// reported violations immediately — but the load and the mutex acquisition are
// not ordered with respect to each other, so another publisher advancing the
// frontier in between made a perfectly monotone atomic look like it had gone
// backwards. The harness was wrong, not the clock.
func TestClock_ConcurrentPublicationIsMonotone(t *testing.T) {
	const n = 4000
	var c Clock
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = c.NextCommitTS()
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(ts uint64) {
			defer wg.Done()
			var prev uint64
			observe := func(where string) {
				got := c.ReadTS()
				if got < prev {
					t.Errorf("frontier moved backwards for one observer (%s): %d after %d",
						where, got, prev)
				}
				prev = got
			}
			observe("before publish")
			c.PublishCommitTS(ts)
			for k := 0; k < 8; k++ {
				observe("after publish")
			}
			// Its own commit is behind the frontier from the moment it
			// published, so a reader starting here must see it.
			if got := c.ReadTS(); got >= ts && !Visible(ts, got, 0) {
				t.Errorf("own commit %d invisible to a reader at %d", ts, got)
			}
		}(ids[i])
	}
	wg.Wait()
	if got := c.ReadTS(); got != uint64(n) {
		t.Fatalf("ReadTS %d after all %d commits finished, want %d", got, n, n)
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits %d after all commits finished, want 0", got)
	}
}

// TestCommitRegistry_HoldsEveryPendingRecordAcrossLaps is the memory and
// capacity half. The registry is a fixed ring of slots, but a timestamp's record
// must stay findable until the frontier passes it however many laps of the ring
// are in flight — nothing may wait for room, because a transaction waiting for
// room could be holding what the frontier's owner needs. So a straggler held back
// while three full laps finish above it must still be publishable, and the
// frontier must then carry over all of them in one step.
func TestCommitRegistry_HoldsEveryPendingRecordAcrossLaps(t *testing.T) {
	var c Clock
	for i := 0; i < 50000; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}
	table := c.reg.slots.Load()
	if table == nil {
		t.Fatal("allocation did not create the registry")
	}
	stalled := c.NextCommitTS()
	above := make([]uint64, 3*registrySlots)
	for i := range above {
		above[i] = c.NextCommitTS()
	}
	if got := c.reg.chained.Load(); got == 0 {
		t.Fatal("more than one lap in flight chained no record: the fixture did not cross a lap")
	}
	for _, ts := range above {
		c.PublishCommitTS(ts)
	}
	if got, want := c.ReadTS(), stalled-1; got != want {
		t.Fatalf("frontier %d with %d still in flight, want %d", got, stalled, want)
	}
	c.PublishCommitTS(stalled)
	if got, want := c.ReadTS(), above[len(above)-1]; got != want {
		t.Fatalf("frontier %d after the stalled timestamp finished, want %d", got, want)
	}
	if c.reg.slots.Load() != table {
		t.Fatal("the registry was reallocated; it must be allocated once for the life of the clock")
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits %d after every commit finished, want 0", got)
	}
}

// TestCommitRegistry_RepeatedAndStaleFinishAreIgnored covers the two inputs
// that must not corrupt the frontier: finishing the same timestamp twice, and
// finishing one the frontier has already swept past — whose slot may by now
// belong to a newer timestamp that is still pending.
func TestCommitRegistry_RepeatedAndStaleFinishAreIgnored(t *testing.T) {
	var c Clock
	for i := 0; i < 10; i++ {
		c.PublishCommitTS(c.NextCommitTS())
	}
	for _, ts := range []uint64{1, 5, 10} {
		c.PublishCommitTS(ts)
		if got := c.ReadTS(); got != 10 {
			t.Fatalf("re-finishing %d moved the frontier to %d, want 10", ts, got)
		}
	}
	for c.ReadTS()%registrySlots != 1 {
		c.PublishCommitTS(c.NextCommitTS())
	}
	gap := c.NextCommitTS()     // f+1, left in flight
	pending := c.NextCommitTS() // f+2 == 3 mod registrySlots: shares stale 3's slot
	if pending%registrySlots != 3 {
		t.Fatalf("fixture: pending %d does not share timestamp 3's slot", pending)
	}
	c.PublishCommitTS(3) // stale, before pending finishes
	c.PublishCommitTS(pending)
	c.PublishCommitTS(3) // stale again, after
	if got, want := c.ReadTS(), gap-1; got != want {
		t.Fatalf("frontier %d with the gap still in flight, want %d: a stale finish advanced it", got, want)
	}
	c.PublishCommitTS(gap)
	if got := c.ReadTS(); got != pending {
		t.Fatalf("frontier %d after the gap closed, want %d: a stale finish displaced a pending one",
			got, pending)
	}
}

// TestCommitRegistry_WrapBoundaries walks the frontier across the ring's wrap,
// where a slot is reused for a timestamp one lap later, with a hole at each
// position around the seam.
func TestCommitRegistry_WrapBoundaries(t *testing.T) {
	for _, hole := range []uint64{
		1, 2, registrySlots - 1, registrySlots, registrySlots + 1,
		2*registrySlots - 1, 2 * registrySlots, 2*registrySlots + 1,
	} {
		var c Clock
		const n = 3 * registrySlots
		for ts := uint64(1); ts <= n; ts++ {
			if got := c.NextCommitTS(); got != ts {
				t.Fatalf("fixture: allocated %d, want %d", got, ts)
			}
		}
		for ts := uint64(1); ts <= n; ts++ {
			if ts != hole {
				c.PublishCommitTS(ts)
			}
		}
		if got, want := c.ReadTS(), hole-1; got != want {
			t.Fatalf("hole at %d: frontier %d, want %d", hole, got, want)
		}
		c.PublishCommitTS(hole)
		if got := c.ReadTS(); got != n {
			t.Fatalf("hole at %d: frontier %d after filling it, want %d", hole, got, n)
		}
	}
}

// TestHorizon_UnderOutOfOrderPublication is acceptance criterion 3's horizon
// half: the reclaimer's watermark must never reach a timestamp that is still in
// flight, whether or not readers are registered.
//
// The two are composed rather than tested separately because the composition is
// where the bug would be. Horizon.Oldest takes the clock's frontier as its
// FALLBACK — the value it returns when nothing is registered — so if the clock
// reported the highest PUBLISHED timestamp instead of the contiguous one, a
// graph with no readers at all would reclaim versions that an in-flight commit
// is about to supersede.
func TestHorizon_UnderOutOfOrderPublication(t *testing.T) {
	var (
		c Clock
		h Horizon
	)
	// Four commits allocated in order; the second is held in flight.
	ids := make([]uint64, 4)
	for i := range ids {
		ids[i] = c.NextCommitTS()
	}
	c.PublishCommitTS(ids[0])
	c.PublishCommitTS(ids[2])
	c.PublishCommitTS(ids[3])

	// No readers: the watermark falls back to the clock, which must be the
	// contiguous frontier and not the highest published timestamp.
	if got, want := h.Oldest(c.ReadTS()), ids[0]; got != want {
		t.Fatalf("watermark %d with commit %d in flight and %d, %d published, want %d: "+
			"a reclaimer would free versions the in-flight commit can still supersede",
			got, ids[1], ids[2], ids[3], want)
	}

	// A reader that started at the frontier holds it there.
	slot := h.EnterHolding()
	h.Publish(slot, c.ReadTS())
	if got := h.Oldest(c.ReadTS()); got != ids[0] {
		t.Fatalf("watermark %d with a reader at the frontier, want %d", got, ids[0])
	}

	// The in-flight commit finishes: the frontier jumps to the newest, and the
	// registered reader — not the clock — is now what holds the watermark back.
	c.PublishCommitTS(ids[1])
	if got := c.ReadTS(); got != ids[3] {
		t.Fatalf("frontier %d once every commit finished, want %d", got, ids[3])
	}
	if got := h.Oldest(c.ReadTS()); got != ids[0] {
		t.Fatalf("watermark %d with a reader still registered at %d, want %d",
			got, ids[0], ids[0])
	}
	h.Leave(slot)
	if got := h.Oldest(c.ReadTS()); got != ids[3] {
		t.Fatalf("watermark %d after the reader left, want the frontier %d", got, ids[3])
	}
}

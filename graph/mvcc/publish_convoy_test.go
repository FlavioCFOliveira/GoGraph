package mvcc

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClock_OutOfOrderPublicationNeverExposesOrRegresses is the safety contract
// of the publish path after rmp #2932, which stopped a locked publication from
// closing the lock-free fast path: under heavy concurrent OUT-OF-ORDER
// publication and abandonment, a reader's frontier must never move backwards and
// must never include a timestamp whose publisher has not yet started to publish
// it. Once every timestamp has finished, the frontier must have reached the last
// one and the fast path must be open again.
//
// "Started to publish" is the tightest oracle a test can hold without reaching
// into the clock: each writer marks a timestamp immediately BEFORE calling
// [Clock.PublishCommitTS] or [Clock.AbandonCommitTS], so a frontier that covers
// an unmarked timestamp has exposed a commit that was still in flight.
func TestClock_OutOfOrderPublicationNeverExposesOrRegresses(t *testing.T) {
	const (
		writers = 16
		rounds  = 4000
		batch   = 4 // timestamps each writer holds at once, published shuffled
	)
	total := writers * rounds * batch
	var (
		c       Clock
		started = make([]atomic.Bool, total+1)
		wg      sync.WaitGroup
		stop    atomic.Bool
		fault   atomic.Pointer[string]
	)
	report := func(msg string) { fault.CompareAndSwap(nil, &msg) }

	// Observers: monotone, and never ahead of what has started to publish.
	var obs sync.WaitGroup
	for o := 0; o < 2; o++ {
		obs.Add(1)
		go func() {
			defer obs.Done()
			prev, checked := uint64(0), uint64(0)
			for !stop.Load() {
				f := c.ReadTS()
				if f < prev {
					report("frontier went backwards")
					return
				}
				prev = f
				for ; checked < f && checked < uint64(total); checked++ {
					if !started[checked+1].Load() {
						report("frontier exposed a timestamp whose publisher had not started")
						return
					}
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, seed^0x5bd1e995)) //nolint:gosec // a reproducible schedule, not a secret
			var held [batch]uint64
			for r := 0; r < rounds; r++ {
				for i := range held {
					held[i] = c.NextCommitTS()
				}
				rng.Shuffle(batch, func(i, j int) { held[i], held[j] = held[j], held[i] })
				for _, ts := range held {
					started[ts].Store(true)
					if rng.IntN(8) == 0 {
						c.AbandonCommitTS(ts)
					} else {
						c.PublishCommitTS(ts)
					}
				}
			}
		}(uint64(w) + 1)
	}
	wg.Wait()
	stop.Store(true)
	obs.Wait()

	if f := fault.Load(); f != nil {
		t.Fatal(*f)
	}
	if got := c.ReadTS(); got != uint64(total) {
		t.Fatalf("frontier = %d once every timestamp finished, want %d: a finished commit is stranded",
			got, total)
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits = %d once every timestamp finished, want 0", got)
	}
	if got := c.reg.pending.Load(); got != 0 {
		t.Fatalf("pending = %d once every timestamp finished, want 0", got)
	}
	if got := c.OutOfOrderPublications(); got == 0 || got > uint64(total) {
		t.Fatalf("OutOfOrderPublications = %d for %d finished timestamps; the schedule publishes out "+
			"of order, so some must have been recorded and none can have been recorded twice", got, total)
	}
}

// TestClock_StragglerDoesNotSerialiseLaterPublications is the convoy half of
// rmp #2932. While one commit is still in flight, every later publication finishes
// out of order. With the previous locked slow path each of them queued on the
// publish lock behind the others; with the ring each records itself and returns,
// so they complete without waiting on one another, and the straggler's own
// publication then carries the frontier over all of them.
//
// The straggler is held back deliberately and the later publications run from
// many goroutines at once; the test requires them all to complete before the
// straggler publishes — which a lock that one of them was descheduled inside of
// could not guarantee to the others — and then requires the frontier to reach
// the last one in a single step.
func TestClock_StragglerDoesNotSerialiseLaterPublications(t *testing.T) {
	var c Clock
	straggler := c.NextCommitTS()
	const writers, each = 8, 256 // 2048 in flight above the straggler, within one ring
	later := make([][]uint64, writers)
	_ = straggler
	for w := range later {
		for i := 0; i < each; i++ {
			later[w] = append(later[w], c.NextCommitTS())
		}
	}
	var wg sync.WaitGroup
	for w := range later {
		wg.Add(1)
		go func(ts []uint64) {
			defer wg.Done()
			for _, t := range ts {
				c.PublishCommitTS(t)
			}
		}(later[w])
	}
	wg.Wait()
	if got, want := c.ReadTS(), straggler-1; got != want {
		t.Fatalf("frontier %d with the straggler %d in flight, want %d", got, straggler, want)
	}
	if got, want := c.OutOfOrderPublications(), uint64(writers*each); got != want {
		t.Fatalf("OutOfOrderPublications = %d, want %d: every later publication finished out of order",
			got, want)
	}
	c.PublishCommitTS(straggler)
	if got, want := c.ReadTS(), uint64(1+writers*each); got != want {
		t.Fatalf("frontier %d after the straggler published, want %d", got, want)
	}
}

// TestClock_HelperPublishesForADescheduledOwner is the deterministic proof of
// helping (rmp #2932). An owner whose record is ready is descheduled — held in a
// test hook — between allocating its timestamp and publishing it. A later
// publication, finding the frontier stuck on that owner, must stamp the owner's
// record and publish it within bounded time, while the owner is still held. The
// owner, once released, must find its own stamp and publication already done and
// its acknowledgement unchanged.
//
// With helping disabled the frontier stays one short for as long as the owner is
// held, so this is also the mutation check for the helping step.
func TestClock_HelperPublishesForADescheduledOwner(t *testing.T) {
	var c Clock
	const ownerTxID = TxIDBase + 1
	owner := NewCommitInfo(ownerTxID)
	held, release := make(chan uint64, 1), make(chan struct{})
	var first atomic.Bool
	c.afterAllocate = func(ts uint64) {
		if first.CompareAndSwap(false, true) { // only the owner is held
			held <- ts
			<-release
		}
	}
	acked := make(chan uint64, 1)
	go func() {
		ts := c.AllocateFor(owner, true)
		owner.Commit(ts)
		c.PublishCommitTS(ts)
		acked <- ts
	}()
	ownerTS := <-held // the owner is now descheduled inside its commit window

	helperTS := c.AllocateFor(NewCommitInfo(TxIDBase+2), true)
	c.PublishCommitTS(helperTS)

	deadline := time.Now().Add(2 * time.Second)
	for c.ReadTS() < helperTS && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := c.ReadTS(); got != helperTS {
		close(release)
		t.Fatalf("frontier %d with the owner of %d descheduled, want %d: no publication helped",
			got, ownerTS, helperTS)
	}
	if got := owner.TS(); got != ownerTS {
		t.Fatalf("the owner's record reads %d, want it stamped at %d before the frontier passed it",
			got, ownerTS)
	}
	if got := c.HelpedPublications(); got != 1 {
		t.Fatalf("HelpedPublications = %d, want 1", got)
	}

	close(release)
	if got := <-acked; got != ownerTS {
		t.Fatalf("the owner acknowledged %d, want %d", got, ownerTS)
	}
	if got := owner.TS(); got != ownerTS {
		t.Fatalf("the owner's own stamp changed its record to %d, want %d", got, ownerTS)
	}
	if got := c.ReadTS(); got != helperTS {
		t.Fatalf("frontier %d after the owner finished, want %d", got, helperTS)
	}
}

// TestClock_HelperNeverPublishesANotReadyOrAbortedCommit pins the other side of
// helping: a record that is not ready — its WAL record not yet durable, or its
// index changes not yet applied — is never published by a helper, and one that
// its owner aborts is released without ever being stamped.
func TestClock_HelperNeverPublishesANotReadyOrAbortedCommit(t *testing.T) {
	var c Clock
	notReady := NewCommitInfo(TxIDBase + 1)
	first := c.AllocateFor(notReady, false)
	second := c.AllocateFor(NewCommitInfo(TxIDBase+2), true)
	c.PublishCommitTS(second)
	if got := c.ReadTS(); got != first-1 {
		t.Fatalf("frontier %d past a commit that was not ready, want %d", got, first-1)
	}
	if got := notReady.TS(); got != TxIDBase+1 {
		t.Fatalf("a record that was not ready was stamped: %d", got)
	}

	// The owner aborts and releases its timestamp.
	notReady.Abort()
	c.AbandonCommitTS(first)
	if got := c.ReadTS(); got != second {
		t.Fatalf("frontier %d after the aborted timestamp was released, want %d", got, second)
	}
	if got := notReady.TS(); got != AbortedTS {
		t.Fatalf("the aborted record reads %d, want AbortedTS: it was published as a commit", got)
	}
	if got := c.HelpedPublications(); got != 0 {
		t.Fatalf("HelpedPublications = %d, want 0", got)
	}
}

// TestClock_HelpingUnderStressKeepsEveryInvariant drives owners that publish
// ready commits, owners that abort, and anonymous allocations, from many
// goroutines, with a hook that randomly deschedules owners inside their commit
// window. Observers assert, on every frontier they read, that it never moves
// backwards and that every commit at or below it is stamped at exactly its own
// timestamp or was released; once every writer is done the frontier must have
// reached the last timestamp and every owner's acknowledgement must match its
// record. The observer files a record only once its owner has returned from the
// allocation, which a helper can outrun; the check waits for the filing, and a
// record that is stamped is stamped by then.
func TestClock_HelpingUnderStressKeepsEveryInvariant(t *testing.T) {
	const (
		writers = 16
		rounds  = 3000
	)
	total := writers * rounds
	var c Clock
	records := make([]atomic.Pointer[CommitInfo], total+1)
	released := make([]atomic.Bool, total+1)
	c.afterAllocate = func(uint64) {
		if rand.IntN(64) == 0 { //nolint:gosec // a schedule, not a secret
			runtime.Gosched()
		}
	}
	var stop atomic.Bool
	var fault atomic.Pointer[string]
	report := func(msg string) { fault.CompareAndSwap(nil, &msg) }

	var obs sync.WaitGroup
	obs.Add(1)
	go func() {
		defer obs.Done()
		prev, checked := uint64(0), uint64(0)
		for !stop.Load() {
			f := c.ReadTS()
			if f < prev {
				report("frontier went backwards")
				return
			}
			prev = f
			for ; checked < f; checked++ {
				ts := checked + 1
				if released[ts].Load() {
					continue
				}
				// A helper may publish a commit before its owner has returned from
				// the allocation and filed its record here; wait for the filing.
				rec := records[ts].Load()
				for rec == nil && !stop.Load() {
					runtime.Gosched()
					rec = records[ts].Load()
				}
				if rec == nil {
					return
				}
				if released[ts].Load() {
					continue
				}
				if rec.TS() != ts {
					report("the frontier passed a commit that was neither stamped at its timestamp nor released")
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	var mismatches atomic.Int64
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w)+1, 7)) //nolint:gosec // a schedule, not a secret
			for r := 0; r < rounds; r++ {
				txID := TxIDBase + uint64(w*rounds+r) + 1
				info := NewCommitInfo(txID)
				switch rng.IntN(8) {
				case 0: // abort: allocated before the decision, never ready
					ts := c.AllocateFor(info, false)
					released[ts].Store(true)
					records[ts].Store(info)
					info.Abort()
					c.AbandonCommitTS(ts)
				default: // commit, ready from allocation
					ts := c.AllocateFor(info, true)
					records[ts].Store(info)
					info.Commit(ts)
					c.PublishCommitTS(ts)
					if info.TS() != ts {
						mismatches.Add(1)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	stop.Store(true)
	obs.Wait()
	if f := fault.Load(); f != nil {
		t.Fatal(*f)
	}
	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d owners found their record stamped at a timestamp other than the one they were given", n)
	}
	if got := c.ReadTS(); got != uint64(total) {
		t.Fatalf("frontier %d once every writer finished, want %d", got, total)
	}
	if got := c.reg.pending.Load(); got != 0 {
		t.Fatalf("pending = %d once every writer finished, want 0: the in-order fast path would "+
			"walk the registry for ever", got)
	}
	t.Logf("helped=%d outOfOrder=%d", c.HelpedPublications(), c.OutOfOrderPublications())
}

package mvcc

// registry_invariants_test.go — invariants of the lock-free commit registry
// (rmp #2932) that the other registry tests do not reach: a record finished
// twice, a record registered twice, a lap link that outlives its purpose, more
// than registrySlots commits in flight under concurrency, and the WAL shape — a
// record allocated not ready and marked ready later — under concurrent helping.
//
// Layer: short.

import (
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestCommitRegistry_DoubleOutOfOrderFinishCountsOnce: a timestamp finished
// twice while an earlier one is still in flight must be counted into pending
// once. Counted twice and settled once, pending stays above zero for ever and
// every later in-order publication walks the registry.
func TestCommitRegistry_DoubleOutOfOrderFinishCountsOnce(t *testing.T) {
	var c Clock
	gap := c.NextCommitTS()
	late := c.NextCommitTS()
	c.PublishCommitTS(late)
	c.PublishCommitTS(late)
	c.AbandonCommitTS(late)
	if got := c.reg.pending.Load(); got != 1 {
		t.Fatalf("pending = %d after finishing %d three times out of order, want 1", got, late)
	}
	if got := c.OutOfOrderPublications(); got != 1 {
		t.Fatalf("OutOfOrderPublications = %d, want 1: a repeated finish is not a publication", got)
	}
	c.PublishCommitTS(gap)
	if got := c.ReadTS(); got != late {
		t.Fatalf("frontier = %d after the gap closed, want %d", got, late)
	}
	if got := c.reg.pending.Load(); got != 0 {
		t.Fatalf("pending = %d once every timestamp finished, want 0", got)
	}
}

// TestClock_AllocateForRefusesARegisteredRecord: a record is registered for at
// most one timestamp. A second registration would overwrite the claim the first
// timestamp is found by, leaving it with no findable owner.
func TestClock_AllocateForRefusesARegisteredRecord(t *testing.T) {
	for _, ready := range []bool{false, true} {
		var c Clock
		info := NewCommitInfo(TxIDBase + 1)
		first := c.AllocateFor(info, false)
		msg := func() (msg string) {
			defer func() {
				if r := recover(); r != nil {
					msg, _ = r.(string)
				}
			}()
			c.AllocateFor(info, ready)
			return ""
		}()
		if !strings.Contains(msg, "already registered") {
			t.Fatalf("ready=%v: a second AllocateFor on one record returned instead of panicking (%q)", ready, msg)
		}
		if got := info.claim.Load(); got != first {
			t.Fatalf("ready=%v: the refused registration changed the record's claim to %d, want %d", ready, got, first)
		}
		if info.ready.Load() {
			t.Fatalf("ready=%v: the refused registration marked the record ready", ready)
		}
		if got := c.InFlightCommits(); got != 1 {
			t.Fatalf("ready=%v: InFlightCommits = %d after the refusal, want 1: it allocated", ready, got)
		}
	}
}

// TestCommitRegistry_PassedRecordsDropTheirLapLink: a record linked behind the
// previous lap's record drops that link once the frontier passes it, on both
// paths that pass records — the in-order fast path and the carrying walk.
// A record keeps its link otherwise for as long as any version references it,
// and with it the chain behind it.
func TestCommitRegistry_PassedRecordsDropTheirLapLink(t *testing.T) {
	for _, inOrder := range []bool{true, false} {
		name := "carried"
		if inOrder {
			name = "in-order"
		}
		t.Run(name, func(t *testing.T) {
			var c Clock
			stalled := c.NextCommitTS()
			recs := make([]*CommitInfo, 2*registrySlots)
			ts := make([]uint64, len(recs))
			for i := range recs {
				recs[i] = NewCommitInfo(TxIDBase + uint64(i) + 1)
				ts[i] = c.AllocateFor(recs[i], false)
			}
			linkedBefore := c.reg.linked.Load()
			if linkedBefore == 0 {
				t.Fatal("fixture: more than one lap in flight linked no record")
			}
			if inOrder {
				c.PublishCommitTS(stalled)
			}
			for i, r := range recs {
				r.Commit(ts[i])
				c.PublishCommitTS(ts[i])
			}
			if !inOrder {
				c.PublishCommitTS(stalled)
			}
			if got, want := c.ReadTS(), ts[len(ts)-1]; got != want {
				t.Fatalf("frontier = %d, want %d", got, want)
			}
			for i, r := range recs {
				if o := r.older.Load(); o != nil {
					t.Fatalf("record of %d still links the record of %d after the frontier passed both "+
						"(%d links were made)", ts[i], o.claim.Load(), linkedBefore)
				}
			}
			if got := c.reg.linked.Load(); got != 0 {
				t.Fatalf("linked = %d once every record was passed, want 0", got)
			}
		})
	}
}

// TestClock_HelperPublishesARecordMarkedReadyAfterAllocation is the WAL shape,
// deterministically: a record allocated not ready (its fsync outstanding) holds
// the frontier; once marked ready, the next stuck publication stamps and
// publishes it for its owner, whose own stamp and publication are then no-ops.
func TestClock_HelperPublishesARecordMarkedReadyAfterAllocation(t *testing.T) {
	var c Clock
	wal := NewCommitInfo(TxIDBase + 1)
	walTS := c.AllocateFor(wal, false)
	b := NewCommitInfo(TxIDBase + 2)
	bTS := c.AllocateFor(b, true)
	b.Commit(bTS)
	c.PublishCommitTS(bTS)
	if got := c.ReadTS(); got != walTS-1 {
		t.Fatalf("frontier = %d past a record whose fsync is outstanding, want %d", got, walTS-1)
	}
	wal.MarkReady() // the fsync completed; the owner has not yet published
	d := NewCommitInfo(TxIDBase + 3)
	dTS := c.AllocateFor(d, true)
	d.Commit(dTS)
	c.PublishCommitTS(dTS)
	if got := c.ReadTS(); got != dTS {
		t.Fatalf("frontier = %d after a stuck publication met a ready record, want %d", got, dTS)
	}
	if got := wal.TS(); got != walTS {
		t.Fatalf("the helped record reads %d, want it stamped at %d", got, walTS)
	}
	if got := c.HelpedPublications(); got != 1 {
		t.Fatalf("HelpedPublications = %d, want 1", got)
	}
	wal.Commit(walTS)
	c.PublishCommitTS(walTS)
	if got := wal.TS(); got != walTS || c.ReadTS() != dTS {
		t.Fatalf("the owner's late publication changed state: record %d, frontier %d", got, c.ReadTS())
	}
	if got := c.reg.pending.Load(); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
}

// TestClock_ManyLapsInFlightUnderConcurrency drives more than registrySlots
// commits in flight at once from many goroutines, in the WAL shape: each record
// is allocated not ready, marked ready after a simulated fsync, then stamped and
// published — or aborted and abandoned — in shuffled order, some finished twice.
// A barrier holds every goroutine's batch in flight together, so the ring is
// lapped. In every round one writer marks its records ready and then withholds
// its own publications until the others are done — an owner descheduled after
// its fsync — so the others' out-of-order publications must help it; its
// aborted records, never ready, must hold the frontier until it abandons them. Observers assert on every frontier they read that it never moves
// backwards and passes only commits that were ready and stamped at their own
// timestamp, or released. Afterwards the frontier is at the last timestamp and
// no record is pending or linked.
func TestClock_ManyLapsInFlightUnderConcurrency(t *testing.T) {
	const (
		writers = 32
		batch   = 256 // writers*batch = 8192 in flight: two laps
		rounds  = 6
	)
	total := writers * batch * rounds
	var c Clock
	records := make([]atomic.Pointer[CommitInfo], total+1)
	readied := make([]atomic.Bool, total+1)
	released := make([]atomic.Bool, total+1)
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
				if !readied[ts].Load() {
					report("the frontier passed a commit that was never marked ready")
					return
				}
				if rec.TS() != ts {
					report("the frontier passed a commit not stamped at its own timestamp")
					return
				}
			}
		}
	}()

	var allocated sync.WaitGroup
	var wg sync.WaitGroup
	for r := 0; r < rounds; r++ {
		allocated.Add(writers)
		wg.Add(writers)
		var others sync.WaitGroup
		others.Add(writers - 1)
		heldReady := make(chan struct{})
		for w := 0; w < writers; w++ {
			go func(w int) {
				defer wg.Done()
				if w != 0 {
					defer others.Done()
				}
				rng := rand.New(rand.NewPCG(uint64(r*writers+w)+1, 11)) //nolint:gosec // a schedule, not a secret
				type held struct {
					info  *CommitInfo
					ts    uint64
					abort bool
				}
				hs := make([]held, batch)
				for i := range hs {
					info := NewCommitInfo(TxIDBase + uint64((r*writers+w)*batch+i) + 1)
					ts := c.AllocateFor(info, false)
					hs[i] = held{info: info, ts: ts, abort: rng.IntN(8) == 0}
					if hs[i].abort {
						released[ts].Store(true)
					}
					records[ts].Store(info)
				}
				allocated.Done()
				allocated.Wait() // every batch of this round is in flight together
				if w == 0 {
					for _, h := range hs {
						if !h.abort {
							readied[h.ts].Store(true)
							h.info.MarkReady()
						}
					}
					close(heldReady)
					others.Wait() // descheduled after the fsync
				} else {
					<-heldReady
				}
				rng.Shuffle(len(hs), func(i, j int) { hs[i], hs[j] = hs[j], hs[i] })
				for _, h := range hs {
					if rng.IntN(16) == 0 {
						runtime.Gosched() // the fsync
					}
					if h.abort {
						h.info.Abort()
						c.AbandonCommitTS(h.ts)
						continue
					}
					readied[h.ts].Store(true)
					h.info.MarkReady()
					h.info.Commit(h.ts)
					c.PublishCommitTS(h.ts)
					if rng.IntN(16) == 0 {
						c.PublishCommitTS(h.ts) // a repeated finish
					}
				}
			}(w)
		}
		wg.Wait()
	}
	stop.Store(true)
	obs.Wait()
	if f := fault.Load(); f != nil {
		t.Fatal(*f)
	}
	if got := c.ReadTS(); got != uint64(total) {
		t.Fatalf("frontier = %d once every writer finished, want %d", got, total)
	}
	if got := c.InFlightCommits(); got != 0 {
		t.Fatalf("InFlightCommits = %d, want 0", got)
	}
	if got := c.reg.pending.Load(); got != 0 {
		t.Fatalf("pending = %d once every writer finished, want 0", got)
	}
	if got := c.reg.linked.Load(); got != 0 {
		t.Fatalf("linked = %d once every record was passed, want 0", got)
	}
	if got := c.reg.chained.Load(); got == 0 {
		t.Fatal("fixture: no record was chained, so the ring was never lapped")
	}
	if got := c.HelpedPublications(); got == 0 {
		t.Fatal("fixture: no publication helped, so the ready-after-allocation shape was not exercised")
	}
	for ts := 1; ts <= total; ts++ {
		if rec := records[ts].Load(); rec.older.Load() != nil {
			t.Fatalf("record of %d still holds its lap link", ts)
		}
	}
	t.Logf("chained=%d helped=%d outOfOrder=%d", c.reg.chained.Load(), c.HelpedPublications(), c.OutOfOrderPublications())
}

// TestCommitRegistry_InOrderPublicationUnlinksADisplacedRecord forces the
// interleaving rmp #2932's re-audit named (N3): an in-order publication moves
// the frontier past its record, a later lap then claims the record's slot — the
// record is passed, so it is displaced without being linked — and only then does
// the publication clear the record's lap link. Looking the record up by its
// timestamp would find nothing at that point; the owner's own record must be used.
func TestCommitRegistry_InOrderPublicationUnlinksADisplacedRecord(t *testing.T) {
	var c Clock
	stalled := NewCommitInfo(TxIDBase + 1)
	sTS := c.AllocateFor(stalled, false)
	mid := make([]uint64, registrySlots-1)
	for i := range mid {
		mid[i] = c.NextCommitTS()
	}
	own := NewCommitInfo(TxIDBase + 2)
	oTS := c.AllocateFor(own, false)
	if own.older.Load() != stalled {
		t.Fatalf("fixture: the record of %d does not link the record of %d", oTS, sTS)
	}
	c.PublishCommitTS(sTS)
	for _, ts := range mid {
		c.PublishCommitTS(ts)
	}
	if got := c.ReadTS(); got != oTS-1 {
		t.Fatalf("fixture: frontier %d, want %d", got, oTS-1)
	}

	var lap []uint64
	c.afterInOrderPublish = func(ts uint64) {
		if ts != oTS || lap != nil {
			return
		}
		lap = make([]uint64, 0, registrySlots)
		for i := 0; i < registrySlots; i++ {
			lap = append(lap, c.NextCommitTS())
		}
	}
	own.Commit(oTS)
	c.PublishCommit(own, oTS)
	c.afterInOrderPublish = nil
	if lap == nil {
		t.Fatal("fixture: the in-order publication hook did not run")
	}
	if c.reg.find(oTS) != nil {
		t.Fatal("fixture: the record was not displaced from its slot before the unlink")
	}
	if o := own.older.Load(); o != nil {
		t.Fatalf("the record of %d still links the record of %d after the frontier passed it and a later lap "+
			"displaced it: the unlink looked it up and missed it", oTS, o.claim.Load())
	}
	for _, ts := range lap {
		c.PublishCommitTS(ts)
	}
	if got := c.reg.linked.Load(); got != 0 {
		t.Fatalf("linked = %d once every record was passed, want 0", got)
	}
}

// TestCommitRegistry_AbandonOfAKeptAnonymousRecordUnlinks is the same forced
// interleaving for the path graph/lpg takes when a durable transaction allocates
// its instant before it has versioned anything: the instant is registered to an
// anonymous record that lpg keeps, and abandoned — the transaction versioned
// nothing — through [Clock.AbandonCommit] with that record. A lookup by
// timestamp would miss it once a later lap has displaced it, and the link, and
// the linked count that decides whether every in-order publication looks, would
// then never be cleared.
func TestCommitRegistry_AbandonOfAKeptAnonymousRecordUnlinks(t *testing.T) {
	var c Clock
	stalled := NewCommitInfo(TxIDBase + 1)
	sTS := c.AllocateFor(stalled, false)
	mid := make([]uint64, registrySlots-1)
	for i := range mid {
		mid[i] = c.NextCommitTS()
	}
	anon := new(CommitInfo)
	aTS := c.AllocateFor(anon, false)
	if anon.older.Load() != stalled {
		t.Fatalf("fixture: the record of %d does not link the record of %d", aTS, sTS)
	}
	c.PublishCommitTS(sTS)
	for _, ts := range mid {
		c.PublishCommitTS(ts)
	}
	var lap []uint64
	c.afterInOrderPublish = func(ts uint64) {
		if ts != aTS || lap != nil {
			return
		}
		lap = make([]uint64, 0, registrySlots)
		for i := 0; i < registrySlots; i++ {
			lap = append(lap, c.NextCommitTS())
		}
	}
	c.AbandonCommit(anon, aTS)
	c.afterInOrderPublish = nil
	if lap == nil || c.reg.find(aTS) != nil {
		t.Fatal("fixture: the record was not displaced before the unlink")
	}
	if anon.older.Load() != nil {
		t.Fatal("the abandoned anonymous record still holds its lap link: the abandonment looked it up and missed it")
	}
	for _, ts := range lap {
		c.PublishCommitTS(ts)
	}
	if got := c.reg.linked.Load(); got != 0 {
		t.Fatalf("linked = %d once every record was passed, want 0: every in-order publication would look up its record for ever", got)
	}
}

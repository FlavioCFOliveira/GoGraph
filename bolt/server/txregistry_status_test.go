package server

// txregistry_status_test.go — the contract rmp #2714 put in writing, asserted
// where a regression would otherwise be silent.
//
// # What changed and what has to hold
//
// Refreshing an open transaction's State and Query used to happen under the
// registry's process-global mutex, which made a listing a single global instant
// for free. It now happens through an atomic pointer on the entry itself, and
// [Server.Transactions] documents the consequence: a listing is consistent PER
// ENTRY and approximate ACROSS entries.
//
// "Consistent per entry" is the half that is load-bearing. Every reader of a
// listing in this module reads State and Query together on one row —
// internal/sim/bolt_tx_registry.go's checkBoltTxListingRow adjudicates both
// against the same plan row, and internal/sim/bolt_tx_registry_test.go waits on
// `txs[0].Query == "RETURN 1" && txs[0].State == "TX_READY"` as a conjunction. A
// design that published the two fields independently would let those observe a
// pair that never held, and the DST oracle would go quietly unsound rather than
// fail. This file is what stops that design being reintroduced.
//
// Layer: short.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// The two coherent pairs the writer alternates between. They share NO field, so
// any mixture of the two is detectable and unambiguous.
const (
	statePairAState = "TX_READY"
	statePairAQuery = "RETURN 1"
	statePairBState = "TX_STREAMING"
	statePairBQuery = "MATCH (n) RETURN n"
)

// TestTxEntryStatus_RowIsNeverTorn drives one entry through a hot alternation
// between two coherent (state, query) pairs while readers list the registry, and
// asserts that no listing ever reports a mixture.
//
// It also asserts that BOTH pairs were actually seen. Without that the test could
// pass by observing one pair a million times — or by the writer never running at
// all — and an oracle that cannot fail proves nothing.
func TestTxEntryStatus_RowIsNeverTorn(t *testing.T) {
	t.Parallel()

	r := newTxRegistry(clock.Real())
	e := newTxEntry("torn-1", "alice", "127.0.0.1:0", "r", statePairAState, nil)
	e.setStatus(statePairAState, statePairAQuery)
	r.register(e)

	var stop atomic.Bool
	var wg sync.WaitGroup

	// The single writer, which is the discipline txEntry documents: only the
	// owning session's message loop writes an entry.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			if i&1 == 0 {
				e.setStatus(statePairBState, statePairBQuery)
			} else {
				e.setStatus(statePairAState, statePairAQuery)
			}
		}
	}()

	const readers = 4
	torn := make([]string, readers)
	sawA := make([]bool, readers)
	sawB := make([]bool, readers)
	for k := 0; k < readers; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for !stop.Load() {
				got := r.list()
				if len(got) != 1 {
					torn[k] = "the listing lost or duplicated the entry"
					return
				}
				switch {
				case got[0].State == statePairAState && got[0].Query == statePairAQuery:
					sawA[k] = true
				case got[0].State == statePairBState && got[0].Query == statePairBQuery:
					sawB[k] = true
				default:
					torn[k] = "State=" + got[0].State + " Query=" + got[0].Query
					return
				}
			}
		}(k)
	}

	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	for k := range torn {
		if torn[k] != "" {
			t.Fatalf("reader %d saw a torn row: %s — State and Query were published "+
				"independently, so a listing described a transaction that never existed", k, torn[k])
		}
	}
	// The oracle must have had something to adjudicate.
	anyA, anyB := false, false
	for k := 0; k < readers; k++ {
		anyA = anyA || sawA[k]
		anyB = anyB || sawB[k]
	}
	if !anyA || !anyB {
		t.Fatalf("the readers observed pair A in %v and pair B in %v: the alternation did not "+
			"reach them, so this run proved nothing about tearing", anyA, anyB)
	}
}

// TestTxEntryStatus_EmptyQueryMeansUnchanged pins the rule the message loop
// depends on: it calls reportTx("") after every message that is not a RUN, and
// that must NOT erase the statement the transaction is running.
func TestTxEntryStatus_EmptyQueryMeansUnchanged(t *testing.T) {
	t.Parallel()

	e := newTxEntry("keep-1", "alice", "127.0.0.1:0", "r", "TX_READY", nil)
	e.setStatus("TX_STREAMING", statePairBQuery)
	e.setStatus("TX_READY", "")

	state, query := e.loadStatus()
	if state != "TX_READY" {
		t.Errorf("state = %q, want TX_READY", state)
	}
	if query != statePairBQuery {
		t.Errorf("query = %q, want %q — an empty query means \"unchanged\", not \"forget it\"",
			query, statePairBQuery)
	}
}

// TestTxEntryStatus_UnchangedPublishesNothing pins the publish-nothing guard by
// identity of the published value, which is the only way to see it from outside:
// a refresh that moves neither field must leave the very same txStatus in place,
// so a multi-chunk PULL restating TX_STREAMING allocates nothing.
func TestTxEntryStatus_UnchangedPublishesNothing(t *testing.T) {
	t.Parallel()

	e := newTxEntry("noop-1", "alice", "127.0.0.1:0", "r", "TX_READY", nil)
	e.setStatus("TX_STREAMING", statePairAQuery)
	before := e.status.Load()

	e.setStatus("TX_STREAMING", statePairAQuery)
	if after := e.status.Load(); after != before {
		t.Error("a refresh that moved nothing published a new value: the guard is gone and " +
			"every message of a multi-chunk PULL now allocates")
	}
	// And a refresh that DOES move something must publish, or the registry would
	// freeze on its first value.
	e.setStatus("TX_READY", "")
	if after := e.status.Load(); after == before {
		t.Fatal("a state transition published nothing: the guard is comparing the wrong thing")
	}
}

// TestTxRegistryRegister_RefusesAnUnarmedEntry pins the invariant that keeps
// loadStatus free of a nil check: an entry that did not come from newTxEntry is
// never published, so no listing can ever dereference a nil status.
func TestTxRegistryRegister_RefusesAnUnarmedEntry(t *testing.T) {
	t.Parallel()

	r := newTxRegistry(clock.Real())
	r.register(&txEntry{id: "unarmed-1", principal: "alice", remote: "127.0.0.1:0", mode: "r"})
	if got := r.list(); len(got) != 0 {
		t.Fatalf("the registry published %d entry built outside newTxEntry; a listing over it "+
			"would dereference a nil status pointer", len(got))
	}
}

// TestTxEntryStatus_AlternationAllocatesNothing pins the two-slot reuse in
// [txEntry.setStatus].
//
// The shape it measures is the one the message loop actually produces: one RUN
// and the PULLs that drain it alternate TX_STREAMING and TX_READY while the
// statement text stays put. Without the reuse that publishes a fresh 32-byte
// txStatus on every message, and the measurement that motivated rmp #2714 showed
// that allocation, not the atomic store, is what caps the refresh's throughput.
//
// The second half of the test is what stops the first half being vacuous: it
// asserts that a shape which DEFEATS the reuse does allocate, so a
// testing.AllocsPerRun that had simply stopped counting could not pass this.
//
// Both figures are build-mode sensitive in general — -race changes allocation
// counts elsewhere in this module — so this asserts a floor of zero and a
// non-zero, not two exact numbers.
// It does NOT call t.Parallel: testing.AllocsPerRun panics in a parallel test,
// because it pins GOMAXPROCS to 1 for the duration and cannot do that while other
// tests are running beside it.
func TestTxEntryStatus_AlternationAllocatesNothing(t *testing.T) {
	e := newTxEntry("alloc-1", "alice", "127.0.0.1:0", "r", "TX_READY", nil)
	// Warm both slots before measuring: the first cycle necessarily allocates.
	e.setStatus(statePairBState, statePairAQuery)
	e.setStatus(statePairAState, statePairAQuery)

	i := 0
	got := testing.AllocsPerRun(1000, func() {
		if i&1 == 0 {
			e.setStatus(statePairBState, statePairAQuery)
		} else {
			e.setStatus(statePairAState, statePairAQuery)
		}
		i++
	})
	if got != 0 {
		t.Errorf("alternating between two pairs allocates %v per refresh, want 0: the two-slot "+
			"reuse in setStatus is gone, and every Bolt message on an open transaction now "+
			"allocates", got)
	}

	// The control: a statement text that never repeats defeats the reuse, and MUST
	// allocate. If this reads zero the measurement above is not measuring.
	f := newTxEntry("alloc-2", "alice", "127.0.0.1:0", "r", "TX_READY", nil)
	j := 0
	ctl := testing.AllocsPerRun(1000, func() {
		f.setStatus(statePairBState, statePairAQuery+string(rune('a'+j%26))+string(rune('a'+j/26%26)))
		j++
	})
	if ctl == 0 {
		t.Errorf("a refresh carrying a brand-new statement text allocated %v: testing.AllocsPerRun "+
			"is not counting, so the zero asserted above proves nothing", ctl)
	}
	t.Logf("alternating: %v allocs/refresh; never-repeating query: %v allocs/refresh", got, ctl)
}

package server

// txbookkeeping_bench_test.go — the cost and the SCALING of the two
// process-global mutexes bolt/server takes on the explicit-transaction path.
//
// # Why an in-package benchmark and not a wire workload
//
// bench/contention and bench/boltwire both drive these locks through the whole
// Bolt stack, which is the right way to price what they cost the module. Neither
// can say how the lock itself behaves, because the engine, the parser and the
// wire dominate the window. These benchmarks call the registry and the quota
// directly, so what -cpu varies is the number of goroutines meeting on ONE
// mutex and nothing else.
//
// Both types are unexported, so this file must live in package server. It is a
// benchmark file: `go test` never runs it without -bench, so it costs the short
// layer only its compilation.
//
// # What each one models
//
// registryUpdate is the refresh the message loop performs after EVERY inbound
// message while a transaction is open: Session.reportTx -> txEntry.setStatus,
// called from the message loop at serve.go:1370. It is the highest-frequency
// registry operation by a wide margin.
//
// It is measured against RegistryUpdateLegacy, which is the implementation this
// package carried until rmp #2714 — one process-global sync.Mutex, entry found by
// a map lookup under it — transcribed into this file so BOTH arms run in ONE
// binary, one process and one interleaved -count. The transcription is validated
// rather than trusted: its numbers are compared against the same benchmark run on
// the pre-change tree, and the task's report records that comparison.
//
// registryChurn is the BEGIN/COMMIT pair: nextID + register + unregister. It
// runs once per transaction rather than once per message.
//
// quotaAcquireRelease is txQuota.acquire/release (txquota.go:81 and :95). The
// principal is the SAME on every goroutine, because that is the production
// shape: one application's connection pool authenticates under one identity, so
// every connection hits one map key.
//
// quotaDisabled is the arm that prices the quota against its own documented
// off switch (a negative Options.MaxOpenTxPerPrincipal), so the delta is the
// lock and not the map.
//
// Run with:
//
//	go test -run='^$' -bench=BenchmarkTxBookkeeping -benchmem -cpu=1,2,4,8 -count=6 ./bolt/server/

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// nextSeq hands each b.RunParallel goroutine a distinct index. testing.PB
// exposes no worker id, so the benchmarks below claim one from this counter at
// the top of their body, before the pb.Next loop and therefore outside the
// measured region.
func nextSeq(seq *int64) int64 { return atomic.AddInt64(seq, 1) - 1 }

// benchPrincipal is the identity every goroutine authenticates as. One value on
// purpose: see the note above.
const benchPrincipal = "neo4j"

// benchEntries is how many entries each registry arm pre-registers: one per
// possible b.RunParallel worker, so no two goroutines ever share an entry — which
// is the production shape, one open transaction per connection.
const benchEntries = 4096

// benchQuery is the statement text a RUN reports. Constant across the arms so the
// comparison is about the mechanism and not about string handling.
const benchQuery = "RETURN 1"

// benchStates is the alternation the ladder feeds every update arm, and getting
// it right is what keeps the comparison honest.
//
// The message loop reports s.state.String() after every inbound message, and over
// a RUN/PULL pair that string ALTERNATES: TX_STREAMING while a result is being
// drained, TX_READY between statements. An arm that fed one constant value would
// let the post-rmp-#2714 implementation short-circuit at its
// "nothing moved, publish nothing" guard on every iteration but the first, and
// would then be measuring a load and a comparison against a rival that measures a
// lock — a rigged comparison. Alternating makes every iteration of the new arm
// publish, which is the hardest case for it and the fairest against the old.
//
// BenchmarkTxBookkeeping_RegistryUpdateUnchanged prices the short-circuit
// separately, because it is a real production shape too: a multi-chunk PULL
// restates TX_STREAMING once per chunk.
var benchStates = [2]string{"TX_STREAMING", "TX_READY"}

// BenchmarkTxBookkeeping_RegistryUpdate measures the per-MESSAGE refresh as it
// stands after rmp #2714: the owning session writes its OWN entry through an
// atomic pointer, so no lock is taken and no cache line is shared.
//
// The state alternates and the statement text does not, which is what one RUN and
// the PULLs that drain it produce, and which the entry's two-slot reuse serves
// without allocating. BenchmarkTxBookkeeping_RegistryUpdateNewQuery is the arm
// that defeats that reuse.
func BenchmarkTxBookkeeping_RegistryUpdate(b *testing.B) {
	r := newTxRegistry(clock.Real())
	var seq int64
	entries := make([]*txEntry, benchEntries)
	for i := range entries {
		entries[i] = newTxEntry(fmt.Sprintf("bench-%d", i), benchPrincipal, "127.0.0.1:0", "r", "TX_READY", nil)
		r.register(entries[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := entries[nextSeq(&seq)%int64(len(entries))]
		i := 0
		for pb.Next() {
			mine.setStatus(benchStates[i&1], benchQuery)
			i++
		}
	})
}

// benchQueries is a rotation of DISTINCT statement texts, long enough that the
// entry's two-slot reuse can never hit. It is pre-built so that generating the
// text is not itself inside the timed region.
var benchQueries = func() []string {
	q := make([]string, 64)
	for i := range q {
		q[i] = fmt.Sprintf("MATCH (n:L%d) RETURN n", i)
	}
	return q
}()

// BenchmarkTxBookkeeping_RegistryUpdateNewQuery is the arm that defeats the
// two-slot reuse on purpose: a different statement text on every message, so
// every refresh must allocate a fresh txStatus.
//
// It is the honest worst case for the post-rmp-#2714 design and it is published
// beside BenchmarkTxBookkeeping_RegistryUpdate rather than instead of it, because
// neither shape alone is the truth: a RUN drained over several PULLs produces the
// first, and a transaction issuing an unbroken stream of distinct statements
// produces the second.
func BenchmarkTxBookkeeping_RegistryUpdateNewQuery(b *testing.B) {
	r := newTxRegistry(clock.Real())
	var seq int64
	entries := make([]*txEntry, benchEntries)
	for i := range entries {
		entries[i] = newTxEntry(fmt.Sprintf("bench-%d", i), benchPrincipal, "127.0.0.1:0", "r", "TX_READY", nil)
		r.register(entries[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := entries[nextSeq(&seq)%int64(len(entries))]
		i := 0
		for pb.Next() {
			mine.setStatus(benchStates[i&1], benchQueries[i%len(benchQueries)])
			i++
		}
	})
}

// BenchmarkTxBookkeeping_RegistryUpdateUnchanged is the same arm fed a pair that
// never moves, which is what a multi-chunk PULL produces: the state stays
// TX_STREAMING and no RUN carries a new query. It prices the publish-nothing
// guard, i.e. the part of the answer to "is the per-message refresh needed at
// all?" that says the CHECK is needed and the WRITE usually is not.
func BenchmarkTxBookkeeping_RegistryUpdateUnchanged(b *testing.B) {
	r := newTxRegistry(clock.Real())
	var seq int64
	entries := make([]*txEntry, benchEntries)
	for i := range entries {
		entries[i] = newTxEntry(fmt.Sprintf("bench-%d", i), benchPrincipal, "127.0.0.1:0", "r", "TX_STREAMING", nil)
		entries[i].setStatus("TX_STREAMING", benchQuery)
		r.register(entries[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := entries[nextSeq(&seq)%int64(len(entries))]
		for pb.Next() {
			mine.setStatus("TX_STREAMING", "")
		}
	})
}

// legacyTxEntry and legacyTxRegistry are the pre-rmp-#2714 implementation,
// transcribed here so the change has a control that runs in the SAME binary and
// the SAME interleaved -count as the arm that replaced it. Nothing outside this
// file refers to them; they are a measuring instrument, not a fallback.
//
// The transcription is deliberately literal: one process-global sync.Mutex, the
// entry reached by a map lookup taken under it, plain string fields, and the same
// "an empty query means unchanged" rule.
type legacyTxEntry struct {
	id    string
	state string
	query string
}

type legacyTxRegistry struct {
	entries map[string]*legacyTxEntry
	mu      sync.Mutex
}

func (r *legacyTxRegistry) update(id, state, query string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return
	}
	e.state = state
	if query != "" {
		e.query = query
	}
}

// BenchmarkTxBookkeeping_RegistryUpdateLegacy is the control: the per-message
// refresh as it was before rmp #2714. The delta against
// BenchmarkTxBookkeeping_RegistryUpdate is the whole of the change, because the
// two arms differ in the mechanism and in nothing else — same entry count, same
// one-entry-per-goroutine ownership, same alternating input.
func BenchmarkTxBookkeeping_RegistryUpdateLegacy(b *testing.B) {
	r := &legacyTxRegistry{entries: make(map[string]*legacyTxEntry, benchEntries)}
	var seq int64
	ids := make([]string, benchEntries)
	for i := range ids {
		ids[i] = fmt.Sprintf("bench-%d", i)
		r.entries[ids[i]] = &legacyTxEntry{id: ids[i], state: "TX_READY"}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := ids[nextSeq(&seq)%int64(len(ids))]
		i := 0
		for pb.Next() {
			r.update(mine, benchStates[i&1], benchQuery)
			i++
		}
	})
}

// perEntryMutexEntry is the alternative design rmp #2714 REJECTED, kept as an arm
// so the rejection rests on a measurement instead of an opinion.
//
// It gives every entry its own sync.Mutex instead of an atomic pointer. That also
// removes the process-global lock, also keeps a row internally consistent, and
// allocates nothing at all even when the statement text never repeats.
//
// The rejection is NARROW and the reason is not the one that was expected.
// MEASURED (rmp #2714, Apple M4, no -race, n=8 interleaved): the mutex costs
// 4.950 ns at one goroutine against the atomic pointer's 4.582 (+8.0%) and
// 0.826 ns at eight against 0.751 (+10.0%), and it scales 5.99x against 6.10x —
// both comfortably above the ladder's own 5.76x floor, and both far above the
// same-vs-same noise floor of 1.5% per level. So it is a real but small write-path
// deficit.
//
// The argument that a LISTING would separate them — one lock per entry while it
// walks the map, against one atomic load — was tested and does NOT hold: over 64,
// 512 and 4096 entries the mutex listing read 1.15x, 1.13x and 0.90x the atomic
// one, i.e. it loses at small n, WINS at large n, and the difference is about
// 3 ns per entry in either direction against a walk dominated by map iteration.
// That reason is recorded as refuted rather than quietly dropped.
//
// It is padded to txEntrySize for the same reason txEntry is, and the first
// version of this arm was NOT: at its natural 40 bytes it landed in the 48-byte
// size class, six entries to a 128-byte line, and read 12.08 ns ±70% at two
// goroutines. That was false sharing in the fixture, not a property of the design,
// and condemning the alternative on it would have been a rigged comparison.
type perEntryMutexEntry struct {
	mu    sync.Mutex
	state string
	query string
	_     [txEntrySize - 40]byte
}

func (e *perEntryMutexEntry) setStatus(state, query string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = state
	if query != "" {
		e.query = query
	}
}

// BenchmarkTxBookkeeping_RegistryUpdatePerEntryMutex prices the rejected
// alternative on the same ladder as the two arms above.
func BenchmarkTxBookkeeping_RegistryUpdatePerEntryMutex(b *testing.B) {
	var seq int64
	entries := make([]*perEntryMutexEntry, benchEntries)
	for i := range entries {
		entries[i] = &perEntryMutexEntry{state: "TX_READY"}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := entries[nextSeq(&seq)%int64(len(entries))]
		i := 0
		for pb.Next() {
			mine.setStatus(benchStates[i&1], benchQuery)
			i++
		}
	})
}

// ladderCell is one goroutine's private scratch, padded well past a cache line so
// two workers never share one. 256 bytes rather than 64 or 128 because Apple
// Silicon's line size is not something this file should assert.
type ladderCell struct {
	v int64
	_ [248]byte
}

// BenchmarkTxBookkeeping_LadderFloor is the ladder's OWN floor, and no ladder
// result should be read without it.
//
// It shares nothing: each goroutine increments a cell no other goroutine touches.
// Whatever it costs at -cpu=8 relative to -cpu=1 is what the machine charges for
// running eight goroutines rather than one, independent of any lock. On the host
// this task measured — an Apple M4 with 4 performance and 6 efficiency cores —
// that is NOT expected to be 1.00x, because eight goroutines cannot all run on
// performance cores.
//
// "Flat within the noise floor" for an update arm therefore means flat against
// THIS, not against 1.00x.
func BenchmarkTxBookkeeping_LadderFloor(b *testing.B) {
	var seq int64
	cells := make([]*ladderCell, benchEntries)
	for i := range cells {
		cells[i] = &ladderCell{}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := cells[nextSeq(&seq)%int64(len(cells))]
		for pb.Next() {
			mine.v++
		}
	})
	runtimeKeepAlive(cells)
}

// runtimeKeepAlive stops the compiler proving the ladder-floor writes dead. It is
// a function rather than a _ = assignment so the escape is unambiguous.
//
//go:noinline
func runtimeKeepAlive(c []*ladderCell) {
	if len(c) > 0 && c[0].v == -1 {
		panic("unreachable")
	}
}

// BenchmarkTxBookkeeping_RegistryChurn measures the per-TRANSACTION registry
// acquisitions: mint an id, register, unregister.
func BenchmarkTxBookkeeping_RegistryChurn(b *testing.B) {
	r := newTxRegistry(clock.Real())
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := r.nextID("s")
			r.register(newTxEntry(id, benchPrincipal, "127.0.0.1:0", "r", "TX_READY", nil))
			r.unregister(id)
		}
	})
}

// BenchmarkTxBookkeeping_QuotaEnabled measures acquire+release against the
// default cap, every goroutine on one principal.
func BenchmarkTxBookkeeping_QuotaEnabled(b *testing.B) {
	q := newTxQuota(DefaultMaxOpenTxPerPrincipal)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := q.acquire(benchPrincipal); err != nil {
				b.Error(err)
				return
			}
			q.release(benchPrincipal)
		}
	})
}

// BenchmarkTxBookkeeping_QuotaDisabled is the same call sequence against a quota
// built with a negative limit, which returns at its `q.limit <= 0` guard and
// takes no lock. The delta against QuotaEnabled is the lock plus the map.
func BenchmarkTxBookkeeping_QuotaDisabled(b *testing.B) {
	q := newTxQuota(-1)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := q.acquire(benchPrincipal); err != nil {
				b.Error(err)
				return
			}
			q.release(benchPrincipal)
		}
	})
}

// BenchmarkTxBookkeeping_QuotaDistinctPrincipals is the control that separates
// the MUTEX from the CACHE LINE. If throughput recovers when every goroutine
// owns its own map key, the cost is the shared counter; if it does not, the cost
// is the single mutex and no amount of key partitioning would help.
func BenchmarkTxBookkeeping_QuotaDistinctPrincipals(b *testing.B) {
	q := newTxQuota(DefaultMaxOpenTxPerPrincipal)
	var seq int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := fmt.Sprintf("principal-%d", nextSeq(&seq))
		for pb.Next() {
			if err := q.acquire(mine); err != nil {
				b.Error(err)
				return
			}
			q.release(mine)
		}
	})
}

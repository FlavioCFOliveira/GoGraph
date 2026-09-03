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
// registryUpdate is the acquisition the message loop performs after EVERY
// inbound message while a transaction is open: Session.reportTx (session.go:739)
// -> txRegistry.update (txregistry.go:150), called from the message loop at
// serve.go:1359. It is the highest-frequency of the four registry acquisitions.
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

// BenchmarkTxBookkeeping_RegistryUpdate measures the per-MESSAGE registry
// acquisition. Every goroutine updates its OWN entry, so the map lookup never
// conflicts and what the ladder measures is the shared mutex alone.
func BenchmarkTxBookkeeping_RegistryUpdate(b *testing.B) {
	r := newTxRegistry(clock.Real())
	// Pre-register one entry per possible worker. b.RunParallel gives no worker
	// index, so each goroutine claims an id from an atomic counter below.
	var seq int64
	ids := make([]string, 4096)
	for i := range ids {
		ids[i] = fmt.Sprintf("bench-%d", i)
		r.register(&txEntry{id: ids[i], principal: benchPrincipal, remote: "127.0.0.1:0", mode: "r"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		mine := ids[nextSeq(&seq)%int64(len(ids))]
		for pb.Next() {
			r.update(mine, "TX_READY", "RETURN 1")
		}
	})
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
			r.register(&txEntry{id: id, principal: benchPrincipal, remote: "127.0.0.1:0", mode: "r"})
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

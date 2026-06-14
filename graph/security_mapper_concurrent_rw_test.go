package graph

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// security_mapper_concurrent_rw_test.go is part of the GoGraph security
// test battery. It is a DEFENSE lock-in for the unsafe-soundness of
// [Mapper] under genuine concurrent read+write: a fleet of reader
// goroutines (Lookup, Resolve, Walk) runs against the same shards that a
// fleet of writer goroutines is concurrently mutating via Intern /
// internSlow.
//
// The Mapper's read fast path takes only an RLock and reinterprets the
// key via unsafe.Pointer in shardFor; the write path takes the Lock,
// appends to the reverse slice and writes the forward map. The standing
// concurrency gate for this package is `go test -race`. The existing
// short tests either pre-populate before reading or read only keys the
// same goroutine just interned; none drives Lookup/Resolve/Walk against a
// shard while a *different* goroutine is appending to that shard's
// reverse slice. This test fills that gap: under -race it must report
// zero data races, and every NodeID a reader observes must round-trip.
//
// It lives in package graph (no internal/shapegen dependency, so no
// import cycle) and is bounded by both an iteration budget and a wall
// deadline so it can never hang the host or leak a goroutine.

// TestSec_Core_MapperConcurrentReadWrite stresses overlapping reads and
// writes across all shards. Writers intern a disjoint, interleaved key
// range each; readers continuously Lookup/Resolve keys that may or may
// not be interned yet and Walk the whole table. The invariant: any id a
// reader resolves must map back to the value that produced it (no torn
// read of the reverse slice across a concurrent append).
func TestSec_Core_MapperConcurrentReadWrite(t *testing.T) {
	t.Parallel()

	const (
		nKeys    = 20_000
		deadline = 30 * time.Second
	)
	nWriters := max(2, runtime.GOMAXPROCS(0))
	nReaders := max(2, runtime.GOMAXPROCS(0))

	m := NewMapper[string]()
	key := func(i int) string { return "rw-" + strconv.Itoa(i) }

	var deadlineExceeded atomic.Bool
	timer := time.AfterFunc(deadline, func() { deadlineExceeded.Store(true) })
	defer timer.Stop()

	// stop is closed once every writer has finished so readers terminate
	// promptly and goleak sees a clean teardown.
	stop := make(chan struct{})
	start := make(chan struct{})
	var writers sync.WaitGroup
	var readers sync.WaitGroup
	var mismatches atomic.Int64

	// Writers: each writer interns an interleaved slice of the key space
	// (writer w handles i ≡ w mod nWriters), so the shards are touched by
	// multiple writers and the reverse slices grow under contention.
	for w := 0; w < nWriters; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			<-start
			for i := w; i < nKeys; i += nWriters {
				if deadlineExceeded.Load() {
					return
				}
				id := m.Intern(key(i))
				// A writer's own freshly-interned id must resolve back to
				// its key — this is the strongest single-goroutine
				// consistency check on the append+map-write path.
				if v, ok := m.Resolve(id); !ok || v != key(i) {
					mismatches.Add(1)
				}
			}
		}(w)
	}

	// Readers: hammer Lookup/Resolve/Walk against the live, growing table
	// until stop is closed (after every writer has finished).
	for r := 0; r < nReaders; r++ {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			<-start
			i := r
			for {
				select {
				case <-stop:
					return
				default:
				}
				if deadlineExceeded.Load() {
					return
				}
				// Lookup a key that may or may not be interned yet; if it
				// is, the returned id must Resolve back to it.
				if id, ok := m.Lookup(key(i % nKeys)); ok {
					if v, ok2 := m.Resolve(id); !ok2 || v != key(i%nKeys) {
						mismatches.Add(1)
					}
				}
				// Walk: snapshot a few pairs and verify each id's shard
				// component is internally consistent. Per the Mapper
				// contract the callback must not re-enter the Mapper, so we
				// only do the O(1) shard-component check here.
				var seen int
				m.Walk(func(id NodeID, v string) bool {
					if !m.shardComponentConsistent(id) {
						mismatches.Add(1)
					}
					_ = v
					seen++
					return seen < 64 // bound the walk so readers stay hot
				})
				i++
			}
		}(r)
	}

	close(start)
	writers.Wait() // all keys interned (or deadline fired)
	close(stop)    // release readers
	readers.Wait()

	if deadlineExceeded.Load() {
		t.Fatalf("deadline (%v) exceeded: concurrent read/write did not converge", deadline)
	}
	if got := mismatches.Load(); got != 0 {
		t.Fatalf("%d id/value round-trip mismatches under concurrent read+write", got)
	}
	if m.Len() != nKeys {
		t.Fatalf("Len() = %d after storm, want %d", m.Len(), nKeys)
	}
	t.Logf("concurrent read+write clean: %d keys, %d writers, %d readers (verify under -race)",
		nKeys, nWriters, nReaders)
}

// shardComponentConsistent is a test-only white-box helper that
// recomputes the shard component of id two independent ways — via the
// public MapperShardOf and via the private unpackNodeID — and reports
// whether they agree. It lets the concurrent reader assert NodeID
// integrity without re-entering the Mapper's locks from inside Walk.
func (m *Mapper[N]) shardComponentConsistent(id NodeID) bool {
	shardPrivate, _ := unpackNodeID(id)
	return MapperShardOf(id) == shardPrivate
}

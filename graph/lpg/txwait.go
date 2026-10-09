package lpg

// txwait.go — waiting for a bounded transaction to end (rmp #2965, round 5,
// finding R5-F2).
//
// # The defect this replaces
//
// A bounded transaction ([Graph.ApplyDurable], the durable store's commit) holds
// its versions across its WAL fsync, so a write that meets one of them must wait
// for the commit to end. It used to wait by POLLING: abort, back off for at most
// a few hundred microseconds, rerun, for up to [directWaitBudget]. Whoever
// happened to rerun first after the holder ended won, so nothing bounded how
// often one writer could lose. At 32 store committers on one node the audit
// measured 822 commits in 3 s with a p99 of 1.000 s — committers exhausting the
// whole budget and being refused — and four direct writers completed 669 writes
// between them.
//
// # The mechanism
//
// Every bounded transaction is entered in a sharded table, keyed by its
// transaction id, for exactly its lifetime. A writer refused by one of its
// versions — the conflict names the head's transaction id — ends its own attempt
// first, so it holds NO version and no lock, then parks on that entry's FIFO
// queue. When the transaction ends (published, aborted, or unwound by a panic)
// its queue is handed to the FIRST live waiter only, together with everyone
// queued behind it: that waiter's next bounded attempt is entered with the queue
// it inherited, so the others now wait for it, in arrival order, and a
// newcomer queues behind them. A direct write that inherits a queue hands it on
// as soon as its own one-operation attempt has ended.
//
// The properties the mandate asks for, and why each holds:
//
//   - No unbounded wait. Every wait carries the deadline of the writer's budget;
//     a holder whose fsync stalls times its waiters out, and they return the
//     retryable refusal they always returned.
//   - No lost wakeup. A waiter enqueues only under the shard lock and only while
//     the entry exists; the holder removes the entry under the same lock before
//     it wakes anyone, so a waiter either finds no entry — and retries at once —
//     or is in the queue the holder hands off.
//   - Released on every exit. The entry is removed by a deferred call registered
//     before the transaction can write anything, so a refusal, a failed fsync and
//     a panic all release the waiters.
//   - No deadlock. A writer waits only after its own attempt has ended, holding
//     nothing, and the transaction it waits for never waits for a waiter: a
//     bounded transaction runs no caller code and waits only for its own fsync
//     and for LOWER-sequence commits, which never wait for it.
//   - Bounded memory. One entry per in-flight bounded transaction and one
//     record per parked goroutine, both freed when they end.
//   - No allocation uncontended. The entry is a value in a map that, once
//     warm, reuses its slots; the waiter record is allocated only by a writer
//     that actually waits.
//
// Per-object serialisation across the fsync is inherent to this design and is
// not removed by it: a version is a claim held until the commit that wrote it is
// durable, so N commits of one object take at least N fsyncs. What the queue
// changes is who goes next — the oldest waiter, not the luckiest poller — and
// that waiters sleep instead of spinning.
//
// # Prior art (structure only, never code)
//
// PostgreSQL waits for a row's writer by waiting for the writer's transaction:
// XactLockTableWait takes a share lock on the transaction-id lock its owner holds
// exclusively until it ends, and rechecks TransactionIdIsInProgress in a loop
// (src/backend/storage/lmgr/lmgr.c). Before sleeping, heap_update and
// heap_lock_tuple first take the TUPLE lock "to establish our priority for the
// tuple", so the waiters of one row are served in queue order rather than by
// whoever wakes first (src/backend/access/heap/heapam.c, heap_acquire_tuplock).
// GoGraph keeps both halves — wait on the transaction, queue in order — but
// queues on the transaction rather than the object and hands the queue to the
// next owner, because its writers release everything before they wait and so
// need no deadlock detector (PostgreSQL's DeadLockCheck in
// src/backend/storage/lmgr/deadlock.c exists because its waiters sleep holding
// locks). Read at postgres/postgres commit
// 50d6e533e4d9a0f70d798c534254007c83c0d428. Memgraph is the contrasting
// choice: PrepareForWrite refuses a write over another transaction's
// uncommitted delta with SERIALIZATION_ERROR at once, leaving any retry to the
// client (memgraph/memgraph commit 3f2d6f8ed27ef6610933a218403f05f7a51a4d81,
// src/storage/v2/vertex_accessor.cpp). GoGraph rejected that for the store
// commit because its claims span an fsync, so an immediate refusal turns every
// hot-key commit into a client-side retry storm.

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// txWaiter is one goroutine parked on a bounded transaction's end.
type txWaiter struct {
	// ch receives exactly one wake, from the hand-off that chose this waiter.
	ch chan struct{}
	// state is waiterWaiting until a hand-off claims the waiter (waiterWoken)
	// or the waiter gives up (waiterAbandoned); exactly one of the two wins.
	state atomic.Uint32
	// next links the FIFO queue. Written only under the owning shard lock, or by
	// the single goroutine that holds a detached queue.
	next *txWaiter
	// inherit is the queue handed over with the wake: the waiters behind this
	// one, which now wait for this waiter's next attempt. Written by the waker
	// before the send on ch, read by this waiter after the receive.
	inherit waitQueue
}

const (
	waiterWaiting uint32 = iota
	waiterWoken
	waiterAbandoned
)

// waitQueue is a FIFO list of parked waiters. The zero value is empty.
type waitQueue struct{ head, tail *txWaiter }

// push appends w to the queue.
func (q *waitQueue) push(w *txWaiter) {
	if q.tail == nil {
		q.head, q.tail = w, w
		return
	}
	q.tail.next = w
	q.tail = w
}

// txWaitShard is one shard of [txWaitTable]. The pad, derived from the field
// sizes, makes the shard stride exactly 64 bytes, so a shard's lock and map
// share cache lines with no more than its two neighbours. It does not align
// shards to line boundaries: the table is embedded in [Graph], whose layout
// fixes where the first shard starts.
type txWaitShard struct {
	mu sync.Mutex
	m  map[uint64]waitQueue
	_  [64 - unsafe.Sizeof(sync.Mutex{}) - unsafe.Sizeof(map[uint64]waitQueue(nil))]byte
}

// txWaitShards is the shard count of [txWaitTable]: a power of two, so the
// shard is a mask of the transaction id.
const txWaitShards = 64

// txWaitTable holds one entry per in-flight bounded transaction, and the queue
// of writers waiting for it to end.
//
// Safe for concurrent use.
type txWaitTable struct {
	shards [txWaitShards]txWaitShard
	// handoffs counts waiters a hand-off has woken that have not yet run the
	// attempt they were woken for. While it is non-zero a FRESH writer — one that
	// has not been refused — defers its first attempt, for at most
	// [bargeDeferLimit], so it does not barge in front of the waiter the queue
	// chose; see [txWaitTable.yieldToHandoffs]. Read once per fresh durable
	// commit and per direct write; written only on contention.
	handoffs atomic.Int64
}

// bargeDeferLimit bounds how long a fresh writer defers to woken waiters. It is
// a priority hint, not an exclusion: past it the writer proceeds, so a woken
// waiter that never runs cannot stall anyone.
const bargeDeferLimit = 200 * time.Microsecond

// yieldToHandoffs defers a fresh writer while a hand-off is in flight, so the
// waiter the FIFO queue woke takes its claims first.
//
// # Why it is needed — measured
//
// Without it the queue was not FIFO in practice. The committer that ended a
// transaction began its NEXT commit and took the object back within
// microseconds, before the waiter it had just woken was scheduled: at 8 and 32
// store committers on one node every commit came from one goroutine chain
// (p50 4 ms) while the others timed out at the 1 s budget. PostgreSQL closes the
// same gap with the tuple lock: a newcomer must queue on it before it may wait
// for, or take, the row (src/backend/access/heap/heapam.c,
// heap_acquire_tuplock). GoGraph has no per-object lock to queue on, so it
// defers newcomers for the short window in which a woken waiter is reclaiming.
func (t *txWaitTable) yieldToHandoffs() {
	if t.handoffs.Load() == 0 {
		return
	}
	deadline := time.Now().Add(bargeDeferLimit)
	for t.handoffs.Load() > 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

// handoffDone retires one hand-off token: the woken waiter has run its attempt.
func (t *txWaitTable) handoffDone() { t.handoffs.Add(-1) }

func (t *txWaitTable) shard(txID uint64) *txWaitShard {
	return &t.shards[txID&(txWaitShards-1)]
}

// enter records txID as in flight, carrying the queue its owner inherited.
func (t *txWaitTable) enter(txID uint64, inherited waitQueue) {
	sh := t.shard(txID)
	sh.mu.Lock()
	if sh.m == nil {
		sh.m = make(map[uint64]waitQueue, 8)
	}
	sh.m[txID] = inherited
	sh.mu.Unlock()
}

// leave removes txID and wakes the first live waiter of its queue, handing it
// the rest. It must run on every exit of the transaction, after its versions
// have been published or withdrawn.
func (t *txWaitTable) leave(txID uint64) {
	sh := t.shard(txID)
	sh.mu.Lock()
	q := sh.m[txID]
	delete(sh.m, txID)
	sh.mu.Unlock()
	t.handOff(q)
}

// handOff wakes the first waiter of q that has not given up, and hands it the
// waiters behind it. Abandoned waiters are skipped and dropped. The woken
// waiter owes one [txWaitTable.handoffDone].
func (t *txWaitTable) handOff(q waitQueue) {
	for w := q.head; w != nil; {
		next := w.next
		w.next = nil
		if w.state.CompareAndSwap(waiterWaiting, waiterWoken) {
			t.handoffs.Add(1)
			if next == nil {
				w.inherit = waitQueue{}
			} else {
				w.inherit = waitQueue{head: next, tail: q.tail}
			}
			w.ch <- struct{}{}
			return
		}
		w = next
	}
}

// wait parks the caller until the bounded transaction txID ends, deadline
// passes, or ctx is done. It reports whether the caller should retry — true
// when txID ended, or had already ended — the queue the caller inherited, which
// it must enter with its next bounded attempt or hand on with
// [txWaitTable.handOff], and whether a hand-off woke it, in which case it owes
// one [txWaitTable.handoffDone] once its next attempt has run.
//
// When ctx is done first, wait returns ctx's error and owes the caller nothing:
// it leaves the queue exactly as a timed-out waiter does, and if a hand-off
// chose it at the same moment it retires that hand-off's token and hands the
// inherited queue on itself, so no waiter behind it is stranded. A ctx whose
// Done channel is nil ([context.Background]) adds no work and no allocation.
//
// The caller must hold no version and no lock.
func (t *txWaitTable) wait(ctx context.Context, txID uint64, deadline time.Time) (inherited waitQueue, retry, woken bool, err error) {
	if err := ctx.Err(); err != nil {
		return waitQueue{}, false, false, err
	}
	sh := t.shard(txID)
	sh.mu.Lock()
	q, inFlight := sh.m[txID]
	if !inFlight {
		sh.mu.Unlock()
		return waitQueue{}, true, false, nil
	}
	w := &txWaiter{ch: make(chan struct{}, 1)}
	q.push(w)
	sh.m[txID] = q
	sh.mu.Unlock()
	metrics.IncCounter("lpg.mvcc.commit_waits", 1)

	d := time.Until(deadline)
	if d <= 0 {
		d = time.Nanosecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	cancelled := false
	select {
	case <-w.ch:
		return w.inherit, true, true, nil
	case <-timer.C:
	case <-ctx.Done():
		cancelled = true
	}
	if w.state.CompareAndSwap(waiterWaiting, waiterAbandoned) {
		if cancelled {
			metrics.IncCounter("lpg.mvcc.commit_wait_cancels", 1)
			return waitQueue{}, false, false, ctx.Err()
		}
		metrics.IncCounter("lpg.mvcc.commit_wait_timeouts", 1)
		return waitQueue{}, false, false, nil
	}
	// A hand-off chose this waiter as the timer fired or ctx finished; its send
	// is imminent. Take it and the queue that came with it — dropping the queue
	// would strand every waiter behind this one until its deadline.
	<-w.ch
	if cancelled {
		// The caller will not run the attempt it was woken for: retire the
		// token and pass the queue to the next live waiter now.
		t.handoffDone()
		t.handOff(w.inherit)
		metrics.IncCounter("lpg.mvcc.commit_wait_cancels", 1)
		return waitQueue{}, false, false, ctx.Err()
	}
	return w.inherit, true, true, nil
}

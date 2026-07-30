// Package ctxlock acquires an exclusive lock while honouring a
// [context.Context] deadline or cancellation.
//
// # Why this exists
//
// Neither [sync.Mutex.Lock] nor [sync.RWMutex.Lock] can be cancelled: once
// called, the goroutine blocks until it owns the lock, whatever its caller's
// deadline says. Every GoGraph API that may block is required to accept a
// context and honour it (see CLAUDE.md, "Context-aware blocking"), and the
// round-3 comparative audit measured the consequence of not doing so on the
// transaction path: Engine.BeginTx with a 50 ms deadline returned after 601 ms
// in one run and after 11.60 s under load — a 232x overrun — and in both cases
// returned err=nil, handing back a live transaction that held the writer
// serialisation and the global visibility barrier on an already-expired context
// (rmp #2174).
//
// # Contract
//
// [Acquire] is not a replacement for a mutex: it is the acquire half only, for
// the specific case of an exclusive lock whose wait must be bounded by a
// context. The caller keeps ownership of the lock and releases it as usual.
package ctxlock

import (
	"context"
	"sync/atomic"
)

// Acquire takes an exclusive lock, returning nil once it is held and the
// context's error if the context finishes first.
//
// tryLock must be the lock's non-blocking acquire (sync.Mutex.TryLock,
// sync.RWMutex.TryLock); lock and unlock its blocking pair. All three must
// address the SAME lock, and lock must be the exclusive (write) acquire — a
// shared acquire would let Acquire report success while another writer holds it.
//
// The context is checked before anything else, so an already-expired context
// never acquires the lock at all.
//
// # How the wait is bounded
//
// The fast path is tryLock, which succeeds whenever the lock is free — the
// common case, and it costs no goroutine and no allocation. Only a CONTENDED
// acquire moves the wait onto a helper goroutine, so that the caller can stop
// waiting when its context finishes. tryLock is not retried in a loop: a
// polling loop does not queue on the lock, so under a steady arrival of shared
// acquirers an exclusive acquirer could be starved indefinitely, whereas a
// blocking Lock queues and (for sync.RWMutex) stops admitting new readers.
//
// When the context finishes first, the helper's queued acquire cannot be
// withdrawn — Go offers no way to leave a lock queue. The helper is therefore
// left to complete the acquisition and release it immediately. Its lifetime is
// bounded, not open-ended: it is waiting on a lock that the current holder must
// release, and for sync.RWMutex the queued exclusive acquire also blocks new
// shared acquirers, so no arrival stream can extend the wait. Between that
// acquisition and its immediate release the lock is briefly held by no logical
// owner, which delays other acquirers by the cost of one scheduling hop and
// mutates nothing.
//
// A caller that must observe the lock becoming free again — a test, typically —
// can poll tryLock.
//
// # What is bounded here, and what is not (rmp #2260)
//
// ONE helper per contended acquire, never two. An earlier form spawned a SECOND
// goroutine on the abandonment path whose only job was to wait for the first and
// unlock, so every abandoned acquire cost two; the atomic handoff in [Acquire]
// folds that job into the helper itself.
//
// The remaining transient is NOT bounded, and its shape matters because it is
// reachable from a client. The live helper count tracks
// ARRIVAL RATE × HOLDER TENURE — not the number of concurrent callers. Each
// abandoned attempt parks one helper until the holder releases, so a caller that
// retries in a tight loop against a long-held lock accumulates them in proportion
// to how long the holder keeps it. Measured against a barrier held for 3 s by
// acquirers with a 2 ms deadline, the previous two-goroutine form reached 597 819
// live goroutines and 1 677 MiB of process memory from 256 callers; halving the
// per-attempt cost does not change that asymptote.
//
// The reachable path is an explicit transaction: a Bolt client supplies
// tx_timeout, and an explicit transaction holds the visibility barrier for its
// whole lifetime, so holder tenure can be tens of seconds. Read-only BEGIN does
// not take the barrier and is unaffected.
//
// Removing the transient altogether would mean refusing an acquire once some
// admission limit is reached, which turns this blocking call into one that can
// fail and so changes the contract of every caller. That is a deliberate
// omission recorded here, not an oversight.
func Acquire(ctx context.Context, tryLock func() bool, lock, unlock func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tryLock() {
		return nil
	}

	// The three-way CAS below is load-bearing. A plain boolean "the caller gave
	// up" flag RACES: the helper can read it as false and publish the lock at the
	// same moment the caller takes the ctx.Done branch, leaving the lock held with
	// no logical owner and nobody to release it. With a CAS exactly one side wins,
	// and each side's losing branch knows that cleaning up is its job.
	const (
		stateWaiting   int32 = 0 // neither side has claimed the acquisition yet
		stateHandedOff int32 = 1 // the helper published it; the caller owns the lock
		stateAbandoned int32 = 2 // the caller gave up first; the helper must unlock
	)
	var state atomic.Int32

	acquired := make(chan struct{})
	go func() {
		lock()
		if state.CompareAndSwap(stateWaiting, stateHandedOff) {
			// The caller is still waiting: publish, and let it own the lock.
			close(acquired)
			return
		}
		// The caller abandoned first. Nothing has been done under the lock, so
		// releasing it immediately is correct.
		unlock()
	}()

	select {
	case <-acquired:
		// Held. Re-check the context so a deadline that elapsed while queued is
		// still reported, rather than returning a lock the caller may no longer
		// use. Releasing here is correct: nothing has been done under it.
		if err := ctx.Err(); err != nil {
			unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		if state.CompareAndSwap(stateWaiting, stateAbandoned) {
			// We won the race: the helper has not published yet, and it will
			// unlock once it acquires. No second goroutine is needed.
			return ctx.Err()
		}
		// The helper published between ctx firing and this CAS, so the lock IS
		// held and we are its owner — releasing it is now our job. acquired is
		// already closed, so this receive cannot block.
		<-acquired
		unlock()
		return ctx.Err()
	}
}

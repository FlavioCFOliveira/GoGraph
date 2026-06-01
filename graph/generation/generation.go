// Package generation publishes immutable graph snapshots
// (typically [csr.CSR] views) under a refcount-protected pointer so
// readers can observe a consistent generation while a new one is
// being prepared in the background.
//
// The pattern is the read-mostly equivalent of an MVCC snapshot:
// every reader Acquires the current generation, uses it, and
// Releases it; a publisher prepares the next generation in a fresh
// allocation and atomically swaps the pointer. Old generations are
// reclaimed only after every outstanding reader has Released them.
package generation

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// ErrDrainTimeout is returned by [Publisher.PublishWithDrain] when
// the old generation still has outstanding readers after the
// configured timeout.
var ErrDrainTimeout = errors.New("generation: drain timeout")

// ErrClosed is returned by Publish when the Publisher has been closed.
var ErrClosed = errors.New("generation: publisher closed")

// Generation wraps an immutable [csr.CSR] snapshot with a refcount.
// Generation is safe for concurrent use; Acquire/Release on the
// same generation can run from any number of goroutines.
type Generation[W any] struct {
	csr      *csr.CSR[W]
	refcount atomic.Int64
	// drainCond signals on the refcount-reaches-zero edge so that
	// PublishWithDrain can sleep instead of busy-polling.
	drainMu   sync.Mutex
	drainCond *sync.Cond
}

// CSR returns the underlying snapshot. The pointer is valid only
// while at least one Acquire is outstanding on g.
func (g *Generation[W]) CSR() *csr.CSR[W] { return g.csr }

// Refcount returns the current refcount. Intended for observability;
// callers must not infer correctness from it because the value can
// race with concurrent Acquire / Release.
func (g *Generation[W]) Refcount() int64 { return g.refcount.Load() }

// Publisher owns the current generation and serialises publication
// of new ones. It is safe for concurrent reads (Acquire/Release) and
// for at most one publisher (Publish/PublishWithDrain).
type Publisher[W any] struct {
	current atomic.Pointer[Generation[W]]
	closed  atomic.Bool
}

// newGeneration constructs a Generation with its sync.Cond wired up.
func newGeneration[W any](c *csr.CSR[W]) *Generation[W] {
	g := &Generation[W]{csr: c}
	g.drainCond = sync.NewCond(&g.drainMu)
	return g
}

// New returns a Publisher seeded with the given CSR as generation 0.
func New[W any](initial *csr.CSR[W]) *Publisher[W] {
	p := &Publisher[W]{}
	p.current.Store(newGeneration(initial))
	return p
}

// Acquire returns the current generation with its refcount
// incremented. The caller must eventually call Release on the
// returned generation to allow stale generations to be reclaimed.
func (p *Publisher[W]) Acquire() *Generation[W] {
	for {
		g := p.current.Load()
		if g == nil {
			return nil
		}
		// Increment refcount before re-checking the pointer; if the
		// publisher swapped concurrently we must retry on the new
		// generation so we never leave a stale refcount behind.
		g.refcount.Add(1)
		if g == p.current.Load() {
			return g
		}
		g.refcount.Add(-1)
	}
}

// Release decrements g's refcount and signals any goroutine waiting
// inside [Publisher.PublishWithDrain] for the refcount to hit zero.
func (p *Publisher[W]) Release(g *Generation[W]) {
	if g == nil {
		return
	}
	if g.refcount.Add(-1) == 0 && g.drainCond != nil {
		g.drainMu.Lock()
		g.drainCond.Broadcast()
		g.drainMu.Unlock()
	}
}

// Publish atomically swaps in a fresh generation built from c and
// returns the new generation. Returns (nil, [ErrClosed]) when the
// Publisher has been closed. The previous generation is not reclaimed
// until its refcount drains to zero (which happens naturally as
// readers Release).
func (p *Publisher[W]) Publish(c *csr.CSR[W]) (*Generation[W], error) {
	if p.closed.Load() {
		return nil, ErrClosed
	}
	next := newGeneration(c)
	p.current.Store(next)
	return next, nil
}

// PublishWithDrain swaps in a fresh generation and blocks until the
// previous generation's refcount reaches zero, or returns
// [ErrDrainTimeout] when the timeout elapses first. Returns
// (nil, [ErrClosed]) when the Publisher has been closed. Callers
// that need to recycle the previous generation's backing storage
// (e.g. to unmap a Tier 2 file) should prefer this variant.
//
// A timeout of zero disables the deadline; PublishWithDrain then
// blocks indefinitely.
func (p *Publisher[W]) PublishWithDrain(c *csr.CSR[W], timeout time.Duration) (*Generation[W], error) {
	if p.closed.Load() {
		return nil, ErrClosed
	}
	prev := p.current.Load()
	next := newGeneration(c)
	p.current.Store(next)
	if prev == nil {
		return next, nil
	}
	// Wait for prev's refcount to reach zero. We use the sync.Cond
	// broadcast from Release so this path is event-driven, not a
	// busy-poll. The timeout (if any) is enforced by a single goroutine
	// that broadcasts after the deadline so the wait loop can wake and
	// check the deadline.
	timedOut := atomic.Bool{}
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			timedOut.Store(true)
			prev.drainMu.Lock()
			prev.drainCond.Broadcast()
			prev.drainMu.Unlock()
		})
		defer timer.Stop()
	}
	prev.drainMu.Lock()
	for prev.refcount.Load() > 0 {
		if timedOut.Load() {
			prev.drainMu.Unlock()
			return next, ErrDrainTimeout
		}
		prev.drainCond.Wait()
	}
	prev.drainMu.Unlock()
	return next, nil
}

// Close marks the Publisher as closed, waits for all outstanding
// acquisitions to drain (with a 30 s safety deadline), and returns.
// After Close returns, Acquire returns nil and Publish returns
// [ErrClosed].
//
// Close is safe to call from any goroutine. Calling Close more than
// once is safe; subsequent calls are no-ops.
func (p *Publisher[W]) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return // already closed
	}
	// Atomically replace the current generation with nil so that new
	// Acquire calls see a closed publisher and return nil immediately.
	prev := p.current.Swap(nil)
	if prev == nil {
		return
	}
	// Wait for any outstanding readers to Release.
	const drainDeadline = 30 * time.Second
	timedOut := atomic.Bool{}
	timer := time.AfterFunc(drainDeadline, func() {
		timedOut.Store(true)
		prev.drainMu.Lock()
		prev.drainCond.Broadcast()
		prev.drainMu.Unlock()
	})
	defer timer.Stop()
	prev.drainMu.Lock()
	for prev.refcount.Load() > 0 && !timedOut.Load() {
		prev.drainCond.Wait()
	}
	prev.drainMu.Unlock()
}

// Current returns the current generation without incrementing its
// refcount. Intended for introspection only; never use the returned
// pointer to access the CSR.
func (p *Publisher[W]) Current() *Generation[W] {
	return p.current.Load()
}

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
	"sync/atomic"
	"time"

	"gograph/graph/csr"
)

// ErrDrainTimeout is returned by [Publisher.PublishWithDrain] when
// the old generation still has outstanding readers after the
// configured timeout.
var ErrDrainTimeout = errors.New("generation: drain timeout")

// Generation wraps an immutable [csr.CSR] snapshot with a refcount.
// Generation is safe for concurrent use; Acquire/Release on the
// same generation can run from any number of goroutines.
type Generation[W any] struct {
	csr      *csr.CSR[W]
	refcount atomic.Int64
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
}

// New returns a Publisher seeded with the given CSR as generation 0.
func New[W any](initial *csr.CSR[W]) *Publisher[W] {
	p := &Publisher[W]{}
	g := &Generation[W]{csr: initial}
	p.current.Store(g)
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

// Release decrements g's refcount.
func (p *Publisher[W]) Release(g *Generation[W]) {
	if g == nil {
		return
	}
	g.refcount.Add(-1)
}

// Publish atomically swaps in a fresh generation built from c and
// returns the new generation. The previous generation is not
// reclaimed until its refcount drains to zero (which happens
// naturally as readers Release).
func (p *Publisher[W]) Publish(c *csr.CSR[W]) *Generation[W] {
	next := &Generation[W]{csr: c}
	p.current.Store(next)
	return next
}

// PublishWithDrain swaps in a fresh generation and blocks until the
// previous generation's refcount reaches zero, or returns
// [ErrDrainTimeout] when the timeout elapses first. Callers that
// need to recycle the previous generation's backing storage (e.g.
// to unmap a Tier 2 file) should prefer this variant.
//
// A timeout of zero disables the deadline; PublishWithDrain then
// blocks indefinitely.
func (p *Publisher[W]) PublishWithDrain(c *csr.CSR[W], timeout time.Duration) (*Generation[W], error) {
	prev := p.current.Load()
	next := &Generation[W]{csr: c}
	p.current.Store(next)
	if prev == nil {
		return next, nil
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for prev.refcount.Load() > 0 {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return next, ErrDrainTimeout
		}
		time.Sleep(50 * time.Microsecond)
	}
	return next, nil
}

// Current returns the current generation without incrementing its
// refcount. Intended for introspection only; never use the returned
// pointer to access the CSR.
func (p *Publisher[W]) Current() *Generation[W] {
	return p.current.Load()
}

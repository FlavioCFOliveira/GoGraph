package explain

import (
	"context"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// ─────────────────────────────────────────────────────────────────────────────
// DbHitsCounter
// ─────────────────────────────────────────────────────────────────────────────

// DbHitsCounter is a per-pipeline counter for logical storage accesses
// (index lookups, property reads, CSR neighbour scans). It is incremented
// by instrumented operator wrappers and read by the PROFILE reporter.
//
// DbHitsCounter is safe for concurrent use.
type DbHitsCounter struct {
	n atomic.Uint64
}

// Add increments the counter by delta.
func (c *DbHitsCounter) Add(delta uint64) {
	c.n.Add(delta)
}

// Load returns the current counter value.
func (c *DbHitsCounter) Load() uint64 {
	return c.n.Load()
}

// Reset sets the counter back to zero.
func (c *DbHitsCounter) Reset() {
	c.n.Store(0)
}

// ─────────────────────────────────────────────────────────────────────────────
// InstrumentedScan
// ─────────────────────────────────────────────────────────────────────────────

// InstrumentedScan is a thin wrapper that counts dbHits per Next call.
// It adds 1 dbHit per row fetched from the underlying scan operator.
//
// InstrumentedScan is NOT safe for concurrent use.
type InstrumentedScan struct {
	inner   exec.Operator
	counter *DbHitsCounter
}

// NewInstrumentedScan wraps op with a dbHit counter. Each successful Next call
// adds 1 to counter.
func NewInstrumentedScan(op exec.Operator, counter *DbHitsCounter) *InstrumentedScan {
	return &InstrumentedScan{inner: op, counter: counter}
}

// Init implements [exec.Operator]. It delegates to the inner operator.
func (s *InstrumentedScan) Init(ctx context.Context) error {
	return s.inner.Init(ctx)
}

// Next implements [exec.Operator]. It delegates to the inner operator and on a
// successful (true, nil) return increments the dbHits counter by 1.
func (s *InstrumentedScan) Next(out *exec.Row) (bool, error) {
	ok, err := s.inner.Next(out)
	if ok && err == nil {
		s.counter.Add(1)
	}
	return ok, err
}

// Close implements [exec.Operator]. It delegates to the inner operator.
func (s *InstrumentedScan) Close() error {
	return s.inner.Close()
}

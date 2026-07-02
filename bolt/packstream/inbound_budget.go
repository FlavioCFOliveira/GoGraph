package packstream

import (
	"errors"
	"sync/atomic"
)

// inboundReserveChunk is the granularity at which a [Decoder] draws from a
// shared [InboundBudget]: a decode charges the shared atomic in ~1 MiB steps
// rather than once per collection, keeping atomic contention to a handful of
// operations per message. The unused tail of the last reserved chunk is the
// only overshoot a decoder holds beyond what it has actually charged — at most
// this many bytes per in-flight connection.
const inboundReserveChunk = 1 << 20 // 1 MiB

// ErrInboundBudgetExceeded is returned by the decoder when a message's
// decoded-collection charge would push the engine-wide inbound-decode memory
// total past its ceiling. It is a TRANSIENT backpressure signal, not a
// malformed-message fault: a server should reject the message with a retryable
// status and keep the connection alive.
var ErrInboundBudgetExceeded = errors.New("packstream: inbound decode memory budget exceeded")

// InboundBudget is an engine-wide (per-Server) ceiling on the total decoded-
// collection memory in flight across every connection's decoder at once.
//
// The per-message decoded-collection budget (maxDecodedCollectionBytes) bounds
// a SINGLE message; without an aggregate bound, that per-message cap times the
// connection limit is unbounded and pre-authentication-reachable — a
// memory-exhaustion denial of service (CWE-770). One InboundBudget is shared by
// every decoder a Server drives, so a flood of concurrent large messages is
// bounded in aggregate: once the pool is drawn down, further charges fail fast
// with [ErrInboundBudgetExceeded] (backpressure) instead of allocating.
//
// It mirrors the engine-wide result-memory ceiling
// (cypher.EngineOptions.GlobalMaxResultBytes) on the inbound-decode side. A nil
// *InboundBudget, or one with limit <= 0, disables the ceiling (unlimited).
//
// InboundBudget is safe for concurrent use: limit is immutable after
// construction and remaining is atomic.
type InboundBudget struct {
	limit     int64        // total pool bytes; <= 0 disables the ceiling
	remaining atomic.Int64 // bytes currently available in the shared pool
}

// NewInboundBudget returns a budget with the given ceiling in bytes. A value
// <= 0 disables the ceiling (the returned budget is a permanent no-op).
func NewInboundBudget(limit int64) *InboundBudget {
	b := &InboundBudget{limit: limit}
	if limit > 0 {
		b.remaining.Store(limit)
	}
	return b
}

// enabled reports whether the budget imposes a ceiling.
func (b *InboundBudget) enabled() bool { return b != nil && b.limit > 0 }

// tryReserve atomically removes n bytes from the pool, returning true on
// success. It never blocks: an insufficient pool returns false (fail-fast
// backpressure). A disabled budget always succeeds without touching state.
func (b *InboundBudget) tryReserve(n int64) bool {
	if !b.enabled() || n <= 0 {
		return true
	}
	for {
		cur := b.remaining.Load()
		if cur < n {
			return false
		}
		if b.remaining.CompareAndSwap(cur, cur-n) {
			return true
		}
	}
}

// release returns n bytes to the pool. A disabled budget is a no-op.
func (b *InboundBudget) release(n int64) {
	if !b.enabled() || n <= 0 {
		return
	}
	b.remaining.Add(n)
}

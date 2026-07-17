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

// Enabled reports whether the budget imposes a ceiling. A nil *InboundBudget, or
// one constructed with limit <= 0, is disabled and reports false.
//
// It is the exported form of the internal enabled check, so a framing-layer
// charger outside this package (the Bolt message-reassembly reader,
// [github.com/FlavioCFOliveira/GoGraph/bolt/proto.ChunkedReader]) can skip all
// accounting — and its per-charge atomic — when the operator has not opted into
// an inbound-memory ceiling. The default configuration therefore pays nothing.
//
// Safe for concurrent use.
func (b *InboundBudget) Enabled() bool { return b.enabled() }

// TryReserve atomically removes n bytes from the shared pool, returning true on
// success and false when the pool cannot satisfy the charge (fail-fast
// backpressure, no side effect on failure). It never blocks.
//
// It is the exported entry point through which the Bolt message-reassembly
// buffer charges its transient bytes against the SAME engine-wide ceiling as the
// decoder, so aggregate inbound memory (reassembly + decode) is bounded
// centrally rather than only implicitly by MaxConnections × MaxMessageBytes.
// Pair every successful reservation with a matching [InboundBudget.Release]. A
// disabled budget always succeeds without touching state.
//
// Safe for concurrent use.
func (b *InboundBudget) TryReserve(n int64) bool { return b.tryReserve(n) }

// Release returns n bytes previously taken with [InboundBudget.TryReserve] to
// the shared pool. It is the symmetric counterpart of TryReserve: a
// reassembly-buffer charger releases exactly what it reserved once the message
// is assembled (or its read aborts). A disabled budget is a no-op.
//
// Safe for concurrent use.
func (b *InboundBudget) Release(n int64) { b.release(n) }

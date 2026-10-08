package graph

// mapper_reserve.go — id reservations (WAL v2 step 4, rmp #3021 B,
// docs/design-wal-v2.md §5).
//
// A durable store must never issue, after a restart, an id that a durable record
// or a checkpoint could already reference. The mapper therefore issues a new id
// only below a per-shard limit that a logged reservation covers. The reservation
// is recorded by an [IDReserver] the store installs; a mapper with none installed
// (an in-memory graph) interns without limit and pays one atomic load per new key.

import (
	"math"
	"sync/atomic"
)

// IDReserver records id reservations for a [Mapper] (WAL v2 step 4). The mapper
// calls it from its interning critical section, holding the shard's write lock,
// so an implementation must never take a mapper lock, directly or by waiting on a
// goroutine that does. It publishes each reservation it records through
// [Mapper.PublishReservation].
//
// Implementations must be safe for concurrent use: different shards call it at
// the same time.
type IDReserver interface {
	// ReserveIDs returns once the limit of shard is at least need — a reservation
	// covering every intra index below need is recorded and published — or once
	// the reserver can record nothing more and has published an unlimited limit
	// (math.MaxUint64). It runs synchronously on the caller's goroutine, so the
	// reservations a sequence of interns produces are a function of that
	// sequence alone.
	ReserveIDs(shard int, need uint64)
}

// idReserverRef boxes an [IDReserver] for the mapper's atomic pointer.
type idReserverRef struct{ r IDReserver }

// SetIDReserver installs r as the mapper's id reserver, or removes it when r is
// nil, and resets every shard's reserved limit to zero: the first new key of each
// shard then reserves through r.
//
// Call it before the mapper is shared by concurrent interners — a store calls it
// at construction. A key interned concurrently with the call may be issued under
// the previous reserver's limits.
func (m *Mapper[N]) SetIDReserver(r IDReserver) {
	for i := range m.shards {
		m.shards[i].idLimit.Store(0)
	}
	if r == nil {
		m.reserver.Store(nil)
		return
	}
	m.reserver.Store(&idReserverRef{r: r})
}

// PublishReservation raises shard's reserved limit to limit; a value below the
// current one is ignored, so publications may arrive in any order. limit
// math.MaxUint64 makes the shard unlimited. Lock-free; safe for concurrent use,
// including from inside the mapper's interning critical section.
func (m *Mapper[N]) PublishReservation(shard int, limit uint64) {
	storeMax(&m.shards[shard].idLimit, limit)
}

// ReservedLimit returns shard's reserved limit: 0 when nothing is reserved or no
// reserver is installed, math.MaxUint64 when the shard is unlimited. Lock-free;
// safe for concurrent use.
func (m *Mapper[N]) ReservedLimit(shard int) uint64 {
	return m.shards[shard].idLimit.Load()
}

// ReservedLimits returns every shard's reserved limit, for a checkpoint's
// nodeids.bin (docs/design-wal-v2.md §2.4, §3.3). An unlimited shard reads 0: its
// reserver can record nothing more, so no durable record can name an id issued
// past the shard's high-water mark, which the checkpoint takes from the mapper
// itself. Lock-free; safe for concurrent use.
func (m *Mapper[N]) ReservedLimits() [MapperShards]uint64 {
	var out [MapperShards]uint64
	for i := range m.shards {
		if v := m.shards[i].idLimit.Load(); v != math.MaxUint64 {
			out[i] = v
		}
	}
	return out
}

// RestoreNext sets shard's high-water mark to next, or to the length of its
// reverse table when that is larger: the indices from there to next become holes
// the next new key skips. Recovery only (WAL v2 step 4): it applies the marks the
// log's reservations and its clean-close record name, which may lie below a
// snapshot's marks. Safe for concurrent use; recovery calls it from one
// goroutine.
func (m *Mapper[N]) RestoreNext(shard int, next uint64) {
	s := &m.shards[shard]
	s.mu.Lock()
	s.next = max(next, uint64(len(s.reverse)))
	s.mu.Unlock()
}

// storeMax raises a to v unless it already holds v or more.
func storeMax(a *atomic.Uint64, v uint64) {
	for {
		cur := a.Load()
		if cur >= v || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

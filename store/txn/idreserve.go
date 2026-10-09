package txn

// idreserve.go — node id reservations (WAL v2 step 4, rmp #3021 B,
// docs/design-wal-v2.md §5).
//
// A durable store issues a new node id only below a per-shard limit that a
// logged ReserveIDs record covers, so a restart that rebuilds the limits from the
// log never reissues an id that a durable record could name. The records are
// appended without an fsync: any id taken from a reservation reaches disk only
// inside a transaction's run, which the WAL orders after the reservation and
// fsyncs. A clean close appends the exact marks (NextIDsExact), so a clean
// restart wastes no id; a crash restart wastes at most one batch per shard.
//
// Reservation is synchronous: the interner that reaches its shard's limit
// appends the reservation itself, under its shard lock. There is no background
// goroutine, so the WAL holds the same frames in the same order for the same
// sequence of operations (user decision, 2026-10-08: determinism of the
// simulator and of every reproducibility test).
//
// Prior art: PostgreSQL's OID counter (src/backend/access/transam/varsup.c:32,
// 620-626, GetNewObjectId, which logs the next batch under OidGenLock;
// xlog.c:8985-9008, XLogPutNextOid, which does not flush; xlog.c:7943-7946, an
// online checkpoint records nextOid + oidCount; xlog.c:9304-9312, a shutdown
// checkpoint is believed exactly; PostgreSQL commit
// 10cc5aa96a6df7cb1d7c47e978cc94f01f785cb7). Ideas only.

import (
	"math"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const (
	// DefaultIDBatch is the number of ids a shard's first reservation covers.
	DefaultIDBatch = 64
	// MaxIDBatch bounds the doubling of a shard's batch.
	MaxIDBatch = 65536
)

// idReserver records reservations for one store's mapper. It implements
// [graph.IDReserver].
//
// Lock order: the mapper calls it holding a shard's write lock; it takes the WAL
// writer's mutex through [wal.Writer.AppendRun]. The WAL writer never takes a
// mapper lock (docs/design-wal-v2.md §5.2).
type idReserver[N comparable] struct {
	wlog *wal.Writer
	m    *graph.Mapper[N]

	// batch is each shard's next batch size, 0 meaning [DefaultIDBatch]. Written
	// only inside an AppendRun callback (under the WAL writer's mutex); atomic so
	// a test can read it.
	batch [graph.MapperShards]atomic.Uint32
	// frame is the reservation payload scratch, used only inside an AppendRun
	// callback (under the WAL writer's mutex).
	frame [wal.ReserveIDsSize]byte
	// closed makes close idempotent.
	closed atomic.Bool
}

// newIDReserver returns a reserver appending to wlog for m.
func newIDReserver[N comparable](m *graph.Mapper[N], wlog *wal.Writer) *idReserver[N] {
	return &idReserver[N]{wlog: wlog, m: m}
}

// ReserveIDs implements [graph.IDReserver]: it appends one reservation of shard
// covering need plus the shard's batch, under the caller's shard lock, and
// doubles the shard's batch for its next reservation, up to [MaxIDBatch]. A
// shard that creates many nodes therefore logs O(log n) reservations, and a
// crash wastes at most the last batch.
func (r *idReserver[N]) ReserveIDs(shard int, need uint64) {
	_, err := r.wlog.AppendRun(func(emit func([]byte) error) error {
		cur := r.m.ReservedLimit(shard)
		if cur >= need {
			return nil // already covered, or unlimited
		}
		k := r.batchOf(shard)
		limit := need + k
		//nolint:gosec // G115: shard < 256
		if err := emit(wal.AppendReserveIDs(r.frame[:0], uint8(shard), limit)); err != nil {
			return err
		}
		r.m.PublishReservation(shard, limit)
		if k < MaxIDBatch {
			r.batch[shard].Store(uint32(min(2*k, MaxIDBatch)))
		}
		return nil
	})
	if err != nil {
		r.fail()
		return
	}
	crashpoint.Breakpoint("wal.reserve.appended-pre-fsync")
}

func (r *idReserver[N]) batchOf(shard int) uint64 {
	if k := r.batch[shard].Load(); k != 0 {
		return uint64(k)
	}
	return DefaultIDBatch
}

// fail makes every shard unlimited after an append failed. The WAL writer then
// is closed or will poison at its next sync (a failed frame write leaves its
// buffer's sticky error), so no id issued from here on can reach a durable
// record.
func (r *idReserver[N]) fail() {
	metrics.IncCounter("store.txn.idReserve.failed", 1)
	for s := 0; s < graph.MapperShards; s++ {
		r.m.PublishReservation(s, math.MaxUint64)
	}
}

// close appends the exact marks, once.
func (r *idReserver[N]) close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	next := r.m.Watermark().Next()
	_, err := r.wlog.AppendRun(func(emit func([]byte) error) error {
		return emit(wal.AppendNextIDsExact(make([]byte, 0, 2+graph.MapperShards*3), &next))
	})
	if err != nil {
		return err
	}
	crashpoint.Breakpoint("wal.close.exactnext-appended-pre-fsync")
	return nil
}

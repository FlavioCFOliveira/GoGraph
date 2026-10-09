package recovery

// idreplay.go — replay of the node id records (WAL v2 step 4, rmp #3021 B,
// docs/design-wal-v2.md §4 items 9–10, §5).

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ErrIDBeyondReservation is returned by [Open] (and is [Result.TailErr]) when a
// commit marker's id annex names a node id at or above every reservation the log
// and the snapshot record for its shard, in a shard the writer was reserving ids
// in. The writer logs a reservation before it issues an id, so the record is
// corrupt. Fail-stop (WAL v2 step 4).
var ErrIDBeyondReservation = errors.New("recovery: commit annex names a node id beyond every reservation")

// ErrIDRecordCorrupt is returned by [Open] (and is [Result.TailErr]) when a
// ReserveIDs or NextIDsExact control record does not parse. Fail-stop (WAL v2
// step 4).
var ErrIDRecordCorrupt = errors.New("recovery: node id control record is corrupt")

// idReplay derives each shard's node id high-water mark from the id records the
// replay reads, in log order:
//
//   - it starts from the larger of the mapper's mark after the snapshot load and
//     the snapshot's nodeids.bin mark;
//   - a ReserveIDs record raises its shard's mark to the reservation's limit;
//   - a NextIDsExact record at or above the redo position replaces every mark
//     with the exact marks: it was written at a clean close, after every id the
//     log or the snapshot could name was issued, so a clean restart wastes none
//     (PostgreSQL believes a shutdown checkpoint's nextOid exactly in the same
//     way, xlog.c:9304-9312 at 10cc5aa9). Below the redo position it only
//     raises, because the snapshot is newer.
//
// bound is the largest mark any record or the start names, never lowered; seen
// marks a shard the writer reserved in. An annexed id at or above bound in a seen
// shard is [ErrIDBeyondReservation]. A shard no record names — a log written
// before step 4, or a shard whose ids all predate the store's reserver — is not
// checked.
type idReplay struct {
	cur   [graph.MapperShards]uint64
	bound [graph.MapperShards]uint64
	seen  [graph.MapperShards]bool
}

// newIDReplay starts from m's marks raised to base (nodeids.bin's marks, nil when
// the snapshot has none).
func newIDReplay[N comparable](m *graph.Mapper[N], base *[graph.MapperShards]uint64) *idReplay {
	r := &idReplay{cur: m.Watermark().Next()}
	if base != nil {
		for i := range r.cur {
			r.cur[i] = max(r.cur[i], base[i])
		}
	}
	r.bound = r.cur
	return r
}

func (r *idReplay) reserve(shard int, limit uint64) {
	r.seen[shard] = true
	r.cur[shard] = max(r.cur[shard], limit)
	r.bound[shard] = max(r.bound[shard], limit)
}

func (r *idReplay) exact(next *[graph.MapperShards]uint64, crossed bool) {
	for i := range next {
		r.seen[i] = true
		r.bound[i] = max(r.bound[i], next[i])
		if crossed {
			r.cur[i] = next[i]
		} else {
			r.cur[i] = max(r.cur[i], next[i])
		}
	}
}

// check is the annex id check: see [ErrIDBeyondReservation].
func (r *idReplay) check(id graph.NodeID) error {
	shard := graph.MapperShardOf(id)
	idx := uint64(id) >> 8
	if r.seen[shard] && idx >= r.bound[shard] {
		return fmt.Errorf("%w: id %d is index %d of shard %d, reserved below %d",
			ErrIDBeyondReservation, uint64(id), idx, shard, r.bound[shard])
	}
	return nil
}

// apply sets the mapper's marks; each is floored at the shard's assigned length,
// so an id the replay placed or interned is never reissued.
func (r *idReplay) apply(m interface{ RestoreNext(int, uint64) }) {
	for i, v := range &r.cur {
		m.RestoreNext(i, v)
	}
}

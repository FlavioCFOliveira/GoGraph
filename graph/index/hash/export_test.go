package hash

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// keyPresent reports whether value's shard currently holds a published entry for
// it, regardless of whether that entry's set is empty. A tombstoned slot reads
// as absent.
//
// It exists so a test can assert on the shard TABLE's STRUCTURE — that an emptied
// value's entry is DROPPED, not merely emptied — which no exported operation can
// distinguish: Lookup, Cardinality and Contains all answer the same for an absent
// value and for a present-but-empty one, and [Index.DistinctValues] is defined to
// ignore an empty entry precisely so it cannot see the difference either.
//
// It takes no lock at all — the shard read path has none since rmp #2699 — so
// it is safe to call concurrently.
func (i *Index[V]) keyPresent(value V) bool {
	s, h := i.locate(value)
	return s.find(h, value) != nil
}

// forEachEntry calls fn for every entry published in every shard, in table
// order. It is the export-test replacement for ranging the shard tables, and it
// takes no shard lock because the read path has none.
func (i *Index[V]) forEachEntry(fn func(v V, e *entry)) {
	for k := range i.shards {
		t := i.shards[k].tbl.Load()
		if t == nil {
			continue
		}
		for si := range t.slots {
			sl := &t.slots[si]
			e := sl.e.Load()
			if e == nil || e == tombstone {
				continue
			}
			fn(sl.key, e)
		}
	}
}

// nonEmptySum returns the SIGNED sum of every shard's non-empty counter.
//
// [Index.DistinctValues] clamps a negative sum to zero, because wrapping to a
// huge positive value would re-introduce rmp #1983 and that is the failure
// direction that matters in production. The clamp would also hide an
// over-decrement from a test, so a test reads the raw signed sum through here and
// asserts it is never negative.
func (i *Index[V]) nonEmptySum() int64 {
	var n int64
	for k := range i.shards {
		n += i.shards[k].nonEmpty.Load()
	}
	return n
}

// entryFor returns value's published entry, so a test can hold the pointer
// [hashShard.reap] takes as its identity argument.
func (i *Index[V]) entryFor(value V) (*entry, bool) {
	s, h := i.locate(value)
	e := s.find(h, value)
	return e, e != nil
}

// markEntryDead force-kills value's entry the way [hashShard.reap] does, but
// without requiring the set to be empty, and keeps the shard's non-empty counter
// exact (reap only ever kills an empty entry, which is already uncounted; killing
// a populated one has to decrement). It reports whether there was an entry to
// kill.
//
// It is the seam the dead-entry retry paths in [Index.Insert] and [Index.Delete]
// are exercised through: production only reaches a dead entry through a race a
// test cannot schedule deterministically.
func (i *Index[V]) markEntryDead(value V) bool {
	s, h := i.locate(value)
	s.w.Lock()
	defer s.w.Unlock()
	t := s.tbl.Load()
	if t == nil {
		return false
	}
	pos := probeStart(h) & t.mask
	for {
		sl := &t.slots[pos]
		e := sl.e.Load()
		if e == nil {
			return false
		}
		if sl.key == value {
			if e == tombstone {
				return false
			}
			e.mu.Lock()
			if e.meta.Load()&metaTagMask != metaEmpty {
				s.nonEmpty.Add(-1)
			}
			e.dead = true
			e.mu.Unlock()
			sl.e.Store(tombstone)
			s.tombs.Add(1)
			return true
		}
		pos = (pos + 1) & t.mask
	}
}

// isDead reports e's dead flag under e's own read lock.
//
// The flag is the whole mechanism by which a mutator parked on a detached entry
// learns to retry (see [entry.dead]), and nothing exported can observe it: a
// detached entry answers every read exactly as an absent value does. So a test
// that wants to prove the flag was PUBLISHED — rather than infer it from a race
// that may not have happened — reads it here.
func (e *entry) isDead() bool {
	e.mu.RLock()
	dead := e.dead
	e.mu.RUnlock()
	return dead
}

// image is an entry's published posting list as a white-box test reads it: the
// set itself plus the tier flag the read path branches on. It is the RESOLVED
// form of the meta-word / snapshot protocol, so a test can assert about a
// published image without spelling that protocol out at every site.
type image struct {
	set    index.NodeSet
	shared bool
}

// loadImage resolves the entry's published image the way a read path does, and
// reports whether a snapshot was involved at all — which is the round-three
// property the inline tags exist for.
//
// It takes NO lock, so a caller that goes on to read a bitmap-tier set through
// the returned header must know no writer is running. Every caller is a
// quiescent assertion after a finished workload; a test that needs a tier probe
// while a writer churns uses [entry.isSharedTier] instead.
func (e *entry) loadImage() (img image, viaSnapshot bool) {
	for {
		m := e.meta.Load()
		if m&metaPtrBit == 0 {
			if m&metaTagMask == metaSingleton {
				img.set.Add(m >> metaShift)
			}
			return img, false
		}
		if sn := e.snap.Load(); sn != nil {
			return image{set: sn.set, shared: sn.shared}, true
		}
		// The demotion window; state 5 on [entry].
	}
}

// isSharedTier reports whether the entry's published image is on the BITMAP
// tier — the one tier whose reads take the entry read lock — as [entry.meta]
// records it.
//
// It is a single atomic load, so a test may call it on an entry a writer is
// churning. The tag it reads agrees with the published snapshot's own shared
// flag by construction, and [Index.imageTagMismatches] pins that agreement
// rather than leaving this helper to be trusted.
func (e *entry) isSharedTier() bool {
	return e.meta.Load()&metaTagMask == metaBitmap
}

// forceBitmapCard overwrites the maintained cardinality in metaBitmap's payload
// for value's entry, so a test can DRIVE the drift that
// [entry.deleteLocked] documents as able to cost performance and never
// correctness. Production cannot produce it — the counter is restated by
// [entry.publish] and moved only by CheckedAdd/CheckedRemove deltas — so the
// claim is unfalsifiable without a seam.
//
// It reports false when the value is not on the bitmap tier, so a test cannot
// pass on a no-op injection.
func (i *Index[V]) forceBitmapCard(value V, card uint64) bool {
	s, h := i.locate(value)
	e := s.find(h, value)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.meta.Load()
	if m&metaTagMask != metaBitmap {
		return false
	}
	e.meta.Store(metaBitmap | card<<metaShift)
	return true
}

// imageTagMismatches sweeps every entry reachable from a shard table and counts
// those whose [entry.meta] tag disagrees with what the entry actually
// publishes. The per-class counts come back too, so a test can refuse to pass
// on a sweep that examined nothing.
//
// Three things are checked, one per way the word and the pointer can fall out
// of step, and none of them is observable through any exported operation:
//
//   - an INLINE tag with a snapshot STILL ATTACHED. This is the round-three
//     regression: the entry answers every read correctly out of the word while
//     going on retaining the 24-byte snapshot — plus, on the small tier, its
//     64-byte backing array — that the tier it left had allocated. It gives the
//     entire memory win back, silently.
//   - a POINTER tag with NO snapshot at rest. A reader in that state re-reads
//     meta and, at rest, finds the same pointer tag: it spins for ever. State 5
//     on [entry] is resolvable only because a writer stored the inline tag
//     first.
//   - metaBitmap over a snapshot that is not shared, or metaSmall over one that
//     is. A reader trusts the snapshot's own flag, so this costs no safety, but
//     it makes the demotion trigger read the wrong payload and the tag stops
//     meaning what this package documents it to mean.
//
// An inline tag's PAYLOAD is not cross-checked here, and cannot be: the word IS
// the image, so there is no second record of the contents to compare it with. A
// wrong payload is caught by the functional read parity tests instead, which
// see the wrong ids.
func (i *Index[V]) imageTagMismatches() (mismatches, inlineEntries, snapshotEntries int) {
	i.forEachEntry(func(_ V, e *entry) {
		e.mu.RLock()
		m := e.meta.Load()
		sn := e.snap.Load()
		switch {
		case m&metaPtrBit == 0:
			inlineEntries++
			if sn != nil {
				mismatches++
			}
		case sn == nil:
			mismatches++
		default:
			snapshotEntries++
			if (m&metaTagMask == metaBitmap) != sn.shared {
				mismatches++
			}
		}
		e.mu.RUnlock()
	})
	return mismatches, inlineEntries, snapshotEntries
}

// deadPublishedCount counts the entries that are STILL REACHABLE from a shard
// map while carrying the dead flag.
//
// The retry loops in [Index.Insert] and [Index.Delete] terminate only because
// that count is always zero at rest: a mutator that observes dead drops the
// entry lock and looks the value up again, and the loop makes progress solely
// because a dead entry's slot has already been tombstoned, so the next find
// misses. An
// entry that were dead AND still published would spin those loops for ever —
// a liveness failure the race detector cannot see and a functional test would
// read as a hang. So the precondition is asserted directly.
func (i *Index[V]) deadPublishedCount() int {
	var n int
	i.forEachEntry(func(_ V, e *entry) {
		e.mu.RLock()
		if e.dead {
			n++
		}
		e.mu.RUnlock()
	})
	return n
}

// bitmapCardMismatches counts the published entries whose maintained demotion
// trigger — the cardinality in metaBitmap's payload — disagrees with the true
// cardinality of the image they publish.
//
// The counter is a hint that can only cost performance (see
// [entry.deleteLocked] for why neither drift direction can mislabel a tier or
// move the non-empty accounting), so a mismatch is not a correctness failure —
// which is exactly why it would otherwise go unnoticed until a posting list
// quietly stopped demoting. Reported alongside the number of SHARED entries
// examined, so a test can refuse to pass on an empty sweep.
//
// It also pins the other half of the encoding: OFF the bitmap tier the payload
// must be zero, except on metaSingleton where the payload is the id itself.
func (i *Index[V]) bitmapCardMismatches() (mismatches, sharedEntries int) {
	i.forEachEntry(func(_ V, e *entry) {
		e.mu.RLock()
		m := e.meta.Load()
		sn := e.snap.Load()
		switch {
		case sn != nil && sn.shared:
			sharedEntries++
			if m>>metaShift != sn.set.Cardinality() {
				mismatches++
			}
		case m&metaTagMask != metaSingleton && m>>metaShift != 0:
			mismatches++
		}
		e.mu.RUnlock()
	})
	return mismatches, sharedEntries
}

// tableStats reports the geometry of the shard that owns value: the published
// table's slot count, the number of non-nil slots, and how many of those are
// tombstones. It takes the WRITER lock, because used and tombs are guarded by
// it and are not atomic.
func (i *Index[V]) tableStats(value V) (slots, used, tombs int) {
	s, _ := i.locate(value)
	s.w.Lock()
	defer s.w.Unlock()
	if t := s.tbl.Load(); t != nil {
		slots = len(t.slots)
	}
	return slots, int(s.used.Load()), int(s.tombs.Load())
}

// slotIndexOf returns the index of the slot in value's shard whose key equals
// value, and how many slots in that shard carry that key. A count above one is
// a corrupt table: a key must occupy exactly one slot.
//
// It counts TOMBSTONED slots as well as live ones, and that is deliberate rather
// than sloppy: the invariant under test is that a revive reuses the key's OWN
// slot, so a test must be able to see a key that exists only as a tombstone.
func (i *Index[V]) slotIndexOf(value V) (idx, count int) {
	s, _ := i.locate(value)
	idx = -1
	t := s.tbl.Load()
	if t == nil {
		return idx, 0
	}
	for si := range t.slots {
		sl := &t.slots[si]
		if sl.e.Load() == nil {
			continue
		}
		if sl.key == value {
			count++
			if idx < 0 {
				idx = si
			}
		}
	}
	return idx, count
}

// duplicateKeyCount reports how many keys occupy more than one slot, across
// every shard, counting each surplus slot once. It must always be zero.
//
// Like [Index.slotIndexOf] it counts TOMBSTONED slots, and that is what gives it
// teeth: TestTable_ProbeChainSurvivesReap calls it with 10 000 live tombstones,
// and a [hashShard.slotFor] that probed past a matching tombstone instead of
// reviving it would put the key in two slots — which is exactly the mutant this
// catches.
func (i *Index[V]) duplicateKeyCount() int {
	var dup int
	for k := range i.shards {
		t := i.shards[k].tbl.Load()
		if t == nil {
			continue
		}
		seen := make(map[V]int)
		for si := range t.slots {
			sl := &t.slots[si]
			if sl.e.Load() == nil {
				continue
			}
			seen[sl.key]++
		}
		for _, n := range seen {
			if n > 1 {
				dup += n - 1
			}
		}
	}
	return dup
}

// maxTableSlots returns the largest published table in the index, so a churn
// test can assert the tables do not grow without bound.
func (i *Index[V]) maxTableSlots() int {
	var mx int
	for k := range i.shards {
		if t := i.shards[k].tbl.Load(); t != nil && len(t.slots) > mx {
			mx = len(t.slots)
		}
	}
	return mx
}

// loadFactorViolations counts the shards whose published table holds more
// non-nil slots than the load factor permits. A violation is a liveness bug,
// not merely a performance one: [hashShard.find] probes until it meets a nil,
// so a table with no nil slot left would spin for ever.
func (i *Index[V]) loadFactorViolations() int {
	var bad int
	for k := range i.shards {
		s := &i.shards[k]
		s.w.Lock()
		if t := s.tbl.Load(); t != nil {
			if int(s.used.Load())*loadDen > len(t.slots)*loadNum {
				bad++
			}
			nils := 0
			for si := range t.slots {
				if t.slots[si].e.Load() == nil {
					nils++
				}
			}
			if nils == 0 {
				bad++
			}
		}
		s.w.Unlock()
	}
	return bad
}

// countingMismatches RECOUNTS every shard's table from the structure and reports
// the shards whose maintained used/tombs disagree with it, plus a description of
// the first disagreement.
//
// # Why this exists when loadFactorViolations already looks like a guard
//
// [Index.loadFactorViolations] compares s.used — the COUNTER — against the
// table's length. It therefore cannot falsify s.used: it measures the counter
// against itself. That blindness was demonstrated, not theorised. Mutating
// [Index.Deserialize]'s `s.used = len(fresh[k])` to `s.used = 0` leaves the
// ENTIRE package suite green (`go test ./graph/index/hash/` exits 0), while the
// resulting undercount stops [hashShard.slotFor] rehashing when it must, fills
// the table, and spins [hashShard.find] for ever at 100% CPU while holding the
// shard's writer lock. Neither -race nor goleak nor any functional test sees it,
// because it is a liveness failure and not a wrong answer.
//
// The termination argument on [table] depends on used being exact. This is the
// only assertion in the package that can prove it is.
func (i *Index[V]) countingMismatches() (bad int, detail string) {
	for k := range i.shards {
		s := &i.shards[k]
		s.w.Lock()
		if t := s.tbl.Load(); t != nil {
			nonNil, tombs := 0, 0
			for si := range t.slots {
				e := t.slots[si].e.Load()
				if e == nil {
					continue
				}
				nonNil++
				if e == tombstone {
					tombs++
				}
			}
			if int64(nonNil) != s.used.Load() || int64(tombs) != s.tombs.Load() {
				bad++
				if detail == "" {
					detail = fmt.Sprintf("shard %d: used=%d realNonNil=%d tombs=%d realTombs=%d slots=%d",
						k, s.used.Load(), nonNil, s.tombs.Load(), tombs, len(t.slots))
				}
			}
		}
		s.w.Unlock()
	}
	return bad, detail
}

// tablesAllocated reports how many shards currently hold a table, so a test can
// assert that a narrow index does not pay for shardCount slot arrays.
func (i *Index[V]) tablesAllocated() (n, slots int) {
	for k := range i.shards {
		if t := i.shards[k].tbl.Load(); t != nil {
			n++
			slots += len(t.slots)
		}
	}
	return n, slots
}

// occupancy sums every shard's maintained used and tombs counters, so a test can
// assert that emptying an index actually RELEASES its slots rather than leaving
// them tombstoned.
func (i *Index[V]) occupancy() (used, tombs int) {
	for k := range i.shards {
		s := &i.shards[k]
		s.w.Lock()
		used += int(s.used.Load())
		tombs += int(s.tombs.Load())
		s.w.Unlock()
	}
	return used, tombs
}

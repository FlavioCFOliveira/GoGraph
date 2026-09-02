package hash

import "github.com/FlavioCFOliveira/GoGraph/graph/index"

// keyPresent reports whether value's shard currently holds a map entry for it,
// regardless of whether that entry's set is empty.
//
// It exists so a test can assert on the shard map's STRUCTURE — that an emptied
// value's entry is DROPPED, not merely emptied — which no exported operation can
// distinguish: Lookup, Cardinality and Contains all answer the same for an absent
// value and for a present-but-empty one, and [Index.DistinctValues] is defined to
// ignore an empty entry precisely so it cannot see the difference either.
//
// It takes the shard read lock, so it is safe to call concurrently.
func (i *Index[V]) keyPresent(value V) bool {
	s := i.shard(value)
	s.mu.RLock()
	_, ok := s.entries[value]
	s.mu.RUnlock()
	return ok
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
	return i.shard(value).lookup(value)
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
	s := i.shard(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[value]
	if !ok {
		return false
	}
	e.mu.Lock()
	if e.meta.Load()&metaTagMask != metaEmpty {
		s.nonEmpty.Add(-1)
	}
	e.dead = true
	e.mu.Unlock()
	delete(s.entries, value)
	return true
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
	e, ok := i.shard(value).lookup(value)
	if !ok {
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

// imageTagMismatches sweeps every entry reachable from a shard map and counts
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
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		for _, e := range s.entries {
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
		}
		s.mu.RUnlock()
	}
	return mismatches, inlineEntries, snapshotEntries
}

// deadPublishedCount counts the entries that are STILL REACHABLE from a shard
// map while carrying the dead flag.
//
// The retry loops in [Index.Insert] and [Index.Delete] terminate only because
// that count is always zero at rest: a mutator that observes dead drops the
// entry lock and looks the value up again, and the loop makes progress solely
// because a dead entry has already left the map, so the next lookup misses. An
// entry that were dead AND still published would spin those loops for ever —
// a liveness failure the race detector cannot see and a functional test would
// read as a hang. So the precondition is asserted directly.
func (i *Index[V]) deadPublishedCount() int {
	var n int
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		for _, e := range s.entries {
			e.mu.RLock()
			if e.dead {
				n++
			}
			e.mu.RUnlock()
		}
		s.mu.RUnlock()
	}
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
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		for _, e := range s.entries {
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
		}
		s.mu.RUnlock()
	}
	return mismatches, sharedEntries
}

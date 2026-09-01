package lpg

// mvcc_label_churn.go — the PER-LABEL churn gate (rmp #2686).
//
// # What it is for
//
// [Graph.labelBitmapAsOfFiltered] corrects a label bitmap against the SUSPECT
// set — every node any writer has touched recently enough for a reader to
// disagree about it. That set is global: it is the union of the node-label
// delta keys, the node-life born/died keys and the deferred index-removal keys,
// and it says nothing about WHICH label each suspect concerns. So a reader
// scanning label L walks, and probes under two shard locks, every node a writer
// has touched anywhere in the graph.
//
// Measured on cypher-mixed-rw at 1024 goroutines (rmp #2686): 93 to 1049
// suspects visited per read; when the writers touch a label the readers do not
// scan, 0.0000 of those visits change a single bit, and even when both touch the
// SAME label 98.4% change nothing. [Graph.suspectNodes] alone takes 258 shard
// read locks per call, 28% of block time.
//
// This is the index that makes the question answerable without walking: one
// counter per label, holding the number of live suspects that could possibly
// change that label's bitmap. A reader whose labels all read zero skips the
// gathering AND the correction outright.
//
// # The safety invariant
//
// The gate MUST NEVER UNDER-COUNT. It is raised before the suspect becomes
// observable and lowered only once it has stopped being one. Over-counting costs
// a needless slow path; under-counting is an ACID Consistency break — a reader
// skips a correction it needed and over-reports a node that is dead or has lost
// the label.
//
// Every raise is therefore paired with exactly one lower, owned by the thing
// whose lifetime the raise tracks:
//
//   - a node-label DELTA holds its own lid, from [nodeLabelShard.pushLabelDelta]
//     until the delta is freed by [Graph.reclaimLabelVersions] or
//     [Graph.reclaimAbortedLabelsLocked];
//   - a DEFERRED INDEX REMOVAL holds the lid in its key, from
//     [Graph.deferLabelIndexRemoval] until the entry is cancelled, applied or
//     withdrawn;
//   - a node-LIFE record holds every label the node carried when the record was
//     written (see [lifeStamp.churn]), because a birth or a death moves the node
//     in or out of EVERY bitmap it is a member of. The set is captured on the
//     record so the release names exactly the labels the raise took.
//
// The three overlap deliberately: a node deleted through [Graph.removeNodeInfo]
// is held by its life record AND by one deferred removal per label AND by the
// scoped hold that call takes across the whole retirement. Redundant holds
// over-count, which is the safe direction; a single missing hold is the unsafe
// one.
//
// This is PostgreSQL's visibility map applied to a label index: a conservative
// per-page bit, set eagerly and cleared only once the page is confirmed clean,
// consulted to skip work that would almost always find nothing to do.
//
// # Why the storage is chunked and not one flat array
//
// Labels are dense small integers, but the population can be large — 200 000
// distinct labels was measured on a probe for rmp #2686 — so the table has to
// grow. Two shapes were rejected before this one:
//
//   - REBUILDING the table per label creation. rmp #2685 measured exactly that
//     shape on the btree spine: a copy-on-write-per-creation went from 13 µs to
//     790 µs per creation between N=1k and N=64k, which is O(N²) over a load.
//   - DOUBLING a flat []atomic.Int64 and copying the counter VALUES across. The
//     copy races every concurrent Add on the old array, and a lost Add is a lost
//     hold, which is the under-count this file exists to prevent.
//
// So counters never move. The spine is a growable array of chunk POINTERS, grown
// by doubling and published through an [atomic.Pointer]; a chunk holds
// churnChunkSize counters and is allocated on first use. Growth copies pointers,
// not counters, so an Add in flight on some other label is untouched. Reads are
// lock-free: one atomic load of the spine, one of the slot, one of the counter.
// A label with no churn ever recorded allocates nothing at all.

import (
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

const (
	// churnChunkBits sizes one chunk at 256 counters — 2 KiB. Small enough that
	// a graph churning three labels out of 200 000 allocates three chunks, large
	// enough that the spine stays short.
	churnChunkBits = 8
	churnChunkSize = 1 << churnChunkBits
	churnChunkMask = churnChunkSize - 1
	// churnSpineMin is the spine's first size, in chunks: 8 chunks covers the
	// first 2048 labels, which is every graph that is not a schema generator.
	churnSpineMin = 8
)

// churnChunk is a fixed block of per-label counters. It is allocated once and
// never moved, which is what lets the spine grow without disturbing a
// concurrent Add.
type churnChunk struct {
	c [churnChunkSize]atomic.Int64
}

// churnSpine indexes the chunks. Each slot is itself atomic so a chunk can be
// installed into a published spine without racing the lock-free readers.
type churnSpine []atomic.Pointer[churnChunk]

// labelChurn is the per-label suspect counter table.
//
// Safe for concurrent use. Reads take no lock; the mutex serialises spine growth
// and chunk installation only, and is never held across anything else.
type labelChurn struct {
	spine atomic.Pointer[churnSpine]
	mu    sync.Mutex
}

// load returns the number of live holds on lid.
func (lc *labelChurn) load(lid LabelID) int64 {
	sp := lc.spine.Load()
	if sp == nil {
		return 0
	}
	s := *sp
	i := int(lid >> churnChunkBits)
	if i >= len(s) {
		return 0
	}
	ch := s[i].Load()
	if ch == nil {
		return 0
	}
	return ch.c[lid&churnChunkMask].Load()
}

// live reports whether lid has any live hold.
func (lc *labelChurn) live(lid LabelID) bool { return lc.load(lid) != 0 }

// add moves lid's counter by d. d is +1 for a raise and -1 for a release; a
// release must name a label a matching raise took, or the gate under-counts.
func (lc *labelChurn) add(lid LabelID, d int64) {
	lc.chunkFor(lid).c[lid&churnChunkMask].Add(d)
}

// raise takes one hold on lid.
func (lc *labelChurn) raise(lid LabelID) { lc.add(lid, 1) }

// release drops one hold on lid.
func (lc *labelChurn) release(lid LabelID) { lc.add(lid, -1) }

// releaseAll drops one hold on each label in lids. It is the paired inverse of
// the raise loops, and tolerates a nil slice so the common no-labels case costs
// one branch.
func (lc *labelChurn) releaseAll(lids []LabelID) {
	for _, lid := range lids {
		lc.add(lid, -1)
	}
}

// chunkFor returns lid's chunk, allocating it — and growing the spine — when it
// does not exist yet.
//
// The fast path is two atomic loads and no lock. Growth is amortised O(1) per
// label because the spine DOUBLES; it copies chunk pointers, never counters, so
// an Add in flight on another label cannot be lost.
func (lc *labelChurn) chunkFor(lid LabelID) *churnChunk {
	i := int(lid >> churnChunkBits)
	if sp := lc.spine.Load(); sp != nil {
		s := *sp
		if i < len(s) {
			if ch := s[i].Load(); ch != nil {
				return ch
			}
		}
	}
	return lc.installChunk(i)
}

// installChunk allocates chunk i, growing the spine if it does not reach that
// far, and returns it. Serialised by lc.mu, so no two installs race and no
// install races a growth.
func (lc *labelChurn) installChunk(i int) *churnChunk {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	var cur churnSpine
	if sp := lc.spine.Load(); sp != nil {
		cur = *sp
	}
	if i < len(cur) {
		// The spine already reaches: install into the published backing array.
		// The slot is atomic, so a reader loading it concurrently sees either
		// nil (and reads zero, which is what an unallocated label holds) or the
		// finished chunk.
		if ch := cur[i].Load(); ch != nil {
			return ch
		}
		ch := &churnChunk{}
		cur[i].Store(ch)
		return ch
	}

	// GROW BY DOUBLING. Anything else makes creation O(N) and a load O(N²); see
	// the file comment for the measurement that rules that out.
	n := len(cur)
	if n < churnSpineMin {
		n = churnSpineMin
	}
	for n <= i {
		n *= 2
	}
	next := make(churnSpine, n)
	for k := range cur {
		// Copied slot by slot through Load/Store rather than by assignment: an
		// atomic.Pointer may not be copied as a value, and the chunk it names is
		// shared with the spine still published, which is exactly what makes
		// growth safe against a concurrent Add.
		if ch := cur[k].Load(); ch != nil {
			next[k].Store(ch)
		}
	}
	ch := &churnChunk{}
	next[i].Store(ch)
	lc.spine.Store(&next)
	return ch
}

// labelSet names the labels one bitmap read concerns.
//
// Exactly one of the two forms is populated, so neither [Graph.LabelBitmapAsOf]
// nor [Graph.LabelsBitmapAsOf] allocates to describe its own labels: the single
// form carries the id by value and the conjunction form borrows the caller's
// slice, which it already holds.
type labelSet struct {
	one  LabelID
	many []LabelID
	// conj distinguishes the two forms EXPLICITLY rather than by whether many is
	// nil. A conjunction over a nil slice names no label at all, and reading that
	// as the single form would consult label 0 — a real label, and the wrong one.
	conj bool
}

// oneLabel names a single-label read.
func oneLabel(lid LabelID) labelSet { return labelSet{one: lid} }

// manyLabels names a conjunction read. An empty conjunction names nothing, and
// its bitmap is empty, so the gate correctly reports it quiet.
func manyLabels(lids []LabelID) labelSet { return labelSet{many: lids, conj: true} }

// churnLive reports whether ANY label this read concerns has a live hold.
//
// The disjunction is the sound direction for a conjunction read: a node's
// membership of the intersection can change when ANY one of its labels changes,
// so the read may only skip when every one of them is quiet.
func (g *Graph[N, W]) churnLive(ls labelSet) bool {
	if !ls.conj {
		return g.labelChurn.live(ls.one)
	}
	for _, lid := range ls.many {
		if g.labelChurn.live(lid) {
			return true
		}
	}
	return false
}

// nodeLabelBagLids returns the labels id currently carries.
//
// It takes the LABEL shard's read lock, so it must never be called while holding
// the life shard lock or the tombstone lock — the two orders the reclaimer and
// [Graph.revive] fix. A node with no labels allocates nothing and returns nil.
//
// It is separate from [Graph.raiseChurnFor] so that a caller which needs the bag
// for something else as well — [Graph.removeNodeInfo] needs it for the churn
// hold, for the bitmap strip and for the death record — reads it ONCE instead of
// three times.
func (g *Graph[N, W]) nodeLabelBagLids(id graph.NodeID) []LabelID {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	n := bag.len()
	if n == 0 {
		sh.mu.RUnlock()
		return nil
	}
	lids := make([]LabelID, 0, n)
	bag.forEach(func(lid LabelID) { lids = append(lids, lid) })
	sh.mu.RUnlock()
	return lids
}

// raiseChurnFor takes one hold on each of lids and returns lids, so the caller
// can release exactly the holds it took.
//
// The hold must be in place BEFORE the change it covers becomes observable; every
// caller here takes it before the lock that publishes that change.
func (g *Graph[N, W]) raiseChurnFor(lids []LabelID) []LabelID {
	for _, lid := range lids {
		g.labelChurn.raise(lid)
	}
	return lids
}

// pinChurnForDivergentBag takes a hold that is NEVER released, for every label
// where the node's bag and the label index are about to stop agreeing and
// nothing will ever reconcile them.
//
// # Why a permanent hold, and why only on a measured divergence
//
// [Graph.tombstoneAborted] and [Graph.reviveAborted] flip the tombstone bitmap
// WITHOUT recording a life instant — that is what withdrawing an aborted create
// or delete means — and they run after [Graph.reclaimAbortedLife] has already
// dropped the node's life records. So the node ends with no record of any kind
// while the label index may still describe the state the aborted transaction
// left. Nothing else will ever visit that entry: there is no delta to reclaim,
// no deferred removal to apply and no life record to sweep, so the divergence is
// permanent and so must the hold be.
//
// It is taken only where the divergence is REAL, which is why the index is
// probed rather than the gate raised blind. In the ordinary flows there is none:
// an aborted CREATE has had its labels withdrawn from both the bag and the index
// by [Graph.reclaimAbortedLabelsLocked] before this runs, so the bag is empty and
// nothing is pinned; an aborted DELETE had its deferred index removals cancelled
// rather than applied, so the entry is still there and matches the revived node.
// The pin fires on the leftover shapes — a revival the abort machinery then
// tombstones, say — where HEAD's answer depended on some unrelated suspect
// keeping the correction alive.
//
// want is what the index SHOULD say about the node once the flip has happened:
// true for a revival (the node exists, so its labels must be indexed) and false
// for a tombstone (the node does not exist, so none of them may be).
func (g *Graph[N, W]) pinChurnForDivergentBag(id graph.NodeID, want bool) {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	n := bag.len()
	if n == 0 {
		sh.mu.RUnlock()
		return
	}
	lids := make([]LabelID, 0, n)
	bag.forEach(func(lid LabelID) { lids = append(lids, lid) })
	sh.mu.RUnlock()
	for _, lid := range lids {
		if g.nodeIdx.Has(uint32(lid), id) != want {
			g.labelChurn.raise(lid)
		}
	}
}

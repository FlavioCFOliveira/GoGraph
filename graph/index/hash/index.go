// Package hash provides a sharded hash index from arbitrary
// comparable property values to the set of NodeIDs that carry them,
// represented as a 64-bit Roaring bitmap.
//
// The structure answers exact-match property predicates (for example
// "every node where email == 'x@y.com'") in O(1) average time. For
// range predicates use the B+ tree index in package
// github.com/FlavioCFOliveira/GoGraph/graph/index/btree (Sprint 2, T19).
//
// Index is safe for concurrent use by any number of goroutines with no
// external synchronisation; [Index] documents the full contract, including
// the two-level lock geometry and the lock order every code path in this
// package obeys. Keys are distributed over the shards by
// [maphash.Comparable] of the key itself, so a shard holds an arbitrary
// subset of the key space rather than a [graph.NodeID] band.
package hash

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"hash/maphash"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// shardCount is the number of shards the key space is spread over, and
// shardMask selects one from a key's hash.
//
// # Both figures are measured, not chosen by feel (rmp #2692)
//
// 256 shards is 8 KB of shard array, which stays resident in L1 alongside the
// working set. Raising it was tried: 1024 and 4096 shards buy 1.6-1.8x at 1024
// concurrent goroutines but REGRESS the single-goroutine path by 17-23%,
// because the array grows to 32 KB and 128 KB respectively and leaves L1. The
// module publishes latency at concurrency 1 as well as at 1024, so a change
// that trades the low end for the high end is not an improvement; 256 is kept.
//
// Padding each shard to a cache line was tried too, and DECLINED: every
// measured ratio landed between 0.98 and 1.10 against an ~11% noise floor, so
// the effect is not distinguishable from noise, and it would cost 24 KB per
// index for no demonstrated gain. The contention this package actually had was
// not false sharing between shard locks — it was readers parking on a shard
// RWMutex, which the per-entry geometry described on [Index] removes.
const (
	shardCount = 256
	shardMask  = shardCount - 1
)

var seed = maphash.MakeSeed()

// Index maps property values of type V to the NodeIDs that carry them.
//
// # Concurrency
//
// Index is safe for concurrent use by any number of goroutines, for every
// exported operation, with no external synchronisation.
//
// There are two lock levels and one lock-free level.
//
// The SPINE lock (hashShard.mu, one per shard) guards its shard's value→entry
// map STRUCTURE: it is taken shared to look a value up and exclusively to
// create or drop one. Each value's [entry] then carries its OWN lock, which
// serialises WRITERS of that value against each other. So two writers touching
// different values never contend, and a reader never takes a lock any writer of
// another value holds.
//
// Above that, each entry publishes its posting list WITHOUT a lock. A reader
// starts from one atomic word, [entry.meta], which carries the tier tag and —
// on the two tiers a wide equality index is mostly made of, an EMPTY posting
// list and a SINGLETON one — the entire image. Those reads finish there: no
// pointer to chase, no heap object to have been allocated, no lock. A wider
// list is an immutable [snapshot] behind one atomic pointer, read with NO LOCK
// AT ALL while it is immutable; only a bitmap-tier snapshot, whose bitmap a
// writer mutates in place, sends the reader through the entry read lock. The
// meta constants define the encoding, [snapshot] states the invariant that
// makes the lock-free read safe, and the five states a lock-free reader can
// observe — including the demotion window — are enumerated on [entry].
//
// # What the geometry costs in MEMORY, and what round three gave back
//
// Retained heap per distinct indexed value, measured as HeapAlloc after two
// forced collections with the index live, against the pre-#2692 baseline:
//
//	tier                  BASE     round 2   round 3   round 3 vs BASE
//	singleton, 200k keys  35.06 B   95.76 B   71.76 B   +104.7%
//	small, 20k x 4 ids   109.80 B  167.20 B  167.20 B   + 52.3%
//	bitmap, 2k x 40 ids  413.50 B  478.00 B  478.00 B   + 15.6%
//
// Medians of 5 interleaved rounds per arm, the three arms running byte-
// identical benchmark code; the spread within an arm was at most 0.01 B/key, so
// these figures are not close calls.
//
// Round two's bill was a near-constant +58 to +65 B per key — an *entry (48 B
// in its size class) plus a *snapshot (24 B) where the baseline held an
// [index.NodeSet] by value inside the shard map. It looks catastrophic only on
// singletons, because their payload is the smallest, and singletons are what a
// wide index is made of: at 10 000 000 distinct values the three arms are
// 351 MB, 958 MB and 718 MB. Round three takes the snapshot off the empty and
// singleton tiers, which is where the bill actually lands, and gives back
// exactly its 24 bytes; it leaves the wider tiers byte for byte as round two
// left them, because their images still need a heap object and a lock they
// still take.
//
// BenchmarkIndex_DistinctKeyFootprint is the measurement, per tier. It is per
// TIER because an aggregate hid this: a constant per-key overhead is a small
// fraction of a wide posting list and a catastrophe for a singleton, so an
// average belongs to no tier and buries the only one that matters.
//
// # And what it cost in THROUGHPUT: nothing, measured twice
//
// Round three was expected to be throughput-neutral, because the tier each key
// reaches decides which path it takes (see [entry.mu]) and no key changed tier.
// Measured against round two, interleaved, on a host at loadavg ~2 of 10 cores:
//
//	                                 sweep 1 (n=8)      sweep 2 (n=6)
//	SeekSingleton/Append              -3.71% p=0.000     -4.32% p=0.002
//	SeekSingleton/Bitmap              -1.61% p=0.009     -1.78% p=0.009
//	CardinalityInlineTier/Spread      -5.19% p=0.000     -5.38% p=0.002
//	CardinalityInlineTier/Hot          ~     p=0.555      ~     p=0.240
//	LookupHot                         +1.26% p=0.000      ~     p=0.370
//
// The three improvements are the inline tags earning their keep: a singleton
// read now resolves out of one word in one cache line instead of chasing a
// pointer into a second, which shows up most on the cold-line Spread shape.
//
// LookupHot is reported as NO REPRODUCIBLE CHANGE, not as +1.26%. It reads a
// ~488-id bitmap-tier key, so it pays one extra atomic load — of a word in the
// same cache line as the pointer it then loads — and the first sweep called
// that 0.33 ns significant while the second, on code identical in codegen,
// could not distinguish it at all. Two sweeps that disagree at p=0.000 and
// p=0.370 mean the effect is inside the run-to-run variation, and a single
// sweep's p-value did not capture it. Allocation counts are unchanged on every
// read path.
//
// Before rmp #2692 a single RWMutex per shard guarded both the map and every
// set inside it. Measured on the `index-hash-rw` contention workload (90%
// [Index.Cardinality] / 10% [Index.Insert], 100 000 int64 keys) at 1024
// goroutines: [Index.Insert] held 98.17% of all mutex delay, readers PARKED on
// the shard RWMutex for 40.7% of total block delay, and the read lock cost
// ~6.5x more CPU than the read it protected (RLock 4.52 s + RUnlock 0.97 s
// against 0.85 s of actual work). Roughly 75% of all CPU in the run was futex
// park/unpark. A ceiling probe put the available win at 1.26x at concurrency 1
// rising to 7.90x at 1024.
//
// # Lock order — SPINE before ENTRY, never the reverse
//
// A goroutine may acquire a shard's mu and then an entry.mu. It must NEVER
// acquire a shard's mu while holding any entry.mu. Every path in this file
// obeys it:
//
//   - [Index.Lookup], [Index.LookupAppend], [Index.Cardinality] and
//     [Index.Contains] release the shard lock before touching entry.mu at all —
//     and touch it only when the value's snapshot is a shared bitmap-tier one.
//   - [Index.Insert]'s creation path, [hashShard.reap] and [Index.Deserialize]
//     take the shard lock first and entry.mu second.
//   - [Index.Insert] and [Index.Delete] detect a stale entry through the
//     entry's own dead flag rather than by re-reading the shard map, so they
//     never need the shard lock while holding an entry lock. See [entry.dead]
//     for the deadlock that avoids.
//
// No operation in this package holds two entry locks at once, and no operation
// holds two shard locks at once.
//
// # What the per-entry locks cost: a multi-value read is no longer one image
//
// [Index.DistinctValues] and [Index.Serialize] sample each shard, and Serialize
// each value within it, under that shard's lock and — for a bitmap-tier value —
// that entry's own lock. Their
// answer is therefore assembled from per-shard (and per-entry) images taken at
// slightly different instants rather than from one image of the whole index.
// That was already true across shards before rmp #2692 — writers have never
// taken an index-wide lock — and it is why [Index.Deserialize] is confined to
// engine construction by its caller; see cypher/index_hydration.go.
//
// A [Index.Delete] that empties a value's set is now TWO critical sections —
// the removal under the entry lock, then the reap under the shard write lock —
// where it used to be one. A concurrent reader can therefore observe the value
// present in the map with an empty set. [Index.DistinctValues] is defined not
// to count such an entry, because a caller depends on its zero
// (cypher.hashIndexKind, rmp #1983); every other read path answers the same for
// an empty entry as for an absent one.
type Index[V comparable] struct {
	// binding, when non-nil, ties the index to one (label, property)
	// pair of a live node graph so [Index.Apply] can translate
	// [index.Change] events into typed Insert / Delete calls. It is set
	// once by [NewBound] before the index is shared and never mutated
	// afterwards, so Apply reads it without synchronisation.
	binding *Binding[V]

	shards [shardCount]hashShard[V]
}

// Binding ties an [Index] to a single (label, property) pair of a live
// node graph. A bound index (see [NewBound]) maintains itself from the
// [index.Manager] change fan-out: property writes insert/delete typed
// keys, and label add/remove events attach/detach a node's current
// value. An unbound index (see [New]) ignores the fan-out entirely and
// is maintained by explicit [Index.Insert] / [Index.Delete] calls.
//
// The identifier fields carry interned IDs from the owning graph's
// registries; the callbacks close over the graph so this package stays
// free of a dependency on any concrete graph implementation. Because
// changes are fanned out at commit time — after the transaction's
// mutations were applied eagerly to the graph — the callbacks observe
// the transaction's FINAL state, which is exactly the state the index
// must converge to.
type Binding[V comparable] struct {
	// Project converts a Change.OldValue / Change.NewValue payload to
	// the index key type. ok is false when the payload is absent or
	// not indexable (wrong kind), in which case the event is skipped
	// for that direction.
	Project func(v any) (V, bool)

	// Eligible reports whether the node should currently be present in
	// the index: it must be live (not deleted) and carry the bound
	// label, evaluated against the graph's final state.
	Eligible func(node graph.NodeID) bool

	// CurrentValue returns the node's current value for the bound
	// property, projected to the key type. ok is false when the node
	// is not live, lacks the property, or the value is not indexable.
	// It is consulted on label add/remove events, which carry no
	// property payload.
	CurrentValue func(node graph.NodeID) (V, bool)

	// Label and Property are the source names behind PropertyID and
	// LabelID. They let a query planner match the index against a
	// (label, property) predicate without access to the registries.
	Label, Property string

	// PropertyID is the interned property-key identifier this index
	// covers. Property changes whose Change.Property differs are
	// ignored.
	PropertyID uint32

	// LabelID is the interned label identifier this index is scoped
	// to. Label changes whose Change.Label differs are ignored. Note
	// that interned IDs start at zero, so this field alone cannot mark
	// an unscoped binding; bindings are always label-scoped.
	LabelID uint32
}

// errBindingIncomplete is returned by [NewBound] when a required
// Binding field is missing.
var errBindingIncomplete = fmt.Errorf("%w: incomplete hash index binding", index.ErrIndexValueTypeUnsupported)

// NewBound returns an empty hash index bound to b. Unlike [New], the
// returned index has a functional [Index.Apply]: it subscribes to the
// node property and label changes selected by b and keeps itself
// consistent with the graph. Returns an error when b is missing its
// Label, Property, or any of the three callbacks.
func NewBound[V comparable](b Binding[V]) (*Index[V], error) {
	if b.Label == "" || b.Property == "" ||
		b.Project == nil || b.Eligible == nil || b.CurrentValue == nil {
		return nil, errBindingIncomplete
	}
	idx := New[V]()
	idx.binding = &b
	return idx, nil
}

// BoundNode returns the (label, property) pair this index is bound to,
// with ok reporting whether the index is bound at all. Query planners
// use it to decide whether the index may serve a predicate: a bound
// index covers exactly its (label, property) pair, while an unbound
// index carries no coverage metadata.
func (i *Index[V]) BoundNode() (label, property string, ok bool) {
	if i.binding == nil {
		return "", "", false
	}
	return i.binding.Label, i.binding.Property, true
}

// inlineTierMax is the largest posting-list cardinality this package treats as
// living on an INLINE [index.NodeSet] tier: one whose whole content sits either
// in the NodeSet's own two words or in an immutable copy-on-write backing
// array, and which therefore aliases nothing any writer mutates in place.
//
// # This package owns the tier decision, deliberately
//
// index.NodeSet does not export its state tag, and the one exported way to ask
// — [index.NodeSet.Bitmap]'s shared return — MATERIALISES A FRESH BITMAP for
// every inline tier. Asking it on the read path would cost an allocation and an
// O(n) copy to learn a single bit, which is more than the read it guards. So
// this package derives the tier from the CARDINALITY instead, against this
// constant, at the one moment an image is published, and records the answer in
// [snapshot.shared] so no reader ever derives it again.
//
// The derivation is exact because this package builds a NodeSet in only three
// ways, each monotone in cardinality with respect to this bound:
//
//   - [index.NodeSetFromSorted] — inline at or below the bound, bitmap above it
//     ([Index.Deserialize]).
//   - [index.NodeSet.Add] applied to a COPY — promotes to a bitmap only when the
//     small tier would overflow the bound ([entry.insertLocked]).
//   - this package's own demotion in [entry.deleteLocked] — republishes an
//     inline image the moment a bitmap-tier list falls back to the bound.
//
// That third one is load-bearing, not an optimisation. index.NodeSet never
// demotes on its own (promote-and-never-demote, #1584), so without it a churned
// key would hold a bitmap-tier set at cardinality 1 and the cardinality test
// would report the wrong tier, handing a mutable bitmap to a lock-free reader.
//
// [index.NodeSet.AddRange] is the one constructor that can build a bitmap-tier
// set BELOW the bound. This package never calls it — only graph/index/label
// does — and must not start to without revisiting this contract.
//
// The value must equal index's own inline/bitmap threshold, and the
// correspondence is not assumed: TestInlineTierMaxMatchesNodeSet pins it
// through the exported constructors, so if nodeset.go's smallSetMax moves, that
// test fails instead of this package silently mis-tiering every key.
const inlineTierMax = 8

// The entry META WORD: the tier tag and, on the inline tags, the whole image.
//
// [entry.meta] is one atomic word carrying a two-bit state tag plus a payload
// whose meaning that tag fixes. It exists so that the two tiers a wide equality
// index is almost entirely made of — an EMPTY posting list and a SINGLETON one
// — are described COMPLETELY by that one word: they need no heap object, no
// pointer to chase on a read, and no copy-on-write publication at all. That is
// what round three of rmp #2692 bought, and [Index] records what it cost.
//
// Because the tag travels in the SAME WORD as the payload it describes, and the
// word is written with one atomic store, no reader can ever observe a tag that
// disagrees with the payload beside it. That is strictly stronger than round
// two, which held the tier flag in a different object from the entry.
//
//   - metaEmpty and metaSingleton are the INLINE tags: [entry.snap] is nil and
//     the published image is this word alone. They are numerically their own
//     cardinality, which is what lets [Index.Cardinality] answer an inline key
//     from the tag with no arithmetic.
//   - metaSmall and metaBitmap are the POINTER tags: the image is the immutable
//     [snapshot] in [entry.snap], and a reader takes its TIER decision from
//     that snapshot rather than from this word (see [snapshot] for why it must).
//     Both carry metaPtrBit, so "is there a snapshot to load at all" is one bit
//     test on the word already in hand.
//   - On metaBitmap the payload is the posting list's CARDINALITY: the O(1)
//     demotion trigger, which is why [entry.deleteLocked] never asks roaring.
//     On metaSmall the payload is zero.
const (
	metaEmpty     uint64 = 0b00 // snap == nil; the list is empty
	metaSingleton uint64 = 0b01 // snap == nil; the one id is meta>>metaShift
	metaSmall     uint64 = 0b10 // snap holds an inline-tier set; payload 0
	metaBitmap    uint64 = 0b11 // snap holds a bitmap-tier set; payload = cardinality

	metaTagMask uint64 = 0b11
	// metaPtrBit is set on exactly the two POINTER tags, so one test separates
	// the tiers that carry a snapshot from the tiers that are the word itself.
	metaPtrBit uint64 = 0b10
	metaShift  uint64 = 2

	// maxInlineID is the largest NodeID the singleton tag can carry: the id
	// occupies meta's remaining 62 bits. NodeIDs come from a monotonic counter
	// that cannot approach 2^62 ~= 4.6e18 in any realistic workload, so a
	// singleton id never overflows this cap; one that somehow did is published
	// through a snapshot instead (see [entry.publish]), never truncated. The
	// bound is deliberately the same as index.NodeSet's own singleton cap.
	maxInlineID uint64 = (1 << 62) - 1
)

// snapshot is an immutable published image of one value's posting list, for the
// tiers that cannot fit in [entry.meta].
//
// # THE INVARIANT that makes the read path safe
//
// A published *snapshot is immutable, with exactly one exception: when shared
// is true its set aliases a *roaring64.Bitmap that a writer may mutate in
// place, and only while holding the owning [entry]'s mu for writing. A
// snapshot with shared == false is immutable FOR EVER, so a reader may read it
// with no synchronisation at all after loading the pointer.
//
// shared is true if and only if set is on index.NodeSet's bitmap tier. This
// package knows that by construction from the cardinality (see
// [inlineTierMax]) and never re-derives it on a read.
//
// # The tier decision a reader trusts is THIS FLAG, never the meta tag
//
// A reader resolves [entry.meta] first and loads snap only when the tag says
// there is one — but it must then take the lock-or-not decision from the
// snapshot it actually loaded, not from the tag that sent it there. The two
// loads are separate, so a promotion can land between them: a reader that read
// metaSmall and then loaded the snapshot a concurrent promotion had just
// published would, if it trusted the tag, read a LIVE roaring bitmap with no
// lock at all. Trusting the snapshot's own flag makes that state harmless — the
// reader simply takes the locked branch for an image it did not expect — and it
// is why this flag was not folded into the meta word when the word was
// introduced. TestReaderTrustsSnapshotFlagNotMetaTag constructs exactly that
// interleaving.
//
// # Why an immutable image and not just a finer lock (rmp #2692, round two)
//
// Round one of #2692 replaced the per-shard RWMutex with a per-ENTRY RWMutex,
// which fixed the scaling decay and regressed the low end badly: measured n=9
// interleaved against the pre-#2692 baseline, BenchmarkIndex_SeekSingleton
// /Append went from 16.53 ns to 30.52 ns (+84.65%) and /Bitmap from 106.7 ns to
// 122.6 ns (+14.90%). An RWMutex read lock WRITES two atomics to a line the
// read then has to own, so on a singleton posting list the lock cost several
// times the read it protected.
//
// A ceiling probe that DELETED the entry read lock from the four read paths —
// unsound, but it bounds what any correct fix can buy — recovered all of it:
// /Append 16.35 ns (-46.44% against round one) and /Bitmap 105.5 ns (-13.95%).
// So the win was in the LOCK, not in the entry indirection, and an immutable
// published image is the way to take a read off the lock without giving up the
// per-value write geometry.
//
// # What the publication costs the WRITER, measured
//
// Every write that CHANGES an inline-tier list now copies index.NodeSet's
// two-word header, applies the change to the copy, and publishes a new
// snapshot, where the pre-#2692 code mutated the set in place under the shard
// lock. Measured with BenchmarkIndex_InsertByTier and _DeleteDemote, n=6
// interleaved against the pre-#2692 baseline on an otherwise idle host:
//
//   - Create 213.3n -> 246.4n (+15.55%), 4 allocs -> 5.
//   - Singleton 45.71n -> 62.16n (+35.98%), 1 alloc -> 2.
//   - Small 41.75n -> 52.31n (+25.30%), 1 alloc -> 2.
//   - Promote 255.2n (baseline cannot be compared; see the benchmark).
//   - Bitmap 36.63n -> 36.62n (~, p=0.937), 0 allocs: an in-place bitmap insert
//     publishes nothing, so it pays nothing.
//   - Duplicate 22.11n -> 22.24n (~, p=0.394), 0 allocs.
//
// The demoting delete is the worst single case: 37.52n -> 95.02n (+153.20%)
// with 4 allocs/op and 280 B/op where the baseline had none. It fires only on
// the exact crossing back to [inlineTierMax] — once per crossing, not once per
// delete — and it is still cheaper than the promotion it mirrors (255.2n). Two
// reductions were tried and BOTH REJECTED on measurement: draining through
// [index.NodeSet.AppendTo] into a stack buffer instead of the bitmap's ToArray
// saves one allocation and 32 bytes but costs +36.17% in time (150.4n against
// 110.4n on the build that predated the maintained cardinality), because
// roaring's iterator is dearer than its ToArray; and hysteresis (demoting well
// below the bound instead of at it) would leave every shrunken key on the
// LOCKED read path, trading the thing this design exists to buy for a rare
// write.
//
// Round three then took the snapshot OFF the two cheapest tiers altogether, and
// that is a write-path saving as well as a memory one. Measured against round
// two, n=8 interleaved, BenchmarkIndex_InsertByTier and _DeleteDemote:
//
//   - Create 37.74n -> 31.88n (-15.53%), 72 B -> 48 B, 2 allocs -> 1. A
//     brand-new singleton key allocates its entry AND NOTHING ELSE.
//   - Promote 260.6n -> 215.4n (-17.34%), same 13 allocs: the timed insert is
//     unchanged, but the untimed rebuild ahead of it now leaves far less
//     garbage for the timed region's collector to deal with.
//   - Delete/Steady 50.84n -> 47.29n (-6.97%), 0 allocs both.
//   - Singleton, Small, Promote (bytes), Bitmap, Duplicate (bytes) and
//     Delete/Demote are unchanged in allocations: those publications still need
//     a snapshot, and still allocate exactly one.
//   - Duplicate 15.07n -> 15.94n (+5.77%): one un-inlined call, explained and
//     accepted on [entry.currentLocked].
//
// A singleton publication now allocates NOTHING:
// TestPublicationAllocationsArePinned holds the whole insert-plus-delete churn
// at 2 allocations where round two needed 3, and [Index] carries the per-tier
// memory measurement.
type snapshot struct {
	set    index.NodeSet
	shared bool
}

// entry is one value's posting list: the inline tiers in one atomic word, the
// wider tiers as an immutable [snapshot] behind one atomic pointer, alongside
// the lock its WRITERS serialise on.
//
// Splitting the lock per value is one half of the geometry: a writer touching
// value A does not serialise against a writer of value B in the same shard,
// where the per-shard RWMutex this replaced serialised every caller whose key
// hashed to that shard. The other half is that a reader of an immutable image
// takes NO LOCK AT ALL, which is what round two of rmp #2692 bought (see
// [snapshot] for the measurements it was bought with), and round three took the
// heap object off the two cheapest tiers on top of that.
//
// # entry is deliberately NOT padded to a cache line
//
// This diverges from the sibling geometry in graph/index/label, whose entry IS
// padded to 128 bytes. The divergence is a consequence of cardinality, not of
// taste. A label index holds one entry per LABEL — a handful, dozens at most —
// so 128 bytes apiece is free and buys separation between two hot locks. This
// index holds one entry per DISTINCT PROPERTY VALUE. On a 10 000 000-distinct-
// value index (an email or external-id column, the shape this index exists to
// serve) padding every entry to 128 bytes would cost ~1.28 GB of resident heap
// for the padding alone. That is a direct violation of the ULTRA EFFICIENT
// mandate, and it would be paid to reduce false sharing between locks that, at
// that cardinality, are almost never contended by two goroutines at once.
//
// # The FIELDS are laid out to a size class, and the layout is asserted
//
// Go's allocator has size classes at 8, 16, 24, 32, 48 and 64 bytes, with
// nothing between 32 and 48 or between 48 and 64. meta (8) + snap (8) + mu (24)
// + dead (1) is 41 bytes, which lands in the 48-byte class with 7 bytes of
// padding to spare. That headroom is the whole reason the demotion trigger
// lives in meta's payload rather than in a uint64 field of its own: a ninth
// word would take the entry to 49 bytes and into the 64-byte class, costing
// +16 B on EVERY key to save the 24 B the singleton tier just stopped paying —
// a net loss on the shape this index exists to serve. TestEntrySizeClass pins
// the arithmetic, because nothing in the compiler does.
//
// # Concurrency contract
//
// meta and snap are loaded by readers with NO lock and written by writers under
// mu held for writing, in the order [entry.publish] fixes. dead is written only
// by [hashShard.reap] and [Index.Deserialize], both under mu for writing, and
// read by [Index.Insert] and [Index.Delete] under mu for writing.
//
// # The five states a lock-free reader can observe, and why each is safe
//
//  1. INLINE -> BITMAP (promotion) while a reader holds the old pointer. The
//     promoting writer builds the new bitmap from a COPY of the old ids and
//     never writes into the old inline backing — [index.NodeSet.Add] applied to
//     a copy of the header is already copy-on-write on every inline tier
//     (nodeset.go invariant 4). The reader's pointer keeps that old image alive
//     through Go's GC, so it keeps reading an internally consistent OLD image.
//     Its answer is therefore slightly stale. That is not new: the per-entry
//     RWMutex of round one already permitted exactly this staleness for a read
//     that completed just before a concurrent insert, and the pre-#2692
//     per-shard RWMutex permitted it too. Nothing here weakens a guarantee that
//     was ever offered.
//
//  2. BITMAP -> INLINE (demotion, a [Index.Delete] falling to [inlineTierMax]).
//     The writer publishes an inline image built from a COPY of the ids
//     (ToArray), so the new image aliases nothing, and it must NEVER touch the
//     old bitmap again: a reader that loaded the old shared snapshot is still
//     reaching that bitmap through the pointer it holds, and an abandoned
//     bitmap that stays frozen is what makes that read a consistent OLD image
//     rather than a moving one.
//
//     Demotion is not an optimisation. index.NodeSet never demotes on its own
//     (promote-and-never-demote, #1584), so without it a churned key would hold
//     a bitmap-tier set at cardinality 1: it would stay on the locked read path
//     for ever, forfeit the inline tier's memory win, and — the part that
//     matters — [entry.publish]'s cardinality -> tier derivation would stop
//     being TOTAL, so any later path that published that set would label a
//     MUTABLE bitmap shared == false and hand it to a lock-free reader.
//
//     What demotion is NOT is protection against a writer mutating that bitmap
//     outside the reader's lock. An entry has exactly ONE mu; every writer of
//     every tier takes it for writing and every reader of a shared snapshot
//     takes it for reading, so the two are mutually excluded whichever image
//     each is looking at. That was checked rather than assumed, and the
//     lock-scope version of this hazard does not exist.
//
//  3. A reader loads a SHARED snapshot, then a writer replaces it before the
//     reader takes RLock. The reader reads the image it already holds: either
//     that image is still the published one, in which case the RLock it now
//     holds excludes the in-place writer, or the image has been superseded, in
//     which case it is frozen for ever by state 2 above and reading it under any
//     lock at all is safe. Its answer is one publication stale, which is inside
//     the staleness state 1 already permits.
//
//     Round two re-LOADED the pointer under the lock here, for freshness rather
//     than for safety: its own note recorded that no test in this package fails
//     when the re-load is removed, and none can. Round three drops it, because
//     snap may now legitimately be nil — a demotion in that same window
//     releases it — so re-loading it unguarded would be a nil dereference on
//     the locked branch, and re-loading it guarded would answer from a THIRD
//     image without buying a guarantee. The image in hand is used instead.
//
//  4. A reader holding a NON-SHARED image concurrent with a reap. The pointer
//     (or the meta word already read) keeps the image intact, so the read
//     returns the pre-reap contents rather than crashing or reading freed
//     memory; a reaped entry is empty and stays empty, so nothing about it can
//     change under the reader.
//
//  5. THE DEMOTION WINDOW: a reader reads a POINTER tag and then finds snap
//     nil, because a writer published an inline image between the two loads.
//     [entry.publish] writes the inline tag BEFORE it releases the snapshot, so
//     a re-read of meta observes an inline tag and the read completes from the
//     word. Go's sync/atomic operations are sequentially consistent, so a
//     reader that observed the nil MUST observe that earlier meta store: one
//     re-read suffices unless a further writer has re-promoted the key in the
//     meantime, in which case the re-read finds a pointer tag with its snapshot
//     already in place. The loop therefore advances one completed publication
//     per iteration and cannot spin without an unbounded stream of tier
//     crossings on that one key, each of which needs the entry write lock.
//     TestDemotionWindowResolvesToInlineImage constructs the state directly.
type entry struct {
	// meta is the tier tag and, on the inline tags, the entire published
	// image; on metaBitmap its payload is also the demotion trigger. See the
	// meta* constants for the encoding.
	meta atomic.Uint64

	// snap is the published posting list on the two POINTER tags and NIL on the
	// two inline ones. A reader loads it only after meta has said there is one,
	// and handles the nil it can still find (state 5 above).
	//
	// # It is released on demotion, deliberately
	//
	// [entry.publishInline] stores nil when an entry leaves a pointer tier.
	// Leaving the old snapshot in place would dodge state 5 entirely, at the
	// price of retaining a 24-byte snapshot — plus, on the small tier, its
	// 64-byte backing array — for every key that was EVER wider than a
	// singleton. On a churning index that is most of them, which would defeat
	// the whole of round three.
	snap atomic.Pointer[snapshot]

	// mu serialises WRITERS against each other, and additionally guards the
	// bitmap that a shared snapshot aliases, so a reader of a shared snapshot
	// must take it for reading. Readers of an inline tag, and readers of a
	// non-shared snapshot, never take it.
	//
	// # It stays an RWMutex, and the reason is MEASURED rather than preferred
	//
	// Downgrading it to a sync.Mutex would save 16 bytes per key — a quarter of
	// what round three gave back on a singleton — and it is refused. The
	// `index-hash-rw` key distribution was reproduced and the tier each key
	// actually reaches counted: mean 2.0 ids/key at concurrency 1, 2.6 at 8, and
	// 121.0 at 1024, with 0% of keys above the inline tier at the first two
	// levels and 100% at the last. So at 1024 goroutines EVERY read takes this
	// lock for reading. An exclusive lock, or one shared across keys in a stripe
	// array, would serialise exactly those readers and restore the convoy round
	// two removed; the 16 bytes would be paid for with the +91.6% that round
	// bought at 1024. The same count is why round three is a memory change and
	// not a throughput one: levels 1 and 8 drive the inline path exclusively and
	// level 1024 drives the locked path exclusively.
	mu sync.RWMutex

	// dead marks an entry that has been detached from its shard's map —
	// reaped because it became empty, or displaced wholesale by
	// [Index.Deserialize]. It is set under mu BEFORE the entry leaves the map,
	// which is what lets a mutator holding mu learn that its entry is stale
	// WITHOUT reading the map.
	//
	// That is not a convenience, it is the deadlock avoidance. The obvious
	// alternative — hold the entry lock and re-read the shard map to confirm
	// the entry is still the published one — acquires the two locks in the
	// order entry-then-spine, while reap acquires them spine-then-entry. That
	// ABBA inversion was implemented in the sibling label index, measured, and
	// it DEADLOCKED: a pending writer on the shard mutex parks the reader half
	// of the identity check for ever while the reaper waits on the entry lock
	// the checker holds. Eight clean throughput sweeps missed it because their
	// workload only ever created keys, so reap never ran, and the race detector
	// does not detect deadlocks. See [Index] for the lock order used instead.
	//
	// It stays a plain bool rather than moving into a spare bit of meta. The
	// move was considered for round three and is unnecessary: the entry is 41
	// bytes with it and 40 without, and both land in the same 48-byte size
	// class, so folding it would buy nothing and would put a writers-only flag
	// into the one word every lock-free reader loads.
	//
	// Mutation coverage of the two guards this flag feeds is ASYMMETRIC, and
	// the asymmetry is recorded here rather than glossed over. Deleting the
	// check in [Index.Insert] is caught by the concurrent reap/read parity test
	// on 12 of 13 runs: an insert into a detached entry loses the id AND
	// increments the live shard's non-empty counter, so the counter and the read
	// surface disagree. Deleting the check in [Index.Delete] was caught on 1 run
	// in 13, and cannot be made deterministic: for the REAP flavour the mutant
	// is behaviourally equivalent, because a reaped entry is empty and removing
	// from it does nothing; for the DESERIALIZE flavour, where the detached
	// entry is still populated, the drift it causes is wiped by the very next
	// Deserialize, which restates each shard's counter absolutely rather than by
	// delta. Nor can the state be constructed directly — an entry that is dead
	// AND still published spins both retry loops for ever (see
	// TestNoPublishedEntryIsDead), and one that is dead and unpublished makes
	// Delete's first lookup miss, so the branch is never reached. The Delete
	// guard is kept on the symmetry argument with the Insert guard, whose harm
	// IS demonstrated, not on a passing test of its own.
	dead bool
}

// publish stores set as the entry's published image, deciding the tier from the
// SET ITSELF rather than from any maintained counter. It is the only way an
// image is published after construction, so nothing else in this package has to
// agree with it about what a tag means, and the maintained cardinality in
// metaBitmap's payload cannot drift away from the tier it tracks.
//
// The caller holds mu for writing, or holds the entry privately (see
// [newEntry]).
//
// # The two stores, and why their ORDER differs by direction
//
// An inline image is published by writing meta and only then releasing the
// snapshot; a pointer image by installing the snapshot and only then writing
// meta. Both orders are chosen so that every intermediate state a lock-free
// reader can observe is a state the entry really was in:
//
//   - INLINE (meta first, snap = nil second). A reader that already read the
//     old POINTER tag and now finds snap nil re-reads meta and finds the inline
//     tag this store wrote — state 5 on [entry], which the read paths resolve.
//   - POINTER (snap first, meta second). A reader that reads the old inline tag
//     answers from the old inline word: a stale image, which state 1 on [entry]
//     already permits. A reader that reads the new pointer tag is guaranteed a
//     non-nil snapshot beneath it.
//
// # What the order buys, stated exactly, because a mutation refuted the first
// # version of this paragraph
//
// It buys a HARD BOUND ON THE SPIN, not correctness. Publishing either
// direction the other way round was implemented and measured: both mutants pass
// the whole package, plain and under -race, and reasoning through them says why.
// With the snapshot released BEFORE the inline tag is written, a reader that
// finds nil re-reads meta and may still see the pointer tag the writer has not
// replaced yet, so it spins until the writer's second store lands — bounded by
// the writer's scheduling rather than by the protocol. With the pointer tag
// written BEFORE its snapshot, the same happens in the other direction, and a
// pointer-to-pointer publication additionally hands the reader the PREVIOUS
// snapshot, which is self-describing and therefore still answered correctly.
// Neither mutant returns a wrong answer.
//
// The order used here makes the re-read in state 5 on [entry] terminate in ONE
// iteration, always: Go's sync/atomic operations are sequentially consistent,
// so a reader that observed the released pointer must observe the inline tag
// stored before it. That is a latency guarantee, and it is the whole claim.
func (e *entry) publish(set index.NodeSet) {
	card := set.Cardinality()
	if card == 0 {
		e.publishInline(metaEmpty)
		return
	}
	if card == 1 {
		if id := set.Minimum(); id <= maxInlineID {
			e.publishInline(metaSingleton | id<<metaShift)
			return
		}
		// A NodeID too large for meta's 62-bit payload cannot be held inline,
		// so it is published through a snapshot like any wider list. That keeps
		// the tag -> image derivation TOTAL; see [maxInlineID] for why no real
		// workload reaches it.
	}
	shared := card > inlineTierMax
	tag := metaSmall
	if shared {
		// The payload is the demotion trigger. A cardinality that did not fit
		// would truncate to a WRONG HINT, never to a wrong tag — the tag bits
		// are ORed in last — and a wrong hint can only cost a demotion, never
		// correctness (see [entry.deleteLocked]).
		tag = metaBitmap | card<<metaShift
	}
	e.snap.Store(&snapshot{set: set, shared: shared})
	e.meta.Store(tag)
}

// publishInline publishes an image that meta carries in full, and then releases
// the snapshot the entry no longer needs. The order is load-bearing; see
// [entry.publish].
//
// The nil store is guarded by a load because having nothing to release is the
// COMMON case — an empty or singleton key that stays one. atomic.Pointer.Store
// goes through the runtime's atomic pointer store, which carries a write
// barrier, where the load is a plain atomic load; the guard keeps that barrier
// off a publication with nothing to release. That difference was NOT measured
// on its own: the shape is taken because it cannot be worse, not on a claimed
// win.
func (e *entry) publishInline(m uint64) {
	e.meta.Store(m)
	if e.snap.Load() != nil {
		e.snap.Store(nil)
	}
}

// currentLocked returns a COPY of the entry's published set together with that
// set's cardinality, for a caller that holds mu for writing and is about to
// mutate it.
//
// The copy is what makes an inline-tier mutation COPY-ON-WRITE: a reader may be
// reading the published image right now with no lock at all, so the two-word
// header is copied and the change applied to the copy (nodeset.go invariant 4).
// On the metaSingleton tag there is nothing to copy at all — the set is rebuilt
// from the word, so it aliases nothing from the start.
//
// Every branch is O(1). That is a requirement, not an observation: roaring's
// GetCardinality walks every container, and asking it here would put the 690x
// latency cliff described on [entry.deleteLocked] back on the write path.
//
// # It is a helper, and the call is REAL: measured +5.77% on one write
//
// The compiler refuses to inline it — "function too complex: cost 170 exceeds
// budget 80" — so the two call sites pay a genuine call, and that is visible:
// BenchmarkIndex_InsertByTier/Duplicate, the steady-state re-index write whose
// id is already present, read 15.07 ns on round two against 15.94 ns here
// (+5.77%, p=0.000, n=8 interleaved). Round two read the published snapshot
// straight from the site instead.
//
// It is kept as a helper anyway, on three grounds. The cost lands on a WRITE
// holding the entry lock, not on the lock-free read path the un-inlined
// [hashShard.lookup] was moved off. Spelling it out would put the tier decoding
// in three places — here, insertLocked and deleteLocked — and one image with
// one decoder is the property round three was built to get. And the same change
// that pays this 0.87 ns took BenchmarkIndex_InsertByTier/Create from 37.74 ns
// to 31.88 ns (-15.53%) and /Promote from 260.6 ns to 215.4 ns (-17.34%) by not
// allocating a snapshot the inline tags do not need.
func (e *entry) currentLocked(m uint64) (set index.NodeSet, card uint64) {
	switch m & metaTagMask {
	case metaEmpty:
		return set, 0
	case metaSingleton:
		set.Add(m >> metaShift)
		return set, 1
	case metaBitmap:
		// Not reached today: [entry.insertLocked] and [entry.deleteLocked] both
		// handle the bitmap tier in full before they get here, because a
		// bitmap-tier mutation is in place rather than copy-on-write. The tag
		// is answered anyway, from the maintained cardinality in meta's own
		// payload, so that this helper is TOTAL — a third caller cannot buy
		// roaring's O(containers) GetCardinality by accident.
		return e.snap.Load().set, m >> metaShift
	}
	// metaSmall: the tier stores its own length, so this is O(1) too.
	sn := e.snap.Load()
	return sn.set, sn.set.Cardinality()
}

// newEntry returns a live entry publishing set as its initial image.
//
// The image is stored BEFORE the entry can be reached by any other goroutine,
// which is what lets every reader resolve the entry with no regard for a
// half-built one: an entry in a shard map always has a published image, and on
// the inline tags that image is one word that was written before the entry
// escaped this frame.
func newEntry(set index.NodeSet) *entry {
	e := &entry{}
	e.publish(set)
	return e
}

// insertLocked adds id to the entry's posting list and reports whether the list
// was EMPTY beforehand, so the caller can move its shard's non-empty count. The
// caller holds mu for writing and has established that the entry is not dead.
func (e *entry) insertLocked(id uint64) (wasEmpty bool) {
	m := e.meta.Load()
	if m&metaTagMask == metaBitmap {
		// Bitmap tier: mutate the aliased bitmap IN PLACE. A per-insert Clone
		// of a large roaring bitmap is unaffordable — it is O(cardinality) on a
		// path taken once per indexed node — and no new image is needed,
		// because an insert cannot take a bitmap-tier list below
		// [inlineTierMax], so the representation and the tag both stay put.
		// Concurrent readers of this snapshot hold mu for reading, which this
		// caller excludes.
		//
		// snap is non-nil: the pointer tag is only ever stored AFTER the
		// snapshot it describes (see [entry.publish]), and a writer holding mu
		// excludes every other writer.
		bm, _ := e.snap.Load().set.Bitmap() // shared tier: the live bitmap, no allocation
		if bm.CheckedAdd(id) {
			// CheckedAdd rather than Add so the cardinality in meta's payload
			// moves on exactly the inserts that changed the set.
			e.meta.Store(metaBitmap | (m>>metaShift+1)<<metaShift)
		}
		return false
	}
	// Inline tag or small tier: COPY-ON-WRITE. The published image must not be
	// touched — a reader may be reading it right now with no lock at all — so
	// the change is applied to a copy and the result published. See state 1 on
	// [entry] for why Add is safe on the copy.
	next, card := e.currentLocked(m)
	wasEmpty = next.Add(id)
	if next.Cardinality() == card {
		// id was already present, so Add changed nothing: publish nothing and
		// allocate nothing. This is the steady-state re-index write, and
		// TestHotPathsAreAllocationFree pins it at zero allocations.
		return false
	}
	e.publish(next)
	return wasEmpty
}

// deleteLocked removes id from the entry's posting list and reports whether the
// list became EMPTY as a result. The caller holds mu for writing and has
// established that the entry is not dead.
//
// # The demotion trigger is a maintained counter, and that was MEASURED
//
// This method has to know whether a removal has brought a bitmap-tier list back
// to [inlineTierMax] in order to demote. Every exported roaring64 way to ask —
// GetCardinality, Select, Rank, GetSizeInBytes, Stats — walks every container
// and sums it, while IsEmpty reads one slice length. Asking on each delete
// turns a single-id [Index.Delete] against a wide posting list from O(1) into
// O(containers). MEASURED two ways, on ids spaced 1<<16 apart so that each id
// occupies its own container:
//
//   - Across arms, n=6, BenchmarkIndex_DeleteWideBitmap. With the counter the
//     cost is flat in the container count and level with the pre-#2692
//     baseline: 1 024 ids 201.5 ns against 207.5 ns (~, p=0.370), 65 536 ids
//     176.1 ns against 193.5 ns (+9.85%). Before it, the same benchmark read
//     2.443 us at 1 024 ids (+1 127.64%) and 125.277 us at 65 536
//     (+68 923.14%) — 690x.
//   - By controlled revert, which is what
//     TestDeleteCostDoesNotScaleWithContainerCount gates: swapping the counter
//     back for GetCardinality moved the wide-over-narrow ratio from 1.04x to
//     63.43x (1.951 us against 123.747 us per delete).
//
// That is a latency cliff, not a cost, on a posting list a real property index
// reaches easily, and the [Reliability mandate] forbids it.
//
// The counter lives in metaBitmap's payload, which is FREE: the entry has 7
// bytes of padding inside its size class and needs none of them for it (see
// [entry]). Round two kept it in a uint64 field of its own, which was free
// then and would not be now.
//
// # It is a HINT: it can cost performance, never correctness
//
// The tier label a reader trusts is [snapshot.shared], and [entry.publish]
// always derives that from the set itself. So if this counter ever drifted: too
// HIGH (including an unsigned wrap below zero) the demotion simply never fires,
// the list stays on the bitmap tier, and shared stays correctly true — the
// lock-free read path is lost, nothing else. Too LOW, a demotion fires early,
// NodeSetFromSorted rebuilds a set from the ids, and publish labels it by
// cardinality again — a wasted rebuild, nothing else. In neither direction can
// a mutable bitmap be labelled shared == false and handed to a lock-free
// reader, and in neither direction can the non-empty accounting move, because
// the emptiness this method reports after a demotion is read off the image it
// just published rather than off the counter. TestBitmapCardIsExact pins the
// counter anyway.
//
// [Reliability mandate]: https://github.com/FlavioCFOliveira/GoGraph/blob/main/CLAUDE.md
func (e *entry) deleteLocked(id uint64) (nowEmpty bool) {
	m := e.meta.Load()
	if m&metaTagMask == metaBitmap {
		bm, _ := e.snap.Load().set.Bitmap() // shared tier: the live bitmap
		if !bm.CheckedRemove(id) {
			return false // absent: nothing changed, nothing to publish
		}
		card := m>>metaShift - 1
		if card > inlineTierMax {
			// Still the bitmap tier: same image, same tag, one payload store.
			// This is the O(1) test the counter exists for; roaring's
			// GetCardinality here cost up to 690x.
			e.meta.Store(metaBitmap | card<<metaShift)
			return false
		}
		// DEMOTE — state 2 on [entry], and what keeps shared == "is on the
		// bitmap tier" true (see [inlineTierMax]). At most inlineTierMax ids
		// remain, and a roaring bitmap holds at most one container per id, so
		// ToArray walks at most that many containers. It copies, so the new
		// image aliases nothing, and bm must NEVER be touched again from here
		// on: a reader inside mu.RLock() may still be reading it.
		e.publish(index.NodeSetFromSorted(bm.ToArray()))
		// Read the emptiness off the image just published rather than off the
		// counter: publish derived that tag from the set itself, so the shard's
		// non-empty accounting cannot be moved by a drifted hint.
		return e.meta.Load()&metaTagMask == metaEmpty
	}
	// Inline tag or small tier: copy-on-write, exactly as in
	// [entry.insertLocked]. Such a removal can only reach another inline tier
	// or the small tier, so it never produces a shared image; publish holds the
	// emptied list in meta alone, which allocates nothing.
	next, card := e.currentLocked(m)
	nowEmpty = next.Remove(id)
	if next.Cardinality() == card {
		return false // absent: nothing changed, nothing to publish
	}
	e.publish(next)
	return nowEmpty
}

// toArray returns the entry's published posting list in strictly ascending
// order as a freshly allocated slice the caller owns: the representation-
// independent wire form [Index.Serialize] writes.
//
// It resolves the meta/snapshot protocol exactly as the read paths do, and
// takes the entry read lock only for a bitmap-tier image. Unlike them it is a
// helper rather than being spelled out at its call site, because Serialize
// already allocates a copy of every key and sorts the result, so one
// un-inlined call is unmeasurable against it (see [hashShard.lookup] for the
// measurement that decided the other way on the hot paths).
func (e *entry) toArray() []uint64 {
	for {
		m := e.meta.Load()
		if m&metaPtrBit == 0 {
			if m&metaTagMask == metaSingleton {
				return []uint64{m >> metaShift}
			}
			return nil // metaEmpty: Serialize skips the key entirely
		}
		sn := e.snap.Load()
		if sn == nil {
			continue // the demotion window; state 5 on [entry]
		}
		if !sn.shared {
			return sn.set.ToArray()
		}
		e.mu.RLock()
		ids := sn.set.ToArray()
		e.mu.RUnlock()
		return ids
	}
}

// hashShard is one shard of the key space: a value→entry map behind the SPINE
// lock, plus a running count of the entries in it whose set is non-empty.
type hashShard[V comparable] struct {
	entries map[V]*entry
	mu      sync.RWMutex

	// nonEmpty counts the entries in this shard whose set holds at least one
	// NodeID. It exists so [Index.DistinctValues] stays O(shardCount) — walking
	// the maps and taking every entry's read lock would make it O(entries),
	// which on a 10 000 000-distinct-value index is a complexity regression —
	// while still refusing to count the empty-but-unreaped entry the per-entry
	// geometry made observable (see [Index]).
	//
	// It is maintained ONLY on the empty <-> non-empty transition, under the
	// entry lock, from the return values [index.NodeSet.Add] and
	// [index.NodeSet.Remove] already provide. A steady-state workload — adding
	// a second node to a populated value, or removing one of several — fires
	// neither branch, so this adds no atomic traffic to the common path.
	nonEmpty atomic.Int64
}

// New returns an empty hash index.
//
// The returned Index is safe for concurrent use.
func New[V comparable]() *Index[V] {
	idx := &Index[V]{}
	for i := range idx.shards {
		idx.shards[i].entries = make(map[V]*entry)
	}
	return idx
}

func (i *Index[V]) shard(v V) *hashShard[V] {
	return &i.shards[maphash.Comparable(seed, v)&shardMask]
}

// lookup returns value's entry under the SPINE read lock, which it releases
// before returning. The caller therefore holds no lock on return and is free to
// acquire the entry's own lock, which is what keeps the lock order one-way.
//
// The returned entry may have been reaped by the time the caller locks it;
// callers that mutate detect that through [entry.dead], and callers that only
// read need not care, because a reaped entry is empty and stays empty.
//
// # Only the MUTATORS call this; the read paths spell it out (rmp #2692)
//
// This helper cannot be inlined — it holds a map access and two mutex calls, so
// it is far over the inlining budget — and on the inline-tier read path that
// un-inlined call was the ENTIRE residual cost of round two over the pre-#2692
// baseline. Measured, interleaved, n=6, on the same host in one session:
// BenchmarkIndex_SeekSingleton/Append read 17.16 ns through this helper against
// a 16.53 ns baseline (+3.84%, p=0.009) and 16.62 ns with the four lines
// spelled out at the call site (p=0.818 against baseline — no detectable
// difference at all). BenchmarkIndex_CardinalityInlineTier moved the same way,
// from +2.24%/+2.18% to no detectable difference.
//
// So [Index.Lookup], [Index.LookupAppend], [Index.Cardinality] and
// [Index.Contains] do not call this. [Index.Insert] and [Index.Delete] do: they
// call it inside a retry loop and around a whole locked mutation, so one call
// is not measurable against that, and the loop is much easier to read with the
// shard read factored out.
//
// Two hypotheses for that residual were tested FIRST and both refuted, so the
// obvious explanations do not need re-testing:
//
//   - "It is the *entry indirection." No: the ceiling probe that deleted the
//     entry read lock kept the indirection and lost nothing (see [snapshot]).
//   - "It is the second dereference, entry -> snapshot, landing on a cold cache
//     line." No: co-locating the first snapshot inside the entry, so that both
//     live in one 64-byte allocation, measured 16.82 ns against 16.83 ns for the
//     separate allocation — no difference — while costing 16 extra bytes on
//     every key that is ever re-published. Declined.
func (s *hashShard[V]) lookup(value V) (*entry, bool) {
	s.mu.RLock()
	e, ok := s.entries[value]
	s.mu.RUnlock()
	return e, ok
}

// reap drops value from the shard when the entry the caller emptied is still
// the published one AND is still empty, so the map does not accumulate keys
// whose set holds nothing. A completed [Index.Delete] that removed the last
// NodeID therefore leaves the key absent, exactly as the single-critical-section
// Delete this replaced did.
//
// want is the entry the caller observed becoming empty. A different pointer
// under the same key means the entry was displaced (by [Index.Deserialize]) and
// re-created since, so the caller's observation says nothing about the entry
// published now and reap declines. A same pointer that is no longer empty means
// a concurrent [Index.Insert] re-added a node between the removal and this
// call, and reap declines for that too.
//
// Lock order: SPINE then ENTRY, released in the reverse order. The dead flag is
// published under the entry lock BEFORE the entry leaves the map, so a mutator
// that already holds this entry lock observes dead and retries rather than
// writing into a detached entry.
//
// It does not touch nonEmpty: the caller already decremented it when the set
// became empty, and an empty entry contributes nothing to that count whether it
// is in the map or not.
func (s *hashShard[V]) reap(value V, want *entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[value]
	if !ok || e != want {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// An entry is empty exactly when its meta tag is metaEmpty: [entry.publish]
	// derives that tag from the set's own cardinality, so the word is the whole
	// answer and no snapshot has to be loaded to obtain it.
	if e.meta.Load()&metaTagMask != metaEmpty {
		return
	}
	e.dead = true
	delete(s.entries, value)
}

// nanKey reports whether v is a float32 or float64 IEEE 754 NaN.
// Go maps use the language == operator: NaN != NaN is always true, so
// inserting a NaN key creates an entry that can never be looked up or
// deleted, causing unbounded accumulation (task #1408). The Insert,
// Delete and Lookup methods skip NaN values entirely to prevent this.
//
//nolint:gocritic // dupSubExpr: f != f is the canonical generic NaN test.
func nanKey[V comparable](v V) bool {
	switch f := any(v).(type) {
	case float64:
		return f != f
	case float32:
		return f != f
	}
	return false
}

// Insert records that node carries the given value. Insert is a no-op
// when value is a float32 or float64 NaN: Go map equality is language-
// fixed (NaN != NaN), so a NaN map key can never be looked up or
// deleted; skipping it prevents unbounded accumulation (task #1408).
//
// Safe for concurrent use. It contends only with other operations on the SAME
// value, plus the brief shared shard lookup; creating a value not yet in the
// index additionally takes that shard's write lock for the publication alone.
//
// The retry loop exists because an entry can be reaped or displaced between the
// shard lookup and the entry lock. It terminates: the next iteration either
// misses the map, and creates a fresh entry under the shard write lock which no
// concurrent reaper can drop before this call has published and filled it (reap
// needs the entry lock the creator still holds, and refuses a non-empty set),
// or finds a live entry and writes into it.
//
// The loop body is written out here and again in [Index.Delete] rather than
// driven through a shared mutate(fn func(*entry)) helper, as graph/index/label
// does. The reason is legibility of the lock protocol, NOT allocation: the
// protocol is what deadlocked when this geometry was first built in the label
// index, so each acquisition and release is spelled out at the site that
// performs it instead of living behind a callback. The two bodies also differ
// in substance — Insert creates, Delete reaps, and they move the shard's
// non-empty counter in opposite directions off different return values.
//
// The allocation argument for the duplication was MEASURED AND REFUTED: a
// closure passed to a helper that only calls it does not escape, so the
// label-style helper is allocation-free too (both read 0 allocs/op via
// testing.AllocsPerRun). Do not re-justify this shape on allocation grounds.
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	if nanKey(value) {
		return
	}
	id := uint64(node)
	s := i.shard(value)
	for {
		e, ok := s.lookup(value)
		if !ok {
			s.mu.Lock()
			if e, ok = s.entries[value]; !ok {
				// The entry is built COMPLETE and only then published, so no
				// entry lock is needed and none is taken: the set is mutated
				// while it is still private to this frame, and no reader or
				// reaper can reach a half-formed entry. Only the SPINE lock is
				// held here, so the lock order cannot be inverted either.
				var set index.NodeSet
				set.Add(id)
				s.entries[value] = newEntry(set)
				// Unconditional: a brand-new entry has just taken its first
				// node, so this is definitionally an empty -> non-empty
				// transition. Add's wasEmpty return has nothing to add.
				s.nonEmpty.Add(1)
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
		}
		e.mu.Lock()
		if !e.dead {
			if e.insertLocked(id) {
				// The list was empty: this entry was emptied by a Delete whose
				// reap has not completed (or has declined). It counts again.
				// The common case — adding to a populated value — takes neither
				// this branch nor any atomic read-modify-write.
				s.nonEmpty.Add(1)
			}
			e.mu.Unlock()
			return
		}
		// Reaped or displaced between the lookup and the lock. Drop the entry
		// lock and start again from the shard — never read the shard map here.
		e.mu.Unlock()
	}
}

// Delete removes node from the set associated with value. No-op if
// absent or if value is a NaN (see [Index.Insert] for the rationale).
//
// Removing the LAST NodeID under a value drops the value's key entirely, so a
// completed Delete leaves [Index.DistinctValues] and [Index.Serialize] with no
// trace of it. The removal and the key drop are two critical sections rather
// than one; see [Index] for what a concurrent reader can observe in between.
//
// Safe for concurrent use.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	if nanKey(value) {
		return
	}
	s := i.shard(value)
	for {
		e, ok := s.lookup(value)
		if !ok {
			return
		}
		e.mu.Lock()
		if e.dead {
			// Reaped or displaced between the lookup and the lock. Drop the
			// entry lock and start again from the shard — never read the shard
			// map here. This terminates: a dead entry is already out of the
			// map, so the next lookup misses and returns.
			e.mu.Unlock()
			continue
		}
		nowEmpty := e.deleteLocked(uint64(node))
		if nowEmpty {
			s.nonEmpty.Add(-1)
		}
		e.mu.Unlock()
		if nowEmpty {
			s.reap(value, e)
		}
		return
	}
}

// Lookup returns a clone of the Roaring bitmap of NodeIDs that carry
// the given value, or an empty bitmap when the value is unknown or is
// a NaN (see [Index.Insert] for the rationale).
// Clone avoids returning the live bitmap to the caller, which could
// otherwise be mutated by concurrent writers.
// Safe for concurrent use. The shard lock is released before the entry lock is
// taken, so this read blocks no writer of any other value.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	if nanKey(value) {
		return roaring64.New()
	}
	// SPINE read lock, released before the entry is touched: the one-way lock
	// order (see [Index]). Spelled out rather than calling [hashShard.lookup],
	// which does not inline — see there for the measurement.
	s := i.shard(value)
	s.mu.RLock()
	e, ok := s.entries[value]
	s.mu.RUnlock()
	if !ok {
		return roaring64.New()
	}
	for {
		m := e.meta.Load()
		if m&metaPtrBit == 0 {
			// INLINE: the whole image is this one word, so there is no pointer
			// to chase and no lock to take. An entry reaped between the shard
			// lookup and this read is empty, and an empty bitmap is exactly the
			// answer an absent value gets.
			bm := roaring64.New()
			if m&metaTagMask == metaSingleton {
				bm.Add(m >> metaShift)
			}
			return bm
		}
		sn := e.snap.Load()
		if sn == nil {
			continue // the demotion window; state 5 on [entry]
		}
		if !sn.shared {
			// Immutable for ever: no lock is taken, and Bitmap has already
			// materialised a fresh caller-owned bitmap from the sorted ids, so
			// no clone is needed either.
			bm, _ := sn.set.Bitmap()
			return bm
		}
		e.mu.RLock()
		bm, shared := sn.set.Bitmap()
		if shared {
			// The bitmap tier: the returned bitmap ALIASES the live one, so
			// clone it under the lock to give the caller the independent copy
			// Lookup has always contracted to return.
			bm = bm.Clone()
		}
		e.mu.RUnlock()
		return bm
	}
}

// LookupAppend appends the NodeIDs carrying value to dst in strictly
// ascending order and returns the extended slice, draining the posting list
// clone-free: out of the entry's meta word alone for a singleton, out of an
// immutable image for a small list, and under the value's entry read lock only
// for a bitmap-tier one. It is the allocation-light
// alternative to [Index.Lookup] for callers that iterate the result once —
// the dominant equality index-seek shape: a singleton or small posting list
// yields its ids with no heap allocation when dst has spare capacity, where
// Lookup would materialise (or clone) a full roaring bitmap plus an iterator.
// A NaN key or an unknown value appends nothing. The appended ids are an
// independent snapshot, so the caller may iterate them after the lock is
// released, exactly as with the cloned bitmap Lookup returns.
//
// Safe for concurrent use. The shard lock is released before the entry lock is
// taken, so this read blocks no writer of any other value.
func (i *Index[V]) LookupAppend(value V, dst []uint64) []uint64 {
	if nanKey(value) {
		return dst
	}
	// SPINE read lock, released before the entry is touched: the one-way lock
	// order (see [Index]). Spelled out rather than calling [hashShard.lookup],
	// which does not inline — see there for the measurement.
	s := i.shard(value)
	s.mu.RLock()
	e, ok := s.entries[value]
	s.mu.RUnlock()
	if !ok {
		return dst
	}
	for {
		m := e.meta.Load()
		if m&metaPtrBit == 0 {
			// The cheapest read in the package, and the one round three exists
			// to serve: a singleton posting list is ONE WORD, drained with no
			// pointer load, no snapshot, and no synchronisation whatsoever.
			if m&metaTagMask == metaSingleton {
				return append(dst, m>>metaShift)
			}
			return dst
		}
		sn := e.snap.Load()
		if sn == nil {
			continue // the demotion window; state 5 on [entry]
		}
		if !sn.shared {
			// Still lock-free: a small posting list is drained straight out of
			// an image that is immutable for ever.
			return sn.set.AppendTo(dst)
		}
		e.mu.RLock()
		dst = sn.set.AppendTo(dst)
		e.mu.RUnlock()
		return dst
	}
}

// Cardinality returns the number of NodeIDs associated with value.
// It is exposed for query planners to choose between index lookup
// and full-scan plans.
//
// Safe for concurrent use, and this is the read the per-entry geometry exists
// for: the shard lock is held only for the map read and released before the
// entry lock is taken, so a concurrent [Index.Insert] on ANY other value in the
// same shard no longer parks this caller (rmp #2692; see [Index]).
func (i *Index[V]) Cardinality(value V) uint64 {
	// SPINE read lock, released before the entry is touched: the one-way lock
	// order (see [Index]). Spelled out rather than calling [hashShard.lookup],
	// which does not inline — see there for the measurement.
	s := i.shard(value)
	s.mu.RLock()
	e, ok := s.entries[value]
	s.mu.RUnlock()
	if !ok {
		return 0
	}
	for {
		m := e.meta.Load()
		if m&metaPtrBit == 0 {
			// INLINE: metaEmpty and metaSingleton are numerically their own
			// cardinality, so the tag IS the answer — one atomic load, one
			// mask, no pointer and no lock.
			return m & metaTagMask
		}
		sn := e.snap.Load()
		if sn == nil {
			continue // the demotion window; state 5 on [entry]
		}
		if !sn.shared {
			return sn.set.Cardinality()
		}
		e.mu.RLock()
		c := sn.set.Cardinality()
		e.mu.RUnlock()
		return c
	}
}

// Contains reports whether node is in the set associated with value.
// Faster than Lookup when only existence matters.
//
// Safe for concurrent use. The shard lock is released before the entry lock is
// taken, so this read blocks no writer of any other value.
func (i *Index[V]) Contains(value V, node graph.NodeID) bool {
	// SPINE read lock, released before the entry is touched: the one-way lock
	// order (see [Index]). Spelled out rather than calling [hashShard.lookup],
	// which does not inline — see there for the measurement.
	s := i.shard(value)
	s.mu.RLock()
	e, ok := s.entries[value]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	for {
		m := e.meta.Load()
		if m&metaPtrBit == 0 {
			// INLINE: an empty list contains nothing, and a singleton contains
			// exactly the id in meta's payload.
			return m&metaTagMask == metaSingleton && m>>metaShift == uint64(node)
		}
		sn := e.snap.Load()
		if sn == nil {
			continue // the demotion window; state 5 on [entry]
		}
		if !sn.shared {
			return sn.set.Contains(uint64(node))
		}
		e.mu.RLock()
		c := sn.set.Contains(uint64(node))
		e.mu.RUnlock()
		return c
	}
}

// DistinctValues returns the number of distinct values currently indexed —
// that is, the number of keys whose NodeID set is NON-EMPTY. Exposed for
// cardinality estimation by the query planner.
//
// # Why non-empty rather than "keys in the map"
//
// [Index.Delete] removes the last NodeID under the entry lock and drops the key
// under the shard write lock, in that order, so a key can transiently be
// present with an empty set (see [Index]). This method must not count it:
// cypher.hashIndexKind reads DistinctValues() == 0 as the authoritative "this
// string hash index holds no data" test, and a false non-zero would pin a
// parameter compared against that property to String and reject an integer
// parameter with a spurious ParamTypeError — rmp #1983, the regression that
// guard exists to prevent. So the count is maintained per shard on the
// empty <-> non-empty transition (see hashShard.nonEmpty) rather than read off
// map length.
//
// It follows that a payload deserialised with a zero-length posting list — which
// [Index.Serialize] never writes, so only a crafted or corrupt image can carry
// one — installs a key this method does not count.
//
// # Cost and consistency
//
// It sums shardCount atomic loads: O(shardCount), independent of how many
// values are indexed, and it takes NO lock at all. The sum is assembled shard
// by shard, so it is a per-shard snapshot rather than one image of the whole
// index; a value moved between two shards' counts by concurrent writers may be
// counted once, or not at all, but never twice.
//
// Safe for concurrent use.
func (i *Index[V]) DistinctValues() uint64 {
	var n int64
	for k := range i.shards {
		n += i.shards[k].nonEmpty.Load()
	}
	// n cannot be negative: each shard's counter is the exact sum of its
	// entries' non-empty indicators, every transition of which is applied
	// once, under that entry's write lock. The clamp is fail-safe rather than
	// fail-silent — it keeps a hypothetical accounting bug from wrapping to a
	// huge positive value and re-introducing rmp #1983, which is the failure
	// direction that matters. TestDistinctValues_SettlesToZeroUnderConcurrentDelete
	// asserts the invariant directly.
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// Kind returns "hash" — satisfies [index.Subscriber].
func (*Index[V]) Kind() string { return "hash" }

// Apply maintains a bound index (see [NewBound]) from the
// [index.Manager] change fan-out; it is a no-op for an unbound index
// (see [New]), which cannot reliably interpret arbitrary
// [index.Change] values without the caller-supplied binding (property
// key + value-type coercion).
//
// For a bound index the rules are, per change:
//
//   - SetNodeProperty on the bound property: the old value (when
//     present and projectable) is deleted unconditionally, and the new
//     value is inserted when the node is eligible in the graph's final
//     state. The unconditional old-value delete is what clears a stale
//     entry even when a label removal in the same batch is replayed
//     before the property change.
//   - DelNodeProperty on the bound property: the old value is deleted.
//   - Add/RemoveNodeLabel on the bound label: the node's CURRENT
//     property value is inserted / deleted. Because changes are
//     applied at commit time the current value is the transaction's
//     final value, so an interleaved property change in the same batch
//     converges to the same final state regardless of replay order.
//
// Apply is idempotent (bitmap add/remove) and safe for concurrent use with readers.
// Edge changes and changes for other properties/labels are ignored.
//
// # What makes CONCURRENT Apply calls safe, restated (rmp #2345)
//
// This used to say "writers are serialised upstream by the engine's single-writer
// transaction contract". THAT IS FALSE since rmp #2320: commitUnderBarrier runs under
// a SHARED hold, so two transactions flush their index buffers concurrently and two
// Apply calls can interleave.
//
// It is nonetheless sound, and the reason is worth stating because it is not the one
// that was written down. Each mutation is made under the target value's own entry
// lock ([Index.Insert], [Index.Delete]), so no individual add or remove can tear.
// Since rmp #2692 that lock is per VALUE rather than per shard, which narrows what
// two concurrent Apply calls contend on but changes nothing about the tearing
// argument: a single add or remove is still one critical section. What
// serialisation would additionally buy is atomicity of the DELETE-then-INSERT pair in
// the OpSetNodeProperty arm — and the only interleaving that could strand a stale
// entry is two transactions writing the SAME node's bound property, which the
// substrate REFUSES: graph/lpg's property write path takes a write-write conflict
// check against the node's version-chain head (graph/lpg/property.go), so one of the
// two aborts and never reaches its Apply at all.
//
// So the ordering guarantee comes from conflict detection on the object, not from
// exclusion on the writers. If that check is ever narrowed, this comment is the one
// to revisit.
//
// On recovery from a corrupted snapshot, the index is left empty;
// callers re-populate via [Index.Insert] from the live LPG.
func (i *Index[V]) Apply(c index.Change) {
	b := i.binding
	if b == nil {
		return
	}
	switch c.Op {
	case index.OpSetNodeProperty:
		if c.Property != b.PropertyID {
			return
		}
		if old, ok := b.Project(c.OldValue); ok {
			i.Delete(old, c.Node)
		}
		if nv, ok := b.Project(c.NewValue); ok && b.Eligible(c.Node) {
			i.Insert(nv, c.Node)
		}
	case index.OpDelNodeProperty:
		if c.Property != b.PropertyID {
			return
		}
		if old, ok := b.Project(c.OldValue); ok {
			i.Delete(old, c.Node)
		}
	case index.OpAddNodeLabel:
		if c.Label != b.LabelID {
			return
		}
		if v, ok := b.CurrentValue(c.Node); ok && b.Eligible(c.Node) {
			i.Insert(v, c.Node)
		}
	case index.OpRemoveNodeLabel:
		if c.Label != b.LabelID {
			return
		}
		if v, ok := b.CurrentValue(c.Node); ok {
			i.Delete(v, c.Node)
		}
	}
}

// hashMagic is the four-byte magic at the head of a serialised hash
// index ('SHSH' little-endian — 0x48534853).
const hashMagic uint32 = 0x48534853

// hashFormatVersion is the on-disk format version of a serialised
// hash index.
const hashFormatVersion uint32 = 1

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// encodeValue serialises one supported value type to bytes. The
// generic Index[V] supports value-type encoding for the most common
// LPG property kinds; other types return
// [index.ErrIndexValueTypeUnsupported]. Callers that need to
// persist an index keyed by an exotic V should convert to one of
// the supported types before registering the index for snapshot.
//
// Supported types and their wire form:
//
//	string   -> raw utf-8 bytes
//	[]byte   -> raw bytes
//	int64    -> 8 bytes little-endian two's-complement
//	int32    -> 4 bytes little-endian
//	uint64   -> 8 bytes little-endian
//	uint32   -> 4 bytes little-endian
//	float64  -> 8 bytes math.Float64bits little-endian
//	bool     -> 1 byte (0x00 / 0x01)
func encodeValue[V comparable](v V) ([]byte, error) {
	switch x := any(v).(type) {
	case string:
		return []byte(x), nil
	case []byte:
		return x, nil
	case int64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case int32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], uint32(x))
		return buf[:], nil
	case uint64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], x)
		return buf[:], nil
	case uint32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], x)
		return buf[:], nil
	case float64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(x))
		return buf[:], nil
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	}
	return nil, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, v)
}

// decodeValue is the inverse of [encodeValue]. It is generic over V
// and works by populating a zero V of the right kind from the buffer.
// Like encodeValue it supports the subset of types documented above;
// any other V returns [index.ErrIndexValueTypeUnsupported].
//
//nolint:gocyclo // type switch over supported value kinds
func decodeValue[V comparable](b []byte) (V, error) {
	var zero V
	switch any(zero).(type) {
	case string:
		var out V
		// safe: V is string here
		assignAny(&out, string(b))
		return out, nil
	case []byte:
		var out V
		cp := make([]byte, len(b))
		copy(cp, b)
		assignAny(&out, cp)
		return out, nil
	case int64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: int64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := int64(binary.LittleEndian.Uint64(b))
		var out V
		assignAny(&out, v)
		return out, nil
	case int32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: int32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := int32(binary.LittleEndian.Uint32(b))
		var out V
		assignAny(&out, v)
		return out, nil
	case uint64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: uint64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := binary.LittleEndian.Uint64(b)
		var out V
		assignAny(&out, v)
		return out, nil
	case uint32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: uint32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := binary.LittleEndian.Uint32(b)
		var out V
		assignAny(&out, v)
		return out, nil
	case float64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: float64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(b))
		var out V
		assignAny(&out, v)
		return out, nil
	case bool:
		if len(b) != 1 {
			return zero, fmt.Errorf("%w: bool wants 1 byte, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, b[0] != 0)
		return out, nil
	}
	return zero, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, zero)
}

// assignAny copies src into *dst, treating dst as an any. The
// caller must guarantee dst's concrete type matches src.
func assignAny[V any](dst *V, src any) {
	*dst = src.(V)
}

// Serialize writes every (value, NodeID-set) pair currently in the
// index to w in the format documented in docs/persistence.md:
//
//	uint32 magic ('SHSH')
//	uint32 formatVersion
//	uint64 entryCount
//	repeat entryCount times:
//	  uint32 valueLen
//	  [valueLen]byte value (kind-specific encoding)
//	  uint64 idCount
//	  [idCount]uint64 NodeIDs (sorted ascending)
//	uint32 crc32c (little-endian, covers every byte above)
//
// Returns [index.ErrIndexValueTypeUnsupported] when V is not one of
// the documented supported types.
//
// Safe for concurrent use. Serialize holds one shard's read lock at a time and,
// within it, each value's entry read lock — the permitted SPINE-then-ENTRY
// order (see [Index]). The image is therefore per-shard consistent rather than
// index-wide consistent; callers needing a whole-index point-in-time image must
// quiesce writers themselves. That was already true before the per-entry
// geometry, because no writer has ever taken an index-wide lock.
func (i *Index[V]) Serialize(w io.Writer) error {
	type wireEntry struct {
		key []byte
		ids []uint64
	}
	// Snapshot every shard under its RLock and materialise into a
	// flat slice. We sort the slice by raw key bytes for
	// deterministic output (helps fixture diffs and test stability).
	var entries []wireEntry
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.RLock()
		if entries == nil {
			entries = make([]wireEntry, 0, len(s.entries))
		}
		for v, e := range s.entries {
			b, err := encodeValue(v)
			if err != nil {
				s.mu.RUnlock()
				return err
			}
			// Clone the bytes so we do not retain references into the
			// shard map's key (string headers can be aliased safely
			// but []byte keys are not allowed for comparable maps).
			cp := make([]byte, len(b))
			copy(cp, b)
			// ToArray is the sorted-ascending logical NodeID list — the
			// representation-independent wire form, identical to the
			// pre-refactor bm.ToArray().
			//
			// Lock order: SPINE (held) then ENTRY, one entry at a time — and
			// no entry lock at all for an immutable snapshot.
			ids := e.toArray()
			if len(ids) == 0 {
				// An empty posting list is not emitted. Before rmp #2692 an
				// empty entry could not exist — Delete removed the last id and
				// dropped the key in one critical section — so no image has
				// ever carried one, and Deserialize would install it as a key
				// that nothing ever reaps. Skipping keeps the on-disk contract
				// exactly as it was; entryCount is written from len(entries)
				// after this loop, so the count stays consistent.
				continue
			}
			entries = append(entries, wireEntry{key: cp, ids: ids})
		}
		s.mu.RUnlock()
	}
	sort.Slice(entries, func(a, b int) bool {
		return bytes.Compare(entries[a].key, entries[b].key) < 0
	})

	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, hashMagic); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, hashFormatVersion); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(entries))); err != nil {
		return err
	}
	for k := range entries {
		if uint64(len(entries[k].key)) > uint64(^uint32(0)) {
			return fmt.Errorf("hash: value too long to serialize: %d", len(entries[k].key))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(entries[k].key))); err != nil {
			return err
		}
		if _, err := tee.Write(entries[k].key); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, uint64(len(entries[k].ids))); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, entries[k].ids); err != nil {
			return err
		}
	}

	if err := binary.Write(bw, binary.LittleEndian, hasher.Sum32()); err != nil {
		return err
	}
	return bw.Flush()
}

// Deserialize replaces the receiver's state with the contents of r.
// Returns [index.ErrIndexCorrupted] on structural or CRC errors and
// [index.ErrIndexValueTypeUnsupported] when V cannot be decoded.
//
// # Concurrency: shard-by-shard, deliberately NOT atomic across shards
//
// The replacement is applied one shard at a time under that shard's write lock,
// so a concurrent reader can observe a half-replaced index. That is a
// documented property this package's caller DEPENDS ON, not an oversight:
// cypher confines every hydration to engine construction, before the
// index.Manager the planner reads is published to any other goroutine, and
// panics on a later attempt rather than degrading silently
// (cypher/index_hydration.go, errHydrationAfterPublish). Making this atomic
// across shards would make that comment and its guard wrong; making it less
// atomic would break the per-shard consistency each reader does rely on.
//
// Within a shard the swap is safe against an in-flight mutator: every displaced
// entry is marked dead under its OWN lock, while the shard write lock is held
// (the permitted SPINE-then-ENTRY order), so a mutator parked on a displaced
// entry learns it is stale and retries against the new map instead of writing
// into an entry nothing will ever read again. Marking every displaced entry
// also drains those in-flight mutations before the non-empty counter is
// restated, which is what makes the restated count exact.
//
//nolint:gocyclo // index deserialize: header + per-entry decode + per-step bounds checks
func (i *Index[V]) Deserialize(r io.Reader) error {
	all, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%w: read: %w", index.ErrIndexCorrupted, err)
	}
	if len(all) < 4 {
		return fmt.Errorf("%w: short payload", index.ErrIndexCorrupted)
	}
	body := all[:len(all)-4]
	trailer := binary.LittleEndian.Uint32(all[len(all)-4:])
	if got := crc32.Checksum(body, castagnoli); got != trailer {
		return fmt.Errorf("%w: crc32c mismatch: got %d, want %d",
			index.ErrIndexCorrupted, got, trailer)
	}

	br := bufio.NewReader(bytes.NewReader(body))
	var magic, version uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		return fmt.Errorf("%w: magic: %w", index.ErrIndexCorrupted, err)
	}
	if magic != hashMagic {
		return fmt.Errorf("%w: bad magic %#x", index.ErrIndexCorrupted, magic)
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("%w: version: %w", index.ErrIndexCorrupted, err)
	}
	if version != hashFormatVersion {
		return fmt.Errorf("%w: unsupported format version %d",
			index.ErrIndexCorrupted, version)
	}
	var entryCount uint64
	if err := binary.Read(br, binary.LittleEndian, &entryCount); err != nil {
		return fmt.Errorf("%w: entryCount: %w", index.ErrIndexCorrupted, err)
	}
	if entryCount > 1<<40 {
		return fmt.Errorf("%w: implausible entryCount %d",
			index.ErrIndexCorrupted, entryCount)
	}

	// Build the fresh maps, then swap them in shard by shard.
	//
	// Only the MAPS are held here, not [shardCount]hashShard[V]: a shard now
	// carries a sync.RWMutex and an atomic.Int64, so a stack array of 256 of
	// them would put 256 mutexes and 256 counters in this frame purely to be
	// thrown away after their maps were extracted.
	var fresh [shardCount]map[V]*entry
	for k := range fresh {
		fresh[k] = make(map[V]*entry)
	}

	// scratch de-reflects the per-key fixed headers (keyLen, idCount): the
	// previous binary.Read(br, LE, &scalar) boxed the destination pointer into
	// `any`, costing one allocation per scalar per key. io.ReadFull into a reused
	// 8-byte buffer + binary.LittleEndian is byte-identical and allocation-free.
	var scratch [8]byte
	for e := uint64(0); e < entryCount; e++ {
		if _, err := io.ReadFull(br, scratch[:4]); err != nil {
			return fmt.Errorf("%w: keyLen: %w", index.ErrIndexCorrupted, err)
		}
		keyLen := binary.LittleEndian.Uint32(scratch[:4])
		if uint64(keyLen) > uint64(len(body)) {
			return fmt.Errorf("%w: implausible keyLen %d",
				index.ErrIndexCorrupted, keyLen)
		}
		kbuf := make([]byte, keyLen)
		if _, err := io.ReadFull(br, kbuf); err != nil {
			return fmt.Errorf("%w: key bytes: %w", index.ErrIndexCorrupted, err)
		}
		v, derr := decodeValue[V](kbuf)
		if derr != nil {
			return derr
		}
		if _, err := io.ReadFull(br, scratch[:8]); err != nil {
			return fmt.Errorf("%w: idCount: %w", index.ErrIndexCorrupted, err)
		}
		idCount := binary.LittleEndian.Uint64(scratch[:8])
		// Each id occupies 8 wire bytes, so len(body)/8 is the true ceiling on
		// how many ids the component's bytes could hold. The prior len(body)
		// bound admitted an ~8x over-declaration, forcing make([]uint64, idCount)
		// (plus binary.Read's transient buffer, ~16x total) to allocate before
		// the short read failed. Bounded amplification, tightened for
		// defense-in-depth to match the len/elemSize cap-hint pattern elsewhere.
		if idCount > uint64(len(body))/8 {
			return fmt.Errorf("%w: implausible idCount %d",
				index.ErrIndexCorrupted, idCount)
		}
		ids := make([]uint64, idCount)
		if err := binary.Read(br, binary.LittleEndian, ids); err != nil {
			return fmt.Errorf("%w: ids: %w", index.ErrIndexCorrupted, err)
		}
		// The writer emits ids in sorted-ascending ToArray order, so
		// NodeSetFromSorted picks the cheapest representation for this
		// cardinality without a re-sort. Ownership of ids transfers.
		// Pick the shard the way the live Insert path would.
		k := maphash.Comparable(seed, v) & shardMask
		fresh[k][v] = newEntry(index.NodeSetFromSorted(ids))
	}

	// Restate each shard's non-empty count from the maps that were actually
	// built, rather than by counting as entries were parsed.
	//
	// Counting during the parse would over-report on a payload that names the
	// SAME key twice: the second entry replaces the first in the map but would
	// have incremented a second time, and an over-reported DistinctValues is
	// exactly the rmp #1983 failure direction (see [Index.DistinctValues]).
	// Serialize cannot emit a duplicate key — its source is a map — so only a
	// crafted or corrupt image can, which is precisely the input that must not
	// be trusted to be well formed. Counting the built maps is duplicate-proof
	// by construction and costs one extra pass over entries that were just
	// parsed, at hydration time only. These maps are not published yet, so no
	// entry lock is needed to read their sets.
	var freshNonEmpty [shardCount]int64
	for k := range fresh {
		for _, e := range fresh[k] {
			if e.meta.Load()&metaTagMask != metaEmpty {
				freshNonEmpty[k]++
			}
		}
	}

	// Shard-by-shard swap; see the method doc for why this is not atomic
	// across shards.
	for k := range i.shards {
		s := &i.shards[k]
		s.mu.Lock()
		for _, e := range s.entries {
			// Lock order: SPINE (held) then ENTRY, one at a time. Marking the
			// displaced entry dead makes an in-flight mutator retry against the
			// new map rather than write into an entry nothing will ever read
			// again — and, because marking needs the entry lock, it also waits
			// out any mutation already in progress, so every counter update
			// that belongs to the OLD map lands before the Store below.
			e.mu.Lock()
			e.dead = true
			e.mu.Unlock()
		}
		s.entries = fresh[k]
		s.nonEmpty.Store(freshNonEmpty[k])
		s.mu.Unlock()
	}
	return nil
}

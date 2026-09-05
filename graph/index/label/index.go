// Package label provides a Roaring-bitmap-backed inverted index from
// label identifiers to the NodeIDs that carry them.
//
// The index is the substrate for label-filtered queries such as
// "find every node with label Person and label Active": each label
// is represented by a 64-bit Roaring bitmap, and compound queries
// are answered via bitmap intersection / union, which Roaring
// implements with run-length and array-bitmap hybrids.
//
// Index is safe for concurrent use; [Index] documents the full contract,
// including the lock order every code path in this package obeys.
//
// # Scope, and why lpg uses the unscoped constructor
//
// [Index.Scope] is consulted in exactly one place — [Index.Apply] — and Apply
// runs only through the [index.Manager] fan-out. No Index is ever registered
// with a Manager in this module: [index.Manager.CreateIndex] is the sole writer
// of the subscriber registry, and every production call site registers a
// btree or hash index. The only two Index values in the module are lpg's
// nodeIdx and edgeIdx, which lpg maintains by calling Add and Remove directly.
//
// So lpg builds BOTH with [NewIndex], the edge index included, because on a
// directly-driven index the scope field is never read. [NewNodeIndex] and
// [NewEdgeIndex] exist for a caller that does register an Index as a
// [index.Subscriber]; there is no such caller today.
//
// A caller that becomes one should know that the two scopes are not equally
// well served. [index.OpAddEdgeLabel] is constructed and delivered in
// production, but [index.OpRemoveEdgeLabel] is constructed nowhere, so a
// registered ScopeEdge index would take every edge-label addition and never a
// removal, accumulating postings for labels that no longer apply. ScopeNode has
// both halves of its event stream; ScopeEdge, today, has one.
//
// The scoped surface, the range operations and the serialized form are
// exercised by the `label-index-scoped` simulator scenario
// (internal/sim/label_index_scoped.go).
package label

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// Scope tags whether the index observes node-label or edge-label
// changes when registered with [index.Manager]. The two scopes share
// a common bitmap shape so the on-disk format is identical.
//
// Scope is an immutable value type and is safe for concurrent use.
type Scope uint8

// Scope values for [NewNodeIndex] / [NewEdgeIndex].
const (
	// ScopeNode listens for [index.OpAddNodeLabel] / [index.OpRemoveNodeLabel]
	// when the index is registered with a [index.Manager]. It is the
	// default; callers building an unregistered index can ignore the
	// scope entirely.
	ScopeNode Scope = iota + 1
	// ScopeEdge listens for [index.OpAddEdgeLabel] / [index.OpRemoveEdgeLabel].
	// Edge bitmaps are keyed by the source NodeID, mirroring the LPG
	// convention exposed by [lpg.Graph.EdgeIndex].
	ScopeEdge
)

// cacheLine is the granularity [entry] is padded to. 128 bytes covers both the
// 64-byte lines of x86-64 and the 128-byte lines of Apple silicon, so a padded
// entry never shares a line with the neighbouring entry on either. Go allocates
// a 128-byte object from the 128-byte size class, whose spans are page-aligned,
// so the padding actually buys the separation it is written for.
const cacheLine = 128

// entryPad pads [entry] out to exactly cacheLine bytes. Its width is asserted
// against unsafe.Sizeof in TestEntryIsCacheLineSized, which fails the build's
// test gate if sync.RWMutex or [index.NodeSet] ever changes size — the padding
// is a measured performance property, not a decoration, so it is checked rather
// than assumed.
const entryPad = 80

// entry is one label's node set behind its own lock. Splitting the lock per
// label is the whole point of the geometry: a writer touching label A does not
// serialise against a writer touching label B, where the single index-wide
// mutex this replaced serialised every caller of every label (rmp #2685).
//
// # Concurrency contract
//
// Every field is guarded by mu. set is read under RLock and mutated under Lock.
// dead is written only by [Index.reap] and [Index.Deserialize], both under Lock,
// and read by [Index.mutate] under Lock.
type entry struct {
	mu sync.RWMutex
	// dead marks an entry that has been detached from the spine — reaped
	// because it became empty, or displaced wholesale by [Index.Deserialize].
	// It is set under mu BEFORE the entry leaves the spine, which is what lets
	// a mutator holding mu learn that its entry is stale WITHOUT reading the
	// spine.
	//
	// That is not a convenience, it is the deadlock fix. The obvious
	// alternative — hold the entry lock and re-read the spine to confirm the
	// entry is still the published one — acquires the two locks in the order
	// entry-then-spine, while reap acquires them spine-then-entry. That ABBA
	// inversion was implemented, measured, and it DEADLOCKED: a pending writer
	// on the spine mutex parks the reader half of the identity check for ever
	// while the reaper waits on the entry lock the checker holds. Eight clean
	// throughput sweeps missed it because their workload only ever created
	// labels, so reap never ran, and the race detector does not detect
	// deadlocks. See [Index] for the lock order this replaced it with.
	dead bool
	set  index.NodeSet
	_    [entryPad]byte
}

// Index maps label identifiers (uint32) to the set of NodeIDs that
// carry them. Different LabelID namespaces (vertices, edges) should
// use distinct Index instances.
//
// Each label's node set is held as an [index.NodeSet]: a sparse label
// carried by one or a handful of nodes stays in the inline small-set
// tier (no per-label roaring overhead), while a dense label — one built
// via [Index.AddRange] over a contiguous NodeID band, or grown past the
// small-set threshold — is a [roaring64.Bitmap] with its run-container
// optimality intact. Promotion to the bitmap tier is one-way, so a dense
// label can never be mis-tiered as a small set (sprint 206, #1585).
//
// # Concurrency
//
// Index is safe for concurrent use by any number of goroutines, for every
// exported operation, with no external synchronisation.
//
// There are two lock levels. The SPINE lock (Index.mu) guards the label→entry
// map's structure: it is taken shared to look a label up and exclusively to
// create or drop one. Each label's [entry] then carries its OWN lock guarding
// that label's node set. So two writers touching different labels never
// contend, and a reader of one label never blocks a writer of another. Before
// rmp #2685 a single index-wide RWMutex guarded everything, and it held 98.66%
// of all mutex delay on a mixed read/write workload at 8 goroutines.
//
// # Lock order — SPINE before ENTRY, never the reverse
//
// A goroutine may acquire Index.mu and then an entry.mu. It must NEVER acquire
// Index.mu while holding any entry.mu. Every path in this file obeys it:
//
//   - lookup, and therefore every read and every mutation of an existing
//     label, releases Index.mu before touching entry.mu at all.
//   - mutate's creation path, reap, and Deserialize take Index.mu first and
//     entry.mu second.
//   - mutate detects a stale entry through the entry's own dead flag rather
//     than by re-reading the spine, so it never needs the spine while holding
//     an entry lock. See [entry.dead] for the deadlock this avoids.
//
// Two entry locks are held at once in exactly one place,
// [Index.IntersectCardinality], and it acquires them in ASCENDING LABEL ID
// order. An entry is reachable under exactly one label id for its whole life,
// so label id is a total order over the entries and the wait-for graph cannot
// contain a cycle.
//
// # What the per-label locks cost: multi-label reads are no longer one image
//
// [Index.Intersect], [Index.Union], [Index.IntersectCardinality] and
// [Index.Serialize] sample each label under that label's own lock, so their
// answer is assembled from per-label images taken at slightly different
// instants rather than from one image of the whole index. A single index-wide
// image is not obtainable from a design whose writers do not take an
// index-wide lock; that is precisely the trade this geometry makes.
//
// This is a property of the raw index only. It was never a transactional
// guarantee: even under the old index-wide lock, Add(L1, n) and Add(L2, n) were
// two separate critical sections, so a concurrent Intersect(L1, L2) could
// already land between the two halves of one logical write. Snapshot-correct
// answers come from the MVCC layer above — graph/lpg's LabelBitmapAsOf and
// LabelsBitmapAsOf re-check every suspect node against the versioned label bag
// and existence record — not from the atomicity of a single index read.
type Index struct {
	mu    sync.RWMutex
	spine map[uint32]*entry
	scope Scope
}

// newIndex returns an empty index in the given scope.
func newIndex(sc Scope) *Index {
	return &Index{spine: make(map[uint32]*entry), scope: sc}
}

// NewIndex returns an empty index in [ScopeNode] — equivalent to
// [NewNodeIndex]. Existing callers that pre-date the scope field
// keep this constructor as the default.
//
// The returned Index is safe for concurrent use.
func NewIndex() *Index { return NewNodeIndex() }

// NewNodeIndex returns an empty index that listens for node-label
// changes when registered with a [index.Manager].
//
// The returned Index is safe for concurrent use.
func NewNodeIndex() *Index { return newIndex(ScopeNode) }

// NewEdgeIndex returns an empty index that listens for edge-label
// changes when registered with a [index.Manager].
//
// It has no caller in this module, and the package documentation records why,
// together with the caveat that [index.OpRemoveEdgeLabel] is not currently
// emitted anywhere in production.
//
// The returned Index is safe for concurrent use.
func NewEdgeIndex() *Index { return newIndex(ScopeEdge) }

// Scope reports which label-event kind the index observes via
// [Index.Apply]. It has no effect on an index that is driven by direct Add and
// Remove calls rather than registered with a [index.Manager], which is every
// index in this module; see the package documentation.
//
// The scope is fixed at construction, so Scope is safe for concurrent use and
// takes no lock.
func (i *Index) Scope() Scope { return i.scope }

// lookup returns label's entry under the SPINE read lock, which it releases
// before returning. The caller therefore holds no lock on return and is free to
// acquire the entry's own lock, which is what keeps the lock order one-way.
//
// The returned entry may have been reaped by the time the caller locks it;
// callers that mutate detect that through [entry.dead], and callers that only
// read need not care, because a reaped entry is empty and stays empty.
func (i *Index) lookup(label uint32) (*entry, bool) {
	i.mu.RLock()
	e, ok := i.spine[label]
	i.mu.RUnlock()
	return e, ok
}

// mutate runs fn under label's entry write lock, creating the entry first when
// create is set. It reports whether fn ran: false means the label had no entry
// and create was false.
//
// The retry loop exists because an entry can be reaped between the spine lookup
// and the entry lock. The loop terminates: with create false a reaped entry is
// gone from the spine, so the next lookup misses and returns false; with create
// true the next iteration creates a fresh entry under the spine write lock,
// which no concurrent reaper can drop before the creator has published and
// filled it (reap needs the entry lock the creator still holds, and refuses a
// non-empty set).
func (i *Index) mutate(label uint32, create bool, fn func(*entry)) bool {
	for {
		e, ok := i.lookup(label)
		if !ok {
			if !create {
				return false
			}
			// Lock order: SPINE first. The entry lock is taken while the spine
			// lock is held, and the spine lock is dropped before fn runs, so
			// fn never executes under the spine lock.
			i.mu.Lock()
			if e, ok = i.spine[label]; !ok {
				e = &entry{}
				e.mu.Lock()
				i.spine[label] = e
				i.mu.Unlock()
				fn(e)
				e.mu.Unlock()
				return true
			}
			i.mu.Unlock()
		}
		e.mu.Lock()
		if !e.dead {
			fn(e)
			e.mu.Unlock()
			return true
		}
		// Reaped or displaced between the lookup and the lock. Drop the entry
		// lock and start again from the spine — never read the spine here.
		e.mu.Unlock()
	}
}

// reap drops label from the spine when its set is empty, so the map does not
// accumulate entries for labels that no longer carry anything. It is a no-op
// when the label is absent or its set has been repopulated since the caller
// observed it empty.
//
// Lock order: SPINE then ENTRY. The dead flag is published under the entry lock
// BEFORE the entry leaves the spine, so a mutator that already holds this entry
// lock will observe dead and retry rather than writing to a detached entry.
func (i *Index) reap(label uint32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.spine[label]
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set.IsEmpty() {
		// Repopulated between the mutation that emptied it and this call.
		return
	}
	e.dead = true
	delete(i.spine, label)
}

// read runs fn under label's entry READ lock, and reports whether the label had
// an entry at all. fn must not mutate the entry and must not acquire the spine
// lock.
func (i *Index) read(label uint32, fn func(*entry)) bool {
	e, ok := i.lookup(label)
	if !ok {
		return false
	}
	e.mu.RLock()
	fn(e)
	e.mu.RUnlock()
	return true
}

// Add records that node carries label.
//
// Safe for concurrent use. It contends only with other operations on the SAME
// label, plus the brief shared spine lookup.
func (i *Index) Add(label uint32, node graph.NodeID) {
	i.mutate(label, true, func(e *entry) { e.set.Add(uint64(node)) })
}

// Remove records that node no longer carries label. No-op if absent.
// A label whose last member is removed loses its entry entirely.
//
// Safe for concurrent use.
func (i *Index) Remove(label uint32, node graph.NodeID) {
	nowEmpty := false
	i.mutate(label, false, func(e *entry) { nowEmpty = e.set.Remove(uint64(node)) })
	if nowEmpty {
		i.reap(label)
	}
}

// AddRange records that all nodes in [fromNode, toNode] (inclusive) carry
// label. It uses [roaring64.Bitmap.AddRange] which represents dense ranges in
// O(1) space, making bulk ingestion of contiguous NodeID bands efficient.
// An interval naming no ids leaves no entry behind, mirroring RemoveRange.
//
// Safe for concurrent use.
func (i *Index) AddRange(label uint32, fromNode, toNode graph.NodeID) {
	// AddRange promotes the label's NodeSet to (or keeps it on) the roaring
	// bitmap tier and uses run-container AddRange, so a contiguous band is
	// stored in O(1) space — the dense-label fast path. Promotion is
	// one-way, so a dense label stays optimal.
	nowEmpty := false
	i.mutate(label, true, func(e *entry) {
		e.set.AddRange(uint64(fromNode), uint64(toNode))
		nowEmpty = e.set.IsEmpty()
	})
	if nowEmpty {
		// An inverted or empty interval names no ids, so there is nothing to
		// record. Keeping the entry would mint a permanent one that Serialize
		// then writes out — 20 bytes and one labelCount apiece — while Count,
		// Scan and Has all report the label as carrying nothing (#2608). This
		// mirrors RemoveRange's reap in the opposite direction. The branch is
		// only reachable when the label had no entry: AddRange cannot empty a
		// set that already held ids.
		i.reap(label)
	}
}

// RemoveRange records that all nodes in [fromNode, toNode] (inclusive) no
// longer carry label. Emptied labels lose their entry so the map does not grow
// unboundedly after bulk-remove operations.
//
// Safe for concurrent use.
func (i *Index) RemoveRange(label uint32, fromNode, toNode graph.NodeID) {
	nowEmpty := false
	i.mutate(label, false, func(e *entry) {
		nowEmpty = e.set.RemoveRange(uint64(fromNode), uint64(toNode))
	})
	if nowEmpty {
		i.reap(label)
	}
}

// Scan returns the sorted slice of NodeIDs that carry label.
// Returns nil when label has no entries.
//
// Safe for concurrent use. The result is a fresh slice the caller owns.
func (i *Index) Scan(label uint32) []graph.NodeID {
	var raw []uint64
	if !i.read(label, func(e *entry) { raw = e.set.ToArray() }) {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	out := make([]graph.NodeID, len(raw))
	for j, v := range raw {
		out[j] = graph.NodeID(v)
	}
	return out
}

// Count returns the number of NodeIDs that carry label.
//
// Safe for concurrent use.
func (i *Index) Count(label uint32) uint64 {
	var n uint64
	i.read(label, func(e *entry) { n = e.set.Cardinality() })
	return n
}

// Has reports whether node carries label.
//
// Safe for concurrent use.
func (i *Index) Has(label uint32, node graph.NodeID) bool {
	var got bool
	i.read(label, func(e *entry) { got = e.set.Contains(uint64(node)) })
	return got
}

// IntersectCardinality returns the EXACT number of NodeIDs carrying every
// supplied label, without materialising the intersection and — in the common case
// — without allocating at all.
//
// It exists because the size of an intersection is a planner DECISION input, and
// paying for the answer defeats the purpose of asking. [Intersect] must clone the
// first label's live bitmap to hand the caller an owned result; a planner that
// only wants the count would pay that clone for nothing. Measured: gating a
// multi-label plan through Intersect cost +85.8% B/op on a query the gate then
// DECLINED, because two bitmaps were materialised purely to be counted.
//
// roaring64.AndCardinality walks the two container arrays by key with skips and
// accumulates per-container intersection counts, touching no allocation. For the
// pairwise case this therefore runs directly against the LIVE bitmaps under the
// two labels' entry read locks. Three or more labels have no k-way cardinality
// primitive, so the pairwise count over the first two is returned; that is an
// UPPER bound on the k-way result (|L₁ ∩ … ∩ L_k| ≤ |L₁ ∩ L₂|), which is what a
// conservative gate needs — pass the two smallest labels to make it tight.
//
// A label absent from the index makes the intersection empty, so the result is 0.
// Fewer than two labels reports (0, false): there is no intersection to size, and
// the caller must not read the count as authoritative.
//
// Safe for concurrent use. This is the only operation in the package that holds
// two entry locks at once; it takes them in ascending LABEL ID order, which
// [Index] explains is a total order over entries and therefore deadlock-free.
// The two labels are sampled under their own locks rather than under one
// index-wide lock, so the answer is not a single consistent image of the whole
// index; [Index] documents why that was never a transactional guarantee.
func (i *Index) IntersectCardinality(labels ...uint32) (uint64, bool) {
	if len(labels) < 2 {
		return 0, false
	}
	la, lb := labels[0], labels[1]
	ea, ok := i.lookup(la)
	if !ok {
		return 0, true
	}
	eb, ok := i.lookup(lb)
	if !ok {
		return 0, true
	}
	// Acquire in ascending label id order so two callers naming the same pair
	// in opposite order cannot build a cycle. Equal labels resolve to the same
	// entry, which must be locked exactly once — sync.RWMutex is not reentrant,
	// but a second RLock on the same mutex from one goroutine is also what
	// deadlocks against a queued writer, so the identity check is load-bearing.
	first, second := ea, eb
	if lb < la {
		first, second = eb, ea
	}
	first.mu.RLock()
	if second != first {
		second.mu.RLock()
	}
	// Bitmap() returns the LIVE bitmap when the set is already in the bitmap state
	// (shared == true). Neither is mutated here — AndCardinality is read-only — so
	// the clone Intersect needs is not needed, which is the whole point.
	a, _ := ea.set.Bitmap()
	b, _ := eb.set.Bitmap()
	n := a.AndCardinality(b)
	if second != first {
		second.mu.RUnlock()
	}
	first.mu.RUnlock()
	return n, true
}

// Intersect returns a fresh Roaring bitmap containing the NodeIDs
// that carry every supplied label. Calling with no labels returns
// the empty bitmap.
//
// Safe for concurrent use. Each label is sampled under its own entry read lock,
// one at a time; see [Index] for what that means for the consistency of a
// multi-label answer.
func (i *Index) Intersect(labels ...uint32) *roaring64.Bitmap {
	if len(labels) == 0 {
		return roaring64.New()
	}
	e, ok := i.lookup(labels[0])
	if !ok {
		return roaring64.New()
	}
	// Materialise a fresh, caller-owned bitmap from the first label's set
	// (clone when it aliases the live bitmap) and intersect the rest into
	// it. A non-shared bitmap was just built from the inline tier's ids and is
	// already caller-owned, so it needs no clone.
	var result *roaring64.Bitmap
	e.mu.RLock()
	bm, shared := e.set.Bitmap()
	if shared {
		result = bm.Clone()
	} else {
		result = bm
	}
	e.mu.RUnlock()
	for _, l := range labels[1:] {
		o, ok := i.lookup(l)
		if !ok {
			return roaring64.New()
		}
		o.mu.RLock()
		other, _ := o.set.Bitmap()
		result.And(other)
		o.mu.RUnlock()
		if result.IsEmpty() {
			return result
		}
	}
	return result
}

// Union returns a fresh Roaring bitmap containing the NodeIDs that
// carry any of the supplied labels.
//
// Safe for concurrent use. Each label is sampled under its own entry read lock,
// one at a time; see [Index] for what that means for the consistency of a
// multi-label answer.
func (i *Index) Union(labels ...uint32) *roaring64.Bitmap {
	result := roaring64.New()
	if len(labels) == 0 {
		return result
	}
	for _, l := range labels {
		if e, ok := i.lookup(l); ok {
			e.mu.RLock()
			e.set.OrInto(result)
			e.mu.RUnlock()
		}
	}
	return result
}

// Kind returns "label" — satisfies [index.Subscriber].
//
// Safe for concurrent use; it takes no lock and reads no state.
func (*Index) Kind() string { return "label" }

// Apply dispatches the change to the underlying bitmaps when the
// change kind matches the index's [Scope]. Other ops are ignored
// (the manager fans every change to every subscriber; per-subscriber
// filtering is the subscriber's responsibility).
//
// Safe for concurrent use; it delegates to Add and Remove.
func (i *Index) Apply(c index.Change) {
	switch c.Op {
	case index.OpAddNodeLabel:
		if i.scope == ScopeNode {
			i.Add(c.Label, c.Node)
		}
	case index.OpRemoveNodeLabel:
		if i.scope == ScopeNode {
			i.Remove(c.Label, c.Node)
		}
	case index.OpAddEdgeLabel:
		if i.scope == ScopeEdge {
			i.Add(c.Label, c.Node)
		}
	case index.OpRemoveEdgeLabel:
		if i.scope == ScopeEdge {
			i.Remove(c.Label, c.Node)
		}
	}
}

// labelMagic is the four-byte magic at the head of a serialised
// label index ('SLBI' little-endian — 0x49424C53).
const labelMagic uint32 = 0x49424C53

// labelFormatVersion is the on-disk format version of a serialised
// label index.
const labelFormatVersion uint32 = 1

// labelCapHintMax caps the eager map size hint in Deserialize so a hostile
// label count cannot drive a large pre-allocation before any entry is read.
// It mirrors the safe sibling ceiling used by store/snapshot/tombstones.bin
// and constraints.bin (1<<20). A legitimately large index is unaffected: the
// map grows past the hint as entries are inserted.
const labelCapHintMax = 1 << 20

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Serialize writes the index's per-label bitmaps to w in the format
// documented in docs/persistence.md. The on-disk layout is:
//
//	uint32 magic ('SLBI')
//	uint32 formatVersion
//	uint32 labelCount
//	repeat labelCount times:
//	  uint32 labelID
//	  uint64 bitmapLen
//	  [bitmapLen]byte bitmap (Roaring native binary format)
//	uint32 crc32c (covers every byte above, little-endian)
//
// Safe for concurrent use. Serialize holds the SPINE read lock for the whole
// emission, which is required for the format itself: labelCount is written up
// front and must match the number of entries that follow, so no label may be
// created or reaped mid-emission. Each label's bytes are then read under that
// label's own entry read lock. A concurrent writer touching an EXISTING label
// can therefore land between two entries, so the image is per-label consistent
// rather than index-wide consistent; see [Index]. Callers needing a whole-index
// point-in-time image must quiesce writers themselves.
//
// The returned error wraps the underlying I/O failure verbatim; the
// caller treats short writes the same as any other I/O error.
func (i *Index) Serialize(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, labelMagic); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, labelFormatVersion); err != nil {
		return err
	}

	// Lock order: SPINE first, each ENTRY second and one at a time.
	i.mu.RLock()
	defer i.mu.RUnlock()

	if uint64(len(i.spine)) > uint64(^uint32(0)) {
		return fmt.Errorf("label: too many labels to serialize: %d", len(i.spine))
	}
	if err := binary.Write(tee, binary.LittleEndian, uint32(len(i.spine))); err != nil {
		return err
	}
	// Iterate in ascending labelID order so the on-disk form is
	// deterministic for a given in-memory state (helps fixture diffs
	// and reproducibility).
	keys := make([]uint32, 0, len(i.spine))
	for k := range i.spine {
		keys = append(keys, k)
	}
	sortUint32(keys)

	var scratch bytes.Buffer
	for _, k := range keys {
		// Materialise a roaring bitmap from the set's logical contents and
		// write its native binary form. roaring64.WriteTo is deterministic for
		// a given in-memory state, so a bitmap built here via AddMany of the
		// sorted ids is BYTE-IDENTICAL to one that held the same ids all along
		// IN THE SAME CONTAINER ENCODING — the inline small-set tier produces
		// exactly the bytes the pre-refactor per-label *roaring64.Bitmap
		// produced, keeping the on-disk format unchanged with zero migration
		// (storage-engine-auditor, #1585). A dense (AddRange) label is already
		// a bitmap, so no materialisation cost is paid.
		//
		// The encoding itself is NOT determined by the contents: AddRange
		// builds a run container where the same ids added one at a time build
		// an array one. CanonicalBitmap normalises that for sets of at most
		// smallSetMax ids — the band the reader down-converts, and so the only
		// band where a Serialize/Deserialize cycle could change the bytes
		// (#2609). It is bounded because normalising a large set means cloning
		// it, for no change in the image; see its godoc for the measurement.
		e := i.spine[k]
		e.mu.RLock()
		bm, _ := e.set.CanonicalBitmap()
		scratch.Reset()
		size := bm.GetSerializedSizeInBytes()
		scratch.Grow(int(size))
		n, werr := bm.WriteTo(&scratch)
		e.mu.RUnlock()
		if werr != nil {
			return werr
		}
		if err := binary.Write(tee, binary.LittleEndian, k); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, uint64(n)); err != nil {
			return err
		}
		if _, err := tee.Write(scratch.Bytes()); err != nil {
			return err
		}
	}

	// CRC trailer is written to the underlying buffered writer only;
	// it must NOT feed back into the hasher.
	sum := hasher.Sum32()
	if err := binary.Write(bw, binary.LittleEndian, sum); err != nil {
		return err
	}
	return bw.Flush()
}

// Deserialize replaces the receiver's state with the contents of r.
// On any structural problem, truncated payload, or CRC mismatch the
// function returns a wrapped [index.ErrIndexCorrupted] and the
// receiver is restored to the pre-call state.
//
// The implementation reads the whole payload into a buffer, validates
// the trailing CRC32C against the prefix, then re-parses the prefix
// to populate the bitmaps. This costs one extra pass over the data
// but keeps the corruption-detection contract simple and lets the
// reader reject malformed inputs before any state mutation.
//
// Safe for concurrent use. The whole spine is swapped under the SPINE write
// lock, and every displaced entry is marked dead under its own lock first
// (lock order SPINE then ENTRY, one entry at a time), so a mutation that is
// in flight against a displaced entry retries against the new spine instead of
// writing into a detached one.
func (i *Index) Deserialize(r io.Reader) error {
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
	if magic != labelMagic {
		return fmt.Errorf("%w: bad magic %#x", index.ErrIndexCorrupted, magic)
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("%w: version: %w", index.ErrIndexCorrupted, err)
	}
	if version != labelFormatVersion {
		return fmt.Errorf("%w: unsupported format version %d",
			index.ErrIndexCorrupted, version)
	}
	var count uint32
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		return fmt.Errorf("%w: count: %w", index.ErrIndexCorrupted, err)
	}

	hint := int(count)
	if hint > labelCapHintMax {
		hint = labelCapHintMax
	}
	fresh := make(map[uint32]*entry, hint)
	for k := uint32(0); k < count; k++ {
		var labelID uint32
		if err := binary.Read(br, binary.LittleEndian, &labelID); err != nil {
			return fmt.Errorf("%w: labelID: %w", index.ErrIndexCorrupted, err)
		}
		var bmLen uint64
		if err := binary.Read(br, binary.LittleEndian, &bmLen); err != nil {
			return fmt.Errorf("%w: bitmapLen: %w", index.ErrIndexCorrupted, err)
		}
		if bmLen > uint64(len(body)) {
			return fmt.Errorf("%w: implausible bitmap length %d",
				index.ErrIndexCorrupted, bmLen)
		}
		buf := make([]byte, bmLen)
		if _, err := io.ReadFull(br, buf); err != nil {
			return fmt.Errorf("%w: bitmap bytes: %w", index.ErrIndexCorrupted, err)
		}
		bm := roaring64.New()
		if _, err := bm.ReadFrom(bytes.NewReader(buf)); err != nil {
			return fmt.Errorf("%w: bitmap parse: %w", index.ErrIndexCorrupted, err)
		}
		// Down-convert a sparse label to the inline small-set tier so a
		// reload recovers the memory win (a snapshot taken before this
		// refactor carries roaring images for singleton labels). A dense
		// label stays on the bitmap tier. This is purely an in-memory
		// representation choice; the bytes already read were validated above
		// and are not affected.
		e := &entry{}
		e.set = index.NodeSetFromBitmap(bm)
		fresh[labelID] = e
	}

	i.mu.Lock()
	for _, e := range i.spine {
		// Lock order: SPINE (held) then ENTRY, one at a time. Marking the
		// displaced entry dead makes an in-flight mutator retry against the new
		// spine rather than write into an entry nothing will ever read again.
		e.mu.Lock()
		e.dead = true
		e.mu.Unlock()
	}
	i.spine = fresh
	i.mu.Unlock()
	return nil
}

// sortUint32 sorts s in place in ascending order. Local to keep the
// import surface small (sort.Slice would force a closure allocation
// for what is a tiny in-place sort on a value type).
func sortUint32(s []uint32) {
	// Insertion sort is fine — labels per index are usually small
	// (dozens, at most thousands), well below the slice sort cutoff.
	for i := 1; i < len(s); i++ {
		x := s[i]
		j := i - 1
		for j >= 0 && s[j] > x {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = x
	}
}

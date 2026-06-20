// Package adjlist provides a mutable, sharded adjacency-list backend
// for the gograph module.
//
// AdjList is the canonical builder used to assemble a graph
// incrementally before it is frozen into an immutable CSR view for
// analytics. It supports directed and undirected graphs (mirrored
// insertion), parallel edges (multigraph mode), and self-loops.
//
// # Storage and concurrency
//
// Storage is split into 256 independently locked shards aligned with
// the [graph.Mapper]'s sharding (the low 8 bits of every NodeID
// identify the shard). Within a shard, adjacency entries are indexed
// by the intra-shard component of the NodeID — a direct slice access
// with no map lookup.
//
// Reads (HasEdge, Neighbours, Order, Size) are lock-free: a reader
// performs a single atomic load on the shard's slot slice, a single
// atomic load on the slot itself, and operates on the immutable
// snapshot of neighbours/weights stored in the resulting entry.
//
// Writes (AddEdge, RemoveEdge) take the shard mutex, copy the entry's
// slices with the modification applied, and publish the new entry
// pointer via [sync/atomic.StorePointer]. Compact likewise takes the
// shard mutex and republishes each slack-bearing entry with its backing
// arrays right-sized to exact length. Concurrent readers always observe
// a consistent snapshot — either the entry before the write or the entry
// after it.
//
// The yield callback of [AdjList.Neighbours] iterates over slices
// owned by a snapshotted entry and may safely call any other AdjList
// operation; no locks are held during yield.
package adjlist

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ErrShardFull is returned by [AdjList.AddNode] and [AdjList.AddEdge]
// when the shard responsible for a new NodeID would have to grow
// beyond [Config.MaxShardCapacity]. Callers inspect this error with
// [errors.Is]; the AdjList state is unchanged when this error is
// returned (no node interned, no edge published, size counter not
// advanced).
var ErrShardFull = errors.New("adjlist: shard capacity exhausted")

// shardCount is the number of independently locked stripes used by
// AdjList. It mirrors [graph.NodeID]'s shard encoding, so the low 8
// bits of any NodeID identify its shard directly.
const (
	shardCount = 256
	shardMask  = shardCount - 1
	shardBits  = 8

	// initialShardCap is the initial capacity of a shard's slot slice.
	// Shards grow as new NodeIDs land in them; the initial capacity
	// avoids the very first allocation on empty graphs.
	initialShardCap = 16
)

// Config selects the variant of graph implemented by an [AdjList].
//
// The zero value (Directed=false, Multigraph=false) builds a simple
// undirected graph, which is rarely what users want; prefer
// constructing a Config explicitly.
type Config struct {
	// Directed, when true, treats AddEdge as a directed insertion. When
	// false, AddEdge also inserts the reverse edge (mirrored insertion).
	Directed bool

	// Multigraph, when true, allows parallel edges between the same
	// pair of endpoints; AddEdge always appends. When false (simple
	// graph), repeated AddEdge calls on the same endpoint pair are
	// idempotent — the existing edge stays and the new weight is
	// ignored.
	Multigraph bool

	// MaxShardCapacity, when > 0, caps the number of node-slots that
	// any individual shard may grow to. AddNode (or AddEdge that
	// would create a new node or store an outgoing entry in the
	// responsible shard) returns [ErrShardFull] when growth past the
	// cap would otherwise occur; the AdjList state is left unchanged.
	// The default (0) places no upper bound — a shard doubles its
	// slot slice indefinitely.
	MaxShardCapacity int
}

// AdjList is a mutable adjacency-list graph generic over the user node
// type N and the edge weight type W. Construct one with [New].
//
// Concurrency: AdjList is safe for any number of concurrent readers
// (Neighbours, LoadEntry, HasEdge, Order, Size) AND concurrent
// writers (AddEdge, AddNode, RemoveEdge). The 256-way shard layout
// serialises only writers landing in the same shard; readers never
// take a mutex and observe a consistent snapshot via atomic.Pointer.
//
// Bounded growth: when [Config.MaxShardCapacity] is set, AddEdge and
// AddNode return [ErrShardFull] instead of growing the affected shard
// past the cap. Callers must propagate the error and stop offering
// new work to the saturated shard; the AdjList state is unchanged
// when ErrShardFull is returned.
//
// NodeID stability: NodeIDs assigned by the Mapper are monotonically
// increasing within each shard and are never reused. Removing an edge
// does not remove the endpoint nodes from the Mapper; their NodeIDs
// remain valid for the lifetime of the AdjList. Code that caches
// NodeIDs (e.g., an external CSR snapshot) may rely on them remaining
// stable as long as the originating AdjList is live.
type AdjList[N comparable, W any] struct {
	mapper *graph.Mapper[N]
	cfg    Config

	shards [shardCount]adjShard[W]

	size atomic.Uint64
}

// adjShard is one independently locked stripe of the adjacency list.
//
// slots stores, for each intra-shard index, a pointer to an immutable
// [adjEntry] snapshot. Writers serialise on mu and publish new
// snapshots via [sync/atomic.StorePointer]; readers atomically load
// the slot without taking mu.
type adjShard[W any] struct {
	mu       sync.Mutex
	slotsRef atomic.Pointer[shardSlots]
}

// shardSlots holds the slot-pointer slice for a shard. It is replaced
// atomically when the shard grows.
type shardSlots struct {
	slots []unsafe.Pointer // each element holds *adjEntry[W]
}

// adjEntry is an immutable snapshot of a node's outgoing adjacency.
// Once an entry is published to a shard slot via atomic.StorePointer
// its slices are never mutated; mutations produce a new entry. This
// makes the read path completely lock-free.
//
// Adjacency is kept unsorted; HasEdge does a linear scan. For the
// typical low average degree of property graphs (4-16) the branch-
// prediction-friendly linear scan beats sorted binary search.
//
// handles is a parallel column carrying a stable per-edge-slot handle
// (uint64) for each neighbour. It is populated only when a caller
// supplies a handle via [AdjList.AddEdgeH]; the plain [AdjList.AddEdge]
// path leaves handles nil so simple graphs that never need per-instance
// edge identity pay no extra memory. When non-nil, handles is the same
// length as neighbours and is carried verbatim across compaction in
// [AdjList.removeOneEdge] — a surviving slot keeps its ORIGINAL handle
// (handles are never renumbered or reused).
//
// labels is a second optional parallel column carrying one OPAQUE 4-byte
// value per neighbour slot. adjlist never interprets it; the higher layer
// (lpg) co-locates a single encoded relationship-type id here instead of
// re-storing the (src,dst) pair in a separate map, which is the dominant
// resident-memory cost on label-heavy graphs. Like handles it is nil until
// a caller first sets a label via [AdjList.SetEdgeLabelSlot]; when non-nil
// it is the same length as neighbours and is carried in lockstep across
// growth (upsertEdgeLocked), compaction (compactEntry), and bulk removal
// (RemoveAllEdgesFrom). The value 0 is reserved by convention for "no
// label on this slot"; lpg owns the +1/-1 bias so that a real label id 0
// maps to the stored value 1.
//
// aux is a third optional parallel column, an OPAQUE immutable [AuxColumn]
// block the higher layer (lpg) uses to co-locate de-boxed typed edge-property
// values aligned 1:1 to neighbours, instead of re-storing the (src,dst) pair
// in a separate property map. Unlike labels (a flat uint32 adjlist partially
// interprets via the 0 sentinel) the property block is rich (several typed
// columns plus a validity plane), so adjlist treats it as wholly opaque: it
// only asks the block to transform itself across the two structural slot
// events via [AuxColumn.GrowSlot] (append one absent slot at position oldLen)
// and [AuxColumn.CompactSlot] (excise slot idx). It is nil until lpg first
// sets a property via [AdjList.UpdateEntryAux]; when non-nil it is carried in
// lockstep across growth (upsertEdgeLocked), compaction (compactEntry), and
// trimming (trimEntry, verbatim — the block has no slack notion).
type adjEntry[W any] struct {
	neighbours []graph.NodeID
	weights    []W
	handles    []uint64  // nil unless AddEdgeH supplied a handle; parallel to neighbours when set
	labels     []uint32  // nil unless SetEdgeLabelSlot set one; parallel to neighbours when set; 0 == no label
	aux        AuxColumn // nil unless UpdateEntryAux set one; opaque, kept length-aligned by GrowSlot/CompactSlot
}

// AuxColumn is an opaque, immutable per-entry side column the higher layer
// attaches to a node's adjacency entry to carry one logical value per
// neighbour slot, aligned 1:1 to the entry's neighbours array. adjlist never
// interprets the contents; it only drives the column's lifecycle so the
// per-slot alignment is preserved across the two structural slot mutations
// adjlist performs:
//
//   - an APPEND grows the entry by one slot at position oldLen;
//   - a REMOVE excises one slot at position idx and shifts the tail down.
//
// Both lifecycle methods return a NEW immutable column (copy-on-write); the
// receiver is never mutated, so a concurrent lock-free reader holding the
// prior entry (and thus the prior column) is unaffected. An implementation
// must keep its own length equal to the entry's neighbour count at all
// observable points.
//
// Concurrency: an AuxColumn value is published as part of an immutable
// [adjEntry] via atomic.StorePointer and is read lock-free thereafter, so an
// implementation must be safe for concurrent reads once returned from a
// lifecycle method. adjlist holds the shard mutex when it calls GrowSlot /
// CompactSlot, so those calls never race each other for one entry.
type AuxColumn interface {
	// GrowSlot returns a new column of length oldLen+1 whose existing slots
	// [0,oldLen) are unchanged and whose new slot at index oldLen is ABSENT
	// (carries no value — the implementation must clear any presence/validity
	// bit for that slot, never reusing a stale value from recycled backing
	// storage). oldLen is the neighbour count of the entry BEFORE the append.
	GrowSlot(oldLen int) AuxColumn

	// CompactSlot returns a new column of length n-1 with the slot at idx
	// excised: result slots [0,idx) equal the receiver's [0,idx) and result
	// slots [idx,n-1) equal the receiver's [idx+1,n). idx is a valid index in
	// [0,n) where n is the receiver's current length.
	CompactSlot(idx int) AuxColumn

	// Compact returns a column logically equal to the receiver but with its
	// internal backing storage right-sized to the live contents, reclaiming any
	// slack an amortised-growth build path left behind, or the receiver itself
	// when it already holds no slack. It is the column analogue of
	// [AdjList.Compact]'s topology-array trimming and is invoked from the same
	// pass. An implementation whose representation is always exactly sized (no
	// over-allocation) may return the receiver unchanged. The returned column
	// must read identically to the receiver and is published the same lock-free
	// way, so it must be a fresh immutable value when it differs.
	Compact() AuxColumn
}

// New returns an empty AdjList configured by cfg.
func New[N comparable, W any](cfg Config) *AdjList[N, W] {
	return &AdjList[N, W]{
		mapper: graph.NewMapper[N](),
		cfg:    cfg,
	}
}

// Mapper returns the underlying [graph.Mapper] that translates between
// user-facing N values and compact NodeIDs.
func (a *AdjList[N, W]) Mapper() *graph.Mapper[N] {
	return a.mapper
}

// Order returns the number of distinct nodes currently referenced.
// The count is read from the underlying [graph.Mapper], which acts as
// the authoritative registry. Order is O(shardCount) and intended for
// occasional inspection rather than hot-path use. Implements
// [graph.Graph].
func (a *AdjList[N, W]) Order() uint64 { return uint64(a.mapper.Len()) }

// Size returns the number of edges currently in the graph. For an
// undirected graph each AddEdge call is counted once; the mirrored
// neighbour entry is stored but not double-counted. In multigraph
// mode every parallel edge counts. Implements [graph.Graph].
func (a *AdjList[N, W]) Size() uint64 { return a.size.Load() }

// Directed reports whether the graph is directed.
func (a *AdjList[N, W]) Directed() bool { return a.cfg.Directed }

// Multigraph reports whether parallel edges are allowed.
func (a *AdjList[N, W]) Multigraph() bool { return a.cfg.Multigraph }

// Config returns the [Config] the AdjList was constructed with. The
// configuration is fixed at [New] and never mutated thereafter, so
// Config is safe to call concurrently with any other operation and
// always returns the same value for the lifetime of the AdjList. It is
// used by the snapshot writer to persist the originating graph's
// directed/multigraph shape so recovery can reconstruct the same
// variant instead of guessing.
func (a *AdjList[N, W]) Config() Config { return a.cfg }

// AddNode inserts n if not already present. The node enters the
// adjacency list lazily on its first outgoing edge; AddNode only
// interns the value with the Mapper, which is sufficient for
// [AdjList.Order] to account for it. Implements [graph.Graph].
//
// AddNode never returns [ErrShardFull] on its own because it does not
// touch any shard's slot array; the bounded-growth contract becomes
// observable on the first AddEdge for which the responsible shard
// would have to grow past [Config.MaxShardCapacity]. The error return
// exists to satisfy the [graph.Graph] contract and to leave room for
// future implementations that reserve shard storage eagerly.
func (a *AdjList[N, W]) AddNode(n N) error { a.mapper.Intern(n); return nil }

// HasEdge reports whether an edge from src to dst is present.
// HasEdge is lock-free and allocation-free on the hot path.
// Implements [graph.Graph].
func (a *AdjList[N, W]) HasEdge(src, dst N) bool {
	srcID, ok := a.mapper.Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := a.mapper.Lookup(dst)
	if !ok {
		return false
	}
	e := loadEntry[W](&a.shards[srcID&shardMask], uint64(srcID)>>shardBits)
	if e == nil {
		return false
	}
	for _, n := range e.neighbours {
		if n == dstID {
			return true
		}
	}
	return false
}

// AddEdge inserts a directed edge from src to dst with weight w, also
// interning the endpoints if they are not yet known. When the graph
// is undirected, the mirrored edge (dst, src) is inserted as well.
// Implements [graph.Graph].
//
// AddEdge returns [ErrShardFull] when [Config.MaxShardCapacity] is
// set and the responsible shard would have to grow past the cap to
// store the new entry. In that case no edge is published; the size
// counter is not advanced. The endpoints may, however, remain
// interned in the underlying [graph.Mapper]: callers that need
// strict orphan-free behaviour should detect ErrShardFull and treat
// the graph as saturated.
func (a *AdjList[N, W]) AddEdge(src, dst N, w W) error {
	return a.addEdge(src, dst, w, edgeExtra{})
}

// AddEdgeH is [AdjList.AddEdge] with an explicit, caller-supplied stable
// edge handle. The handle is stored in the slot's parallel handle column
// (see [adjEntry.handles]) so the read path can recover per-slot edge
// identity without inferring it from CSR slot order. The handle is
// carried verbatim across compaction on [AdjList.RemoveEdge]: a surviving
// parallel slot keeps its original handle, and handles are never reused
// or renumbered.
//
// For an undirected graph the mirrored (dst, src) slot receives the SAME
// handle, so both directions of one logical edge share one identity.
//
// AddEdgeH honours the same [ErrShardFull] and all-or-nothing contract as
// [AdjList.AddEdge]. In simple-graph mode a duplicate (src, dst) is still
// a no-op and the supplied handle is ignored (the existing slot keeps its
// original handle).
func (a *AdjList[N, W]) AddEdgeH(src, dst N, w W, handle uint64) error {
	return a.addEdge(src, dst, w, edgeExtra{handle: handle, hasHandle: true})
}

// AddEdgeLabeled is [AdjList.AddEdge] with an OPAQUE 4-byte label supplied AT
// edge-insertion time. The label is written into the slot's parallel label
// column (see [adjEntry.labels]) within the SAME O(1)-amortised append fast
// path that writes the neighbour and weight — no separate copy-on-write of the
// whole column afterwards. This is the bulk-build path for a labelled graph: a
// degree-d source is assembled in O(d) amortised total, not O(d²).
//
// adjlist treats label as opaque: any uint32 is accepted, including 0 (the
// higher layer's "no label" sentinel). A label-free graph that never calls this
// method keeps the labels column nil and pays no extra memory.
//
// For an undirected graph the mirrored (dst, src) slot receives the SAME label,
// so both directions of one logical edge carry the same relationship type.
//
// AddEdgeLabeled honours the same [ErrShardFull] and all-or-nothing contract as
// [AdjList.AddEdge]. In simple-graph mode a duplicate (src, dst) is still a
// no-op and the supplied label is ignored (the existing slot keeps its label).
// Use [AdjList.SetEdgeLabelSlot] to (re)label a slot of a pre-existing edge.
func (a *AdjList[N, W]) AddEdgeLabeled(src, dst N, w W, label uint32) error {
	return a.addEdge(src, dst, w, edgeExtra{label: label, hasLabel: true})
}

// AddEdgeLabeledH fuses [AdjList.AddEdgeH] and [AdjList.AddEdgeLabeled]: it
// stores both a caller-supplied stable handle and an opaque label on the new
// slot within the same append fast path. Both optional columns are written at
// position oldLen at insertion time, so a labelled, handle-carrying edge is
// still an O(1)-amortised append.
func (a *AdjList[N, W]) AddEdgeLabeledH(src, dst N, w W, handle uint64, label uint32) error {
	return a.addEdge(src, dst, w, edgeExtra{handle: handle, hasHandle: true, label: label, hasLabel: true})
}

// edgeExtra carries the two OPTIONAL parallel-column values an edge insertion
// may stamp onto its new slot AT append time: a stable handle and an opaque
// label. Each is gated by its own "has" flag so an absent column stays nil for
// a label-free / handle-free graph. Bundling them keeps the append fast path's
// signature stable as new optional columns are added, and the struct is a small
// value type passed by copy (no heap escape).
type edgeExtra struct {
	handle    uint64
	label     uint32
	hasHandle bool
	hasLabel  bool
}

// addEdge is the shared implementation of [AdjList.AddEdge], [AdjList.AddEdgeH],
// [AdjList.AddEdgeLabeled], and [AdjList.AddEdgeLabeledH]. The optional handle
// and label in ex are stamped onto the new slot's parallel columns at append
// time; when a "has" flag is false that column is left untouched (nil for a
// fresh entry).
//
// For undirected multigraphs where src and dst land in DIFFERENT shards the
// two shard locks are acquired simultaneously in a canonical (lower-index-
// first) order before both appends are performed. This ensures the forward
// and mirror slots are assigned atomically — no concurrent parallel-edge
// insertion can interleave between the two appends — so both directions
// always reflect the same slot ordering.
func (a *AdjList[N, W]) addEdge(src, dst N, w W, ex edgeExtra) error {
	srcID := a.mapper.Intern(src)
	dstID := a.mapper.Intern(dst)

	// Directed graphs and self-loops need only the forward append.
	if a.cfg.Directed || srcID == dstID {
		inserted, err := a.upsertEdge(srcID, dstID, w, ex)
		if err != nil {
			return err
		}
		if inserted {
			a.size.Add(1)
		}
		return nil
	}

	// Undirected, non-self-loop: both directions must be appended atomically.
	srcShard := srcID & shardMask
	dstShard := dstID & shardMask

	if srcShard == dstShard {
		// Single shard covers both endpoints: one lock suffices.
		inserted, err := a.upsertEdge(srcID, dstID, w, ex)
		if err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		a.size.Add(1)
		if _, err := a.upsertEdge(dstID, srcID, w, ex); err != nil {
			// Both endpoints share a shard, so the forward append and this
			// mirror append are already serialised; just undo the forward.
			a.removeOneEdge(srcID, dstID)
			a.size.Add(^uint64(0))
			return err
		}
		return nil
	}

	// Different shards: acquire both locks in canonical (lower-index-first)
	// order to prevent deadlock, then perform both appends inside the combined
	// critical section so no concurrent goroutine can interleave between them.
	sLo := &a.shards[min(srcShard, dstShard)]
	sHi := &a.shards[max(srcShard, dstShard)]
	sLo.mu.Lock()
	sHi.mu.Lock()

	inserted, err := a.upsertEdgeLocked(srcID, dstID, w, ex)
	if err != nil {
		sHi.mu.Unlock()
		sLo.mu.Unlock()
		return err
	}
	if !inserted {
		sHi.mu.Unlock()
		sLo.mu.Unlock()
		return nil
	}
	// Forward slot appended. Now append the mirror under the same lock pair.
	if _, err := a.upsertEdgeLocked(dstID, srcID, w, ex); err != nil {
		// Undo the forward append before releasing — we still hold both locks
		// so the rollback is atomic with respect to any reader.
		a.removeOneEdgeLocked(srcID, dstID)
		sHi.mu.Unlock()
		sLo.mu.Unlock()
		return err
	}

	sHi.mu.Unlock()
	sLo.mu.Unlock()
	a.size.Add(1)
	return nil
}

// upsertEdge publishes a new adjacency snapshot for src that includes
// (dst, w). Returns (false, nil) when (in simple-graph mode) dst is
// already a neighbour, and (false, ErrShardFull) when the responsible
// shard would have to grow past [Config.MaxShardCapacity]. The new
// snapshot is constructed fresh and swapped in via atomic.StorePointer
// so concurrent readers always observe a consistent immutable
// adjacency.
func (a *AdjList[N, W]) upsertEdge(src, dst graph.NodeID, w W, ex edgeExtra) (bool, error) {
	s := &a.shards[src&shardMask]
	s.mu.Lock()
	defer s.mu.Unlock()
	return a.upsertEdgeLocked(src, dst, w, ex)
}

// growCap returns the next backing-array capacity to use when appending to a
// slice of length cur. Growth is geometric (×2) with a minimum of 4, giving
// amortised O(1) appends and O(log d) large allocations for a degree-d hub.
func growCap(cur int) int {
	if cur < 4 {
		return 4
	}
	return cur * 2
}

// upsertEdgeLocked is the lock-free body of [AdjList.upsertEdge]. The
// caller must already hold the shard mutex for src (i.e.
// a.shards[src&shardMask].mu). This variant exists so that [AdjList.addEdge]
// can acquire multiple shard locks at once — for undirected multigraph
// cross-shard pairs — and perform both appends inside the combined critical
// section.
//
// Append strategy: when the current backing array has spare capacity the new
// entry reuses the SAME backing array (new slice headers only, no allocation).
// This is safe because concurrent readers of the old entry iterate only
// [0:len], and the write to position len is sequenced before the atomic
// storeEntry (Go memory model: write before release-store; acquire-load by
// readers). When the array is full a new one is allocated with geometric
// capacity (growCap), amortising the copy cost to O(log d) large allocations
// per degree-d hub.
func (a *AdjList[N, W]) upsertEdgeLocked(src, dst graph.NodeID, w W, ex edgeExtra) (bool, error) {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		// First entry for this node: allocate with geometric initial capacity.
		c := growCap(0) // == 4
		entry := &adjEntry[W]{
			neighbours: make([]graph.NodeID, 1, c),
			weights:    make([]W, 1, c),
		}
		entry.neighbours[0] = dst
		entry.weights[0] = w
		if ex.hasHandle {
			entry.handles = make([]uint64, 1, c)
			entry.handles[0] = ex.handle
		}
		// Fused label: when a label is supplied at insertion time the column is
		// allocated here and the first (only) slot receives it directly — the
		// same O(1) write as neighbours/weights, never a separate column copy.
		if ex.hasLabel {
			entry.labels = make([]uint32, 1, c)
			entry.labels[0] = ex.label
		}
		if err := storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, entry); err != nil {
			return false, err
		}
		return true, nil
	}
	if !a.cfg.Multigraph {
		for _, n := range current.neighbours {
			if n == dst {
				return false, nil
			}
		}
	}

	oldLen := len(current.neighbours)
	newLen := oldLen + 1

	// Fast path: the current backing array has spare capacity. Reuse it by
	// writing the new element at position oldLen and publishing new slice
	// headers that expose it. No large allocation; O(1) per call.
	if cap(current.neighbours) >= newLen {
		nb := current.neighbours[:newLen]
		ws := current.weights[:newLen]
		nb[oldLen] = dst
		ws[oldLen] = w

		var hs []uint64
		if ex.hasHandle || current.handles != nil {
			if current.handles != nil && cap(current.handles) >= newLen {
				hs = current.handles[:newLen]
			} else {
				// Size capacity from oldLen, not len(current.handles): a
				// handle-less prefix leaves the handle column shorter than
				// neighbours (len 0 while still nil), and growCap of that short
				// length can be < newLen — make([]uint64, newLen, <newLen) panics.
				// growCap(oldLen) mirrors the slow path and is always >= newLen.
				// copy back-fills the leading slots; any gap up to oldLen stays
				// the 0 ("no handle") sentinel, keeping the column length-aligned
				// with neighbours.
				hs = make([]uint64, newLen, growCap(oldLen))
				copy(hs, current.handles)
			}
			hs[oldLen] = ex.handle
		}
		// Carry the optional labels column forward when it exists OR when this
		// call supplies a fused label. The new slot at oldLen receives ex.label
		// (0 when no fused label was supplied — the "no label" sentinel, exactly
		// as a label-less append would leave it). The same growCap(oldLen)
		// capacity rule and back-fill apply as for handles.
		var ls []uint32
		if ex.hasLabel || current.labels != nil {
			if current.labels != nil && cap(current.labels) >= newLen {
				ls = current.labels[:newLen]
			} else {
				ls = make([]uint32, newLen, growCap(oldLen))
				copy(ls, current.labels) // no-op when current.labels is nil
			}
			ls[oldLen] = ex.label
		}
		// Carry the opaque aux column forward when it exists. GrowSlot returns a
		// fresh column of length newLen whose new slot at oldLen is absent; the
		// higher layer is responsible for clearing any presence bit there. A
		// fresh entry never has an aux column, so a label-/property-free graph
		// pays nothing.
		ax := growAux(current.aux, oldLen)
		if err := storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{neighbours: nb, weights: ws, handles: hs, labels: ls, aux: ax}); err != nil {
			return false, err
		}
		return true, nil
	}

	// Slow path: backing array is full — allocate with geometric capacity and
	// copy existing data. This happens only O(log d) times for a degree-d hub.
	newCap := growCap(oldLen)
	newNb := make([]graph.NodeID, newLen, newCap)
	newW := make([]W, newLen, newCap)
	copy(newNb, current.neighbours)
	copy(newW, current.weights)
	newNb[oldLen] = dst
	newW[oldLen] = w
	// Carry the handle column forward only when this graph uses handles
	// — either the existing entry already has one or this call supplies
	// one. A previously handle-less entry that gains its first handle on
	// a later append back-fills the earlier slots with 0 (the reserved
	// "no handle" sentinel), so the column stays length-aligned with
	// neighbours.
	var newH []uint64
	if ex.hasHandle || current.handles != nil {
		newH = make([]uint64, newLen, newCap)
		copy(newH, current.handles) // copy is a no-op when current.handles is nil
		newH[oldLen] = ex.handle
	}
	// Carry the optional labels column forward when it exists OR a fused label
	// is supplied. The appended slot at oldLen receives ex.label (0 when none),
	// so it stays the sentinel for a label-less append.
	var newL []uint32
	if ex.hasLabel || current.labels != nil {
		newL = make([]uint32, newLen, newCap)
		copy(newL, current.labels) // no-op when current.labels is nil
		newL[oldLen] = ex.label
	}
	// Carry the opaque aux column forward when it exists; the new slot at
	// oldLen is absent (GrowSlot clears its presence bit).
	newAux := growAux(current.aux, oldLen)
	if err := storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{neighbours: newNb, weights: newW, handles: newH, labels: newL, aux: newAux}); err != nil {
		return false, err
	}
	return true, nil
}

// growAux returns the aux column for an entry that has just grown by one slot
// at position oldLen, or nil when the entry carries no aux column. It is the
// single place the append fast and slow paths consult so the "nil stays nil,
// otherwise GrowSlot" rule lives in one spot.
func growAux(cur AuxColumn, oldLen int) AuxColumn {
	if cur == nil {
		return nil
	}
	return cur.GrowSlot(oldLen)
}

// RemoveEdge removes the directed edge from src to dst if present. For
// multigraphs only one occurrence is removed per call. The endpoints
// remain in the graph. Implements [graph.Graph].
//
// For undirected multigraphs, after removing the first-match slot from the
// forward direction, the mirror is removed by handle identity (when the
// removed slot carried a non-zero handle). This ensures that — even after
// concurrent parallel adds that may have reshuffled slot positions relative
// to what they were at creation time — the same logical edge is retired from
// both directions. When no handle is present (plain AddEdge path) the mirror
// falls back to first-match behaviour, which is correct in the single-writer
// case that plain AddEdge implies.
func (a *AdjList[N, W]) RemoveEdge(src, dst N) {
	srcID, ok := a.mapper.Lookup(src)
	if !ok {
		return
	}
	dstID, ok := a.mapper.Lookup(dst)
	if !ok {
		return
	}
	removed, removedHandle := a.removeOneEdgeWithHandle(srcID, dstID)
	if !removed {
		return
	}
	a.size.Add(^uint64(0))

	if a.cfg.Directed || srcID == dstID {
		return
	}
	// Mirror removal: prefer handle-based targeting when the removed slot
	// carried a non-zero handle; fall back to first-match otherwise.
	if removedHandle != 0 {
		a.removeOneEdgeByHandle(dstID, srcID, removedHandle)
	} else {
		a.removeOneEdge(dstID, srcID)
	}
}

// RemoveAllEdgesFrom removes all edges incident from src in O(d) time for a
// degree-d hub, instead of the O(d²) cost of d sequential [AdjList.RemoveEdge]
// calls.
//
// For directed graphs the method zeroes src's adjacency slot atomically and
// decrements the edge counter by the number of removed edges. For undirected
// graphs it additionally removes the mirror entry (src from each dst's list)
// with one removeOneEdge call per neighbour; those calls are each O(degree-of-
// dst), which is O(1) for typical star topologies.
//
// Concurrent readers observe either the full pre-deletion state or the post-
// deletion state; no partial state is ever visible (the src slot is published
// atomically, and each mirror removal is a separate atomic store).
//
// RemoveAllEdgesFrom is safe for concurrent use.
func (a *AdjList[N, W]) RemoveAllEdgesFrom(src N) {
	srcID, ok := a.mapper.Lookup(src)
	if !ok {
		return
	}

	s := &a.shards[srcID&shardMask]
	intraIdx := uint64(srcID) >> shardBits

	s.mu.Lock()
	old := loadEntry[W](s, intraIdx)
	if old == nil || len(old.neighbours) == 0 {
		s.mu.Unlock()
		return
	}
	// Publish nil atomically: readers after this store see an empty adjacency
	// for src. storeEntry cannot fail here because the slot already exists.
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, nil)
	removed := len(old.neighbours)
	// Copy neighbour IDs before releasing the lock so the mirror-removal loop
	// below is not affected by concurrent writes to the shard.
	dsts := make([]graph.NodeID, removed)
	copy(dsts, old.neighbours)
	s.mu.Unlock()

	// Adjust the edge counter atomically. The two's-complement trick
	// (^uint64(removed-1)) is equivalent to -removed for unsigned arithmetic.
	a.size.Add(^uint64(removed - 1))

	if a.cfg.Directed {
		return // no mirrors to clean up for directed graphs
	}
	// Undirected: remove src from each dst's list. Self-loops are already
	// cleared by the slot zeroing above and must not be processed again.
	for _, dstID := range dsts {
		if dstID != srcID {
			a.removeOneEdge(dstID, srcID)
		}
	}
}

// removeOneEdgeWithHandle publishes a new adjacency snapshot for src that
// omits one occurrence of dst (first match). Returns (true, handle) when an
// edge was removed, where handle is the handle value stored in the removed
// slot (0 when no handle column is present).
func (a *AdjList[N, W]) removeOneEdgeWithHandle(src, dst graph.NodeID) (removed bool, handle uint64) {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		return false, 0
	}
	idx := -1
	for i, n := range current.neighbours {
		if n == dst {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, 0
	}
	var removedH uint64
	if current.handles != nil {
		removedH = current.handles[idx]
	}
	if len(current.neighbours) == 1 {
		// storeEntry cannot fail here because the slot already exists
		// in the shard's slot array; no growth is required. Publish nil
		// instead of an empty struct to avoid a small allocation on each
		// last-edge removal; loadEntry handles nil slots correctly.
		_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, nil)
		return true, removedH
	}
	newEntry := compactEntry(current, idx)
	// storeEntry cannot fail here: same slot, no growth required.
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, newEntry)
	return true, removedH
}

// removeOneEdge publishes a new adjacency snapshot for src that omits
// one occurrence of dst. Returns true when an edge was removed.
func (a *AdjList[N, W]) removeOneEdge(src, dst graph.NodeID) bool {
	removed, _ := a.removeOneEdgeWithHandle(src, dst)
	return removed
}

// removeOneEdgeLocked is the lock-free body of [AdjList.removeOneEdge].
// The caller must already hold the shard mutex for src. This variant is
// used by [AdjList.addEdge] to roll back a forward append while still
// holding both shard locks.
func (a *AdjList[N, W]) removeOneEdgeLocked(src, dst graph.NodeID) {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		return
	}
	idx := -1
	for i, n := range current.neighbours {
		if n == dst {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	if len(current.neighbours) == 1 {
		_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, nil)
		return
	}
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, compactEntry(current, idx))
}

// removeOneEdgeByHandle publishes a new adjacency snapshot for src that omits
// the slot whose handle equals targetHandle. The search scans dst-directed
// neighbours for a matching handle. Returns true when a slot was removed.
// Falls back silently when no slot with the target handle exists.
func (a *AdjList[N, W]) removeOneEdgeByHandle(src, dst graph.NodeID, targetHandle uint64) bool {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil || current.handles == nil {
		// No handle column: fall back to first-match by neighbour.
		return a.removeOneEdgeFallback(s, intraIdx, current, dst)
	}
	// Find the slot whose neighbour is dst AND whose handle matches.
	idx := -1
	for i, n := range current.neighbours {
		if n == dst && current.handles[i] == targetHandle {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	if len(current.neighbours) == 1 {
		_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, nil)
		return true
	}
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, compactEntry(current, idx))
	return true
}

// removeOneEdgeFallback is the first-match fallback used inside
// [AdjList.removeOneEdgeByHandle] when the handle column is absent.
// The caller must hold s.mu.
func (a *AdjList[N, W]) removeOneEdgeFallback(s *adjShard[W], intraIdx uint64, current *adjEntry[W], dst graph.NodeID) bool {
	if current == nil {
		return false
	}
	idx := -1
	for i, n := range current.neighbours {
		if n == dst {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	if len(current.neighbours) == 1 {
		_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, nil)
		return true
	}
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, compactEntry(current, idx))
	return true
}

// compactEntry returns a new adjEntry[W] equal to current with the slot at
// idx excised. All surviving slots keep their original handles (stable-
// identity invariant). compactEntry must only be called with a valid idx in
// [0, len(current.neighbours)).
func compactEntry[W any](current *adjEntry[W], idx int) *adjEntry[W] {
	newNb := make([]graph.NodeID, len(current.neighbours)-1)
	newW := make([]W, len(current.weights)-1)
	copy(newNb, current.neighbours[:idx])
	copy(newW, current.weights[:idx])
	copy(newNb[idx:], current.neighbours[idx+1:])
	copy(newW[idx:], current.weights[idx+1:])
	// Compact the handle column in lock-step with neighbours/weights so
	// every SURVIVING slot keeps its ORIGINAL handle. This is the core
	// stable-identity invariant: removing one parallel edge must not
	// renumber the handles of the edges that remain.
	var newH []uint64
	if current.handles != nil {
		newH = make([]uint64, len(current.handles)-1)
		copy(newH, current.handles[:idx])
		copy(newH[idx:], current.handles[idx+1:])
	}
	// Compact the labels column in lock-step too: every surviving slot keeps
	// its ORIGINAL opaque label value. Removing one parallel edge must not
	// renumber or drop the labels of the edges that remain.
	var newL []uint32
	if current.labels != nil {
		newL = make([]uint32, len(current.labels)-1)
		copy(newL, current.labels[:idx])
		copy(newL[idx:], current.labels[idx+1:])
	}
	// Compact the opaque aux column in lock-step via CompactSlot, so its
	// surviving slots keep the SAME positional binding as the surviving
	// neighbours. The validity plane (if any) compacts under the same index
	// transform inside the implementation.
	var newAux AuxColumn
	if current.aux != nil {
		newAux = current.aux.CompactSlot(idx)
	}
	return &adjEntry[W]{neighbours: newNb, weights: newW, handles: newH, labels: newL, aux: newAux}
}

// Neighbours returns an iterator over the live out-neighbours of src
// and the weight of each connecting edge. The iterator captures a
// consistent immutable snapshot of src's adjacency at the time of the
// first call; concurrent mutations of src after that point do not
// affect this iteration. Implements [graph.Graph].
func (a *AdjList[N, W]) Neighbours(src N) iter.Seq2[N, W] {
	return func(yield func(N, W) bool) {
		srcID, ok := a.mapper.Lookup(src)
		if !ok {
			return
		}
		e := loadEntry[W](&a.shards[srcID&shardMask], uint64(srcID)>>shardBits)
		if e == nil {
			return
		}
		for i, n := range e.neighbours {
			v, vok := a.mapper.Resolve(n)
			if !vok {
				continue
			}
			if !yield(v, e.weights[i]) {
				return
			}
		}
	}
}

// Compact right-sizes every adjacency entry's backing arrays to their
// exact live length, reclaiming the slack left by geometric (×2) append
// growth. After a bulk build a degree-d hub typically over-allocates its
// neighbours/weights (and optional handles/labels) columns by up to ~2×;
// across a graph the wasted capacity averages ≈21% of the adjacency
// arrays. Compact walks every shard, and for each occupied slot whose
// entry has any column with spare capacity (cap > len) it builds a fresh
// entry whose every column is allocated at EXACT length (cap == len),
// copying only the live [0:len] data, then publishes it via the same
// atomic store-pointer mechanism the writers use. Entries with no slack
// (every column already cap == len) are skipped to avoid useless churn.
//
// Compact is best run once after a build-then-query workload has finished
// mutating the graph and before the read-heavy query phase, so the
// resident footprint reflects the tight arrays.
//
// Concurrency: Compact is safe for concurrent use with lock-free readers.
// It takes one shard mutex at a time (never two simultaneously, so it can
// never participate in a lock-ordering cycle with the two-lock
// cross-shard [AdjList.addEdge] path) and publishes each trimmed entry
// with [sync/atomic.StorePointer]. A reader holding the prior entry
// pointer keeps iterating the old, untrimmed — but never mutated —
// backing arrays; a reader that loads after the store observes the
// trimmed entry. No reader ever sees a torn or half-trimmed entry. The
// trimmed entry preserves the nil-vs-empty distinction of the optional
// handles/labels columns exactly: a nil column stays nil (a label- or
// handle-free graph never gains a zero-length slice).
//
// Compact honours ctx cancellation between shards, so a cancelled Compact
// of a very large graph stops promptly with whatever shards it has
// already trimmed left consistently published.
func (a *AdjList[N, W]) Compact(ctx context.Context) {
	for i := range a.shards {
		if err := ctx.Err(); err != nil {
			return
		}
		a.compactShard(&a.shards[i])
	}
}

// compactShard trims every slack-bearing entry of a single shard under the
// shard mutex. It is the per-shard body of [AdjList.Compact].
func (a *AdjList[N, W]) compactShard(s *adjShard[W]) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ss := s.slotsRef.Load()
	if ss == nil {
		return
	}
	for idx := range ss.slots {
		p := atomic.LoadPointer(&ss.slots[idx])
		if p == nil {
			continue
		}
		e := (*adjEntry[W])(p)
		trimmed := trimEntry(e)
		if trimmed == nil {
			continue // no slack: leave the slot untouched.
		}
		atomic.StorePointer(&ss.slots[idx], unsafe.Pointer(trimmed)) //nolint:gosec // typed atomic publication of *adjEntry[W]
	}
}

// trimEntry returns a new adjEntry whose every column is allocated at exact
// length (cap == len), or nil when e already has no slack in any column (so
// the caller can skip republishing it). The optional handles/labels columns
// preserve the nil-vs-empty distinction: a nil source column stays nil in the
// result, exactly as [compactEntry] does, so downstream code that branches on
// `handles == nil` / `labels == nil` is unaffected. Column alignment is
// preserved: result.X[i] still corresponds to result.neighbours[i].
func trimEntry[W any](e *adjEntry[W]) *adjEntry[W] {
	// Right-size the opaque aux column too: an lpg sparse property column built by
	// amortised-growth inserts can carry backing slack the topology arrays do not.
	// Compact returns the receiver when it has no slack, so auxChanged is false in
	// the common dense/exactly-sized case.
	var auxTrimmed AuxColumn
	auxChanged := false
	if e.aux != nil {
		auxTrimmed = e.aux.Compact()
		auxChanged = auxTrimmed != e.aux
	}
	if cap(e.neighbours) == len(e.neighbours) &&
		cap(e.weights) == len(e.weights) &&
		(e.handles == nil || cap(e.handles) == len(e.handles)) &&
		(e.labels == nil || cap(e.labels) == len(e.labels)) &&
		!auxChanged {
		return nil
	}
	n := len(e.neighbours)
	nb := make([]graph.NodeID, n)
	copy(nb, e.neighbours)
	ws := make([]W, n)
	copy(ws, e.weights)
	var hs []uint64
	if e.handles != nil {
		hs = make([]uint64, len(e.handles))
		copy(hs, e.handles)
	}
	var ls []uint32
	if e.labels != nil {
		ls = make([]uint32, len(e.labels))
		copy(ls, e.labels)
	}
	// Carry the (possibly compacted) aux column into the trimmed entry — a nil aux
	// stays nil — so a graph that uses edge properties keeps its column logically
	// unchanged while both the topology arrays and the column backing are
	// right-sized.
	return &adjEntry[W]{neighbours: nb, weights: ws, handles: hs, labels: ls, aux: auxTrimmed}
}

// MaxNodeID returns one more than the largest [graph.NodeID] that has
// been assigned by the underlying Mapper. The value is a stable upper
// bound on the NodeID space at the moment of the call and is the
// natural size of a NodeID-indexed companion array (for example, the
// CSR offsets array).
func (a *AdjList[N, W]) MaxNodeID() graph.NodeID {
	return a.mapper.MaxNodeID()
}

// LoadEntry returns immutable snapshots of the neighbours and parallel
// weights of the node identified by id, or (nil, nil) if id has no
// outgoing edges. The returned slices are owned by the current
// adjacency snapshot and must not be mutated by the caller.
func (a *AdjList[N, W]) LoadEntry(id graph.NodeID) (neighbours []graph.NodeID, weights []W) {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if e == nil {
		return nil, nil
	}
	return e.neighbours, e.weights
}

// LoadEntryH returns immutable snapshots of the neighbours, parallel
// weights, and parallel stable handles of the node identified by id. The
// handles slice is nil when this graph carries no per-slot handles (no
// caller ever used [AdjList.AddEdgeH] for id); when non-nil it is the same
// length as neighbours and handles[i] is the stable handle of the edge to
// neighbours[i]. The returned slices are owned by the current adjacency
// snapshot and must not be mutated by the caller.
func (a *AdjList[N, W]) LoadEntryH(id graph.NodeID) (neighbours []graph.NodeID, weights []W, handles []uint64) {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if e == nil {
		return nil, nil, nil
	}
	return e.neighbours, e.weights, e.handles
}

// LoadEntryLabels returns an immutable snapshot of the optional per-slot
// label column of the node identified by id, or nil when id has no outgoing
// edges or no caller has ever set a label on any of id's slots. When non-nil
// the slice is the same length as the neighbours returned by [AdjList.LoadEntry]
// and labels[i] is the OPAQUE label value of the edge to neighbours[i] (0 means
// "no label on that slot"). adjlist never interprets the value; the higher
// layer owns its meaning. The returned slice is owned by the current adjacency
// snapshot and must not be mutated by the caller.
func (a *AdjList[N, W]) LoadEntryLabels(id graph.NodeID) []uint32 {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if e == nil {
		return nil
	}
	return e.labels
}

// LoadEntryAux returns the opaque [AuxColumn] currently attached to the
// adjacency entry of the node identified by id, or nil when id has no outgoing
// edges or no caller has attached an aux column via [AdjList.UpdateEntryAux].
// The returned column is part of the current immutable adjacency snapshot and
// must not be mutated by the caller; the higher layer reads it lock-free. To
// align positionally with the column, read the neighbours from the SAME logical
// snapshot via [AdjList.LoadEntry] and bound any per-slot scan by the shorter
// of the two lengths (a concurrent writer may publish a longer neighbours
// snapshot after the column is loaded).
//
// LoadEntryAux is lock-free and safe for concurrent use.
func (a *AdjList[N, W]) LoadEntryAux(id graph.NodeID) AuxColumn {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if e == nil {
		return nil
	}
	return e.aux
}

// UpdateEntryAux atomically replaces the opaque [AuxColumn] of src's adjacency
// entry under the shard write lock, using the caller-supplied transform fn. fn
// receives the entry's CURRENT aux column (nil when none has been attached yet)
// and an immutable snapshot of its neighbours, and returns the NEW aux column
// plus a bool reporting whether anything changed. When fn reports false the
// entry is left untouched and no new snapshot is published; when it reports
// true a fresh immutable [adjEntry] sharing the unchanged neighbours / weights /
// handles / labels headers and carrying the returned aux is published via the
// same atomic store-pointer mechanism the writers use.
//
// UpdateEntryAux returns false when src has no adjacency entry (no outgoing
// edge), in which case fn is not called — the higher layer's contract is that
// an aux value (an edge property) is only ever attached to a live edge slot, so
// there is no entry to attach to. It returns the bool fn reported otherwise.
//
// The transform MUST build the new column copy-on-write and return it
// fully-populated: adjlist publishes it with a single atomic store, so a
// concurrent lock-free reader observes either the prior column or the new one,
// never a half-built column. fn runs under the shard write lock and must not
// call back into any AdjList method that takes the same shard lock.
//
// UpdateEntryAux is safe for concurrent use.
func (a *AdjList[N, W]) UpdateEntryAux(
	src graph.NodeID,
	fn func(cur AuxColumn, neighbours []graph.NodeID) (AuxColumn, bool),
) bool {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		return false
	}
	newAux, changed := fn(current.aux, current.neighbours)
	if !changed {
		return false
	}
	entry := &adjEntry[W]{
		neighbours: current.neighbours,
		weights:    current.weights,
		handles:    current.handles,
		labels:     current.labels,
		aux:        newAux,
	}
	// storeEntry cannot fail here: the slot already exists, so no growth is
	// required.
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, entry)
	return true
}

// SetEdgeLabelSlot stores the opaque label value v on the first adjacency
// slot of src whose neighbour is dst, publishing a new immutable entry
// snapshot. It returns true when such a slot was found and updated, false
// when src has no edge to dst (the caller's higher layer is responsible for
// the no-live-edge case via its own overflow store).
//
// adjlist treats v as opaque: any uint32 is accepted, including 0 (which the
// higher layer uses as the "no label" sentinel, so passing 0 clears the
// slot's label). When src's entry carries no label column yet, one is
// allocated lazily, length-aligned with neighbours, with every other slot at
// 0; a label-free graph therefore never pays for the column.
//
// Concurrency: the update is copy-on-write. The label column (and the entry)
// is copied with the change applied and published via the same atomic
// store-pointer mechanism as [AdjList.AddEdgeH]; the slot's existing index is
// never mutated in place, so a concurrent lock-free reader holding the prior
// snapshot is unaffected. SetEdgeLabelSlot is safe for concurrent use.
func (a *AdjList[N, W]) SetEdgeLabelSlot(src, dst graph.NodeID, v uint32) bool {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		return false
	}
	idx := -1
	for i, n := range current.neighbours {
		if n == dst {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	// Copy-on-write the label column. Sharing the immutable neighbours/weights/
	// handles headers into the new entry is safe (they are never mutated after
	// publication); only the label column is replaced so an in-place write at a
	// live index can never race a concurrent reader.
	newL := make([]uint32, len(current.neighbours))
	copy(newL, current.labels) // no-op when current.labels is nil
	newL[idx] = v
	entry := &adjEntry[W]{
		neighbours: current.neighbours,
		weights:    current.weights,
		handles:    current.handles,
		labels:     newL,
		// The label-COW shares the immutable aux header unchanged: only the
		// label column is replaced here.
		aux: current.aux,
	}
	// storeEntry cannot fail here: the slot already exists, so no growth is
	// required.
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, entry)
	return true
}

// ClearEdgeLabelSlotValue clears the opaque label of the FIRST adjacency slot
// of src whose neighbour is dst AND whose label equals v, publishing a new
// immutable entry snapshot. It returns true when such a slot was found and
// cleared. This targets a specific label value so a multigraph pair whose
// parallel slots carry different labels can drop exactly one of them without
// disturbing the others. No-op when v is 0, src has no label column, or no
// dst-matching slot carries v.
//
// Concurrency: copy-on-write, identical to [AdjList.SetEdgeLabelSlot]; safe
// for concurrent use.
func (a *AdjList[N, W]) ClearEdgeLabelSlotValue(src, dst graph.NodeID, v uint32) bool {
	if v == 0 {
		return false
	}
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil || current.labels == nil {
		return false
	}
	idx := -1
	for i, n := range current.neighbours {
		if n == dst && i < len(current.labels) && current.labels[i] == v {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	newL := make([]uint32, len(current.labels))
	copy(newL, current.labels)
	newL[idx] = 0
	entry := &adjEntry[W]{
		neighbours: current.neighbours,
		weights:    current.weights,
		handles:    current.handles,
		labels:     newL,
		// The label-COW shares the immutable aux header unchanged: only the
		// label column is replaced here.
		aux: current.aux,
	}
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, entry)
	return true
}

// ClearEdgeLabelSlots clears the opaque label of EVERY adjacency slot of src
// whose neighbour is dst (all parallel slots in a multigraph), publishing a
// single new immutable entry snapshot. It is the bulk inverse used when the
// last edge between an endpoint pair is removed and the higher layer must drop
// the pair's per-slot labels in lockstep with its own overflow state. No-op
// (and no allocation) when src has no label column or no dst-matching slot
// carries a label.
//
// Concurrency: copy-on-write, identical to [AdjList.SetEdgeLabelSlot]; safe
// for concurrent use.
func (a *AdjList[N, W]) ClearEdgeLabelSlots(src, dst graph.NodeID) {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil || current.labels == nil {
		return
	}
	// Find dst-matching slots that still carry a label; skip the allocation
	// when there is nothing to clear.
	var newL []uint32
	for i, n := range current.neighbours {
		if n != dst || i >= len(current.labels) || current.labels[i] == 0 {
			continue
		}
		if newL == nil {
			newL = make([]uint32, len(current.labels))
			copy(newL, current.labels)
		}
		newL[i] = 0
	}
	if newL == nil {
		return
	}
	entry := &adjEntry[W]{
		neighbours: current.neighbours,
		weights:    current.weights,
		handles:    current.handles,
		labels:     newL,
		// The label-COW shares the immutable aux header unchanged: only the
		// label column is replaced here.
		aux: current.aux,
	}
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, entry)
}

// loadEntry atomically reads the entry stored at intraIdx within s.
// It returns nil when intraIdx is beyond the shard's allocated slot
// range or when no entry has yet been published at that slot.
func loadEntry[W any](s *adjShard[W], intraIdx uint64) *adjEntry[W] {
	ss := s.slotsRef.Load()
	if ss == nil || intraIdx >= uint64(len(ss.slots)) {
		return nil
	}
	p := atomic.LoadPointer(&ss.slots[intraIdx])
	if p == nil {
		return nil
	}
	return (*adjEntry[W])(p)
}

// storeEntry publishes entry at intraIdx within s. The caller must
// hold s.mu. The shard grows on demand to accommodate intraIdx,
// honouring maxCap as a hard upper bound when maxCap > 0. Returns
// [ErrShardFull] when intraIdx+1 exceeds maxCap; on that path no
// shard mutation is performed.
func storeEntry[W any](s *adjShard[W], intraIdx uint64, maxCap int, entry *adjEntry[W]) error {
	ss := s.slotsRef.Load()
	if ss == nil || intraIdx >= uint64(len(ss.slots)) {
		if err := growShardLocked[W](s, intraIdx+1, maxCap); err != nil {
			return err
		}
		ss = s.slotsRef.Load()
	}
	atomic.StorePointer(&ss.slots[intraIdx], unsafe.Pointer(entry)) //nolint:gosec // typed atomic publication of *adjEntry[W]
	return nil
}

// growShardLocked enlarges s.slotsRef so that minLen slots are
// available, capping growth at maxCap when maxCap > 0. The caller
// must hold s.mu. Returns [ErrShardFull] when minLen exceeds maxCap;
// in that case s.slotsRef is left unchanged.
func growShardLocked[W any](s *adjShard[W], minLen uint64, maxCap int) error {
	if maxCap > 0 && minLen > uint64(maxCap) {
		return ErrShardFull
	}
	old := s.slotsRef.Load()
	var oldLen uint64
	if old != nil {
		oldLen = uint64(len(old.slots))
	}
	newLen := oldLen
	if newLen < initialShardCap {
		newLen = initialShardCap
	}
	for newLen < minLen {
		newLen *= 2
	}
	if maxCap > 0 && newLen > uint64(maxCap) {
		newLen = uint64(maxCap)
	}
	if newLen == oldLen {
		return nil
	}
	next := &shardSlots{slots: make([]unsafe.Pointer, newLen)}
	if old != nil {
		// A plain copy is safe: the caller holds s.mu, so no concurrent
		// StorePointer can target old.slots, and unsafe.Pointer values
		// are plain words. Readers still observing the old slice continue
		// to see consistent per-slot snapshots until they next reload
		// slotsRef.
		copy(next.slots, old.slots)
	}
	s.slotsRef.Store(next)
	return nil
}

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
// Writes (AddEdge, RemoveEdge, Compact) take the shard mutex, copy
// the entry's slices with the modification applied, and publish the
// new entry pointer via [sync/atomic.StorePointer]. Concurrent
// readers always observe a consistent snapshot — either the entry
// before the write or the entry after it.
//
// The yield callback of [AdjList.Neighbours] iterates over slices
// owned by a snapshotted entry and may safely call any other AdjList
// operation; no locks are held during yield.
package adjlist

import (
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
type adjEntry[W any] struct {
	neighbours []graph.NodeID
	weights    []W
	handles    []uint64 // nil unless AddEdgeH supplied a handle; parallel to neighbours when set
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
	return a.addEdge(src, dst, w, 0, false)
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
	return a.addEdge(src, dst, w, handle, true)
}

// addEdge is the shared implementation of [AdjList.AddEdge] and
// [AdjList.AddEdgeH]. When hasHandle is false the slot's handle column is
// left untouched (nil for a fresh entry); when true the supplied handle is
// stored parallel to the new neighbour.
//
// For undirected multigraphs where src and dst land in DIFFERENT shards the
// two shard locks are acquired simultaneously in a canonical (lower-index-
// first) order before both appends are performed. This ensures the forward
// and mirror slots are assigned atomically — no concurrent parallel-edge
// insertion can interleave between the two appends — so both directions
// always reflect the same slot ordering.
func (a *AdjList[N, W]) addEdge(src, dst N, w W, handle uint64, hasHandle bool) error {
	srcID := a.mapper.Intern(src)
	dstID := a.mapper.Intern(dst)

	// Directed graphs and self-loops need only the forward append.
	if a.cfg.Directed || srcID == dstID {
		inserted, err := a.upsertEdge(srcID, dstID, w, handle, hasHandle)
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
		inserted, err := a.upsertEdge(srcID, dstID, w, handle, hasHandle)
		if err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		a.size.Add(1)
		if _, err := a.upsertEdge(dstID, srcID, w, handle, hasHandle); err != nil {
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

	inserted, err := a.upsertEdgeLocked(srcID, dstID, w, handle, hasHandle)
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
	if _, err := a.upsertEdgeLocked(dstID, srcID, w, handle, hasHandle); err != nil {
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
func (a *AdjList[N, W]) upsertEdge(src, dst graph.NodeID, w W, handle uint64, hasHandle bool) (bool, error) {
	s := &a.shards[src&shardMask]
	s.mu.Lock()
	defer s.mu.Unlock()
	return a.upsertEdgeLocked(src, dst, w, handle, hasHandle)
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
func (a *AdjList[N, W]) upsertEdgeLocked(src, dst graph.NodeID, w W, handle uint64, hasHandle bool) (bool, error) {
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
		if hasHandle {
			entry.handles = make([]uint64, 1, c)
			entry.handles[0] = handle
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
		if hasHandle || current.handles != nil {
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
			hs[oldLen] = handle
		}
		if err := storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{neighbours: nb, weights: ws, handles: hs}); err != nil {
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
	if hasHandle || current.handles != nil {
		newH = make([]uint64, newLen, newCap)
		copy(newH, current.handles) // copy is a no-op when current.handles is nil
		newH[oldLen] = handle
	}
	if err := storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{neighbours: newNb, weights: newW, handles: newH}); err != nil {
		return false, err
	}
	return true, nil
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
	return &adjEntry[W]{neighbours: newNb, weights: newW, handles: newH}
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

// Compact is a no-op in the current copy-on-write implementation:
// removed edges already release their slots' previous slices on the
// next write. It is retained for API symmetry with future backends
// that may benefit from explicit compaction.
func (a *AdjList[N, W]) Compact() {}

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

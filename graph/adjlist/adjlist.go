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
func (a *AdjList[N, W]) addEdge(src, dst N, w W, handle uint64, hasHandle bool) error {
	srcID := a.mapper.Intern(src)
	dstID := a.mapper.Intern(dst)

	inserted, err := a.upsertEdge(srcID, dstID, w, handle, hasHandle)
	if err != nil {
		return err
	}
	if !inserted {
		return nil
	}
	a.size.Add(1)

	if a.cfg.Directed || srcID == dstID {
		return nil
	}
	if _, err := a.upsertEdge(dstID, srcID, w, handle, hasHandle); err != nil {
		// The forward edge has already been published; undo it to
		// preserve the all-or-nothing contract of AddEdge on
		// undirected graphs.
		a.removeOneEdge(srcID, dstID)
		a.size.Add(^uint64(0))
		return err
	}
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
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		entry := &adjEntry[W]{
			neighbours: []graph.NodeID{dst},
			weights:    []W{w},
		}
		if hasHandle {
			entry.handles = []uint64{handle}
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
	newNb := make([]graph.NodeID, len(current.neighbours)+1)
	newW := make([]W, len(current.weights)+1)
	copy(newNb, current.neighbours)
	copy(newW, current.weights)
	newNb[len(current.neighbours)] = dst
	newW[len(current.weights)] = w
	// Carry the handle column forward only when this graph uses handles
	// — either the existing entry already has one or this call supplies
	// one. A previously handle-less entry that gains its first handle on
	// a later append back-fills the earlier slots with 0 (the reserved
	// "no handle" sentinel), so the column stays length-aligned with
	// neighbours.
	var newH []uint64
	if hasHandle || current.handles != nil {
		newH = make([]uint64, len(current.neighbours)+1)
		copy(newH, current.handles) // copy is a no-op when current.handles is nil
		newH[len(current.neighbours)] = handle
	}
	if err := storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{neighbours: newNb, weights: newW, handles: newH}); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveEdge removes the directed edge from src to dst if present. For
// multigraphs only one occurrence is removed per call. The endpoints
// remain in the graph. Implements [graph.Graph].
func (a *AdjList[N, W]) RemoveEdge(src, dst N) {
	srcID, ok := a.mapper.Lookup(src)
	if !ok {
		return
	}
	dstID, ok := a.mapper.Lookup(dst)
	if !ok {
		return
	}
	if !a.removeOneEdge(srcID, dstID) {
		return
	}
	a.size.Add(^uint64(0))

	if a.cfg.Directed || srcID == dstID {
		return
	}
	a.removeOneEdge(dstID, srcID)
}

// removeOneEdge publishes a new adjacency snapshot for src that omits
// one occurrence of dst. Returns true when an edge was removed.
func (a *AdjList[N, W]) removeOneEdge(src, dst graph.NodeID) bool {
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
	if len(current.neighbours) == 1 {
		// storeEntry cannot fail here because the slot already exists
		// in the shard's slot array; no growth is required.
		_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{})
		return true
	}
	newNb := make([]graph.NodeID, len(current.neighbours)-1)
	newW := make([]W, len(current.weights)-1)
	copy(newNb, current.neighbours[:idx])
	copy(newW, current.weights[:idx])
	copy(newNb[idx:], current.neighbours[idx+1:])
	copy(newW[idx:], current.weights[idx+1:])
	// Compact the handle column in lock-step with neighbours/weights so
	// every SURVIVING slot keeps its ORIGINAL handle. This is the core
	// stable-identity invariant: removing one parallel edge must not
	// renumber the handles of the edges that remain. The positional
	// per-instance inference the old read path used broke precisely
	// here, because the slot shift after a delete re-mapped the
	// remaining slots to the wrong CREATE indices.
	var newH []uint64
	if current.handles != nil {
		newH = make([]uint64, len(current.handles)-1)
		copy(newH, current.handles[:idx])
		copy(newH[idx:], current.handles[idx+1:])
	}
	// storeEntry cannot fail here: same slot, no growth required.
	_ = storeEntry[W](s, intraIdx, a.cfg.MaxShardCapacity, &adjEntry[W]{neighbours: newNb, weights: newW, handles: newH})
	return true
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
		// Copy is safe because the caller holds s.mu, preventing other
		// writers, and unsafe.Pointer values are plain words. Readers
		// still observing the old slice continue to see consistent
		// per-slot snapshots until they next reload slotsRef.
		for i, p := range old.slots {
			next.slots[i] = atomic.LoadPointer(&old.slots[i])
			_ = p
		}
	}
	s.slotsRef.Store(next)
	return nil
}

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
	"iter"
	"sync"
	"sync/atomic"
	"unsafe"

	"gograph/graph"
)

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
}

// AdjList is a mutable adjacency-list graph generic over the user node
// type N and the edge weight type W. Construct one with [New].
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
type adjEntry[W any] struct {
	neighbours []graph.NodeID
	weights    []W
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

// AddNode inserts n if not already present. The node enters the
// adjacency list lazily on its first outgoing edge; AddNode only
// interns the value with the Mapper, which is sufficient for
// [AdjList.Order] to account for it. Implements [graph.Graph].
func (a *AdjList[N, W]) AddNode(n N) { a.mapper.Intern(n) }

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
func (a *AdjList[N, W]) AddEdge(src, dst N, w W) {
	srcID := a.mapper.Intern(src)
	dstID := a.mapper.Intern(dst)

	if !a.upsertEdge(srcID, dstID, w) {
		return
	}
	a.size.Add(1)

	if a.cfg.Directed || srcID == dstID {
		return
	}
	a.upsertEdge(dstID, srcID, w)
}

// upsertEdge publishes a new adjacency snapshot for src that includes
// (dst, w). Returns false when (in simple-graph mode) dst is already
// a neighbour. The new snapshot is constructed fresh and swapped in
// via atomic.StorePointer so concurrent readers always observe a
// consistent immutable adjacency.
func (a *AdjList[N, W]) upsertEdge(src, dst graph.NodeID, w W) bool {
	s := &a.shards[src&shardMask]
	intraIdx := uint64(src) >> shardBits

	s.mu.Lock()
	defer s.mu.Unlock()

	current := loadEntry[W](s, intraIdx)
	if current == nil {
		storeEntry[W](s, intraIdx, &adjEntry[W]{
			neighbours: []graph.NodeID{dst},
			weights:    []W{w},
		})
		return true
	}
	if !a.cfg.Multigraph {
		for _, n := range current.neighbours {
			if n == dst {
				return false
			}
		}
	}
	newNb := make([]graph.NodeID, len(current.neighbours)+1)
	newW := make([]W, len(current.weights)+1)
	copy(newNb, current.neighbours)
	copy(newW, current.weights)
	newNb[len(current.neighbours)] = dst
	newW[len(current.weights)] = w
	storeEntry[W](s, intraIdx, &adjEntry[W]{neighbours: newNb, weights: newW})
	return true
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
		storeEntry[W](s, intraIdx, &adjEntry[W]{})
		return true
	}
	newNb := make([]graph.NodeID, len(current.neighbours)-1)
	newW := make([]W, len(current.weights)-1)
	copy(newNb, current.neighbours[:idx])
	copy(newW, current.weights[:idx])
	copy(newNb[idx:], current.neighbours[idx+1:])
	copy(newW[idx:], current.weights[idx+1:])
	storeEntry[W](s, intraIdx, &adjEntry[W]{neighbours: newNb, weights: newW})
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
// hold s.mu. The shard grows on demand to accommodate intraIdx.
func storeEntry[W any](s *adjShard[W], intraIdx uint64, entry *adjEntry[W]) {
	ss := s.slotsRef.Load()
	if ss == nil || intraIdx >= uint64(len(ss.slots)) {
		growShardLocked[W](s, intraIdx+1)
		ss = s.slotsRef.Load()
	}
	atomic.StorePointer(&ss.slots[intraIdx], unsafe.Pointer(entry)) //nolint:gosec // typed atomic publication of *adjEntry[W]
}

// growShardLocked enlarges s.slotsRef so that minLen slots are
// available. The caller must hold s.mu.
func growShardLocked[W any](s *adjShard[W], minLen uint64) {
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
	if newLen == oldLen {
		return
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
}

// Package lpg implements the Labelled Property Graph model on top of
// the [gograph/graph/adjlist] mutable adjacency-list backend.
//
// An LPG decorates each node and each edge with a set of labels
// (interned strings identifying classes/types) and a bag of typed
// properties. This package provides the label half of that contract
// (see [SetNodeLabel], [SetEdgeLabel]); typed properties are added
// by subsequent tasks in the same sprint.
//
// # Concurrency
//
// The Graph type is safe for concurrent use. Label operations are
// guarded by their own RWMutexes; the underlying adjacency list
// retains its own contracts.
package lpg

import (
	"sync"
	"sync/atomic"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/index"
	"gograph/graph/index/label"
)

// LabelID is the compact internal identifier produced by the
// [LabelRegistry] for an interned label string.
type LabelID uint32

// LabelRegistry interns label names and assigns sequential LabelIDs.
// It is safe for concurrent use.
type LabelRegistry struct {
	mu      sync.RWMutex
	forward map[string]LabelID
	reverse []string
}

// NewLabelRegistry returns an empty registry.
func NewLabelRegistry() *LabelRegistry {
	return &LabelRegistry{forward: make(map[string]LabelID)}
}

// Intern returns a stable LabelID for name, allocating one on first
// encounter. The fast path takes a read lock only.
func (r *LabelRegistry) Intern(name string) LabelID {
	r.mu.RLock()
	if id, ok := r.forward[name]; ok {
		r.mu.RUnlock()
		return id
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.forward[name]; ok {
		return id
	}
	id := LabelID(len(r.reverse))
	r.reverse = append(r.reverse, name)
	r.forward[name] = id
	return id
}

// Lookup returns the LabelID for name and true, or 0 and false when
// name has not been interned.
func (r *LabelRegistry) Lookup(name string) (LabelID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.forward[name]
	return id, ok
}

// Resolve returns the name interned under id, or the empty string and
// false when id is unknown.
func (r *LabelRegistry) Resolve(id LabelID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if uint64(id) >= uint64(len(r.reverse)) {
		return "", false
	}
	return r.reverse[id], true
}

// edgeKey identifies a single directed edge endpoints pair for label
// storage. Multigraph parallel edges share a key here; v1 stores the
// union of labels across parallel edges. A future revision can carry
// a per-edge index when parallel-edge label semantics matter.
type edgeKey struct {
	src, dst graph.NodeID
}

// propMapShards is the number of independent locks striping the
// per-vertex and per-edge property maps. Sized to keep contention
// below 5% on workloads with up to a few hundred concurrent
// readers/writers; not as wide as adjlist's 256 because property
// access is less hot than adjacency.
const propMapShards = 16

// nodePropShard is one stripe of the per-vertex property map.
type nodePropShard struct {
	mu sync.RWMutex
	m  map[graph.NodeID]map[PropertyKeyID]PropertyValue
}

// edgePropShard is one stripe of the per-edge property map.
type edgePropShard struct {
	mu sync.RWMutex
	m  map[edgeKey]map[PropertyKeyID]PropertyValue
}

// nodeLabelShard is one stripe of the node-label bag. The mutex
// serialises mutations on this shard only; readers hold an RLock
// for HasNodeLabel / NodeLabels. Splitting the bag into 16 shards
// removes the global nodeMu contention point that previously
// serialised every Set/Remove/Has across all NodeIDs in the graph.
type nodeLabelShard struct {
	mu sync.RWMutex
	m  map[graph.NodeID]map[LabelID]struct{}
}

// edgeLabelShard is the edge-label counterpart of [nodeLabelShard];
// the shard is keyed by the src endpoint so all labels of edges
// out of one node coalesce in the same shard (favourable for the
// common access pattern: walk-out-of-node-then-inspect-label).
type edgeLabelShard struct {
	mu sync.RWMutex
	m  map[edgeKey]map[LabelID]struct{}
}

// Graph is a labelled property graph generic over the user node type
// N and edge weight type W. It composes an [adjlist.AdjList] with a
// label registry and per-vertex / per-edge label storage backed by
// [label.Index] bitmaps.
type Graph[N comparable, W any] struct {
	adj     *adjlist.AdjList[N, W]
	reg     *LabelRegistry
	pkeys   *PropertyKeyRegistry
	nodeIdx *label.Index
	edgeIdx *label.Index

	nodeLabelShards [propMapShards]nodeLabelShard
	edgeLabelShards [propMapShards]edgeLabelShard

	nodePropShards [propMapShards]nodePropShard
	edgePropShards [propMapShards]edgePropShard

	// tombstones records NodeIDs that have been removed by RemoveNode.
	// The underlying Mapper cannot release the index slot (NodeID stability
	// is a hard contract), so removal is observable only via this set:
	// every read path (Order, IsTombstoned, WalkLiveNodes) must filter
	// tombstoned ids.
	tombstoneMu sync.RWMutex
	tombstones  map[graph.NodeID]struct{}

	// nodesAddedCount / nodesRemovedCount / edgesAddedCount /
	// edgesRemovedCount track per-direction counters used by the TCK
	// side-effect comparator. Net Order() / Size() can't distinguish a
	// CREATE+DELETE from a no-op, so the comparator needs the explicit
	// addition and removal counts.
	nodesAddedCount   atomic.Uint64
	nodesRemovedCount atomic.Uint64
	edgesAddedCount   atomic.Uint64
	edgesRemovedCount atomic.Uint64

	idxMgr    *index.Manager
	validator atomicValidator
}

// SetValidator installs v as the runtime schema validator for this graph.
// Once set, every call to [Graph.SetNodeProperty] and [Graph.SetEdgeProperty]
// will invoke v.Validate before applying the write; a non-nil error from
// Validate causes the write to be rejected and the error returned to the
// caller.
//
// Pass nil to remove any previously installed validator.
//
// SetValidator is safe for concurrent use.
func (g *Graph[N, W]) SetValidator(v SchemaValidator) { g.validator.store(v) }

// nodePropShardFor returns the shard responsible for NodeID id.
func (g *Graph[N, W]) nodePropShardFor(id graph.NodeID) *nodePropShard {
	return &g.nodePropShards[uint64(id)&(propMapShards-1)]
}

// nodeLabelShardFor returns the label shard responsible for NodeID id.
func (g *Graph[N, W]) nodeLabelShardFor(id graph.NodeID) *nodeLabelShard {
	return &g.nodeLabelShards[uint64(id)&(propMapShards-1)]
}

// edgeLabelShardFor returns the label shard responsible for the
// edgeKey k. Keyed on the src endpoint so the shard alignment
// matches [edgePropShardFor].
func (g *Graph[N, W]) edgeLabelShardFor(k edgeKey) *edgeLabelShard {
	return &g.edgeLabelShards[uint64(k.src)&(propMapShards-1)]
}

// edgePropShardFor returns the shard responsible for the edgeKey k.
// The shard is keyed by the src endpoint so all properties of edges
// out of one node coalesce in the same shard (favourable for the
// common access pattern: enumerate-outgoing-edges-with-property).
func (g *Graph[N, W]) edgePropShardFor(k edgeKey) *edgePropShard {
	return &g.edgePropShards[uint64(k.src)&(propMapShards-1)]
}

// propKeys returns the property-key registry.
func (g *Graph[N, W]) propKeys() *PropertyKeyRegistry { return g.pkeys }

// PropertyKeys returns the property-key registry.
func (g *Graph[N, W]) PropertyKeys() *PropertyKeyRegistry { return g.pkeys }

// New returns a fresh LPG built on top of a new [adjlist.AdjList]
// configured by cfg.
func New[N comparable, W any](cfg adjlist.Config) *Graph[N, W] {
	g := &Graph[N, W]{
		adj:     adjlist.New[N, W](cfg),
		reg:     NewLabelRegistry(),
		pkeys:   NewPropertyKeyRegistry(),
		nodeIdx: label.NewIndex(),
		edgeIdx: label.NewIndex(),
	}
	for i := range g.nodeLabelShards {
		g.nodeLabelShards[i].m = make(map[graph.NodeID]map[LabelID]struct{})
	}
	for i := range g.edgeLabelShards {
		g.edgeLabelShards[i].m = make(map[edgeKey]map[LabelID]struct{})
	}
	for i := range g.nodePropShards {
		g.nodePropShards[i].m = make(map[graph.NodeID]map[PropertyKeyID]PropertyValue)
	}
	for i := range g.edgePropShards {
		g.edgePropShards[i].m = make(map[edgeKey]map[PropertyKeyID]PropertyValue)
	}
	return g
}

// AdjList returns the underlying adjacency-list backend.
func (g *Graph[N, W]) AdjList() *adjlist.AdjList[N, W] { return g.adj }

// Registry returns the underlying label registry.
func (g *Graph[N, W]) Registry() *LabelRegistry { return g.reg }

// NodeIndex returns the label index over nodes.
func (g *Graph[N, W]) NodeIndex() *label.Index { return g.nodeIdx }

// EdgeIndex returns the label index over edges. Edge bitmaps are
// keyed by the source NodeID; this is suitable for label-filtered
// out-neighbour scans but not for direct edge enumeration.
func (g *Graph[N, W]) EdgeIndex() *label.Index { return g.edgeIdx }

// IndexManager returns the manager of secondary indexes attached to
// this graph, or nil when no manager has been set. Callers that need
// snapshot-durable indexes must register them via [index.Manager.CreateIndex]
// on a manager set via [Graph.SetIndexManager].
//
// IndexManager is safe for concurrent use.
func (g *Graph[N, W]) IndexManager() *index.Manager { return g.idxMgr }

// SetIndexManager installs m as the manager of secondary indexes on
// this graph. Passing nil detaches the current manager. The Graph
// retains a borrowed reference to m; the caller owns m's lifetime.
//
// SetIndexManager is intended to be called during graph construction
// (before any concurrent mutators are spawned). It is safe to call
// from any goroutine, but readers that captured g.IndexManager()
// before the swap continue to see the previous value until they
// re-read.
func (g *Graph[N, W]) SetIndexManager(m *index.Manager) { g.idxMgr = m }

// AddNode inserts n if not already present. The error contract
// matches the underlying [adjlist.AdjList.AddNode]: callers must
// propagate [adjlist.ErrShardFull] when the responsible shard is at
// [adjlist.Config.MaxShardCapacity].
func (g *Graph[N, W]) AddNode(n N) error { return g.adj.AddNode(n) }

// AddEdge inserts a directed edge (mirrored when the graph is
// undirected) from src to dst with weight w. The error contract
// matches the underlying [adjlist.AdjList.AddEdge]: callers must
// propagate [adjlist.ErrShardFull] when the responsible shard is at
// [adjlist.Config.MaxShardCapacity].
func (g *Graph[N, W]) AddEdge(src, dst N, w W) error { return g.adj.AddEdge(src, dst, w) }

// SetNodeLabel attaches label to n, inserting n if needed. Returns
// the error from the underlying [adjlist.AdjList.AddNode] (which can
// only happen via a future bounded-growth implementation); the
// current [adjlist.AdjList.AddNode] never fails, so callers in
// codepaths that do not configure [adjlist.Config.MaxShardCapacity]
// may safely ignore the return.
func (g *Graph[N, W]) SetNodeLabel(n N, name string) error {
	if err := g.adj.AddNode(n); err != nil {
		return err
	}
	id, _ := g.adj.Mapper().Lookup(n)
	lid := g.reg.Intern(name)
	sh := g.nodeLabelShardFor(id)
	sh.mu.Lock()
	bag, ok := sh.m[id]
	if !ok {
		bag = make(map[LabelID]struct{})
		sh.m[id] = bag
	}
	bag[lid] = struct{}{}
	sh.mu.Unlock()
	g.nodeIdx.Add(uint32(lid), id)
	return nil
}

// RemoveNode marks the node n as removed. Subsequent reads through
// IsTombstoned / WalkLiveNodes treat n as absent. The underlying
// Mapper retains the slot (NodeID stability is a hard contract), but
// label, property, and adjacency reads on the tombstoned id remain
// safe; callers should also strip labels / properties / incident
// edges before calling RemoveNode so the tombstone reflects the
// fully-deleted node state. No-op when n was never interned or is
// already tombstoned.
func (g *Graph[N, W]) RemoveNode(n N) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	g.tombstoneMu.Lock()
	if g.tombstones == nil {
		g.tombstones = make(map[graph.NodeID]struct{})
	}
	g.tombstones[id] = struct{}{}
	g.tombstoneMu.Unlock()
}

// IsTombstoned reports whether id has been marked removed via
// [Graph.RemoveNode]. Used by the Cypher executor's AllNodesScan to
// skip phantom nodes (those that the Mapper still indexes but that
// the graph treats as deleted).
func (g *Graph[N, W]) IsTombstoned(id graph.NodeID) bool {
	g.tombstoneMu.RLock()
	defer g.tombstoneMu.RUnlock()
	_, ok := g.tombstones[id]
	return ok
}

// LiveOrder returns the number of non-tombstoned interned nodes.
func (g *Graph[N, W]) LiveOrder() uint64 {
	total := g.adj.Order()
	g.tombstoneMu.RLock()
	dead := uint64(len(g.tombstones))
	g.tombstoneMu.RUnlock()
	if dead > total {
		return 0
	}
	return total - dead
}

// SideEffectCounters returns the per-direction counters maintained by the
// graph: nodes added, nodes removed, edges added, edges removed since
// SnapshotSideEffectCounters was last called. Used by the Cypher TCK
// side-effect comparator to verify +nodes / -nodes / +relationships /
// -relationships are accurate counts (not net changes).
func (g *Graph[N, W]) SideEffectCounters() (nodesAdded, nodesRemoved, edgesAdded, edgesRemoved uint64) {
	return g.nodesAddedCount.Load(),
		g.nodesRemovedCount.Load(),
		g.edgesAddedCount.Load(),
		g.edgesRemovedCount.Load()
}

// IncrNodesAdded / IncrNodesRemoved / IncrEdgesAdded / IncrEdgesRemoved
// expose the per-direction counters to the cypher executor so the
// mutator adapters can record each event as it happens. The graph
// itself does not call these — node and edge mutation flow through
// the adapters, which know whether a given AddNode/AddEdge was a
// fresh allocation or a no-op re-intern.
func (g *Graph[N, W]) IncrNodesAdded()   { g.nodesAddedCount.Add(1) }
func (g *Graph[N, W]) IncrNodesRemoved() { g.nodesRemovedCount.Add(1) }
func (g *Graph[N, W]) IncrEdgesAdded()   { g.edgesAddedCount.Add(1) }
func (g *Graph[N, W]) IncrEdgesRemoved() { g.edgesRemovedCount.Add(1) }

// RemoveNodeLabel detaches name from n. No-op if absent.
func (g *Graph[N, W]) RemoveNodeLabel(n N, name string) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	lid, ok := g.reg.Lookup(name)
	if !ok {
		return
	}
	sh := g.nodeLabelShardFor(id)
	sh.mu.Lock()
	if bag, ok2 := sh.m[id]; ok2 {
		delete(bag, lid)
		if len(bag) == 0 {
			delete(sh.m, id)
		}
	}
	sh.mu.Unlock()
	g.nodeIdx.Remove(uint32(lid), id)
}

// HasNodeLabel reports whether n carries the named label.
func (g *Graph[N, W]) HasNodeLabel(n N, name string) bool {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return false
	}
	lid, ok := g.reg.Lookup(name)
	if !ok {
		return false
	}
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	bag, ok := sh.m[id]
	if !ok {
		return false
	}
	_, ok = bag[lid]
	return ok
}

// NodeLabels returns the names of every label attached to n in
// unspecified order.
func (g *Graph[N, W]) NodeLabels(n N) []string {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return nil
	}
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag, ok := sh.m[id]
	if !ok {
		sh.mu.RUnlock()
		return nil
	}
	out := make([]string, 0, len(bag))
	for lid := range bag {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	}
	sh.mu.RUnlock()
	return out
}

// SetEdgeLabel attaches label to the directed edge (src, dst). The
// edge must already exist in the underlying adjacency list; otherwise
// the call is a no-op. The label is associated with the source
// NodeID's row in the edge index.
func (g *Graph[N, W]) SetEdgeLabel(src, dst N, name string) {
	if !g.adj.HasEdge(src, dst) {
		return
	}
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	lid := g.reg.Intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	bag, ok := sh.m[k]
	if !ok {
		bag = make(map[LabelID]struct{})
		sh.m[k] = bag
	}
	bag[lid] = struct{}{}
	sh.mu.Unlock()
	g.edgeIdx.Add(uint32(lid), srcID)
}

// HasEdgeLabel reports whether the directed edge (src, dst) carries
// name as a label.
func (g *Graph[N, W]) HasEdgeLabel(src, dst N, name string) bool {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	lid, ok := g.reg.Lookup(name)
	if !ok {
		return false
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	bag, ok := sh.m[k]
	if !ok {
		return false
	}
	_, ok = bag[lid]
	return ok
}

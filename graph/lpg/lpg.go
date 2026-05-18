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

	"gograph/graph"
	"gograph/graph/adjlist"
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

// Graph is a labelled property graph generic over the user node type
// N and edge weight type W. It composes an [adjlist.AdjList] with a
// label registry and per-vertex / per-edge label storage backed by
// [label.Index] bitmaps.
type Graph[N comparable, W any] struct {
	adj     *adjlist.AdjList[N, W]
	reg     *LabelRegistry
	nodeIdx *label.Index
	edgeIdx *label.Index
	nodeMu  sync.RWMutex
	edgeMu  sync.RWMutex
	nodeBag map[graph.NodeID]map[LabelID]struct{}
	edgeBag map[edgeKey]map[LabelID]struct{}
}

// New returns a fresh LPG built on top of a new [adjlist.AdjList]
// configured by cfg.
func New[N comparable, W any](cfg adjlist.Config) *Graph[N, W] {
	return &Graph[N, W]{
		adj:     adjlist.New[N, W](cfg),
		reg:     NewLabelRegistry(),
		nodeIdx: label.NewIndex(),
		edgeIdx: label.NewIndex(),
		nodeBag: make(map[graph.NodeID]map[LabelID]struct{}),
		edgeBag: make(map[edgeKey]map[LabelID]struct{}),
	}
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

// AddNode inserts n if not already present.
func (g *Graph[N, W]) AddNode(n N) { g.adj.AddNode(n) }

// AddEdge inserts a directed edge (mirrored when the graph is
// undirected) from src to dst with weight w.
func (g *Graph[N, W]) AddEdge(src, dst N, w W) { g.adj.AddEdge(src, dst, w) }

// SetNodeLabel attaches label to n, inserting n if needed.
func (g *Graph[N, W]) SetNodeLabel(n N, name string) {
	g.adj.AddNode(n)
	id, _ := g.adj.Mapper().Lookup(n)
	lid := g.reg.Intern(name)
	g.nodeMu.Lock()
	bag, ok := g.nodeBag[id]
	if !ok {
		bag = make(map[LabelID]struct{})
		g.nodeBag[id] = bag
	}
	bag[lid] = struct{}{}
	g.nodeMu.Unlock()
	g.nodeIdx.Add(uint32(lid), id)
}

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
	g.nodeMu.Lock()
	if bag, ok2 := g.nodeBag[id]; ok2 {
		delete(bag, lid)
		if len(bag) == 0 {
			delete(g.nodeBag, id)
		}
	}
	g.nodeMu.Unlock()
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
	g.nodeMu.RLock()
	defer g.nodeMu.RUnlock()
	bag, ok := g.nodeBag[id]
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
	g.nodeMu.RLock()
	bag, ok := g.nodeBag[id]
	if !ok {
		g.nodeMu.RUnlock()
		return nil
	}
	out := make([]string, 0, len(bag))
	for lid := range bag {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	}
	g.nodeMu.RUnlock()
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
	g.edgeMu.Lock()
	k := edgeKey{src: srcID, dst: dstID}
	bag, ok := g.edgeBag[k]
	if !ok {
		bag = make(map[LabelID]struct{})
		g.edgeBag[k] = bag
	}
	bag[lid] = struct{}{}
	g.edgeMu.Unlock()
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
	g.edgeMu.RLock()
	defer g.edgeMu.RUnlock()
	bag, ok := g.edgeBag[edgeKey{src: srcID, dst: dstID}]
	if !ok {
		return false
	}
	_, ok = bag[lid]
	return ok
}

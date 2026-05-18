package search

import (
	"errors"
	"reflect"
	"sync"

	"gograph/graph"
	"gograph/graph/csr"
)

// reflectTypeOf is named so it can be inlined / monkey-patched in
// tests without dragging the reflect import into the public path.
func reflectTypeOf(v any) reflect.Type { return reflect.TypeOf(v) }

// Weight is the constraint accepted by weighted algorithms such as
// [Dijkstra]. It permits all built-in numeric types and their named
// derivatives. Comparisons rely on the standard ordering.
type Weight interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64
}

// ErrNegativeWeight is returned by algorithms that require non-negative
// edge weights (such as Dijkstra) when the input graph contains an
// edge with weight strictly less than the zero value of W.
var ErrNegativeWeight = errors.New("search: negative edge weight")

// Distances is the result of a single-source shortest-path query.
// It exposes constant-time distance lookup and parent-chain path
// reconstruction.
//
// Distances is safe for concurrent reads.
type Distances[W Weight] struct {
	dist   []W
	parent []graph.NodeID
	found  []bool
	src    graph.NodeID
}

// Source returns the source NodeID the distances are anchored to.
func (d *Distances[W]) Source() graph.NodeID { return d.src }

// Distance returns the cost of the shortest path from the source to
// node and true if node is reachable, or the zero value of W and
// false otherwise.
func (d *Distances[W]) Distance(node graph.NodeID) (W, bool) {
	id := uint64(node)
	if id >= uint64(len(d.found)) || !d.found[id] {
		var zero W
		return zero, false
	}
	return d.dist[id], true
}

// Path reconstructs the shortest path from the source to node by
// walking the parent chain. It returns nil when node is unreachable
// from the source, and a single-element slice {src} when node == src.
// The returned slice is freshly allocated; the caller owns it.
func (d *Distances[W]) Path(node graph.NodeID) []graph.NodeID {
	id := uint64(node)
	if id >= uint64(len(d.found)) || !d.found[id] {
		return nil
	}
	// Determine the length first.
	length := 1
	for cur := node; cur != d.src; {
		cur = d.parent[uint64(cur)]
		length++
	}
	out := make([]graph.NodeID, length)
	cur := node
	for i := length - 1; i > 0; i-- {
		out[i] = cur
		cur = d.parent[uint64(cur)]
	}
	out[0] = d.src
	return out
}

// dijkHeap is a min-heap of (node, dist) pairs used by [Dijkstra].
// Storing both fields inline avoids pointer chasing; the slice is
// reused across calls via [sync.Pool].
type dijkHeap[W Weight] struct {
	items []dijkItem[W]
}

type dijkItem[W Weight] struct {
	dist W
	node graph.NodeID
}

func (h *dijkHeap[W]) push(d W, n graph.NodeID) {
	h.items = append(h.items, dijkItem[W]{dist: d, node: n})
	i := len(h.items) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if h.items[parent].dist <= h.items[i].dist {
			break
		}
		h.items[parent], h.items[i] = h.items[i], h.items[parent]
		i = parent
	}
}

func (h *dijkHeap[W]) pop() dijkItem[W] {
	top := h.items[0]
	last := len(h.items) - 1
	h.items[0] = h.items[last]
	h.items = h.items[:last]
	if len(h.items) == 0 {
		return top
	}
	i := 0
	for {
		left := 2*i + 1
		right := 2*i + 2
		smallest := i
		if left < len(h.items) && h.items[left].dist < h.items[smallest].dist {
			smallest = left
		}
		if right < len(h.items) && h.items[right].dist < h.items[smallest].dist {
			smallest = right
		}
		if smallest == i {
			break
		}
		h.items[smallest], h.items[i] = h.items[i], h.items[smallest]
		i = smallest
	}
	return top
}

func (h *dijkHeap[W]) len() int { return len(h.items) }

// dijkstraState bundles the per-call working storage. A separate pool
// is kept per (W) instantiation by the generics machinery, so the
// pooled object is always of the correct concrete type.
type dijkstraState[W Weight] struct {
	dist   []W
	parent []graph.NodeID
	found  []bool
	heap   dijkHeap[W]
}

// Dijkstra computes single-source shortest paths in c starting at src
// over non-negative edge weights. It returns [ErrNegativeWeight]
// (without performing any traversal) when any weight in c is strictly
// less than the zero value of W.
//
// The implementation uses a classic binary-heap priority queue.
// Working storage is pooled across calls so steady-state workloads
// reach zero allocations per inner-loop iteration.
func Dijkstra[W Weight](c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	weights := c.WeightsSlice()
	var zero W
	for _, w := range weights {
		if w < zero {
			return nil, ErrNegativeWeight
		}
	}

	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)

	for i := range st.dist {
		st.dist[i] = zero
		st.parent[i] = 0
		st.found[i] = false
	}

	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) {
		out := newDistancesCopy(st, src, maxID)
		return out, nil
	}

	st.found[uint64(src)] = true
	st.parent[uint64(src)] = src
	st.heap.push(zero, src)

	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	for st.heap.len() > 0 {
		top := st.heap.pop()
		if top.dist != st.dist[uint64(top.node)] && st.found[uint64(top.node)] {
			// Stale entry — a shorter path was already accepted.
			if top.dist > st.dist[uint64(top.node)] {
				continue
			}
		}
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			cand := top.dist + weights[k]
			if !st.found[uint64(nb)] || cand < st.dist[uint64(nb)] {
				st.dist[uint64(nb)] = cand
				st.parent[uint64(nb)] = top.node
				st.found[uint64(nb)] = true
				st.heap.push(cand, nb)
			}
		}
	}

	return newDistancesCopy(st, src, maxID), nil
}

// newDistancesCopy materialises a stable Distances value, copying the
// pooled state so the caller is decoupled from the pool's lifecycle.
func newDistancesCopy[W Weight](st *dijkstraState[W], src graph.NodeID, maxID uint64) *Distances[W] {
	out := &Distances[W]{
		src: src,
	}
	out.dist = make([]W, maxID)
	copy(out.dist, st.dist[:maxID])
	out.parent = make([]graph.NodeID, maxID)
	copy(out.parent, st.parent[:maxID])
	out.found = make([]bool, maxID)
	copy(out.found, st.found[:maxID])
	return out
}

func acquireDijkstra[W Weight](maxID uint64) *dijkstraState[W] {
	st, _ := dijkstraPool[W]().Get().(*dijkstraState[W])
	if st == nil {
		st = &dijkstraState[W]{}
	}
	if uint64(cap(st.dist)) < maxID {
		st.dist = make([]W, maxID)
		st.parent = make([]graph.NodeID, maxID)
		st.found = make([]bool, maxID)
	} else {
		st.dist = st.dist[:maxID]
		st.parent = st.parent[:maxID]
		st.found = st.found[:maxID]
	}
	st.heap.items = st.heap.items[:0]
	return st
}

func releaseDijkstra[W Weight](st *dijkstraState[W]) {
	dijkstraPool[W]().Put(st)
}

// dijkstraPools holds the per-W [sync.Pool] for [dijkstraState]
// objects, keyed by the reflect.Type of the zero value of W. Using a
// reflection key keeps each generic instantiation cleanly separated
// without resorting to unsafe per-type globals.
var dijkstraPools sync.Map //nolint:gochecknoglobals // per-package pool registry

func dijkstraPool[W Weight]() *sync.Pool {
	var zero W
	key := reflectTypeOf(zero)
	if v, ok := dijkstraPools.Load(key); ok {
		return v.(*sync.Pool) //nolint:errcheck // statically known type
	}
	p := &sync.Pool{New: func() any { return &dijkstraState[W]{} }}
	actual, _ := dijkstraPools.LoadOrStore(key, p)
	return actual.(*sync.Pool) //nolint:errcheck // statically known type
}

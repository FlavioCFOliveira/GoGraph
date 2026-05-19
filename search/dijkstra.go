package search

import (
	"context"
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

// ErrBufferTooSmall is returned by the *Into variants when any of the
// caller-provided scratch slices is shorter than c.MaxNodeID().
var ErrBufferTooSmall = errors.New("search: caller-provided buffer is too small")

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
//
// For hot loops where the caller can amortise buffer allocation
// (e.g. Yen's k-shortest-paths), prefer the zero-allocation primitive
// [DijkstraInto].
func Dijkstra[W Weight](c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	return DijkstraCtx(context.Background(), c, src)
}

// DijkstraCtx is the context-aware variant of [Dijkstra]. ctx.Err()
// is checked every 4096 heap pops; on cancellation it returns
// (nil, wrapped ctx.Err()).
func DijkstraCtx[W Weight](ctx context.Context, c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)

	if err := dijkstraCore[W](ctx, c, src, st.dist[:maxID], st.parent[:maxID], st.found[:maxID], &st.heap); err != nil {
		return nil, err
	}
	return newDistancesCopy(st, src, maxID), nil
}

// DijkstraInto is the zero-allocation primitive behind [Dijkstra]. It
// writes single-source shortest-path results directly into the
// caller-provided dist, parent, and found slices, each of which must
// have length at least c.MaxNodeID(); otherwise it returns
// [ErrBufferTooSmall]. The slices are reset in-place before the
// traversal, so any previous contents are overwritten.
//
// On return, dist[i] holds the cost from src to node i when found[i]
// is true and parent[i] is the predecessor on the shortest path
// (with parent[src] = src by convention). When found[i] is false,
// dist[i] is the zero value of W and parent[i] is zero.
//
// The only heap allocation performed is the priority queue itself,
// which is obtained from a per-W [sync.Pool] and is therefore zero
// in the steady state.
//
// Concurrency: the caller's slices are written in-place; concurrent
// callers must supply separate buffers. The internal heap pool is
// safe for concurrent acquisition.
func DijkstraInto[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src graph.NodeID,
	dist []W,
	parent []graph.NodeID,
	found []bool,
) error {
	maxID := uint64(c.MaxNodeID())
	if uint64(len(dist)) < maxID || uint64(len(parent)) < maxID || uint64(len(found)) < maxID {
		return ErrBufferTooSmall
	}
	h := acquireDijkHeap[W]()
	defer releaseDijkHeap(h)
	return dijkstraCore[W](ctx, c, src, dist[:maxID], parent[:maxID], found[:maxID], h)
}

// dijkstraCore is the shared traversal body invoked by both
// [DijkstraCtx] and [DijkstraInto]. Pre-conditions:
//   - len(dist), len(parent), len(found) all equal c.MaxNodeID();
//   - heap has been reset to empty.
//
//nolint:gocyclo // canonical Dijkstra: negative-weight scan + heap loop + relaxation
func dijkstraCore[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	src graph.NodeID,
	dist []W,
	parent []graph.NodeID,
	found []bool,
	h *dijkHeap[W],
) error {
	weights := c.WeightsSlice()
	var zero W
	for _, w := range weights {
		if w < zero {
			return ErrNegativeWeight
		}
	}

	for i := range dist {
		dist[i] = zero
		parent[i] = 0
		found[i] = false
	}
	h.items = h.items[:0]

	verts := c.VerticesSlice()
	if uint64(src)+1 >= uint64(len(verts)) {
		return nil
	}
	edges := c.EdgesSlice()

	found[uint64(src)] = true
	parent[uint64(src)] = src
	h.push(zero, src)

	popCount := 0
	for h.len() > 0 {
		if popCount&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		popCount++
		top := h.pop()
		if top.dist != dist[uint64(top.node)] && found[uint64(top.node)] {
			if top.dist > dist[uint64(top.node)] {
				continue
			}
		}
		start := verts[uint64(top.node)]
		end := verts[uint64(top.node)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			cand := top.dist + weights[k]
			if !found[uint64(nb)] || cand < dist[uint64(nb)] {
				dist[uint64(nb)] = cand
				parent[uint64(nb)] = top.node
				found[uint64(nb)] = true
				h.push(cand, nb)
			}
		}
	}
	return nil
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

// dijkHeapPools is the per-W heap-only pool used by [DijkstraInto]
// and other *Into entrypoints that operate on caller-provided
// buffers. Kept separate from [dijkstraPools] so that Into callers
// don't pay for buffer allocations they already own.
var dijkHeapPools sync.Map //nolint:gochecknoglobals // per-package heap pool

func dijkHeapPool[W Weight]() *sync.Pool {
	var zero W
	key := reflectTypeOf(zero)
	if v, ok := dijkHeapPools.Load(key); ok {
		return v.(*sync.Pool) //nolint:errcheck // statically known type
	}
	p := &sync.Pool{New: func() any { return &dijkHeap[W]{} }}
	actual, _ := dijkHeapPools.LoadOrStore(key, p)
	return actual.(*sync.Pool) //nolint:errcheck // statically known type
}

func acquireDijkHeap[W Weight]() *dijkHeap[W] {
	h, _ := dijkHeapPool[W]().Get().(*dijkHeap[W])
	if h == nil {
		h = &dijkHeap[W]{}
	}
	h.items = h.items[:0]
	return h
}

func releaseDijkHeap[W Weight](h *dijkHeap[W]) {
	dijkHeapPool[W]().Put(h)
}

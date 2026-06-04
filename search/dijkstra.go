package search

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
// less than the zero value of W. For floating-point Weight types it
// validates that no edge weight is NaN or +/-Inf and returns
// [ErrInvalidInput] otherwise; integer Weight types skip that pass.
//
// The implementation uses a classic binary-heap priority queue.
// Working storage is pooled across calls so steady-state workloads
// reach zero allocations per inner-loop iteration.
//
// Integer-Weight overflow precondition. The cumulative distance is
// accumulated in W's own arithmetic with no overflow guard on the hot
// path. For an integer Weight type the caller must ensure that the
// cumulative weight of the longest shortest path explored fits W;
// otherwise the addition wraps and the relaxation comparison yields a
// silently incorrect path. The NaN/+-Inf gate above covers only
// floating-point W. A development build with -tags gograph_debug adds
// an assertion to [BellmanFord] and [JohnsonAPSP] that panics on such a
// wraparound; the production hot loop carries no such check.
//
// For hot loops where the caller can amortise buffer allocation
// (e.g. Yen's k-shortest-paths), prefer the zero-allocation primitive
// [DijkstraInto].
func Dijkstra[W Weight](c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	defer metrics.Time("search.Dijkstra")()
	res, err := DijkstraCtx(context.Background(), c, src)
	if err != nil {
		metrics.IncCounter("search.Dijkstra.errors", 1)
	}
	return res, err
}

// DijkstraCtx is the context-aware variant of [Dijkstra]. ctx.Err()
// is checked every 4096 heap pops; on cancellation it returns
// (nil, wrapped ctx.Err()).
func DijkstraCtx[W Weight](ctx context.Context, c *csr.CSR[W], src graph.NodeID) (*Distances[W], error) {
	defer metrics.Time("search.DijkstraCtx")()
	maxID := uint64(c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)

	if err := dijkstraCore[W](ctx, c, src, st.dist[:maxID], st.parent[:maxID], st.found[:maxID], &st.heap); err != nil {
		metrics.IncCounter("search.DijkstraCtx.errors", 1)
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
	defer metrics.Time("search.DijkstraInto")()
	maxID := uint64(c.MaxNodeID())
	if uint64(len(dist)) < maxID || uint64(len(parent)) < maxID || uint64(len(found)) < maxID {
		metrics.IncCounter("search.DijkstraInto.errors", 1)
		return ErrBufferTooSmall
	}
	h := acquireDijkHeap[W]()
	defer releaseDijkHeap(h)
	err := dijkstraCore[W](ctx, c, src, dist[:maxID], parent[:maxID], found[:maxID], h)
	if err != nil {
		metrics.IncCounter("search.DijkstraInto.errors", 1)
	}
	return err
}

// dijkstraCore is the shared traversal body invoked by both
// [DijkstraCtx] and [DijkstraInto]. Pre-conditions:
//   - len(dist), len(parent), len(found) all equal c.MaxNodeID();
//   - heap has been reset to empty.
//
//nolint:gocyclo // canonical Dijkstra: NaN/Inf gate + negative-weight scan + heap loop + relaxation
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
	// Float Weight types: NaN / +/-Inf in an edge weight silently
	// drops every relaxation (cand<dist is always false against NaN).
	// Fail fast at the public boundary; integer W short-circuits.
	if anyFloatInvalid(weights) {
		return ErrInvalidInput
	}
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

// dijkstraCoreWithWeights mirrors [dijkstraCore] but reads edge
// weights from a caller-provided slice instead of c.WeightsSlice(),
// so the caller can run Dijkstra over a reweighted view of the same
// CSR (Johnson's algorithm builds such a view after the Bellman-Ford
// pass).
//
// Pre-conditions:
//   - len(weights) >= len(c.EdgesSlice());
//   - every weights[k] is non-negative (i.e. >= W's zero value);
//   - len(dist), len(parent), len(found) all equal c.MaxNodeID();
//   - h has been reset to empty.
//
// Concurrency: identical to [dijkstraCore] — the caller's slices are
// written in place; concurrent callers must supply separate buffers.
//
//nolint:gocyclo // mirrors dijkstraCore body
func dijkstraCoreWithWeights[W Weight](
	ctx context.Context,
	c *csr.CSR[W],
	weights []W,
	src graph.NodeID,
	dist []W,
	parent []graph.NodeID,
	found []bool,
	h *dijkHeap[W],
) error {
	var zero W
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
			// Debug builds (-tags gograph_debug) trap an integer
			// cumulative-distance overflow in Johnson's reweighted
			// inner Dijkstra here; a no-op otherwise. (The plain
			// dijkstraCore path is intentionally not instrumented; see
			// its godoc precondition.)
			assertNoRelaxOverflow(top.dist, weights[k], cand)
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
	metrics.IncCounter("search.pool.dijkstra.get", 1)
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
	metrics.IncCounter("search.pool.dijkstra.put", 1)
	dijkstraPool[W]().Put(st)
}

// Per-base-type pools for [dijkstraState]. The Weight interface
// admits the thirteen built-in numeric base types plus any defined
// type whose underlying type is one of them; the type switch in
// [dijkstraPool] dispatches to the corresponding package-level pool
// in <5ns, with a reflection-keyed fallback for defined types.
//
//nolint:gochecknoglobals // per-W pool registry; immutable after init
var (
	dijkstraPoolInt     = &sync.Pool{New: func() any { return &dijkstraState[int]{} }}
	dijkstraPoolInt8    = &sync.Pool{New: func() any { return &dijkstraState[int8]{} }}
	dijkstraPoolInt16   = &sync.Pool{New: func() any { return &dijkstraState[int16]{} }}
	dijkstraPoolInt32   = &sync.Pool{New: func() any { return &dijkstraState[int32]{} }}
	dijkstraPoolInt64   = &sync.Pool{New: func() any { return &dijkstraState[int64]{} }}
	dijkstraPoolUint    = &sync.Pool{New: func() any { return &dijkstraState[uint]{} }}
	dijkstraPoolUint8   = &sync.Pool{New: func() any { return &dijkstraState[uint8]{} }}
	dijkstraPoolUint16  = &sync.Pool{New: func() any { return &dijkstraState[uint16]{} }}
	dijkstraPoolUint32  = &sync.Pool{New: func() any { return &dijkstraState[uint32]{} }}
	dijkstraPoolUint64  = &sync.Pool{New: func() any { return &dijkstraState[uint64]{} }}
	dijkstraPoolUintptr = &sync.Pool{New: func() any { return &dijkstraState[uintptr]{} }}
	dijkstraPoolFloat32 = &sync.Pool{New: func() any { return &dijkstraState[float32]{} }}
	dijkstraPoolFloat64 = &sync.Pool{New: func() any { return &dijkstraState[float64]{} }}
)

// dijkstraPoolsByType holds the per-W [sync.Pool] for [dijkstraState]
// objects when W is a defined type whose underlying type matches one
// of the base Weight types. Used only as a fallback; the base-type
// cases dispatch through [dijkstraPool]'s type switch first.
//
//nolint:gochecknoglobals // fallback registry; one entry per defined Weight type
var dijkstraPools sync.Map

// dijkstraPool returns the [sync.Pool] of [dijkstraState] values for
// the type parameter W. The fast path is an inlined type switch over
// the thirteen built-in Weight base types; defined Weight types
// (e.g. type Distance int64) fall through to a reflect-keyed
// sync.Map registry.
func dijkstraPool[W Weight]() *sync.Pool {
	var zero W
	switch any(zero).(type) {
	case int:
		return dijkstraPoolInt
	case int8:
		return dijkstraPoolInt8
	case int16:
		return dijkstraPoolInt16
	case int32:
		return dijkstraPoolInt32
	case int64:
		return dijkstraPoolInt64
	case uint:
		return dijkstraPoolUint
	case uint8:
		return dijkstraPoolUint8
	case uint16:
		return dijkstraPoolUint16
	case uint32:
		return dijkstraPoolUint32
	case uint64:
		return dijkstraPoolUint64
	case uintptr:
		return dijkstraPoolUintptr
	case float32:
		return dijkstraPoolFloat32
	case float64:
		return dijkstraPoolFloat64
	}
	return dijkstraPoolReflect[W]()
}

func dijkstraPoolReflect[W Weight]() *sync.Pool {
	var zero W
	key := reflectTypeOf(zero)
	if v, ok := dijkstraPools.Load(key); ok {
		return v.(*sync.Pool) //nolint:errcheck // statically known type
	}
	p := &sync.Pool{New: func() any { return &dijkstraState[W]{} }}
	actual, _ := dijkstraPools.LoadOrStore(key, p)
	return actual.(*sync.Pool) //nolint:errcheck // statically known type
}

// Per-base-type pools for the heap-only acquire used by [DijkstraInto]
// and other *Into entrypoints; mirror layout of the dijkstraState
// pools above.
//
//nolint:gochecknoglobals // per-W heap pool registry; immutable after init
var (
	dijkHeapPoolInt     = &sync.Pool{New: func() any { return &dijkHeap[int]{} }}
	dijkHeapPoolInt8    = &sync.Pool{New: func() any { return &dijkHeap[int8]{} }}
	dijkHeapPoolInt16   = &sync.Pool{New: func() any { return &dijkHeap[int16]{} }}
	dijkHeapPoolInt32   = &sync.Pool{New: func() any { return &dijkHeap[int32]{} }}
	dijkHeapPoolInt64   = &sync.Pool{New: func() any { return &dijkHeap[int64]{} }}
	dijkHeapPoolUint    = &sync.Pool{New: func() any { return &dijkHeap[uint]{} }}
	dijkHeapPoolUint8   = &sync.Pool{New: func() any { return &dijkHeap[uint8]{} }}
	dijkHeapPoolUint16  = &sync.Pool{New: func() any { return &dijkHeap[uint16]{} }}
	dijkHeapPoolUint32  = &sync.Pool{New: func() any { return &dijkHeap[uint32]{} }}
	dijkHeapPoolUint64  = &sync.Pool{New: func() any { return &dijkHeap[uint64]{} }}
	dijkHeapPoolUintptr = &sync.Pool{New: func() any { return &dijkHeap[uintptr]{} }}
	dijkHeapPoolFloat32 = &sync.Pool{New: func() any { return &dijkHeap[float32]{} }}
	dijkHeapPoolFloat64 = &sync.Pool{New: func() any { return &dijkHeap[float64]{} }}
)

//nolint:gochecknoglobals // fallback heap-pool registry
var dijkHeapPools sync.Map

func dijkHeapPool[W Weight]() *sync.Pool {
	var zero W
	switch any(zero).(type) {
	case int:
		return dijkHeapPoolInt
	case int8:
		return dijkHeapPoolInt8
	case int16:
		return dijkHeapPoolInt16
	case int32:
		return dijkHeapPoolInt32
	case int64:
		return dijkHeapPoolInt64
	case uint:
		return dijkHeapPoolUint
	case uint8:
		return dijkHeapPoolUint8
	case uint16:
		return dijkHeapPoolUint16
	case uint32:
		return dijkHeapPoolUint32
	case uint64:
		return dijkHeapPoolUint64
	case uintptr:
		return dijkHeapPoolUintptr
	case float32:
		return dijkHeapPoolFloat32
	case float64:
		return dijkHeapPoolFloat64
	}
	return dijkHeapPoolReflect[W]()
}

func dijkHeapPoolReflect[W Weight]() *sync.Pool {
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

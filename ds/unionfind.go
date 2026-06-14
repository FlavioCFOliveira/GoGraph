// Package ds provides small generic data-structure primitives that
// support gograph's algorithms but do not themselves model a graph.
package ds

// UnionFind is a disjoint-set (union-find) data structure with path
// compression and union by rank. Both operations run in
// O(alpha(n)) amortised time — effectively constant for any
// practical input size.
//
// UnionFind is generic over a comparable element type T. Elements
// are added implicitly on first reference; the structure grows as
// needed.
//
// UnionFind is not safe for concurrent use; callers that need
// concurrent access must guard it externally.
type UnionFind[T comparable] struct {
	parent map[T]T
	rank   map[T]int
}

// New returns an empty UnionFind.
func New[T comparable]() *UnionFind[T] {
	return &UnionFind[T]{
		parent: make(map[T]T),
		rank:   make(map[T]int),
	}
}

// MakeSet records x as a singleton set if it is not already known.
// It is idempotent.
func (u *UnionFind[T]) MakeSet(x T) {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
		u.rank[x] = 0
	}
}

// Find returns the representative of the set containing x, adding x
// as its own singleton set if it is not already known. The lookup
// path is compressed so subsequent Find calls run in near-constant
// time.
func (u *UnionFind[T]) Find(x T) T {
	if _, ok := u.parent[x]; !ok {
		u.MakeSet(x)
		return x
	}
	// Two-pass iterative path compression.
	root := x
	for u.parent[root] != root {
		root = u.parent[root]
	}
	for u.parent[x] != root {
		next := u.parent[x]
		u.parent[x] = root
		x = next
	}
	return root
}

// Union merges the sets containing a and b. Returns true when the
// two were in distinct sets (i.e., a merge actually occurred).
func (u *UnionFind[T]) Union(a, b T) bool {
	ra := u.Find(a)
	rb := u.Find(b)
	if ra == rb {
		return false
	}
	if u.rank[ra] < u.rank[rb] {
		u.parent[ra] = rb
	} else if u.rank[ra] > u.rank[rb] {
		u.parent[rb] = ra
	} else {
		u.parent[rb] = ra
		u.rank[ra]++
	}
	return true
}

// Connected reports whether a and b are in the same set. Elements
// not yet known are treated as singletons.
func (u *UnionFind[T]) Connected(a, b T) bool {
	return u.Find(a) == u.Find(b)
}

// Len returns the number of elements ever introduced to the
// structure.
func (u *UnionFind[T]) Len() int { return len(u.parent) }

// Reset returns the structure to its empty state, retaining the
// backing maps so callers that pool *UnionFind across many short
// queries (kruskal MST, connected-components, Tarjan SCC under a
// soak load) can avoid the per-call map allocation. The two maps
// are cleared with Go 1.21's builtin clear() which preserves the
// underlying bucket array — subsequent inserts reuse capacity.
//
// After Reset, the UnionFind behaves identically to a freshly
// constructed instance via [New].
func (u *UnionFind[T]) Reset() {
	clear(u.parent)
	clear(u.rank)
}

// UnionFindSlice is a slice-backed disjoint-set structure for a
// bounded integer ID space [0, n). It offers the same amortised
// O(alpha(n)) Find/Union complexity as [UnionFind] but trades the
// generic map storage for two contiguous slices, gaining ~5-10x
// faster operations on typical Kruskal-MST workloads where the
// element set is densely packed in a known range.
//
// The parent slice is widened to the platform int (64-bit on every
// 64-bit target) so the element-ID domain matches the int domain of
// the graph NodeID / MaxNodeID callers feed into [NewSlice]
// (search.WCC, search.KruskalMST). An earlier int32 backing silently
// truncated element IDs once the universe exceeded math.MaxInt32,
// wrapping high IDs to negative slice indices and corrupting set
// membership (rmp #1476); the int backing removes that ceiling
// entirely. Correctness is preferred over the ~2x density int32 would
// save, and only on universes already near the 2^31 boundary.
//
// UnionFindSlice is not safe for concurrent use; callers that need
// concurrent access must guard it externally.
type UnionFindSlice struct {
	parent []int
	rank   []uint8
}

// NewSlice returns a UnionFindSlice covering elements [0, n). Each
// element starts in its own singleton set.
//
// n is the universe size in the platform int domain; the parent slice
// is indexed by int throughout, so any n that a Go slice can hold is
// supported without truncation. Passing a negative n panics in make,
// matching every other slice constructor in the standard library.
func NewSlice(n int) *UnionFindSlice {
	u := &UnionFindSlice{
		parent: make([]int, n),
		rank:   make([]uint8, n),
	}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

// Find returns the representative of the set containing x with
// two-pass path compression. x must be in [0, len(parent)).
func (u *UnionFindSlice) Find(x int) int {
	root := x
	for u.parent[root] != root {
		root = u.parent[root]
	}
	cur := x
	for u.parent[cur] != root {
		next := u.parent[cur]
		u.parent[cur] = root
		cur = next
	}
	return root
}

// Union merges the sets containing a and b. Returns true when the
// two were in distinct sets (i.e., a merge actually occurred).
func (u *UnionFindSlice) Union(a, b int) bool {
	ra := u.Find(a)
	rb := u.Find(b)
	if ra == rb {
		return false
	}
	switch {
	case u.rank[ra] < u.rank[rb]:
		u.parent[ra] = rb
	case u.rank[ra] > u.rank[rb]:
		u.parent[rb] = ra
	default:
		u.parent[rb] = ra
		u.rank[ra]++
	}
	return true
}

// Connected reports whether a and b are in the same set.
func (u *UnionFindSlice) Connected(a, b int) bool {
	return u.Find(a) == u.Find(b)
}

// Len returns the size of the universe (n at construction time).
func (u *UnionFindSlice) Len() int { return len(u.parent) }

// Reset returns every element of u to its own singleton set,
// preserving the slice capacity for callers that pool
// *UnionFindSlice across many short queries. The new universe size
// stays at len(parent); use [NewSlice] when the universe size must
// change. Rank is zeroed via a single clear() call.
func (u *UnionFindSlice) Reset() {
	for i := range u.parent {
		u.parent[i] = i
	}
	clear(u.rank)
}

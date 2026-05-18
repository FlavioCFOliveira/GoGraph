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

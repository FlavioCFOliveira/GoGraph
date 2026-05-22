// Package graph defines the core types and interfaces shared by every
// backend in the gograph module.
//
// The package establishes a small set of contracts:
//
//   - [NodeID] is the compact internal identifier used by all storage
//     backends. It is an opaque alias of uint64.
//   - [Mapper] interns arbitrary comparable user-facing identifiers to
//     stable NodeID values, keeping the public API generic while the
//     storage stays cache-friendly.
//   - [Graph] is the minimal directed-graph contract every backend
//     implements. Specialised interfaces (undirected, weighted,
//     property graphs) extend it in dedicated subpackages.
//
// Concurrency. The interfaces declared here do not, on their own,
// prescribe a concurrency model: each implementation documents its own
// contract on its concrete types. As a rule of thumb, gograph
// implementations are safe for concurrent reads on an immutable
// snapshot, and serialise writes per backend.
package graph

import "iter"

// NodeID is the compact internal identifier assigned by a [Mapper] to a
// user-facing node value. It is an opaque uint64; callers must not
// interpret its bit layout, which is an implementation detail of the
// Mapper that produced it.
//
// The zero value of NodeID is a valid identifier (the first node a
// mapper interns); use the bool returned by [Mapper.Resolve] to
// distinguish "unknown" from "first node".
type NodeID uint64

// Graph is the minimal directed-graph contract.
//
// Implementations choose their own storage layout (adjacency list, CSR,
// mmap'd file, …) and document their own concurrency contract; this
// interface only fixes the shape callers see.
//
// N is the user-facing node type (any comparable type). W is the edge
// weight type; use struct{} for unweighted graphs.
type Graph[N comparable, W any] interface {
	// Order returns the number of distinct nodes currently in the graph.
	Order() uint64

	// Size returns the number of edges currently in the graph. For
	// multigraphs, parallel edges count separately.
	Size() uint64

	// Neighbours returns an iterator over the out-neighbours of n and
	// the weight of each connecting edge. The iterator yields no values
	// when n is not present in the graph. The order in which neighbours
	// are visited is implementation-defined.
	Neighbours(n N) iter.Seq2[N, W]

	// HasEdge reports whether an edge from src to dst is present.
	// It returns false when either endpoint is unknown.
	HasEdge(src, dst N) bool

	// AddNode inserts n if not already present. It is a no-op when n is
	// already known. Implementations return an error when a bounded
	// resource (for example, a sharded slot array with an explicit
	// upper bound) is exhausted; callers must handle that error and
	// stop offering further work on the affected shard.
	AddNode(n N) error

	// AddEdge inserts a directed edge from src to dst with weight w,
	// adding the endpoints if they are not yet present. Multigraph
	// implementations accept parallel edges; simple implementations
	// document their de-duplication policy. Implementations return an
	// error when a bounded resource is exhausted (see [Graph.AddNode]);
	// no edge is published in that case.
	AddEdge(src, dst N, w W) error

	// RemoveEdge removes the directed edge from src to dst if present.
	// It is a no-op when no such edge exists. The endpoints remain in
	// the graph.
	RemoveEdge(src, dst N)
}

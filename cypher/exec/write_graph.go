package exec

// write_graph.go — graphMutator interface used by all write operators.
//
// graphMutator is the minimal write contract the Cypher executor requires from
// the underlying LPG. Using an interface (rather than a concrete
// *lpg.Graph[string,float64]) keeps each write operator testable with simple
// in-process stubs and avoids coupling the exec package to the lpg generic
// instantiation.
//
// The concrete binding (*lpg.Graph[string, float64]) is provided by
// cypher/api.go via the lpgMutatorAdapter type.

import (
	"gograph/graph"
	"gograph/graph/lpg"
)

// GraphMutator is the write surface exposed to Cypher write operators.
//
// All methods accept the user-facing node key (string) used by the
// lpg.Graph[string,float64] instantiation. The graph is responsible for
// interning the value and returning the stable internal NodeID where
// applicable.
//
// GraphMutator is NOT safe for concurrent use from multiple goroutines; each
// physical operator tree owns exactly one instance.
type GraphMutator interface {
	// AddNode interns n and returns its stable NodeID. Returns the
	// error from the underlying graph implementation (currently only
	// [adjlist.ErrShardFull] is reachable, and only when the
	// underlying [adjlist.Config.MaxShardCapacity] is set).
	AddNode(n string) (graph.NodeID, error)

	// AddEdge inserts a directed edge (src→dst) with weight w, interning
	// endpoints as needed. Returns the stable NodeIDs of src and dst and
	// any error from the underlying graph implementation.
	AddEdge(src, dst string, w float64) (srcID, dstID graph.NodeID, err error)

	// RemoveEdge removes the directed edge from src to dst (no-op if absent).
	RemoveEdge(src, dst string)

	// SetNodeLabel attaches label to n (inserting n if absent). Returns
	// any error from the underlying [adjlist.AdjList.AddNode] (see
	// [GraphMutator.AddNode]).
	SetNodeLabel(n, label string) error

	// RemoveNodeLabel detaches label from n (no-op if absent).
	RemoveNodeLabel(n, label string)

	// RemoveNode tombstones n in the underlying graph so subsequent reads
	// (AllNodesScan, count(*), Order) treat the node as absent. Callers
	// should strip labels/properties/incident edges before invoking
	// RemoveNode so the tombstone reflects the fully-deleted state.
	RemoveNode(n string)

	// IsTombstoned reports whether the NodeID has been tombstoned. Used by
	// AllNodesScan to skip phantom entries the Mapper still indexes.
	IsTombstoned(id graph.NodeID) bool

	// SetNodeProperty sets the named property on n. Returns any error
	// from the underlying [adjlist.AdjList.AddNode] (see
	// [GraphMutator.AddNode]).
	SetNodeProperty(n, key string, value lpg.PropertyValue) error

	// DelNodeProperty removes the named property from n (no-op if absent).
	DelNodeProperty(n, key string)

	// NodeProperties returns a snapshot of all properties currently on n.
	NodeProperties(n string) map[string]lpg.PropertyValue

	// NodeLabels returns a snapshot of all labels currently on n in
	// unspecified order.
	NodeLabels(n string) []string

	// HasEdge reports whether a directed edge from src to dst is present.
	HasEdge(src, dst string) bool

	// SetEdgeLabel attaches label to the directed edge (src, dst).
	SetEdgeLabel(src, dst, label string)

	// SetEdgeProperty sets the named property on the directed edge (src, dst).
	// Returns any error from the underlying graph (e.g. schema violation).
	SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error

	// DelEdgeProperty removes the named property from the directed edge
	// (src, dst) (no-op if absent).
	DelEdgeProperty(src, dst, key string)

	// EdgeProperties returns a snapshot of every property currently set on
	// the directed edge (src, dst). Returns an empty map when the edge has
	// no properties or does not exist.
	EdgeProperties(src, dst string) map[string]lpg.PropertyValue

	// EdgeLabels returns a snapshot of every label currently attached to
	// the directed edge (src, dst). Returns an empty slice when the edge
	// has no labels or does not exist. Used by DELETE r to capture the
	// relationship type before tombstoning the edge so the row's
	// post-delete RelationshipValue keeps `RETURN type(r)` working.
	EdgeLabels(src, dst string) []string

	// IncEdgeCreateCount bumps the Cypher CREATE-call multiplicity
	// counter for the directed edge (src, dst) by one and returns the
	// new (1-based) value. The counter records how many CREATE
	// statements have targeted the same endpoint pair regardless of
	// whether the underlying storage already had an entry — MERGE
	// consults it to emit multiplicity rows when an existing edge
	// satisfies the merge pattern (Merge5 [3]). The returned index is
	// the per-instance idx callers pass to the *At family of
	// metadata-write helpers.
	IncEdgeCreateCount(src, dst string) int64
	// EdgeCreateCount returns the current CREATE-call multiplicity
	// counter for the directed edge (src, dst), or 0 when no CREATE
	// has been recorded.
	EdgeCreateCount(src, dst string) int64
	// DecEdgeCreateCount decrements the CREATE-call multiplicity
	// counter (floor 0). Called by DELETE so subsequent MERGEs see
	// the correct multiplicity.
	DecEdgeCreateCount(src, dst string)
	// SetEdgeLabelAt attaches `label` to the directed edge instance
	// (src, dst) at the supplied 1-based CREATE index. Used by
	// CreateRelationship so parallel CREATEs of the same endpoint
	// pair retain their distinct labels (Match2 [6] / Match7 [29]).
	SetEdgeLabelAt(src, dst string, idx int64, label string)
	// EdgeLabelsAt returns the labels recorded at instance `idx` of
	// the directed edge (src, dst), or nil when the instance has no
	// per-CREATE labels.
	EdgeLabelsAt(src, dst string, idx int64) []string
	// SetEdgePropertyAt records `key`=`value` on the directed edge
	// instance (src, dst) at the supplied 1-based CREATE index.
	SetEdgePropertyAt(src, dst string, idx int64, key string, value lpg.PropertyValue)
	// EdgePropertiesAt returns the property map recorded at instance
	// `idx` of the directed edge (src, dst), or nil when no
	// per-CREATE map was captured.
	EdgePropertiesAt(src, dst string, idx int64) map[string]lpg.PropertyValue
	// RemoveEdgeInstance drops every per-CREATE label and property
	// associated with (src, dst) at `idx`. Used by DELETE to discard
	// a specific logical edge while leaving sibling instances
	// untouched.
	RemoveEdgeInstance(src, dst string, idx int64)

	// OutNeighbours returns the outgoing neighbour node keys of n as a
	// snapshot slice. Callers must not mutate the returned slice.
	OutNeighbours(n string) []string

	// InNeighbours returns the incoming neighbour node keys of n as a
	// snapshot slice. This requires a full graph walk for directed adjacency
	// lists that do not maintain a reverse index. Callers must not mutate the
	// returned slice.
	InNeighbours(n string) []string

	// OutDegree returns the number of outgoing edges from n.
	OutDegree(n string) int

	// ResolveNodeID translates a user-facing node key to its internal NodeID,
	// returning ok=false when the node has not been interned yet.
	ResolveNodeID(n string) (graph.NodeID, bool)

	// ResolveNodeLabel translates an internal NodeID back to the user-facing
	// node key, returning ok=false when id is unknown.
	ResolveNodeLabel(id graph.NodeID) (string, bool)

	// WalkNodeIDs calls fn for every node currently interned in the graph.
	WalkNodeIDs(fn func(graph.NodeID) bool)
}

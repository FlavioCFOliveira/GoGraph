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
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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
//
// # Token limit (rmp #2748, #2956)
//
// Every method that takes a label, a relationship type or a property key
// returns an error, and refuses a name longer than [lpg.MaxTokenLen] bytes with
// one wrapping [lpg.ErrTokenTooLong] BEFORE it changes anything — the graph,
// the undo record, the counters, the constraint reservations or the staged
// WAL ops. The seven methods that used to return nothing (RemoveNodeLabel,
// DelNodeProperty, SetEdgeLabel, DelEdgeProperty, SetEdgeLabelAt,
// SetEdgeLabelByHandle, DelEdgePropertyByHandle) gained that error result: a
// breaking change, made because the WAL-backed implementation could not
// report the refusal at all, and a relationship type it could not log was
// acknowledged and recovered as empty (rmp #2956). Callers must propagate it.
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

	// AddEdgeH is [GraphMutator.AddEdge] with a stable per-edge handle
	// allocated by the underlying graph and stamped onto the adjacency
	// slot. The returned handle keys the *ByHandle metadata setters below
	// so a parallel CREATE's type and properties are resolvable on the
	// read path by an identity that survives sibling-edge deletion. Used
	// by CreateRelationship and by MergeRelationship's create path (rmp
	// #1683) so a MERGE-built parallel edge of a distinct type keeps its
	// own per-instance identity instead of collapsing onto the per-pair
	// label union. The handle is always non-zero on success.
	AddEdgeH(src, dst string, w float64) (srcID, dstID graph.NodeID, handle uint64, err error)

	// RemoveEdge removes the directed edge from src to dst (no-op if absent).
	// In a multigraph it removes the FIRST src→dst adjacency slot regardless of
	// which parallel instance is intended; callers that hold a bound instance's
	// stable handle must use RemoveEdgeByHandle for instance precision.
	RemoveEdge(src, dst string)

	// RemoveEdgeByHandle removes the single parallel edge instance identified by
	// the stable handle on the (src, dst) pair — its adjacency slot AND its
	// per-handle label/property metadata — leaving sibling instances intact.
	// Used by DELETE of a specifically-bound relationship so a parallel edge of
	// a distinct type/property is retired EXACTLY, not by first-match (rmp
	// #2018). handle must be non-zero; a caller with no resolvable handle (simple
	// graph, or a post-projection binding that lost the edge position) uses
	// RemoveEdge instead. No-op when no slot carries handle.
	RemoveEdgeByHandle(src, dst string, handle uint64)

	// SetNodeLabel attaches label to n (inserting n if absent). Returns
	// any error from the underlying [adjlist.AdjList.AddNode] (see
	// [GraphMutator.AddNode]).
	SetNodeLabel(n, label string) error

	// RemoveNodeLabel detaches label from n (no-op if absent).
	RemoveNodeLabel(n, label string) error

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
	DelNodeProperty(n, key string) error

	// NodeProperties returns a snapshot of all properties currently on n.
	NodeProperties(n string) map[string]lpg.PropertyValue

	// NodeLabels returns a snapshot of all labels currently on n in
	// unspecified order.
	NodeLabels(n string) []string

	// HasEdge reports whether a directed edge from src to dst is present.
	HasEdge(src, dst string) bool

	// SetEdgeLabel attaches label to the directed edge (src, dst).
	SetEdgeLabel(src, dst, label string) error

	// SetEdgeProperty sets the named property on the directed edge (src, dst).
	// Returns any error from the underlying graph (e.g. schema violation).
	SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error

	// DelEdgeProperty removes the named property from the directed edge
	// (src, dst) (no-op if absent).
	DelEdgeProperty(src, dst, key string) error

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
	SetEdgeLabelAt(src, dst string, idx int64, label string) error
	// EdgeLabelsAt returns the labels recorded at instance `idx` of
	// the directed edge (src, dst), or nil when the instance has no
	// per-CREATE labels.
	EdgeLabelsAt(src, dst string, idx int64) []string
	// SetEdgePropertyAt records `key`=`value` on the directed edge
	// instance (src, dst) at the supplied 1-based CREATE index. Returns
	// any error returned by the installed SchemaValidator.
	SetEdgePropertyAt(src, dst string, idx int64, key string, value lpg.PropertyValue) error
	// EdgePropertiesAt returns the property map recorded at instance
	// `idx` of the directed edge (src, dst), or nil when no
	// per-CREATE map was captured.
	EdgePropertiesAt(src, dst string, idx int64) map[string]lpg.PropertyValue
	// RemoveEdgeInstance drops every per-CREATE label and property
	// associated with (src, dst) at `idx`. Used by DELETE to discard
	// a specific logical edge while leaving sibling instances
	// untouched.
	RemoveEdgeInstance(src, dst string, idx int64)

	// SetEdgeLabelByHandle attaches `label` to the edge identified by the
	// stable `handle` on the (src, dst) pair (see [GraphMutator.AddEdgeH]).
	// The handle-keyed analogue of SetEdgeLabelAt; the read path resolves a
	// parallel CREATE's type by this identity instead of a positional CSR
	// index. No-op when handle is 0.
	SetEdgeLabelByHandle(src, dst string, handle uint64, label string) error
	// EdgeLabelsByHandle returns the labels recorded for the edge
	// identified by `handle` on the (src, dst) pair, or nil when none.
	EdgeLabelsByHandle(src, dst string, handle uint64) []string
	// SetEdgePropertyByHandle records `key`=`value` on the edge identified
	// by the stable `handle` on the (src, dst) pair. No-op when handle is 0.
	// Returns any error returned by the installed SchemaValidator.
	SetEdgePropertyByHandle(src, dst string, handle uint64, key string, value lpg.PropertyValue) error
	// DelEdgePropertyByHandle removes exactly `key` from the property bag of
	// the edge identified by the stable `handle` on the (src, dst) pair,
	// leaving sibling handles untouched. No-op when handle is 0 or the handle
	// never carried the key. The single-key removal analogue of
	// RemoveEdgeInstanceByHandle (which drops ALL of a handle's metadata); used
	// by REMOVE r.x / SET r.x = null on one parallel relationship instance.
	DelEdgePropertyByHandle(src, dst string, handle uint64, key string) error
	// EdgePropertiesByHandle returns the property map recorded for the edge
	// identified by `handle` on the (src, dst) pair, or nil when none.
	EdgePropertiesByHandle(src, dst string, handle uint64) map[string]lpg.PropertyValue
	// RemoveEdgeInstanceByHandle drops every per-handle label and property
	// associated with (src, dst) at `handle`. Used by DELETE to discard a
	// specific logical edge while leaving sibling handles untouched.
	RemoveEdgeInstanceByHandle(src, dst string, handle uint64)

	// FirstEdgeHandle returns the stable per-edge handle stamped on the FIRST
	// adjacency slot from src to dst, and whether such a handled slot exists.
	// It is the by-PAIR handle resolver: MERGE binds a single logical
	// (src, dst) edge rather than a specific parallel instance, so the
	// ON MATCH / ON CREATE action path uses it to mirror its per-pair property
	// writes onto the matched edge's by-handle store. Returns 0 / false when
	// either endpoint is unknown, no src→dst edge exists, or the matched slot
	// carries the 0 "no handle" sentinel (pre-handle storage) —
	// in which case the caller mutates the per-pair store only and never a
	// by-handle instance.
	FirstEdgeHandle(src, dst string) (uint64, bool)

	// EdgeHandles appends to buf the stable handle of every stored src→dst
	// relationship instance, one entry per adjacency slot in slot order, and
	// returns the extended slice. A slot stamped without a handle contributes
	// the 0 sentinel, so the appended count is the number of parallel src→dst
	// relationships. It is the per-instance enumerator MERGE's match path binds
	// one row per matching relationship from; FirstEdgeHandle names only the
	// first. Allocates only when buf must grow.
	EdgeHandles(src, dst string, buf []uint64) []uint64

	// HasEdgeHandle reports whether a stored src→dst slot carries the non-zero
	// handle. A bound relationship value names a stored instance of the pair
	// exactly when this holds for one of the pair's two orders; DELETE uses it
	// to remove that instance rather than the pair's first slot.
	HasEdgeHandle(src, dst string, handle uint64) bool

	// OutNeighbours returns the outgoing neighbour node keys of n as a
	// snapshot slice. Callers must not mutate the returned slice.
	OutNeighbours(n string) []string

	// InNeighbours returns the incoming neighbour node keys of n as a
	// snapshot slice. This requires a full graph walk for directed adjacency
	// lists that do not maintain a reverse index. Callers must not mutate the
	// returned slice.
	InNeighbours(n string) []string

	// RemoveAllEdgesFrom removes all edges incident from n in O(degree) time
	// and returns the outgoing neighbour keys that were removed. For undirected
	// graphs the mirror entries are also removed. The per-pair edge state
	// (labels, properties, handle metadata, CREATE counters) is cleared for
	// every removed pair, exactly as a sequence of RemoveEdge calls would do.
	//
	// Callers that also need to roll back undo-log entries or emit WAL records
	// must capture the neighbours via OutNeighbours before calling this method;
	// RemoveAllEdgesFrom does not emit WAL records on its own.
	RemoveAllEdgesFrom(n string)

	// OutDegree returns the number of outgoing edges from n.
	OutDegree(n string) int

	// ResolveNodeID translates a user-facing node key to its internal NodeID,
	// returning ok=false when the node has not been interned yet.
	ResolveNodeID(n string) (graph.NodeID, bool)

	// KeySequence returns the target graph's synthetic node-key sequence, from
	// which CREATE and MERGE mint the keys of nodes the query does not name.
	// Every mutator over one graph returns that graph's sequence.
	KeySequence() *lpg.KeySequence

	// ResolveNodeLabel translates an internal NodeID back to the user-facing
	// node key, returning ok=false when id is unknown.
	ResolveNodeLabel(id graph.NodeID) (string, bool)

	// WalkNodeIDs calls fn for every node currently interned in the graph.
	// fn may call back into the mutator (ResolveNodeLabel, label and property
	// reads): no lock of the enumeration is held while fn runs (rmp #2897).
	WalkNodeIDs(fn func(graph.NodeID) bool)
}

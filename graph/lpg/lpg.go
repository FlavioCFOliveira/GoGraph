// Package lpg implements the Labelled Property Graph model on top of
// the [github.com/FlavioCFOliveira/GoGraph/graph/adjlist] mutable adjacency-list backend.
//
// An LPG decorates each node and each edge with a set of labels
// (interned strings identifying classes/types) and a bag of typed
// properties. This package provides labels (see [Graph.SetNodeLabel],
// [Graph.SetEdgeLabel]) and typed properties (see [Graph.SetNodeProperty],
// [Graph.SetEdgeProperty]).
//
// # Concurrency
//
// The Graph type is safe for concurrent use: every individual operation
// is internally synchronised — label and property shards by RWMutex,
// adjacency by lock-free atomic per-shard snapshots, and the per-instance,
// edge-create-count, and edge-handle stores by mutex — so no single
// accessor races another.
//
// Transaction-atomic visibility, however, is OPT-IN. A committed
// transaction may span several operations across several substructures
// (adjacency, node/edge labels, node/edge properties, tombstones, the
// roaring label bitmaps, and the secondary indexes). To observe a whole
// transaction atomically — never a partial transaction, never a torn
// cross-substructure view — reads must run inside [Graph.View] and writes
// inside [Graph.ApplyAtomically], which flip a transaction's writes
// visible as one step under a single visibility barrier:
//
//   - Per-operation atomicity holds for every accessor, always.
//   - Partial-transaction-free reads hold ONLY inside [Graph.View].
//   - Cross-substructure consistency (e.g. "if the edge exists, both of
//     its endpoint labels exist") holds ONLY inside [Graph.View].
//
// A direct accessor call made outside [Graph.View] therefore observes a
// consistent single operation, but may observe a multi-operation
// transaction half-applied. The full model — and the tracked lock-free
// per-shard snapshot that will make every read transaction-consistent
// without the barrier — is described in docs/isolation-design.md.
package lpg

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/ctxlock"
)

// LabelID is the compact internal identifier produced by the
// [LabelRegistry] for an interned label string.
type LabelID uint32

// labelNames is an immutable id→name table published by
// [LabelRegistry] via copy-on-write. Once stored into the registry's
// atomic pointer it is never mutated; a new interning allocates a fresh
// table. Readers load the pointer once with zero synchronisation and
// index into the slice, so the read path (Resolve) is fully lock-free.
type labelNames struct {
	names []string
}

// forwardLabelName is the immutable name→id table published by
// [LabelRegistry] via copy-on-write. Once stored it is never mutated;
// interning a previously unseen name allocates a fresh map. Readers
// (Lookup) load the pointer once with zero synchronisation, so the name→id
// read path is fully lock-free — matching the id→name Resolve path.
type forwardLabelName struct {
	m map[string]LabelID
}

// LabelRegistry interns label names and assigns sequential LabelIDs.
// It is safe for concurrent use.
//
// Both read paths are fully lock-free: [LabelRegistry.Lookup] (name→id)
// loads the immutable forward table through an [atomic.Pointer] and
// [LabelRegistry.Resolve] (id→name) loads the immutable id→name snapshot,
// neither taking any lock. The write path ([LabelRegistry.Intern] of a
// previously unseen name — a rare event) serialises under a mutex, builds
// fresh immutable tables extended by one entry, and publishes them — the
// id→name snapshot before the name→id table — so any reader that observes
// an id from Lookup can already Resolve it, and any reader that observes an
// id in a bag observes (by release/acquire ordering through that bag's own
// publication) tables at least as new as the ones Intern published. Lookup
// and Resolve therefore never miss a live id.
type LabelRegistry struct {
	// fwd holds the immutable name→id table. Loaded lock-free by Lookup;
	// swapped under mu by Intern.
	fwd atomic.Pointer[forwardLabelName]
	// snap holds the immutable id→name table. Loaded lock-free by
	// Resolve; swapped under mu by Intern.
	snap atomic.Pointer[labelNames]
	// mu serialises Intern (the write path) only; the read paths never take
	// it. The steady-state label vocabulary is small and stable, so Intern
	// is contended only while the vocabulary is first built up.
	mu sync.Mutex
}

// NewLabelRegistry returns an empty registry.
func NewLabelRegistry() *LabelRegistry {
	r := &LabelRegistry{}
	r.snap.Store(&labelNames{})
	r.fwd.Store(&forwardLabelName{m: make(map[string]LabelID)})
	return r
}

// Intern returns a stable LabelID for name, allocating one on first
// encounter. It runs on the write path only (label assignment). A
// lock-free fast path returns an already-interned id without taking the
// mutex; only the first interning of a previously unseen name serialises
// under mu to publish the extended tables. The steady-state label
// vocabulary is small and stable.
func (r *LabelRegistry) Intern(name string) LabelID {
	if id, ok := r.fwd.Load().m[name]; ok {
		return id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.fwd.Load()
	if id, ok := cur.m[name]; ok { // re-check under the lock
		return id
	}
	names := r.snap.Load()
	id := LabelID(len(names.names))
	nextNames := &labelNames{names: make([]string, len(names.names)+1)}
	copy(nextNames.names, names.names)
	nextNames.names[id] = name
	nextFwd := &forwardLabelName{m: make(map[string]LabelID, len(cur.m)+1)}
	for k, v := range cur.m {
		nextFwd.m[k] = v
	}
	nextFwd.m[name] = id
	// Publish id→name before name→id so a reader that observes the new id
	// via Lookup can already Resolve it.
	r.snap.Store(nextNames)
	r.fwd.Store(nextFwd)
	return id
}

// Lookup returns the LabelID for name and true, or 0 and false when name
// has not been interned. It is lock-free: it loads the immutable name→id
// table once and reads it, so concurrent per-row label-predicate lookups
// never serialise nor bounce a shared reader-count cache line.
func (r *LabelRegistry) Lookup(name string) (LabelID, bool) {
	id, ok := r.fwd.Load().m[name]
	return id, ok
}

// Resolve returns the name interned under id, or the empty string and
// false when id is unknown. It is lock-free: it loads the immutable
// id→name snapshot once and indexes into it.
func (r *LabelRegistry) Resolve(id LabelID) (string, bool) {
	s := r.snap.Load()
	if uint64(id) >= uint64(len(s.names)) {
		return "", false
	}
	return s.names[id], true
}

// edgeKey identifies a single directed edge endpoints pair for label
// storage. Multigraph parallel edges share a key here; v1 stores the
// union of labels across parallel edges. A future revision can carry
// a per-edge index when parallel-edge label semantics matter.
type edgeKey struct {
	src, dst graph.NodeID
}

// propMapShards is the number of independent locks striping the
// per-vertex and per-edge property maps. It MUST stay a power of two (the
// shard index is NodeID & (propMapShards-1)). It is a runtime-only
// concurrency-striping constant — no on-disk format or snapshot depends on
// it, so it can change freely between versions.
//
// The 2026-06-24 performance audit measured that per-row property reads
// (NodePropertyByID, hit once per scanned row per reader) ARE hot under
// concurrent full-table scans: at high core counts the per-shard RWMutex
// reader-count atomic bounces its cache line. Widening from 16 to 64 spreads
// those reader-count atomics across 4x more cache lines, so a full scan's
// readers collide on a given shard far less often. (The fully lock-free
// per-shard read remains the deferred F3/#1671 COW epic; this widening is the
// cheap, ACID-neutral mitigation.) 64 is still well below adjlist's 256; the
// extra empty shards cost only a few KB per graph.
const propMapShards = 64

// nodePropShard is one stripe of the per-vertex property map. The inner
// per-node bag is a compact tiered [propBag] (sprint 207, #1587) stored by
// value, not a nested Go map: a node carrying a handful of properties pays a
// single small slice backing instead of ~300 B of map overhead. The bag is
// guarded by mu exactly as the nested map was.
type nodePropShard struct {
	m map[graph.NodeID]propBag
	// d is the SPARSE per-node property-delta head map (rmp #2279), nil until
	// this shard takes its first delta. Sparse for the same reason the label
	// one is: GoGraph has no per-node struct, so a field on propBag would be
	// paid by every node with a property, including in graphs that never write.
	// See mvcc_props.go.
	d  map[graph.NodeID]*nodePropDelta
	mu sync.RWMutex
}

// nodeLabelShard is one stripe of the node-label bag. The mutex
// serialises mutations on this shard only; readers hold an RLock
// for HasNodeLabel / NodeLabels. Splitting the bag into 64 shards
// removes the global nodeMu contention point that previously
// serialised every Set/Remove/Has across all NodeIDs in the graph.
type nodeLabelShard struct {
	m map[graph.NodeID]labelBag
	// d is the SPARSE per-node label-delta head map of the P0 MVCC spike
	// (rmp #2275), nil until this shard takes its first delta. It is sparse
	// rather than a field on labelBag because GoGraph has no per-node struct:
	// a pointer on the bag would grow the map value for every labelled node,
	// including in graphs that never write. See mvcc_labels.go.
	d  map[graph.NodeID]*nodeLabelDelta
	mu sync.RWMutex
}

// edgeLabelShard is the OVERFLOW half of the edge-label store, the edge
// counterpart of [nodeLabelShard]; the shard is keyed by the src endpoint so
// all overflow labels of edges out of one node coalesce in the same shard
// (favourable for the common access pattern: walk-out-of-node-then-inspect-
// label) and the shard alignment matches [edgePropShardFor].
//
// # Two-tier representation (task #1583)
//
// The single relationship type of the overwhelmingly common LIVE single-label
// edge is NOT stored here at all: it lives in the per-slot label column
// co-located in the [adjlist.AdjList] adjacency entry (encoded as id+1, 0 ==
// no label), removing the redundant 16-byte (src,dst) key plus per-entry map
// overhead that previously dominated resident memory on label-heavy graphs.
//
// This overflow map holds only the two cases the slot column cannot:
//
//	(a) the 2nd..Nth label of a multi-label endpoint pair (>= 2 labels), the
//	    first of which is in the slot; and
//	(b) ORPHANED labels — a label set on a (src,dst) whose adjacency edge was
//	    later removed within a failed statement, since [Graph.RemoveEdgeLabel]
//	    does not require the edge to still exist (the executor's transaction-
//	    undo path). Such a label has no live slot to live in, so it can only
//	    reside in overflow.
//
// The per-pair label set a caller observes via [Graph.EdgeLabels] is therefore
// DERIVED: the dedup-union of the decoded slot labels of every dst-matching
// adjacency slot and overflow[k]. The overflow map is allocated lazily, so an
// all-single-label graph never pays for sixteen empty spill maps.
type edgeLabelShard struct {
	// overflow holds the extra (beyond the slot) labels of a pair, deduplicated.
	// nil until the first label spills here.
	overflow map[edgeKey][]LabelID
	// v indexes the pre-image chains of the pairs a writer has touched, so a
	// reader can reconstruct the overflow set as of its own start timestamp
	// (rmp #2291). Lazily allocated: a shard nothing has written keeps none.
	v  sideVersions[edgeKey, []LabelID]
	mu sync.RWMutex
}

// addOverflow appends lid to k's overflow list if not already present, and
// reports whether it changed the list. The caller must hold sh.mu for writing.
//
// The bool is load-bearing rather than informational: [Graph.SetEdgeLabel] bumps
// the topology generation only on a genuine change (rmp #2255), so a caller that
// re-asserts a label already present must be distinguishable from one that adds
// a new one.
func (sh *edgeLabelShard) addOverflow(k edgeKey, lid LabelID) bool {
	ls := sh.overflow[k]
	for _, x := range ls {
		if x == lid {
			return false
		}
	}
	if sh.overflow == nil {
		sh.overflow = make(map[edgeKey][]LabelID)
	}
	sh.overflow[k] = append(ls, lid)
	return true
}

// hasOverflow reports whether k's overflow list carries lid. The caller must
// hold sh.mu for reading (or writing).
func (sh *edgeLabelShard) hasOverflow(k edgeKey, lid LabelID) bool {
	for _, x := range sh.overflow[k] {
		if x == lid {
			return true
		}
	}
	return false
}

// removeOverflow detaches lid from k's overflow list, dropping the entry when
// its last label goes. Returns true when lid was present. The caller must hold
// sh.mu for writing.
func (sh *edgeLabelShard) removeOverflow(k edgeKey, lid LabelID) bool {
	ls, ok := sh.overflow[k]
	if !ok {
		return false
	}
	for i, x := range ls {
		if x == lid {
			ls = append(ls[:i], ls[i+1:]...)
			if len(ls) == 0 {
				delete(sh.overflow, k)
			} else {
				sh.overflow[k] = ls
			}
			return true
		}
	}
	return false
}

// clearOverflow drops every overflow label on k and returns how many were
// dropped, so the caller can keep [Graph.edgeLabelOverflowActive] exact. The
// caller must hold sh.mu for writing.
func (sh *edgeLabelShard) clearOverflow(k edgeKey) int {
	n := len(sh.overflow[k])
	delete(sh.overflow, k)
	return n
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

	// labelDeltas arms the P0 MVCC spike (rmp #2275); labelDeltaActive mirrors
	// the number of live label deltas as a lock-free gate, exactly as
	// tombstoneActive does for the tombstone set. Both are inert unless
	// [Graph.EnableLabelDeltas] has been called. See mvcc_labels.go.
	labelDeltas      bool
	labelDeltaActive atomic.Int64
	// mvccClock mints commit timestamps and transaction ids from the two
	// disjoint ranges either side of mvcc.TxIDBase, so one uint64 on a
	// version's commit record distinguishes in-flight from committed. Shared
	// with the adjacency's versioning, which is why it lives in graph/mvcc.
	mvccClock mvcc.Clock
	// propDeltas / propDeltaActive are the node-property half of the substrate,
	// kept separate from the label pair so a property write cannot push a label
	// reader off its fast path. See mvcc_props.go.
	propDeltas      bool
	propDeltaActive atomic.Int64

	nodeLabelShards [propMapShards]nodeLabelShard
	edgeLabelShards [propMapShards]edgeLabelShard

	nodePropShards [propMapShards]nodePropShard
	// Edge properties are NOT stored in a per-pair map. They live in the
	// per-source-node columnar block ([edgePropCols]) carried inside each
	// adjacency entry as its opaque aux column (sprint 222, #1637-1643). See
	// edge_property.go and edge_property_column.go.

	// edgeCreateCountShards tracks how many CREATE statements have
	// targeted each directed (src, dst) endpoint pair — separate from
	// the underlying simple-graph adjacency, which silently collapses
	// duplicate CREATEs. Used by MERGE to emit one output row per
	// recorded CREATE call when the search matches an existing edge
	// (Merge5 [3]). See edge_create_count.go for full semantics.
	edgeCreateCountShards [propMapShards]edgeCreateCountShard

	// edgeInstanceLabelShards / edgeInstancePropShards carry the
	// per-CREATE-instance label and property sets so each parallel
	// CREATE call can supply its own attributes independent of the
	// merged-union view the per-pair stores keep. The instance index
	// is the 1-based value returned by IncEdgeCreateCount; CreateRelationship
	// writes through both stores so the per-pair surfaces stay
	// untouched while the per-instance surfaces unlock Match2 [6] /
	// Match7 [29] / Merge5 [21] / Match6 [14]. See
	// edge_instance_labels.go and edge_instance_props.go.
	edgeInstanceLabelShards [propMapShards]edgeInstanceLabelShard
	edgeInstancePropShards  [propMapShards]edgeInstancePropShard

	// edgeHandleLabelShards / edgeHandlePropShards are the stable-handle
	// keyed analogue of the *InstanceLabel/*InstanceProp stores above.
	// Where the instance stores key per-CREATE metadata by the 1-based
	// per-pair CREATE index — which the read path had to re-derive
	// positionally from CSR slot order, breaking after a delete — these
	// stores key by the immutable per-edge handle allocated by
	// [Graph.AddEdgeH], so the read path resolves an edge's type and
	// properties by an identity that survives sibling-edge deletion.
	// Populated only in multigraph mode (one handle per CREATE); see
	// edge_handle.go.
	edgeHandleLabelShards [propMapShards]edgeHandleLabelShard
	edgeHandlePropShards  [propMapShards]edgeHandlePropShard

	// tombstones records NodeIDs that have been removed by RemoveNode.
	// The underlying Mapper cannot release the index slot (NodeID stability
	// is a hard contract), so removal is observable only via this set:
	// every logical read path (LiveOrder, IsTombstoned, and every
	// TombstonedIDs consumer) must filter tombstoned ids. A tombstone is
	// cleared by revive (re-materialising the node), so the set holds
	// exactly the currently-removed ids.
	//
	// The set is published through an atomic.Pointer to an IMMUTABLE
	// roaring64.Bitmap so readers are fully lock-free: IsTombstoned,
	// LiveOrder, TombstonedIDs and every introspection consumer load the
	// pointer once (a nil pointer means no tombstone) and do read-only
	// Contains/iteration with no synchronisation — a per-node MATCH (n)
	// scan no longer bounces a shared reader-count cache line across cores
	// (rmp #2039). Mutators (RemoveNode, revive, RestoreTombstones) hold
	// tombstoneMu, CLONE the current bitmap, mutate the private clone, and
	// atomic.Store the new pointer — copy-on-write. The clone cost is
	// O(tombstones) and paid only on the rare delete/revive, never on a
	// read. A concurrent lpg.Graph.View reader therefore observes either
	// the pre- or the post-mutation set, never a torn state; the clone
	// deep-copies (copyOnWrite is never enabled), so mutating it cannot
	// race a reader still holding the previously published bitmap.
	tombstoneMu sync.Mutex
	tombstones  atomic.Pointer[roaring64.Bitmap]
	// tombstoneActive mirrors the tombstone-set cardinality as a lock-free
	// gate. AddNode is a hot path; on the overwhelmingly common case of a
	// graph that has never deleted a node this lets AddNode (and every read
	// path) skip the pointer load and the mapper lookup entirely. It is
	// mutated only under tombstoneMu, in lock-step with the published
	// bitmap, so it always equals the bitmap's cardinality.
	tombstoneActive atomic.Int64

	// edgeLabelOverflowActive mirrors the total number of overflow labels held
	// across every [edgeLabelShard] as a lock-free gate, exactly as
	// tombstoneActive gates the tombstone set.
	//
	// It exists for [Graph.slotCarriesType], which resolves a relationship type
	// once PER SLOT of a walked adjacency entry. A pair's overflow list is the
	// one part of its type state that is not per-slot addressable (see
	// [edgeLabelShard]), so a slot whose own column entry does not carry the
	// wanted type must still consult it — and doing so unconditionally would take
	// the pair's shard read lock on every non-matching slot of every typed degree
	// walk. Overflow is populated only by a [Graph.SetEdgeLabel] that found no
	// free slot to place the type in, which no Cypher-built graph and no
	// single-type graph ever reaches, so this counter is 0 for essentially every
	// graph and one atomic load replaces the lock.
	//
	// It is mutated only under the owning shard's write lock, in lock-step with
	// the overflow map itself. It is therefore exact, and a reader that observes
	// 0 is observing a state in which no overflow label existed.
	edgeLabelOverflowActive atomic.Int64

	// constraintActive mirrors the cypher engine's schema-constraint count as a
	// lock-free gate. The checkpointer reads it (via HasConstraints) to decide
	// whether a snapshot must carry constraints.bin before the WAL prefix that
	// first declared the constraints can be truncated; without it an embedder
	// that forgets checkpoint.WithConstraintSpecs would silently lose every
	// schema constraint on the next reopen (#1464). It is maintained by
	// Engine.syncConstraintCount under the engine's single-writer lock.
	constraintActive atomic.Int64

	// indexActive mirrors the cypher engine's secondary-index-definition count
	// as a lock-free gate, the exact index analogue of constraintActive. The
	// engine's CREATE INDEX commits via Tx.CommitWALOnly, which appends the WAL
	// frame but does NOT replay it through the store apply path, so the
	// store-direct storeIndexActive counter never sees an engine index. The
	// checkpointer's phase-3 self-sufficiency re-check therefore consults
	// HasIndexes, which ORs this engine-mirrored count with the store-direct one,
	// so a CREATE INDEX committed during the lock-free snapshot-write window is
	// caught and the WAL prefix holding it is retained (#1755). It is maintained
	// by Engine.syncIndexCount under the engine's single-writer lock.
	indexActive atomic.Int64

	// storeConstraints tracks the schema constraints declared through the
	// txn.Store-direct API (txn.Tx.CreateConstraint / DropConstraint) for an
	// embedder that does NOT drive the graph through the cypher engine and so
	// never calls SetActiveConstraintCount. It is keyed by (kind, label,
	// property) — the same identity recovery dedups on — so re-declaring the
	// same constraint is idempotent and a drop removes exactly one slot. The
	// store-layer checkpoint fail-safe (Graph.HasConstraints) consults its
	// count via storeConstraintActive so a store-direct constraint is never
	// silently dropped by a WAL-truncating checkpoint (#1756). It is a separate
	// source from constraintActive so an embedder that mixes the engine and the
	// store on one graph cannot corrupt either counter.
	storeConstraintMu     sync.Mutex
	storeConstraints      map[storeConstraintKey]struct{}
	storeConstraintActive atomic.Int64

	// storeIndexes tracks the secondary indexes declared through the
	// txn.Store-direct API (txn.Tx.CreateIndex / DropIndex) for an embedder
	// that does NOT drive the graph through the cypher engine and so never goes
	// through Engine.registerRecoveredIndexes. It is keyed by index NAME — the
	// index identity recovery dedups on (indexSet.byName, last-writer-wins) — so
	// re-declaring the same index is idempotent and a drop removes exactly one
	// slot. The store-layer checkpoint fail-safe (Graph.HasIndexes) consults its
	// count via storeIndexActive so a store-direct index definition is never
	// silently dropped by a WAL-truncating checkpoint (#1755). It is a separate
	// source from the cypher engine's own index-def registry so an embedder that
	// mixes the engine and the store on one graph cannot corrupt either count.
	storeIndexMu     sync.Mutex
	storeIndexes     map[string]struct{}
	storeIndexActive atomic.Int64

	// nodesAddedCount / nodesRemovedCount / edgesAddedCount /
	// edgesRemovedCount track per-direction counters used by the TCK
	// side-effect comparator. Net Order() / Size() can't distinguish a
	// CREATE+DELETE from a no-op, so the comparator needs the explicit
	// addition and removal counts.
	nodesAddedCount   atomic.Uint64
	nodesRemovedCount atomic.Uint64
	edgesAddedCount   atomic.Uint64
	edgesRemovedCount atomic.Uint64

	// topoGeneration is a dedicated, purely monotonic counter — bumped by
	// exactly 1 on every edge addition, removal, or undo of either (never
	// decremented), whether driven through the Cypher engine's write
	// adapters/undo log ([Graph.IncrEdgesAdded] and siblings) or through a
	// direct [store/txn.Tx] write with no Cypher statement in the loop
	// ([Graph.BumpTopoGeneration]) — that a caller can use to tell "has
	// anything that could change a CSR-position-keyed structure happened
	// since I last built it" with a single equality check (rmp #1871). It is
	// deliberately NOT the same counters as edgesAddedCount/edgesRemovedCount
	// above (which exist for the TCK side-effect comparator, a Cypher-
	// statement-scoped concern that could change independently in the
	// future, and that a store-direct write was never part of in the first
	// place) and deliberately NOT a net/live
	// count such as Size() (two independent accumulators that only ever
	// increase can't produce a false-unchanged reading across an add and an
	// unrelated remove, but a net count can: add X, later remove an
	// unrelated pre-existing Y, and the net count returns to its original
	// value even though every CSR position from Y's old slot onward has
	// shifted).
	//
	// Interning a fresh node does NOT bump this: its CSR range is appended after
	// every existing range, so it shifts no existing edge's position. TOMBSTONING
	// does bump it (RemoveNode, revive, RestoreTombstones), because
	// csr.BuildFromAdjListLive omits the arcs incident to a tombstoned node, so the
	// LIVE topology a cache is derived from has changed even when no edge was
	// touched (rmp #2143).
	topoGeneration atomic.Uint64

	// edgeHandleSeq is the source of stable per-edge handles for this
	// graph. It is bumped once per logical edge creation by
	// [Graph.AddEdgeH] / [Graph.nextEdgeHandle]; handles are monotone and
	// never reused, even after the edge is deleted. See edge_handle.go for
	// the full contract (and the Stage 2 note on durability). The first
	// handle is 1 — 0 is reserved as the "no handle" sentinel in the
	// adjlist/CSR handle columns.
	edgeHandleSeq atomic.Uint64

	idxMgr    atomicIndexManager
	validator atomicValidator

	// visMu is the transaction-visibility barrier (audit gap F3,
	// docs/isolation-design.md). Since rmp #2290 it is a WRITE-side mechanism:
	// [Engine.Run] does not take it, and a read gets its consistency from a
	// snapshot instead. A writer still holds it exclusively for the whole apply,
	// which is what makes the write path single-writer and what lets the
	// adjacency's commit window and the shared commit record use plain fields.
	//
	// docs/isolation-design.md). A writer applying a multi-op transaction
	// holds visMu via [Graph.ApplyAtomically] for the whole apply, so the
	// transaction's writes across every substructure become observable to
	// readers as one atomic step; a transactional reader pins a consistent,
	// partial-transaction-free view via [Graph.View]. The per-shard mutexes
	// below visMu still guard individual writes; visMu adds the missing
	// transaction-level atomicity that single-op locking cannot provide.
	// It is a RWMutex (not an atomic snapshot pointer) by deliberate,
	// correctness-first choice; the lock-free per-shard snapshot is the
	// performance optimisation tracked by the later F3 sub-tasks. The
	// immutable CSR analytics path does not go through these methods and
	// stays lock-free.
	visMu sync.RWMutex

	// writeTx names the write transaction whose bracket is currently open, and
	// through it the read view the WRITE path resolves through (rmp #2299): as of
	// the instant the transaction began, plus its own uncommitted versions.
	//
	// A SLOT holding per-transaction state, not the state itself (rmp #2301): the
	// commit record, the version count, the snapshot and the conflict flag all
	// live on the [writeCtx] the bracket owns, so two writers cannot corrupt each
	// other's. Before that they were fields here and on [mvcc.WriteStamp], and
	// audit finding E3 is what a second writer did to them — see
	// graph/mvcc/stamp.go.
	//
	// The state is recycled through [Graph.writeCtxPool] so opening a bracket
	// still allocates nothing, which
	// TestBarrierGuard_ApplyAtomicallyAllocatesNothing requires.
	writeTx atomic.Pointer[writeCtx]
	// writeCtxFree caches ONE finished [writeCtx] for the next bracket to reuse,
	// so per-transaction state costs no allocation in steady state. See
	// [Graph.acquireWriteCtx] for why one slot and not a sync.Pool, with the
	// measurement that settled it.
	writeCtxFree atomic.Pointer[writeCtx]

	// adjVer is the per-node adjacency write-write conflict index (rmp #2300).
	// Adjacency keeps no per-object delta chain — its only version signal is the
	// global topoGeneration below — so it cannot use the rule every other store
	// uses, and this holds the two stamps per node that replace it. See
	// [adjVersions] for the rule, and for the Memgraph source that settled why an
	// adjacency APPEND is commutative and must not conflict with another append.
	adjVer adjVersions

	// barrier enforces that no single goroutine re-enters visMu via
	// [Graph.View] / [Graph.ApplyAtomically]. visMu is not re-entrant, so a
	// nested acquisition from a goroutine already inside the barrier would
	// deadlock the engine; the guard converts that silent hang into an immediate
	// panic.
	//
	// It is enforcing only under -race or -tags gograph_debug
	// (reentrancy_enabled.go); a released binary links the zero-sized no-op in
	// reentrancy_disabled.go, which documents the mechanism, the measured cost
	// that motivated the split, and the diagnosability it trades away.
	barrier barrierGuard

	// ── MVCC write-side state (rmp #2288) ───────────────────────────────────
	//
	// Placed LAST on purpose. These fields are touched only by the write path,
	// and inserting them in the middle of the struct moved visMu and barrier
	// away from the fields the read path touches beside them — measured as
	// +3 % on BenchmarkBarrier_View, paid by a read that never reads any of
	// this.

	// mvccArmed is the single arm for the whole versioning substrate — the node
	// label and property delta chains plus the adjacency's entry chains. Set
	// once at construction, so it stays a plain field. See mvcc_write.go.
	mvccArmed bool
	// The three per-edge side stores each keep their OWN lock-free version
	// counter (rmp #2291), so a read that touches one of them is not pushed off
	// its fast path by churn in another — the same separation the node-label
	// and node-property pairs have, for the same reason. See mvcc_edge_side.go.
	edgeLabelVersionActive         atomic.Int64
	edgeHandleLabelVersionActive   atomic.Int64
	edgeHandlePropVersionActive    atomic.Int64
	edgeInstanceLabelVersionActive atomic.Int64
	edgeInstancePropVersionActive  atomic.Int64
	// stamp NAMES the write transaction a write that carries none must resolve
	// to, SHARED with the adjacency so one transaction's labels, properties,
	// topology, relationship types and edge properties all publish with one
	// atomic store. The record is allocated lazily, so a bracket that versions
	// nothing allocates nothing.
	//
	// It holds no transaction state of its own since rmp #2301 — only the clock,
	// the slot naming the open transaction's [mvcc.TxState], and the count of
	// versions belonging to no transaction at all. See mvcc_write.go and
	// [mvcc.WriteStamp].
	stamp mvcc.WriteStamp
	// horizon tracks the start timestamps of active readers so reclamation
	// knows which versions no reader can reach. Readers register with it from
	// MVCC P4c onward; until then it is empty and the watermark is the clock,
	// which reclaims everything a completed write superseded.
	//
	// A POINTER, not a value: the horizon is 64 slots padded to a cache line
	// each, so embedding it would put 8 KiB inside every Graph.
	horizon *mvcc.Horizon
	// reclaimDebt counts versions created since the last reclamation pass, so
	// the pass runs on a bounded amount of churn rather than on every commit.
	// See [Graph.reclaimIfDue].
	reclaimDebt atomic.Int64
	// sweeping serialises reclamation against itself. The reclaimers are
	// mutually excluded from WRITERS by the per-shard locks they and the write
	// path both take, and safe against readers by the watermark argument — but
	// two sweeps at once would race on the same chains, so exactly one runs.
	sweeping atomic.Bool
	// idleTicks paces the opportunistic sweep a read makes; see
	// [Graph.ReclaimIdle].
	idleTicks atomic.Uint64
	// nodeLifeShards version node EXISTENCE — when each node was created and
	// when it was removed — so a reader knows which nodes it may see at all,
	// not merely what they contain. nodeLifeActive is the lock-free gate. See
	// mvcc_life.go.
	nodeLifeShards [propMapShards]nodeLifeShard
	nodeLifeActive atomic.Int64
	// lifeSeq orders two existence events a commit timestamp cannot separate;
	// see [lifeStamp].
	lifeSeq atomic.Uint64
	// idxDeferred holds label-index removals waiting for the watermark, so a
	// reader older than the removal still finds the entry; idxPendingActive is
	// the lock-free gate and the observable backlog. See mvcc_index.go.
	idxDeferred      deferredIdx
	idxPendingActive atomic.Int64
}

// ApplyAtomically runs fn while holding the graph's transaction-visibility
// write lock. Every mutation fn performs (across adjacency, labels,
// properties, tombstones, bitmaps, and indexes) becomes visible to
// [Graph.View] readers as a single atomic step: a concurrent View reader
// observes either none of fn's writes or all of them, never a partial set.
// fn is the in-memory apply of one durable transaction; callers invoke it
// only after the transaction's WAL frames are fsynced.
//
// ApplyAtomically must not be called re-entrantly, and the mutations inside fn
// must not call [Graph.View] or [Graph.ApplyAtomically] (the RWMutex is not
// re-entrant, so a nested acquisition from this goroutine would deadlock).
//
// The invariant is CHECKED in builds made with -race or -tags gograph_debug: a
// nested call from a goroutine already inside the barrier panics with a clear
// message instead of deadlocking. The panic indicates a programmer error and is
// not recovered by this package. A released build omits the check, because
// identifying the calling goroutine costs a runtime.Stack call that measured
// 97-99% of this method and did not scale with cores; there, violating the
// invariant deadlocks silently. Build with -tags gograph_debug to diagnose a
// suspected freeze. See graph/lpg/reentrancy_disabled.go for the full rationale.
//
// The graph's
// per-shard write methods that fn calls take their own shard locks beneath
// visMu, which is safe because visMu is acquired only here and in View.
//
// Concurrent calls from DIFFERENT goroutines are unaffected: they serialise on
// visMu as before, and the guard never trips on them.
func (g *Graph[N, W]) ApplyAtomically(fn func() error) error {
	// Guard ordering (#1286, #1355): the re-entrancy CHECK runs before Lock so
	// a nested call panics instead of deadlocking, but the writer STAMP is
	// taken only after Lock succeeds — a writer queued on visMu must never
	// overwrite the active writer's identity, or the active writer's nested
	// View/ApplyAtomically would sail past the guard into the deadlock. The
	// clear is deferred after the deferred Unlock, so on the unwind (LIFO) the
	// stamp is removed while the lock is still held and only ever by its
	// owner.
	gid := g.barrier.checkWriter() // panics on re-entry from this goroutine
	g.visMu.Lock()
	g.barrier.stampWriter(gid)
	// Open the adjacency commit window for exactly the visMu-held region so the
	// transaction's adjacency writes clone each touched shard once (then mutate
	// in place) instead of once per op (task #1526). EndCommit must run while
	// visMu is still held — it freezes the per-shard builders — so it is
	// deferred AFTER Unlock to run BEFORE it on the LIFO unwind.
	g.adj.BeginCommit()
	// Allocate the commit record that stamps every version this apply creates,
	// on the same bracket as the adjacency window and for the same reason: this
	// region IS one write transaction. endWrite publishes it, and is deferred
	// LAST so the LIFO unwind runs it FIRST — while the barrier is still held,
	// so the instant a transaction becomes visible does not move. See
	// mvcc_write.go, including why a rolled-back apply publishes too.
	g.beginWrite()
	// ONE deferred call for the whole unwind rather than three. Each open-coded
	// defer costs about a nanosecond of bookkeeping, and adding a fourth for
	// endWrite took this bracket from 9.6 ns to 14.3 ns on an EMPTY apply —
	// most of it the defer, not the atomics. Folding the three into
	// finishWrite gets it back; see that method for the ordering the fold must
	// preserve.
	defer g.visMu.Unlock()
	defer g.finishWrite(gid)
	return fn()
}

// finishWrite is the unwind of a barrier-held write, in the one order that is
// correct: publish the transaction's versions, freeze the adjacency's per-shard
// builders, then release the re-entrancy stamp — all with visMu still held,
// because the caller defers Unlock FIRST so it runs LAST.
//
// Publishing before the builders freeze keeps the commit record live for the
// whole window it stamped. Clearing the stamp under the lock keeps it removable
// only by its owner, which is what stops a queued writer clobbering it (#1286,
// #1355).
func (g *Graph[N, W]) finishWrite(gid int64) {
	g.endWrite()
	// Return the writer's horizon slot after endWrite, so nothing the writer
	// still reads is reclaimable while it runs, and unconditionally, because a
	// bracket that versioned nothing still took a slot (rmp #2299).
	g.releaseWriterSnapshot()
	g.adj.EndCommit()
	g.barrier.clearWriter(gid)
}

// LockBarrier acquires the graph's transaction-visibility write lock and stamps
// the calling goroutine as the barrier holder, identical to [Graph.ApplyAtomically]
// but split into a manual acquire/release pair for callers that need to hold the
// barrier across multiple operations (e.g. an explicit multi-statement transaction
// that must block concurrent readers for its whole lifetime, task #1412).
//
// The caller MUST release the lock with exactly one paired call to
// [Graph.UnlockBarrier], even if an error or panic occurs — failing to do so
// deadlocks the engine. The typical pattern is:
//
//	g.LockBarrier()
//	defer g.UnlockBarrier()
//
// While the lock is held, any operation inside the barrier that needs to run
// under the same lock (e.g. [Engine.execUnderBarrier] called from an in-flight
// Exec) MUST use [Graph.ApplyInsideLocked] instead of [Graph.ApplyAtomically];
// calling ApplyAtomically from the goroutine that holds the barrier via
// LockBarrier panics (re-entrancy guard) — in a build made with -race or
// -tags gograph_debug; a released build deadlocks instead, see
// [Graph.ApplyAtomically].
//
// LockBarrier must not be called from a goroutine already inside the barrier
// (ApplyAtomically or a previous LockBarrier). Under -race or
// -tags gograph_debug it panics instead of deadlocking; a released build
// deadlocks.
// LockBarrier waits for the barrier however long it takes. Prefer
// [Graph.LockBarrierCtx], which bounds the wait by a context.
func (g *Graph[N, W]) LockBarrier() {
	// context.Background() never finishes, so Acquire reduces to the blocking
	// acquire this method has always performed and cannot return an error.
	_ = g.LockBarrierCtx(context.Background())
}

// LockBarrierCtx is [Graph.LockBarrier] with the acquisition bounded by ctx. It
// returns nil once the barrier is held, or ctx's error — wrapping
// [context.Canceled] or [context.DeadlineExceeded] — if ctx finishes first.
//
// On error NOTHING is held and [Graph.UnlockBarrier] must NOT be called; on nil
// the caller owns the barrier and must release it exactly once, as with
// LockBarrier.
//
// The wait exists because [Graph.View] readers hold the barrier's read side for
// the duration of their query, so a writer arriving mid-read queues behind it.
// Before rmp #2174 that wait was unbounded from the caller's point of view: the
// round-3 audit measured Engine.BeginTx with a 50 ms deadline returning after
// 601 ms, and after 11.60 s under load, in both cases with a live transaction
// and err=nil. See internal/ctxlock for how the wait is bounded and why a
// queued acquire cannot simply be abandoned.
func (g *Graph[N, W]) LockBarrierCtx(ctx context.Context) error {
	gid := g.barrier.checkWriter() // panics on re-entry from this goroutine
	if err := ctxlock.Acquire(ctx, g.visMu.TryLock, g.visMu.Lock, g.visMu.Unlock); err != nil {
		return err
	}
	// The stamp records the CALLING goroutine, which is the logical holder even
	// when ctxlock performed the acquire on a helper goroutine: the guard exists
	// to detect same-goroutine nesting, and only the caller runs user code under
	// the barrier.
	g.barrier.stampWriter(gid)
	// Open the adjacency commit window for the whole explicit-transaction
	// lifetime; UnlockBarrier closes it. This makes the window span every
	// statement applied via ApplyInsideLocked under this barrier, so the
	// transaction's adjacency writes share the per-shard clone-once dedup
	// (task #1526). It is opened only on the success path, so a cancelled
	// acquisition leaves no window to close.
	g.adj.BeginCommit()
	// One commit record for the WHOLE explicit transaction, so every statement
	// applied under this barrier via ApplyInsideLocked stamps its versions with
	// it and the transaction publishes atomically. UnlockBarrier closes it.
	g.beginWrite()
	return nil
}

// UnlockBarrier releases the transaction-visibility write lock acquired via
// [Graph.LockBarrier]. It MUST be called from the same goroutine that called
// LockBarrier, and exactly once per LockBarrier call. After this call completes,
// concurrent [Graph.View] readers may proceed and [Graph.ApplyAtomically] may be
// called again from any goroutine.
func (g *Graph[N, W]) UnlockBarrier() {
	gid := g.barrier.currentGID()
	// Publish the transaction's versions while the barrier is still held, so
	// its commit instant is exactly where it has always been. Before EndCommit
	// so the record is live for the whole window it stamped.
	g.endWrite()
	// Return the writer's horizon slot. After endWrite, so nothing it reads is
	// reclaimable while it still runs, and unconditionally, because a bracket
	// that versioned nothing still took a slot.
	g.releaseWriterSnapshot()
	// Close the adjacency commit window while visMu is still held (freezes the
	// per-shard builders), matching the BeginCommit in LockBarrier, before
	// releasing the barrier.
	g.adj.EndCommit()
	g.barrier.clearWriter(gid)
	g.visMu.Unlock()
}

// ApplyInsideLocked is the barrier-already-held variant of [Graph.ApplyAtomically].
// It runs fn directly without acquiring or releasing visMu — the caller MUST already
// hold the barrier via [Graph.LockBarrier]. The re-entrancy guard is NOT re-checked
// (the caller's stamp stays in effect) and the lock is NOT released afterward.
//
// This method exists solely to satisfy callers that hold the barrier for the
// lifetime of an explicit transaction (task #1412) and need to run a sub-operation
// (e.g. one Exec statement) under the same already-held lock. Calling this
// method without first calling LockBarrier yields undefined behaviour.
func (g *Graph[N, W]) ApplyInsideLocked(fn func() error) error {
	return fn()
}

// View runs fn while holding the graph's transaction-visibility read lock.
//
// # It is NO LONGER how the query engine reads (rmp #2274, #2290)
//
// [Engine.Run] used to bracket every query in this, which gave it a consistent
// view by EXCLUDING writers — and that exclusion is what starved readers: a
// write takes the same barrier exclusively, Go's sync.RWMutex prefers a waiting
// writer, and every reader arriving behind one parked for as long as the
// longest in-flight read. A query now takes a SNAPSHOT ([Graph.BeginRead]) and
// no lock at all.
//
// This remains the right primitive for a caller that needs a consistent view of
// structures MVCC does not version — the raw label index, the count store, the
// mapper — or that reads the graph through accessors with no as-of form. It is
// still what the checkpointer and the statistics builder use. Nothing about it
// changed; what changed is that it is no longer on the query hot path.
//
// The original description follows.
//
// so fn observes a consistent snapshot of the graph in which no in-flight
// transaction is partially applied: any transaction committed via
// [Graph.ApplyAtomically] is visible to fn either entirely or not at all,
// and that view is stable for fn's whole duration (snapshot isolation for
// the bracketed reads). Concurrent View readers do not block one another.
//
// Transactional readers that must not observe a partial transaction — the
// query executor's read clauses, and any goroutine reading the mutable
// graph concurrently with writers — should perform their reads inside View.
// Reads issued outside View remain per-operation atomic (the long-standing
// concurrency contract) but may observe a partially-applied multi-op
// transaction; View is what closes that window.
//
// fn must not perform writes and must not call [Graph.ApplyAtomically] or
// [Graph.View] (the RWMutex is not re-entrant). A nested [Graph.View] would
// deadlock the instant any writer queues behind the outer read lock, and a
// nested [Graph.ApplyAtomically] always deadlocks.
//
// Both are CHECKED in builds made with -race or -tags gograph_debug: a nested
// call from a goroutine already inside the barrier panics with a clear message
// instead of deadlocking. The panic indicates a programmer error and is not
// recovered by this package. A released build omits the check — identifying the
// calling goroutine cost a runtime.Stack call measured at 97-99% of this method,
// with a 64 B allocation and no scaling across cores — so there a violation
// deadlocks silently. See graph/lpg/reentrancy_disabled.go.
//
// Concurrent View readers from DIFFERENT goroutines do not block one another and
// never trip the guard; only a same-goroutine nested acquisition does.
func (g *Graph[N, W]) View(fn func()) {
	gid := g.barrier.enterReader() // panics on re-entry from this goroutine
	defer g.barrier.exitReader(gid)
	g.visMu.RLock()
	defer g.visMu.RUnlock()
	fn()
}

// SetValidator installs v as the runtime schema validator for this graph.
// Once set, every call to [Graph.SetNodeProperty] and [Graph.SetEdgeProperty]
// will invoke v.Validate before applying the write; a non-nil error from
// Validate causes the write to be rejected and the error returned to the
// caller.
//
// When v also implements [NodeValidator] (as *schema.Schema does), whole-node
// invariants such as required-property existence are enforced separately, at
// the node-finalisation boundary, via [Graph.ValidateNode]. Per-property
// typing is enforced eagerly here at each [Graph.SetNodeProperty]; existence
// cannot be, because a node acquires its properties one mutation at a time and
// is not complete until finalised.
//
// Pass nil to remove any previously installed validator.
//
// SetValidator is safe for concurrent use.
func (g *Graph[N, W]) SetValidator(v SchemaValidator) { g.validator.store(v) }

// ValidateNode enforces the installed validator's whole-node invariants
// against the current, complete label and property set of the node interned
// under n. It is the node-finalisation hook: a caller building a node (one
// [Graph.AddNode], then any number of [Graph.SetNodeLabel] and
// [Graph.SetNodeProperty] calls) invokes ValidateNode once the node is fully
// populated to reject it when it violates a required-property/existence
// constraint that the per-value [Graph.SetNodeProperty] check cannot detect.
//
// Enforcement is deliberately split from the mutation point. Per-property
// typing is checked eagerly inside [Graph.SetNodeProperty] because a single
// value can be judged in isolation; required-property existence cannot, since
// a legitimate node receives its label before the property that the label
// requires (for example CREATE (:User {email:'a@b'}) sets the User label
// before the email property). Validating existence at the mutation point would
// reject such a node mid-construction, so existence is enforced here instead,
// once the node is finalised.
//
// ValidateNode returns nil when no validator is installed, when the installed
// validator does not implement [NodeValidator], or when the node satisfies
// every whole-node invariant. It does not mutate the graph; on a non-nil
// return the caller is responsible for rolling back or discarding the
// half-built node.
//
// ValidateNode is safe for concurrent use, under the same per-operation
// snapshot contract as [Graph.NodeLabels] and [Graph.NodeProperties]: it reads
// a consistent label set and a consistent property bag, but a writer mutating
// the same node concurrently may change the node between the two reads. Build
// a node to completion before finalising it.
func (g *Graph[N, W]) ValidateNode(n N) error {
	v := g.validator.load()
	if v == nil {
		return nil
	}
	nv, ok := v.(NodeValidator)
	if !ok {
		return nil
	}
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		// n was never interned: there is no node to validate. A caller
		// finalising a node it built always has it interned (AddNode/Set*
		// intern it), so this is the benign "nothing to check" case.
		return nil
	}
	labels := g.NodeLabelsByID(id)
	props := g.NodePropertiesByID(id)
	if props == nil {
		// NodePropertiesByID returns nil for a node with no recorded
		// properties; NodeValidator expects a (possibly empty) map so a
		// required-property check reports the property as missing rather than
		// dereferencing nil.
		props = map[string]PropertyValue{}
	}
	return nv.ValidateNode(labels, props)
}

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

// encodeSlotLabel maps a [LabelID] to its on-slot encoding. The adjacency
// label column reserves 0 for "no label", so the stored value is lid+1. The
// id space is uint32; the +1 bias forbids only the single id math.MaxUint32,
// which would require 2^32 distinct relationship-type names to ever reach.
func encodeSlotLabel(lid LabelID) uint32 { return uint32(lid) + 1 }

// decodeSlotLabel is the inverse of [encodeSlotLabel]. The second return is
// false for the 0 ("no label") sentinel.
func decodeSlotLabel(v uint32) (LabelID, bool) {
	if v == 0 {
		return 0, false
	}
	return LabelID(v - 1), true
}

// slotLabelsForPair scans src's adjacency label column and invokes fn for the
// decoded label of every slot whose neighbour is dstID and that carries a
// label. fn may be called more than once with the SAME id when parallel edges
// happen to share a relationship type; callers that enumerate distinct labels
// must deduplicate. This reads the lock-free adjacency snapshot and takes no
// lock; it is safe for concurrent use.
func (g *Graph[N, W]) slotLabelsForPair(srcID, dstID graph.NodeID, snap *Snapshot, fn func(LabelID)) {
	// ONE entry, so the neighbour and label columns are the same version. The
	// previous form loaded them separately and bounded the scan by the shorter
	// of the two lengths, which kept the index in range but did not make the
	// columns agree; a snapshot read has to.
	//
	// It asks for exactly these two columns rather than the whole
	// [adjlist.EntryView]: copying the five-field view's discarded slice
	// headers measured 21.7 ns per call here, over half of the entire
	// EdgeLabelsByID read.
	nbs, labs := g.entrySlotLabels(srcID, snap)
	if labs == nil {
		return
	}
	n := len(nbs)
	if len(labs) < n {
		n = len(labs)
	}
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		if lid, ok := decodeSlotLabel(labs[i]); ok {
			fn(lid)
		}
	}
}

// entrySlotLabels resolves a node's neighbour and per-slot label columns from
// ONE entry, either as of snap or as they currently stand.
func (g *Graph[N, W]) entrySlotLabels(id graph.NodeID, snap *Snapshot) ([]graph.NodeID, []uint32) {
	startTS, txID, walk := snapshotTimes(snap)
	if !walk {
		return g.adj.LoadEntrySlotLabels(id)
	}
	return g.adj.EntrySlotLabelsAsOf(id, startTS, txID)
}

// clearSlotLabels drops the relationship-type label from every dst-matching
// adjacency slot of src. It is the slot half of [Graph.clearEdgePairState];
// the caller must hold the pair's edge-label shard write lock so the slot and
// overflow halves transition together.
func (g *Graph[N, W]) clearSlotLabels(srcID, dstID graph.NodeID) {
	g.adj.ClearEdgeLabelSlots(srcID, dstID)
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
		horizon: &mvcc.Horizon{},
	}
	for i := range g.nodeLabelShards {
		g.nodeLabelShards[i].m = make(map[graph.NodeID]labelBag)
	}
	// The edge-label overflow maps stay nil until the first label spills there
	// (a multi-label or orphaned-label pair); a single-label graph keeps every
	// relationship type inline in the adjacency slot column and never allocates
	// these sixteen maps.
	for i := range g.nodePropShards {
		g.nodePropShards[i].m = make(map[graph.NodeID]propBag)
	}
	// Edge properties need no per-shard map: they are carried in the adjacency
	// entries' columnar aux blocks, allocated lazily on the first SetEdgeProperty.
	for i := range g.edgeCreateCountShards {
		g.edgeCreateCountShards[i].m = make(map[edgeKey]int64)
	}
	// Register the aux-column factory the fused property-carrying append path
	// (AddEdgeLabeledWithProperty) uses to build a node's FIRST edge-property
	// block: adjlist cannot construct the opaque columnar block itself, so it
	// calls back through this factory. See edge_property_column.go
	// (newEdgePropColsWithValue) and edge_property.go (AddEdgeLabeledWithProperty).
	g.adj.SetAuxFactory(newEdgePropColsAux)
	g.barrier.init()
	// MVCC is ARMED by default (rmp #2288). Every store a read touches carries
	// version chains, every write allocates one shared commit record for them,
	// and the reclamation driver keeps the memory bounded. Turn it off with
	// [Graph.DisableMVCC] to measure the alternative in the same process.
	g.EnableMVCC()
	return g
}

// AdjList returns the underlying adjacency-list backend.
func (g *Graph[N, W]) AdjList() *adjlist.AdjList[N, W] { return g.adj }

// Config returns the [adjlist.Config] the graph was constructed with.
// It delegates to the underlying [adjlist.AdjList.Config]; the
// configuration is fixed at [New] and never mutated, so Config is safe
// to call concurrently with any other operation and always returns the
// same value for the lifetime of the graph. The snapshot writer reads
// it to persist the directed/multigraph shape into the manifest.
func (g *Graph[N, W]) Config() adjlist.Config { return g.adj.Config() }

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
// IndexManager is safe for concurrent use; the pointer is loaded with
// sequential consistency.
func (g *Graph[N, W]) IndexManager() *index.Manager { return g.idxMgr.load() }

// SetIndexManager installs m as the manager of secondary indexes on
// this graph. Passing nil detaches the current manager. The Graph
// retains a borrowed reference to m; the caller owns m's lifetime.
//
// SetIndexManager is safe for concurrent use; the pointer is stored
// with sequential consistency. Goroutines that call [Graph.IndexManager]
// after this store returns will observe m (or a later value).
func (g *Graph[N, W]) SetIndexManager(m *index.Manager) { g.idxMgr.store(m) }

// AddNode inserts n if not already present. The error contract
// matches the underlying [adjlist.AdjList.AddNode]: callers must
// propagate [adjlist.ErrShardFull] when the responsible shard is at
// [adjlist.Config.MaxShardCapacity].
//
// AddNode also clears any tombstone on n: re-creating a node that was
// previously removed via [Graph.RemoveNode] brings it back to life under
// the same stable NodeID (resurrection). This is the single node-
// materialising entry point through which a delete→recreate cycle flows —
// in-process, on WAL replay, and on snapshot apply — so it is the one
// place that must revive. [Graph.SetNodeLabel] does not revive: a
// tombstoned node is never matched by a read clause, so a label can only
// reach a removed key after AddNode has already revived it.
func (g *Graph[N, W]) AddNode(n N) error {
	return g.addNodeInfo(n, nil)
}

// addNodeInfo is [Graph.AddNode] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) addNodeInfo(n N, tx *writeCtx) error {
	// InternNew rather than Intern: MVCC has to know WHEN a node came into
	// existence, so a reader from before that instant does not see it, and this
	// is the one call that can tell without a second map probe on every
	// AddNode. See mvcc_life.go.
	// The birth is recorded UNDER the mapper's write lock, before the id
	// becomes reachable through Walk. Recording it afterwards leaves a window
	// in which a reader finds the node with no birth record and treats it as
	// having existed forever; see [graph.Mapper.InternNewHook].
	//
	// The two arms are spelled out rather than selected into a variable so the
	// autocommit path keeps exactly the shape it had before the transaction was
	// threaded through — a bare method value the compiler can keep off the heap.
	// The hook cannot refuse, and does not need to: InternNewHook fires only for
	// an id NEVER seen before, whose life chain is therefore empty, and an empty
	// chain never conflicts. A delete-then-recreate reaches [Graph.revive]
	// instead, which does propagate a refusal.
	var (
		id      graph.NodeID
		created bool
	)
	if tx == nil {
		id, created = g.adj.Mapper().InternNewHook(n, g.noteNodeBornAutocommit)
	} else {
		id, created = g.adj.Mapper().InternNewHook(n, func(nid graph.NodeID) { g.noteNodeBorn(nid, tx) })
	}
	if created {
		if g.tombstoneActive.Load() == 0 {
			g.reclaimAfterDirectWrite()
			return nil
		}
	}
	// Fast path: no node has ever been removed, so there is nothing to
	// revive. This keeps the common AddNode free of the tombstone lock.
	if g.tombstoneActive.Load() == 0 {
		return nil
	}
	g.revive(id, tx)
	g.reclaimAfterDirectWrite()
	return nil
}

// revive clears any tombstone on id, marking the node live again. It is
// the inverse of [Graph.RemoveNode] and is invoked by [Graph.AddNode] when
// a removed node is re-created. The clear publishes a fresh copy-on-write
// bitmap under tombstoneMu, so it becomes visible atomically to the
// lock-free IsTombstoned / LiveOrder / TombstonedIDs readers.
//
// # When the revival is REFUSED
//
// noteNodeRevived can report a write-write conflict (rmp #2300) — a revival is
// the one birth that can, because the node already carries a death record and
// its life chain is therefore not empty. The conflict is recorded on tx and the
// transaction can no longer commit, so the revival never becomes VISIBLE. The
// tombstone bitmap has already been cleared by then, and is repaired by the
// physical undo log when the statement rolls back (cypher/undo.go).
//
// The check cannot be hoisted ahead of the tombstone clear here: it must run
// under the life shard's lock, and holding that across tombstoneMu would invert
// the order the reclaimer uses. Like [Graph.clearEdgePairState], this path is
// currently unreachable with a non-nil tx — every caller is an autocommit path
// and the engine still writes under the exclusive barrier — and rmp #2304 must
// resolve the ordering when it removes that barrier.
func (g *Graph[N, W]) revive(id graph.NodeID, tx *writeCtx) {
	revived := false
	defer func() {
		// Recorded OUTSIDE the tombstone lock, and after it: noteNodeRevived
		// takes a shard lock of its own, and taking one under the tombstone
		// lock would invert the order the reclaimer uses.
		if revived {
			g.noteNodeRevived(id, tx)
		}
	}()
	g.tombstoneMu.Lock()
	if cur := g.tombstones.Load(); cur != nil && cur.Contains(uint64(id)) {
		next := cur.Clone()
		next.Remove(uint64(id))
		g.tombstones.Store(next)
		g.tombstoneActive.Add(-1)
		revived = true
		// Reviving restores the node's arcs to the live topology, so the same
		// invalidation argument as [Graph.RemoveNode] applies in reverse. Bumping
		// on BOTH transitions is also what makes the generation a sound cache key:
		// the tombstone COUNT is not, because removing one node and reviving
		// another leaves it unchanged while the live set differs.
		g.topoGeneration.Add(1)
	}
	g.tombstoneMu.Unlock()
	// Re-add id to all label bitmaps for labels that survived in the
	// node's label bag. RemoveNode strips those bitmaps when tombstoning;
	// revive must restore them so label-index consumers observe the node
	// again without requiring a SetNodeLabel call for each old label.
	g.restoreLabelBitmaps(id)
}

// Revive clears any tombstone on the node interned under key n, marking it
// live again. It is the exported, key-addressed inverse of [Graph.RemoveNode]
// used by the Cypher executor's transaction-undo path to restore a node that a
// failed write query had tombstoned. No-op when n was never interned or is not
// currently tombstoned. The clear is taken under the same lock as
// [Graph.IsTombstoned]/[Graph.LiveOrder], so it is atomic against those
// readers.
//
// Revive is safe for concurrent use.
func (g *Graph[N, W]) Revive(n N) {
	g.reviveInfo(n, nil)
}

// reviveInfo is [Graph.Revive] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) reviveInfo(n N, tx *writeCtx) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	g.revive(id, tx)
}

// AddEdge inserts a directed edge (mirrored when the graph is
// undirected) from src to dst with weight w. The error contract
// matches the underlying [adjlist.AdjList.AddEdge]: callers must
// propagate [adjlist.ErrShardFull] when the responsible shard is at
// [adjlist.Config.MaxShardCapacity].
//
// AddEdge does NOT revive a tombstoned endpoint: only [Graph.AddNode]
// clears a tombstone. The contract is that callers materialise node
// patterns via AddNode before linking them, so a live edge is never
// created onto a logically-removed node. The query executor upholds
// this (CREATE routes every endpoint through the mutator's AddNode).
func (g *Graph[N, W]) AddEdge(src, dst N, w W) error {
	return g.addEdgeInfo(src, dst, w, nil)
}

// addEdgeInfo is [Graph.AddEdge] with an explicit write transaction; tx is nil
// for a direct Go-API mutation, which is committed the instant it is made and
// takes no conflict check. See [writeCtx].
//
// The append is the COMMUTATIVE adjacency write: it conflicts only with another
// transaction's non-commutative write to the same source, never with another
// append, and it records a stamp regardless so a later removal can see it. The
// rule, and the Memgraph source that settled it, are in [adjVersions].
func (g *Graph[N, W]) addEdgeInfo(src, dst N, w W, tx *writeCtx) error {
	// Checked BEFORE the adjacency mutation so a doomed transaction appends
	// nothing. A node that does not exist yet cannot carry a stamp, so nothing
	// could conflict with it — an append is allowed to create its endpoints.
	if tx != nil {
		if srcID, ok := g.adj.Mapper().Lookup(src); ok {
			if err := g.adjVer.checkAppend(srcID, tx); err != nil {
				return err
			}
		}
		if !g.adj.Directed() {
			if dstID, ok := g.adj.Mapper().Lookup(dst); ok {
				if err := g.adjVer.checkAppend(dstID, tx); err != nil {
					return err
				}
			}
		}
	}
	if err := g.adj.AddEdge(src, dst, w); err != nil {
		return err
	}
	// Stamped AFTER the insert, because an append may CREATE its source and that
	// node's id does not exist until now. Stamping before the insert skipped every
	// edge-creates-its-endpoint write — most of a bulk CREATE — leaving those
	// nodes invisible to a later removal's conflict check. See
	// [adjVersions.checkAppend].
	if tx != nil {
		if srcID, ok := g.adj.Mapper().Lookup(src); ok {
			g.adjVer.stampAppend(srcID, tx)
		}
		if !g.adj.Directed() {
			if dstID, ok := g.adj.Mapper().Lookup(dst); ok {
				g.adjVer.stampAppend(dstID, tx)
			}
		}
	}
	defer g.reclaimAfterDirectWrite()
	// topoGeneration is bumped HERE, in the graph's own mutator, rather than left to
	// callers. Every CSR-position-keyed cache is invalidated by that counter, and a
	// caller that mutates the graph directly through this API -- an embedder holding
	// the *Graph it handed to cypher.NewEngine -- has no reason to know it must bump
	// anything. Leaving it to callers made a committed edge invisible to every
	// subsequent query on that Engine (rmp #2143). The write adapters and store/txn
	// also bump; a double bump is unobservable because the counter is only ever
	// compared for equality. This mirrors PostgreSQL, which calls
	// CacheInvalidateHeapTuple inside heap_insert/heap_delete rather than delegating
	// it to callers.
	g.topoGeneration.Add(1)
	return nil
}

// AddEdgeLabeled inserts a directed edge (mirrored when the graph is
// undirected) from src to dst with weight w and tags it with the
// relationship-type name in a SINGLE adjacency operation: the type is interned
// and written into the edge's inline label slot AT insertion time, instead of
// the two-step [Graph.AddEdge] + [Graph.SetEdgeLabel] which copies the whole
// label column after the append. For a bulk labelled build this restores
// O(degree) amortised cost per source (the fused append is O(1) amortised),
// versus the O(degree²) a per-edge column copy-on-write would cost.
//
// AddEdgeLabeled is the labelled-build fast path. For the simple single-label
// case its observable result is identical to AddEdge followed by SetEdgeLabel:
// the type lands in the first dst-matching inline slot, so [Graph.EdgeLabels],
// [Graph.HasEdgeLabel], the per-slot label scan, and the TCK read path all see
// exactly the same derived label set. To ADD A SECOND distinct type to an
// already-labelled pair, or to (re)label a PRE-EXISTING edge, use
// [Graph.SetEdgeLabel]; that path keeps its general copy-on-write semantics and
// the overflow spill for multi-label pairs.
//
// The coarse src-keyed edge-label index (g.edgeIdx) is updated exactly as
// SetEdgeLabel updates it, so index-driven candidate enumeration is unaffected.
//
// AddEdgeLabeled honours the same error and revival contract as [Graph.AddEdge]:
// it propagates [adjlist.ErrShardFull] and does NOT revive a tombstoned
// endpoint. When the underlying adjacency no-ops the insertion (a simple-graph
// duplicate (src, dst)) the supplied type is not stamped on the existing slot;
// callers that may re-label an existing edge must use SetEdgeLabel.
//
// AddEdgeLabeled is safe for concurrent use.
func (g *Graph[N, W]) AddEdgeLabeled(src, dst N, w W, relType string) error {
	lid := g.reg.Intern(relType)
	if err := g.adj.AddEdgeLabeled(src, dst, w, encodeSlotLabel(lid)); err != nil {
		return err
	}
	// Deferred so BOTH exits bump: this function has an early `return nil` below
	// when the source id cannot be resolved, and the edge is already inserted by
	// then. Registering the bump here rather than before each return keeps the
	// invariant from resting on adjlist interning src before it returns nil — a
	// detail two layers down.
	defer g.topoGeneration.Add(1)
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	g.edgeIdx.Add(uint32(lid), srcID)
	return nil
}

// AddEdgeLabeledWithProperty inserts a directed edge (mirrored when the graph is
// undirected) from src to dst with weight w, tags it with the relationship-type
// name, AND records one property (key, value) on it — all in a SINGLE adjacency
// operation. Both the type and the property value are written into the new edge's
// inline slot AT insertion time, instead of the three-step [Graph.AddEdgeLabeled]
// + [Graph.SetEdgeProperty] whose final step copies the whole per-source property
// column. For a bulk property-carrying build this restores O(degree) amortised
// cost per source (the fused append is O(1) amortised), versus the O(degree²) the
// per-edge column copy-on-write of [Graph.SetEdgeProperty] costs.
//
// AddEdgeLabeledWithProperty is the property-carrying labelled-build fast path.
// Its observable result is identical to AddEdgeLabeled followed by
// SetEdgeProperty for the simple single-edge-per-pair case the bulk builders use:
// the type lands in the first dst-matching inline slot and the value lands on the
// new slot's columnar block, so [Graph.EdgeProperties], [Graph.GetEdgeProperty],
// the per-pair coalesce, and the TCK read path all see exactly the same derived
// state. To set a SECOND property on the edge, or to mutate a PRE-EXISTING edge,
// use [Graph.SetEdgeProperty]; that path keeps its general copy-on-write
// semantics.
//
// If the installed [SchemaValidator] rejects the value the edge is NOT inserted
// and the error is returned (validation runs before any mutation), so the
// fused write keeps the same all-or-nothing contract as a validated
// SetEdgeProperty. AddEdgeLabeledWithProperty otherwise honours the same error
// and revival contract as [Graph.AddEdge]: it propagates [adjlist.ErrShardFull]
// and does NOT revive a tombstoned endpoint. When the underlying adjacency
// no-ops the insertion (a simple-graph duplicate (src, dst)) neither the type nor
// the property is stamped on the existing slot.
//
// A date-shaped string value (a Cypher Date delivered as a SOH-tagged canonical
// string) is folded into the int32 epoch-day column exactly as SetEdgeProperty
// folds it, so it round-trips to a native Date through the Cypher read path.
//
// AddEdgeLabeledWithProperty is safe for concurrent use.
func (g *Graph[N, W]) AddEdgeLabeledWithProperty(src, dst N, w W, relType, key string, value PropertyValue) error {
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	lid := g.reg.Intern(relType)
	keyID := g.pkeys.Intern(key)
	payload := &edgePropPayload{keyID: keyID, value: value}
	if err := g.adj.AddEdgeLabeledWithProp(src, dst, w, encodeSlotLabel(lid), payload); err != nil {
		return err
	}
	// Deferred so BOTH exits bump: this function has an early `return nil` below
	// when the source id cannot be resolved, and the edge is already inserted by
	// then. Registering the bump here rather than before each return keeps the
	// invariant from resting on adjlist interning src before it returns nil — a
	// detail two layers down.
	defer g.topoGeneration.Add(1)
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	g.edgeIdx.Add(uint32(lid), srcID)
	return nil
}

// AddEdgeH inserts a directed edge exactly like [Graph.AddEdge] but first
// allocates a stable per-edge handle for it and stamps that handle onto
// the adjacency slot (via [adjlist.AdjList.AddEdgeH]). It returns the
// handle so the caller can key per-instance edge metadata
// (SetEdgeLabelByHandle / SetEdgePropertyByHandle) by an identity that
// survives sibling-edge deletion, instead of the positional CREATE index
// that the old read path re-derived from CSR slot order.
//
// The returned handle is always non-zero. On the simple-graph collapse of
// a duplicate (src, dst) the underlying adjacency no-ops the slot write
// and the supplied handle is not stored, but a fresh handle value is still
// consumed (monotonicity is a property of the counter, not of storage), so
// callers must treat the handle as advisory in simple-graph mode and keep
// using the per-pair / per-CREATE-index surfaces there. See edge_handle.go.
//
// AddEdgeH honours the same error and revival contract as [Graph.AddEdge].
func (g *Graph[N, W]) AddEdgeH(src, dst N, w W) (handle uint64, err error) {
	h := g.nextEdgeHandle()
	if err := g.adj.AddEdgeH(src, dst, w, h); err != nil {
		return 0, err
	}
	// Invalidate every CSR-position-keyed cache at SOURCE; see [Graph.AddEdge].
	g.topoGeneration.Add(1)
	return h, nil
}

// nextEdgeHandle returns a fresh, never-reused stable edge handle. Handles
// start at 1; 0 is the reserved "no handle" sentinel in the adjacency and
// CSR handle columns. See edge_handle.go for the full contract.
func (g *Graph[N, W]) nextEdgeHandle() uint64 { return g.edgeHandleSeq.Add(1) }

// NextEdgeHandle returns a fresh, never-reused stable edge handle from the
// per-graph monotone counter (the exported form of [Graph.nextEdgeHandle]).
// It is used by the transactional store ([store/txn]) to mint the handle
// stamped onto a durable OpAddEdgeH WAL frame BEFORE the edge is applied,
// so the same handle is written to the log and to the in-memory adjacency.
// Handles start at 1; 0 is the reserved "no handle" sentinel. The counter
// is re-seeded after recovery via [Graph.SeedEdgeHandle] so handles stay
// monotone across a reopen.
//
// NextEdgeHandle is safe for concurrent use.
func (g *Graph[N, W]) NextEdgeHandle() uint64 { return g.nextEdgeHandle() }

// RemoveEdge removes one edge (src, dst) from the adjacency layer (and the
// mirrored (dst, src) edge when the graph is undirected). When this leaves
// the endpoint pair with NO remaining edge — the last parallel edge between
// them is gone — RemoveEdge also strips the per-pair edge labels and edge
// properties, so re-creating an edge between the same endpoints later does
// not resurrect the removed edge's labels or properties (the edge analogue
// of node-tombstone hygiene). While any parallel edge between the pair
// survives, the shared per-pair label and property surfaces are left intact.
//
// RemoveEdge is the edge-deletion entry point used by the Cypher executor
// and WAL replay, so the in-memory state and the recovered state agree.
// Callers that operate purely on adjacency (e.g. search algorithms) may keep
// using [adjlist.AdjList.RemoveEdge] directly; that path does not touch
// labels or properties.
func (g *Graph[N, W]) RemoveEdge(src, dst N) {
	g.removeEdgeInfo(src, dst, nil)
}

// removeEdgeInfo is [Graph.RemoveEdge] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) removeEdgeInfo(src, dst N, tx *writeCtx) {
	defer g.reclaimAfterDirectWrite()
	srcID, srcOK := g.adj.Mapper().Lookup(src)
	dstID, dstOK := g.adj.Mapper().Lookup(dst)

	// An arc removal is the NON-COMMUTATIVE adjacency write: it may not step over
	// another transaction's in-flight append or removal on this source. Checked
	// before anything is captured or mutated, so a doomed transaction leaves the
	// adjacency untouched (rmp #2300). Recorded rather than returned — this
	// primitive is void by contract, which is exactly the case
	// [writeCtx.conflictErr] exists for.
	if srcOK && tx != nil {
		if err := g.adjVer.noteExclusive(srcID, tx); err != nil {
			return
		}
		if !g.adj.Directed() && dstOK {
			if err := g.adjVer.noteExclusive(dstID, tx); err != nil {
				return
			}
		}
	}

	// Capture the per-pair label set BEFORE the adjacency removal. The
	// underlying adjlist removes the first-matching slot, which may be the very
	// slot carrying an inline relationship type; if a parallel edge survives we
	// must re-assert the captured set so removing one parallel edge never drops
	// a label the surviving edges still share (the per-pair coalesced-union
	// contract). Reverse-direction labels are captured too for the undirected
	// case below.
	var fwdLabels, revLabels []LabelID
	// Capture the per-pair PROPERTY maps BEFORE the adjacency removal too. The
	// adjlist removes the first-matching slot, which may be the slot a property
	// was fanned out to; a newly-appended parallel slot is absent until the next
	// SetEdgeProperty, so removing the value-bearing slot could otherwise drop a
	// property the surviving edges still share. Re-asserting the captured map
	// onto the surviving slots re-establishes the lockstep (the property analogue
	// of reassertPairLabels). EdgeProperties returns the coalesced latest-wins map.
	var fwdProps, revProps map[string]PropertyValue
	if srcOK && dstOK {
		fwdLabels = g.pairLabelIDs(srcID, dstID)
		fwdProps = g.EdgeProperties(src, dst)
		if !g.adj.Directed() {
			revLabels = g.pairLabelIDs(dstID, srcID)
			revProps = g.EdgeProperties(dst, src)
		}
	}

	g.adj.RemoveEdge(src, dst)
	// Deferred, not immediate: the bump must follow the LAST write to any
	// epoch-keyed state, and the label/property re-assertion below is such a
	// write. A reader that samples the epoch between an immediate bump and that
	// re-assertion would cache a filter missing a surviving parallel edge's
	// re-asserted type, under the FINAL epoch — F1's "committed change invisible
	// to queries" shape again. Deferring also preserves the error-path skip,
	// because the defer is registered only after the mutation succeeded.
	defer g.topoGeneration.Add(1)

	if g.adj.HasEdge(src, dst) {
		// Parallel edge(s) remain: keep the shared per-pair surfaces. Re-assert
		// any captured labels and properties in case the removed slot was the one
		// holding them.
		if srcOK && dstOK {
			g.reassertPairLabels(srcID, dstID, fwdLabels, tx)
			g.reassertPairProps(src, dst, fwdProps)
			if !g.adj.Directed() {
				g.reassertPairLabels(dstID, srcID, revLabels, tx)
				g.reassertPairProps(dst, src, revProps)
			}
		}
		return
	}
	if !srcOK || !dstOK {
		return
	}
	g.clearEdgePairState(edgeKey{src: srcID, dst: dstID}, tx)
	if !g.adj.Directed() {
		// The undirected edge is fully gone; clear the mirror direction's
		// per-pair surfaces too (a label may have been set under either
		// endpoint order).
		g.clearEdgePairState(edgeKey{src: dstID, dst: srcID}, tx)
	}
}

// RemoveEdgeByHandle removes the single parallel edge instance identified by
// the stable handle on the (src, dst) pair — its adjacency slot (via
// [adjlist.AdjList.RemoveEdgeByHandle]) AND its per-handle label/property
// metadata (via [Graph.RemoveEdgeInstanceByHandle]) — leaving every sibling
// instance's slot, handle, and metadata intact. It returns true when a slot
// carrying handle was removed and false when none matched (already removed,
// wrong handle, or unknown endpoint).
//
// It is the instance-precise analogue of [Graph.RemoveEdge] (which removes the
// FIRST src→dst slot regardless of identity): a Cypher DELETE of a
// specifically-bound parallel-edge instance must retire the EXACT instance,
// not the lowest-indexed one (rmp #2018). Like RemoveEdge it applies edge
// tombstone hygiene on the per-pair coalesced surfaces: the shared per-pair
// labels and properties are captured before the adjacency removal and
// re-asserted onto the survivors when a sibling remains, and cleared when the
// removal leaves the pair fully disconnected (so a later re-add between the
// same endpoints does not resurrect stale labels/properties).
//
// A handle of 0 has no stable identity, so it falls back to [Graph.RemoveEdge]
// (first-match) and returns whether an edge was present — a caller that lost
// the handle still removes one edge rather than silently no-opping.
//
// RemoveEdgeByHandle is the by-handle edge-deletion entry point used by the
// Cypher executor and WAL replay, so the in-memory state and the recovered
// state agree.
func (g *Graph[N, W]) RemoveEdgeByHandle(src, dst N, handle uint64) bool {
	return g.removeEdgeByHandleInfo(src, dst, handle, nil)
}

// removeEdgeByHandleInfo is [Graph.RemoveEdgeByHandle] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) removeEdgeByHandleInfo(src, dst N, handle uint64, tx *writeCtx) bool {
	if handle == 0 {
		had := g.adj.HasEdge(src, dst)
		g.RemoveEdge(src, dst)
		return had
	}

	srcID, srcOK := g.adj.Mapper().Lookup(src)
	dstID, dstOK := g.adj.Mapper().Lookup(dst)

	// A by-handle removal is a non-commutative adjacency write, exactly as
	// [Graph.removeEdgeInfo]'s is, and takes the same check before it mutates
	// anything (rmp #2300). Returns false rather than a typed error because that
	// is this primitive's own signature; the conflict is recorded on tx and the
	// commit refuses it. See [writeCtx.conflictErr].
	if srcOK && tx != nil {
		if err := g.adjVer.noteExclusive(srcID, tx); err != nil {
			return false
		}
		if !g.adj.Directed() && dstOK {
			if err := g.adjVer.noteExclusive(dstID, tx); err != nil {
				return false
			}
		}
	}

	// Capture the per-pair label set and property maps BEFORE the adjacency
	// removal, exactly as [Graph.RemoveEdge] does: the removed slot may be the
	// very one a shared label/property was fanned out to, so re-asserting the
	// captured surfaces onto the survivors preserves the per-pair coalesced-union
	// contract. Reverse-direction captures cover the undirected case below.
	var fwdLabels, revLabels []LabelID
	var fwdProps, revProps map[string]PropertyValue
	if srcOK && dstOK {
		fwdLabels = g.pairLabelIDs(srcID, dstID)
		fwdProps = g.EdgeProperties(src, dst)
		if !g.adj.Directed() {
			revLabels = g.pairLabelIDs(dstID, srcID)
			revProps = g.EdgeProperties(dst, src)
		}
	}

	if !g.adj.RemoveEdgeByHandle(src, dst, handle) {
		return false
	}
	// Deferred so the bump follows the LAST write to epoch-keyed state below; see
	// [Graph.RemoveEdge].
	defer g.topoGeneration.Add(1)
	// Drop the removed instance's per-handle labels and properties. Sibling
	// handles are untouched.
	g.RemoveEdgeInstanceByHandle(src, dst, handle)

	if g.adj.HasEdge(src, dst) {
		// Parallel sibling(s) remain: keep the shared per-pair surfaces alive by
		// re-asserting the captured labels/properties in case the removed slot was
		// the one holding them.
		if srcOK && dstOK {
			g.reassertPairLabels(srcID, dstID, fwdLabels, tx)
			g.reassertPairProps(src, dst, fwdProps)
			if !g.adj.Directed() {
				g.reassertPairLabels(dstID, srcID, revLabels, tx)
				g.reassertPairProps(dst, src, revProps)
			}
		}
		return true
	}
	if !srcOK || !dstOK {
		return true
	}
	g.clearEdgePairState(edgeKey{src: srcID, dst: dstID}, tx)
	if !g.adj.Directed() {
		g.clearEdgePairState(edgeKey{src: dstID, dst: srcID}, tx)
	}
	return true
}

// pairLabelIDs returns the deduplicated label-id set of the directed pair
// (srcID, dstID) — the union of inline slot labels and overflow — under the
// pair's edge-label shard RLock. Used to snapshot a pair's labels before an
// adjacency mutation that may relocate them.
func (g *Graph[N, W]) pairLabelIDs(srcID, dstID graph.NodeID) []LabelID {
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	var ids []LabelID
	seen := func(lid LabelID) bool {
		for _, x := range ids {
			if x == lid {
				return true
			}
		}
		return false
	}
	g.slotLabelsForPair(srcID, dstID, nil, func(lid LabelID) {
		if !seen(lid) {
			ids = append(ids, lid)
		}
	})
	for _, lid := range sh.overflow[k] {
		if !seen(lid) {
			ids = append(ids, lid)
		}
	}
	return ids
}

// reassertPairLabels re-applies every label in ids to the directed pair
// (srcID, dstID), placing each on a surviving inline slot or in overflow. It
// is idempotent: a label already present is a no-op. Called after removing one
// of several parallel edges, when the removed slot might have carried a label
// the surviving edges still share.
// It places each label on ONE free slot, not on every free slot the way
// [Graph.SetEdgeLabel] does. The two are different operations: SetEdgeLabel NAMES
// the pair, so it types every one of the pair's column-typed slots; this only
// REPAIRS a set that the removal may have taken a carrier away from, and the
// surviving slots keep the types they already had. Typing every free slot here
// would spread the removed slot's type onto siblings that never carried it and
// inflate their typed degree.
func (g *Graph[N, W]) reassertPairLabels(srcID, dstID graph.NodeID, ids []LabelID, tx *writeCtx) {
	if len(ids) == 0 {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	for _, lid := range ids {
		g.repairEdgeLabelLocked(k, lid, tx)
	}
	sh.mu.Unlock()
}

// repairEdgeLabelLocked re-establishes lid as carried by the pair k, placing it
// on the FIRST free column-typed slot or, when there is none, in overflow. It is
// the single-carrier counterpart of [Graph.setEdgeLabelLocked] and carries the
// same lock requirement: the caller must hold k's edge-label shard write lock.
// A no-op when the pair already carries lid.
func (g *Graph[N, W]) repairEdgeLabelLocked(k edgeKey, lid LabelID, tx *writeCtx) {
	enc := encodeSlotLabel(lid)
	free, present := g.columnTypedSlots(k.src, k.dst, lid, enc)
	if len(free) > 0 {
		g.adj.SetEdgeLabelSlotsAt(k.src, k.dst, free[:1], enc)
		return
	}
	if present {
		return
	}
	sh := g.edgeLabelShardFor(k)
	if g.addOverflowVersioned(sh, k, lid, tx) {
		g.edgeLabelOverflowActive.Add(1)
	}
}

// reassertPairProps re-applies every property in props to the directed pair
// (src, dst) via [Graph.SetEdgeProperty], fanning each back onto every surviving
// dst-matching adjacency slot. It is the property analogue of
// [Graph.reassertPairLabels], called after removing one of several parallel
// edges when the removed slot might have carried a value the surviving edges
// still share (a newly-appended parallel slot is absent until the next set, so
// the value may have lived only on the removed slot). It is idempotent: writing
// the same value to a slot that already carries it is a no-op. Called only when
// at least one parallel edge survives, so SetEdgeProperty's HasEdge gate passes.
func (g *Graph[N, W]) reassertPairProps(src, dst N, props map[string]PropertyValue) {
	for key, v := range props {
		// The validator already accepted these values when they were first set,
		// and re-asserting cannot introduce a new violation, so a validator error
		// here is not expected; ignore it to keep the removal path total.
		_ = g.SetEdgeProperty(src, dst, key, v)
	}
}

// RemoveAllEdgesFrom removes all edges incident from src in O(d) time for a
// degree-d hub, rather than the O(d²) cost of d sequential [Graph.RemoveEdge]
// calls. After clearing the adjacency layer it also clears the per-pair edge
// state (labels, properties, handles, instance records, CREATE counters) for
// every endpoint pair that src was involved in, exactly as [Graph.RemoveEdge]
// does for each individual edge.
//
// For directed graphs the outgoing edges are removed and their forward per-pair
// state is cleared. For undirected graphs the mirror entries are also removed
// and both directions' per-pair state are cleared.
//
// RemoveAllEdgesFrom is safe for concurrent use.
func (g *Graph[N, W]) RemoveAllEdgesFrom(src N) {
	g.removeAllEdgesFromInfo(src, nil)
}

// removeAllEdgesFromInfo is [Graph.RemoveAllEdgesFrom] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) removeAllEdgesFromInfo(src N, tx *writeCtx) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	// Snapshot the outgoing neighbours BEFORE the bulk removal so we know
	// which per-pair state buckets to clear afterwards.
	nbs, _ := g.adj.LoadEntry(srcID)
	if len(nbs) == 0 {
		return
	}
	dstIDs := make([]graph.NodeID, len(nbs))
	copy(dstIDs, nbs)

	// Bulk-remove from the adjacency layer. For undirected graphs this also
	// removes the mirror entries from each dst's list.
	g.adj.RemoveAllEdgesFrom(src)
	// Deferred so the bump follows the LAST write to epoch-keyed state below; see
	// [Graph.RemoveEdge].
	defer g.topoGeneration.Add(1)

	// Clear per-pair state for every affected endpoint pair.
	for _, dstID := range dstIDs {
		g.clearEdgePairState(edgeKey{src: srcID, dst: dstID}, tx)
		if !g.adj.Directed() {
			g.clearEdgePairState(edgeKey{src: dstID, dst: srcID}, tx)
		}
	}
}

// clearEdgePairState drops the per-pair edge-label and edge-property bags for
// k. The coarse, src-keyed edge label index (g.edgeIdx) is intentionally left
// untouched: it is read only as an over-approximation that the executor
// verifies against the authoritative per-pair labels, so a stale entry can
// cost at most a filtered-out candidate, never a wrong result.
//
// It reports whether the drop was applied. FALSE means tx hit a write-write
// conflict on one of the pair's side stores (rmp #2300): the conflict is
// recorded on the transaction, the store that refused keeps its data, and the
// transaction can no longer commit.
//
// # The atomicity boundary, stated plainly
//
// The four side stores take four separate locks, as they did before detection
// existed, so a refusal part-way leaves the earlier stores dropped and the
// later ones intact. That is not a hole, for two reasons that must BOTH hold:
// the transaction is doomed and none of its versions can become visible, and
// GoGraph rolls a failed statement back PHYSICALLY through the undo log
// (cypher/undo.go), which restores the stored values. Pre-images already
// recorded belong to an aborting transaction and are reclaimed.
//
// It is also currently unreachable with a non-nil tx: every caller is an
// autocommit path and the engine's writes still run under the exclusive
// barrier. When rmp #2304 removes that barrier, this path needs its conflict
// checks HOISTED ahead of the adjacency removal its callers perform first, so a
// refusal costs no physical rollback at all. That hoist is B2's work, not this
// task's, and is recorded here so it is not discovered by accident.
func (g *Graph[N, W]) clearEdgePairState(k edgeKey, tx *writeCtx) bool {
	// Clear both halves of the per-pair label set together under the pair's
	// shard write lock: the overflow entry AND the label on every dst-matching
	// adjacency slot. They are two halves of one logical set and must transition
	// atomically with respect to a concurrent EdgeLabels reader (which takes the
	// same shard RLock), so a re-CREATE of the same endpoints later cannot
	// resurrect a removed edge's relationship type.
	lsh := g.edgeLabelShardFor(k)
	lsh.mu.Lock()
	dropped := g.clearOverflowVersioned(lsh, k, tx)
	g.clearSlotLabels(k.src, k.dst)
	lsh.mu.Unlock()
	if tx.doomed() {
		// clearOverflowVersioned refused; it recorded the conflict and left the
		// overflow list alone.
		return false
	}
	if dropped > 0 {
		g.edgeLabelOverflowActive.Add(int64(-dropped))
	}
	// Edge properties need no explicit per-pair clear: they live ONLY on the
	// adjacency slots, and clearEdgePairState is reached exclusively after the
	// last edge between the pair has been removed (RemoveEdge / RemoveAllEdgesFrom
	// run the adjacency removal first). The removed slot's columnar cells are
	// dropped in lockstep by the adjlist compaction (CompactSlot), so by the time
	// we get here there is no dst-matching slot left carrying the pair's
	// properties — re-creating an edge between the same endpoints starts from an
	// absent column slot, exactly as the old map-delete guaranteed.
	// Drop the stable-handle keyed per-instance metadata for the pair too,
	// matching the per-pair hygiene above: once the last edge between the
	// endpoints is gone, no handle for the pair can be resolved again, so
	// re-creating an edge between the same endpoints must not resurrect a
	// removed edge's per-handle type or properties.
	hlsh := g.edgeHandleLabelShardFor(k)
	hlsh.mu.Lock()
	ok := g.pushHandleLabelVersionsForPair(hlsh, k, tx)
	if ok {
		delete(hlsh.m, k)
	}
	hlsh.mu.Unlock()
	if !ok {
		return false
	}
	hpsh := g.edgeHandlePropShardFor(k)
	hpsh.mu.Lock()
	ok = g.pushHandlePropVersionsForPair(hpsh, k, tx)
	if ok {
		delete(hpsh.m, k)
	}
	hpsh.mu.Unlock()
	if !ok {
		return false
	}
	// Drop the per-CREATE-instance label, property, and multiplicity-counter
	// stores. Without these, re-creating an edge between the same endpoints
	// after RemoveEdge would resurrect the removed edge's per-instance labels
	// and properties, and the CREATE counter would resume from its old value
	// rather than starting fresh at 1.
	ilsh := g.edgeInstanceLabelShardFor(k)
	ilsh.mu.Lock()
	ok = g.pushInstanceLabelVersionsForPair(ilsh, k, tx)
	if ok {
		delete(ilsh.m, k)
	}
	ilsh.mu.Unlock()
	if !ok {
		return false
	}
	ipsh := g.edgeInstancePropShardFor(k)
	ipsh.mu.Lock()
	ok = g.pushInstancePropVersionsForPair(ipsh, k, tx)
	if ok {
		delete(ipsh.m, k)
	}
	ipsh.mu.Unlock()
	if !ok {
		return false
	}
	ccsh := g.edgeCreateCountShardFor(k)
	ccsh.mu.Lock()
	delete(ccsh.m, k)
	ccsh.mu.Unlock()
	return true
}

// EdgeWeight returns the weight of the first edge from src to dst and true when
// such an edge exists, or the zero weight and false otherwise. When several
// parallel edges connect the pair it returns the weight of the first slot, which
// is sufficient for the executor's transaction-undo path: it captures the weight
// of an edge before a failed write query removes it so the inverse [Graph.AddEdge]
// restores the same weight.
//
// EdgeWeight performs an O(out-degree) scan of src's adjacency and allocates
// nothing. It is safe for concurrent use under the same lock-free adjacency
// snapshot contract as [adjlist.AdjList.LoadEntry].
//
// For a weightless graph (adjlist.Config.Weightless) the adjacency carries no
// weights column, so a present edge reports the zero value of W with ok=true.
func (g *Graph[N, W]) EdgeWeight(src, dst N) (W, bool) {
	return g.EdgeWeightAsOf(src, dst, nil)
}

// EdgeWeightAsOf is [Graph.EdgeWeight] as the pair stood at snap. A nil
// snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgeWeightAsOf(src, dst N, snap *Snapshot) (W, bool) {
	var zero W
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return zero, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return zero, false
	}
	v := g.EntryViewAsOf(srcID, snap)
	nbs, ws := v.Neighbours, v.Weights
	for i, nb := range nbs {
		if nb == dstID {
			// A weightless graph (adjlist.Config.Weightless) carries no weights
			// column, so ws is nil; the edge exists with the zero weight.
			if ws == nil {
				return zero, true
			}
			return ws[i], true
		}
	}
	return zero, false
}

// SetNodeLabel attaches label to n, inserting n if needed. Returns
// the error from the underlying [adjlist.AdjList.AddNode] (which can
// only happen via a future bounded-growth implementation); the
// current [adjlist.AdjList.AddNode] never fails, so callers in
// codepaths that do not configure [adjlist.Config.MaxShardCapacity]
// may safely ignore the return.
func (g *Graph[N, W]) SetNodeLabel(n N, name string) error {
	err := g.setNodeLabelInfo(n, name, nil)
	g.reclaimAfterDirectWrite()
	return err
}

// setNodeLabelInfo is [Graph.SetNodeLabel] with an explicit commit record.
//
// info is the transaction's shared record, or nil for an autocommit write —
// which is what the exported method passes, matching the single-statement
// transaction semantics the Go API already documents. It is threaded rather
// than looked up so a transaction's deltas all point at ONE record and its
// commit is a single store (rmp #2278).
func (g *Graph[N, W]) setNodeLabelInfo(n N, name string, tx *writeCtx) error {
	if err := g.adj.AddNode(n); err != nil {
		return err
	}
	id, _ := g.adj.Mapper().Lookup(n)
	lid := g.reg.Intern(name)
	sh := g.nodeLabelShardFor(id)
	sh.mu.Lock()
	// labelBag is stored by value: read it out, mutate, write it back under the
	// shard lock. The write-back is load-bearing — add may grow or promote the
	// bag's backing storage, so the updated header must be re-stored.
	bag := sh.m[id]
	// P0 MVCC spike (rmp #2275), inert unless armed: record the undo BEFORE
	// mutating, and only when the label is not already present — re-asserting a
	// label the node already carries changes nothing, so a delta for it would be
	// a version that never existed. MERGE's MATCH branch re-asserts labels on
	// every match, so this guard is what keeps a delta per WRITE rather than a
	// delta per statement.
	if g.labelDeltasEnabled() && !bag.has(lid) {
		// Write-write conflict (rmp #2300), tested against THIS transaction's
		// snapshot — which travels in tx rather than being looked up on the
		// graph, so a concurrent writer is never tested against a transaction
		// that is not its own (rmp #2301). Checked before deltaStamp so a
		// doomed write allocates no commit record.
		if head := sh.headStamp(id); tx.conflicts(head) {
			sh.mu.Unlock()
			return tx.conflictErr("node labels", head)
		}
		ci, ts := g.deltaStamp(tx.record())
		sh.pushLabelDelta(id, undoRemoveLabel, lid, ci, ts, &g.labelDeltaActive)
	}
	bag.add(lid)
	sh.m[id] = bag
	sh.mu.Unlock()
	g.nodeIdx.Add(uint32(lid), id)
	// Withdraw any pending removal for this entry. Re-attaching a label the
	// same statement stripped is what a ROLLBACK does, and a surviving deferred
	// removal would delete the restored entry at the next sweep.
	g.cancelDeferredIndexRemoval(uint32(lid), id)
	return nil
}

// RemoveNode marks the node n as removed. Subsequent reads through
// IsTombstoned / LiveOrder / TombstonedIDs treat n as absent. The underlying
// Mapper retains the slot (NodeID stability is a hard contract), but
// label, property, and adjacency reads on the tombstoned id remain
// safe; callers should also strip labels / properties / incident
// edges before calling RemoveNode so the tombstone reflects the
// fully-deleted node state. No-op when n was never interned or is
// already tombstoned.
func (g *Graph[N, W]) RemoveNode(n N) {
	g.removeNodeInfo(n, nil)
}

// removeNodeInfo is [Graph.RemoveNode] with an explicit write transaction; tx is nil
// for a direct Go-API mutation, which is committed the instant it is made and takes
// no conflict check. See [writeCtx].
func (g *Graph[N, W]) removeNodeInfo(n N, tx *writeCtx) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	died := false
	defer func() {
		// Outside the tombstone lock, for the lock-order reason given on
		// [Graph.revive]. A reader older than this instant must still SEE the
		// node, and the bitmap alone cannot tell it that.
		if died {
			g.noteNodeDied(id, tx)
			g.reclaimAfterDirectWrite()
		}
	}()
	g.tombstoneMu.Lock()
	cur := g.tombstones.Load()
	if cur == nil || !cur.Contains(uint64(id)) {
		died = true
		var next *roaring64.Bitmap
		if cur == nil {
			next = roaring64.New()
		} else {
			next = cur.Clone()
		}
		next.Add(uint64(id))
		g.tombstones.Store(next)
		g.tombstoneActive.Add(1)
		// Tombstoning changes the LIVE edge topology every CSR-position-keyed
		// cache is derived from: csr.BuildFromAdjListLive omits the arcs incident
		// to a tombstoned node, so a cache built before this call describes a
		// graph that no longer exists. Bump the generation for exactly the reason
		// [Graph.BumpTopoGeneration] documents — a caller reaching RemoveNode
		// directly through the Go API has no enclosing Cypher statement to
		// attribute an edge-removal counter to, but the live topology still
		// changed. Without this a cached CSR (or edge-type filter) would serve
		// ghost edges for a logically-deleted node, which is the #1790 defect the
		// live filter exists to prevent.
		g.topoGeneration.Add(1)
	}
	g.tombstoneMu.Unlock()
	// Strip id from every label bitmap so label-index consumers see the
	// node as absent without consulting IsTombstoned (task #1409,
	// option a). This is a no-op when the caller already removed labels
	// via RemoveNodeLabel (the Cypher executor delete path), and
	// correct when RemoveNode is called directly via the Go API without
	// prior label removal.
	g.stripLabelBitmaps(id)
}

// stripLabelBitmaps removes id from every label bitmap that records it.
// Called by RemoveNode to keep nodeIdx exact so consumers outside the
// Cypher executor do not need to consult IsTombstoned (task #1409).
func (g *Graph[N, W]) stripLabelBitmaps(id graph.NodeID) {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	lids := make([]LabelID, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		lids = append(lids, lid)
	})
	sh.mu.RUnlock()
	for _, lid := range lids {
		// DEFERRED while versioning is armed: a reader older than the removal
		// must still find this node in the label bitmap, or it silently loses a
		// row. See mvcc_index.go.
		if !g.deferLabelIndexRemoval(uint32(lid), id) {
			g.nodeIdx.Remove(uint32(lid), id)
		}
	}
}

// restoreLabelBitmaps re-adds id to every label bitmap for labels still
// in the node's label bag. It is the inverse of [Graph.stripLabelBitmaps],
// called when a tombstoned node is revived via [Graph.AddNode]: the label
// bag survives tombstoning (only the bitmap is stripped), so reviving the
// node must restore those entries so label-index consumers observe the
// node again (task #1409).
func (g *Graph[N, W]) restoreLabelBitmaps(id graph.NodeID) {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	lids := make([]LabelID, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		lids = append(lids, lid)
	})
	sh.mu.RUnlock()
	for _, lid := range lids {
		g.nodeIdx.Add(uint32(lid), id)
	}
}

// TombstonedIDs returns the NodeIDs currently marked removed via
// [Graph.RemoveNode], in ascending order. The result is a fresh slice the
// caller owns; an empty (never-deleted) graph returns a zero-length slice.
// Used by the snapshot writer to persist the tombstone set durably so node
// deletions survive a store reopen.
//
// TombstonedIDs is safe for concurrent use: it loads the immutable
// published bitmap once and reads it without any lock.
func (g *Graph[N, W]) TombstonedIDs() []graph.NodeID {
	bm := g.tombstones.Load()
	if bm == nil {
		return []graph.NodeID{}
	}
	// roaring64 stores ids in ascending order, so ToArray already yields the
	// ascending sequence the snapshot writer expects — no explicit sort.
	arr := bm.ToArray()
	out := make([]graph.NodeID, len(arr))
	for i, v := range arr {
		out[i] = graph.NodeID(v)
	}
	return out
}

// TombstoneCount returns the number of NodeIDs currently marked removed.
// It reads a lock-free counter, so it is cheap enough to gate the optional
// emission of the snapshot tombstone component on every checkpoint.
//
// TombstoneCount is safe for concurrent use.
func (g *Graph[N, W]) TombstoneCount() int { return int(g.tombstoneActive.Load()) }

// OutDegree returns the number of out-neighbours of src that a traversal would
// visit, without enumerating them. ok is false when src is not interned; a node
// with no outgoing edges reports (0, true).
//
// It exists so a degree-answerable question — "does this node have any outgoing
// :KNOWS edge?", "how many does it have?" — costs a counter read rather than an
// expansion. The audit that motivated it measured `COUNT { (a)-[:K]->(:P) } > 0`
// at 88× a bare label scan per outer row, because the count was reached by
// enumerating every neighbour in order to compare against zero.
//
// # Live-node semantics
//
// Unlike [github.com/FlavioCFOliveira/GoGraph/graph/adjlist.AdjList.OutDegree],
// which counts adjacency slots, this method excludes edges whose far endpoint has
// been tombstoned. That is the difference that makes it substitutable for a
// traversal: [Graph.RemoveNode] tombstones a node and strips it from the label
// bitmaps but does NOT remove the incident edges other nodes hold, so a raw slot
// count would include an edge to a node the query layer treats as absent.
//
// For an UNDIRECTED graph the result is the node's full degree, because the
// adjacency mirrors insertion. For a DIRECTED graph it is the out-degree only:
// in-degree is not an adjacency-local quantity and is served by the reverse CSR.
//
// # Cost
//
// O(1) when the graph holds no tombstones, which is the common case — the
// tombstone set is consulted through one lock-free counter, and on zero the
// adjacency column length is returned directly. O(d) in the node's degree once
// anything has been deleted, since each far endpoint must then be checked.
//
// # Concurrency
//
// Safe for concurrent use with readers and writers, and lock-free.
func (g *Graph[N, W]) OutDegree(src N) (int, bool) {
	if g.tombstoneActive.Load() == 0 {
		return g.adj.OutDegree(src)
	}
	return g.outDegreeFiltered(src, false, 0)
}

// OutDegreeByType is [Graph.OutDegree] restricted to edges whose relationship
// type is relType. ok is false when src is not interned.
//
// # Cost
//
// O(d) in the node's degree: the relationship type of each slot must be resolved
// to decide which edges match. No allocation, and — on a graph with no overflow
// labels, which is every Cypher-built one — no lock and no access beyond src's own
// columns and the per-handle records of its typed slots.
//
// It routes through the same walk as [Graph.OutDegreeByTypeBoundedByID] rather
// than reading the adjacency label column directly. The column alone is not the
// whole per-slot truth — a Cypher-created edge records its type against its
// stable handle — so the column-only count reported 1 for three Cypher-created
// parallel :K edges where 3 was correct, and 0 for a type that had spilled to the
// pair's overflow list. Sharing the walk is what makes the bounded and unbounded
// forms unable to disagree about WHICH edges count, which is the contract their
// documentation rests on (rmp #2241/#2258).
func (g *Graph[N, W]) OutDegreeByType(src N, relType LabelID) (int, bool) {
	return g.outDegreeFiltered(src, true, relType)
}

// outDegreeFiltered counts src's out-edges excluding tombstoned endpoints, and
// when byType is set also restricting to relType.
//
// For the UNTYPED count it is the slow path, taken only once the graph holds at
// least one tombstone; [Graph.OutDegree] answers from the column length otherwise.
// For the TYPED count it is the only path: resolving a slot's relationship type
// needs the slot's handle as well as its column entry, so there is no O(1)
// shortcut to fall back from (see [Graph.OutDegreeByType]).
//
// It counts by iterating src's neighbours through the adjacency's own iterator,
// so the population it walks is exactly the one [Graph.Neighbours] exposes and
// the two cannot drift apart.
func (g *Graph[N, W]) outDegreeFiltered(src N, byType bool, relType LabelID) (int, bool) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return 0, false
	}
	// Routed through the same body as the bounded walkers so a typed count is
	// resolved by HANDLE here too — otherwise the unbounded and bounded forms
	// would disagree about parallel edges, which is exactly the drift their
	// shared-predicate contract rules out (rmp #2241).
	return g.outDegreeMatchingByID(srcID, relType, byType, maxInt, nil, nil)
}

// OutDegreeByID is [Graph.OutDegree] keyed by an already-resolved
// [graph.NodeID]. Tombstoned far endpoints are excluded exactly as
// [Graph.OutDegree] excludes them, through the same predicate.
//
// It exists for the query layer, which holds ids and would otherwise pay an
// id → node-value → id round-trip (an array read plus a string hash) on every
// call; see [github.com/FlavioCFOliveira/GoGraph/graph/adjlist.AdjList.OutDegreeByID]
// for the measurement that motivated it.
//
// # Cost
//
// O(1) when the graph holds no tombstones; O(d) once anything has been deleted.
// A caller that only needs to know whether the degree reaches some bound should
// use [Graph.OutDegreeBoundedByID], which does not pay the full O(d) walk.
//
// # Concurrency
//
// Safe for concurrent use with readers and writers, and lock-free.
func (g *Graph[N, W]) OutDegreeByID(srcID graph.NodeID) (int, bool) {
	return g.OutDegreeBoundedByID(srcID, maxInt)
}

// OutDegreeBoundedByID is [Graph.OutDegreeByID] with an early exit: it returns
// min(trueLiveOutDegree, limit) and stops as soon as limit live out-edges have
// been counted. A non-positive limit returns 0 without inspecting any edge, the
// correct answer for "at most zero".
//
// It is the untyped counterpart of [Graph.OutDegreeByTypeBoundedByID], and it
// exists because the untyped degree is only O(1) while the graph is free of
// tombstones. Once ANY node has been deleted — anywhere in the graph, related or
// not — the count has to exclude edges landing on a tombstone, which means
// walking the node's adjacency. Without a limit to stop it, that walk runs to the
// end of a supernode's column to answer a question a bounded caller settled after
// one edge: rmp #2265 measured `EXISTS { (a)-->() }` at degree 400 000 going from
// 2 µs to 2.243 ms — 1170×, permanent and graph-wide — after a single unrelated
// DELETE, purely because the caller's limit was replaced with maxInt here.
//
// # The cap counts LIVE edges only
//
// The limit is charged per edge that SURVIVES the liveness gate, never per slot
// inspected. That distinction is the whole correctness of the bound: a node whose
// first slots all point at tombstoned neighbours must keep walking past them, and
// a cap that counted slots would stop early and report a degree of zero for a node
// that has live edges further along its column. The gate and the cap are
// independent concerns, and [github.com/FlavioCFOliveira/GoGraph/graph/adjlist.AdjList.OutDegreeFuncBoundedByID]
// keeps them so.
//
// # Cost
//
// O(1) when the graph holds no tombstones; O(min(d, limit)) once anything has
// been deleted, where the min is taken over LIVE edges as described above — a
// column of tombstoned slots is still walked through.
//
// # Concurrency
//
// Safe for concurrent use with readers and writers, and lock-free.
func (g *Graph[N, W]) OutDegreeBoundedByID(srcID graph.NodeID, limit int) (int, bool) {
	return g.OutDegreeBoundedByIDAsOf(srcID, limit, nil)
}

// OutDegreeBoundedByIDAsOf is [Graph.OutDegreeBoundedByID] as the node stood at
// snap. A nil snapshot reads the current value.
//
// The tombstone gate is NOT yet versioned: a node deleted after this reader
// started is still excluded. That is the candidate-set gap P4c (rmp #2290)
// closes; until then the visibility barrier is what keeps a read from
// straddling it.
//
// Safe for concurrent use.
func (g *Graph[N, W]) OutDegreeBoundedByIDAsOf(srcID graph.NodeID, limit int, snap *Snapshot) (int, bool) {
	if limit <= 0 {
		return 0, true
	}
	if snap == nil && g.tombstoneActive.Load() == 0 {
		n, ok := g.adj.OutDegreeByID(srcID)
		if !ok {
			return 0, false
		}
		return min(n, limit), true
	}
	if snap != nil {
		v := g.EntryViewAsOf(srcID, snap)
		if _, interned := g.adj.Mapper().Resolve(srcID); !interned {
			return 0, false
		}
		n := 0
		for _, dst := range v.Neighbours {
			if g.IsTombstoned(dst) {
				continue
			}
			n++
			if n >= limit {
				break
			}
		}
		return n, true
	}
	return g.adj.OutDegreeFuncBoundedByID(srcID, limit, func(dst graph.NodeID, _ uint32) bool {
		return !g.IsTombstoned(dst)
	})
}

// OutDegreeByTypeBoundedByID is [Graph.OutDegreeByTypeBounded] keyed by an
// already-resolved [graph.NodeID]. See [Graph.OutDegreeByID].
func (g *Graph[N, W]) OutDegreeByTypeBoundedByID(srcID graph.NodeID, relType LabelID, limit int) (int, bool) {
	return g.outDegreeMatchingByID(srcID, relType, true, limit, nil, nil)
}

// OutDegreeByTypeBoundedByIDAsOf is [Graph.OutDegreeByTypeBoundedByID] as the
// node stood at snap. A nil snapshot reads the current value.
//
// Safe for concurrent use.
func (g *Graph[N, W]) OutDegreeByTypeBoundedByIDAsOf(srcID graph.NodeID, relType LabelID, limit int, snap *Snapshot) (int, bool) {
	return g.outDegreeMatchingByID(srcID, relType, true, limit, nil, snap)
}

// slotCarriesTypeAsOf is [Graph.slotCarriesType] as the slot stood at snap. All
// three sources it consults are versioned, so the precedence between them —
// a handle record is authoritative, else the column, else the pair's
// overflow — is resolved at the reader's own instant rather than at the
// writer's.
func (g *Graph[N, W]) slotCarriesTypeAsOf(srcID, dst graph.NodeID, handle uint64, lbl, want uint32, relType LabelID, snap *Snapshot) bool {
	if has, known := g.edgeHandleHasLabelAsOf(srcID, dst, handle, relType, snap); known {
		return has
	}
	if lbl == want {
		return true
	}
	return g.pairOverflowHasTypeAsOf(srcID, dst, relType, snap)
}

// pairOverflowHasTypeAsOf is [Graph.pairOverflowHasType] as the pair stood at
// snap.
//
// The gate keeps its two halves for two different reasons: with no snapshot a
// graph that has never spilled a type takes no lock at all, and with one the
// version counter must be consulted too, because a type PRESENT for this reader
// may already have been removed from the live list.
func (g *Graph[N, W]) pairOverflowHasTypeAsOf(srcID, dstID graph.NodeID, lid LabelID, snap *Snapshot) bool {
	if g.edgeLabelOverflowActive.Load() == 0 &&
		(snap == nil || g.edgeLabelVersionActive.Load() == 0) {
		return false
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	has := false
	for _, x := range g.overflowLabelsAsOf(sh, k, snap) {
		if x == lid {
			has = true
			break
		}
	}
	sh.mu.RUnlock()
	return has
}

// outDegreeMatchingByID is the shared walk behind [Graph.OutDegreeByTypeBoundedByID]
// and [Graph.OutDegreeMatchingBoundedByID]: it counts srcID's out-edges that
// pass the type gate, the liveness gate and, when farOK is non-nil, the caller's
// far-node predicate, stopping once limit have been counted.
//
// It walks the handle-carrying entry ([AdjList.LoadEntryH]) rather than going
// through [AdjList.OutDegreeFuncBoundedByID], because resolving a slot's type
// correctly requires the slot's HANDLE and that walk does not expose one.
//
// # Hoisted type gates
//
// [Graph.slotCarriesType] consults three sources per slot, but two of them are
// properties of the ENTRY (or of the whole graph) rather than of the slot, and are
// read once here:
//
//   - handles == nil means no slot of this entry carries a stable handle, so the
//     handle store cannot have a record for any of them.
//   - edgeLabelOverflowActive == 0 means no pair in the graph has an overflow
//     label, so the overflow half cannot contribute for any of them.
//
// When neither can contribute, the per-slot test is provably just `lbl == want`,
// and inlining it keeps the walk a single comparison per slot. That matters: going
// through the general resolver unconditionally measured ~11× slower per slot on a
// 4096-slot labelled hub than the column-only scan it replaced, for a graph on
// which the general resolver could not possibly return a different answer.
func (g *Graph[N, W]) outDegreeMatchingByID(
	srcID graph.NodeID,
	relType LabelID,
	typed bool,
	limit int,
	farOK func(dst graph.NodeID) bool,
	snap *Snapshot,
) (int, bool) {
	if limit <= 0 {
		return 0, true
	}
	if _, interned := g.adj.Mapper().Resolve(srcID); !interned {
		return 0, false
	}
	// ONE entry, so the neighbour, handle and label columns are the same
	// version. The previous form loaded LoadEntryH and LoadEntryLabels
	// separately, which under a snapshot could pair a handle with a label from
	// a different instant.
	v := g.EntryViewAsOf(srcID, snap)
	nbs, handles, labs := v.Neighbours, v.Handles, v.Labels
	want := encodeSlotLabel(relType) // see OutDegreeByType on the encoding
	live := g.tombstoneActive.Load() != 0
	// See "Hoisted type gates" above. Read before the loop, never inside it.
	columnOnly := typed && handles == nil && g.edgeLabelOverflowActive.Load() == 0

	// Specialised scan for the shape a plain typed degree actually asks for: the
	// column is the whole truth, nothing is tombstoned, there is no far-node
	// predicate, and the cap cannot bind. It reads ONE 4-byte column sequentially
	// with no per-slot bound check, cap check or neighbour load, which is what the
	// column-only count this method replaced did — the general loop below touches
	// three times the bytes per slot and measured ~4.8× slower on a 4096-slot hub.
	if columnOnly && !live && farOK == nil {
		m := min(len(nbs), len(labs))
		if limit >= m {
			n := 0
			for _, lbl := range labs[:m] {
				if lbl == want {
					n++
				}
			}
			return n, true
		}
	}

	n := 0
	for i, dst := range nbs {
		if typed {
			var lbl uint32
			if i < len(labs) {
				lbl = labs[i]
			}
			if columnOnly {
				if lbl != want {
					continue
				}
			} else {
				var handle uint64
				if i < len(handles) {
					handle = handles[i]
				}
				if !g.slotCarriesTypeAsOf(srcID, dst, handle, lbl, want, relType, snap) {
					continue
				}
			}
		}
		if live && g.IsTombstoned(dst) {
			continue
		}
		if farOK != nil && !farOK(dst) {
			continue
		}
		n++
		if n >= limit {
			break
		}
	}
	return n, true
}

// OutDegreeMatchingBoundedByID counts srcID's out-edges whose far endpoint
// satisfies farOK, optionally restricted to one relationship type, capped at
// limit. It returns min(trueMatchingCount, limit); ok is false when srcID is not
// interned.
//
// When typed is false relType is ignored and every out-edge is offered to farOK.
//
// It exists for a caller that must qualify the FAR NODE — the cypher engine
// counting `(a)-[:K]->(:P)` without materialising a neighbour (rmp #2235). A
// plain degree cannot answer that: a degree counts every out-edge and has no
// way to ask anything about where the edge lands. Pushing the predicate in here
// rather than exposing the raw adjacency walk keeps the TOMBSTONE GATE in one
// place — a caller that assembled this from [AdjList.OutDegreeFuncBoundedByID]
// would have to re-derive the liveness rule and could drift from
// [Graph.OutDegreeByType], which is precisely the disagreement about WHICH edges
// count that the bounded/unbounded pair is documented to rule out.
//
// farOK is called at most once per out-edge, in adjacency order, and only for
// edges that already passed the type and liveness gates — so it never sees a
// tombstoned endpoint and need not re-check one.
//
// # Cost
//
// O(min(d, limit)) in the node's degree, plus the cost of farOK. No allocation.
//
// # Concurrency
//
// Safe for concurrent use with readers and writers, and lock-free, on the same
// terms as [Graph.OutDegreeByTypeBoundedByID].
func (g *Graph[N, W]) OutDegreeMatchingBoundedByID(
	srcID graph.NodeID,
	relType LabelID,
	typed bool,
	limit int,
	farOK func(dst graph.NodeID) bool,
) (int, bool) {
	return g.OutDegreeMatchingBoundedByIDAsOf(srcID, relType, typed, limit, farOK, nil)
}

// OutDegreeMatchingBoundedByIDAsOf is [Graph.OutDegreeMatchingBoundedByID] as
// the node stood at snap. A nil snapshot reads the current value.
//
// Safe for concurrent use.
func (g *Graph[N, W]) OutDegreeMatchingBoundedByIDAsOf(
	srcID graph.NodeID,
	relType LabelID,
	typed bool,
	limit int,
	farOK func(dst graph.NodeID) bool,
	snap *Snapshot,
) (int, bool) {
	if farOK == nil {
		return 0, false
	}
	return g.outDegreeMatchingByID(srcID, relType, typed, limit, farOK, snap)
}

// maxInt is the effectively-unbounded limit for the bounded degree walkers, used
// where the caller wants every matching edge counted but still needs the
// id-keyed, predicate-carrying walk.
const maxInt = int(^uint(0) >> 1)

// OutDegreeByTypeBounded is [Graph.OutDegreeByType] capped at limit: it stops as
// soon as limit matching edges have been counted and returns
// min(trueDegree, limit). ok is false when src is not interned.
//
// It answers a comparison of a typed degree against a small literal without
// walking a high-degree node to the end. The untyped degree has no bounded
// companion because it is already O(1) — there is nothing to stop early.
//
// Tombstoned far endpoints are excluded exactly as [Graph.OutDegreeByType]
// excludes them, through the same predicate, so a bounded and an unbounded count
// can never disagree about WHICH edges count — only about when they stop
// counting.
//
// # Cost
//
// O(min(d, limit)) in the node's degree. No allocation.
//
// # Concurrency
//
// Safe for concurrent use with readers and writers, and lock-free.
func (g *Graph[N, W]) OutDegreeByTypeBounded(src N, relType LabelID, limit int) (int, bool) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return 0, false
	}
	return g.outDegreeMatchingByID(srcID, relType, true, limit, nil, nil)
}

// HasConstraints reports whether the cypher engine currently has any schema
// constraint registered on this graph. It reads a lock-free counter maintained
// by the engine (SetActiveConstraintCount), so it is cheap enough for the
// checkpointer to consult on every checkpoint to gate the constraints.bin
// self-sufficiency requirement (#1464).
//
// HasConstraints is safe for concurrent use.
//
// It reports true when EITHER the engine-maintained count
// (SetActiveConstraintCount) OR the store-direct count
// (AddStoreConstraint, maintained by the txn.Store apply path) is positive,
// so the checkpoint fail-safe is correct whether the constraint was declared
// through the cypher engine or directly through txn.Tx.CreateConstraint
// (#1756).
func (g *Graph[N, W]) HasConstraints() bool {
	return g.constraintActive.Load() > 0 || g.storeConstraintActive.Load() > 0
}

// storeConstraintKey identifies one store-direct constraint slot. Name is
// deliberately excluded so re-declaring a constraint on the same (kind, label,
// property) under a different name updates the one slot rather than adding a
// second — matching the recovery accumulator's dedup identity.
type storeConstraintKey struct {
	label    string
	property string
	kind     uint8
}

// AddStoreConstraint records that a schema constraint of the given kind on
// (label, property) is declared through the txn.Store-direct API. It is the
// store-layer dual of the cypher engine's syncConstraintCount: the txn.Store
// commit-apply path calls it for every committed OpCreateConstraint so that
// Graph.HasConstraints reports the constraint to a WAL-truncating checkpoint,
// independent of whether a cypher engine is wired in (#1756).
//
// The (kind, label, property) key makes re-declaring the same constraint
// idempotent — the active count never over-counts a single durable
// constraint, the only direction that could let a checkpoint silently drop it.
//
// AddStoreConstraint is safe for concurrent use.
func (g *Graph[N, W]) AddStoreConstraint(kind uint8, labelName, property string) {
	key := storeConstraintKey{kind: kind, label: labelName, property: property}
	g.storeConstraintMu.Lock()
	if g.storeConstraints == nil {
		g.storeConstraints = make(map[storeConstraintKey]struct{}, 1)
	}
	if _, dup := g.storeConstraints[key]; !dup {
		g.storeConstraints[key] = struct{}{}
		g.storeConstraintActive.Add(1)
	}
	g.storeConstraintMu.Unlock()
}

// RemoveStoreConstraint drops the store-direct constraint slot identified by
// (kind, label, property), the dual of [Graph.AddStoreConstraint] for a
// committed OpDropConstraint. Dropping a constraint that was never recorded is
// a no-op, so a DROP that suppresses a CREATE folded away by a prior checkpoint
// cannot drive the active count negative.
//
// RemoveStoreConstraint is safe for concurrent use.
func (g *Graph[N, W]) RemoveStoreConstraint(kind uint8, labelName, property string) {
	key := storeConstraintKey{kind: kind, label: labelName, property: property}
	g.storeConstraintMu.Lock()
	if _, ok := g.storeConstraints[key]; ok {
		delete(g.storeConstraints, key)
		g.storeConstraintActive.Add(-1)
	}
	g.storeConstraintMu.Unlock()
}

// StoreConstraint is a durable schema-constraint slot recorded on the graph by
// the txn.Store apply path or by recovery (see [Graph.AddStoreConstraint]). It
// carries the constraint's enforcement identity — kind, label, property — but
// not its user-defined name, which the store-direct path does not retain.
type StoreConstraint struct {
	// Label is the constrained node label.
	Label string
	// Property is the constrained property key.
	Property string
	// Kind is the constraint kind (0 = UNIQUE, 1 = NOT NULL), matching the
	// txn package's ConstraintKind ordinals.
	Kind uint8
}

// StoreConstraints returns a snapshot of the store-direct schema constraints
// recorded on this graph (seeded by recovery, or by the txn.Store apply path).
// It lets the cypher engine re-enforce durable UNIQUE / NOT NULL constraints
// when it opens over a recovered store even if the caller did not thread them
// explicitly (see cypher.NewEngineWithStore). The returned order is
// unspecified; the slice is a fresh copy the caller owns.
//
// StoreConstraints is safe for concurrent use.
func (g *Graph[N, W]) StoreConstraints() []StoreConstraint {
	g.storeConstraintMu.Lock()
	defer g.storeConstraintMu.Unlock()
	if len(g.storeConstraints) == 0 {
		return nil
	}
	out := make([]StoreConstraint, 0, len(g.storeConstraints))
	for k := range g.storeConstraints {
		out = append(out, StoreConstraint{Kind: k.kind, Label: k.label, Property: k.property})
	}
	return out
}

// ClearStoreConstraints empties the store-direct constraint set, returning the
// store-direct count to zero. The cypher engine calls it when it takes
// ownership of a recovered graph: from that point the engine's own count
// (SetActiveConstraintCount) is the authoritative source for HasConstraints, so
// the store-direct count — seeded by recovery for the engine-less case — must
// not linger and force a checkpoint to over-retain the WAL after the engine
// later drops a constraint.
//
// ClearStoreConstraints is safe for concurrent use.
func (g *Graph[N, W]) ClearStoreConstraints() {
	g.storeConstraintMu.Lock()
	if len(g.storeConstraints) > 0 {
		g.storeConstraints = nil
		g.storeConstraintActive.Store(0)
	}
	g.storeConstraintMu.Unlock()
}

// HasIndexes reports whether any secondary index has been declared through the
// txn.Store-direct API on this graph. It reads a lock-free counter maintained
// by the txn.Store apply path (AddStoreIndex / RemoveStoreIndex), so it is
// cheap enough for the checkpointer to consult on every checkpoint to gate the
// indexdefs.bin self-sufficiency requirement: a checkpoint that truncates the
// WAL prefix which first declared an index must carry the index definition in
// the snapshot, or the index is silently lost on the next reopen (#1755).
//
// It reports true when EITHER the engine-maintained count (SetActiveIndexCount)
// OR the store-direct count (AddStoreIndex, maintained by the txn.Store apply
// path) is positive, so the checkpoint fail-safe is correct whether the index
// was declared through the cypher engine or directly through txn.Tx.CreateIndex
// (#1755). Two sources are required because the engine's CREATE INDEX commits
// via Tx.CommitWALOnly, which never replays through the store apply path, so
// storeIndexActive alone would be blind to every engine-declared index.
//
// HasIndexes is safe for concurrent use.
func (g *Graph[N, W]) HasIndexes() bool {
	return g.indexActive.Load() > 0 || g.storeIndexActive.Load() > 0
}

// AddStoreIndex records that a secondary index named name is declared through
// the txn.Store-direct API. It is the store-layer dual of the cypher engine's
// index-def registry: the txn.Store commit-apply path calls it for every
// committed OpCreateIndex so that Graph.HasIndexes reports the index to a
// WAL-truncating checkpoint, independent of whether a cypher engine is wired in
// (#1755).
//
// The index NAME key makes re-declaring the same index idempotent — the active
// count never over-counts a single durable index, the only direction that could
// let a checkpoint silently drop it.
//
// AddStoreIndex is safe for concurrent use.
func (g *Graph[N, W]) AddStoreIndex(name string) {
	g.storeIndexMu.Lock()
	if g.storeIndexes == nil {
		g.storeIndexes = make(map[string]struct{}, 1)
	}
	if _, dup := g.storeIndexes[name]; !dup {
		g.storeIndexes[name] = struct{}{}
		g.storeIndexActive.Add(1)
	}
	g.storeIndexMu.Unlock()
}

// RemoveStoreIndex drops the store-direct index slot identified by name, the
// dual of [Graph.AddStoreIndex] for a committed OpDropIndex. Dropping an index
// that was never recorded is a no-op, so a DROP that suppresses a CREATE folded
// away by a prior checkpoint cannot drive the active count negative.
//
// RemoveStoreIndex is safe for concurrent use.
func (g *Graph[N, W]) RemoveStoreIndex(name string) {
	g.storeIndexMu.Lock()
	if _, ok := g.storeIndexes[name]; ok {
		delete(g.storeIndexes, name)
		g.storeIndexActive.Add(-1)
	}
	g.storeIndexMu.Unlock()
}

// ClearStoreIndexes empties the store-direct index set, returning the
// store-direct count to zero. The cypher engine calls it when it takes
// ownership of a recovered graph: from that point the engine's own index-def
// registry is the authoritative source it threads into the checkpoint, so the
// store-direct count — seeded by recovery for the engine-less case — must not
// linger and force a checkpoint to over-retain the WAL after the engine later
// drops an index.
//
// ClearStoreIndexes is safe for concurrent use.
func (g *Graph[N, W]) ClearStoreIndexes() {
	g.storeIndexMu.Lock()
	if len(g.storeIndexes) > 0 {
		g.storeIndexes = nil
		g.storeIndexActive.Store(0)
	}
	g.storeIndexMu.Unlock()
}

// SetActiveConstraintCount records the number of schema constraints currently
// registered, for HasConstraints to report. The cypher engine calls it under
// its single-writer lock after every constraint registration, drop, and
// recovery re-seed, so the value never under-counts a durably-registered
// constraint that a concurrent checkpoint might otherwise miss.
//
// SetActiveConstraintCount is safe for concurrent use.
func (g *Graph[N, W]) SetActiveConstraintCount(n int64) { g.constraintActive.Store(n) }

// SetActiveIndexCount records the number of secondary indexes currently
// registered in the cypher engine's index-def registry, for HasIndexes to
// report. The cypher engine calls it under its single-writer lock after every
// index registration, drop, and recovery re-seed (Engine.syncIndexCount), so
// the value never under-counts a durably-registered index that a concurrent
// checkpoint might otherwise miss — the index analogue of
// SetActiveConstraintCount (#1755).
//
// SetActiveIndexCount is safe for concurrent use.
func (g *Graph[N, W]) SetActiveIndexCount(n int64) { g.indexActive.Store(n) }

// RestoreTombstones marks every id in ids as removed, reconstructing the
// tombstone set captured by [Graph.TombstonedIDs] at snapshot time. It is
// the load-phase dual of [Graph.RemoveNode] used by snapshot recovery: it
// re-tombstones by NodeID directly and does not require the natural key to
// be resolvable. A later [Graph.AddNode] for the same id still revives it,
// so a delete→recreate that straddles a snapshot resolves correctly.
//
// RestoreTombstones is intended for the one-shot snapshot-load phase of
// recovery and is not safe to call concurrently with other mutations or
// reads on g.
func (g *Graph[N, W]) RestoreTombstones(ids []graph.NodeID) {
	if len(ids) == 0 {
		return
	}
	g.tombstoneMu.Lock()
	cur := g.tombstones.Load()
	var next *roaring64.Bitmap
	if cur == nil {
		next = roaring64.New()
	} else {
		next = cur.Clone()
	}
	var added int64
	for _, id := range ids {
		if next.CheckedAdd(uint64(id)) {
			added++
		}
	}
	if added > 0 {
		g.tombstones.Store(next)
		g.tombstoneActive.Add(added)
		// The THIRD tombstone transition, alongside RemoveNode and revive: it flips
		// LiveNodeFilter from nil to non-nil, which changes what
		// csr.BuildFromAdjListLive emits. Recovery calls this before publishing the
		// graph, so no cache can exist yet — but the method is exported, so it bumps
		// rather than relying on that precondition holding for every future caller.
		g.topoGeneration.Add(1)
	}
	g.tombstoneMu.Unlock()
}

// IsTombstoned reports whether id has been marked removed via
// [Graph.RemoveNode]. Used by the Cypher executor's AllNodesScan to
// skip phantom nodes (those that the Mapper still indexes but that
// the graph treats as deleted).
func (g *Graph[N, W]) IsTombstoned(id graph.NodeID) bool {
	// Lock-free fast path: on a graph that has never tombstoned a node the
	// answer is always false, so skip even the pointer load (mirroring the
	// same gate in AddNode). This matters under concurrent reads —
	// AllNodesScan calls IsTombstoned per node, and the previous per-call
	// tombstoneMu.RLock bounced the RWMutex reader-count cache line across
	// cores, capping read scaling (rmp #2039). tombstoneActive is mutated
	// only under tombstoneMu, so a 0 observed here means no tombstone is
	// committed.
	if g.tombstoneActive.Load() == 0 {
		return false
	}
	// Once any tombstone exists, load the immutable published bitmap once and
	// do a read-only membership test — no lock. The bitmap is never mutated
	// after publication (writers clone-mutate-store), so this is race-free
	// against a concurrent RemoveNode/revive; the reader sees either the pre-
	// or the post-mutation set. The nil check is defensive: tombstoneActive
	// and the pointer move together under tombstoneMu, so a non-zero count
	// implies a published bitmap.
	bm := g.tombstones.Load()
	if bm == nil {
		return false
	}
	return bm.Contains(uint64(id))
}

// LiveNodeFilter returns a predicate reporting whether a NodeID is live (not
// tombstoned), or nil when the graph carries no tombstones at all. It is the
// liveness argument for [csr.BuildFromAdjListLive]: passing it builds a search
// CSR that omits the ghost edges left behind by [Graph.RemoveNode] (which
// tombstones a node without stripping its incident edges), while the nil return
// on a tombstone-free graph preserves the zero-overhead build fast path (#1790).
//
// The returned predicate is a point-in-time view: it closes over the graph and
// re-reads tombstone state on each call, so it must be used against a quiescent
// graph (the same single state the CSR build snapshots).
func (g *Graph[N, W]) LiveNodeFilter() func(graph.NodeID) bool {
	if g.tombstoneActive.Load() == 0 {
		return nil
	}
	return func(id graph.NodeID) bool { return !g.IsTombstoned(id) }
}

// LiveOrder returns the number of non-tombstoned interned nodes.
//
// LiveOrder is safe for concurrent use and takes no lock: tombstoneActive
// mirrors the published bitmap's cardinality exactly (both move together
// under tombstoneMu), so the dead count is a single atomic load.
func (g *Graph[N, W]) LiveOrder() uint64 {
	total := g.adj.Order()
	dead := uint64(g.tombstoneActive.Load())
	if dead > total {
		return 0
	}
	return total - dead
}

// TopoGeneration returns the current value of the graph's edge-topology
// generation counter (rmp #1871): a purely monotonic count of every change to
// the graph's LIVE edge topology since it was created. Two reads returning the
// same value guarantee that topology did not change in between, which is exactly
// the invalidation signal a CSR-position-keyed cache needs — the Cypher engine's
// edge-type-filter cache and its forward/reverse CSR pair cache both key on it.
//
// It counts edge additions and removals, undos of either, and — since rmp #2143 —
// the three TOMBSTONE transitions (RemoveNode, reviving a removed node via
// AddNode, and RestoreTombstones). Tombstoning touches no edge, but
// csr.BuildFromAdjListLive omits the arcs incident to a tombstoned node, so the
// live topology a cache is derived from has changed.
//
// Since rmp #2255 it ALSO counts every change to an edge's derived LABEL set —
// [Graph.SetEdgeLabel], [Graph.RemoveEdgeLabel] and
// [Graph.SetEdgeLabelByHandle]. An edge label moves no CSR position, so this is
// wider than the counter's name suggests, and the reason is concrete: the Cypher
// engine's edge-type-filter cache keys on this epoch while resolving relationship
// TYPE from those labels, so a label change that left the epoch still was served
// a stale filter. The observable defect was a durably committed relationship-type
// change staying invisible to a warm Engine indefinitely, and — via the rollback
// inverse — an aborted one staying visible.
//
// A mutation that changes nothing does NOT bump: re-asserting a label already
// present, removing one that is absent, or targeting an edge that does not exist
// all leave the epoch alone. That distinction is load-bearing rather than tidy,
// because the MERGE MATCH branch re-asserts an existing relationship's type on
// every match, and bumping there would force an O(V+E) CSR-pair rebuild per
// MERGE on a read-mostly workload.
//
// It says nothing about interning a fresh node or about property-only mutations,
// neither of which shifts an existing edge's CSR position or changes a
// relationship type; see the topoGeneration field doc for why that scope is
// sufficient and intentional.
//
// Callers do NOT need to bump it themselves: every [Graph] mutator that changes
// live topology bumps it internally, after publishing the change. Safe for
// concurrent use.
func (g *Graph[N, W]) TopoGeneration() uint64 { return g.topoGeneration.Load() }

// BumpTopoGeneration advances the edge-topology generation counter by one.
// Deliberately separate from [Graph.IncrEdgesAdded] / [Graph.IncrEdgesRemoved]
// (which bump topoGeneration too, alongside the unrelated TCK side-effect
// counters): a caller that mutates edge topology WITHOUT an enclosing Cypher
// statement — a direct [store/txn.Store]/[store/txn.Tx] user, bypassing the
// engine's write adapters entirely — has no Cypher-statement side-effect
// count to attribute an Incr/Decr to, but the graph's edge topology still
// changed, so any CSR-position-keyed cache still needs invalidating. Calling
// this alone leaves edgesAddedCount/edgesRemovedCount untouched, which is
// correct: those counters answer "how many edges did this Cypher statement
// add/remove," a question a store-direct write was never part of. Safe for
// concurrent use.
func (g *Graph[N, W]) BumpTopoGeneration() { g.topoGeneration.Add(1) }

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
// IncrNodesAdded records that one node was freshly added.
func (g *Graph[N, W]) IncrNodesAdded() { g.nodesAddedCount.Add(1) }

// IncrNodesRemoved records that one node was removed.
func (g *Graph[N, W]) IncrNodesRemoved() { g.nodesRemovedCount.Add(1) }

// IncrEdgesAdded records that one edge was freshly added.
func (g *Graph[N, W]) IncrEdgesAdded() {
	g.edgesAddedCount.Add(1)
	g.topoGeneration.Add(1)
}

// IncrEdgesRemoved records that one edge was removed.
func (g *Graph[N, W]) IncrEdgesRemoved() {
	g.edgesRemovedCount.Add(1)
	g.topoGeneration.Add(1)
}

// DecrNodesAdded / DecrNodesRemoved / DecrEdgesAdded / DecrEdgesRemoved are the
// exact inverses of the Incr* counters above. They exist for one purpose: the
// Cypher executor's transaction-undo path replays the inverse of every eagerly
// applied mutation when a write query errors or panics, and the per-query side-
// effect deltas the openCypher TCK asserts ([Graph.SideEffectCounters]) must
// not retain the increments of a rolled-back statement. Each subtracts one from
// the matching monotone counter.
//
// These must only be called to invert a prior Incr* on the same graph; they do
// not floor at zero, so a stray over-decrement would underflow the unsigned
// counter. The undo log guarantees one Decr per recorded Incr.
//
// Decr* are safe for concurrent use.
func (g *Graph[N, W]) DecrNodesAdded() { g.nodesAddedCount.Add(^uint64(0)) }

// DecrNodesRemoved subtracts one from the removed-node counter.
func (g *Graph[N, W]) DecrNodesRemoved() { g.nodesRemovedCount.Add(^uint64(0)) }

// DecrEdgesAdded subtracts one from the added-edge counter. topoGeneration is
// NOT decremented — it only ever increases, on the Incr side too, because an
// undo is itself a topology-changing event for any CSR-position-keyed cache:
// the graph's content afterward differs from the content the moment before
// the undo ran, even though it matches the content from further back.
func (g *Graph[N, W]) DecrEdgesAdded() {
	g.edgesAddedCount.Add(^uint64(0))
	g.topoGeneration.Add(1)
}

// DecrEdgesRemoved subtracts one from the removed-edge counter. See
// [Graph.DecrEdgesAdded] for why topoGeneration still only ever increases.
func (g *Graph[N, W]) DecrEdgesRemoved() {
	g.edgesRemovedCount.Add(^uint64(0))
	g.topoGeneration.Add(1)
}

// RemoveNodeLabel detaches name from n. No-op if absent.
func (g *Graph[N, W]) RemoveNodeLabel(n N, name string) {
	g.removeNodeLabelInfo(n, name, nil)
	g.reclaimAfterDirectWrite()
}

// removeNodeLabelInfo is [Graph.RemoveNodeLabel] with an explicit commit
// record; see [Graph.setNodeLabelInfo].
func (g *Graph[N, W]) removeNodeLabelInfo(n N, name string, tx *writeCtx) {
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
		// P0 MVCC spike (rmp #2275), inert unless armed. Guarded on the label
		// actually being present for the same reason as the add path: removing
		// a label the node does not carry changes nothing.
		if g.labelDeltasEnabled() && bag.has(lid) {
			// See setNodeLabelInfo. removeNodeLabelInfo cannot return an error,
			// so the conflict is RECORDED on the transaction and the write is
			// skipped: applying a doomed write would put a version on a chain
			// whose head belongs to someone else. The caller learns of it from
			// the error its next writing call returns, or from commit, which
			// refuses to publish a transaction carrying one. Recording is what
			// makes the two paths equivalent — without it this removal would be
			// dropped and the transaction would commit as if it had happened.
			if head := sh.headStamp(id); tx.conflicts(head) {
				_ = tx.conflictErr("node labels", head)
				sh.mu.Unlock()
				return
			}
			ci, ts := g.deltaStamp(tx.record())
			sh.pushLabelDelta(id, undoAddLabel, lid, ci, ts, &g.labelDeltaActive)
		}
		if bag.del(lid) {
			// Bag became empty: drop the entry so a node with no labels costs
			// no map slot (matches the prior map behaviour).
			delete(sh.m, id)
		} else {
			sh.m[id] = bag
		}
	}
	sh.mu.Unlock()
	// Deferred; see stripLabelBitmaps and mvcc_index.go.
	if !g.deferLabelIndexRemoval(uint32(lid), id) {
		g.nodeIdx.Remove(uint32(lid), id)
	}
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
	bag := sh.m[id]
	return bag.has(lid)
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
	out := make([]string, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	})
	sh.mu.RUnlock()
	return out
}

// NodeLabelsByID is the NodeID-keyed counterpart of [Graph.NodeLabels]. It
// skips the external-key → NodeID Mapper lookup for callers that already hold
// the NodeID (the Cypher result-materialisation path), returning the label
// names in unspecified order, or nil when id carries no labels.
func (g *Graph[N, W]) NodeLabelsByID(id graph.NodeID) []string {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag, ok := sh.m[id]
	if !ok {
		sh.mu.RUnlock()
		return nil
	}
	out := make([]string, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	})
	sh.mu.RUnlock()
	return out
}

// ForEachNodeLabelByID streams the labels of the node identified by id, invoking
// visit once per resolved label name without materialising the []string that
// [Graph.NodeLabelsByID] returns. It is the allocation-fusing counterpart of
// NodeLabelsByID — the label analogue of [Graph.NodePropertiesByIDFunc] —
// chiefly for the snapshot writer, which re-keys every label into its own
// string table and would otherwise allocate a throwaway slice per node.
//
// Concurrency: visit runs while the node-label shard's read lock is held, so it
// observes a consistent snapshot of the node's labels relative to any concurrent
// writer holding the shard write lock — identical to [Graph.NodeLabelsByID].
// visit therefore MUST NOT call back into any Graph method that takes a
// node-label-shard lock (it would deadlock); copying the name string out is safe.
func (g *Graph[N, W]) ForEachNodeLabelByID(id graph.NodeID, visit func(name string)) {
	g.ForEachNodeLabelByIDAsOf(id, nil, visit)
}

// ForEachNodeLabelByIDAsOf is [Graph.ForEachNodeLabelByID] as the node stood at
// snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// The bag is resolved BEFORE visit runs and no shard lock is held across it,
// which the plain form could not offer: a reconstructed version is a private
// copy, so there is nothing left to protect.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ForEachNodeLabelByIDAsOf(id graph.NodeID, snap *Snapshot, visit func(name string)) {
	g.withLabelBag(id, snap, func(bag labelBag) {
		bag.forEach(func(lid LabelID) {
			if name, ok := g.reg.Resolve(lid); ok {
				visit(name)
			}
		})
	})
}

// HasNodeLabelByID is the NodeID-keyed, allocation-free counterpart of
// [Graph.HasNodeLabel]: it reports whether the node identified by id carries
// the named label without the external-key → NodeID Mapper lookup and without
// materialising the node's label slice (which [NodeLabelsByID] would).
//
// It backs the lazy `n:Label` predicate fast path in the Cypher engine, which
// holds the NodeID already and only needs a membership test. An unknown label
// name (never interned) is a definite "absent" answer, mirroring
// [Graph.HasNodeLabel].
func (g *Graph[N, W]) HasNodeLabelByID(id graph.NodeID, name string) bool {
	lid, ok := g.reg.Lookup(name)
	if !ok {
		return false
	}
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	present := bag.has(lid)
	sh.mu.RUnlock()
	return present
}

// SetEdgeLabel attaches label to the directed edge (src, dst). The
// edge must already exist in the underlying adjacency list; otherwise
// the call is a no-op. The label is associated with the source
// NodeID's row in the edge index.
//
// The first relationship type of a pair is stored inline in the adjacency
// slot's label column; a second distinct type spills to the per-shard
// overflow store. The two together form the pair's derived label set
// returned by [Graph.EdgeLabels]. The whole update runs under the pair's
// edge-label shard write lock so the slot and overflow halves transition
// together with respect to a concurrent reader.
func (g *Graph[N, W]) SetEdgeLabel(src, dst N, name string) {
	g.setEdgeLabelInfo(src, dst, name, nil)
}

// setEdgeLabelInfo is [Graph.SetEdgeLabel] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made and
// takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) setEdgeLabelInfo(src, dst N, name string, tx *writeCtx) {
	if !g.adj.HasEdge(src, dst) {
		return
	}
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	lid := g.reg.Intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	changed := g.setEdgeLabelLocked(k, lid, tx)
	sh.mu.Unlock()
	g.edgeIdx.Add(uint32(lid), srcID)
	if changed {
		// The derived edge-label set is part of what [Graph.TopoGeneration]
		// covers, because the Cypher engine's edge-type-filter cache is keyed on
		// it (rmp #2255). Bumping AFTER the shard is released and after the index
		// add is the ordering rmp #2151/fafc50c7 established: a reader that
		// samples the new epoch must be unable to miss the write it announces.
		g.topoGeneration.Add(1)
	}
}

// setEdgeLabelLocked adds lid to the label set of every column-typed slot of k.
// The caller must hold k's edge-label shard write lock AND must already have
// verified that the edge (k.src, k.dst) exists (the slot to receive an inline
// label is present).
//
// # Placement is per-SLOT
//
// A relationship type belongs to the relationship INSTANCE, not to the endpoint
// pair, so on a multigraph each of a pair's parallel slots carries its own type.
// [Graph.SetEdgeLabel] names the PAIR, and therefore names every one of the
// pair's slots whose type is read from the adjacency label column — the slots
// [Graph.slotCarriesType] resolves from that column because no per-edge handle
// records their type. Those are the pair's COLUMN-TYPED slots.
//
// The label goes into EVERY column-typed slot that is still free. That is what
// makes two parallel handle-less edges both report the type, where placing it in
// the first free slot alone left the second at the 0 sentinel and a typed degree
// counted 1 where 2 was correct (rmp #2258).
//
// When no column-typed slot is free the type has nowhere per-slot to live and
// spills to the pair's overflow list, which [Graph.slotCarriesType] reads as
// carried by every column-typed slot of the pair — the only reading consistent
// with SetEdgeLabel naming the pair. So `AddEdge` ×2 + SetEdgeLabel(K) +
// SetEdgeLabel(M) gives two relationships that each carry both types, whereas
// interleaving the calls (`AddEdge`, SetEdgeLabel(K), `AddEdge`,
// SetEdgeLabel(M)) gives one K relationship and one M relationship: the two
// construction sequences are distinguishable, which under the former per-pair
// storage they were not.
//
// A slot whose type IS recorded against a handle is deliberately skipped: that
// record is authoritative for it (see [Graph.slotCarriesType]), so writing the
// column there could not change what the slot reports and would only make the
// column disagree with the handle store. This is what keeps a Cypher-created
// typed edge and a Go-API edge on the same pair counting as two distinct
// relationships with their own types.
//
// It reports whether the label set actually changed, which is what
// [Graph.SetEdgeLabel] uses to decide whether to bump the topology generation
// (rmp #2255). Re-asserting a type the pair already carries reports false — the
// MERGE match branch re-asserts on every idempotent MERGE, and a spurious bump
// would force an O(V+E) CSR cache rebuild for a mutation that changed nothing.
func (g *Graph[N, W]) setEdgeLabelLocked(k edgeKey, lid LabelID, tx *writeCtx) bool {
	enc := encodeSlotLabel(lid)
	free, present := g.columnTypedSlots(k.src, k.dst, lid, enc)
	if len(free) > 0 {
		// At least one column-typed slot is free: place the type on all of them.
		g.adj.SetEdgeLabelSlotsAt(k.src, k.dst, free, enc)
		return true
	}
	if present {
		// Already carried, by a column-typed slot or by a handle record.
		return false
	}
	// No free column-typed slot; spill to overflow, deduplicated.
	sh := g.edgeLabelShardFor(k)
	if !g.addOverflowVersioned(sh, k, lid, tx) {
		return false
	}
	g.edgeLabelOverflowActive.Add(1)
	return true
}

// columnTypedSlots partitions the dst-matching adjacency slots of srcID by
// whether they can receive lid in the label column. free lists the indexes of the
// COLUMN-TYPED slots (those with no handle-keyed label record, so
// [Graph.slotCarriesType] resolves their type from the column) that carry no
// label yet; present reports whether lid is ALREADY carried, by a column-typed
// slot's own entry or by a handle record on one of the pair's slots.
//
// The free indexes are collected into a caller-supplied-free slice rather than
// applied one at a time so [Graph.setEdgeLabelLocked] can publish one
// copy-on-write column for the whole pair ([adjlist.AdjList.SetEdgeLabelSlotsAt])
// instead of one per slot, which would be O(d²) on a hub.
//
// It reads the lock-free adjacency snapshot and the handle-label shards; it does
// NOT hold the adjacency shard lock, so an index it returns may be stale by the
// time the write lands. That is why SetEdgeLabelSlotsAt re-checks the neighbour
// under the lock. Taking the adjacency lock here instead would nest it inside
// both the edge-label and the handle-label shard locks and invert the module's
// lock order.
func (g *Graph[N, W]) columnTypedSlots(srcID, dstID graph.NodeID, lid LabelID, enc uint32) (free []int, present bool) {
	nbs, _, handles := g.adj.LoadEntryH(srcID)
	labs := g.adj.LoadEntryLabels(srcID)
	for i, nb := range nbs {
		if nb != dstID {
			continue
		}
		var handle uint64
		if i < len(handles) {
			handle = handles[i]
		}
		if has, known := g.edgeHandleHasLabel(srcID, dstID, handle, lid); known {
			// The handle store is authoritative for this slot; the column is not
			// consulted for it, so it is not ours to write.
			if has {
				present = true
			}
			continue
		}
		var cur uint32
		if i < len(labs) {
			cur = labs[i]
		}
		switch cur {
		case 0:
			free = append(free, i)
		case enc:
			present = true
		}
	}
	return free, present
}

// HasEdgeLabel reports whether the directed edge (src, dst) carries
// name as a label.
func (g *Graph[N, W]) HasEdgeLabel(src, dst N, name string) bool {
	return g.HasEdgeLabelAsOf(src, dst, name, nil)
}

// HasEdgeLabelAsOf is [Graph.HasEdgeLabel] as the edge stood at snap. A nil
// snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasEdgeLabelAsOf(src, dst N, name string, snap *Snapshot) bool {
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
	for _, x := range g.overflowLabelsAsOf(sh, k, snap) {
		if x == lid {
			return true
		}
	}
	found := false
	g.slotLabelsForPair(srcID, dstID, snap, func(slotLid LabelID) {
		if slotLid == lid {
			found = true
		}
	})
	return found
}

// RemoveEdgeLabel detaches name from the directed edge (src, dst). It is the
// exported inverse of [Graph.SetEdgeLabel] used by the Cypher executor's
// transaction-undo path to strip a label a failed write query had attached.
// No-op when either endpoint is unknown, name was never interned, or the label
// is not present on the pair. Unlike [Graph.SetEdgeLabel] it does not require
// the edge to still exist in the adjacency, so it can also undo a label that
// was set on an edge later removed within the same failed statement.
//
// Like [Graph.clearEdgePairState], the coarse src-keyed edge label index
// (g.edgeIdx) is intentionally left untouched: it is read only as an
// over-approximation the executor verifies against the authoritative per-pair
// labels, so a stale entry can cost at most a filtered-out candidate, never a
// wrong result.
//
// RemoveEdgeLabel is safe for concurrent use.
func (g *Graph[N, W]) RemoveEdgeLabel(src, dst N, name string) {
	g.removeEdgeLabelInfo(src, dst, name, nil)
}

// removeEdgeLabelInfo is [Graph.RemoveEdgeLabel] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) removeEdgeLabelInfo(src, dst N, name string, tx *writeCtx) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	lid, ok := g.reg.Lookup(name)
	if !ok {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	// Detach the type from BOTH halves: the pair's overflow copy and EVERY
	// dst-matching slot whose column entry decodes to lid. Clearing only the first
	// such slot would leave a parallel sibling still reporting the type, which is
	// the same per-pair-versus-per-slot mismatch rmp #2258 fixed on the read side —
	// and this is the exact inverse of [Graph.SetEdgeLabel], which types every free
	// column-typed slot of the pair. The whole update is under the shard lock so
	// the slot and overflow halves transition together for readers.
	changed := g.removeOverflowVersioned(sh, k, lid, tx)
	if changed {
		g.edgeLabelOverflowActive.Add(-1)
	}
	if g.clearSlotLabelsValue(srcID, dstID, lid) {
		changed = true
	}
	sh.mu.Unlock()
	if changed {
		// See [Graph.SetEdgeLabel]: the derived edge-label set is inside
		// [Graph.TopoGeneration]'s scope (rmp #2255). Removal matters as much as
		// addition — this is the rollback inverse of a label SET, so without the
		// bump an ABORTED transaction's relationship type stayed visible to a
		// warm Engine, which is an Atomicity violation and not merely a stale read.
		g.topoGeneration.Add(1)
	}
}

// clearSlotLabelsValue clears the label of EVERY dst-matching adjacency slot of
// src whose column entry decodes to lid — not merely the first, because a
// multigraph pair's parallel slots each carry their own type and
// [Graph.SetEdgeLabel] types all of the free ones, so detaching the type must
// clear all of them. Slots carrying a DIFFERENT type are untouched.
//
// The caller must hold the pair's edge-label shard write lock. No-op when no slot
// carries lid (e.g. the edge was already removed — the orphan case, where the
// label lived only in overflow and was handled by the caller).
//
// It reports whether any slot was actually cleared, which [Graph.RemoveEdgeLabel]
// uses to bump the topology generation only on a genuine change (rmp #2255).
func (g *Graph[N, W]) clearSlotLabelsValue(srcID, dstID graph.NodeID, lid LabelID) bool {
	return g.adj.ClearEdgeLabelSlotsValue(srcID, dstID, encodeSlotLabel(lid)) > 0
}

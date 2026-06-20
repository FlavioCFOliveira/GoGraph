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
	"sort"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
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

// LabelRegistry interns label names and assigns sequential LabelIDs.
// It is safe for concurrent use.
//
// The read path ([LabelRegistry.Resolve]) is lock-free: it loads an
// immutable id→name snapshot through an [atomic.Pointer] and indexes
// into it without taking any lock. The write path ([LabelRegistry.Intern]
// of a previously unseen name — a rare event) serialises under a mutex,
// builds a new immutable snapshot extended by one entry, and atomically
// publishes it. Because LabelIDs are only ever appended (never reused or
// renamed) and Intern publishes the snapshot carrying names[id] before
// returning id — i.e. before id can be stored into any node/edge bag —
// every reader that observes an id in a bag observes, by release/acquire
// ordering through that bag's own publication, a snapshot at least as new
// as the one Intern published. Resolve therefore never misses a live id.
type LabelRegistry struct {
	// mu serialises Intern (write path) and guards forward. It is never
	// taken on the read path.
	mu      sync.Mutex
	forward map[string]LabelID
	// snap holds the immutable id→name table. Loaded lock-free by
	// Resolve; swapped under mu by Intern.
	snap atomic.Pointer[labelNames]
}

// NewLabelRegistry returns an empty registry.
func NewLabelRegistry() *LabelRegistry {
	r := &LabelRegistry{forward: make(map[string]LabelID)}
	r.snap.Store(&labelNames{})
	return r
}

// Intern returns a stable LabelID for name, allocating one on first
// encounter. It runs on the write path only (label assignment), so it
// serialises under the write mutex; the steady-state label vocabulary is
// small and stable, so the mutex is contended only while the vocabulary
// is first built up.
func (r *LabelRegistry) Intern(name string) LabelID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.forward[name]; ok {
		return id
	}
	cur := r.snap.Load()
	id := LabelID(len(cur.names))
	next := &labelNames{names: make([]string, len(cur.names)+1)}
	copy(next.names, cur.names)
	next.names[id] = name
	r.snap.Store(next)
	r.forward[name] = id
	return id
}

// Lookup returns the LabelID for name and true, or 0 and false when
// name has not been interned.
func (r *LabelRegistry) Lookup(name string) (LabelID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.forward[name]
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
// per-vertex and per-edge property maps. Sized to keep contention
// below 5% on workloads with up to a few hundred concurrent
// readers/writers; not as wide as adjlist's 256 because property
// access is less hot than adjacency.
const propMapShards = 16

// nodePropShard is one stripe of the per-vertex property map. The inner
// per-node bag is a compact tiered [propBag] (sprint 207, #1587) stored by
// value, not a nested Go map: a node carrying a handful of properties pays a
// single small slice backing instead of ~300 B of map overhead. The bag is
// guarded by mu exactly as the nested map was.
type nodePropShard struct {
	mu sync.RWMutex
	m  map[graph.NodeID]propBag
}

// nodeLabelShard is one stripe of the node-label bag. The mutex
// serialises mutations on this shard only; readers hold an RLock
// for HasNodeLabel / NodeLabels. Splitting the bag into 16 shards
// removes the global nodeMu contention point that previously
// serialised every Set/Remove/Has across all NodeIDs in the graph.
type nodeLabelShard struct {
	mu sync.RWMutex
	m  map[graph.NodeID]labelBag
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
	mu sync.RWMutex
	// overflow holds the extra (beyond the slot) labels of a pair, deduplicated.
	// nil until the first label spills here.
	overflow map[edgeKey][]LabelID
}

// addOverflow appends lid to k's overflow list if not already present. The
// caller must hold sh.mu for writing.
func (sh *edgeLabelShard) addOverflow(k edgeKey, lid LabelID) {
	ls := sh.overflow[k]
	for _, x := range ls {
		if x == lid {
			return
		}
	}
	if sh.overflow == nil {
		sh.overflow = make(map[edgeKey][]LabelID)
	}
	sh.overflow[k] = append(ls, lid)
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

// clearOverflow drops every overflow label on k. The caller must hold sh.mu
// for writing.
func (sh *edgeLabelShard) clearOverflow(k edgeKey) {
	delete(sh.overflow, k)
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
	tombstoneMu sync.RWMutex
	tombstones  map[graph.NodeID]struct{}
	// tombstoneActive mirrors len(tombstones) as a lock-free gate. AddNode
	// is a hot path; on the overwhelmingly common case of a graph that has
	// never deleted a node this lets AddNode skip the tombstone lock and the
	// mapper lookup entirely. It is mutated only under tombstoneMu.
	tombstoneActive atomic.Int64

	// constraintActive mirrors the cypher engine's schema-constraint count as a
	// lock-free gate. The checkpointer reads it (via HasConstraints) to decide
	// whether a snapshot must carry constraints.bin before the WAL prefix that
	// first declared the constraints can be truncated; without it an embedder
	// that forgets checkpoint.WithConstraintSpecs would silently lose every
	// schema constraint on the next reopen (#1464). It is maintained by
	// Engine.syncConstraintCount under the engine's single-writer lock.
	constraintActive atomic.Int64

	// nodesAddedCount / nodesRemovedCount / edgesAddedCount /
	// edgesRemovedCount track per-direction counters used by the TCK
	// side-effect comparator. Net Order() / Size() can't distinguish a
	// CREATE+DELETE from a no-op, so the comparator needs the explicit
	// addition and removal counts.
	nodesAddedCount   atomic.Uint64
	nodesRemovedCount atomic.Uint64
	edgesAddedCount   atomic.Uint64
	edgesRemovedCount atomic.Uint64

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

	// barrier enforces that no single goroutine re-enters visMu via
	// [Graph.View] / [Graph.ApplyAtomically]. visMu is not re-entrant, so a
	// nested acquisition from a goroutine already inside the barrier would
	// deadlock the engine; the guard converts that silent hang into an immediate
	// panic. See reentrancy.go for the mechanism and cost.
	barrier barrierGuard
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
// re-entrant, so a nested acquisition from this goroutine would deadlock). That
// invariant is enforced: a nested call from a goroutine already inside the
// barrier panics with a clear message instead of deadlocking. The panic
// indicates a programmer error and is not recovered by this package. The graph's
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
	defer g.visMu.Unlock()
	defer g.barrier.clearWriter(gid)
	return fn()
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
// LockBarrier panics (re-entrancy guard).
//
// LockBarrier must not be called from a goroutine already inside the barrier
// (ApplyAtomically or a previous LockBarrier); it panics instead of deadlocking.
func (g *Graph[N, W]) LockBarrier() {
	gid := g.barrier.checkWriter() // panics on re-entry from this goroutine
	g.visMu.Lock()
	g.barrier.stampWriter(gid)
}

// UnlockBarrier releases the transaction-visibility write lock acquired via
// [Graph.LockBarrier]. It MUST be called from the same goroutine that called
// LockBarrier, and exactly once per LockBarrier call. After this call completes,
// concurrent [Graph.View] readers may proceed and [Graph.ApplyAtomically] may be
// called again from any goroutine.
func (g *Graph[N, W]) UnlockBarrier() {
	gid := goID()
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

// View runs fn while holding the graph's transaction-visibility read lock,
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
// nested [Graph.ApplyAtomically] always deadlocks; both are enforced — a nested
// call from a goroutine already inside the barrier panics with a clear message
// instead of deadlocking. The panic indicates a programmer error and is not
// recovered by this package.
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
func (g *Graph[N, W]) slotLabelsForPair(srcID, dstID graph.NodeID, fn func(LabelID)) {
	labs := g.adj.LoadEntryLabels(srcID)
	if labs == nil {
		return
	}
	nbs, _ := g.adj.LoadEntry(srcID)
	// labs is published in lockstep with neighbours, but a concurrent writer
	// may publish a longer neighbours snapshot after we loaded labs; bound the
	// scan by the shorter length so an index is never out of range.
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

// clearSlotLabels drops the relationship-type label from every dst-matching
// adjacency slot of src. It is the slot half of [Graph.clearEdgePairState];
// the caller must hold the pair's edge-label shard write lock so the slot and
// overflow halves transition together.
func (g *Graph[N, W]) clearSlotLabels(srcID, dstID graph.NodeID) {
	g.adj.ClearEdgeLabelSlots(srcID, dstID)
}

// firstSlotLabel returns the encoded label currently on the FIRST dst-matching
// adjacency slot of src and whether such a slot exists. encoded == 0 means the
// slot exists but carries no label. Reads the lock-free adjacency snapshot.
func (g *Graph[N, W]) firstSlotLabel(srcID, dstID graph.NodeID) (encoded uint32, slotExists bool) {
	nbs, _ := g.adj.LoadEntry(srcID)
	labs := g.adj.LoadEntryLabels(srcID)
	for i, nb := range nbs {
		if nb != dstID {
			continue
		}
		if labs != nil && i < len(labs) {
			return labs[i], true
		}
		return 0, true
	}
	return 0, false
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
	g.barrier.init()
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
	if err := g.adj.AddNode(n); err != nil {
		return err
	}
	// Fast path: no node has ever been removed, so there is nothing to
	// revive. This keeps the common AddNode free of the tombstone lock and
	// the mapper lookup below.
	if g.tombstoneActive.Load() == 0 {
		return nil
	}
	if id, ok := g.adj.Mapper().Lookup(n); ok {
		g.revive(id)
	}
	return nil
}

// revive clears any tombstone on id, marking the node live again. It is
// the inverse of [Graph.RemoveNode] and is invoked by [Graph.AddNode] when
// a removed node is re-created. The clear is taken under tombstoneMu so it
// is atomic against IsTombstoned / LiveOrder / TombstonedIDs readers.
func (g *Graph[N, W]) revive(id graph.NodeID) {
	g.tombstoneMu.Lock()
	if g.tombstones != nil {
		if _, ok := g.tombstones[id]; ok {
			delete(g.tombstones, id)
			g.tombstoneActive.Add(-1)
		}
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
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	g.revive(id)
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
func (g *Graph[N, W]) AddEdge(src, dst N, w W) error { return g.adj.AddEdge(src, dst, w) }

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
	srcID, srcOK := g.adj.Mapper().Lookup(src)
	dstID, dstOK := g.adj.Mapper().Lookup(dst)

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

	if g.adj.HasEdge(src, dst) {
		// Parallel edge(s) remain: keep the shared per-pair surfaces. Re-assert
		// any captured labels and properties in case the removed slot was the one
		// holding them.
		if srcOK && dstOK {
			g.reassertPairLabels(srcID, dstID, fwdLabels)
			g.reassertPairProps(src, dst, fwdProps)
			if !g.adj.Directed() {
				g.reassertPairLabels(dstID, srcID, revLabels)
				g.reassertPairProps(dst, src, revProps)
			}
		}
		return
	}
	if !srcOK || !dstOK {
		return
	}
	g.clearEdgePairState(edgeKey{src: srcID, dst: dstID})
	if !g.adj.Directed() {
		// The undirected edge is fully gone; clear the mirror direction's
		// per-pair surfaces too (a label may have been set under either
		// endpoint order).
		g.clearEdgePairState(edgeKey{src: dstID, dst: srcID})
	}
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
	g.slotLabelsForPair(srcID, dstID, func(lid LabelID) {
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
func (g *Graph[N, W]) reassertPairLabels(srcID, dstID graph.NodeID, ids []LabelID) {
	if len(ids) == 0 {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	for _, lid := range ids {
		g.setEdgeLabelLocked(k, lid)
	}
	sh.mu.Unlock()
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

	// Clear per-pair state for every affected endpoint pair.
	for _, dstID := range dstIDs {
		g.clearEdgePairState(edgeKey{src: srcID, dst: dstID})
		if !g.adj.Directed() {
			g.clearEdgePairState(edgeKey{src: dstID, dst: srcID})
		}
	}
}

// clearEdgePairState drops the per-pair edge-label and edge-property bags for
// k. The coarse, src-keyed edge label index (g.edgeIdx) is intentionally left
// untouched: it is read only as an over-approximation that the executor
// verifies against the authoritative per-pair labels, so a stale entry can
// cost at most a filtered-out candidate, never a wrong result.
func (g *Graph[N, W]) clearEdgePairState(k edgeKey) {
	// Clear both halves of the per-pair label set together under the pair's
	// shard write lock: the overflow entry AND the label on every dst-matching
	// adjacency slot. They are two halves of one logical set and must transition
	// atomically with respect to a concurrent EdgeLabels reader (which takes the
	// same shard RLock), so a re-CREATE of the same endpoints later cannot
	// resurrect a removed edge's relationship type.
	lsh := g.edgeLabelShardFor(k)
	lsh.mu.Lock()
	lsh.clearOverflow(k)
	g.clearSlotLabels(k.src, k.dst)
	lsh.mu.Unlock()
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
	delete(hlsh.m, k)
	hlsh.mu.Unlock()
	hpsh := g.edgeHandlePropShardFor(k)
	hpsh.mu.Lock()
	delete(hpsh.m, k)
	hpsh.mu.Unlock()
	// Drop the per-CREATE-instance label, property, and multiplicity-counter
	// stores. Without these, re-creating an edge between the same endpoints
	// after RemoveEdge would resurrect the removed edge's per-instance labels
	// and properties, and the CREATE counter would resume from its old value
	// rather than starting fresh at 1.
	ilsh := g.edgeInstanceLabelShardFor(k)
	ilsh.mu.Lock()
	delete(ilsh.m, k)
	ilsh.mu.Unlock()
	ipsh := g.edgeInstancePropShardFor(k)
	ipsh.mu.Lock()
	delete(ipsh.m, k)
	ipsh.mu.Unlock()
	ccsh := g.edgeCreateCountShardFor(k)
	ccsh.mu.Lock()
	delete(ccsh.m, k)
	ccsh.mu.Unlock()
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
func (g *Graph[N, W]) EdgeWeight(src, dst N) (W, bool) {
	var zero W
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return zero, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return zero, false
	}
	nbs, ws := g.adj.LoadEntry(srcID)
	for i, nb := range nbs {
		if nb == dstID {
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
	bag.add(lid)
	sh.m[id] = bag
	sh.mu.Unlock()
	g.nodeIdx.Add(uint32(lid), id)
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
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	g.tombstoneMu.Lock()
	if g.tombstones == nil {
		g.tombstones = make(map[graph.NodeID]struct{})
	}
	if _, dup := g.tombstones[id]; !dup {
		g.tombstones[id] = struct{}{}
		g.tombstoneActive.Add(1)
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
		g.nodeIdx.Remove(uint32(lid), id)
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
// TombstonedIDs is safe for concurrent use.
func (g *Graph[N, W]) TombstonedIDs() []graph.NodeID {
	g.tombstoneMu.RLock()
	out := make([]graph.NodeID, 0, len(g.tombstones))
	for id := range g.tombstones {
		out = append(out, id)
	}
	g.tombstoneMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TombstoneCount returns the number of NodeIDs currently marked removed.
// It reads a lock-free counter, so it is cheap enough to gate the optional
// emission of the snapshot tombstone component on every checkpoint.
//
// TombstoneCount is safe for concurrent use.
func (g *Graph[N, W]) TombstoneCount() int { return int(g.tombstoneActive.Load()) }

// HasConstraints reports whether the cypher engine currently has any schema
// constraint registered on this graph. It reads a lock-free counter maintained
// by the engine (SetActiveConstraintCount), so it is cheap enough for the
// checkpointer to consult on every checkpoint to gate the constraints.bin
// self-sufficiency requirement (#1464).
//
// HasConstraints is safe for concurrent use.
func (g *Graph[N, W]) HasConstraints() bool { return g.constraintActive.Load() > 0 }

// SetActiveConstraintCount records the number of schema constraints currently
// registered, for HasConstraints to report. The cypher engine calls it under
// its single-writer lock after every constraint registration, drop, and
// recovery re-seed, so the value never under-counts a durably-registered
// constraint that a concurrent checkpoint might otherwise miss.
//
// SetActiveConstraintCount is safe for concurrent use.
func (g *Graph[N, W]) SetActiveConstraintCount(n int64) { g.constraintActive.Store(n) }

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
	if g.tombstones == nil {
		g.tombstones = make(map[graph.NodeID]struct{}, len(ids))
	}
	for _, id := range ids {
		if _, dup := g.tombstones[id]; !dup {
			g.tombstones[id] = struct{}{}
			g.tombstoneActive.Add(1)
		}
	}
	g.tombstoneMu.Unlock()
}

// IsTombstoned reports whether id has been marked removed via
// [Graph.RemoveNode]. Used by the Cypher executor's AllNodesScan to
// skip phantom nodes (those that the Mapper still indexes but that
// the graph treats as deleted).
func (g *Graph[N, W]) IsTombstoned(id graph.NodeID) bool {
	g.tombstoneMu.RLock()
	defer g.tombstoneMu.RUnlock()
	_, ok := g.tombstones[id]
	return ok
}

// LiveOrder returns the number of non-tombstoned interned nodes.
func (g *Graph[N, W]) LiveOrder() uint64 {
	total := g.adj.Order()
	g.tombstoneMu.RLock()
	dead := uint64(len(g.tombstones))
	g.tombstoneMu.RUnlock()
	if dead > total {
		return 0
	}
	return total - dead
}

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
func (g *Graph[N, W]) IncrEdgesAdded() { g.edgesAddedCount.Add(1) }

// IncrEdgesRemoved records that one edge was removed.
func (g *Graph[N, W]) IncrEdgesRemoved() { g.edgesRemovedCount.Add(1) }

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

// DecrEdgesAdded subtracts one from the added-edge counter.
func (g *Graph[N, W]) DecrEdgesAdded() { g.edgesAddedCount.Add(^uint64(0)) }

// DecrEdgesRemoved subtracts one from the removed-edge counter.
func (g *Graph[N, W]) DecrEdgesRemoved() { g.edgesRemovedCount.Add(^uint64(0)) }

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
	sh := g.nodeLabelShardFor(id)
	sh.mu.Lock()
	if bag, ok2 := sh.m[id]; ok2 {
		if bag.del(lid) {
			// Bag became empty: drop the entry so a node with no labels costs
			// no map slot (matches the prior map behaviour).
			delete(sh.m, id)
		} else {
			sh.m[id] = bag
		}
	}
	sh.mu.Unlock()
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
	if !g.adj.HasEdge(src, dst) {
		return
	}
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	lid := g.reg.Intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	g.setEdgeLabelLocked(k, lid)
	sh.mu.Unlock()
	g.edgeIdx.Add(uint32(lid), srcID)
}

// setEdgeLabelLocked adds lid to the derived label set of k. The caller must
// hold k's edge-label shard write lock AND must already have verified that the
// edge (k.src, k.dst) exists (the slot to receive an inline label is present).
//
// Placement: if the first dst-matching slot carries no label, the label goes
// there; if that slot already carries the SAME label the call is a no-op (the
// label is already in the set); otherwise — the slot holds a different label —
// the label spills to overflow (deduplicated). Membership in the derived union
// is invariant under which slot or overflow physically holds the id.
func (g *Graph[N, W]) setEdgeLabelLocked(k edgeKey, lid LabelID) {
	enc := encodeSlotLabel(lid)
	cur, slotExists := g.firstSlotLabel(k.src, k.dst)
	if slotExists && cur == 0 {
		// Empty inline slot: store the first label there.
		g.adj.SetEdgeLabelSlot(k.src, k.dst, enc)
		return
	}
	if slotExists && cur == enc {
		// Already the inline label — nothing to do.
		return
	}
	// Either the inline slot holds a different label, or (defensively) there is
	// no slot; spill to overflow, but skip if it is already the slot label.
	sh := g.edgeLabelShardFor(k)
	sh.addOverflow(k, lid)
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
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if sh.hasOverflow(k, lid) {
		return true
	}
	found := false
	g.slotLabelsForPair(srcID, dstID, func(slotLid LabelID) {
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
	// Prefer dropping the overflow copy; if the label is not in overflow it
	// must live in an inline slot, so clear the FIRST dst-matching slot whose
	// label decodes to lid. The whole update is under the shard lock so the
	// slot and overflow halves of the derived set stay consistent for readers.
	if !sh.removeOverflow(k, lid) {
		g.clearFirstSlotLabel(srcID, dstID, lid)
	}
	sh.mu.Unlock()
}

// clearFirstSlotLabel clears the label of the FIRST dst-matching adjacency
// slot of src whose label decodes to lid (not merely the first dst-matching
// slot — a multigraph pair may carry different labels on its parallel slots).
// The caller must hold the pair's edge-label shard write lock. No-op when no
// such slot carries lid (e.g. the edge was already removed — the orphan case,
// where the label lived only in overflow and was handled by the caller).
func (g *Graph[N, W]) clearFirstSlotLabel(srcID, dstID graph.NodeID, lid LabelID) {
	g.adj.ClearEdgeLabelSlotValue(srcID, dstID, encodeSlotLabel(lid))
}

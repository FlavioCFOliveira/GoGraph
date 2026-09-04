package exec

// create_index.go — CreateIndex DDL operator (task-294, extended by rmp #2703).
//
// CreateIndex is a single-row DDL Volcano operator that registers a secondary
// index — or an index PAIR — with an index.Manager. It emits zero output rows
// on success.
//
// # Two forms
//
// [NewCreateIndexOp] is the standalone form, for an embedder driving the exec
// layer directly. It CONSTRUCTS the subscriber itself from an [IndexKindExec]:
//
//   - hash  → graph/index/hash.Index[string] (property values treated as strings)
//   - btree → graph/index/btree.Index[string] (property values treated as strings)
//
// and registers it with no barrier. The indexes it builds are UNBOUND — empty
// until populated by explicit Insert calls.
//
// [NewCreateIndexPairOp] is the form the Cypher engine uses for CREATE INDEX on
// a btree (Engine.createBTreeIndexLocked, rmp #2703). The caller supplies
// subscribers it has already built and backfilled, plus the barrier to register
// them under, and the operator registers BOTH inside ONE barrier invocation.
// That constructor documents why splitting the pair across two barriers is a
// consistency defect, not merely an inelegance.
//
// The registered subscribers are included in the next snapshot via
// store/snapshot.WriteIndexes.
//
// Note: the Cypher engine routes hash CREATE INDEX through its own path
// (Engine.runCreateHashIndex, task #1340) rather than through this operator; it
// builds a BOUND hash index — backfilled from pre-existing data and
// self-maintaining via the change fan-out — and registers its own pair without
// a barrier, for the reason recorded on Engine.createHashIndexLocked.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
)

// IndexKindExec distinguishes hash vs. btree in the exec layer.
type IndexKindExec uint8

const (
	// ExecIndexHash creates a hash.Index[string].
	ExecIndexHash IndexKindExec = iota
	// ExecIndexBTree creates a btree.Index[string].
	ExecIndexBTree
)

// IndexRegistration names one already-built subscriber and the name to register
// it under. A zero IndexRegistration (nil Sub) means "no index": it is how
// [NewCreateIndexPairOp] is told that no companion was built.
//
// Sub is an interface, so a caller holding a typed nil pointer must leave the
// whole struct zero rather than assigning that pointer — an interface wrapping
// a nil pointer is not itself nil.
type IndexRegistration struct {
	// Sub is the subscriber to register. nil means the slot is unused.
	Sub index.Subscriber
	// Name is the manager key to register Sub under.
	Name string
}

// CreateIndexOp is a Volcano DDL operator that registers a new secondary index,
// optionally together with a companion index in the same barrier.
//
// CreateIndexOp is NOT safe for concurrent use.
type CreateIndexOp struct {
	ctx            context.Context //nolint:containedctx // stored for per-Next ctx check
	mgr            *index.Manager
	barrier        func(func() error) error
	onSchemaChange func()
	primary        index.Subscriber
	companion      index.Subscriber
	name           string
	companionName  string
	idxType        IndexKindExec
	ifNotExists    bool
	done           bool
	registered     bool
	companionDone  bool
}

// NewCreateIndexOp creates a standalone CreateIndexOp: it builds an unbound
// index of kind kind and registers it under name, with no barrier and no
// companion. onSchemaChange, when non-nil, is invoked exactly once after the
// operator successfully creates a new index in mgr — i.e. NOT when the
// IF NOT EXISTS branch silently absorbs a duplicate.
//
// For the paired, barrier-wrapped form the Cypher engine uses, see
// [NewCreateIndexPairOp].
func NewCreateIndexOp(
	name string,
	kind IndexKindExec,
	ifNotExists bool,
	mgr *index.Manager,
	onSchemaChange func(),
) *CreateIndexOp {
	return &CreateIndexOp{
		name:           name,
		idxType:        kind,
		ifNotExists:    ifNotExists,
		mgr:            mgr,
		onSchemaChange: onSchemaChange,
	}
}

// NewCreateIndexPairOp creates a CreateIndexOp that registers primary and, when
// companion.Sub is non-nil, companion — BOTH inside ONE invocation of barrier.
// The subscribers are supplied already built and, where the caller needs it,
// already backfilled; this operator does not construct or populate them.
//
// # Why the pair must not be split across two barriers
//
// The barrier the Cypher engine passes is [lpg.Graph.ApplyAtomically], which
// holds the graph's visibility gate EXCLUSIVELY, while a write transaction's
// index-change fan-out ([IndexBuffer.Commit] → index.Manager.ApplyBatch) runs
// under a SHARED hold of that same gate. One barrier therefore excludes the
// fan-out from the whole registration: a concurrent batch lands entirely before
// both registrations or entirely after them, and both indexes converge.
//
// Split across two barriers, the gate is released between them and a batch can
// land in the gap. It is fanned out to the subscribers registered AT THAT
// MOMENT — the primary but not the companion — and the companion, whose
// backfill snapshot predates that batch, misses those entries PERMANENTLY: it
// self-maintains only from changes delivered after it is registered. A later
// query served by the companion then returns an incomplete result for a
// committed write. That is a Consistency defect, not a missed optimisation,
// which is why the single barrier is a contract of this constructor and is
// gated by a test (TestCreateIndexPairOp_PairRegistersInOneBarrier).
//
// index.Manager's own lock does not supply this: CreateIndex takes it
// exclusively once PER REGISTRATION, so two registrations are two acquisitions
// with a window between them, whereas ApplyBatch holds it shared across a whole
// batch.
//
// # Prior art
//
// Publishing every structure of a multi-object DDL in ONE swap is the mechanism
// Neo4j uses for the same problem: IndexingService.createIndexes takes a
// varargs IndexDescriptor... and performs a single IndexMapReference.modify —
// one copy-on-write swap of a volatile IndexMap — so N index proxies become
// visible together (neo4j 2026.07.0, commit f213380f812b820a1b312e2ea52cb3d8f,
// community/kernel/src/main/java/org/neo4j/kernel/impl/api/index/
// IndexingService.java and IndexMapReference.java).
//
// PostgreSQL reaches the same guarantee by a different route that GoGraph
// cannot borrow: every catalogue row CREATE INDEX writes — pg_class,
// pg_attribute, pg_index, pg_constraint — is stamped with the creating
// transaction's single xmin, so they become visible in one instant at commit
// without any lock (postgres 17.11, commit ec3f6a6a7dd82a8ce455a0710ef75172f9,
// src/backend/catalog/index.c index_create, src/backend/access/heap/heapam.c
// heap_prepare_insert). That is an MVCC catalogue; this index manager is a
// plain map with no per-row visibility, so the barrier is the only mechanism
// available here.
//
// PostgreSQL's non-atomic path is instructive for what it does INSTEAD when it
// cannot be atomic: CREATE INDEX CONCURRENTLY publishes the index in stages and
// gates them with a one-way flag ladder in which MAINTENANCE turns on before
// USE — indisready makes the executor start inserting into the index, and only
// the later indisvalid lets the planner read from it
// (src/backend/optimizer/util/plancat.c, "the executor still needs to insert
// into 'invalid' indexes, if they're marked indisready"). That ordering is the
// alternative to a barrier here, and is NOT what this operator does: the
// companion is registered for maintenance and for use at the same instant,
// which is sound only because the barrier makes that instant indivisible.
//
// barrier may be nil, in which case the registrations run unwrapped — correct
// only for a caller that already excludes the fan-out by other means, or has no
// fan-out to exclude.
//
// ifNotExists absorbs [index.ErrIndexExists] on the PRIMARY, exactly as the
// standalone form does; the companion's ErrIndexExists is always absorbed,
// since two user indexes on the same (label, property) legitimately share one
// companion. onSchemaChange, when non-nil, is invoked INSIDE the barrier after
// a real registration, so the caches it invalidates cannot be repopulated from
// the pre-change catalog before the change is visible. It is NOT invoked when
// IF NOT EXISTS absorbed the primary. The Engine's CREATE INDEX (btree) path
// wires e.ClearPlanCache as onSchemaChange, so cached plans are invalidated
// after a real schema mutation.
//
// After Next has run, [CreateIndexOp.Registered] and
// [CreateIndexOp.CompanionRegistered] report which registrations THIS operator
// performed, so a caller can unwind exactly those and no others.
func NewCreateIndexPairOp(
	primary IndexRegistration,
	companion IndexRegistration,
	ifNotExists bool,
	mgr *index.Manager,
	barrier func(func() error) error,
	onSchemaChange func(),
) *CreateIndexOp {
	return &CreateIndexOp{
		name:           primary.Name,
		primary:        primary.Sub,
		companionName:  companion.Name,
		companion:      companion.Sub,
		ifNotExists:    ifNotExists,
		mgr:            mgr,
		barrier:        barrier,
		onSchemaChange: onSchemaChange,
	}
}

// Registered reports whether THIS operator registered the primary index. It is
// false before Next runs, and false when IF NOT EXISTS absorbed an existing
// name — the case in which the statement applied no schema change.
func (op *CreateIndexOp) Registered() bool { return op.registered }

// CompanionRegistered reports whether THIS operator registered the companion
// index. It is false when no companion was supplied and when the companion's
// name was already taken by a registration this operator did not perform, so an
// unwinding caller never drops a companion another live index still relies on.
func (op *CreateIndexOp) CompanionRegistered() bool { return op.companionDone }

// Init implements Operator.
func (op *CreateIndexOp) Init(ctx context.Context) error {
	op.ctx = ctx
	op.done = false
	op.registered = false
	op.companionDone = false
	return nil
}

// Next implements Operator. It performs the CREATE INDEX side effect on the
// first call, then signals end-of-stream. Returns (false, nil) immediately on
// subsequent calls.
func (op *CreateIndexOp) Next(_ *Row) (bool, error) {
	if op.done {
		return false, nil
	}
	op.done = true

	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	sub := op.primary
	if sub == nil {
		var err error
		if sub, err = newUnboundIndex(op.idxType); err != nil {
			return false, err
		}
	}

	// ONE barrier invocation for the whole pair — see [NewCreateIndexPairOp].
	if op.barrier == nil {
		return false, op.register(sub)
	}
	return false, op.barrier(func() error { return op.register(sub) })
}

// register performs both registrations. It is the body the barrier wraps, so
// everything it does — the primary, the companion, and the schema-change
// notification — happens inside ONE barrier invocation.
func (op *CreateIndexOp) register(sub index.Subscriber) error {
	if err := op.mgr.CreateIndex(op.name, sub); err != nil {
		if op.ifNotExists && errors.Is(err, index.ErrIndexExists) {
			// IF NOT EXISTS — silently succeed; no schema change, and no
			// companion registration either: an index already covering this
			// name already has whatever companion it needs.
			return nil
		}
		return fmt.Errorf("exec: CreateIndex %q: %w", op.name, err)
	}
	op.registered = true

	if op.companion != nil {
		// ErrIndexExists is absorbed: a second user index on the same
		// (label, property) shares the one companion already registered.
		// companionDone stays false there, so the caller's unwind never drops a
		// companion another live index relies on.
		if err := op.mgr.CreateIndex(op.companionName, op.companion); err == nil {
			op.companionDone = true
		} else if !errors.Is(err, index.ErrIndexExists) {
			return fmt.Errorf("exec: CreateIndex %q (numeric companion): %w", op.companionName, err)
		}
	}

	// Real schema mutation: notify so dependent caches (e.g. the plan cache)
	// can invalidate stale entries built before the new index existed. Called
	// inside the barrier, so no cache can be refilled from the pre-change
	// catalog between the registration and the invalidation.
	if op.onSchemaChange != nil {
		op.onSchemaChange()
	}
	return nil
}

// newUnboundIndex builds the empty subscriber the standalone form registers.
func newUnboundIndex(kind IndexKindExec) (index.Subscriber, error) {
	switch kind {
	case ExecIndexHash:
		return indexhash.New[string](), nil
	case ExecIndexBTree:
		return indexbtree.New[string](), nil
	default:
		return nil, fmt.Errorf("exec: CreateIndex: unknown index type %d", kind)
	}
}

// Close implements Operator.
func (op *CreateIndexOp) Close() error { return nil }

package cypher

// index_binding.go — bound hash indexes for CREATE INDEX (task #1340).
//
// CREATE INDEX used to register an empty, unbound hash.Index[string]: nothing
// backfilled the pre-existing nodes and hash.Index.Apply was a no-op, so the
// index stayed permanently empty while the planner happily rewrote the
// matching equality predicate into a NodeByIndexSeek — every query on the
// indexed (label, property) pair returned zero rows with no error, for both
// pre-existing and future data.
//
// The fix has three legs, all in this file plus graph/index/hash:
//
//  1. The engine now creates a BOUND hash index (hash.NewBound) whose
//     binding closures give hash.Index.Apply enough context — interned
//     property/label IDs, value projection, and final-state liveness/label
//     gates — to maintain itself from the index.Manager change fan-out the
//     write path already emits at commit time.
//  2. CREATE INDEX backfills the index from the live graph BEFORE
//     registering it, all under the engine's writer serialisation, so no
//     write transaction can slip between the scan and the registration.
//  3. The index content is exactly the live nodes of the bound label whose
//     bound property holds a plain string — the same population Apply
//     maintains — so the planner's NodeByIndexSeek rewrite stays a
//     transparent optimisation.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// projectStringPropValue projects an index.Change value payload (an
// lpg.PropertyValue on the engine's write path) to a hash-index string key.
// ok is false for absent payloads, non-string property kinds, and the
// SOH-tagged temporal encodings (see decodeTemporalString): a temporal value
// is not equal to any plain Cypher string, so indexing its raw encoded form
// would let a pathological string literal seek match a node the scan+filter
// path would reject.
func projectStringPropValue(v any) (string, bool) {
	pv, ok := v.(lpg.PropertyValue)
	if !ok || pv.Kind() != lpg.PropString {
		return "", false
	}
	s, ok := pv.String()
	if !ok {
		return "", false
	}
	if _, isTemporal := decodeTemporalString(s); isTemporal {
		return "", false
	}
	return s, true
}

// newBoundNodeHashIndex builds a hash.Index[string] bound to (label, prop) on
// g. The binding closures read g's FINAL state — Apply runs at commit time,
// after the transaction's eager mutations — which is the state the index must
// converge to (see hash.Binding).
func newBoundNodeHashIndex(
	g *lpg.Graph[string, float64], label, prop string,
) (*indexhash.Index[string], error) {
	labelID := uint32(g.Registry().Intern(label))
	propID := uint32(g.PropertyKeys().Intern(prop))
	mapper := g.AdjList().Mapper()
	nodeIdx := g.NodeIndex()
	return indexhash.NewBound(indexhash.Binding[string]{
		PropertyID: propID,
		LabelID:    labelID,
		Label:      label,
		Property:   prop,
		Project:    projectStringPropValue,
		Eligible: func(id graph.NodeID) bool {
			return !g.IsTombstoned(id) && nodeIdx.Has(labelID, id)
		},
		CurrentValue: func(id graph.NodeID) (string, bool) {
			if g.IsTombstoned(id) {
				return "", false
			}
			key, ok := mapper.Resolve(id)
			if !ok {
				return "", false
			}
			pv, ok := g.GetNodeProperty(key, prop)
			if !ok {
				return "", false
			}
			return projectStringPropValue(pv)
		},
	})
}

// backfillNodeHashIndex inserts every live node of label whose prop holds an
// indexable string into idx. Callers must hold the engine's writer
// serialisation so no write transaction can interleave with the scan.
//
// The scan is two-phase for the same liveness reason as
// [Engine.scanLabelProperty] (task #1339): phase 1 snapshots the interned
// (id, key) pairs under the mapper shard locks and touches nothing else;
// phase 2 resolves tombstone, label, and property state with no shard lock
// held, so a queued writer on a mapper shard cannot deadlock a nested lookup.
func (e *Engine) backfillNodeHashIndex(idx *indexhash.Index[string], label, prop string) {
	mapper := e.g.AdjList().Mapper()

	type nodeRef struct {
		id  graph.NodeID
		key string
	}
	refs := make([]nodeRef, 0, mapper.Len())
	mapper.Walk(func(id graph.NodeID, key string) bool {
		refs = append(refs, nodeRef{id: id, key: key})
		return true
	})

	for i := range refs {
		r := refs[i]
		if e.g.IsTombstoned(r.id) {
			continue
		}
		if !e.g.HasNodeLabel(r.key, label) {
			continue
		}
		pv, ok := e.g.GetNodeProperty(r.key, prop)
		if !ok {
			continue
		}
		if s, ok := projectStringPropValue(pv); ok {
			idx.Insert(s, r.id)
		}
	}
}

// indexFanoutActive reports whether the write path must capture old property
// values and node-removal changes for the secondary-index fan-out: only when
// a change buffer is wired AND at least one index is registered. Registration
// happens under the writer serialisation the calling transaction already
// holds, so the count cannot change mid-transaction. Keeping the capture
// gated spares index-free workloads the extra pre-image reads.
func indexFanoutActive(g *lpg.Graph[string, float64], buf *exec.IndexBuffer) bool {
	if buf == nil {
		return false
	}
	mgr := g.IndexManager()
	return mgr != nil && mgr.Count() > 0
}

// enqueueNodeRemovalChanges emits the per-property and per-label removal
// changes for a live node about to be removed, so subscribed indexes drop
// every entry that describes it (a deleted node must never be served by a
// NodeByIndexSeek). Callers invoke it BEFORE tombstoning the node — the
// captures read the node's pre-removal state — and only when
// [indexFanoutActive] holds. Property deletions are enqueued first so a
// bound hash index removes the node via the change's old value; the label
// removals that follow are no-ops for it (the node is tombstoned by the time
// the batch applies) but keep any label-scoped subscriber consistent.
func enqueueNodeRemovalChanges(g *lpg.Graph[string, float64], buf *exec.IndexBuffer, n string, id graph.NodeID) {
	for key, pv := range g.NodeProperties(n) {
		buf.Enqueue(index.Change{
			Op:       index.OpDelNodeProperty,
			Node:     id,
			Property: uint32(g.PropertyKeys().Intern(key)),
			OldValue: pv,
		})
	}
	for _, lb := range g.NodeLabels(n) {
		buf.Enqueue(index.Change{
			Op:    index.OpRemoveNodeLabel,
			Node:  id,
			Label: uint32(g.Registry().Intern(lb)),
		})
	}
}

// lockWriterForDDL acquires the engine's writer serialisation for a schema
// operation that must be atomic against concurrent write transactions (the
// CREATE INDEX backfill + registration). For a WAL-backed engine the store's
// single-writer mutex is taken by opening (and later rolling back) an empty
// transaction — the same mutex every RunInTx / BeginTx write holds for its
// duration; for a store-less engine it is [Engine.writeMu], mirroring
// [Engine.lockWriter]. The returned unlock must be called exactly once.
//
// The rollback of the op-less transaction is a pure mutex release: no op was
// appended, so nothing reaches the WAL and nothing is undone.
func (e *Engine) lockWriterForDDL(ctx context.Context) (unlock func(), err error) {
	if e.store != nil {
		tx, berr := e.store.BeginCtx(ctx)
		if berr != nil {
			return nil, berr
		}
		return func() { _ = tx.Rollback() }, nil
	}
	e.writeMu.Lock()
	return e.writeMu.Unlock, nil
}

// runCreateHashIndex executes CREATE INDEX for the hash kind: it builds a
// bound index (so post-creation writes maintain it via the change fan-out),
// backfills it from the pre-existing data, and registers it — all under the
// engine's writer serialisation so no concurrent write can race between the
// backfill scan and the registration (task #1340).
//
// IF NOT EXISTS that absorbs an already-registered name is a silent no-op
// with no schema change and no plan-cache invalidation, matching the
// historical CreateIndexOp semantics; a real registration invalidates the
// plan cache exactly once.
func (e *Engine) runCreateHashIndex(ctx context.Context, p *ir.CreateIndex, idxMgr *index.Manager) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock, err := e.lockWriterForDDL(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Duplicate-name fast path: skip the O(N) backfill scan. The error shape
	// matches the historical CreateIndexOp path ("exec: CreateIndex %q:"
	// wrapping the manager's ErrIndexExists) so callers matching with
	// errors.Is or asserting on the message observe no change.
	if _, gerr := idxMgr.GetIndex(p.Name); gerr == nil {
		if p.IfNotExists {
			return emptyDDLResult(ctx), nil
		}
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name,
			fmt.Errorf("%w: %q", index.ErrIndexExists, p.Name))
	}

	idx, err := newBoundNodeHashIndex(e.g, p.Label, p.Property)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name, err)
	}

	// Backfill BEFORE registration: a concurrent reader's plan build either
	// misses the index (scan+filter, correct) or sees it fully populated.
	e.backfillNodeHashIndex(idx, p.Label, p.Property)

	if cerr := idxMgr.CreateIndex(p.Name, idx); cerr != nil {
		if p.IfNotExists && errors.Is(cerr, index.ErrIndexExists) {
			return emptyDDLResult(ctx), nil
		}
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name, cerr)
	}
	// Real schema mutation: invalidate cached plans built before the index
	// existed (mirrors CreateIndexOp's onSchemaChange contract).
	e.ClearPlanCache()
	return emptyDDLResult(ctx), nil
}

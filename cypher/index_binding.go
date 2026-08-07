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
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
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
	g *lpg.ReadView[string, float64], label, prop string,
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

// backfillParallelMinNodes is the snapshot size at or above which the lock-free
// phase-2 of [Engine.backfillNodeHashIndex] is partitioned across a bounded
// worker pool. Below it the goroutine fan-out costs more than it saves, and the
// common small-graph CREATE INDEX stays a serial loop. The per-node phase-2
// work (tombstone + label + property + project + insert) is cheap, so the floor
// is set well above the fan-out break-even; the win the parallel path targets
// is wall-clock on a huge (millions-of-nodes) DDL backfill.
const backfillParallelMinNodes = 8192

// pollGranularityMask selects a cancellation checkpoint every 4096 rows —
// shared by every ctx-polling scan/backfill loop in this file, so the
// granularity is changed in exactly one place if it is ever retuned.
const pollGranularityMask = 0xFFF

// shouldPollWorkerRelative reports whether i is a cancellation checkpoint
// within a worker's own [lo, hi) range (rmp #1872), extracted to a named,
// directly testable function rather than left as an inline expression inside
// [Engine.backfillNodeHashIndex]'s processRange closure: i=lo always
// satisfies (lo-lo)&pollGranularityMask == 0 trivially, so every worker's
// very first iteration is guaranteed to be a checkpoint regardless of how
// its range happens to align with the absolute pollGranularityMask boundary
// — unlike the pre-fix global i&pollGranularityMask==0 check, which placed
// zero checkpoints inside most workers' own ranges (see processRange's own
// comment for the audit's 20,000-row/10-worker measurement).
func shouldPollWorkerRelative(i, lo int) bool {
	return (i-lo)&pollGranularityMask == 0
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
//
// Phase 2 is partitioned across a bounded worker pool (capped at GOMAXPROCS)
// for large snapshots (#1723): the graph reads it performs are the same
// concurrent-safe reads any read query issues, idx (a hash.Index) is documented
// safe for concurrent use, and per-key insertion is commutative (set
// semantics), so the resulting index contents are identical regardless of
// worker count or scheduling. CREATE INDEX runs under the exclusive DDL
// lock, so the only effect is shorter wall-clock on the blocking DDL — but that
// matters: a 100M-node index build must not freeze writers for minutes.
//
// ctx is polled every 4096 nodes. On cancellation the backfill stops and
// returns ctx.Err(); the caller aborts the CREATE INDEX/CONSTRAINT, and because
// the index is registered only after a successful backfill, a cancelled partial
// index is discarded and never observed (atomicity preserved). Callers that
// must not be interruptible (recovery, constraint-drop rewind) pass a
// background context, for which this never returns an error.
func (e *Engine) backfillNodeHashIndex(ctx context.Context, idx *indexhash.Index[string], label, prop string) error {
	mapper := e.g.AdjList().Mapper()

	type nodeRef struct {
		key string
		id  graph.NodeID
	}
	refs := make([]nodeRef, 0, mapper.Len())
	mapper.Walk(func(id graph.NodeID, key string) bool {
		refs = append(refs, nodeRef{id: id, key: key})
		return true
	})

	// processRange runs the lock-free phase-2 over refs[lo:hi], inserting into
	// the concurrent-safe hash index and polling ctx every 4096 nodes via
	// [shouldPollWorkerRelative], relative to this worker's OWN range start
	// (rmp #1872) rather than the shared slice's absolute index: a global
	// i&0xFFF==0 check places zero checkpoints inside most workers' own
	// ranges whenever lo is not itself a multiple of 4096 (which chunk
	// boundaries rarely are), leaving those workers unable to observe an
	// early cancellation request at all.
	processRange := func(lo, hi int) error {
		for i := lo; i < hi; i++ {
			if shouldPollWorkerRelative(i, lo) {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
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
		return nil
	}

	workers := runtime.GOMAXPROCS(0)
	// A forced-serial backfill (EngineOptions.DisableParallelBackfill) takes the
	// single-goroutine path regardless of size; both paths populate identical
	// index contents, which the serial-vs-parallel differential test relies on.
	if !e.parallelBackfillEnabled || len(refs) < backfillParallelMinNodes || workers <= 1 {
		return processRange(0, len(refs))
	}
	if workers > len(refs) {
		workers = len(refs)
	}
	chunk := (len(refs) + workers - 1) / workers
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * chunk
		if lo >= len(refs) {
			break
		}
		hi := lo + chunk
		if hi > len(refs) {
			hi = len(refs)
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			errs[w] = processRange(lo, hi)
		}(w, lo, hi)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// numericCompanionSuffix is the reserved name suffix of the internal numeric
// btree companion (#1652). A user index name never carries it (the DDL parser
// assigns no such suffix), so it unambiguously identifies the companion. The
// procs package duplicates this constant (it must not import cypher) to filter
// the companion out of db.indexes(); the two must stay in sync.
const numericCompanionSuffix = "_btree_num"

// numericBTreeName is the deterministic internal name of the numeric companion
// btree built alongside the user-named string btree on every btree CREATE
// INDEX (#1652). It mirrors the string auto-name "<label>_<prop>_btree" that
// findBoundStringBTree probes, with a "_num" suffix, so findBoundNumericBTree
// can locate it without the user ever naming or seeing it. The companion is
// internal: db.indexes() / SHOW INDEXES filter the suffix so the user observes
// exactly the one index they created (see procs.dbIndexes).
func numericBTreeName(label, prop string) string {
	return strings.ToLower(label) + "_" + strings.ToLower(prop) + numericCompanionSuffix
}

// projectNumericPropValue projects an index.Change value payload (an
// lpg.PropertyValue on the engine's write path) to a unified float64 numeric
// btree key (#1652). BOTH PropInt64 and PropFloat64 are indexed under one
// float64 order, because openCypher orders integers and floats in a single
// numeric order and a numeric range seek must be a SUPERSET of every numeric
// match — an int64-only index would silently drop the float-valued matches and
// be a non-superset.
//
// ok is false for absent payloads, non-numeric kinds (string, bool, list, and
// the SOH-tagged temporal encodings carried as PropString), and for a NaN
// float. NaN is never indexed: under the btree total order it would sort below
// every real value and a non-NaN range never returns it, but excluding it at
// projection keeps the index free of a key the predicate `n.x > 30` can never
// match, and the residual Filter is the final backstop regardless. A large
// int64 whose float64 widening loses precision is still indexed — the residual
// Filter removes any boundary false positive (cypher-expert-consultant).
func projectNumericPropValue(v any) (float64, bool) {
	pv, ok := v.(lpg.PropertyValue)
	if !ok {
		return 0, false
	}
	switch pv.Kind() {
	case lpg.PropInt64:
		i, ok := pv.Int64()
		if !ok {
			return 0, false
		}
		return float64(i), true
	case lpg.PropFloat64:
		f, ok := pv.Float64()
		if !ok || math.IsNaN(f) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// newBoundNodeBTreeIndexNumeric builds a btree.Index[float64] bound to
// (label, prop) on g, the UNIFIED numeric companion to the string btree
// [newBoundNodeBTreeIndex] (#1652). It self-maintains from the index.Manager
// change fan-out exactly like the string btree, using projectNumericPropValue
// so a key is created for every integer- or float-valued node and never for a
// non-numeric, temporal, or NaN value. The companion shares the same Binding
// shape, so the engine's CREATE INDEX wiring builds it from the same closures.
func newBoundNodeBTreeIndexNumeric(
	g *lpg.ReadView[string, float64], label, prop string,
) (*indexbtree.Index[float64], error) {
	labelID := uint32(g.Registry().Intern(label))
	propID := uint32(g.PropertyKeys().Intern(prop))
	mapper := g.AdjList().Mapper()
	nodeIdx := g.NodeIndex()
	return indexbtree.NewBound(indexbtree.Binding[float64]{
		PropertyID: propID,
		LabelID:    labelID,
		Label:      label,
		Property:   prop,
		Project:    projectNumericPropValue,
		Eligible: func(id graph.NodeID) bool {
			return !g.IsTombstoned(id) && nodeIdx.Has(labelID, id)
		},
		CurrentValue: func(id graph.NodeID) (float64, bool) {
			if g.IsTombstoned(id) {
				return 0, false
			}
			key, ok := mapper.Resolve(id)
			if !ok {
				return 0, false
			}
			pv, ok := g.GetNodeProperty(key, prop)
			if !ok {
				return 0, false
			}
			return projectNumericPropValue(pv)
		},
	})
}

// backfillNodeBTreeIndexNumeric bulk-loads every live node of label whose prop
// holds an indexable numeric value (integer or float, NaN excluded) into idx,
// the float64 companion (#1652). It mirrors [Engine.backfillNodeBTreeIndex]
// exactly — the same two-phase scan (snapshot interned (id, key) pairs under
// the mapper shard locks, then resolve liveness/label/property with no shard
// lock held, #1339), the same ~4096-row cancellation-poll granularity as
// [Engine.backfillNodeHashIndex] (rmp #1872), and the same O(n log n) BulkLoad
// (a per-key Insert loop would be O(n²) on the sorted-array leaves). Callers
// must hold the engine's writer serialisation so no write transaction can
// interleave with the scan. Returns ctx.Err() if cancelled mid-scan; the
// index is never registered in that case, so atomicity is unaffected
// (mirroring the hash path's own contract).
func (e *Engine) backfillNodeBTreeIndexNumeric(ctx context.Context, idx *indexbtree.Index[float64], label, prop string) error {
	mapper := e.g.AdjList().Mapper()

	type nodeRef struct {
		key string
		id  graph.NodeID
	}
	refs := make([]nodeRef, 0, mapper.Len())
	mapper.Walk(func(id graph.NodeID, key string) bool {
		refs = append(refs, nodeRef{id: id, key: key})
		return true
	})

	values := make([]float64, 0, len(refs))
	nodes := make([]graph.NodeID, 0, len(refs))
	for i := range refs {
		if i&pollGranularityMask == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
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
		if f, ok := projectNumericPropValue(pv); ok {
			values = append(values, f)
			nodes = append(nodes, r.id)
		}
	}
	// BulkLoad cannot fail here: values and nodes are appended in lockstep, so
	// their lengths are equal by construction.
	_ = idx.BulkLoad(values, nodes)
	return nil
}

// newBoundNodeBTreeIndex builds a btree.Index[string] bound to (label, prop)
// on g, mirroring [newBoundNodeHashIndex]. The bound btree self-maintains from
// the index.Manager change fan-out (btree.Apply) and uses the SAME
// projectStringPropValue gate as the hash index, so a btree key is never
// created for a non-string or SOH-tagged temporal value — load-bearing for an
// ORDERED index, where a raw temporal encoding would otherwise sort into the
// string key space and a range scan could return nodes the scan+filter path
// rejects (#1505, confirmed by storage-engine-auditor).
func newBoundNodeBTreeIndex(
	g *lpg.ReadView[string, float64], label, prop string,
) (*indexbtree.Index[string], error) {
	labelID := uint32(g.Registry().Intern(label))
	propID := uint32(g.PropertyKeys().Intern(prop))
	mapper := g.AdjList().Mapper()
	nodeIdx := g.NodeIndex()
	return indexbtree.NewBound(indexbtree.Binding[string]{
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

// backfillNodeBTreeIndex bulk-loads every live node of label whose prop holds
// an indexable string into idx. Callers must hold the engine's writer
// serialisation so no write transaction can interleave with the scan.
// Returns ctx.Err() if cancelled mid-scan; the index is never registered in
// that case, so atomicity is unaffected (mirroring the hash path's own
// contract).
//
// Unlike [Engine.backfillNodeHashIndex], the population uses
// [btree.Index.BulkLoad] (O(n log n)), not a per-key Insert loop: the sorted-
// array btree's per-key Insert is O(n), so an Insert loop would be O(n²) on
// the pre-existing data (storage-engine-auditor). The two-phase scan is the
// same as the hash backfill: phase 1 snapshots the interned (id, key) pairs
// under the mapper shard locks; phase 2 resolves liveness/label/property with
// no shard lock held, so a queued writer cannot deadlock a nested lookup
// (#1339). Polls ctx at the same ~4096-row granularity as the hash path
// (rmp #1872 — this backfill polled no cancellation at all before).
func (e *Engine) backfillNodeBTreeIndex(ctx context.Context, idx *indexbtree.Index[string], label, prop string) error {
	mapper := e.g.AdjList().Mapper()

	type nodeRef struct {
		key string
		id  graph.NodeID
	}
	refs := make([]nodeRef, 0, mapper.Len())
	mapper.Walk(func(id graph.NodeID, key string) bool {
		refs = append(refs, nodeRef{id: id, key: key})
		return true
	})

	values := make([]string, 0, len(refs))
	nodes := make([]graph.NodeID, 0, len(refs))
	for i := range refs {
		if i&pollGranularityMask == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
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
			values = append(values, s)
			nodes = append(nodes, r.id)
		}
	}
	// BulkLoad cannot fail here: values and nodes are appended in lockstep, so
	// their lengths are equal by construction.
	_ = idx.BulkLoad(values, nodes)
	return nil
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

// indexDefEntry is one user secondary-index definition tracked by the engine's
// [indexDefRegistry]. It carries exactly the durable fields a checkpoint must
// persist (and recovery rebuild from): kind, name, label, property.
type indexDefEntry struct {
	label    string
	property string
	hash     bool // true: hash index; false: btree index
}

// indexDefRegistry is the engine's live registry of USER secondary-index
// definitions, keyed by index name (the identity recovery dedups on). It is the
// index analogue of [exec.ConstraintRegistry] for the snapshot path: the
// checkpointer reads it (via [Engine.IndexSpecsForSnapshot]) to persist
// indexdefs.bin so an index survives a WAL-truncating checkpoint (#1755).
//
// It is internally synchronised: mutations (record / forget) run under the
// engine's DDL exclusion, but snapshot reads happen concurrently
// from the checkpointer goroutine, so every access takes the mutex. The numeric
// companion and UNIQUE backing indexes never enter this registry, so it contains
// exactly the user-named indexes a plain CREATE INDEX declares — no name-suffix
// filtering is needed on read.
type indexDefRegistry struct {
	byName map[string]indexDefEntry
	mu     sync.Mutex
}

// newIndexDefRegistry returns an empty registry.
func newIndexDefRegistry() *indexDefRegistry {
	return &indexDefRegistry{byName: make(map[string]indexDefEntry)}
}

// record inserts or replaces the definition for a user index (last-writer-wins
// by name, matching recovery's indexSet). It is idempotent on a repeated CREATE
// of the same name.
func (r *indexDefRegistry) record(name string, e indexDefEntry) {
	r.mu.Lock()
	r.byName[name] = e
	r.mu.Unlock()
}

// forget removes the definition for name, the dual of record for DROP INDEX.
// Dropping a name that was never recorded is a no-op.
func (r *indexDefRegistry) forget(name string) {
	r.mu.Lock()
	delete(r.byName, name)
	r.mu.Unlock()
}

// count returns the number of user index definitions currently registered.
func (r *indexDefRegistry) count() int {
	r.mu.Lock()
	n := len(r.byName)
	r.mu.Unlock()
	return n
}

// labelProp returns the (label, property) recorded for the user index named
// name, and ok=true when name is a user index. It returns ("", "", false) for
// any name the registry does not hold — a UNIQUE-constraint backing index or a
// numeric companion, neither of which ever enters this registry. It is the
// enrichment source SHOW INDEXES uses to populate its labelsOrTypes/properties
// columns for user indexes (#1922).
func (r *indexDefRegistry) labelProp(name string) (label, property string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, found := r.byName[name]
	if !found {
		return "", "", false
	}
	return e.label, e.property, true
}

// specs returns the registered definitions as snapshot index-def specs in
// deterministic order (by name), for IndexSpecsForSnapshot. nil when empty.
func (r *indexDefRegistry) specs() []snapshot.IndexDefSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byName) == 0 {
		return nil
	}
	out := make([]snapshot.IndexDefSpec, 0, len(r.byName))
	for name, e := range r.byName {
		kind := uint8(txn.IndexKindBTree)
		if e.hash {
			kind = uint8(txn.IndexKindHash)
		}
		out = append(out, snapshot.IndexDefSpec{
			Kind:     kind,
			Name:     name,
			Label:    e.label,
			Property: e.property,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// recordIndexDef records a user index definition in the engine registry and
// re-syncs the graph's lock-free index-count gate. Callers hold the engine's
// exclusive DDL lock. The numeric companion and UNIQUE backing index
// are NOT recorded through this method (their creation sites do not call it).
func (e *Engine) recordIndexDef(name string, hash bool, label, property string) {
	e.indexDefReg.record(name, indexDefEntry{hash: hash, label: label, property: property})
	e.syncIndexCount()
}

// forgetIndexDef drops a user index definition from the engine registry and
// re-syncs the graph's lock-free index-count gate. Callers hold the engine's
// exclusive DDL lock.
func (e *Engine) forgetIndexDef(name string) {
	e.indexDefReg.forget(name)
	e.syncIndexCount()
}

// registerRecoveredIndexes re-creates each durable index that was recovered
// from the WAL (or from the snapshot) and registers it on the engine's
// [index.Manager] so the planner's NodeByIndexSeek rewrites stay effective
// after a restart. It is invoked once at construction from
// [NewEngineWithOptions]. An empty slice is a no-op (store-less or fresh engine).
//
// Hash indexes are reconstructed as bound indexes backfilled from the live
// graph, matching the behaviour of the original CREATE INDEX (task #1340).
// BTree indexes are registered as fresh, empty subscribers — the BTree
// implementation self-maintains from future change events. [index.ErrIndexExists]
// is silently absorbed so that a snapshot that already wired the index does
// not conflict with the WAL replay that also carries a CREATE INDEX op for
// the same name.
func (e *Engine) registerRecoveredIndexes(defs []IndexDef) {
	// The engine is now the authoritative owner of this graph's indexes, so
	// discard any store-direct index count recovery seeded for the engine-less
	// case (#1755) — the engine's own SetActiveIndexCount (via syncIndexCount
	// below) drives HasIndexes from here on. Mirrors registerRecoveredConstraints'
	// ClearStoreConstraints.
	e.g.ClearStoreIndexes()
	idxMgr := e.g.IndexManager()
	for i := range defs {
		d := defs[i]
		// Seed the engine index-def registry from the recovered def. This is the
		// gap-closing step of #1755 option (b): the def carries label/property
		// even when the bound-index build below falls back to an unbound
		// subscriber (empty-graph binding failure), so IndexSpecsForSnapshot can
		// re-persist it on the next checkpoint regardless of bind state — a
		// reconstruction from the index.Manager via BoundNode() could not.
		e.indexDefReg.record(d.Name, indexDefEntry{hash: d.Hash, label: d.Label, property: d.Property})
		if d.Hash {
			// Build a bound hash index and backfill it from the recovered graph
			// so index seeks on the re-opened engine return the correct rows.
			// Binding failures (e.g. an empty graph) fall back to an unbound
			// index; the index will be correct for future writes but empty for
			// pre-existing data — the worst outcome is a NodeByIndexSeek miss
			// that is equivalent to the pre-fix behaviour.
			if boundIdx, bidxErr := newBoundNodeHashIndex(e.g.ReadAt(nil), d.Label, d.Property); bidxErr == nil {
				// Recovery must complete: a background context never cancels, so
				// the backfill never returns an error here.
				_ = e.backfillNodeHashIndex(context.Background(), boundIdx, d.Label, d.Property)
				_ = idxMgr.CreateIndex(d.Name, boundIdx) // absorb ErrIndexExists
			} else {
				sub := indexhash.New[string]()
				_ = idxMgr.CreateIndex(d.Name, sub) // absorb ErrIndexExists
			}
		} else {
			// BTree index: rebuild a BOUND btree backfilled from the recovered
			// graph so range seeks on the re-opened engine return the correct
			// rows (#1505). A fresh empty unbound btree (the pre-#1505
			// behaviour) is never maintained and would make every range seek
			// return zero rows. Binding failures (e.g. an empty graph) fall
			// back to an unbound index, which the planner declines to seek
			// (BoundNode reports false) — the worst outcome is a scan+filter,
			// never wrong rows.
			if boundIdx, bidxErr := newBoundNodeBTreeIndex(e.g.ReadAt(nil), d.Label, d.Property); bidxErr == nil {
				// Recovery must complete: a background context never cancels, so
				// the backfill never returns an error here.
				_ = e.backfillNodeBTreeIndex(context.Background(), boundIdx, d.Label, d.Property)
				_ = idxMgr.CreateIndex(d.Name, boundIdx) // absorb ErrIndexExists
			} else {
				sub := indexbtree.New[string]()
				_ = idxMgr.CreateIndex(d.Name, sub) // absorb ErrIndexExists
			}
		}
		// Rebuild the UNIFIED numeric companion from the SAME recovered def, for
		// BOTH index kinds (#1652 for btree, #2226 for hash): the durable record
		// carries only the one user def (format-neutral — no new persisted
		// IndexKind), so recovery is self-sufficient by re-deriving the companion
		// here, exactly as createBTreeIndexLocked and createHashIndexLocked build
		// it on a live CREATE INDEX. Without this a numeric seek on the re-opened
		// engine would find no companion and fall back to a scan+filter — correct,
		// but the optimisation would be silently lost across a restart, which is
		// the same class of defect as an index that reports ONLINE while holding
		// no entries. The err-guard is defensive: newBoundNodeBTreeIndexNumeric
		// supplies every Binding field, so it does not fail here; if it ever did,
		// the seek would simply fall back to a scan+filter. When two user defs
		// cover the same (label, property) the second CreateIndex(numName, …)
		// returns ErrIndexExists and is absorbed (idempotent rebuild).
		numName := numericBTreeName(d.Label, d.Property)
		if numIdx, nerr := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), d.Label, d.Property); nerr == nil {
			// Recovery must complete: a background context never cancels, so the
			// backfill never returns an error here.
			_ = e.backfillNodeBTreeIndexNumeric(context.Background(), numIdx, d.Label, d.Property)
			_ = idxMgr.CreateIndex(numName, numIdx) // absorb ErrIndexExists
		}
	}
	// Mirror the recovered index set onto the graph's lock-free gate so a
	// post-recovery checkpoint knows indexes exist even when the embedder did not
	// wire checkpoint.WithIndexSpecs (#1755). Synced once after the loop rather
	// than per-record since recovery seeds the registry directly.
	e.syncIndexCount()
}

// runCreateHashIndex executes CREATE INDEX for the hash kind: it builds a
// bound index (so post-creation writes maintain it via the change fan-out),
// backfills it from the pre-existing data, and registers it — all under the
// engine's writer serialisation so no concurrent write can race between the
// backfill scan and the registration (task #1340).
//
// On a WAL-backed engine the successful registration is also made durable: the
// CREATE INDEX op is appended to the WAL and fsynced before the function
// returns, so the index definition survives a crash and is re-registered by
// [store/recovery.Open] (task #1343). A WAL-append failure unwinds the
// in-memory registration (DropIndex) to keep the index manager and the durable
// state consistent (registered ⇔ durable).
//
// IF NOT EXISTS that absorbs an already-registered name is a silent no-op
// with no schema change and no plan-cache invalidation, matching the
// historical CreateIndexOp semantics; a real registration invalidates the
// plan cache exactly once.
func (e *Engine) runCreateHashIndex(ctx context.Context, p *ir.CreateIndex, idxMgr *index.Manager) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.schemaGate.StrongLock()
	defer e.schemaGate.StrongUnlock()
	if e.store == nil {
		return e.createHashIndexLocked(ctx, p, idxMgr, nil)
	}
	// WAL-backed: open the serialising transaction before the backfill scan so
	// no concurrent write can slip between the scan and the registration.
	tx, err := e.store.BeginCtx(ctx)
	if err != nil {
		return nil, err
	}
	res, rerr := e.createHashIndexLocked(ctx, p, idxMgr, tx)
	// Guarded no-op after CommitWALOnly (success or failure); releases the
	// writer registration on earlier error paths. See runCreateConstraint.
	_ = tx.Rollback()
	return res, rerr
}

// createHashIndexLocked executes the CREATE INDEX sequence under the writer
// serialisation held by the caller; tx is the serialising transaction on a
// WAL-backed engine (nil on a store-less one).
func (e *Engine) createHashIndexLocked(ctx context.Context, p *ir.CreateIndex, idxMgr *index.Manager, tx *txn.Tx[string, float64]) (*Result, error) {
	// Duplicate-name fast path: skip the O(N) backfill scan. The error shape
	// matches the historical CreateIndexOp path ("exec: CreateIndex %q:"
	// wrapping the manager's ErrIndexExists) so callers matching with
	// errors.Is or asserting on the message observe no change.
	if _, gerr := idxMgr.GetIndex(p.Name); gerr == nil {
		if p.IfNotExists {
			return emptyDDLResult(), nil
		}
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name,
			fmt.Errorf("%w: %q", index.ErrIndexExists, p.Name))
	}

	idx, err := newBoundNodeHashIndex(e.g.ReadAt(nil), p.Label, p.Property)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name, err)
	}

	// Backfill BEFORE registration: a concurrent reader's plan build either
	// misses the index (scan+filter, correct) or sees it fully populated. A
	// cancelled backfill returns before registration, so the partial index is
	// discarded (and tx is rolled back by the caller) — nothing is observed.
	if berr := e.backfillNodeHashIndex(ctx, idx, p.Label, p.Property); berr != nil {
		return nil, berr
	}

	// Build the UNIFIED numeric companion alongside the hash index (#2226),
	// exactly as createBTreeIndexLocked does for a btree (#1652). Without it a
	// point lookup on a NUMERIC property gets no index at all: the hash index
	// stores only string keys (projectStringPropValue rejects every non-PropString
	// kind), so `CREATE INDEX … ON (n.age)` on an integer property builds an index
	// that can never hold an entry, SHOW INDEXES still reports it ONLINE, and
	// `MATCH (n:L {age: 30})` silently falls back to a full label scan. Since
	// hash is the DEFAULT index type, that was what a plain CREATE INDEX gave a
	// user with a numeric key — measured at 3.90 ms against 6.25 µs with a btree
	// (docs/benchmarks/index-key-type-2026-07-27.md).
	//
	// The companion is keyed by its own deterministic internal name, so the
	// equality rewrite (#2169) finds it through findBoundNumericBTree without a
	// user-named btree existing at all. As on the btree path this adds NO
	// storage-format change: only the hash def is persisted, and
	// registerRecoveredIndexes re-derives the companion from it.
	//
	// The two registrations are NOT wrapped in one visibility barrier, and do not
	// need to be: the companion is internal and purely an optimisation, so a
	// reader that observes only one of the pair is still correct. Seeing only the
	// hash index makes a numeric seek decline and fall back to scan+filter; seeing
	// only the backfilled companion makes the seek return the right rows. Neither
	// order can produce a wrong answer.
	numName := numericBTreeName(p.Label, p.Property)
	numIdx, _ := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), p.Label, p.Property)
	if numIdx != nil {
		if berr := e.backfillNodeBTreeIndexNumeric(ctx, numIdx, p.Label, p.Property); berr != nil {
			return nil, berr
		}
	}

	if cerr := idxMgr.CreateIndex(p.Name, idx); cerr != nil {
		if p.IfNotExists && errors.Is(cerr, index.ErrIndexExists) {
			return emptyDDLResult(), nil
		}
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name, cerr)
	}
	// Absorb ErrIndexExists: two CREATE INDEX statements on the same
	// (label, property) share one companion, whichever index kind they are.
	numRegistered := false
	if numIdx != nil {
		if nerr := idxMgr.CreateIndex(numName, numIdx); nerr == nil {
			numRegistered = true
		}
	}
	// Real schema mutation: invalidate cached plans built before the index
	// existed (mirrors CreateIndexOp's onSchemaChange contract).
	e.ClearPlanCache()

	// Record the user index def in the engine registry BEFORE the WAL commit,
	// while this DDL still holds the exclusive DDL lock (#F-STORE1). A
	// concurrent non-blocking checkpoint captures (WAL watermark, index defs)
	// under the commit lock + inflight drain; performing the registry update
	// here — inside the serialised window, before commitIndexTx releases it —
	// guarantees the checkpoint never observes a watermark past this CREATE
	// while the def registry still lags. (Recording after the commit left that
	// window open: the dropped/created def could be captured inconsistently and
	// the WAL truncated past it, losing or resurrecting the index across a
	// crash.) It is unwound below if the WAL commit fails, so registered ⇔
	// durable ⇔ recorded still holds once the operation returns; a crash in the
	// pre-durable window is harmless because recovery rebuilds the registry from
	// the WAL, which does not carry the un-fsynced CREATE.
	e.recordIndexDef(p.Name, true /* hash */, p.Label, p.Property)

	// Durability: append the CREATE INDEX op to the WAL and fsync so the index
	// definition survives a crash (task #1343). On failure unwind the in-memory
	// registration AND the def record: the index would otherwise stay active in
	// this session while silently vanishing on the next reopen (registered ⇔
	// durable invariant).
	if tx != nil {
		if err := commitIndexTx(tx, txn.OpCreateIndex, txn.IndexKindHash, p.Label, p.Property, p.Name); err != nil {
			// Best-effort unwind: forget the def and drop the just-registered
			// index. Errors are joined but the original cause is always returned.
			e.forgetIndexDef(p.Name)
			derr := idxMgr.DropIndex(p.Name)
			// The companion this statement registered is unwound with it, but
			// only when no OTHER surviving index still covers the pair — two
			// indexes on the same (label, property) share one companion, so an
			// unconditional drop would strip one the other relies on. This runs
			// AFTER dropping p.Name: the orphan check inspects the surviving
			// indexes, and p.Name would otherwise still count as covering.
			if numRegistered {
				dropNumericCompanionIfOrphaned(idxMgr, p.Label, p.Property)
			}
			if derr != nil {
				return nil, errors.Join(err, fmt.Errorf("cypher: unwind CREATE INDEX registration: %w", derr))
			}
			return nil, err
		}
	}
	// The index is registered and durable: report the schema effect (#2212). The two
	// earlier returns in this function are IF NOT EXISTS no-ops and stay uncounted.
	return countedDDLResult(func(c *exec.QueryCounters) { c.IndexesAdded++ }), nil
}

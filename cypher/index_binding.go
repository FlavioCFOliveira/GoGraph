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

// beginIndexBuild opens the two windows a CREATE INDEX build reconciles with —
// the catch-up recording of rmp #2738 and the scan snapshot of rmp #2778 — in
// the one order that is correct, and it exists so that order is written down
// once rather than at each DDL path.
//
// It returns the build log to hand [index.Manager.FinishBuild] (or
// [exec.CreateIndexOp.Catching]), the read view both backfills must scan, and
// the idempotent release for that view's reclamation-horizon slot.
//
// # Why the scan reads a snapshot and not the live graph
//
// The backfill used to read the graph's PHYSICAL LATEST state — [lpg.Graph]'s
// plain accessors, documented to return the current stored value with no version
// walk. That state includes an explicit transaction's EAGER, UNCOMMITTED
// mutations: a statement inside an open transaction applies to the property bag
// immediately and is unwound only by [ExplicitTx.Rollback]'s undo replay. So a
// backfill running while such a transaction was open indexed a value nothing had
// committed, and the rollback then discarded that transaction's
// [exec.IndexBuffer] WITHOUT inverting it — the buffer describes changes that
// were never fanned out, so there is nothing to invert — leaving the backfill's
// own entry in the index forever.
//
// The consequence was a WRONG answer, not merely an incomplete one: the entry
// for the uncommitted value was fabricated, and the entry for the value the
// graph actually held was never written. A seek returned a row the graph does
// not contain, because [exec.NodeByIndexSeek]'s only residual check is on the
// node's LABEL and never on its value (graph/index/build.go carries the argument
// for why that makes a stale entry unfilterable here, unlike in PostgreSQL). No
// concurrency is needed to reach it: one transaction, one CREATE INDEX, one
// rollback, on a single goroutine.
//
// A snapshot read excludes those mutations by construction. The versioned
// accessors walk back over any version stamped by a transaction the snapshot
// cannot see, so the scan reads the last COMMITTED value — which is both the
// state the index must converge to and the state a label scan over the same
// predicate reports.
//
// # Prior art: this is what PostgreSQL does when writers are NOT excluded
//
// PostgreSQL chooses between the two shapes explicitly, on exactly this
// criterion (postgres REL_17_2, commit 6304632eaa2107bb1763d29e213ff166ff6104c0,
// src/backend/access/heap/heapam_handler.c heapam_index_build_range_scan,
// lines 1235-1262):
//
//	"In a normal index build, we use SnapshotAny because we must retrieve all
//	 tuples and do our own time qual checks ... In a concurrent build, or during
//	 bootstrap, we take a regular MVCC snapshot and index whatever's live
//	 according to that."
//
// The SnapshotAny arm does index an uncommitted insert — "We must index such
// tuples, since if the index build commits then they're good" (same file, 1528)
// — and the source states the premise that makes it safe: "Since caller should
// hold ShareLock or better, normally the only way to see this is if it was
// inserted earlier in our own transaction", warning when it is not (1474-1484).
// Both halves of that premise fail here. GoGraph's schema gate is not a
// ShareLock over explicit transactions — rmp #2738 established it cannot be,
// without a three-way deadlock against the store quiesce — and the uncommitted
// writer is a DIFFERENT transaction from the DDL, so their fates are
// independent: the DDL commits while the transaction rolls back, and "if the
// index build commits then they're good" simply does not hold. GoGraph's DDL is
// therefore in PostgreSQL's CONCURRENT position, and the MVCC snapshot is
// PostgreSQL's own answer for that position.
//
// # Why recording starts BEFORE the snapshot is opened
//
// The two windows must OVERLAP. Together with the live fan-out they give every
// change exactly one route into the new index:
//
//   - committed at or before the SNAPSHOT — the scan reads it;
//   - committed after BeginBuild — recorded and replayed by FinishBuild;
//   - committed after FinishBuild — the index is registered, so the fan-out
//     delivers it.
//
// Because recording starts first, a change committed between the two is applied
// TWICE, which the build log's contract already relies on being harmless: an
// index entry is set membership, so a repeated insert is idempotent and a delete
// of an absent value is a no-op (see graph/index/build.go).
//
// Reverse the order and the windows leave a GAP instead: a transaction that
// commits after the snapshot but before recording starts is invisible to the
// scan AND unrecorded, so its change reaches the new index by no route at all —
// the permanent loss rmp #2738 exists to close. This is the same ordering
// PostgreSQL's CREATE INDEX CONCURRENTLY uses for the same reason: it publishes
// indisready so writers begin maintaining the index and only THEN takes the
// reference snapshot — "Now we know that any subsequently-started transactions
// will see the index and insert their new tuples into it. We then take a new
// reference snapshot" (src/backend/catalog/index.c, validate_index's header
// comment at 3247-3252).
//
// The order is enforced here by being in ONE place that both DDL paths call,
// which is the same "correct by construction" discipline rmp #2738 used to keep
// a half-built index out of the manager's map rather than behind a flag. It is
// NOT separately gated by a test, and that is stated rather than implied: the
// window an inversion opens is a few instructions wide, so no test can hit it
// reliably, and a test that claimed to would be measuring the scheduler.
//
// # Lifetime
//
// releaseScanView returns the reclamation-horizon slot the snapshot holds and
// MUST run on every path, including a panic — hence a closure to defer rather
// than a bare [lpg.Graph.EndRead] to remember. It is idempotent, so a caller
// defers it AND calls it early, once the scans are done, so the horizon is not
// pinned across the registration. It is not safe for concurrent use; the DDL
// releases it on its own goroutine.
//
// The build log's own retirement stays with the caller: [index.Manager.AbandonBuild]
// must run AFTER the registration, where releaseScanView must run before it, so
// the two cannot share one closure.
func (e *Engine) beginIndexBuild(idxMgr *index.Manager) (
	log *index.BuildLog, scanView *lpg.ReadView[string, float64], releaseScanView func(),
) {
	// Recording FIRST, snapshot SECOND. Do not reorder — see above.
	log = idxMgr.BeginBuild()
	snap := e.g.BeginRead()
	released := false
	return log, e.g.ReadAt(snap), func() {
		if released {
			return
		}
		released = true
		e.g.EndRead(snap)
	}
}

// uniqueValueSeed is the property values a UNIQUE constraint's value-set must be
// seeded with, collected by [Engine.backfillNodeHashIndex] FROM ITS OWN READS
// (rmp #2792).
//
// # Why the seed comes from the backfill and not from a second scan
//
// A UNIQUE constraint is enforced by two structures that must agree: the value-set
// in [exec.ConstraintRegistry], which is what a write is checked against, and the
// backing hash index, which is what a seek reads. Before rmp #2792 each was
// populated by its own scan of the LIVE graph — [Engine.scanLabelProperty] for the
// value-set, this backfill for the index — so each could observe a different
// instant, and both could observe an explicit transaction's eager, UNCOMMITTED
// mutations. Measured: a rolled-back SET left the index holding one entry for a
// value the graph never committed and none for the value it did hold, while the
// value-set had lost the committed value, so a duplicate of it was ACCEPTED.
//
// Collecting the seed here makes the two answer from THE SAME READ of the same
// node, not merely from the same instant — a stronger property than two scans at
// one snapshot could give, and it costs no extra pass over the graph.
//
// It records the value BEFORE the string projection, because the two structures
// cover different value sets by design: the hash index takes only
// [projectStringPropValue]-projectable strings, while
// [exec.ConstraintRegistry.SeedUniqueValues] canonicalises EVERY non-null value,
// numbers included. Collecting after the projection would silently drop numeric
// values from the value-set and stop UNIQUE being enforced over them.
//
// A nil *uniqueValueSeed means "do not collect", which is what every CREATE INDEX
// and recovery call site passes; those paths allocate nothing for it.
//
// NOT safe for concurrent use. The parallel phase-2 gives each worker its OWN
// instance and merges them in worker order afterwards, so the merged slice is
// byte-for-byte the order the serial path produces.
type uniqueValueSeed struct {
	// values holds one entry per node that carried label and a present prop, in
	// scan order.
	values []lpg.PropertyValue
}

// backfillNodeHashIndex inserts every node of label whose prop holds an
// indexable string, AS OF rv's instant, into idx, and — when seed is non-nil —
// records every non-null value of prop it read into seed (rmp #2792).
//
// rv decides which state is indexed, and it is the caller's whole choice of
// semantics (rmp #2778). A CREATE INDEX passes a view bound to a read snapshot
// ([Engine.beginIndexBuild]) so the scan reads COMMITTED state and cannot
// index an explicit transaction's eager, uncommitted mutation. The recovery and
// constraint call sites pass e.g.ReadAt(nil) — the live stored value, with no
// version walk — which is what they read before this parameter existed and
// which is correct for a graph no transaction is open against.
//
// Every graph read in the body goes through rv. That is deliberate and is the
// property to preserve: a reader auditing this scan for a read that escaped the
// snapshot finds no e.g. reference to check. The one exception is the node
// MAPPER, reached through rv.AdjList(), which is an unversioned CANDIDATE
// source by design — it may over-approximate with ids an uncommitted
// transaction created, and the versioned liveness, label and property reads
// below reject each of them. That is lpg's P4c candidate-set discipline, not a
// gap: an id the snapshot cannot see fails rv.IsTombstoned or rv.HasNodeLabel.
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
// How MUCH shorter now depends on rv, and the parallel win is NOT unconditional
// (measured, rmp #2778). All workers share one [lpg.Snapshot], whose visibility
// memo is guarded by a single mutex, and a versioned property read takes that
// mutex once per node that carries a live version record. On a 50 000-node
// fixture in which EVERY node carried one, the parallel scan went 5.53 ms →
// 10.61 ms (+91.96%, p=0.002, n=6) against the same scan on a live view, and a
// mutex profile attributed 460.69 ms of 549.14 ms of total delay (83.89%) to
// lpg.(*Snapshot).visible reached through rv.GetNodeProperty. With no live
// version records the two views were indistinguishable (p=0.485). Allocations
// are identical either way. The cost is bought deliberately: it buys the
// committed-state guarantee above, it lands only on a blocking DDL that is
// already O(n), and correctness does not trade down to speed. Removing it needs
// a per-worker snapshot fork in lpg, which is not this function's to make.
//
// ctx is polled every 4096 nodes. On cancellation the backfill stops and
// returns ctx.Err(); the caller aborts the CREATE INDEX/CONSTRAINT, and because
// the index is registered only after a successful backfill, a cancelled partial
// index is discarded and never observed (atomicity preserved). Callers that
// must not be interruptible (recovery, constraint-drop rewind) pass a
// background context, for which this never returns an error.
func (e *Engine) backfillNodeHashIndex(
	ctx context.Context, rv *lpg.ReadView[string, float64],
	idx *indexhash.Index[string], label, prop string, seed *uniqueValueSeed,
) error {
	mapper := rv.AdjList().Mapper()

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
	// out collects the value-set seed for this range, or is nil when the caller
	// asked for none. Each parallel worker gets its own, so the collection needs
	// no synchronisation and stays deterministic (see [uniqueValueSeed]).
	processRange := func(lo, hi int, out *uniqueValueSeed) error {
		for i := lo; i < hi; i++ {
			if shouldPollWorkerRelative(i, lo) {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			r := refs[i]
			if rv.IsTombstoned(r.id) {
				continue
			}
			if !rv.HasNodeLabel(r.key, label) {
				continue
			}
			pv, ok := rv.GetNodeProperty(r.key, prop)
			if !ok {
				continue
			}
			if out != nil {
				// BEFORE the projection: the value-set covers every non-null
				// value, the index only projectable strings. See [uniqueValueSeed].
				out.values = append(out.values, pv)
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
		return processRange(0, len(refs), seed)
	}
	if workers > len(refs) {
		workers = len(refs)
	}
	chunk := (len(refs) + workers - 1) / workers
	errs := make([]error, workers)
	// One seed accumulator per worker, merged in worker order below, so the
	// parallel phase produces the SAME slice the serial path would: worker w owns
	// refs[lo:hi] in order, and the ranges are consecutive.
	var parts []uniqueValueSeed
	if seed != nil {
		parts = make([]uniqueValueSeed, workers)
	}
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
			var out *uniqueValueSeed
			if parts != nil {
				out = &parts[w]
			}
			errs[w] = processRange(lo, hi, out)
		}(w, lo, hi)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	// Merge only after every worker returned without error: a cancelled backfill
	// registers nothing, so a partial seed must never reach the caller.
	//
	// Pre-sized from the parts, because the exact total is known here and growing
	// into it instead cost measured bytes for nothing: an alloc profile at
	// -memprofilerate=1 attributed 199.7 MB of 680.2 MB over 30 iterations to this
	// function's own frame, the growth reallocations of this append among them.
	// A capacity hint is what [Performance-First Engineering] asks for wherever
	// the upper bound is knowable, and here it is not merely an upper bound but
	// the answer.
	total := 0
	for w := range parts {
		total += len(parts[w].values)
	}
	if total > 0 {
		seed.values = append(make([]lpg.PropertyValue, 0, len(seed.values)+total), seed.values...)
	}
	for w := range parts {
		seed.values = append(seed.values, parts[w].values...)
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

// backfillNodeBTreeIndexNumeric bulk-loads every node of label whose prop holds
// an indexable numeric value (integer or float, NaN excluded) AS OF rv's
// instant into idx, the float64 companion (#1652). It mirrors
// [Engine.backfillNodeBTreeIndex] exactly — the same two-phase scan (snapshot
// interned (id, key) pairs under the mapper shard locks, then resolve
// liveness/label/property with no shard lock held, #1339), the same ~4096-row
// cancellation-poll granularity as [Engine.backfillNodeHashIndex] (rmp #1872),
// and the same O(n log n) BulkLoad (a per-key Insert loop would be O(n²) on the
// sorted-array leaves). Returns ctx.Err() if cancelled mid-scan; the index is
// never registered in that case, so atomicity is unaffected (mirroring the hash
// path's own contract).
//
// rv carries the same contract as [Engine.backfillNodeHashIndex]'s: a CREATE
// INDEX passes a snapshot-bound view, and it passes THE SAME view it gave the
// primary index, so the user index and its numeric companion are populated from
// ONE instant and cannot disagree about a value that changed between the two
// scans (rmp #2778). Recovery passes e.g.ReadAt(nil).
func (e *Engine) backfillNodeBTreeIndexNumeric(
	ctx context.Context, rv *lpg.ReadView[string, float64],
	idx *indexbtree.Index[float64], label, prop string,
) error {
	mapper := rv.AdjList().Mapper()

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
		if rv.IsTombstoned(r.id) {
			continue
		}
		if !rv.HasNodeLabel(r.key, label) {
			continue
		}
		pv, ok := rv.GetNodeProperty(r.key, prop)
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

// backfillNodeBTreeIndex bulk-loads every node of label whose prop holds an
// indexable string, AS OF rv's instant, into idx. Returns ctx.Err() if
// cancelled mid-scan; the index is never registered in that case, so atomicity
// is unaffected (mirroring the hash path's own contract).
//
// rv carries the same contract as [Engine.backfillNodeHashIndex]'s: a CREATE
// INDEX passes a snapshot-bound view so the scan cannot index an uncommitted
// mutation (rmp #2778); recovery passes e.g.ReadAt(nil).
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
func (e *Engine) backfillNodeBTreeIndex(
	ctx context.Context, rv *lpg.ReadView[string, float64],
	idx *indexbtree.Index[string], label, prop string,
) error {
	mapper := rv.AdjList().Mapper()

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
		if rv.IsTombstoned(r.id) {
			continue
		}
		if !rv.HasNodeLabel(r.key, label) {
			continue
		}
		pv, ok := rv.GetNodeProperty(r.key, prop)
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
// Both index kinds are reconstructed as BOUND indexes — so they self-maintain
// from the commit-time change fan-out — and POPULATED before registration,
// either by hydrating the snapshot payload recovery certified for the name or by
// backfilling from the recovered graph (see index_hydration.go for the
// preconditions and the corruption contract). Populate-then-register is what
// guarantees no index is ever seekable while still empty.
//
// A name already claimed by an earlier registration in this same constructor —
// in practice a UNIQUE constraint's backing index, since
// [Engine.registerRecoveredConstraints] runs first — belongs to the INCUMBENT.
// That instance is the one queries reach and the one its own registration path
// already populated, so nothing is built or populated for it here; only the
// definition is recorded. This is where the pre-#2490 code built a full
// duplicate, backfilled it over the whole mapper, and then discarded it when
// CreateIndex returned [index.ErrIndexExists].
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
		// An already-claimed name belongs to the incumbent (see the method doc):
		// it is populated, it is what queries reach, and building a rival for it
		// would cost a whole-mapper scan to produce something immediately
		// discarded.
		if _, gerr := idxMgr.GetIndex(d.Name); gerr == nil {
			e.registerNumericCompanion(idxMgr, d.Label, d.Property)
			continue
		}
		if d.Hash {
			// Build a bound hash index and POPULATE it from the recovered
			// state — the snapshot payload when recovery certified it usable,
			// otherwise a backfill of the recovered graph — so index seeks on
			// the re-opened engine return the correct rows. Binding failures
			// (e.g. an empty graph) fall back to an unbound index; the index
			// will be correct for future writes but empty for pre-existing
			// data — the worst outcome is a NodeByIndexSeek miss that is
			// equivalent to the pre-fix behaviour.
			if boundIdx, bidxErr := newBoundNodeHashIndex(e.g.ReadAt(nil), d.Label, d.Property); bidxErr == nil {
				e.populateRecoveredIndex(d.Name, d.Label, d.Property, boundIdx, func() {
					// Recovery must complete: a background context never
					// cancels, so the backfill never returns an error here.
					// Live view, not a snapshot: recovery runs inside
					// NewEngineWithOptions before the engine is published, so no
					// transaction can be open against this graph and the live
					// state IS the committed state (rmp #2778).
					_ = e.backfillNodeHashIndex(context.Background(), e.g.ReadAt(nil), boundIdx, d.Label, d.Property, nil)
				})
				_ = idxMgr.CreateIndex(d.Name, boundIdx) // absorb ErrIndexExists
			} else {
				sub := indexhash.New[string]()
				_ = idxMgr.CreateIndex(d.Name, sub) // absorb ErrIndexExists
			}
		} else {
			// BTree index: rebuild a BOUND btree populated from the recovered
			// state so range seeks on the re-opened engine return the correct
			// rows (#1505). A fresh empty unbound btree (the pre-#1505
			// behaviour) is never maintained and would make every range seek
			// return zero rows. Binding failures (e.g. an empty graph) fall
			// back to an unbound index, which the planner declines to seek
			// (BoundNode reports false) — the worst outcome is a scan+filter,
			// never wrong rows.
			if boundIdx, bidxErr := newBoundNodeBTreeIndex(e.g.ReadAt(nil), d.Label, d.Property); bidxErr == nil {
				e.populateRecoveredIndex(d.Name, d.Label, d.Property, boundIdx, func() {
					// Recovery must complete: a background context never
					// cancels, so the backfill never returns an error here.
					// Live view for the reason given on the hash arm above.
					_ = e.backfillNodeBTreeIndex(context.Background(), e.g.ReadAt(nil), boundIdx, d.Label, d.Property)
				})
				_ = idxMgr.CreateIndex(d.Name, boundIdx) // absorb ErrIndexExists
			} else {
				sub := indexbtree.New[string]()
				_ = idxMgr.CreateIndex(d.Name, sub) // absorb ErrIndexExists
			}
		}
		e.registerNumericCompanion(idxMgr, d.Label, d.Property)
	}
	// Mirror the recovered index set onto the graph's lock-free gate so a
	// post-recovery checkpoint knows indexes exist even when the embedder did not
	// wire checkpoint.WithIndexSpecs (#1755). Synced once after the loop rather
	// than per-record since recovery seeds the registry directly.
	e.syncIndexCount()
}

// registerNumericCompanion rebuilds the UNIFIED numeric companion btree for
// (label, property) from a recovered user index definition, for BOTH index kinds
// (#1652 for btree, #2226 for hash).
//
// The durable record carries only the one user def (format-neutral — no new
// persisted IndexKind), so recovery is self-sufficient by re-deriving the
// companion here, exactly as createBTreeIndexLocked and createHashIndexLocked
// build it on a live CREATE INDEX. Without it a numeric seek on the re-opened
// engine would find no companion and fall back to a scan+filter — correct, but
// the optimisation would be silently lost across a restart, which is the same
// class of defect as an index that reports ONLINE while holding no entries.
//
// The companion is populated before registration like every other recovered
// index: hydrated from its own snapshot payload (it is a registered index, so a
// checkpoint persisted it under its own deterministic internal name) or
// backfilled from the recovered graph.
//
// When two user defs cover the same (label, property) they share ONE companion,
// so the second call finds the name already taken and returns without building a
// rival — the pre-#2490 code built and backfilled one, then discarded it on
// [index.ErrIndexExists]. The err-guard on the build is defensive:
// newBoundNodeBTreeIndexNumeric supplies every Binding field, so it does not
// fail; if it ever did, a numeric seek would simply fall back to a scan+filter.
//
// Callers hold the engine's construction-time exclusivity (it is only reached
// from [Engine.registerRecoveredIndexes]).
func (e *Engine) registerNumericCompanion(idxMgr *index.Manager, label, property string) {
	numName := numericBTreeName(label, property)
	if _, gerr := idxMgr.GetIndex(numName); gerr == nil {
		// Already registered: two user defs on the same (label, property) share
		// one companion.
		return
	}
	numIdx, nerr := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), label, property)
	if nerr != nil {
		return
	}
	e.populateRecoveredIndex(numName, label, property, numIdx, func() {
		// Recovery must complete: a background context never cancels, so the
		// backfill never returns an error here. Live view for the reason given
		// in [Engine.registerRecoveredIndexes] (rmp #2778).
		_ = e.backfillNodeBTreeIndexNumeric(context.Background(), e.g.ReadAt(nil), numIdx, label, property)
	})
	_ = idxMgr.CreateIndex(numName, numIdx) // absorb ErrIndexExists
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

	// Record every change fanned out from here until the pair is registered
	// (rmp #2738). The DDL's schema gate excludes autocommit writers for the
	// whole sequence, but an EXPLICIT TRANSACTION takes no such gate — it cannot,
	// being a registered store writer from BEGIN — so its commit-time index
	// fan-out can land between the backfill scan below and the registration at
	// the end, where it reaches no index at all and is lost permanently. The
	// recording is replayed into both indexes by FinishBuild, under the manager's
	// exclusive lock, at the instant they become reachable. See
	// [index.Manager.BeginBuild] for why every change is applied exactly once.
	//
	// Retired unconditionally: AbandonBuild is a no-op once FinishBuild has
	// retired the log, and on every early return below — a duplicate name, a
	// cancelled backfill, a failed WAL commit — it is what stops the manager
	// recording into a log nobody will ever drain.
	// scanView is the instant BOTH backfills below read, and it is opened after
	// the recording starts — the ordering, and why the scan must not read the
	// live property bag at all, are in [Engine.beginIndexBuild] (rmp #2738,
	// rmp #2778).
	buildLog, scanView, releaseScanView := e.beginIndexBuild(idxMgr)
	defer idxMgr.AbandonBuild(buildLog)
	defer releaseScanView()

	idx, err := newBoundNodeHashIndex(e.g.ReadAt(nil), p.Label, p.Property)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateIndex %q: %w", p.Name, err)
	}

	// Backfill BEFORE registration: a concurrent reader's plan build either
	// misses the index (scan+filter, correct) or sees it fully populated — the
	// half-built index is not in the manager's map, so no reader can reach it. A
	// cancelled backfill returns before registration, so the partial index is
	// discarded (and tx is rolled back by the caller) — nothing is observed.
	if berr := e.backfillNodeHashIndex(ctx, scanView, idx, p.Label, p.Property, nil); berr != nil {
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
		if berr := e.backfillNodeBTreeIndexNumeric(ctx, scanView, numIdx, p.Label, p.Property); berr != nil {
			return nil, berr
		}
	}
	// Both scans are done: return the horizon slot before the registration so a
	// long registration cannot pin reclamation. Idempotent with the defer above.
	releaseScanView()

	// Replay the changes recorded since BeginBuild into BOTH indexes and register
	// them, all under one exclusive hold of the manager's lock (rmp #2738). The
	// single hold also supplies here what rmp #2703 established for the btree
	// path with a visibility barrier: the pair becomes reachable in one instant,
	// so a concurrent fan-out cannot land between the two registrations and reach
	// one index but not the other.
	numRegistered := false
	if ferr := idxMgr.FinishBuild(buildLog, func(reg index.RegisterFunc) error {
		if cerr := reg(p.Name, idx); cerr != nil {
			return fmt.Errorf("exec: CreateIndex %q: %w", p.Name, cerr)
		}
		// Absorb ErrIndexExists: two CREATE INDEX statements on the same
		// (label, property) share one companion, whichever index kind they are.
		// Every companion error is absorbed, exactly as before this became one
		// locked sequence: the companion is internal and purely an optimisation,
		// so a user index without it is still correct.
		if numIdx != nil {
			if nerr := reg(numName, numIdx); nerr == nil {
				numRegistered = true
			}
		}
		return nil
	}); ferr != nil {
		if p.IfNotExists && errors.Is(ferr, index.ErrIndexExists) {
			return emptyDDLResult(), nil
		}
		return nil, ferr
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

package cypher

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pendingIndexDelta is the set of (label, property) coordinates on which the
// enclosing write transaction has already mutated the graph WITHOUT the property
// indexes having been told (rmp #2814).
//
// # Why it exists
//
// The graph is mutated EAGERLY — every statement reads through
// [lpg.Graph.WriterViewOf], which sees the transaction's own writes — while the
// property indexes are written ONLY at the transaction boundary: the write
// operators enqueue an [index.Change] per mutation into an [exec.IndexBuffer],
// and the single fan-out to [index.Manager.ApplyBatch] is
// [exec.IndexBuffer.Commit]. Between the mutation and that drain, a property
// index describes the graph as it was BEFORE the transaction started.
//
// That gap is invisible to the LABEL half of the planner, because label reads go
// through the MVCC label overlay in graph/lpg/mvcc_index.go: label adds are
// applied eagerly and removals are deferred and filtered against the reader's
// snapshot. The property half has no overlay at all —
// [github.com/FlavioCFOliveira/GoGraph/graph/index/hash.Index] carries no
// snapshot parameter — so a seek reads the pre-transaction posting list. And
// because the equality rewrite SUBSUMES the Selection it replaces
// ([tryBuildIndexSeekFromSelection] returns the seek in place of the whole
// Selection, leaving only a label residual), nothing downstream re-checks the
// value. Measured at 5b7e930, hash index on (:L, s) over 512 nodes:
//
//	uncommitted SET moves s from "v7" to "zzz", query the OLD key "v7":
//	    seek = 1 (a row no node carries any more), scan = 0
//	same, query the NEW key "zzz":
//	    seek = 0 (the row is lost),               scan = 1
//
// The remedy this type implements is to DECLINE the index access path for the
// coordinates the transaction has dirtied, which turns a wrong answer into a
// slower correct one: the fallback is the scan+filter, which reads the writer
// view and is therefore right in both directions at once.
//
// # Why these two dimensions, and no more
//
// The keys are exactly what a bound index's own [index.Subscriber.Apply] uses to
// decide whether a change concerns it (see hash/index.go Apply: the property arms
// compare Change.Property against the binding's PropertyID, the label arms
// compare Change.Label against its LabelID). So "this index would have consumed
// at least one buffered change" is precisely "this index may be stale", and the
// decline is as tight as the buffer's contents allow:
//
//   - A property change carries Property (the interned key) and Label == 0, so it
//     dirties every index on that property, whatever its label.
//   - A label change carries Label and Property == 0, so it dirties every index
//     scoped to that label, whatever its property — a bound index attaches or
//     detaches the node's CURRENT property value on a label add/remove.
//   - Edge changes dirty nothing here: every access path guarded by this type
//     seeks NODES through a node-bound index, and a node binding ignores edge
//     changes outright.
//
// A delta on (:A, x) therefore leaves seeks on (:B, y) alone, which is the point:
// the common write-then-read statement writes one property and seeks on another.
//
// # Lifetime
//
// It is built once per statement, inside the visibility barrier, from the buffer
// the statement's own mutator adapter writes into, and is read only during that
// statement's plan build. A nil *pendingIndexDelta means "nothing pending" and is
// the state every read-only query is in, so the read path pays one nil check per
// guarded access path and nothing else.
type pendingIndexDelta struct {
	// props holds the property-key NAMES with a buffered node-property change.
	// Names rather than interned ids because the plan-build sites hold the
	// property key as a string and interning at plan time would MUTATE the
	// registry; resolving ids to names here is O(buf.Len()) and read-only.
	props map[string]struct{}
	// labels holds the label NAMES with a buffered node-label change, for the
	// same reason.
	labels map[string]struct{}
	// all forces every property-index access path to decline. It is the answer to
	// "I cannot tell which coordinate this write touched", and the sound answer to
	// that is "every coordinate", never "none". Two things set it:
	//
	//   - a buffered change whose interned id will not resolve back to a name,
	//     which should not happen (the id was interned by the same registry
	//     moments earlier) and is therefore treated as the failure it would be;
	//   - a write node in THIS statement's plan whose property map is an opaque
	//     printed string or a run-time-evaluated expression — CREATE, MERGE,
	//     `SET n += …`, DELETE and DETACH DELETE. See [pendingIndexDelta.addPlanWrites].
	all bool
}

// newPendingIndexDelta summarises buf into the coordinates a property index may
// have gone stale on. It returns nil — the "nothing pending" state — when there
// is no buffer, the buffer is empty, or no buffered change concerns a node label
// or a node property.
//
// Returning nil rather than an empty struct is deliberate: the guards test the
// pointer, so an autocommit statement that has not written yet, and every
// read-only query, allocate no map and build no set.
func newPendingIndexDelta(buf *exec.IndexBuffer, g *lpg.Graph[string, float64]) *pendingIndexDelta {
	if buf == nil || g == nil || buf.Len() == 0 {
		return nil
	}
	var d pendingIndexDelta
	for _, c := range buf.Pending() {
		switch c.Op {
		case index.OpSetNodeProperty, index.OpDelNodeProperty:
			name, ok := g.PropertyKeys().Resolve(lpg.PropertyKeyID(c.Property))
			if !ok || name == "" {
				d.all = true
				continue
			}
			if d.props == nil {
				d.props = make(map[string]struct{}, 4)
			}
			d.props[name] = struct{}{}
		case index.OpAddNodeLabel, index.OpRemoveNodeLabel:
			name, ok := g.Registry().Resolve(lpg.LabelID(c.Label))
			if !ok || name == "" {
				d.all = true
				continue
			}
			if d.labels == nil {
				d.labels = make(map[string]struct{}, 2)
			}
			d.labels[name] = struct{}{}
		default:
			// Edge changes (OpAddEdgeLabel, OpRemoveEdgeLabel, OpSetEdgeProperty,
			// OpDelEdgeProperty): a node-bound property index ignores them, so they
			// cannot make one stale. This is what keeps the bulk-load idiom
			// `UNWIND $rows AS r MATCH (a:A {id: r.a}) MATCH (b:A {id: r.b})
			// CREATE (a)-[:E]->(b)` seeking on (:A, id) — the shape rmp #2225
			// admitted the write-path seek for in the first place.
		}
	}
	if !d.all && d.props == nil && d.labels == nil {
		return nil
	}
	return &d
}

// blocksNodeIndex reports whether an index bound to (label, property) may be
// stale for this transaction, and must therefore not be used as an access path.
//
// A nil receiver reports false, which is what makes the read path free.
//
// An empty label is the [ir.AllNodesScan] leaf: the rewrite then has no label to
// match an index binding against, and any buffered label change could have
// attached a node to whatever label the index it finds is scoped to — so ANY
// pending label change blocks it. In practice the hash-seek funnels decline an
// empty label before reaching here ([tryNamedHashSeek]) or are reached only with
// a labelled scan leaf, so this arm is a belt on top of a brace.
func (d *pendingIndexDelta) blocksNodeIndex(label, property string) bool {
	if d == nil {
		return false
	}
	if d.all {
		return true
	}
	if _, ok := d.props[property]; ok {
		return true
	}
	if label == "" {
		return len(d.labels) > 0
	}
	_, ok := d.labels[label]
	return ok
}

// addProp records one property-key name as dirtied.
func (d *pendingIndexDelta) addProp(name string) {
	if name == "" {
		// A write whose property key is not statically known. The sound answer is
		// every coordinate, not none.
		d.all = true
		return
	}
	if d.props == nil {
		d.props = make(map[string]struct{}, 4)
	}
	d.props[name] = struct{}{}
}

// addLabel records one label name as dirtied.
func (d *pendingIndexDelta) addLabel(name string) {
	if name == "" {
		d.all = true
		return
	}
	if d.labels == nil {
		d.labels = make(map[string]struct{}, 2)
	}
	d.labels[name] = struct{}{}
}

// addPlanWrites folds the coordinates THIS statement's own write clauses may
// touch into d.
//
// # Why the buffer alone is not enough
//
// The change buffer answers what EARLIER statements of an explicit transaction
// left unflushed, because the plan is built before this statement's first row
// flows. It is therefore empty for a statement that writes and then reads back
// within itself, and that shape reproduces the defect with no BEGIN anywhere:
//
//	CREATE (:L {s: 'qqq'}) WITH count(*) AS n MATCH (m:L {s: 'qqq'}) RETURN count(m)
//
// returned 0 through the seek at 5b7e930 where the scan returned 1 — the WITH is a
// pipeline breaker, so the CREATE has fully drained by the time the seek runs, and
// the autocommit index drain happens later still (Result.commitUnderBarrier, after
// materialize). A plan-time decision cannot observe that write in the buffer, so
// it reads it off the PLAN instead.
//
// # Precision, and where it is deliberately abandoned
//
// Four write nodes name their coordinate as a plain string and are recorded
// exactly, which is what keeps the overwhelmingly common write-then-seek shape
// seeking:
//
//	MATCH (n:Person {email: $e}) SET n.lastSeen = $t
//
// writes `lastSeen` and seeks on `email`, so the seek survives.
//
// The rest carry their property map as an opaque printed string
// ([ir.CreateNode.Properties]) or as an AST that may hold row-driven expressions
// and `$param` maps resolved only at run time. Decoding those reliably is a larger
// piece of work than a patch release should carry, and decoding them WRONGLY
// reinstates the defect silently — so they set all and decline every
// property-index access path in the statement. That is slower and correct, which
// is the trade this whole change makes.
//
// Relationship writes contribute nothing: every access path guarded here seeks
// NODES through a node-bound index, and [ir.CreateRelationship],
// [ir.DeleteRelationship] and [ir.MergeRelationship] write no node label and no
// node property (verified in cypher/exec/merge_relationship.go, which touches no
// node state at all). That is what keeps the bulk-load idiom
//
//	UNWIND $rows AS r MATCH (a:A {id: r.a}) MATCH (b:A {id: r.b}) MERGE (a)-[:E]->(b)
//
// on the seeks rmp #2225 gave it.
//
// The recursion is [ir.LogicalPlan.Children], which reaches every nested plan
// including both arms of [ir.Foreach]; a nil child is skipped (a leading FOREACH
// has a nil Outer). Any node type not named below is treated as non-writing and
// recursed through — the drift risk that carries is what
// TestPendingIndexDelta_EveryWriteIRNodeIsClassified exists to catch.
func (d *pendingIndexDelta) addPlanWrites(plan ir.LogicalPlan) {
	if plan == nil || d.all {
		return
	}
	switch p := plan.(type) {
	// ── Statically decodable node writes ──
	case *ir.SetProperty:
		// EntityVar may name a node OR a relationship, and which one is not always
		// decidable here. Recording it as a node property over-approximates, which
		// is the safe direction.
		d.addProp(p.PropertyKey)
	case *ir.RemoveProperty:
		d.addProp(p.PropertyKey)
	case *ir.SetLabels:
		for _, l := range p.Labels {
			d.addLabel(l)
		}
	case *ir.RemoveLabels:
		for _, l := range p.Labels {
			d.addLabel(l)
		}

	// ── Node writes whose coordinates are not statically decodable ──
	case *ir.CreateNode, *ir.SetAllProperties, *ir.DeleteNode, *ir.DetachDelete,
		*ir.Merge, *ir.MergePattern:
		// CreateNode/Merge/MergePattern carry the property map as an opaque printed
		// string or a row-driven AST; SetAllProperties writes whatever keys a map
		// expression yields at run time; DeleteNode and DetachDelete enqueue an
		// OpDelNodeProperty for EVERY property the deleted node carries
		// (enqueueNodeRemovalChanges) plus an OpRemoveNodeLabel for every label.
		d.all = true
		return

	// ── Relationship-only writes: no node coordinate, nothing to record ──
	case *ir.CreateRelationship, *ir.DeleteRelationship, *ir.MergeRelationship:
	}
	for _, c := range plan.Children() {
		d.addPlanWrites(c)
	}
}

// mutatorIndexDelta returns the index-staleness summary the plan build for plan
// must respect: the changes the enclosing transaction has already buffered
// (rmp #2814's cross-statement half) UNION the coordinates this statement's own
// write clauses may touch (its same-statement half). It returns nil — nothing
// pending — for a read-only or unrecognised mutator, and for a write statement
// with neither an unflushed change nor a node write.
//
// It reads the buffer and the graph off the mutator the caller already holds,
// exactly as [setMutatorWriteTx], [mutatorBuildScratch] and [mutatorCounters] do,
// so no call site grows a parameter and the PUBLIC [BuildPlanWithMutator] path —
// which passes an all-false planGates and has no engine behind it — is guarded by
// the same code as the engine's own write path.
//
// Returning nil for an unrecognised mutator is right rather than merely
// convenient: the only other mutators are the read-only test stubs, which enqueue
// nothing and never reach a write operator, so there is nothing to declare.
func mutatorIndexDelta(m exec.GraphMutator, plan ir.LogicalPlan) *pendingIndexDelta {
	var buf *exec.IndexBuffer
	var g *lpg.Graph[string, float64]
	switch a := m.(type) {
	case *lpgMutatorAdapter:
		buf, g = a.buf, a.g
	case *walMutatorAdapter:
		buf, g = a.buf, a.g
	default:
		return nil
	}
	// No buffer means no index fan-out is wired for this mutator at all, so no
	// index can be made stale by it and the statement's own writes are irrelevant
	// too. This is the read-only adapter instance.
	if buf == nil || g == nil {
		return nil
	}
	// NO INDEX, NO QUESTION. With nothing registered, every funnel this delta
	// feeds ([tryNamedHashSeek], [tryAnyHashSeek], [buildSeekSetOperator],
	// [findBoundStringBTree], [findBoundNumericBTree]) fails to find a covering
	// index anyway, so the answer cannot change the plan — and building it would
	// be pure waste on the write hot path.
	//
	// It is not a micro-optimisation, it is the difference between a cost and no
	// cost at all for an index-free workload. Measured on
	// BenchmarkMergeMatch_LabelsOnly/hot=8/cold=0 (which registers NO index):
	// without this gate the guard cost +4 allocs/op and +72 B/op on every write
	// statement — one for the delta itself and three for the [ir.LogicalPlan.Children]
	// slices the footprint walk allocates on its way down to the write node — for
	// an answer that was always "nothing is pending".
	//
	// Count is one RLock over a map length, the same probe [indexFanoutActive]
	// uses, and registration happens under the DDL exclusion the calling
	// transaction already holds, so the count cannot change mid-statement.
	if mgr := g.IndexManager(); mgr == nil || mgr.Count() == 0 {
		return nil
	}
	d := newPendingIndexDelta(buf, g)
	if d == nil {
		d = &pendingIndexDelta{}
	}
	d.addPlanWrites(plan)
	if !d.all && d.props == nil && d.labels == nil {
		return nil
	}
	return d
}

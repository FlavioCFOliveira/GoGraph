package sim

// counters_oracle.go — per-op QueryCounters oracle (rmp #2448).
//
// Every committed write the deterministic tick loop executes already has its
// effect modelled by the GraphOracle (applyToOracle), yet until #2448 the
// engine's own per-statement effect report — cypher.Result.Counters(), the
// twelve write-effect counters of exec.QueryCounters — was never read by the
// DST. That left a free, high-yield invariant unused: a write that applies the
// right effect but REPORTS the wrong counters (or whose counters drift from the
// actual effect) was invisible to every existing checker, because the parity
// checks compare graph state, not the effect report.
//
// The check here closes that gap as a pure function of (op, reported counters,
// oracle state BEFORE the op is applied to the oracle): for each workload
// template the oracle can adjudicate exactly — did the MATCH find the node, did
// MERGE create or match, how many edges were incident to the deleted node — so
// the expected counter set is derived from the model, never from the engine.
// It draws no randomness and issues no extra engine query (the counters are
// read from the SAME drained result the tick executed), so a clean run stays
// byte-identical to one without the check.
//
// A template the oracle does not model exactly is skipped rather than asserted
// vacuously — the four wired scenarios (default, schema-mutation, merge-rel,
// edge-properties) emit only modelled templates, so nothing they run escapes
// the check.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// counterReporter is the optional facet of [Result] that surfaces the engine's
// per-statement write-effect counters ([exec.QueryCounters]). The
// [EngineAdapter]'s result wrapper implements it; a Result that does not is
// reported as carrying no counters.
type counterReporter interface {
	// Counters returns the per-statement write-effect counters: nil for a
	// read-only statement, the applied effect set for a write. The returned
	// pointer is owned by the result and must be treated as read-only.
	Counters() *exec.QueryCounters
}

// CheckOpCounters compares the engine-reported per-statement write-effect
// counters of one executed operation against the effect the [GraphOracle]
// predicts for it, and returns a [ViolationOracleDeviation] per disagreement.
// It must be called AFTER the op executed (got is read from the drained
// result) and BEFORE [Simulator.applyToOracle] advances the oracle, because
// the expected effect is a function of the pre-op model (e.g. whether a MERGE
// matches, or how many edges a DETACH DELETE takes with it).
//
// The rules, in order:
//
//   - An op the engine did not commit is skipped. When the statement failed
//     before producing a result there are no counters at all; when it produced
//     a result whose drain failed, the statement was rolled back and the
//     engine contract deliberately does not pin that result's own counters
//     (rollback restores the graph, not the report — what IS pinned, by
//     cypher's TestQueryCounters_RolledBackStatementReportsNothing, is that no
//     effect leaks into the NEXT statement's counters). The oracle stays
//     frozen for the op, so state parity is still enforced by the per-tick
//     checker.
//   - A pure read ([OpMatch]) must report NIL counters — the engine contract
//     distinguishes "no write surface" (nil) from "wrote nothing" (all-zero),
//     so a read that reports counters is a deviation.
//   - A committed write of a modelled template must report NON-nil counters
//     that equal the derived expectation on every one of the twelve fields.
//     A committed write of an unmodelled template is skipped.
//
// The check is a pure function of its arguments: it draws no randomness,
// issues no engine query, and its per-op cost is O(1) except for DETACH
// DELETE, whose incident-edge count is the same O(edges) scan the oracle's own
// ApplyDelete already pays on the same tick.
func CheckOpCounters(tick int64, op Op, committed bool, got *exec.QueryCounters, oracle *GraphOracle) []Violation {
	if !committed {
		return nil
	}
	if op.Kind == OpMatch {
		if got != nil {
			return []Violation{{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "query counters",
				Message: fmt.Sprintf("read-only op %q reported non-nil counters %+v; a statement with no write surface must report nil", op.Cypher, *got),
			}}
		}
		return nil
	}
	if !op.Kind.IsWrite() {
		return nil
	}
	want, exact := expectedOpCounters(op, oracle)
	if !exact {
		return nil
	}
	if got == nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "query counters",
			Message: fmt.Sprintf("committed write %q reported nil counters; a write statement must report its (possibly all-zero) effect set", op.Cypher),
		}}
	}
	diff := diffCounters(&want, got)
	if diff == "" {
		return nil
	}
	return []Violation{{
		Kind: ViolationOracleDeviation, Tick: tick, Op: "query counters",
		Message: fmt.Sprintf("op %q counters disagree with the oracle's expected effect: %s", op.Cypher, diff),
	}}
}

// diffCounters renders every field on which want and got disagree as
// "Field: want=W got=G" clauses joined by "; ", or "" when they are equal. All
// twelve fields are compared so a schema-effect leak (indexes/constraints) on a
// data statement is caught too.
func diffCounters(want, got *exec.QueryCounters) string {
	fields := [...]struct {
		name      string
		want, got int64
	}{
		{"NodesCreated", want.NodesCreated, got.NodesCreated},
		{"NodesDeleted", want.NodesDeleted, got.NodesDeleted},
		{"RelationshipsCreated", want.RelationshipsCreated, got.RelationshipsCreated},
		{"RelationshipsDeleted", want.RelationshipsDeleted, got.RelationshipsDeleted},
		{"PropertiesSet", want.PropertiesSet, got.PropertiesSet},
		{"PropertiesRemoved", want.PropertiesRemoved, got.PropertiesRemoved},
		{"LabelsAdded", want.LabelsAdded, got.LabelsAdded},
		{"LabelsRemoved", want.LabelsRemoved, got.LabelsRemoved},
		{"IndexesAdded", want.IndexesAdded, got.IndexesAdded},
		{"IndexesRemoved", want.IndexesRemoved, got.IndexesRemoved},
		{"ConstraintsAdded", want.ConstraintsAdded, got.ConstraintsAdded},
		{"ConstraintsRemoved", want.ConstraintsRemoved, got.ConstraintsRemoved},
	}
	var out string
	for _, f := range fields {
		if f.want == f.got {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += fmt.Sprintf("%s: want=%d got=%d", f.name, f.want, f.got)
	}
	return out
}

// expectedOpCounters derives the exact [exec.QueryCounters] a committed op must
// report, from the oracle state BEFORE the op is applied to the model. ok is
// false when op's template is not modelled exactly (the caller skips it rather
// than asserting vacuously).
//
// The numbers follow the engine's pinned counter semantics
// (cypher/query_counters_test.go, the openCypher TCK side-effect vocabulary):
// counters record effects ACTUALLY APPLIED — a duplicate simple-graph edge
// CREATE is not an addition, adding a label the node carries counts nothing,
// removing an absent property counts nothing — while every property ASSIGNMENT
// applied counts as +properties even when the value is unchanged, and a
// whole-entity replace (SET n = {…}) clears every present property (each one
// -properties) before writing the map's entries (each one +properties). A
// DELETE reports only -nodes/-relationships: the deleted node's label and
// property teardown is suppressed by the engine as not user-visible.
func expectedOpCounters(op Op, oracle *GraphOracle) (want exec.QueryCounters, ok bool) {
	switch op.Cypher {
	case tmplCreatePerson:
		// CREATE (n:Person {name, age}): one node, one label, two properties.
		// Under an active UNIQUE(Person.name) a duplicate is REJECTED by the
		// engine (committed == false), so it never reaches this derivation.
		return exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2}, true

	case tmplCreateKnows:
		a, okA := paramString(op.Params, "a")
		b, okB := paramString(op.Params, "b")
		if !okA || !okB {
			return exec.QueryCounters{}, false
		}
		srcID, srcOK := oracle.byName[a]
		dstID, dstOK := oracle.byName[b]
		if !srcOK || !dstOK {
			// MATCH found nothing: CREATE ran zero times.
			return exec.QueryCounters{}, true
		}
		if oracle.HasEdge(srcID, dstID, "KNOWS") {
			// The engine REJECTS a duplicate parallel-edge CREATE on the sim's
			// simple graph (typed error, committed == false), so a committed op
			// never reaches this branch today; were one ever to commit, the only
			// sound effect for a graph that cannot gain a parallel edge is zero.
			return exec.QueryCounters{}, true
		}
		return exec.QueryCounters{RelationshipsCreated: 1}, true

	case tmplSetAge:
		return expectSetProps(op, oracle, 1)

	case tmplDetachDelete:
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		id, found := oracle.byName[name]
		if !found {
			return exec.QueryCounters{}, true
		}
		// -nodes 1 and one -relationships per incident edge; the node's label
		// and property teardown is suppressed (not a user-visible effect).
		return exec.QueryCounters{NodesDeleted: 1, RelationshipsDeleted: oracle.incidentEdges(id)}, true

	case tmplMergePerson:
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		if _, exists := oracle.byName[name]; exists {
			// MERGE matched: nothing was applied.
			return exec.QueryCounters{}, true
		}
		// MERGE created: one node, one label, the pattern's name property plus
		// ON CREATE SET n.created — two properties.
		return exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2}, true

	case tmplMergeKnowsN:
		a, okA := paramString(op.Params, "a")
		b, okB := paramString(op.Params, "b")
		if !okA || !okB {
			return exec.QueryCounters{}, false
		}
		srcID, srcOK := oracle.byName[a]
		dstID, dstOK := oracle.byName[b]
		if !srcOK || !dstOK {
			return exec.QueryCounters{}, true // MATCH found nothing; MERGE ran zero times.
		}
		if oracle.HasEdge(srcID, dstID, "KNOWS") {
			// MERGE matched: ON MATCH SET r.n = r.n+1 applies one assignment.
			return exec.QueryCounters{PropertiesSet: 1}, true
		}
		// MERGE created: the edge plus ON CREATE SET r.n = 1.
		return exec.QueryCounters{RelationshipsCreated: 1, PropertiesSet: 1}, true

	case tmplCreateKnowsInst, tmplSetKnowsWeight, tmplRemoveKnowsSince, tmplDeleteKnowsInst:
		// The eid-pinned relationship-write templates of the edge-properties
		// scenario (rmp #2449) are derived by a dedicated helper.
		return expectedKnowsInstCounters(op, oracle)

	case tmplSetTag:
		return expectSetProps(op, oracle, 1)

	case tmplRemoveTag:
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		id, found := oracle.byName[name]
		if !found {
			return exec.QueryCounters{}, true
		}
		if _, has := oracle.nodes[id].Properties["tag"]; has {
			return exec.QueryCounters{PropertiesRemoved: 1}, true
		}
		return exec.QueryCounters{}, true // removing an absent property counts nothing.

	case tmplAddVip:
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		id, found := oracle.byName[name]
		if found && !hasLabel(oracle.nodes[id], "Vip") {
			return exec.QueryCounters{LabelsAdded: 1}, true
		}
		return exec.QueryCounters{}, true // absent node, or label already carried.

	case tmplRemoveVip:
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		id, found := oracle.byName[name]
		if found && hasLabel(oracle.nodes[id], "Vip") {
			return exec.QueryCounters{LabelsRemoved: 1}, true
		}
		return exec.QueryCounters{}, true // absent node, or label not carried.

	case tmplMergeProps:
		// SET n += $props applies one assignment per non-null map entry.
		props, okP := op.Params["props"].(map[string]any)
		if !okP {
			return exec.QueryCounters{}, false
		}
		return expectSetProps(op, oracle, nonNilEntries(props))

	case tmplReplaceProps:
		// SET n = $props clears every property the node carries (each one
		// -properties) and then writes the map's entries (each one +properties),
		// re-set keys included on both sides.
		props, okP := op.Params["props"].(map[string]any)
		if !okP {
			return exec.QueryCounters{}, false
		}
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		id, found := oracle.byName[name]
		if !found {
			return exec.QueryCounters{}, true
		}
		return exec.QueryCounters{
			PropertiesRemoved: int64(len(oracle.nodes[id].Properties)),
			PropertiesSet:     nonNilEntries(props),
		}, true

	default:
		return exec.QueryCounters{}, false
	}
}

// expectedKnowsInstCounters derives the exact effect set for the eid-pinned
// relationship-write templates of the edge-properties scenario (rmp #2449),
// from the pre-apply model:
//
//   - [tmplCreateKnowsInst]: one relationship and three property assignments
//     per created instance — including a PARALLEL instance between an
//     already-linked pair (the scenario runs on a multigraph). A missing
//     endpoint means the MATCH found nothing and CREATE ran zero times.
//   - [tmplSetKnowsWeight]: exactly one assignment when the pinned instance
//     exists (an assignment counts even when the value is unchanged), zero
//     rows otherwise.
//   - [tmplRemoveKnowsSince]: SKIPPED (ok == false) because of a KNOWN ENGINE
//     DEFECT this oracle found: the exact expectation is one -properties when
//     the pinned instance still carries `since`, but the engine gates
//     PropertiesRemoved on the PER-PAIR aggregate presence probe
//     (lpgMutatorAdapter.DelEdgeProperty, cypher/api.go) while the mutation
//     itself is correctly per-handle (DelEdgePropertyByHandle counts
//     nothing). On parallel edges only the first REMOVE per (src,dst) pair
//     reports -properties 1; a later removal of a genuinely present property
//     on a sibling instance reports 0. The per-instance STATE effect — right
//     instance stripped, sibling untouched — stays fully guarded by
//     [CheckEdgeProperties]. TestEdgeProperties_KnownDefect_RemoveCountersPerPair
//     is the canary: it fails the moment the engine starts counting
//     per-instance, which is the cue to restore the exact expectation here.
//   - [tmplDeleteKnowsInst]: exactly one deleted relationship and ZERO deleted
//     nodes — a standalone edge deletion must never cascade to its endpoints,
//     and the deleted edge's property teardown is suppressed as not
//     user-visible.
func expectedKnowsInstCounters(op Op, oracle *GraphOracle) (want exec.QueryCounters, ok bool) {
	if op.Cypher == tmplRemoveKnowsSince {
		return exec.QueryCounters{}, false // known engine defect; see above.
	}
	k, okP, found := oracle.knowsInstParams(op.Params)
	if !okP {
		return exec.QueryCounters{}, false
	}
	switch op.Cypher {
	case tmplCreateKnowsInst:
		if !found {
			return exec.QueryCounters{}, true
		}
		return exec.QueryCounters{RelationshipsCreated: 1, PropertiesSet: 3}, true
	case tmplSetKnowsWeight:
		if found {
			if _, exists := oracle.edges[k]; exists {
				return exec.QueryCounters{PropertiesSet: 1}, true
			}
		}
		return exec.QueryCounters{}, true
	case tmplDeleteKnowsInst:
		if found {
			if _, exists := oracle.edges[k]; exists {
				return exec.QueryCounters{RelationshipsDeleted: 1, NodesDeleted: 0}, true
			}
		}
		return exec.QueryCounters{}, true
	default:
		return exec.QueryCounters{}, false
	}
}

// expectSetProps is the shared expectation for the MATCH-by-name SET templates:
// when the name resolves, the statement applies perHit property assignments;
// when it does not, the MATCH found nothing and the statement applies nothing.
func expectSetProps(op Op, oracle *GraphOracle, perHit int64) (exec.QueryCounters, bool) {
	name, okN := paramString(op.Params, "name")
	if !okN {
		return exec.QueryCounters{}, false
	}
	if _, found := oracle.byName[name]; !found {
		return exec.QueryCounters{}, true
	}
	return exec.QueryCounters{PropertiesSet: perHit}, true
}

// nonNilEntries counts the non-nil values of a bound property map: a nil map
// value is openCypher's "SET with null" key removal, not an assignment. The
// workloads never bind nil map entries today, so this is a guard, not a path.
func nonNilEntries(props map[string]any) int64 {
	var n int64
	for _, v := range props {
		if v != nil {
			n++
		}
	}
	return n
}

// incidentEdges returns how many modelled edges touch node id (in, out, or
// self-loop — a self-loop is one edge and counts once), which is exactly the
// -relationships effect a DETACH DELETE of that node must report.
func (o *GraphOracle) incidentEdges(id uint64) int64 {
	var n int64
	for k := range o.edges {
		if k.src == id || k.dst == id {
			n++
		}
	}
	return n
}

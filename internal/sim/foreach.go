package sim

// foreach.go — FOREACH write-path equivalence (rmp #2454).
//
// FOREACH (x IN list | body) is fully implemented in the engine
// (cypher/exec/foreach.go, covered by cypher/foreach_test.go) but was never
// driven by the DST. The two templates here add it to the schema-mutation
// scenario with a STRUCTURAL equivalence oracle: the GraphOracle models each
// FOREACH statement as its EXPANSION — the state transition of the equivalent
// batch of per-item single statements — and the scenario's existing read-back
// machinery adjudicates the engine against that expansion:
//
//   - the per-tick invariant checker compares node/edge counts and sampled
//     existence, so an engine that ran the body a different number of times
//     diverges from the expansion's population;
//   - [CheckSchemaMutation] reads every modelled Person back BY NAME and
//     compares each scalar property and label, so each per-item CREATE and the
//     last-wins SET are verified element by element, including immediately
//     after crash/recovery (the FOREACH-written state must survive the WAL);
//   - the per-op counters oracle ([CheckOpCounters], rmp #2448) pins the
//     statement's reported effect set exactly: a FOREACH CREATE over an N-item
//     list must report N nodes / N labels / N properties, and a FOREACH SET
//     over a K-item list must report K assignments — the engine's own effect
//     report is held to the expansion, not merely to "something happened".
//
// This is a real equivalence guarantee without a second engine: the expansion
// IS the reference semantics (openCypher defines FOREACH as running the body
// once per list element), the oracle applies it independently of the engine,
// and state + counters are both compared against it. Running the equivalent
// UNWIND…CREATE against a second primed engine would re-verify the same two
// surfaces at roughly double the runtime, adding discrimination only for a
// defect that corrupts both engines identically — which a shared-engine
// differential cannot exclude either.
//
// Engine contracts pinned by cypher/foreach_test.go and re-verified here:
// FOREACH over [] runs the body zero times and is a committed no-op
// (TestForeach_EmptyList), and a FOREACH SET on an outer MATCH variable
// assigns once per element (TestForeach_Set_OuterVar), so the final value is
// the list's last element.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// FOREACH templates (rmp #2454). Like every workload template they are shared
// constants so the actors and the oracle cannot drift apart.
const (
	// tmplForeachCreatePersons creates one Person per element of the bound
	// $names list — the FOREACH expansion of a batch of single CREATEs. The
	// created Persons carry only the name key (no age), which the schema-mutation
	// checker reads back as NULL for the other scalar keys, exactly as modelled.
	tmplForeachCreatePersons = "FOREACH (x IN $names | CREATE (:Person {name: x}))"
	// tmplForeachSetTag matches one Person by name and assigns n.tag once per
	// element of the bound $tags list (SET on an outer variable inside the
	// FOREACH body). Each assignment counts in the effect report; the final
	// stored value is the LAST element.
	tmplForeachSetTag = "MATCH (n:Person {name:$name}) FOREACH (x IN $tags | SET n.tag = x)"
)

// opForeachCreate builds a [tmplForeachCreatePersons] op over a seed-chosen
// small list (1..4 names). Names are drawn through [HonestWriter.uniqueName]
// and re-drawn on a within-list duplicate, so the expansion never creates two
// Persons with the same logical key in one statement (the redraw loop is a
// pure function of the seed stream, preserving bit-reproducibility).
func (SchemaMutationWriter) opForeachCreate(seed *Seed) Op {
	n := 1 + seed.IntN(4)
	names := make([]any, 0, n)
	used := make(map[string]bool, n)
	for len(names) < n {
		name := HonestWriter{}.uniqueName(seed)
		if used[name] {
			continue
		}
		used[name] = true
		names = append(names, name)
	}
	return Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons, Params: map[string]any{"names": names}}
}

// opForeachSetTag builds a [tmplForeachSetTag] op against a seed-chosen
// existing Person with a seed-chosen small list (1..4 tags).
func (SchemaMutationWriter) opForeachSetTag(seed *Seed, names []string) Op {
	name := names[seed.IntN(len(names))]
	k := 1 + seed.IntN(4)
	tags := make([]any, 0, k)
	for i := 0; i < k; i++ {
		tags = append(tags, fmt.Sprintf("t%d", seed.IntN(1000)))
	}
	return Op{Kind: OpUpdate, Cypher: tmplForeachSetTag, Params: map[string]any{"name": name, "tags": tags}}
}

// applyForeachCreatePersons advances the model for [tmplForeachCreatePersons]
// by applying its expansion: one Person node per list element, carrying only
// the name property — exactly the state transition of the equivalent batch of
// single CREATEs. An empty list is a committed zero-effect statement (the
// engine contract pinned by TestForeach_EmptyList). The template is emitted
// only by constraint-free workloads, so no UNIQUE rejection is modelled here.
func (o *GraphOracle) applyForeachCreatePersons(params map[string]any) OracleResult {
	names, ok := params["names"].([]any)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: foreach create missing/!list names"}
	}
	created := 0
	for _, v := range names {
		name, ok := v.(string)
		if !ok {
			return OracleResult{ErrorMsg: "oracle: foreach create non-string name"}
		}
		id := o.nextNodeID
		o.nextNodeID++
		o.nodes[id] = &NodeState{ID: id, Labels: []string{"Person"}, Properties: map[string]any{"name": name}}
		o.byName[name] = id
		created++
	}
	return OracleResult{Committed: true, NodesCreated: created}
}

// applyForeachSetTag advances the model for [tmplForeachSetTag] by applying
// its expansion: len(tags) successive assignments of n.tag, whose net state
// effect is the LAST element. A name miss (MATCH found nothing) and an empty
// list (body ran zero times) are committed zero-effect statements.
func (o *GraphOracle) applyForeachSetTag(params map[string]any) OracleResult {
	name, okN := paramString(params, "name")
	tags, okT := params["tags"].([]any)
	if !okN || !okT {
		return OracleResult{ErrorMsg: "oracle: foreach set missing name/tags"}
	}
	if id, found := o.byName[name]; found && len(tags) > 0 {
		o.nodes[id].Properties["tag"] = tags[len(tags)-1]
	}
	return OracleResult{Committed: true}
}

// expectedForeachCounters derives the exact effect set a committed FOREACH
// template must report, per the engine's pinned counter semantics (every
// assignment applied counts, effects actually applied only):
//
//   - [tmplForeachCreatePersons]: the body ran once per list element, so N
//     elements report N nodes, N labels, and N name-property assignments. An
//     empty list reports the all-zero (still non-nil) effect set.
//   - [tmplForeachSetTag]: K elements report K assignments when the name
//     resolves (same-value re-assignments included); a name miss means the
//     MATCH found nothing and the whole statement applied nothing.
func expectedForeachCounters(op Op, oracle *GraphOracle) (exec.QueryCounters, bool) {
	switch op.Cypher {
	case tmplForeachCreatePersons:
		names, ok := op.Params["names"].([]any)
		if !ok {
			return exec.QueryCounters{}, false
		}
		n := int64(len(names))
		return exec.QueryCounters{NodesCreated: n, LabelsAdded: n, PropertiesSet: n}, true
	case tmplForeachSetTag:
		tags, okT := op.Params["tags"].([]any)
		if !okT {
			return exec.QueryCounters{}, false
		}
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		if _, found := oracle.byName[name]; !found {
			return exec.QueryCounters{}, true
		}
		return exec.QueryCounters{PropertiesSet: int64(len(tags))}, true
	default:
		return exec.QueryCounters{}, false
	}
}

// foreachStats is the schema-mutation run's assert-something-was-seen record
// for the FOREACH templates: how often each was issued, which FOREACH-created
// names were committed, and whether the crash/recovery machinery demonstrably
// exercised FOREACH-written state.
type foreachStats struct {
	// createdNames records every name a committed [tmplForeachCreatePersons]
	// wrote, so the post-crash hook can prove a FOREACH-created Person was
	// still modelled — and therefore probed by the post-recovery
	// [CheckSchemaMutation] — after at least one recovery.
	createdNames map[string]bool
	// createIssued / setIssued count how many ops of each FOREACH template the
	// workload issued.
	createIssued int
	setIssued    int
	// crashAfterForeach reports that at least one crash/recovery happened after
	// a FOREACH op had already committed.
	crashAfterForeach bool
	// survivorChecked reports that, at some post-recovery check, at least one
	// FOREACH-created Person was still modelled — so the durability probe ran
	// on FOREACH-written data at least once.
	survivorChecked bool
}

// newForeachStats returns an empty stats record.
func newForeachStats() *foreachStats {
	return &foreachStats{createdNames: make(map[string]bool)}
}

// noteOp records one executed op. It must be called for every tick's op so the
// issued counts and the committed-create name set stay complete.
func (fs *foreachStats) noteOp(op Op, committed bool) {
	switch op.Cypher {
	case tmplForeachCreatePersons:
		fs.createIssued++
		if committed {
			if names, ok := op.Params["names"].([]any); ok {
				for _, v := range names {
					if s, ok := v.(string); ok {
						fs.createdNames[s] = true
					}
				}
			}
		}
	case tmplForeachSetTag:
		fs.setIssued++
	}
}

// noteRecovery records one crash/recovery observed AFTER the tick loop already
// executed ops, marking whether any FOREACH op preceded it and whether a
// FOREACH-created Person survived into the post-recovery model.
func (fs *foreachStats) noteRecovery(oracle *GraphOracle) {
	if fs.createIssued == 0 && fs.setIssued == 0 {
		return
	}
	fs.crashAfterForeach = true
	for name := range fs.createdNames {
		if oracle.HasPersonName(name) {
			fs.survivorChecked = true
			return
		}
	}
}

// checkForeachNonVacuity is the terminal assert-something-was-seen gate of the
// FOREACH coverage (rmp #2454): a clean schema-mutation run must have issued
// BOTH FOREACH templates, crashed at least once after a FOREACH op, and run a
// post-recovery check while a FOREACH-created Person was still modelled — so a
// green run is genuine evidence that FOREACH-written state was exercised
// through crash/recovery, not a run in which the new templates never fired.
func checkForeachNonVacuity(tick int64, fs *foreachStats) []Violation {
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "foreach non-vacuity", Message: msg})
	}
	if fs.createIssued == 0 {
		fail("no FOREACH…CREATE op was issued: the FOREACH create arm was vacuous")
	}
	if fs.setIssued == 0 {
		fail("no FOREACH…SET op was issued: the FOREACH set arm was vacuous")
	}
	if !fs.crashAfterForeach {
		fail("no crash/recovery happened after a FOREACH op: FOREACH-written state was never exercised through recovery")
	}
	if !fs.survivorChecked {
		fail("no post-recovery check saw a surviving FOREACH-created Person: the durability probe never ran on FOREACH-written data")
	}
	return vs
}

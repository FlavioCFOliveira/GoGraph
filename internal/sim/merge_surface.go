package sim

// merge_surface.go — MERGE surface completeness (rmp #2461).
//
// Before this file the DST drove exactly two MERGE shapes: a node MERGE with an
// ON CREATE branch only ([tmplMergePerson]) and a relationship MERGE with both
// branches ([tmplMergeKnowsN], rmp #2449). Four families of the clause were
// therefore never executed against an oracle, and each of them carries
// semantics that no other template can expose:
//
//   - a node MERGE with BOTH branches, where the ON MATCH branch reads the
//     merged node's OWN property ([tmplMergePersonCounter]);
//   - the whole-entity ON CREATE assignment ([tmplMergePersonSetAll]), whose
//     replace semantics reach back over the property the merge pattern itself
//     just wrote;
//   - a whole-pattern MERGE that must create both endpoints AND the
//     relationship in one clause ([tmplMergePairPattern]);
//   - a MERGE with a bare map parameter ([tmplMergeParamMap]), which the
//     openCypher TCK requires the engine to REJECT.
//
// # Engine semantics pinned here
//
// Every number below was MEASURED against the real engine, not inferred from
// the specification, and each is asserted continuously by the counters oracle
// (rmp #2448) and by the scenario's read-back parity checks:
//
//  1. ON CREATE SET n = $map REPLACES: the clause writes the pattern property
//     (name) first, then CLEARS every property the new node carries — the merge
//     key included — and only then writes the map's entries. The effect report
//     is therefore 1+len(map) properties SET and exactly 1 property REMOVED, and
//     a map that omits `name` leaves a NAMELESS node behind, which makes the
//     statement NON-IDEMPOTENT (the next MERGE cannot match what it created and
//     creates a second node). The workload always binds `name` into the map for
//     that reason — the same convention [tmplReplaceProps] already follows —
//     and TestMergeSurface_SetAllReplacesMergeKey pins the nameless variant
//     directly, so the destructive behaviour is recorded without letting it
//     strand a node the durability probe reads by name.
//
//  2. Whole-pattern MERGE is ALL-OR-NOTHING and never reuses an unbound
//     endpoint. Either the WHOLE pattern matches (a committed all-zero
//     statement) or the WHOLE pattern is created — two FRESH nodes and one
//     relationship — even when a node with the same label and key property
//     already exists. Measured across all four sub-cases: with `a` already
//     present, with both present, and with an endpoint born from a plain
//     CREATE, the statement still reports 2 nodes / 1 relationship / 2
//     properties / 2 labels and leaves DUPLICATE key values behind. This is
//     openCypher-correct (the pattern, not its parts, is the unit of MERGE) and
//     is exactly why the family runs in its own key namespace — see
//     [mergePairKeys].
//
//  3. ON MATCH SET n.mc = n.mc + 1 over a node with NO `mc` evaluates
//     null + 1 = null, and an assignment of null REMOVES the property. Removing
//     an absent property is an openCypher no-op, so the statement reports the
//     ALL-ZERO effect set and changes nothing. The co-actor's
//     [tmplReplaceProps] can wipe `mc` off a counter Person at any tick, so this
//     is a live path in the workload, not a hypothetical one, and modelling it
//     exactly is what keeps the counters oracle honest rather than skipped.
//
//  4. MERGE (n $map) is REJECTED at compile time. The engine raises a scope
//     error before any mutation, which the openCypher TCK requires
//     (cypher/tck/features/clauses/merge/Merge1.feature scenario [16],
//     InvalidParameterUse) and cypher/create_param_map_test.go
//     TestMergeParamMap_StillRejected pins. The family is therefore modelled as
//     an [OpMalformed] no-op and adjudicated by [checkMergeRejection], which
//     fails if the engine ever ACCEPTS it — the negative-space coverage that
//     makes a silent relaxation of the rule visible to the DST.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// MERGE-surface templates (rmp #2461). Like every workload template they are
// shared constants so the actors and the oracle cannot drift apart.
const (
	// tmplMergePersonCounter merges a Person by name with BOTH action branches,
	// initialising a hit counter n.mc to 1 on creation and incrementing it on
	// every subsequent match — the node analogue of [tmplMergeKnowsN]. The ON
	// MATCH right-hand side reads the merged node's own stored property, so it
	// also exercises the per-row action evaluator.
	tmplMergePersonCounter = "MERGE (n:Person {name:$name}) ON CREATE SET n.mc = 1 ON MATCH SET n.mc = n.mc + 1"
	// tmplMergePersonSetAll merges a Person by name and, on creation only,
	// REPLACES its whole property set with a bound map. The map always carries
	// `name` so the created node stays matchable by the key the pattern merged
	// on (the replace would otherwise destroy it — see the file comment).
	tmplMergePersonSetAll = "MERGE (n:Person {name:$name}) ON CREATE SET n = $map"
	// tmplMergePairPattern merges a WHOLE pattern: two Person endpoints and the
	// PAIRED relationship between them. Neither endpoint is bound by an earlier
	// clause, so the engine matches or creates the pattern as a unit.
	tmplMergePairPattern = "MERGE (a:Person {name:$a})-[r:PAIRED]->(b:Person {name:$b})"
	// tmplMergeParamMap is the map-parameter MERGE the engine must REJECT at
	// compile time (openCypher TCK Merge1 scenario [16]). It is emitted as an
	// [OpMalformed] op and must never commit.
	tmplMergeParamMap = "MERGE (n $map)"
)

// relPaired is the relationship type of the whole-pattern MERGE family. It is
// deliberately distinct from KNOWS so no KNOWS-keyed helper or checker can see
// these edges, while the shared edge-parity probes — which interpolate the
// modelled label into MATCH (a:Person {name})-[r:LABEL]->(b:Person {name}) —
// still reach them, because the endpoints carry the Person label and a name.
const relPaired = "PAIRED"

// mergePairKeys is the closed key namespace of the whole-pattern MERGE family.
//
// It is small (so repeated draws drive the pattern through all four
// match/create sub-cases within a run) and DISJOINT from every other name the
// workload binds: [HonestWriter.uniqueName] always produces "<FirstName>-<n>",
// so no other actor can ever draw one of these keys. That disjointness is
// load-bearing rather than cosmetic. The family legitimately creates DUPLICATE
// Person nodes for the same key (see semantics 2 in the file comment), and the
// oracle's name index — like the per-name property, label, and durability
// probes built on it — assumes one node per name. Keeping the family's nodes
// out of that index ([GraphOracle.applyMergePairPattern] never writes byName)
// and out of every other actor's reach is what lets the duplicates exist
// exactly as the engine creates them while node- and edge-count parity, and the
// endpoint-name edge probes, all keep working unchanged.
var mergePairKeys = [...]string{"wp0", "wp1", "wp2", "wp3", "wp4", "wp5"}

// opMergeCounter builds a [tmplMergePersonCounter] op. One draw in two targets
// an EXISTING name (driving the ON MATCH branch) and the rest a fresh unique
// name (driving ON CREATE), so both branches fire over a run. names must be the
// oracle's current name list; an empty list forces the create branch.
func (SchemaMutationWriter) opMergeCounter(seed *Seed, names []string) Op {
	name := HonestWriter{}.uniqueName(seed)
	if len(names) > 0 && seed.IntN(2) == 0 {
		name = names[seed.IntN(len(names))]
	}
	return Op{Kind: OpMerge, Cypher: tmplMergePersonCounter, Params: map[string]any{"name": name}}
}

// opMergeSetAll builds a [tmplMergePersonSetAll] op. As with the counter arm,
// one draw in two targets an existing name so the ON-CREATE-does-not-fire path
// is exercised too. The bound map always carries `name` (the merge key the
// replace would otherwise destroy) plus an age, mirroring the convention
// [tmplReplaceProps] established.
func (SchemaMutationWriter) opMergeSetAll(seed *Seed, names []string) Op {
	name := HonestWriter{}.uniqueName(seed)
	if len(names) > 0 && seed.IntN(2) == 0 {
		name = names[seed.IntN(len(names))]
	}
	return Op{Kind: OpMerge, Cypher: tmplMergePersonSetAll, Params: map[string]any{
		"name": name,
		"map":  map[string]any{"name": name, "age": int64(seed.IntN(100))},
	}}
}

// opMergePairPattern builds a [tmplMergePairPattern] op over two DISTINCT keys
// of [mergePairKeys]. The redraw loop is a pure function of the seed stream, so
// reproducibility is preserved. Distinctness keeps the pattern a two-node one:
// binding the same key to both positions would still be well defined, but the
// four-sub-case adjudication of [mergePatternCase] would no longer have a
// single unambiguous reading.
func (SchemaMutationWriter) opMergePairPattern(seed *Seed) Op {
	a := mergePairKeys[seed.IntN(len(mergePairKeys))]
	b := a
	for b == a {
		b = mergePairKeys[seed.IntN(len(mergePairKeys))]
	}
	return Op{Kind: OpMerge, Cypher: tmplMergePairPattern, Params: map[string]any{"a": a, "b": b}}
}

// opMergeParamMap builds the [tmplMergeParamMap] op the engine must reject. It
// is an [OpMalformed] op: the oracle records it as an expected-error no-op and
// [checkMergeRejection] asserts the engine never accepted it.
func (SchemaMutationWriter) opMergeParamMap(seed *Seed) Op {
	return Op{Kind: OpMalformed, Cypher: tmplMergeParamMap, Params: map[string]any{
		"map": map[string]any{"name": HonestWriter{}.uniqueName(seed), "age": int64(seed.IntN(100))},
	}}
}

// applyMergePersonCounter advances the model for [tmplMergePersonCounter]:
// a missing name creates the Person with mc = 1 (ON CREATE), a present one
// increments its mc (ON MATCH). When the matched Person carries NO integer mc —
// the co-actor's whole-map replace can wipe it — the right-hand side evaluates
// to null and the assignment REMOVES an absent property, which is a measured
// no-op, so the model leaves the node untouched.
func (o *GraphOracle) applyMergePersonCounter(params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: merge counter missing name"}
	}
	id, found := o.byName[name]
	if !found {
		id = o.nextNodeID
		o.nextNodeID++
		o.nodes[id] = &NodeState{
			ID:         id,
			Labels:     []string{"Person"},
			Properties: map[string]any{"name": name, "mc": int64(1)},
		}
		o.byName[name] = id
		return OracleResult{Committed: true, NodesCreated: 1}
	}
	if mc, isInt := o.nodes[id].Properties["mc"].(int64); isInt {
		o.nodes[id].Properties["mc"] = mc + 1
	}
	return OracleResult{Committed: true}
}

// applyMergePersonSetAll advances the model for [tmplMergePersonSetAll]. A
// present name MATCHES, so the ON CREATE branch does not fire and nothing
// changes. A missing name CREATES the node and then replaces its whole property
// set with the bound map: the merge key the pattern wrote is cleared along with
// everything else, so the modelled property set is EXACTLY the map's non-nil
// entries. The node is indexed by the name the MAP carries (which the workload
// always binds equal to the merge key); a map without one leaves a nameless
// node, modelled faithfully by simply not indexing it.
func (o *GraphOracle) applyMergePersonSetAll(params map[string]any) OracleResult {
	name, okN := paramString(params, "name")
	m, okM := params["map"].(map[string]any)
	if !okN || !okM {
		return OracleResult{ErrorMsg: "oracle: merge set-all missing name/map"}
	}
	if _, exists := o.byName[name]; exists {
		return OracleResult{Committed: true} // matched; ON CREATE does not fire.
	}
	props := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil {
			continue // a null map entry removes the key rather than assigning it.
		}
		props[k] = v
	}
	id := o.nextNodeID
	o.nextNodeID++
	o.nodes[id] = &NodeState{ID: id, Labels: []string{"Person"}, Properties: props}
	if nm, isStr := props["name"].(string); isStr {
		o.byName[nm] = id
	}
	return OracleResult{Committed: true, NodesCreated: 1}
}

// applyMergePairPattern advances the model for [tmplMergePairPattern] with the
// engine's measured all-or-nothing semantics: when a PAIRED edge already runs
// from a Person named $a to a Person named $b the WHOLE pattern matches and
// nothing changes; otherwise the WHOLE pattern is created as two FRESH Person
// nodes plus the edge, even when nodes with those names already exist. The new
// nodes are deliberately NOT indexed by name — see [mergePairKeys].
func (o *GraphOracle) applyMergePairPattern(params map[string]any) OracleResult {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	if !okA || !okB {
		return OracleResult{ErrorMsg: "oracle: merge pair missing endpoint"}
	}
	if o.hasPairedEdge(a, b) {
		return OracleResult{Committed: true} // whole pattern matched.
	}
	srcID := o.newPairNode(a)
	dstID := o.newPairNode(b)
	o.edges[edgeKey{src: srcID, dst: dstID, label: relPaired}] = &EdgeState{
		SrcID: srcID, DstID: dstID, Label: relPaired, Properties: map[string]any{},
	}
	return OracleResult{Committed: true, NodesCreated: 2, EdgesCreated: 1}
}

// newPairNode adds one whole-pattern endpoint: a Person node carrying only the
// key property, registered in the node set (so count parity and the endpoint
// name lookup work) but NOT in the name index.
func (o *GraphOracle) newPairNode(name string) uint64 {
	id := o.nextNodeID
	o.nextNodeID++
	o.nodes[id] = &NodeState{ID: id, Labels: []string{"Person"}, Properties: map[string]any{"name": name}}
	return id
}

// hasPairedEdge reports whether the model holds a PAIRED edge from a Person
// named a to a Person named b — exactly the pattern the engine's MERGE matches
// on, evaluated over the model rather than the engine.
func (o *GraphOracle) hasPairedEdge(a, b string) bool {
	for k := range o.edges {
		if k.label != relPaired {
			continue
		}
		if o.nameOf(k.src) == a && o.nameOf(k.dst) == b {
			return true
		}
	}
	return false
}

// anyPersonNamed reports whether ANY modelled Person carries the given name. It
// is the sub-case discriminator for the whole-pattern family, where duplicates
// of the same name legitimately exist, so the single-valued name index cannot
// answer the question.
func (o *GraphOracle) anyPersonNamed(name string) bool {
	for _, n := range o.nodes {
		if !hasLabel(n, "Person") {
			continue
		}
		if nm, ok := n.Properties["name"].(string); ok && nm == name {
			return true
		}
	}
	return false
}

// mergePatternCase enumerates the four sub-cases a whole-pattern MERGE can be
// in when it is issued, adjudicated on the PRE-apply model.
type mergePatternCase int

// The whole-pattern MERGE sub-cases.
const (
	// mergePatternNeither: no Person carries either key — the pattern is
	// created from nothing.
	mergePatternNeither mergePatternCase = iota
	// mergePatternOneEndpoint: exactly one of the two keys is already present,
	// and the engine still creates BOTH endpoints afresh.
	mergePatternOneEndpoint
	// mergePatternBothEndpoints: both keys are present but no PAIRED edge joins
	// them, and the engine still creates both endpoints afresh.
	mergePatternBothEndpoints
	// mergePatternWhole: the whole pattern already exists, so the statement
	// matches and applies nothing.
	mergePatternWhole
	// mergePatternCaseCount is the number of sub-cases.
	mergePatternCaseCount
)

// String renders the sub-case for a violation message.
func (c mergePatternCase) String() string {
	switch c {
	case mergePatternNeither:
		return "neither endpoint present"
	case mergePatternOneEndpoint:
		return "one endpoint present"
	case mergePatternBothEndpoints:
		return "both endpoints present, no relationship"
	case mergePatternWhole:
		return "whole pattern present"
	default:
		return fmt.Sprintf("mergePatternCase(%d)", int(c))
	}
}

// classifyMergePattern returns which of the four sub-cases a
// [tmplMergePairPattern] op falls into against the PRE-apply model, and whether
// the op's parameters were readable at all.
func classifyMergePattern(op Op, oracle *GraphOracle) (mergePatternCase, bool) {
	a, okA := paramString(op.Params, "a")
	b, okB := paramString(op.Params, "b")
	if !okA || !okB {
		return 0, false
	}
	if oracle.hasPairedEdge(a, b) {
		return mergePatternWhole, true
	}
	switch {
	case oracle.anyPersonNamed(a) && oracle.anyPersonNamed(b):
		return mergePatternBothEndpoints, true
	case oracle.anyPersonNamed(a) || oracle.anyPersonNamed(b):
		return mergePatternOneEndpoint, true
	default:
		return mergePatternNeither, true
	}
}

// expectedMergeSurfaceCounters derives the exact effect set a committed
// MERGE-surface template must report, from the pre-apply model. Every number is
// the measured engine behaviour documented in the file comment:
//
//   - [tmplMergePersonCounter] on a create: one node, one label, and TWO
//     property assignments (the pattern's `name` plus the ON CREATE `mc`). On a
//     match with an integer `mc`: exactly one assignment. On a match with no
//     `mc`: the ALL-ZERO set, because null + 1 is null and assigning null to an
//     absent property removes nothing.
//   - [tmplMergePersonSetAll] on a create: one node, one label, 1+len(map)
//     assignments (the pattern's `name` and then every non-null map entry) and
//     exactly ONE removal — the whole-entity replace clearing the merge key the
//     pattern had just written. On a match: the all-zero set, since ON CREATE
//     does not fire.
//   - [tmplMergePairPattern]: the all-zero set when the whole pattern already
//     exists, and otherwise two nodes, one relationship, two key properties and
//     two labels — the pattern is created as a unit and existing endpoints are
//     never reused.
func expectedMergeSurfaceCounters(op Op, oracle *GraphOracle) (exec.QueryCounters, bool) {
	switch op.Cypher {
	case tmplMergePersonCounter:
		name, okN := paramString(op.Params, "name")
		if !okN {
			return exec.QueryCounters{}, false
		}
		id, found := oracle.byName[name]
		if !found {
			return exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2}, true
		}
		if _, isInt := oracle.nodes[id].Properties["mc"].(int64); isInt {
			return exec.QueryCounters{PropertiesSet: 1}, true
		}
		return exec.QueryCounters{}, true

	case tmplMergePersonSetAll:
		name, okN := paramString(op.Params, "name")
		m, okM := op.Params["map"].(map[string]any)
		if !okN || !okM {
			return exec.QueryCounters{}, false
		}
		if _, exists := oracle.byName[name]; exists {
			return exec.QueryCounters{}, true
		}
		return exec.QueryCounters{
			NodesCreated:      1,
			LabelsAdded:       1,
			PropertiesSet:     1 + nonNilEntries(m),
			PropertiesRemoved: 1,
		}, true

	case tmplMergePairPattern:
		c, ok := classifyMergePattern(op, oracle)
		if !ok {
			return exec.QueryCounters{}, false
		}
		if c == mergePatternWhole {
			return exec.QueryCounters{}, true
		}
		return exec.QueryCounters{
			NodesCreated: 2, RelationshipsCreated: 1, PropertiesSet: 2, LabelsAdded: 2,
		}, true

	default:
		return exec.QueryCounters{}, false
	}
}

// checkMergeRejection adjudicates the map-parameter MERGE family: the engine
// must REJECT [tmplMergeParamMap] before applying anything, so a run in which
// it COMMITS is a deviation from the openCypher TCK contract the engine is held
// to. It is a per-op check so the failure lands on the tick that caused it.
func checkMergeRejection(tick int64, op Op, committed bool) []Violation {
	if op.Cypher != tmplMergeParamMap || !committed {
		return nil
	}
	return []Violation{{
		Kind: ViolationOracleDeviation, Tick: tick, Op: "merge param-map rejection",
		Message: fmt.Sprintf("engine ACCEPTED %q; openCypher requires a map parameter as the whole node pattern to be rejected at compile time (TCK Merge1 scenario [16])", op.Cypher),
	}}
}

// mergeSurfaceStats is the assert-something-was-seen record for the MERGE
// families (rmp #2461): which templates were issued, which branch of the
// counter family fired, which whole-pattern sub-cases were reached, and whether
// MERGE-written state was exercised through crash/recovery.
type mergeSurfaceStats struct {
	// patternCases records which of the four whole-pattern sub-cases were
	// reached, indexed by [mergePatternCase].
	patternCases [mergePatternCaseCount]bool
	// counterNames records every name a committed counter MERGE left with a
	// modelled mc, so the post-crash hook can prove such a Person was still
	// modelled — and therefore probed by the post-recovery parity check.
	counterNames map[string]bool
	// issued counts the ops of each family the workload emitted.
	counterIssued, setAllIssued, patternIssued, paramMapIssued int
	// counterCreated / counterMatched report that the ON CREATE and the ON
	// MATCH branch of the counter family each fired at least once.
	counterCreated, counterMatched bool
	// setAllCreated reports that the whole-map ON CREATE branch actually fired
	// (an arm that only ever matched would never exercise the replace).
	setAllCreated bool
	// crashAfterMerge reports that at least one crash/recovery happened after a
	// MERGE-surface op had already committed.
	crashAfterMerge bool
	// survivorChecked reports that some post-recovery check ran while a
	// counter-MERGE Person was still modelled, so the durability probe ran on
	// MERGE-written data at least once.
	survivorChecked bool
}

// newMergeSurfaceStats returns an empty stats record.
func newMergeSurfaceStats() *mergeSurfaceStats {
	return &mergeSurfaceStats{counterNames: make(map[string]bool)}
}

// noteOp records one executed op. It must be called for every tick's op and
// BEFORE the oracle is advanced, because the create-vs-match branch and the
// whole-pattern sub-case are both properties of the PRE-apply model.
func (ms *mergeSurfaceStats) noteOp(op Op, committed bool, oracle *GraphOracle) {
	switch op.Cypher {
	case tmplMergePersonCounter:
		ms.counterIssued++
		name, ok := paramString(op.Params, "name")
		if !ok || !committed {
			return
		}
		if _, found := oracle.byName[name]; found {
			ms.counterMatched = true
		} else {
			ms.counterCreated = true
		}
		ms.counterNames[name] = true
	case tmplMergePersonSetAll:
		ms.setAllIssued++
		name, ok := paramString(op.Params, "name")
		if !ok || !committed {
			return
		}
		if _, found := oracle.byName[name]; !found {
			ms.setAllCreated = true
		}
	case tmplMergePairPattern:
		ms.patternIssued++
		if !committed {
			return
		}
		if c, ok := classifyMergePattern(op, oracle); ok {
			ms.patternCases[c] = true
		}
	case tmplMergeParamMap:
		ms.paramMapIssued++
	}
}

// noteRecovery records one crash/recovery observed after the tick loop already
// executed ops, marking whether any MERGE-surface op preceded it and whether a
// counter-MERGE Person survived into the post-recovery model.
func (ms *mergeSurfaceStats) noteRecovery(oracle *GraphOracle) {
	if ms.counterIssued == 0 && ms.setAllIssued == 0 && ms.patternIssued == 0 {
		return
	}
	ms.crashAfterMerge = true
	for name := range ms.counterNames {
		if id, found := oracle.byName[name]; found {
			if _, isInt := oracle.nodes[id].Properties["mc"].(int64); isInt {
				ms.survivorChecked = true
				return
			}
		}
	}
}

// patternCasesSeen returns how many of the four whole-pattern sub-cases the run
// reached.
func (ms *mergeSurfaceStats) patternCasesSeen() int {
	n := 0
	for _, seen := range ms.patternCases {
		if seen {
			n++
		}
	}
	return n
}

// mergePatternCasesRequired is how many of the four whole-pattern sub-cases a
// clean run must reach. Three of four is the gate: the fourth
// ([mergePatternNeither]) is only available while a key is still unseen, so
// requiring all four would make the gate a function of how early the family is
// first drawn rather than of whether the surface was exercised.
const mergePatternCasesRequired = 3

// checkMergeSurfaceNonVacuity is the terminal assert-something-was-seen gate of
// the MERGE-surface coverage (rmp #2461). A clean schema-mutation run must have
// issued every one of the four families, fired BOTH branches of the counter
// family and the create branch of the whole-map family, reached at least
// [mergePatternCasesRequired] of the four whole-pattern sub-cases, and carried
// MERGE-written state through a crash into a post-recovery probe — so a green
// run is genuine evidence that the surface was exercised, not a run in which
// the new templates never fired.
func checkMergeSurfaceNonVacuity(tick int64, ms *mergeSurfaceStats) []Violation {
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge non-vacuity", Message: msg})
	}
	if ms.counterIssued == 0 {
		fail("no MERGE … ON CREATE/ON MATCH counter op was issued: the two-branch node-MERGE arm was vacuous")
	}
	if ms.setAllIssued == 0 {
		fail("no MERGE … ON CREATE SET n = $map op was issued: the whole-map arm was vacuous")
	}
	if ms.patternIssued == 0 {
		fail("no whole-pattern MERGE op was issued: the pattern arm was vacuous")
	}
	if ms.paramMapIssued == 0 {
		fail("no MERGE (n $map) op was issued: the rejection arm was vacuous")
	}
	if !ms.counterCreated {
		fail("the counter MERGE never took its ON CREATE branch")
	}
	if !ms.counterMatched {
		fail("the counter MERGE never took its ON MATCH branch")
	}
	if !ms.setAllCreated {
		fail("the whole-map MERGE never took its ON CREATE branch, so SET n = $map never ran")
	}
	if seen := ms.patternCasesSeen(); seen < mergePatternCasesRequired {
		fail(fmt.Sprintf("whole-pattern MERGE reached only %d of the %d sub-cases (need %d): %s",
			seen, int(mergePatternCaseCount), mergePatternCasesRequired, ms.patternCasesMissing()))
	}
	if !ms.crashAfterMerge {
		fail("no crash/recovery happened after a MERGE-surface op: MERGE-written state was never exercised through recovery")
	}
	if !ms.survivorChecked {
		fail("no post-recovery check saw a surviving counter-MERGE Person: the durability probe never ran on MERGE-written data")
	}
	return vs
}

// patternCasesMissing renders the whole-pattern sub-cases the run did not
// reach, for the gate's violation message.
func (ms *mergeSurfaceStats) patternCasesMissing() string {
	out := "missing:"
	for c := mergePatternCase(0); c < mergePatternCaseCount; c++ {
		if !ms.patternCases[c] {
			out += " [" + c.String() + "]"
		}
	}
	return out
}

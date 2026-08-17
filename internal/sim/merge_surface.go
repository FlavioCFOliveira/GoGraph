package sim

// merge_surface.go — MERGE surface completeness (rmp #2461, #2510, #2511, #2512).
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
//
//  5. ON CREATE SET r += $map on a WHOLE-PATTERN merge writes the relationship's
//     properties, and reports them. This is the shape rmp #2510 lost outright: a
//     whole-entity action on the relationship variable of a pattern whose
//     endpoints are NOT already bound reaches MergePattern, which deferred every
//     relationship target to a fast path that only ever runs for
//     both-endpoints-bound, all-literal-map statements. The write therefore had
//     no owner: the statement committed, created the pattern, reported only the
//     two endpoint-name properties, and left the relationship property unset,
//     with no error anywhere. The family is modelled with its exact effect set
//     (2 nodes / 1 relationship / 2+len(map) properties / 2 labels on create,
//     all-zero on match) and read back in BOTH directions by
//     [CheckMergePairRelProps], so a regression surfaces as a counters deviation
//     on the tick that caused it AND as a value deviation at the next check.
//
//  6. An ON CREATE action may target a variable bound by a clause PRECEDING the
//     MERGE, and it must write it. This is rmp #2511, the same class as 5 one
//     level out: MergePattern resolved an action target against its own chain
//     positions and chain hops only, so a target the pattern did not bind had no
//     writer in EITHER action path — the per-property form and the whole-entity
//     form alike. Two families cover it, both on the whole-pattern MERGE:
//     [tmplMergePairOuter] writes a scalar onto an outer NODE and
//     [tmplMergePairOuterRel] writes one onto an outer RELATIONSHIP, the arm that
//     additionally needs the endpoint/handle column triplet to resolve at all.
//     Neither family needs a checker of its own: the node write lands on a
//     name-indexed Person, which [CheckSchemaMutation]'s per-name property probe
//     already reads back (mo is in [schemaMutationProps]), and the relationship
//     write lands on a PAIRED edge, which [CheckMergePairRelProps] already reads
//     back in both directions. Both probes re-run after every crash, so the
//     writes are proven durable and not merely present in memory.
//
//  7. A MERGE whose DRIVING CLAUSE produced no rows must not run at all. This is
//     rmp #2512, and it is the one family here whose oracle expectation is the
//     ALL-ZERO effect set on every tick and whose read-back asserts the ABSENCE
//     of state. Both merge operators were defective — [tmplMergeZeroDriverNode]
//     reaches the node-only one and [tmplMergeZeroDriverPair] the whole-pattern
//     one — each firing once against an empty row whenever the child was
//     exhausted, because an exhausted child is indistinguishable from a MERGE
//     that is the query's LEADING clause. Every cell of the family is therefore a
//     detector in two places at once: the counters oracle fails on the tick the
//     phantom write is reported, and [CheckMergeZeroDriverAbsent] fails at the
//     next check — including after a crash, which is what proves a phantom node
//     was not merely created but durably persisted.

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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
	// tmplMergePairSetAll merges the SAME whole pattern as
	// [tmplMergePairPattern] and, on creation only, applies a WHOLE-ENTITY
	// action to the RELATIONSHIP variable from a bound map — the shape rmp
	// #2510 lost. See semantics 5 in the file comment.
	tmplMergePairSetAll = "MERGE (a:Person {name:$a})-[r:PAIRED]->(b:Person {name:$b}) ON CREATE SET r += $map"
	// tmplMergePairOuter merges the SAME whole pattern as [tmplMergePairPattern]
	// and, on creation only, writes a scalar onto a NODE bound by the preceding
	// MATCH — a target the merge pattern does not itself bind. The shape rmp #2511
	// lost. The target is a name-indexed Person, so [CheckSchemaMutation]'s
	// per-name property probe reads the write back on every check and after every
	// recovery. See semantics 6 in the file comment.
	tmplMergePairOuter = "MATCH (m:Person {name:$m}) " +
		"MERGE (a:Person {name:$a})-[r:PAIRED]->(b:Person {name:$b}) ON CREATE SET m." +
		mergeOuterNodeKey + " = $v"
	// tmplMergePairOuterRel is [tmplMergePairOuter]'s relationship counterpart: the
	// ON CREATE action targets a RELATIONSHIP bound by the preceding MATCH. It is
	// the arm that needs the endpoint/handle column triplet to resolve the target
	// at all, so it is the one a regression in that wiring reaches first. The
	// target is an existing PAIRED edge, which [CheckMergePairRelProps] already
	// reads back in both directions.
	tmplMergePairOuterRel = "MATCH (x:Person {name:$x})-[k:PAIRED]->(y:Person {name:$y}) " +
		"MERGE (a:Person {name:$a})-[r:PAIRED]->(b:Person {name:$b}) ON CREATE SET k." +
		mergePairRelKey + " = $v"
	// tmplMergeZeroDriverNode is a node MERGE whose DRIVING clause binds nothing:
	// $absent is [mergeZeroAbsentName], a key no actor can ever create. openCypher
	// runs MERGE once per incoming row, so zero rows must run it zero times — the
	// statement must report the ALL-ZERO effect set and create no node. The ON
	// CREATE action is attached so a regression that fires the clause is caught by
	// its property count too, not only by the node. See semantics 7.
	tmplMergeZeroDriverNode = "MATCH (m:Person {name:$absent}) " +
		"MERGE (n:Person {name:$z}) ON CREATE SET n.mc = 1"
	// tmplMergeZeroDriverPair is [tmplMergeZeroDriverNode]'s whole-pattern
	// counterpart, driven by the same never-matching clause. The two forms route
	// to the two DIFFERENT merge operators, and rmp #2512 defeated both, so one
	// template could not have covered the defect.
	tmplMergeZeroDriverPair = "MATCH (m:Person {name:$absent}) " +
		"MERGE (a:Person {name:$za})-[r:PAIRED]->(b:Person {name:$zb})"
)

// mergeOuterNodeKey is the single property key the outer-NODE action writes. It
// is in [schemaMutationProps], so the per-name Person probe verifies it in both
// directions — the written value where the model carries one, null everywhere
// else — with no checker of its own.
const mergeOuterNodeKey = "mo"

// mergePairRelKey is the single property key the whole-entity relationship
// action writes. One key keeps the modelled effect set exact (the create reports
// 2 endpoint names + 1 relationship property) while still being the write that
// vanished, and it is read back by [CheckMergePairRelProps] in BOTH directions —
// present where the model says present, absent where it says absent.
const mergePairRelKey = "w"

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

// mergeZeroKeys is the closed key namespace of the ZERO-ROW-DRIVER family
// (rmp #2512). Unlike every other namespace in this file it names state that must
// NEVER exist: the family's MERGE clauses are unreachable, so no Person may ever
// carry one of these keys and no PAIRED edge may ever join two of them. That is
// what [CheckMergeZeroDriverAbsent] asserts.
//
// Disjointness from every other name the workload binds is what makes the
// assertion exact rather than probabilistic: [HonestWriter.uniqueName] always
// produces "<FirstName>-<n>", and [mergePairKeys] is the "wp<n>" namespace, so no
// actor can create a "zd<n>" Person by any route other than the defect.
var mergeZeroKeys = [...]string{"zd0", "zd1", "zd2"}

// mergeZeroAbsentName is the name the zero-row-driver family's leading MATCH
// looks for. It is in the same unreachable namespace as [mergeZeroKeys], so the
// MATCH binds nothing on every tick of every run — the premise the whole family
// rests on, and one [CheckMergeZeroDriverAbsent] verifies rather than assumes: a
// Person by this name would silently turn the family into a ONE-row driver, whose
// all-zero expectation would then be wrong.
const mergeZeroAbsentName = "zd-absent"

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

// opMergePairSetAll builds a [tmplMergePairSetAll] op over two DISTINCT keys of
// [mergePairKeys], drawn exactly as [SchemaMutationWriter.opMergePairPattern]
// draws them so both families share the one key namespace and therefore the one
// match-or-create adjudication: an edge either family created makes the other
// family's next MERGE for that ordered pair MATCH, which is precisely the
// ON-CREATE-does-not-fire path worth covering.
func (SchemaMutationWriter) opMergePairSetAll(seed *Seed) Op {
	a := mergePairKeys[seed.IntN(len(mergePairKeys))]
	b := a
	for b == a {
		b = mergePairKeys[seed.IntN(len(mergePairKeys))]
	}
	return Op{Kind: OpMerge, Cypher: tmplMergePairSetAll, Params: map[string]any{
		"a": a, "b": b,
		"map": map[string]any{mergePairRelKey: int64(seed.IntN(1000))},
	}}
}

// opMergePairOuter builds a [tmplMergePairOuter] op: the whole-pattern MERGE
// keys drawn exactly as the sibling families draw them, plus an OUTER Person
// drawn from the oracle's live name index.
//
// Drawing the outer name from the index is load-bearing, not convenience. It
// guarantees the preceding MATCH binds exactly one row — the family's key
// namespace is disjoint from the index (see [mergePairKeys]), so the outer name
// can never be one of the duplicated PAIRED endpoints — which keeps the modelled
// effect set exact. names must be non-empty; the caller only reaches this arm
// with a non-empty index.
func (SchemaMutationWriter) opMergePairOuter(seed *Seed, names []string) Op {
	a := mergePairKeys[seed.IntN(len(mergePairKeys))]
	b := a
	for b == a {
		b = mergePairKeys[seed.IntN(len(mergePairKeys))]
	}
	return Op{Kind: OpMerge, Cypher: tmplMergePairOuter, Params: map[string]any{
		"a": a, "b": b,
		"m": names[seed.IntN(len(names))],
		"v": int64(seed.IntN(1000)),
	}}
}

// opMergePairOuterRel builds a [tmplMergePairOuterRel] op over an EXISTING
// modelled PAIRED edge, so the preceding MATCH binds exactly one relationship.
// When the model holds no PAIRED edge yet the family cannot fire, and the draw
// falls back to the plain whole-pattern MERGE — which is what creates the edges
// this family later targets. The fallback consumes the same seed draws either
// way, so the op stream stays a pure function of (seed state, oracle state).
func (w SchemaMutationWriter) opMergePairOuterRel(seed *Seed, edges []pairedEdgeByName) Op {
	a := mergePairKeys[seed.IntN(len(mergePairKeys))]
	b := a
	for b == a {
		b = mergePairKeys[seed.IntN(len(mergePairKeys))]
	}
	if len(edges) == 0 {
		return Op{Kind: OpMerge, Cypher: tmplMergePairPattern, Params: map[string]any{"a": a, "b": b}}
	}
	e := edges[seed.IntN(len(edges))]
	return Op{Kind: OpMerge, Cypher: tmplMergePairOuterRel, Params: map[string]any{
		"a": a, "b": b,
		"x": e.src, "y": e.dst,
		"v": int64(seed.IntN(1000)),
	}}
}

// opMergeZeroDriverNode builds a [tmplMergeZeroDriverNode] op: a node MERGE
// behind a MATCH that binds nothing. The merge key is drawn from
// [mergeZeroKeys], the namespace that must stay empty; the draw exists so a
// regression cannot be confined to one key.
func (SchemaMutationWriter) opMergeZeroDriverNode(seed *Seed) Op {
	return Op{Kind: OpMerge, Cypher: tmplMergeZeroDriverNode, Params: map[string]any{
		"absent": mergeZeroAbsentName,
		"z":      mergeZeroKeys[seed.IntN(len(mergeZeroKeys))],
	}}
}

// opMergeZeroDriverPair builds a [tmplMergeZeroDriverPair] op: the whole-pattern
// MERGE behind the same never-matching MATCH, over two DISTINCT keys of
// [mergeZeroKeys] so the pattern is a two-node one, exactly as the reachable
// whole-pattern families draw theirs.
func (SchemaMutationWriter) opMergeZeroDriverPair(seed *Seed) Op {
	a := mergeZeroKeys[seed.IntN(len(mergeZeroKeys))]
	b := a
	for b == a {
		b = mergeZeroKeys[seed.IntN(len(mergeZeroKeys))]
	}
	return Op{Kind: OpMerge, Cypher: tmplMergeZeroDriverPair, Params: map[string]any{
		"absent": mergeZeroAbsentName, "za": a, "zb": b,
	}}
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

// applyMergePairSetAll advances the model for [tmplMergePairSetAll]. The
// match-or-create decision is [tmplMergePairPattern]'s, unchanged — the pattern
// is identical, so an edge either family created matches here. The only delta is
// on create: the ON CREATE branch fires and its whole-entity `+=` writes the
// bound map's non-nil entries onto the NEW relationship. On a match the branch
// does not fire, so the existing edge's properties are left exactly as they are
// (including left ABSENT when the plain family created it), which is what makes
// [CheckMergePairRelProps]'s absent-direction assertion meaningful.
func (o *GraphOracle) applyMergePairSetAll(params map[string]any) OracleResult {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	m, okM := params["map"].(map[string]any)
	if !okA || !okB || !okM {
		return OracleResult{ErrorMsg: "oracle: merge pair set-all missing endpoint/map"}
	}
	if o.hasPairedEdge(a, b) {
		return OracleResult{Committed: true} // whole pattern matched; ON CREATE does not fire.
	}
	props := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil {
			continue // a null map entry removes the key rather than assigning it.
		}
		props[k] = v
	}
	srcID := o.newPairNode(a)
	dstID := o.newPairNode(b)
	o.edges[edgeKey{src: srcID, dst: dstID, label: relPaired}] = &EdgeState{
		SrcID: srcID, DstID: dstID, Label: relPaired, Properties: props,
	}
	return OracleResult{Committed: true, NodesCreated: 2, EdgesCreated: 1}
}

// applyMergePairOuter advances the model for [tmplMergePairOuter]. The
// match-or-create decision is [tmplMergePairPattern]'s, unchanged. The only delta
// is on create: the ON CREATE branch fires and writes $v onto the OUTER Person
// named $m, which the preceding MATCH bound. On a match the branch does not fire
// and the outer Person is left exactly as it is — the absent-direction the
// per-name property probe asserts.
//
// An outer name the model does not hold cannot occur: the op builder draws it
// from the live name index in the same tick the op executes. It is reported as a
// modelling error rather than guessed at, so a future change that breaks the
// invariant fails the run instead of silently diverging.
func (o *GraphOracle) applyMergePairOuter(params map[string]any) OracleResult {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	m, okM := paramString(params, "m")
	if !okA || !okB || !okM {
		return OracleResult{ErrorMsg: "oracle: merge pair outer missing endpoint/outer name"}
	}
	id, found := o.byName[m]
	if !found {
		return OracleResult{ErrorMsg: "oracle: merge pair outer names an unmodelled Person: " + m}
	}
	if o.hasPairedEdge(a, b) {
		return OracleResult{Committed: true} // whole pattern matched; ON CREATE does not fire.
	}
	srcID := o.newPairNode(a)
	dstID := o.newPairNode(b)
	o.edges[edgeKey{src: srcID, dst: dstID, label: relPaired}] = &EdgeState{
		SrcID: srcID, DstID: dstID, Label: relPaired, Properties: map[string]any{},
	}
	o.nodes[id].Properties[mergeOuterNodeKey] = params["v"]
	return OracleResult{Committed: true, NodesCreated: 2, EdgesCreated: 1}
}

// applyMergePairOuterRel advances the model for [tmplMergePairOuterRel]. As
// above, the match-or-create decision is the plain family's, and on create the ON
// CREATE branch writes $v onto the OUTER PAIRED edge the preceding MATCH bound —
// never onto the relationship this MERGE creates, which is left property-free.
//
// The outer edge is addressed by endpoint NAMES, exactly as the engine's MATCH
// addresses it: at most one PAIRED edge exists per ordered name pair, because
// every family's create decision goes through [GraphOracle.hasPairedEdge], which
// is itself by name.
func (o *GraphOracle) applyMergePairOuterRel(params map[string]any) OracleResult {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	x, okX := paramString(params, "x")
	y, okY := paramString(params, "y")
	if !okA || !okB || !okX || !okY {
		return OracleResult{ErrorMsg: "oracle: merge pair outer-rel missing endpoint"}
	}
	outer := o.pairedEdgeBetween(x, y)
	if outer == nil {
		return OracleResult{ErrorMsg: "oracle: merge pair outer-rel names an unmodelled PAIRED edge: " + x + "->" + y}
	}
	if o.hasPairedEdge(a, b) {
		return OracleResult{Committed: true} // whole pattern matched; ON CREATE does not fire.
	}
	srcID := o.newPairNode(a)
	dstID := o.newPairNode(b)
	o.edges[edgeKey{src: srcID, dst: dstID, label: relPaired}] = &EdgeState{
		SrcID: srcID, DstID: dstID, Label: relPaired, Properties: map[string]any{},
	}
	outer.Properties[mergePairRelKey] = params["v"]
	return OracleResult{Committed: true, NodesCreated: 2, EdgesCreated: 1}
}

// applyMergeZeroDriver advances the model for the zero-row-driver families
// ([tmplMergeZeroDriverNode], [tmplMergeZeroDriverPair]): the leading MATCH binds
// nothing, so the MERGE runs zero times and the statement commits having changed
// nothing at all. The model is therefore untouched — deliberately, and on every
// tick: this is the one MERGE family whose correct outcome is that the graph did
// not move (rmp #2512).
func (o *GraphOracle) applyMergeZeroDriver() OracleResult {
	return OracleResult{Committed: true}
}

// pairedEdgeBetween returns the modelled PAIRED edge running from a Person named
// a to a Person named b, or nil when there is none — the same by-name addressing
// [GraphOracle.hasPairedEdge] uses, returning the state rather than a bool so the
// outer-relationship action can write through it.
func (o *GraphOracle) pairedEdgeBetween(a, b string) *EdgeState {
	for k, e := range o.edges {
		if k.label != relPaired {
			continue
		}
		if o.nameOf(k.src) == a && o.nameOf(k.dst) == b {
			return e
		}
	}
	return nil
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

	case tmplMergePairSetAll:
		c, ok := classifyMergePattern(op, oracle)
		m, okM := op.Params["map"].(map[string]any)
		if !ok || !okM {
			return exec.QueryCounters{}, false
		}
		if c == mergePatternWhole {
			return exec.QueryCounters{}, true
		}
		// The pattern's own effect set plus one assignment per non-nil map entry
		// — the whole-entity relationship write. Reporting only the pattern's 2
		// properties is exactly the shape of rmp #2510, so this arm fails the run
		// on the tick the write is lost.
		return exec.QueryCounters{
			NodesCreated: 2, RelationshipsCreated: 1,
			PropertiesSet: 2 + nonNilEntries(m), LabelsAdded: 2,
		}, true

	case tmplMergePairOuter, tmplMergePairOuterRel:
		c, ok := classifyMergePattern(op, oracle)
		if !ok {
			return exec.QueryCounters{}, false
		}
		if c == mergePatternWhole {
			return exec.QueryCounters{}, true
		}
		// The pattern's own effect set plus exactly ONE assignment: the single
		// scalar the ON CREATE branch writes onto the outer target. A SET counts
		// even when it assigns the value the target already carried, so the
		// expectation is unconditional. Reporting only the pattern's 2 properties
		// is exactly the shape of rmp #2511, so this arm fails the run on the tick
		// the write is lost.
		return exec.QueryCounters{
			NodesCreated: 2, RelationshipsCreated: 1, PropertiesSet: 3, LabelsAdded: 2,
		}, true

	case tmplMergeZeroDriverNode, tmplMergeZeroDriverPair:
		// The leading MATCH binds nothing, so the MERGE runs zero times and the
		// statement reports the ALL-ZERO effect set — unconditionally, with no
		// dependence on the model, because the driver never matches on any tick.
		// Any non-zero counter here is rmp #2512 reappearing, caught on the very
		// tick the phantom write is reported.
		return exec.QueryCounters{}, true

	default:
		return exec.QueryCounters{}, false
	}
}

// CheckMergeZeroDriverAbsent asserts that the zero-row-driver family (rmp #2512)
// created nothing: no Person carries a [mergeZeroKeys] name, and no PAIRED edge
// joins two of them. It is the read-back half of the guard — the counters oracle
// catches the phantom effect REPORT on the tick it happens, this catches the
// phantom STATE, and running it after crash/recovery is what distinguishes a
// phantom that was merely created from one that was durably persisted.
//
// It also verifies the family's PREMISE rather than assuming it: [mergeZeroAbsentName]
// must not exist, since a Person by that name would turn the never-matching driver
// into a one-row driver and make the all-zero expectation wrong. The namespace is
// disjoint from every name the workload can bind, so this can only fail if that
// disjointness is ever broken — which is exactly when a silent pass would begin.
func CheckMergeZeroDriverAbsent(tick int64, engine *EngineAdapter) []Violation {
	var vs []Violation
	count := func(q, what string) (int64, bool) {
		n, err := engine.scalarCount(q)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "merge zero-driver read",
				Message: fmt.Sprintf("%s: query %q failed: %v", what, q, err)})
			return 0, false
		}
		return n, true
	}
	if n, ok := count(
		fmt.Sprintf("MATCH (m:Person {name:'%s'}) RETURN count(m)", mergeZeroAbsentName),
		"zero-driver premise",
	); ok && n != 0 {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge zero-driver premise",
			Message: fmt.Sprintf("%d Person{name:%q} exist; the zero-row-driver family's MATCH must bind NOTHING, "+
				"otherwise its all-zero expectation is wrong and the guard is vacuous", n, mergeZeroAbsentName)})
	}
	for _, k := range mergeZeroKeys {
		n, ok := count(fmt.Sprintf("MATCH (n:Person {name:'%s'}) RETURN count(n)", k), "zero-driver node")
		if !ok || n == 0 {
			continue
		}
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge zero-driver node",
			Message: fmt.Sprintf("%d Person{name:%q} exist, want 0: a MERGE whose driving clause produced no rows "+
				"CREATED a node (rmp #2512)", n, k)})
	}
	for _, a := range mergeZeroKeys {
		for _, b := range mergeZeroKeys {
			if a == b {
				continue
			}
			q := fmt.Sprintf("MATCH (:Person {name:'%s'})-[r:PAIRED]->(:Person {name:'%s'}) RETURN count(r)", a, b)
			if n, ok := count(q, "zero-driver relationship"); ok && n != 0 {
				vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge zero-driver relationship",
					Message: fmt.Sprintf("%d PAIRED(%q->%q) exist, want 0: a whole-pattern MERGE whose driving clause "+
						"produced no rows CREATED the pattern (rmp #2512)", n, a, b)})
			}
		}
	}
	return vs
}

// CheckMergePairRelProps reads every modelled PAIRED relationship's property
// back through the real engine and asserts it matches the model in BOTH
// directions: the value the whole-entity ON CREATE action wrote where the model
// carries one, and NULL where it does not.
//
// Both directions are load-bearing. The present-direction is the rmp #2510
// regression detector — a lost whole-entity relationship write reads back null.
// The absent-direction is what stops the checker from degenerating into a
// tautology satisfied by an engine that writes the property onto every edge: the
// plain [tmplMergePairPattern] family creates PAIRED edges with NO properties in
// the same key namespace, so a spurious write is caught too.
//
// Each ordered (name_a, name_b) pair carries at most one PAIRED edge — a second
// MERGE of the same pair matches rather than creating — so the by-name read is
// unambiguous even though the family deliberately leaves duplicate Person nodes
// behind. Running it after crash/recovery is what proves the relationship write
// is DURABLE and not merely present in memory.
func CheckMergePairRelProps(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	for _, e := range oracle.pairedEdgesByName() {
		q := fmt.Sprintf(
			"MATCH (a:Person {name:'%s'})-[r:PAIRED]->(b:Person {name:'%s'}) RETURN r.%s",
			e.src, e.dst, mergePairRelKey)
		got, err := engine.projectRowValues(ctx, q, 1)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "merge pair rel-prop read",
				Message: fmt.Sprintf("PAIRED(%q->%q).%s read failed: %v", e.src, e.dst, mergePairRelKey, err)})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{Kind: ViolationACIDDurability, Tick: tick, Op: "merge pair existence",
				Message: fmt.Sprintf("committed PAIRED(%q->%q) absent (did not survive recovery)", e.src, e.dst)})
			continue
		}
		want, modelled := e.props[mergePairRelKey]
		isNull := got[0] == nil || expr.IsNull(got[0])
		if !modelled {
			if !isNull {
				vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge pair rel-prop",
					Message: fmt.Sprintf("PAIRED(%q->%q).%s = %s, want null (no ON CREATE action wrote this edge)",
						e.src, e.dst, mergePairRelKey, got[0].String())})
			}
			continue
		}
		if isNull {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge pair rel-prop",
				Message: fmt.Sprintf("PAIRED(%q->%q).%s is null, want %s (ON CREATE SET r += $map was LOST: rmp #2510)",
					e.src, e.dst, mergePairRelKey, canonicalValueString(want))})
			continue
		}
		if wantStr := canonicalValueString(want); got[0].String() != wantStr {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge pair rel-prop",
				Message: fmt.Sprintf("PAIRED(%q->%q).%s = %s, want %s (whole-entity relationship write did not round-trip)",
					e.src, e.dst, mergePairRelKey, got[0].String(), wantStr)})
		}
	}
	return vs
}

// pairedEdgeByName is one modelled PAIRED edge addressed by its endpoint NAMES,
// which is how [CheckMergePairRelProps] reaches it through Cypher.
type pairedEdgeByName struct {
	props    map[string]any
	src, dst string
}

// pairedEdgesByName returns every modelled PAIRED edge with its endpoint names
// and property map, in deterministic order. Edges whose endpoints have lost their
// name (which the family itself never does) are skipped, since no by-name query
// could address them.
//
// The result is SORTED by endpoint name rather than left in map-iteration order,
// because [SchemaMutationWriter.opMergePairOuterRel] DRAWS from this list with
// the seed: an unstable order would make the op stream unreproducible. At most
// one PAIRED edge exists per ordered name pair, so the sort is a total order. The
// property maps are the live ones, so the outer-relationship action can write
// through them.
func (o *GraphOracle) pairedEdgesByName() []pairedEdgeByName {
	out := make([]pairedEdgeByName, 0, len(o.edges))
	for k, e := range o.edges {
		if k.label != relPaired {
			continue
		}
		src, dst := o.nameOf(k.src), o.nameOf(k.dst)
		if src == "" || dst == "" {
			continue
		}
		out = append(out, pairedEdgeByName{src: src, dst: dst, props: e.Properties})
	}
	slices.SortFunc(out, func(a, b pairedEdgeByName) int {
		if a.src != b.src {
			return cmp.Compare(a.src, b.src)
		}
		return cmp.Compare(a.dst, b.dst)
	})
	return out
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
	// pairSetAllIssued counts the whole-pattern MERGE ops carrying a
	// whole-entity relationship action (rmp #2510).
	pairSetAllIssued int
	// pairOuterIssued / pairOuterRelIssued count the whole-pattern MERGE ops whose
	// action targets a NODE / a RELATIONSHIP bound by the preceding clause
	// (rmp #2511).
	pairOuterIssued, pairOuterRelIssued int
	// zeroDriverNodeIssued / zeroDriverPairIssued count the ops of the
	// zero-row-driver family (rmp #2512), one per merge operator. Both must be
	// non-zero for the run to be evidence: the family's whole assertion is that
	// nothing happened, which an unissued family satisfies trivially.
	zeroDriverNodeIssued, zeroDriverPairIssued int
	// counterCreated / counterMatched report that the ON CREATE and the ON
	// MATCH branch of the counter family each fired at least once.
	counterCreated, counterMatched bool
	// setAllCreated reports that the whole-map ON CREATE branch actually fired
	// (an arm that only ever matched would never exercise the replace).
	setAllCreated bool
	// pairSetAllCreated reports that the whole-pattern family's ON CREATE branch
	// fired at least once, which is the only path on which the whole-entity
	// relationship write runs at all: a run that only ever MATCHED would never
	// have exercised the rmp #2510 shape, and its green result would be vacuous.
	pairSetAllCreated bool
	// pairOuterCreated / pairOuterRelCreated report that the outer-target families
	// took their ON CREATE branch at least once, which is the only path on which
	// the outer write runs at all: a run that only ever MATCHED would never have
	// exercised the rmp #2511 shape, and its green result would be vacuous.
	pairOuterCreated, pairOuterRelCreated bool
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
	case tmplMergePairSetAll:
		ms.pairSetAllIssued++
		if !committed {
			return
		}
		// The sub-case adjudication is the plain family's, so both families feed
		// the one patternCases record; only a create fires the relationship write.
		c, ok := classifyMergePattern(op, oracle)
		if !ok {
			return
		}
		ms.patternCases[c] = true
		if c != mergePatternWhole {
			ms.pairSetAllCreated = true
		}
	case tmplMergePairOuter:
		ms.pairOuterIssued++
		if !committed {
			return
		}
		c, ok := classifyMergePattern(op, oracle)
		if !ok {
			return
		}
		ms.patternCases[c] = true
		if c != mergePatternWhole {
			ms.pairOuterCreated = true
		}
	case tmplMergePairOuterRel:
		ms.pairOuterRelIssued++
		if !committed {
			return
		}
		c, ok := classifyMergePattern(op, oracle)
		if !ok {
			return
		}
		ms.patternCases[c] = true
		if c != mergePatternWhole {
			ms.pairOuterRelCreated = true
		}
	case tmplMergeZeroDriverNode:
		ms.zeroDriverNodeIssued++
	case tmplMergeZeroDriverPair:
		ms.zeroDriverPairIssued++
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
	if ms.pairSetAllIssued == 0 {
		fail("no whole-pattern MERGE … ON CREATE SET r += $map op was issued: the whole-entity relationship arm was vacuous")
	}
	if !ms.pairSetAllCreated {
		fail("the whole-pattern MERGE with a relationship action never took its ON CREATE branch, so SET r += $map never ran")
	}
	if ms.pairOuterIssued == 0 {
		fail("no whole-pattern MERGE … ON CREATE SET <outer node>.x op was issued: the outer-node target arm was vacuous")
	}
	if !ms.pairOuterCreated {
		fail("the outer-node MERGE action never took its ON CREATE branch, so the outer-node write never ran")
	}
	if ms.pairOuterRelIssued == 0 {
		fail("no whole-pattern MERGE … ON CREATE SET <outer relationship>.x op was issued: the outer-relationship target arm was vacuous")
	}
	if !ms.pairOuterRelCreated {
		fail("the outer-relationship MERGE action never took its ON CREATE branch, so the outer-relationship write never ran")
	}
	// The zero-row-driver family asserts an ABSENCE, so an unissued family passes
	// its checker trivially. These two clauses are what stop that silent pass.
	if ms.zeroDriverNodeIssued == 0 {
		fail("no zero-row-driver node MERGE op was issued: the node arm of the zero-row-driver family was vacuous (rmp #2512)")
	}
	if ms.zeroDriverPairIssued == 0 {
		fail("no zero-row-driver whole-pattern MERGE op was issued: the pattern arm of the zero-row-driver family was vacuous (rmp #2512)")
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

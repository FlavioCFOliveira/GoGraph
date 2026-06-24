package sim

import (
	"cmp"
	"fmt"
	"slices"
)

// The Phase-1 workload emits a small, fixed set of Cypher templates. The oracle
// recognises an operation by matching the query string against these exact
// templates and reads its parameters from the bound params map. Keeping the
// templates as shared constants means the actors (which build the queries) and
// the oracle (which models their effect) cannot drift apart: a new template
// must be added in both places or the oracle returns an "unmodelled" result and
// the checker flags the deviation.
const (
	// tmplCreatePerson creates one Person node with name and age.
	tmplCreatePerson = "CREATE (n:Person {name:$name, age:$age})"
	// tmplCreateKnows links two existing Person nodes by name with a KNOWS edge.
	tmplCreateKnows = "MATCH (a:Person {name:$a}),(b:Person {name:$b}) CREATE (a)-[:KNOWS]->(b)"
	// tmplSetAge updates the age of the Person matched by name.
	tmplSetAge = "MATCH (n:Person {name:$name}) SET n.age=$age"
	// tmplDetachDelete removes the Person matched by name and its edges.
	tmplDetachDelete = "MATCH (n:Person {name:$name}) DETACH DELETE n"
	// tmplMergePerson merges a Person by name, marking newly-created ones.
	tmplMergePerson = "MERGE (n:Person {name:$name}) ON CREATE SET n.created=true"
	// tmplCreateTyped creates one Typed node carrying a property of every
	// round-tripping Cypher kind — string, integer, float, boolean, list, and an
	// ISO-8601 temporal string — keyed by a unique integer id. The type-coverage
	// scenario uses it to verify each kind survives commit + crash/recovery.
	tmplCreateTyped = "CREATE (n:Typed {id:$id, s:$s, i:$i, f:$f, b:$b, lst:$lst, ts:$ts})"
)

// typedPropKeys are the property keys a [tmplCreateTyped] node carries, in a
// fixed order, plus the never-set key "absent" the checker verifies reads NULL.
// The checker projects exactly these so engine and oracle compare the same set.
var typedPropKeys = []string{"id", "s", "i", "f", "b", "lst", "ts", "absent"}

// NodeState is the oracle's record of a single node: its synthetic oracle id,
// labels, and properties. It mirrors what the engine must hold, not how the
// engine stores it.
type NodeState struct {
	ID         uint64
	Labels     []string
	Properties map[string]any
}

// EdgeState is the oracle's record of a single directed edge between two
// oracle node ids, carrying its relationship label and properties.
type EdgeState struct {
	SrcID, DstID uint64
	Label        string
	Properties   map[string]any
}

// OracleResult is the oracle's prediction for one operation: whether it commits,
// how many nodes and edges it creates, and (when it predicts a failure) the
// reason. The simulator records it for comparison with the engine outcome and
// for replay/shrinking.
type OracleResult struct {
	Committed    bool
	NodesCreated int
	EdgesCreated int
	ErrorMsg     string
}

// OracleOp is one entry in the oracle's operation history: the tick at which it
// ran, the Cypher and parameters issued, and the predicted result. The history
// is retained so a future phase can shrink a failing trace to a minimal
// reproducer.
type OracleOp struct {
	Tick     int64
	Cypher   string
	Params   map[string]any
	Expected OracleResult
}

// nameKey indexes a Person node by its (unique) name property so MATCH/MERGE by
// name is O(1) in the oracle.
type nameKey = string

// GraphOracle is a correct-by-construction shadow model of what the graph must
// contain after a sequence of Phase-1 workload operations. It is deliberately
// minimal: it models only the five templates the workload emits and treats the
// Person name property as a logical key (the workload binds names uniquely and
// MERGE de-duplicates on it), which is what makes its predictions obviously
// correct without re-implementing the engine.
//
// # Concurrency contract
//
// GraphOracle is NOT safe for concurrent use; it is mutated and read from the
// single simulation goroutine.
type GraphOracle struct {
	nodes      map[uint64]*NodeState
	byName     map[nameKey]uint64
	edges      map[edgeKey]*EdgeState
	nextNodeID uint64
	ops        []OracleOp
	// typed models [tmplCreateTyped] nodes keyed by their unique integer id,
	// storing the full property map (string/int/float/bool/list/temporal-string)
	// so the type-coverage checker can read each back from the engine and confirm
	// it round-trips and survives recovery. Empty for every other scenario.
	typed map[int64]map[string]any
	// uniqueOnName, when true, models an active UNIQUE constraint on
	// (Person, name): a CREATE of a name that already exists must be REJECTED
	// by the engine (a typed constraint-violation error, no state change), which
	// createPerson predicts. It is off by default, so every constraint-free
	// scenario keeps the prior "CREATE always commits" behaviour. The constraint
	// is engine schema (it survives crash/recovery via the WAL), so the oracle
	// keeps modelling it across a crash — the recovered engine must still enforce
	// it. See [GraphOracle.SetUniqueOnName].
	uniqueOnName bool
}

// edgeKey identifies an edge by source, destination, and label, matching the
// (src,dst,label) identity the checker probes.
type edgeKey struct {
	src, dst uint64
	label    string
}

// NewGraphOracle returns an empty oracle. Node ids start at 1 so zero can mean
// "no node".
func NewGraphOracle() *GraphOracle {
	return &GraphOracle{
		nodes:      make(map[uint64]*NodeState),
		byName:     make(map[nameKey]uint64),
		edges:      make(map[edgeKey]*EdgeState),
		nextNodeID: 1,
		typed:      make(map[int64]map[string]any),
	}
}

// paramString returns the string value bound to key, or "" with ok=false.
func paramString(params map[string]any, key string) (string, bool) {
	v, ok := params[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// recordOp appends an operation to the history and returns its predicted
// result so callers can both record and return in one statement.
func (o *GraphOracle) recordOp(cypher string, params map[string]any, res OracleResult) OracleResult {
	o.ops = append(o.ops, OracleOp{Cypher: cypher, Params: params, Expected: res})
	return res
}

// ApplyCreate models the CREATE templates: a bare Person create
// ([tmplCreatePerson]) or a KNOWS edge between two existing Person nodes
// ([tmplCreateKnows]). It mutates the oracle to reflect the predicted committed
// state and returns the prediction.
func (o *GraphOracle) ApplyCreate(cypher string, params map[string]any) OracleResult {
	switch cypher {
	case tmplCreatePerson:
		return o.recordOp(cypher, params, o.createPerson(params))
	case tmplCreateKnows:
		return o.recordOp(cypher, params, o.createKnows(params))
	case tmplCreateTyped:
		return o.recordOp(cypher, params, o.createTyped(params))
	default:
		return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled CREATE"})
	}
}

// createTyped models [tmplCreateTyped]: it records the full typed property set
// of a Typed node, keyed by its unique integer id, so the type-coverage checker
// can verify every kind round-trips. CREATE always commits (no constraint here).
func (o *GraphOracle) createTyped(params map[string]any) OracleResult {
	idv, ok := params["id"].(int64)
	if !ok {
		return OracleResult{ErrorMsg: "oracle: createTyped missing/!int64 id"}
	}
	props := make(map[string]any, len(typedPropKeys))
	for _, k := range typedPropKeys {
		if k == "absent" {
			continue // never set; the checker verifies it reads NULL
		}
		props[k] = params[k]
	}
	o.typed[idv] = props
	// Also register a plain node so node-count parity holds.
	nid := o.nextNodeID
	o.nextNodeID++
	o.nodes[nid] = &NodeState{ID: nid, Labels: []string{"Typed"}, Properties: props}
	return OracleResult{Committed: true, NodesCreated: 1}
}

// TypedNode returns the modelled property map for the Typed node with the given
// id, and whether it exists. The returned map is the oracle's own and must not
// be mutated.
func (o *GraphOracle) TypedNode(id int64) (map[string]any, bool) {
	p, ok := o.typed[id]
	return p, ok
}

// TypedIDs returns the ids of every modelled Typed node in ascending order
// (deterministic, for reproducible checker iteration).
func (o *GraphOracle) TypedIDs() []int64 {
	out := make([]int64, 0, len(o.typed))
	for id := range o.typed {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// SetUniqueOnName declares (or clears) an active UNIQUE constraint on
// (Person, name) in the model, so the oracle predicts the engine will REJECT a
// CREATE of a duplicate name. It is called by the constraint-enforcement
// scenario after it creates the constraint in the engine, and again after a
// crash/recovery to assert the constraint is still modelled as enforced.
func (o *GraphOracle) SetUniqueOnName(active bool) { o.uniqueOnName = active }

// UniqueOnName reports whether the oracle models an active UNIQUE (Person, name)
// constraint.
func (o *GraphOracle) UniqueOnName() bool { return o.uniqueOnName }

// createPerson adds a Person node. The workload binds unique names; if a name
// somehow repeats, the engine still creates a second node (CREATE is not
// idempotent), so the oracle does too and overwrites the name index to the
// latest id (the checker only samples existence, not name→id bijection).
//
// Under an active UNIQUE (Person, name) constraint ([GraphOracle.SetUniqueOnName])
// a CREATE of an already-present name is instead predicted REJECTED: the engine
// must raise a typed constraint-violation error and apply nothing, so the oracle
// returns a non-committed result and changes no state.
func (o *GraphOracle) createPerson(params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: createPerson missing name"}
	}
	if o.uniqueOnName {
		if _, dup := o.byName[name]; dup {
			// Predicted UNIQUE violation: the engine rejects, nothing changes.
			return OracleResult{Committed: false, ErrorMsg: "oracle: UNIQUE(Person.name) violation"}
		}
	}
	age := params["age"]
	id := o.nextNodeID
	o.nextNodeID++
	o.nodes[id] = &NodeState{
		ID:         id,
		Labels:     []string{"Person"},
		Properties: map[string]any{"name": name, "age": age},
	}
	o.byName[name] = id
	return OracleResult{Committed: true, NodesCreated: 1}
}

// createKnows adds a KNOWS edge between the Person nodes named $a and $b. When
// either endpoint is absent the MATCH yields no rows, so CREATE runs zero times
// and nothing is created — a committed, zero-effect result, exactly as the
// engine behaves.
func (o *GraphOracle) createKnows(params map[string]any) OracleResult {
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	if !okA || !okB {
		return OracleResult{ErrorMsg: "oracle: createKnows missing endpoint"}
	}
	srcID, srcOK := o.byName[a]
	dstID, dstOK := o.byName[b]
	if !srcOK || !dstOK {
		return OracleResult{Committed: true} // MATCH found nothing; no edge created.
	}
	k := edgeKey{src: srcID, dst: dstID, label: "KNOWS"}
	o.edges[k] = &EdgeState{SrcID: srcID, DstID: dstID, Label: "KNOWS", Properties: map[string]any{}}
	return OracleResult{Committed: true, EdgesCreated: 1}
}

// ApplyMatch models read-only and SET templates. Pure reads ([RETURN]/aggregate
// queries) never change state and commit trivially; the SET template
// ([tmplSetAge]) updates the matched node's age in place.
func (o *GraphOracle) ApplyMatch(cypher string, params map[string]any) OracleResult {
	if cypher == tmplSetAge {
		return o.recordOp(cypher, params, o.setAge(params))
	}
	// Every other MATCH the workload emits is a pure read with no side effects.
	return o.recordOp(cypher, params, OracleResult{Committed: true})
}

// setAge updates the age property of the Person matched by name. A miss is a
// committed zero-effect result (MATCH found nothing, SET ran zero times).
func (o *GraphOracle) setAge(params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: setAge missing name"}
	}
	id, found := o.byName[name]
	if !found {
		return OracleResult{Committed: true}
	}
	o.nodes[id].Properties["age"] = params["age"]
	return OracleResult{Committed: true}
}

// ApplyMerge models [tmplMergePerson]: MERGE by name creates the Person only
// when absent (setting created=true on the new one) and is a no-op otherwise.
func (o *GraphOracle) ApplyMerge(cypher string, params map[string]any) OracleResult {
	if cypher != tmplMergePerson {
		return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled MERGE"})
	}
	name, ok := paramString(params, "name")
	if !ok {
		return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: merge missing name"})
	}
	if _, exists := o.byName[name]; exists {
		return o.recordOp(cypher, params, OracleResult{Committed: true}) // matched; no create.
	}
	id := o.nextNodeID
	o.nextNodeID++
	o.nodes[id] = &NodeState{
		ID:         id,
		Labels:     []string{"Person"},
		Properties: map[string]any{"name": name, "created": true},
	}
	o.byName[name] = id
	return o.recordOp(cypher, params, OracleResult{Committed: true, NodesCreated: 1})
}

// ApplyDelete models [tmplDetachDelete]: DETACH DELETE removes the Person
// matched by name together with every incident edge. A miss is a committed
// zero-effect result.
func (o *GraphOracle) ApplyDelete(cypher string, params map[string]any) OracleResult {
	if cypher != tmplDetachDelete {
		return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled DELETE"})
	}
	name, ok := paramString(params, "name")
	if !ok {
		return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: delete missing name"})
	}
	id, found := o.byName[name]
	if !found {
		return o.recordOp(cypher, params, OracleResult{Committed: true})
	}
	for k := range o.edges {
		if k.src == id || k.dst == id {
			delete(o.edges, k)
		}
	}
	delete(o.nodes, id)
	delete(o.byName, name)
	return o.recordOp(cypher, params, OracleResult{Committed: true})
}

// ApplyMalformed models an intentionally ill-formed operation ([OpMalformed]
// from [MalformedSender]): the engine is expected to reject it with a typed
// error and apply no mutation, so the oracle records it as an expected-error
// no-op and changes no modelled state. Recording it keeps the operation history
// complete for replay/shrinking.
func (o *GraphOracle) ApplyMalformed(cypher string, params map[string]any) OracleResult {
	return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: malformed op (expected engine error, no state change)"})
}

// NodeCount returns the number of nodes the oracle currently models.
func (o *GraphOracle) NodeCount() int { return len(o.nodes) }

// EdgeCount returns the number of edges the oracle currently models.
func (o *GraphOracle) EdgeCount() int { return len(o.edges) }

// HasNode reports whether the oracle models a node with the given id.
func (o *GraphOracle) HasNode(id uint64) bool {
	_, ok := o.nodes[id]
	return ok
}

// HasEdge reports whether the oracle models a directed edge of the given label
// between src and dst.
func (o *GraphOracle) HasEdge(src, dst uint64, label string) bool {
	_, ok := o.edges[edgeKey{src: src, dst: dst, label: label}]
	return ok
}

// Ops returns the recorded operation history (for replay and Phase-4
// shrinking). The returned slice aliases the oracle's backing store and must
// not be mutated.
func (o *GraphOracle) Ops() []OracleOp { return o.ops }

// NodeNames returns the Person names currently modelled, in ascending sorted
// order. The deterministic order is load-bearing: actors index into this slice
// with seed-derived integers, so a non-deterministic (map-range) order would
// make the op stream depend on Go's randomised map iteration and break
// reproducibility. The returned slice is freshly allocated and owned by the
// caller.
func (o *GraphOracle) NodeNames() []string {
	out := make([]string, 0, len(o.byName))
	for name := range o.byName {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// edgeStates returns a freshly-allocated slice of the modelled edges in a
// deterministic order (by source id, then destination id, then label) so the
// checker's seed-driven sampling is reproducible. The returned EdgeState values
// are copies.
func (o *GraphOracle) edgeStates() []EdgeState {
	out := make([]EdgeState, 0, len(o.edges))
	for _, e := range o.edges {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b EdgeState) int {
		if a.SrcID != b.SrcID {
			return cmp.Compare(a.SrcID, b.SrcID)
		}
		if a.DstID != b.DstID {
			return cmp.Compare(a.DstID, b.DstID)
		}
		return cmp.Compare(a.Label, b.Label)
	})
	return out
}

// nameOf returns the name property of the oracle node with the given id, or ""
// if absent. It lets the checker translate a sampled edge's endpoint ids back
// to the Person names it can probe in the engine.
func (o *GraphOracle) nameOf(id uint64) string {
	n, ok := o.nodes[id]
	if !ok {
		return ""
	}
	if name, ok := n.Properties["name"].(string); ok {
		return name
	}
	return ""
}

// String renders a compact summary of the oracle state for inclusion in a
// failure report.
func (o *GraphOracle) String() string {
	return fmt.Sprintf("GraphOracle{nodes:%d edges:%d ops:%d}", len(o.nodes), len(o.edges), len(o.ops))
}

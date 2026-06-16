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
)

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
	default:
		return o.recordOp(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled CREATE"})
	}
}

// createPerson adds a Person node. The workload binds unique names; if a name
// somehow repeats, the engine still creates a second node (CREATE is not
// idempotent), so the oracle does too and overwrites the name index to the
// latest id (the checker only samples existence, not name→id bijection).
func (o *GraphOracle) createPerson(params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: createPerson missing name"}
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

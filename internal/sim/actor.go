package sim

import (
	"fmt"
	"strings"
)

// OpKind classifies an operation so the simulator can route it to the engine's
// read or write path and the oracle to the matching Apply method.
type OpKind string

// Operation kinds.
const (
	OpCreate OpKind = "OpCreate"
	OpMatch  OpKind = "OpMatch"
	OpMerge  OpKind = "OpMerge"
	OpDelete OpKind = "OpDelete"
	OpUpdate OpKind = "OpUpdate"
	// OpMalformed is an intentionally ill-formed operation emitted by
	// [MalformedSender]. The engine must reject it with a typed error without
	// panicking, corrupting state, or applying any partial mutation; the oracle
	// models it as a no-op (it never changes modelled state).
	OpMalformed OpKind = "OpMalformed"
)

// IsWrite reports whether an operation of this kind mutates the graph and must
// therefore run through the engine's write (RunInTx) path. Malformed operations
// are routed through the write path too: it is the stricter, atomicity-bearing
// path, so proving a malformed statement is rejected there (with a full
// rollback and no partial application) is the stronger guarantee. A malformed
// read-shaped statement run through the write path still simply errors.
func (k OpKind) IsWrite() bool {
	switch k {
	case OpCreate, OpMerge, OpDelete, OpUpdate, OpMalformed:
		return true
	default:
		return false
	}
}

// Op is a single Cypher operation an actor emits: the query text, its bound
// parameters (string-keyed, value kinds limited to those toExprParams
// supports), and its kind.
type Op struct {
	Cypher string
	Params map[string]any
	Kind   OpKind
}

// Actor produces operations for the workload. An actor is stateless beyond the
// arguments to [Actor.NextOp]; all randomness comes from the supplied [Seed]
// and all knowledge of current graph contents from the supplied [GraphOracle],
// so the operation an actor emits is a pure function of (seed state, oracle
// state).
//
// # Concurrency contract
//
// Actors are NOT safe for concurrent use; they are invoked from the single
// simulation goroutine.
type Actor interface {
	// Name returns a stable identifier for the actor (used in reports).
	Name() string
	// NextOp returns the next operation to execute, drawing all randomness from
	// seed and reading current contents from oracle.
	NextOp(seed *Seed, oracle *GraphOracle) Op
}

// firstNames is the pool of name fragments the writer composes Person names
// from. Names are made unique by appending a seed-derived integer suffix, so
// the workload does not accidentally collide CREATE names while still letting
// MERGE exercise the de-duplication path on the bare fragments.
var firstNames = []string{
	"Ada", "Alan", "Grace", "Edsger", "Donald", "Barbara",
	"Tim", "Linus", "Ken", "Dennis", "Margaret", "John",
}

// HonestWriter emits valid mutating operations: it creates Person nodes, links
// existing ones with KNOWS edges, updates ages, merges by name, and detaches
// and deletes. Every edge, SET, and DELETE references a node the oracle already
// knows about, so the writer never emits a statement that the engine would
// reject on well-formedness grounds.
type HonestWriter struct{}

// Name returns the writer's identifier.
func (HonestWriter) Name() string { return "HonestWriter" }

// NextOp chooses a mutating operation. When the oracle is empty it can only
// create (there is nothing to reference yet); otherwise it picks among create,
// link, update, merge, and delete with fixed seed-driven weights.
func (w HonestWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) == 0 {
		return w.opCreatePerson(seed)
	}
	switch seed.IntN(5) {
	case 0:
		return w.opCreatePerson(seed)
	case 1:
		return w.opCreateKnows(seed, names)
	case 2:
		return w.opSetAge(seed, names)
	case 3:
		return w.opMerge(seed)
	default:
		return w.opDelete(seed, names)
	}
}

// uniqueName composes a unique Person name from a seed-chosen fragment and a
// seed-derived suffix.
func (HonestWriter) uniqueName(seed *Seed) string {
	return fmt.Sprintf("%s-%d", seed.Pick(firstNames), seed.Uint64N(1<<32))
}

func (w HonestWriter) opCreatePerson(seed *Seed) Op {
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreatePerson,
		Params: map[string]any{
			"name": w.uniqueName(seed),
			"age":  int64(seed.IntN(100)),
		},
	}
}

func (HonestWriter) opCreateKnows(seed *Seed, names []string) Op {
	a := names[seed.IntN(len(names))]
	b := names[seed.IntN(len(names))]
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreateKnows,
		Params: map[string]any{"a": a, "b": b},
	}
}

func (HonestWriter) opSetAge(seed *Seed, names []string) Op {
	return Op{
		Kind:   OpUpdate,
		Cypher: tmplSetAge,
		Params: map[string]any{
			"name": names[seed.IntN(len(names))],
			"age":  int64(seed.IntN(100)),
		},
	}
}

// opMerge merges on a bare fragment (no suffix) so MERGE genuinely exercises
// both the create-on-miss and match-on-hit branches as the same fragment recurs
// across ticks.
func (HonestWriter) opMerge(seed *Seed) Op {
	return Op{
		Kind:   OpMerge,
		Cypher: tmplMergePerson,
		Params: map[string]any{"name": seed.Pick(firstNames)},
	}
}

func (HonestWriter) opDelete(seed *Seed, names []string) Op {
	return Op{
		Kind:   OpDelete,
		Cypher: tmplDetachDelete,
		Params: map[string]any{"name": names[seed.IntN(len(names))]},
	}
}

// churnHighWater is the modelled node count at or above which a
// [BoundedChurnWriter] switches to delete-biased behaviour, and below which it
// switches to create-biased behaviour, keeping the working set near this size
// over an arbitrarily long run.
const churnHighWater = 200

// BoundedChurnWriter is an honest writer whose create/delete bias is steered by
// the current modelled node count so the graph stays BOUNDED near
// [churnHighWater] over a very long run: below the high-water mark it favours
// creates and links; at or above it, it favours deletes. It reuses
// [HonestWriter]'s well-formed statement builders, so every op it emits is a
// statement the engine accepts. It is the long-running scenario's writer.
//
// # Concurrency contract
//
// BoundedChurnWriter is NOT safe for concurrent use; it is invoked from the
// single simulation goroutine.
type BoundedChurnWriter struct{}

// Name returns the actor's identifier.
func (BoundedChurnWriter) Name() string { return "BoundedChurnWriter" }

// NextOp steers create-vs-delete by the current node count to keep the working
// set bounded. Below the high-water mark it creates (and occasionally links);
// at or above it, it deletes (and occasionally updates), so the modelled graph
// oscillates around [churnHighWater] indefinitely.
func (BoundedChurnWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	w := HonestWriter{}
	names := oracle.NodeNames()
	if len(names) == 0 {
		return w.opCreatePerson(seed)
	}
	if len(names) >= churnHighWater {
		// Delete-biased: 4/5 delete, 1/5 update, holding the count down.
		if seed.IntN(5) == 0 {
			return w.opSetAge(seed, names)
		}
		return w.opDelete(seed, names)
	}
	// Create-biased: 3/5 create, 1/5 link, 1/5 update, growing toward the mark.
	switch seed.IntN(5) {
	case 0, 1, 2:
		return w.opCreatePerson(seed)
	case 3:
		return w.opCreateKnows(seed, names)
	default:
		return w.opSetAge(seed, names)
	}
}

// readTemplates is the fixed set of read queries HonestReader rotates through.
// All are pure reads with no side effects: a projection with LIMIT, a join over
// KNOWS, a filtered aggregate, and a variable-length path length projection.
var readTemplates = []readTemplate{
	{cypher: "MATCH (n:Person) RETURN n.name, n.age LIMIT 10"},
	{cypher: "MATCH (a:Person)-[:KNOWS]->(b) RETURN a.name, b.name LIMIT 50"},
	{cypher: "MATCH (n:Person) WHERE n.age > $age RETURN count(n)", needsAge: true},
	// The variable-length path read is bounded with LIMIT so its enumeration
	// cost stays linear in the limit rather than exploding combinatorially on a
	// dense KNOWS cluster — the engine still exercises the VLE expansion plan,
	// but a supernode cannot make a single read dominate the whole run.
	{cypher: "MATCH p=(a:Person)-[:KNOWS*1..3]->(b) RETURN length(p) LIMIT 50"},
}

// readTemplate is a read query and a flag indicating whether it binds the $age
// parameter.
type readTemplate struct {
	cypher   string
	needsAge bool
}

// HonestReader emits valid read-only operations: projections, a relationship
// join, a filtered aggregate, and a bounded variable-length path query. It
// never mutates the graph, so its operations always commit with no effect on
// the oracle state.
type HonestReader struct{}

// Name returns the reader's identifier.
func (HonestReader) Name() string { return "HonestReader" }

// NextOp picks one read template at random and binds its parameters from the
// seed.
func (HonestReader) NextOp(seed *Seed, _ *GraphOracle) Op {
	tmpl := readTemplates[seed.IntN(len(readTemplates))]
	op := Op{Kind: OpMatch, Cypher: tmpl.cypher}
	if tmpl.needsAge {
		op.Params = map[string]any{"age": int64(seed.IntN(100))}
	}
	return op
}

// overloadReadTemplates are deterministic, self-contained read statements whose
// result deliberately exceeds a clamped logical-resource budget (MaxResultRows
// or MaxCollectItems): a large UNWIND, a self-Cartesian UNWIND product, a
// whole-graph collect, and a deep variable-length expansion. The first two are
// graph-independent so they over-run the row budget on every tick regardless of
// graph contents; the latter two scale with the live graph. Each is a pure read,
// so a budget refusal changes no state and the oracle stays in lock-step.
var overloadReadTemplates = []string{
	"UNWIND range(1, 5000) AS x RETURN x",
	"UNWIND range(1, 400) AS a UNWIND range(1, 400) AS b RETURN a, b",
	"MATCH (n:Person) RETURN collect(n.name)",
	"MATCH p=(a:Person)-[:KNOWS*1..4]->(b) RETURN length(p)",
}

// OverloadReader is the deterministic, engine-API counterpart of the wire-only
// [OverloadActor]: it emits over-budget READ statements (see
// [overloadReadTemplates]) to drive the engine's bounded-resource / graceful-
// degradation contract under the mem-pressure scenario. With the engine's
// logical budgets clamped low, each over-budget read is refused with a typed
// resource-exhausted error during the result drain — never a panic, never a
// partial result — and, being a read, changes no modelled state. The oracle
// records it as a no-op read, so engine and oracle stay in lock-step.
//
// # Concurrency contract
//
// OverloadReader is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type OverloadReader struct{}

// Name returns the actor's identifier.
func (OverloadReader) Name() string { return "OverloadReader" }

// NextOp returns one over-budget read, chosen by a single seed draw so the op
// stream stays a pure function of the seed.
func (OverloadReader) NextOp(seed *Seed, _ *GraphOracle) Op {
	return Op{Kind: OpMatch, Cypher: overloadReadTemplates[seed.IntN(len(overloadReadTemplates))]}
}

// malformedKindCount is the number of distinct malformed-operation families
// [MalformedSender] rotates through. It must equal the number of cases in
// [MalformedSender.NextOp]; the unit test asserts every family is reachable.
const malformedKindCount = 6

// malformedNestingDepth is the bracket-nesting depth the oversized-input family
// emits. It must exceed the parser's nesting guard (256) so the statement is
// always rejected before any execution, yet stays bounded so the harness itself
// allocates a fixed, small amount.
const malformedNestingDepth = 600

// MalformedSender is a bad actor: it emits intentionally ill-formed operations —
// invalid Cypher syntax, missing parameters, wrong parameter types,
// type-mismatched predicates, and oversized-but-bounded inputs — to assert that
// the engine rejects each with a typed error WITHOUT panicking, corrupting
// state, or applying any partial mutation. Every operation it emits is modelled
// by the oracle as a no-op ([OpMalformed]), so a clean run sees the engine error
// and the modelled state stay in lock-step (unchanged) after each one.
//
// # Concurrency contract
//
// MalformedSender is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type MalformedSender struct{}

// Name returns the actor's identifier.
func (MalformedSender) Name() string { return "MalformedSender" }

// NextOp returns one malformed operation, chosen by a single seed draw across
// the malformed families. Each family is constructed to be rejected by the
// engine for a distinct reason, so the workload exercises several rejection
// paths (parser, parameter binding, type checking, input caps) rather than one.
func (m MalformedSender) NextOp(seed *Seed, _ *GraphOracle) Op {
	switch seed.IntN(malformedKindCount) {
	case 0:
		return m.opSyntaxError(seed)
	case 1:
		return m.opMissingParam()
	case 2:
		return m.opWrongParamType(seed)
	case 3:
		return m.opTypeMismatchedPredicate()
	case 4:
		return m.opOversizedInput(seed)
	default:
		return m.opUnknownFunction()
	}
}

// opSyntaxError emits a statement the parser cannot accept (an unbalanced
// pattern). The suffix varies with the seed so the workload does not repeat one
// fixed string, but every variant is unparsable.
func (MalformedSender) opSyntaxError(seed *Seed) Op {
	return Op{
		Kind:   OpMalformed,
		Cypher: fmt.Sprintf("MATCH (n:Person RETURN n.%s", seed.Pick(firstNames)),
	}
}

// opMissingParam references $name but binds no parameters, so the engine must
// reject it as a missing parameter rather than treating the name as null.
func (MalformedSender) opMissingParam() Op {
	return Op{
		Kind:   OpMalformed,
		Cypher: "MATCH (n:Person {name:$name}) RETURN n",
		// Params intentionally nil: $name is unbound.
	}
}

// opWrongParamType binds $val (an integer) to a SET-map assignment that
// requires a Map, so the engine rejects it with a TypeError at plan time. It
// exercises both parameter binding and type checking, and the rejecting
// statement is a write (CREATE ... SET) so atomicity-on-error is exercised too:
// the leading CREATE must NOT survive the rejected SET. The integer suffix
// varies with the seed so the workload does not repeat one fixed value.
func (MalformedSender) opWrongParamType(seed *Seed) Op {
	return Op{
		Kind:   OpMalformed,
		Cypher: "CREATE (n:Person) SET n = $val",
		Params: map[string]any{"val": int64(seed.IntN(1000))},
	}
}

// opTypeMismatchedPredicate accesses a property on a non-graph literal
// (x.foo where x is an integer), which the engine rejects with a typed
// InvalidArgumentType error before any execution. The rejecting statement is a
// write so the leading binding never produces a committed node.
func (MalformedSender) opTypeMismatchedPredicate() Op {
	return Op{
		Kind:   OpMalformed,
		Cypher: "WITH 1 AS x CREATE (n:Person {name: x.foo})",
	}
}

// opOversizedInput emits a statement whose bracket nesting exceeds the parser's
// pre-parse nesting guard, exercising the engine's input caps. The depth is
// bounded by [malformedNestingDepth] so the harness allocates a fixed small
// amount, and the statement is guaranteed to be rejected before any execution,
// so — unlike a genuinely-huge-but-valid property value — it can never commit
// and leave engine and oracle out of sync.
func (MalformedSender) opOversizedInput(_ *Seed) Op {
	var b strings.Builder
	b.WriteString("RETURN ")
	for i := 0; i < malformedNestingDepth; i++ {
		b.WriteByte('(')
	}
	b.WriteByte('1')
	for i := 0; i < malformedNestingDepth; i++ {
		b.WriteByte(')')
	}
	return Op{Kind: OpMalformed, Cypher: b.String()}
}

// opUnknownFunction calls a function that does not exist, which the engine must
// reject at planning/semantic time with a typed error.
func (MalformedSender) opUnknownFunction() Op {
	return Op{
		Kind:   OpMalformed,
		Cypher: "RETURN nonExistentFunction(1, 2, 3)",
	}
}

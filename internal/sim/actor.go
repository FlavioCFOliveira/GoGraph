package sim

import "fmt"

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
)

// IsWrite reports whether an operation of this kind mutates the graph and must
// therefore run through the engine's write (RunInTx) path.
func (k OpKind) IsWrite() bool {
	switch k {
	case OpCreate, OpMerge, OpDelete, OpUpdate:
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

// readTemplates is the fixed set of read queries HonestReader rotates through.
// All are pure reads with no side effects: a projection with LIMIT, a join over
// KNOWS, a filtered aggregate, and a variable-length path length projection.
var readTemplates = []readTemplate{
	{cypher: "MATCH (n:Person) RETURN n.name, n.age LIMIT 10"},
	{cypher: "MATCH (a:Person)-[:KNOWS]->(b) RETURN a.name, b.name"},
	{cypher: "MATCH (n:Person) WHERE n.age > $age RETURN count(n)", needsAge: true},
	{cypher: "MATCH p=(a:Person)-[:KNOWS*1..3]->(b) RETURN length(p)"},
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

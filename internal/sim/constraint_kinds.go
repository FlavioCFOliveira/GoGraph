package sim

import (
	"fmt"
	"math"
)

// constraint_kinds.go — constraint-KIND adjudication for the constraint-enforce
// scenario (rmp #2455).
//
// The original scenario exercised exactly one route into a UNIQUE constraint:
// CREATE-time violation of a string key. This file extends the scenario's op
// generator with every other engine-supported route, each adjudicated by the
// same proven pattern (a local prediction that is a pure function of the op
// stream, compared per op against the engine's accept/reject outcome):
//
//   - UNIQUE violation via SET n.name = <existing> — the plain-SET path, which
//     must release the old value on a successful rename (#1905) and reject a
//     cross-node duplicate;
//   - UNIQUE violation via MERGE ... ON CREATE SET — the merge-created node
//     must NOT bypass the constraint (#1904), and a rejected MERGE applies
//     nothing (the merge-created node does not survive);
//   - a NUMERIC unique key on (Num, val) — including the int/float numeric
//     identity of #1910: a float spelling of an already-held integer value is
//     the SAME value and must be rejected;
//   - constraint-on-SET-label — the engine REJECTS `SET n:Person` when the
//     node's name would collide with a live Person's under the UNIQUE
//     constraint, and the label does not stick (the #2352 contract, pinned by
//     cypher/constraint_set_label_test.go); a non-colliding promote commits
//     and brings the node under the constraint.
//
// Every route was verified against the live engine before being modelled here
// (typed exec.ErrConstraintViolation on each rejection; nothing applied).

// Constraint-kind op templates. Fixed query texts keep each op's predicted
// outcome a pure function of (template, params, model state).
const (
	// tmplSetPersonName renames a Person; under UNIQUE (Person, name) a rename
	// onto a live Person's name must be rejected, and a successful rename frees
	// the old name.
	tmplSetPersonName = "MATCH (n:Person {name:$from}) SET n.name = $to"
	// tmplMergePersonByK always creates (the $k key is monotone and fresh), so
	// its ON CREATE SET drives the merge route into the constraint.
	tmplMergePersonByK = "MERGE (n:Person {k:$k}) ON CREATE SET n.name = $name"
	// tmplCreatePlain creates a node OUTSIDE the constrained label; it always
	// commits, even with a name a Person already holds.
	tmplCreatePlain = "CREATE (n:Plain {name:$name})"
	// tmplSetLabelPlain promotes a Plain node into the constrained Person
	// label; the engine rejects the promotion when the node's name collides.
	tmplSetLabelPlain = "MATCH (n:Plain {name:$name}) SET n:Person"
	// tmplCreateNum creates a node under the numeric UNIQUE (Num, val)
	// constraint; $val is bound as int64 or float64 (numeric identity: an
	// integer and its numerically-equal float are one value).
	tmplCreateNum = "CREATE (n:Num {val:$val})"
)

// Constraint-kind arm labels for the non-vacuity gate. Each names one
// (route, outcome) pair the run must have exercised at least once.
const (
	armCreateCommit   = "create/commit"
	armCreateReject   = "create/reject"
	armRenameCommit   = "rename/commit"
	armRenameReject   = "rename/reject"
	armMergeCommit    = "merge/commit"
	armMergeReject    = "merge/reject"
	armPlainCommit    = "plain/commit"
	armSetLabelCommit = "setlabel/commit"
	armSetLabelReject = "setlabel/reject"
	armNumCommit      = "num/commit"
	armNumReject      = "num/reject"
	armNumRejectFloat = "num/reject-float"
)

// constraintKindArms lists every arm the non-vacuity gate requires, in a fixed
// report order.
var constraintKindArms = []string{
	armCreateCommit, armCreateReject,
	armRenameCommit, armRenameReject,
	armMergeCommit, armMergeReject,
	armPlainCommit,
	armSetLabelCommit, armSetLabelReject,
	armNumCommit, armNumReject, armNumRejectFloat,
}

// constraintKindState is the scenario-local model state behind the
// constraint-kind op generator and its predictions. The Person-name routes
// predict against the shared oracle's byName set (which IS the UNIQUE value
// set: the scenario has no Person deletes, and every committed name mutation
// updates it); the numeric route keeps its own value set under float64
// numeric identity. It survives crash/recovery in-process, exactly like the
// oracle, because the constraint (engine schema) survives too.
//
// # Concurrency contract
//
// constraintKindState is NOT safe for concurrent use; it is driven from the
// single simulation goroutine.
type constraintKindState struct {
	fresh   int   // fresh Person-name counter ("c%d")
	renameN int   // fresh rename-target counter ("r%d")
	plainN  int   // fresh Plain-name counter ("pl%d")
	mergeN  int   // fresh merge-name counter ("m%d")
	mergeK  int64 // monotone MERGE key (always creates)

	// pending maps each un-promoted Plain node's name to its oracle node id.
	// At most one Plain per name (enforced at generation), so a promote
	// matches exactly one candidate besides already-promoted nodes (whose
	// re-labelling is a self no-op).
	pending      map[string]uint64
	pendingNames []string // deterministic draw order for promotes

	// numVals is the committed (Num, val) value set under numeric identity
	// (float64-canonical); numList is its deterministic draw order. isInt
	// tracks the original spelling so the dup arm can deliberately re-spell an
	// integer value as a float (#1910).
	numVals map[float64]bool
	numList []float64
	isInt   map[float64]bool
	numN    int64 // monotone fresh numeric seed

	arms map[string]int // non-vacuity counters
}

// newConstraintKindState returns an empty constraint-kind state.
func newConstraintKindState() *constraintKindState {
	return &constraintKindState{
		pending: make(map[string]uint64),
		numVals: make(map[float64]bool),
		isInt:   make(map[float64]bool),
		arms:    make(map[string]int),
	}
}

// nextOp draws the next constraint-kind op. The route mix keeps the original
// CREATE arm dominant (it also feeds the name population every other route
// draws from) and gives every new route enough draws that both of its arms
// occur within the scenario budget. All draws come from the master seed, so
// the op stream is a pure function of (seed, model state).
func (st *constraintKindState) nextOp(seed *Seed, oracle *GraphOracle) Op {
	switch seed.IntN(10) {
	case 0, 1, 2, 3:
		return st.personCreateOp(seed, oracle)
	case 4:
		return st.renameOp(seed, oracle)
	case 5:
		return st.mergeOp(seed, oracle)
	case 6:
		return st.plainCreateOp(seed, oracle)
	case 7:
		return st.setLabelOp(seed, oracle)
	default:
		return st.numCreateOp(seed)
	}
}

// personCreateOp is the original scenario arm: a fresh unique-name Person
// (~60%) or a duplicate of an existing name (~40% once names exist).
func (st *constraintKindState) personCreateOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	var name string
	if dup := len(names) > 0 && seed.Float64() < 0.4; dup {
		name = names[seed.IntN(len(names))]
	} else {
		name = fmt.Sprintf("c%d", st.fresh)
		st.fresh++
	}
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreatePerson,
		Params: map[string]any{"name": name, "age": int64(seed.IntN(100))},
	}
}

// renameOp renames an existing Person onto another live Person's name (~50%,
// must be rejected) or onto a fresh name (must commit and free the old one).
// With fewer than two names it falls back to a fresh create.
func (st *constraintKindState) renameOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) < 2 {
		return st.personCreateOp(seed, oracle)
	}
	i := seed.IntN(len(names))
	from := names[i]
	var to string
	if dup := seed.Float64() < 0.5; dup {
		// A different existing name: a self-rename is a permitted no-op, not
		// the cross-node violation this arm exists to drive.
		to = names[(i+1+seed.IntN(len(names)-1))%len(names)]
	} else {
		to = fmt.Sprintf("r%d", st.renameN)
		st.renameN++
	}
	return Op{Kind: OpUpdate, Cypher: tmplSetPersonName, Params: map[string]any{"from": from, "to": to}}
}

// mergeOp issues a MERGE that always creates (monotone fresh $k) and, via
// ON CREATE SET, writes a name that is a duplicate of a live Person's (~50%,
// must be rejected with nothing applied) or fresh (must commit).
func (st *constraintKindState) mergeOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	k := st.mergeK
	st.mergeK++
	var name string
	if dup := len(names) > 0 && seed.Float64() < 0.5; dup {
		name = names[seed.IntN(len(names))]
	} else {
		name = fmt.Sprintf("m%d", st.mergeN)
		st.mergeN++
	}
	return Op{Kind: OpMerge, Cypher: tmplMergePersonByK, Params: map[string]any{"k": k, "name": name}}
}

// plainCreateOp creates a Plain node carrying a name that is either a live
// Person's (~50%, seeding a later REJECTED promote) or fresh (seeding a later
// COMMITTED promote). It always commits — Plain is outside the constraint. At
// most one un-promoted Plain exists per name, so a promote never matches two
// violating candidates at once (which would flip a predicted commit into a
// reject).
func (st *constraintKindState) plainCreateOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	var name string
	if dup := len(names) > 0 && seed.Float64() < 0.5; dup {
		name = names[seed.IntN(len(names))]
	}
	if name == "" || st.pending[name] != 0 {
		name = fmt.Sprintf("pl%d", st.plainN)
		st.plainN++
	}
	return Op{Kind: OpCreate, Cypher: tmplCreatePlain, Params: map[string]any{"name": name}}
}

// setLabelOp promotes a seed-chosen un-promoted Plain node into the Person
// label. Whether it commits is decided at APPLY time against the then-current
// name set (a name may have been freed by a rename, or claimed since). With no
// pending Plain it falls back to creating one.
func (st *constraintKindState) setLabelOp(seed *Seed, oracle *GraphOracle) Op {
	if len(st.pendingNames) == 0 {
		return st.plainCreateOp(seed, oracle)
	}
	name := st.pendingNames[seed.IntN(len(st.pendingNames))]
	return Op{Kind: OpUpdate, Cypher: tmplSetLabelPlain, Params: map[string]any{"name": name}}
}

// numCreateOp creates a Num node under UNIQUE (Num, val): a duplicate of a
// committed value (~40%, must be rejected — an integer value is re-spelled as
// a float half the time, driving the #1910 numeric identity) or a fresh value
// (int spelling ~70%, x.5 float spelling ~30%; must commit).
func (st *constraintKindState) numCreateOp(seed *Seed) Op {
	var val any
	if dup := len(st.numList) > 0 && seed.Float64() < 0.4; dup {
		f := st.numList[seed.IntN(len(st.numList))]
		if st.isInt[f] && seed.Float64() < 0.5 {
			val = f // float64 spelling of a held integer value (#1910)
		} else if st.isInt[f] {
			val = int64(f)
		} else {
			val = f
		}
	} else if seed.Float64() < 0.3 {
		val = float64(st.numN) + 0.5
		st.numN++
	} else {
		val = st.numN
		st.numN++
	}
	if iv, ok := val.(int64); ok {
		return Op{Kind: OpCreate, Cypher: tmplCreateNum, Params: map[string]any{"val": iv}}
	}
	return Op{Kind: OpCreate, Cypher: tmplCreateNum, Params: map[string]any{"val": val.(float64)}}
}

// apply predicts op's accept/reject outcome and advances the shared oracle and
// the local state per that prediction (never per the engine outcome — the
// caller aborts the run on any disagreement, so agreed state stays exact).
// Every prediction is recorded in the oracle history like the built-in
// templates.
func (st *constraintKindState) apply(oracle *GraphOracle, op Op) OracleResult {
	switch op.Cypher {
	case tmplCreatePerson:
		return oracle.ApplyCreate(op.Cypher, op.Params)
	case tmplSetPersonName:
		return oracle.recordOp(op.Cypher, op.Params, st.applyRename(oracle, op.Params))
	case tmplMergePersonByK:
		return oracle.recordOp(op.Cypher, op.Params, st.applyMerge(oracle, op.Params))
	case tmplCreatePlain:
		return oracle.recordOp(op.Cypher, op.Params, st.applyPlainCreate(oracle, op.Params))
	case tmplSetLabelPlain:
		return oracle.recordOp(op.Cypher, op.Params, st.applySetLabel(oracle, op.Params))
	case tmplCreateNum:
		return oracle.recordOp(op.Cypher, op.Params, st.applyNumCreate(oracle, op.Params))
	default:
		return oracle.recordOp(op.Cypher, op.Params, OracleResult{ErrorMsg: "oracle: unmodelled constraint-kind op"})
	}
}

// applyRename models tmplSetPersonName: a rename onto a live Person's name is
// rejected; otherwise it commits, freeing the old name (#1905's release).
func (st *constraintKindState) applyRename(oracle *GraphOracle, params map[string]any) OracleResult {
	from, okF := paramString(params, "from")
	to, okT := paramString(params, "to")
	if !okF || !okT {
		return OracleResult{ErrorMsg: "oracle: rename missing from/to"}
	}
	id, found := oracle.byName[from]
	if !found {
		// MATCH found nothing: SET runs zero times — a committed no-op.
		return OracleResult{Committed: true}
	}
	if _, held := oracle.byName[to]; held && to != from {
		return OracleResult{Committed: false, ErrorMsg: "oracle: UNIQUE(Person.name) violation via SET"}
	}
	oracle.nodes[id].Properties["name"] = to
	delete(oracle.byName, from)
	oracle.byName[to] = id
	return OracleResult{Committed: true}
}

// applyMerge models tmplMergePersonByK: the fresh $k always creates, so the
// ON CREATE SET name decides — a duplicate is rejected with NOTHING applied
// (the merge-created node does not survive, #1904), a fresh name commits a
// full (k, name) Person.
func (st *constraintKindState) applyMerge(oracle *GraphOracle, params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: merge missing name"}
	}
	if _, held := oracle.byName[name]; held {
		return OracleResult{Committed: false, ErrorMsg: "oracle: UNIQUE(Person.name) violation via MERGE ON CREATE"}
	}
	id := oracle.nextNodeID
	oracle.nextNodeID++
	oracle.nodes[id] = &NodeState{
		ID:         id,
		Labels:     []string{"Person"},
		Properties: map[string]any{"k": params["k"], "name": name},
	}
	oracle.byName[name] = id
	return OracleResult{Committed: true, NodesCreated: 1}
}

// applyPlainCreate models tmplCreatePlain: always commits (Plain is outside
// the constraint) and registers the node as promotable.
func (st *constraintKindState) applyPlainCreate(oracle *GraphOracle, params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: plain create missing name"}
	}
	id := oracle.nextNodeID
	oracle.nextNodeID++
	oracle.nodes[id] = &NodeState{ID: id, Labels: []string{"Plain"}, Properties: map[string]any{"name": name}}
	st.pending[name] = id
	st.pendingNames = append(st.pendingNames, name)
	return OracleResult{Committed: true, NodesCreated: 1}
}

// applySetLabel models tmplSetLabelPlain per the #2352 contract: the promote
// is rejected — and the label does not stick — when a live Person holds the
// name; otherwise it commits and the node joins the constrained label (and
// the byName set, so it now blocks that name). An already-promoted node that
// still carries the Plain label re-adds Person as a self no-op, which never
// changes the outcome because at most one un-promoted Plain exists per name.
func (st *constraintKindState) applySetLabel(oracle *GraphOracle, params map[string]any) OracleResult {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: set-label missing name"}
	}
	id, pending := st.pending[name]
	if !pending {
		return OracleResult{Committed: true} // nothing to promote: no-op
	}
	if _, held := oracle.byName[name]; held {
		// Rejected: the Plain node stays un-promoted and retryable (the name
		// may be freed later by a rename).
		return OracleResult{Committed: false, ErrorMsg: "oracle: UNIQUE(Person.name) violation via SET label"}
	}
	oracle.addLabel(id, "Person")
	oracle.byName[name] = id
	delete(st.pending, name)
	for i, n := range st.pendingNames {
		if n == name {
			st.pendingNames = append(st.pendingNames[:i], st.pendingNames[i+1:]...)
			break
		}
	}
	return OracleResult{Committed: true}
}

// applyNumCreate models tmplCreateNum under numeric identity: the float64
// image of the bound value decides membership, so int 7 and float 7.0 are one
// value (#1910). A committed create registers the Num node in the shared
// oracle so node-count parity and the crash durability count stay exact.
func (st *constraintKindState) applyNumCreate(oracle *GraphOracle, params map[string]any) OracleResult {
	var f float64
	var isInt bool
	switch v := params["val"].(type) {
	case int64:
		f, isInt = float64(v), true
	case float64:
		f, isInt = v, v == math.Trunc(v)
	default:
		return OracleResult{ErrorMsg: fmt.Sprintf("oracle: num create val type %T", params["val"])}
	}
	if st.numVals[f] {
		return OracleResult{Committed: false, ErrorMsg: "oracle: UNIQUE(Num.val) violation"}
	}
	st.numVals[f] = true
	st.numList = append(st.numList, f)
	st.isInt[f] = isInt
	id := oracle.nextNodeID
	oracle.nextNodeID++
	oracle.nodes[id] = &NodeState{ID: id, Labels: []string{"Num"}, Properties: map[string]any{"val": params["val"]}}
	return OracleResult{Committed: true, NodesCreated: 1}
}

// note counts the (route, outcome) arm an executed op landed on, for the
// terminal non-vacuity gate. floatSpelled tags a rejected duplicate that was
// bound as a float64 image of a held integer value (the #1910 identity arm).
func (st *constraintKindState) note(op Op, committed bool) {
	arm := func(commit, reject string) string {
		if committed {
			return commit
		}
		return reject
	}
	switch op.Cypher {
	case tmplCreatePerson:
		st.arms[arm(armCreateCommit, armCreateReject)]++
	case tmplSetPersonName:
		st.arms[arm(armRenameCommit, armRenameReject)]++
	case tmplMergePersonByK:
		st.arms[arm(armMergeCommit, armMergeReject)]++
	case tmplCreatePlain:
		st.arms[armPlainCommit]++
	case tmplSetLabelPlain:
		st.arms[arm(armSetLabelCommit, armSetLabelReject)]++
	case tmplCreateNum:
		st.arms[arm(armNumCommit, armNumReject)]++
		if !committed {
			if _, isFloat := op.Params["val"].(float64); isFloat {
				if f := op.Params["val"].(float64); f == math.Trunc(f) && st.isInt[f] {
					st.arms[armNumRejectFloat]++
				}
			}
		}
	}
}

// checkNonVacuity is the terminal assert-something-was-seen gate: every
// constraint-kind arm — both the commit and the reject side of each route,
// including the float-spelled integer duplicate — must have occurred at least
// once, or the run proved nothing about that route.
func (st *constraintKindState) checkNonVacuity(tick int64) []Violation {
	var vs []Violation
	for _, arm := range constraintKindArms {
		if st.arms[arm] == 0 {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "constraint-kind non-vacuity",
				Message: fmt.Sprintf("constraint-kind arm %q never occurred: that route/outcome was vacuous this run", arm),
			})
		}
	}
	return vs
}

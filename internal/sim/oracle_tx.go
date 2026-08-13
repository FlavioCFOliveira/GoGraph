package sim

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// pendingEdge is a KNOWS edge decided inside a transaction, held by endpoint
// names until Commit resolves the names to parent node ids.
type pendingEdge struct {
	props map[string]any
	a, b  string
}

// OracleTx is a per-transaction workspace over a [GraphOracle], so the
// simulator can adjudicate runs where several transactions are in flight at
// once (the MVCC multi-session mode). Obtain one with [GraphOracle.BeginTx];
// finish it with exactly one [OracleTx.Commit] or [OracleTx.Abort].
//
// # Model
//
// An OracleTx mirrors what the engine promises an explicit WRITE transaction
// under MVCC (see cypher/exectx.go): each statement reads the PRESENT — the
// committed state at the instant the statement runs — plus the transaction's
// own pending writes, and nothing the transaction wrote is visible to anyone
// else until Commit publishes it as one step. The workspace therefore keeps an
// OVERLAY (pending creates, deletes, property updates, pending edges) over the
// live parent oracle, decides every statement's effect at statement time
// against parent+overlay, and folds the DECIDED effects into the parent only
// at [OracleTx.Commit] — never re-evaluating them, exactly as the engine
// publishes rather than re-runs a transaction at COMMIT.
//
// The workspace models the transactional workload's template set only, and it
// does NOT model schema constraints ([GraphOracle.SetUniqueOnName] and friends
// stay autocommit-scenario concerns): the transactional workload binds
// globally-unique names for CREATE, so the only cross-transaction name
// collision it can produce is a MERGE race, which Commit's validation refuses
// (see below).
//
// # Adjudication boundary
//
// The workspace applies decided effects; it does not decide whether the engine
// was ENTITLED to commit. When the engine refuses a transaction with a
// serialization conflict, the driver calls [OracleTx.Abort] and the workspace
// vanishes without trace. When the engine acknowledges a commit, the driver
// calls [OracleTx.Commit]; a decided effect that no longer folds cleanly (its
// target vanished from — or its MERGE-decided create appeared in — the
// committed state since the statement ran) is reported as an error and the
// parent is left UNCHANGED: the schedule let two transactions collide where
// the engine should have conflicted, and the checker above this layer owns
// that verdict.
//
// # Concurrency contract
//
// OracleTx is NOT safe for concurrent use. Like the [GraphOracle] it overlays,
// it is created, mutated, and folded from the single simulation goroutine;
// "concurrent transactions" in the deterministic mode are interleaved on that
// one goroutine, never parallel.
type OracleTx struct {
	parent *GraphOracle
	// created holds Person nodes this transaction created or merged into
	// existence, keyed by name. Their ids are allocated only at Commit, so an
	// aborted transaction consumes nothing.
	created map[string]*NodeState
	// deleted marks committed-state Person names this transaction DETACH
	// DELETEd. A name both created and deleted inside the transaction is
	// removed from created instead and never appears here.
	deleted map[string]bool
	// ageSet holds pending age updates on COMMITTED nodes, keyed by name.
	// Updates to nodes in created are applied to the created entry in place.
	ageSet map[string]any
	// edges holds pending KNOWS edges by endpoint names, deduplicated on
	// (a,b) to mirror the parent's (src,dst,label)-keyed edge map.
	edges []pendingEdge
	// ops is the transaction-local operation history. It is folded into the
	// parent's history at Commit and discarded at Abort, so replay/shrinking
	// sees exactly the operations that took effect.
	ops []OracleOp
	// finished is set by Commit and Abort; a finished workspace refuses
	// further use.
	finished bool
}

// BeginTx opens a per-transaction workspace over the oracle's committed
// state. The workspace reads the parent LIVE (present + own pending writes,
// the engine's write-transaction contract) and publishes nothing until
// [OracleTx.Commit].
func (o *GraphOracle) BeginTx() *OracleTx {
	return &OracleTx{
		parent:  o,
		created: make(map[string]*NodeState),
		deleted: make(map[string]bool),
		ageSet:  make(map[string]any),
	}
}

// record appends a statement to the transaction-local history and returns its
// predicted result, mirroring [GraphOracle.recordOp] at workspace scope.
func (t *OracleTx) record(cypher string, params map[string]any, res OracleResult) OracleResult {
	t.ops = append(t.ops, OracleOp{Cypher: cypher, Params: params, Expected: res})
	return res
}

// visible reports whether a Person of the given name is visible to this
// transaction (committed and not pending-deleted, or pending-created), along
// with the node state it would observe. The returned state is the live record
// (parent's or the overlay's) and must not be mutated by readers.
func (t *OracleTx) visible(name string) (*NodeState, bool) {
	if n, ok := t.created[name]; ok {
		return n, true
	}
	if t.deleted[name] {
		return nil, false
	}
	id, ok := t.parent.byName[name]
	if !ok {
		return nil, false
	}
	return t.parent.nodes[id], true
}

// ApplyCreate models [tmplCreatePerson] inside the transaction: the node lands
// in the overlay, invisible to every other session until Commit.
func (t *OracleTx) ApplyCreate(cypher string, params map[string]any) OracleResult {
	if t.finished {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: op on finished tx"})
	}
	if cypher != tmplCreatePerson {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled tx CREATE"})
	}
	name, ok := paramString(params, "name")
	if !ok {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: createPerson missing name"})
	}
	// A re-create after an in-tx delete resurrects the name: the entry in
	// created wins visibility (visible checks it first) while the name STAYS in
	// deleted, so the fold still removes the old committed node before the new
	// one takes the name.
	t.created[name] = &NodeState{
		Labels:     []string{"Person"},
		Properties: map[string]any{"name": name, "age": params["age"]},
	}
	return t.record(cypher, params, OracleResult{Committed: true, NodesCreated: 1})
}

// ApplyMatch models [tmplSetAge] and pure reads inside the transaction. A SET
// on a node visible to the transaction is decided now (against present + own
// writes) and folded at Commit; a miss is a committed zero-effect result.
func (t *OracleTx) ApplyMatch(cypher string, params map[string]any) OracleResult {
	if t.finished {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: op on finished tx"})
	}
	if cypher != tmplSetAge {
		// Every other MATCH the transactional workload emits is a pure read.
		return t.record(cypher, params, OracleResult{Committed: true})
	}
	name, ok := paramString(params, "name")
	if !ok {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: setAge missing name"})
	}
	if n, ok := t.created[name]; ok {
		n.Properties["age"] = params["age"] // own uncommitted node: update in place
		return t.record(cypher, params, OracleResult{Committed: true})
	}
	if _, ok := t.visible(name); !ok {
		return t.record(cypher, params, OracleResult{Committed: true}) // MATCH miss
	}
	t.ageSet[name] = params["age"]
	return t.record(cypher, params, OracleResult{Committed: true})
}

// ApplyMerge models [tmplMergePerson] inside the transaction: MERGE by name is
// a no-op when the name is visible (committed or own-pending) and a pending
// create otherwise.
func (t *OracleTx) ApplyMerge(cypher string, params map[string]any) OracleResult {
	if t.finished {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: op on finished tx"})
	}
	if cypher != tmplMergePerson {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled tx MERGE"})
	}
	name, ok := paramString(params, "name")
	if !ok {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: merge missing name"})
	}
	if _, ok := t.visible(name); ok {
		return t.record(cypher, params, OracleResult{Committed: true}) // matched; no create
	}
	// Like ApplyCreate, a MERGE-create over an in-tx-deleted name leaves the
	// name in deleted so the fold removes the old committed node first.
	t.created[name] = &NodeState{
		Labels:     []string{"Person"},
		Properties: map[string]any{"name": name, "created": true},
	}
	return t.record(cypher, params, OracleResult{Committed: true, NodesCreated: 1})
}

// ApplyDelete models [tmplDetachDelete] inside the transaction: an own-pending
// node is cancelled outright (with its pending edges); a committed node is
// marked for deletion at Commit. A miss is a committed zero-effect result.
func (t *OracleTx) ApplyDelete(cypher string, params map[string]any) OracleResult {
	if t.finished {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: op on finished tx"})
	}
	if cypher != tmplDetachDelete {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: unmodelled tx DELETE"})
	}
	name, ok := paramString(params, "name")
	if !ok {
		return t.record(cypher, params, OracleResult{ErrorMsg: "oracle: delete missing name"})
	}
	if _, wasPending := t.created[name]; wasPending {
		delete(t.created, name)
		t.dropPendingEdges(name)
		return t.record(cypher, params, OracleResult{Committed: true})
	}
	if _, ok := t.visible(name); !ok {
		return t.record(cypher, params, OracleResult{Committed: true}) // MATCH miss
	}
	t.deleted[name] = true
	delete(t.ageSet, name)
	t.dropPendingEdges(name)
	return t.record(cypher, params, OracleResult{Committed: true})
}

// ApplyCreateKnows models [tmplCreateKnows] inside the transaction: the edge is
// decided against the endpoints visible NOW (present + own pending nodes) and
// folded at Commit. Like the parent model, a missing endpoint is a committed
// zero-effect result and a duplicate (a,b) is idempotent.
func (t *OracleTx) ApplyCreateKnows(params map[string]any) OracleResult {
	if t.finished {
		return t.record(tmplCreateKnows, params, OracleResult{ErrorMsg: "oracle: op on finished tx"})
	}
	a, okA := paramString(params, "a")
	b, okB := paramString(params, "b")
	if !okA || !okB {
		return t.record(tmplCreateKnows, params, OracleResult{ErrorMsg: "oracle: createKnows missing endpoint"})
	}
	if _, ok := t.visible(a); !ok {
		return t.record(tmplCreateKnows, params, OracleResult{Committed: true})
	}
	if _, ok := t.visible(b); !ok {
		return t.record(tmplCreateKnows, params, OracleResult{Committed: true})
	}
	for _, e := range t.edges {
		if e.a == a && e.b == b {
			return t.record(tmplCreateKnows, params, OracleResult{Committed: true}) // idempotent re-create
		}
	}
	t.edges = append(t.edges, pendingEdge{a: a, b: b, props: map[string]any{}})
	return t.record(tmplCreateKnows, params, OracleResult{Committed: true, EdgesCreated: 1})
}

// dropPendingEdges removes every pending edge with the given name as either
// endpoint (the workspace half of DETACH).
func (t *OracleTx) dropPendingEdges(name string) {
	t.edges = slices.DeleteFunc(t.edges, func(e pendingEdge) bool {
		return e.a == name || e.b == name
	})
}

// HasPerson reports whether a Person of the given name is visible to this
// transaction: its own pending writes overlaid on the committed present.
func (t *OracleTx) HasPerson(name string) bool {
	_, ok := t.visible(name)
	return ok
}

// AgeOf returns the age this transaction would observe for the named Person,
// and whether the node is visible: a pending in-tx update wins over the
// committed value.
func (t *OracleTx) AgeOf(name string) (any, bool) {
	if n, ok := t.created[name]; ok {
		return n.Properties["age"], true
	}
	if age, ok := t.ageSet[name]; ok {
		return age, true
	}
	n, ok := t.visible(name)
	if !ok {
		return nil, false
	}
	return n.Properties["age"], true
}

// NodeNames returns the Person names visible to this transaction in ascending
// sorted order — committed names minus pending deletes, plus pending creates.
// The deterministic order is load-bearing for the same reason as
// [GraphOracle.NodeNames]: actors index into it with seed-derived integers.
func (t *OracleTx) NodeNames() []string {
	out := make([]string, 0, len(t.parent.byName)+len(t.created))
	for name := range t.parent.byName {
		if !t.deleted[name] {
			out = append(out, name)
		}
	}
	for name := range t.created {
		// A created name is extra only when the committed side contributes no
		// entry for it: absent there, or suppressed by this tx's own delete
		// (the resurrect case, where created wins visibility).
		if _, committed := t.parent.byName[name]; !committed || t.deleted[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// NodeCount returns the number of nodes visible to this transaction.
func (t *OracleTx) NodeCount() int {
	n := len(t.parent.nodes) - len(t.deleted)
	for name := range t.created {
		if _, committed := t.parent.byName[name]; !committed || t.deleted[name] {
			n++
		}
	}
	return n
}

// Ops returns the transaction-local operation history (folded into the parent
// at Commit, discarded at Abort). The returned slice aliases the workspace's
// backing store and must not be mutated.
func (t *OracleTx) Ops() []OracleOp { return t.ops }

// Abort discards the workspace: no pending create, delete, update, or edge
// reaches the parent, and the transaction-local history is dropped. It is
// idempotent and safe after Commit (where it is a no-op).
func (t *OracleTx) Abort() { t.finished = true }

// Commit folds the transaction's decided effects into the parent atomically:
// it VALIDATES every effect first and applies only when all of them fold
// cleanly, so a failed Commit leaves the parent byte-identical (all-or-nothing,
// like the engine's own publish step).
//
// A validation failure means the schedule let this transaction's decided
// effects collide with a commit that landed after they were decided — a
// collision the engine is expected to refuse with a serialization conflict, in
// which case the driver must call [OracleTx.Abort] instead. Commit returning
// an error is therefore a checker-level finding, not a state transition: the
// workspace stays unfinished so the caller can still Abort it.
func (t *OracleTx) Commit() error {
	if t.finished {
		return errors.New("oracle: Commit on finished tx")
	}
	p := t.parent

	// Validate: every decided effect must still fold cleanly against the
	// committed present. Nothing is mutated in this pass. Pending edges never
	// reference a name this tx deleted — ApplyDelete drops them (DETACH) — so
	// only the committed side needs checking here.
	for _, name := range slices.Sorted(maps.Keys(t.deleted)) {
		if _, ok := p.byName[name]; !ok {
			return fmt.Errorf("oracle: tx delete of %q no longer folds (name gone from committed state)", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(t.ageSet)) {
		if _, ok := p.byName[name]; !ok {
			return fmt.Errorf("oracle: tx SET age of %q no longer folds (name gone from committed state)", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(t.created)) {
		// The create was decided against a state where the name was absent (the
		// workload's CREATE names are globally unique; MERGE creates only on a
		// miss). A name that has since appeared in the committed state is a
		// MERGE race the engine must refuse — folding it would re-point the name
		// index and orphan the concurrent node. A name this tx also DELETES is
		// exempt: the fold applies the delete first, freeing the name for the
		// re-create (the in-tx resurrect case).
		if _, ok := p.byName[name]; ok && !t.deleted[name] {
			return fmt.Errorf("oracle: tx create of %q no longer folds (name appeared in committed state)", name)
		}
	}
	for _, e := range t.edges {
		for _, end := range [2]string{e.a, e.b} {
			if _, pendingHere := t.created[end]; pendingHere {
				continue // endpoint is created by this very transaction
			}
			if _, ok := p.byName[end]; !ok {
				return fmt.Errorf("oracle: tx edge (%q)->(%q) no longer folds (endpoint gone from committed state)", e.a, e.b)
			}
		}
	}

	// Apply: deletes, then creates, then updates, then edges — each loop in
	// sorted order so parent id allocation is deterministic across runs.
	for _, name := range slices.Sorted(maps.Keys(t.deleted)) {
		id := p.byName[name]
		for k := range p.edges {
			if k.src == id || k.dst == id {
				delete(p.edges, k)
			}
		}
		delete(p.nodes, id)
		delete(p.byName, name)
	}
	for _, name := range slices.Sorted(maps.Keys(t.created)) {
		n := t.created[name]
		id := p.nextNodeID
		p.nextNodeID++
		p.nodes[id] = &NodeState{ID: id, Labels: n.Labels, Properties: n.Properties}
		p.byName[name] = id
	}
	for _, name := range slices.Sorted(maps.Keys(t.ageSet)) {
		p.nodes[p.byName[name]].Properties["age"] = t.ageSet[name]
	}
	for _, e := range t.edges {
		k := edgeKey{src: p.byName[e.a], dst: p.byName[e.b], label: "KNOWS"}
		p.edges[k] = &EdgeState{SrcID: k.src, DstID: k.dst, Label: "KNOWS", Properties: e.props}
	}
	p.ops = append(p.ops, t.ops...)
	t.finished = true
	return nil
}

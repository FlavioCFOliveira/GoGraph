package cypher

// index_commit_apply.go — the commit-time secondary-index fan-out, resolved
// against the state the commit produces (rmp #2931).
//
// # The defect
//
// A transaction's index changes are buffered ([exec.IndexBuffer]) and fanned out
// once, at commit. Several of them need a fact the change does not carry: a label
// add inserts the node under its CURRENT value of the indexed property, a label
// remove deletes that value, and a property set inserts only when the node is
// ELIGIBLE (live and carrying the indexed label). The bound indexes used to answer
// those questions from the graph's PRESENT — the newest stored value — which
// includes other transactions' eager, uncommitted writes. Measured with a hash
// index on (:L, s), four steps on one node:
//
//	commit     REMOVE n:L
//	T1 (open)  SET n.s = 'polluted'
//	commit     SET n:L              ← the fan-out inserts the node under 'polluted'
//	T1         ROLLBACK
//
// Afterwards, permanently: a seek for the committed value lost the node, and a
// seek for 'polluted' returned a node that carries no such value. T1's rollback
// discards its buffer, so nothing ever corrected the entry.
//
// # The fix
//
// Reading the committing transaction's own snapshot instead would miss commits
// that landed after that snapshot was taken, so it is not the committed state
// either. The fan-out now runs as an [lpg.CommitApplier]: inside the publishing
// bracket, holding the commit-apply lock of every node shard the changes name,
// BEFORE the commit record is stamped — and, except on the WAL path, before its
// instant is even allocated — reading through a snapshot that sees every stamped
// commit and this transaction's own writes, and nothing else in flight. The
// record is then marked ready, stamped and published under the same locks. On
// the WAL path the instant was allocated before the fsync and is held, not
// ready, across the apply. See [lpg.CommitApplier] for why that yields exactly
// the committed state.
//
// The change payloads themselves (the OLD and NEW property values) are not read
// here and did not need to change: the old value is captured before the write,
// and a write that succeeds has, by the node-level write-write conflict rule
// (graph/lpg/mvcc_node_conflict.go), no other transaction's pending or
// newer-committed write on that node — so what the capture read is the committed
// value the write replaces.
//
// # The zero-cost path
//
// Only a batch that concerns an index ([index.Manager.Concerns]) registers the
// applier. Every other write statement drops its buffered changes exactly as a
// delivery to no interested subscriber would have, takes no mutex, and allocates
// nothing extra.

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// resultApplier and explicitTxApplier deliver one transaction's buffered index
// changes when the transaction is published ([lpg.CommitApplier]). They are the
// Result and the ExplicitTx that own the commit, seen through an unexported type
// so the applier methods stay off the public API — and, unlike a field, cost those
// objects no bytes: a write statement that touches no indexed property must not
// pay for the path it does not take (rmp #2931). Each runs once, on the
// publishing goroutine, and is NOT safe for concurrent use.
type (
	resultApplier     Result
	explicitTxApplier ExplicitTx
)

// CommitApplyShards implements [lpg.CommitApplier].
func (a *resultApplier) CommitApplyShards(nodes lpg.CommitNodes) uint64 {
	done := false
	defer undefinedUnlessDone(a.idxMgr, &done, "CommitApplyShards")
	shards := commitApplyShards(a.buf, nodes)
	done = true
	return shards
}

// ApplyCommitted implements [lpg.CommitApplier].
func (a *resultApplier) ApplyCommitted(snap *lpg.Snapshot) {
	done := false
	defer undefinedUnlessDone(a.idxMgr, &done, "ApplyCommitted")
	applyCommitted(a.buf, a.idxMgr, a.g, snap)
	done = true
}

// Committed implements [lpg.CommitApplier].
func (a *resultApplier) Committed(ts uint64) { a.idxMgr.FinishApplied(ts) }

// DiscardCommitted implements [lpg.CommitApplier].
func (a *resultApplier) DiscardCommitted() { a.buf.Rollback() }

// CommitApplyShards implements [lpg.CommitApplier].
func (a *explicitTxApplier) CommitApplyShards(nodes lpg.CommitNodes) uint64 {
	done := false
	defer undefinedUnlessDone(a.eng.g.IndexManager(), &done, "CommitApplyShards")
	shards := commitApplyShards(a.buf, nodes)
	done = true
	return shards
}

// ApplyCommitted implements [lpg.CommitApplier].
func (a *explicitTxApplier) ApplyCommitted(snap *lpg.Snapshot) {
	done := false
	defer undefinedUnlessDone(a.eng.g.IndexManager(), &done, "ApplyCommitted")
	applyCommitted(a.buf, a.eng.g.IndexManager(), a.eng.g, snap)
	if h := a.eng.indexDeliveredHookForTest; h != nil {
		h()
	}
	done = true
}

// undefinedUnlessDone is deferred by every applier method that can leave the
// indexes part-way through a commit: when the method is being left by a panic —
// done was never set — it records the indexes' state as undefined, which
// fail-stops the engine ([ErrEngineFailStopped]). It does not recover: the panic
// continues, and lpg settles the transaction on its way out (see
// [lpg.CommitApplier.ApplyCommitted]). A CommitApplyShards panic leaves the
// indexes untouched, but on the WAL path lpg then publishes a durable commit
// whose changes the indexes never receive, so their state is undefined all the
// same.
func undefinedUnlessDone(mgr *index.Manager, done *bool, method string) {
	if !*done {
		mgr.MarkUndefined("the commit-time index " + method + " panicked")
	}
}

// Committed implements [lpg.CommitApplier].
func (a *explicitTxApplier) Committed(ts uint64) { a.eng.g.IndexManager().FinishApplied(ts) }

// DiscardCommitted implements [lpg.CommitApplier].
func (a *explicitTxApplier) DiscardCommitted() { a.buf.Rollback() }

// commitApplyShards is every node shard a buffered change names, since that is
// every node whose state the fan-out reads and whose index entries it writes —
// except the nodes the committing transaction created, which no other
// transaction can name ([lpg.CommitNodes.Private]). It is computed from the
// buffer when lpg asks, so nothing is stored for it. A node's changes are
// buffered together, so the answer for the previous change's node is reused.
func commitApplyShards(buf *exec.IndexBuffer, nodes lpg.CommitNodes) uint64 {
	var shards uint64
	last, lastPrivate, have := graph.NodeID(0), false, false
	for _, c := range buf.Pending() {
		if !have || c.Node != last {
			last, lastPrivate, have = c.Node, nodes.Private(c.Node), true
		}
		if !lastPrivate {
			shards |= 1 << lpg.CommitApplyShard(c.Node)
		}
	}
	return shards
}

// commitNodeStates recycles the per-apply resolution state, so a commit that
// delivers index changes allocates nothing for it in steady state.
var commitNodeStates = sync.Pool{New: func() any { return new(commitNodeState) }}

// applyCommitted delivers buf's changes resolved against snap, the state the
// commit produces; see [lpg.CommitApplier.ApplyCommitted].
func applyCommitted(buf *exec.IndexBuffer, mgr *index.Manager, g *lpg.Graph[string, float64], snap *lpg.Snapshot) {
	st, ok := commitNodeStates.Get().(*commitNodeState)
	if !ok {
		st = new(commitNodeState)
	}
	st.g = g
	st.reset(snap)
	buf.CommitInState(mgr, st)
	st.g, st.snap = nil, nil
	commitNodeStates.Put(st)
}

// commitNodeState implements [index.NodeState] over the commit-apply snapshot.
//
// It memoises its answers for the batch it serves. A Cypher index is a string
// index plus its numeric companion, bound to the same (label, property), so every
// question is asked once per subscriber about the same node; the memo answers the
// second from the first, and the node's existence is looked up once for both
// questions. The memo is exact: the snapshot cannot change during the apply, and
// the memo is reset by applyCommitted before each one.
type commitNodeState struct {
	g    *lpg.Graph[string, float64]
	snap *lpg.Snapshot
	// scratch is what NodeValue returns a pointer to, so resolving a value boxes
	// nothing: the bindings' projections read it at once (see propValueOf).
	scratch lpg.PropertyValue
	memo    [commitMemoSize]commitMemoEntry
	memoN   int
}

// commitMemoSize bounds the memo. A commit touches a handful of indexed nodes;
// past this many distinct questions the memo simply stops remembering.
const commitMemoSize = 8

type commitMemoEntry struct {
	node    graph.NodeID
	coord   uint32 // label id for eligibility, property id for values
	isValue bool
	ok      bool
	value   lpg.PropertyValue
}

func (s *commitNodeState) reset(snap *lpg.Snapshot) {
	s.snap, s.memoN = snap, 0
}

func (s *commitNodeState) lookup(id graph.NodeID, coord uint32, isValue bool) (*commitMemoEntry, bool) {
	for i := 0; i < s.memoN; i++ {
		e := &s.memo[i]
		if e.node == id && e.coord == coord && e.isValue == isValue {
			return e, true
		}
	}
	return nil, false
}

func (s *commitNodeState) remember(e commitMemoEntry) {
	if s.memoN < commitMemoSize {
		s.memo[s.memoN] = e
		s.memoN++
	}
}

// NodeValue implements [index.NodeState]. It returns a pointer to scratch,
// valid until the next call, so the lookup allocates nothing.
func (s *commitNodeState) NodeValue(id graph.NodeID, propID uint32) (any, bool) {
	pv, ok := s.value(id, propID)
	if !ok {
		return nil, false
	}
	s.scratch = pv
	return &s.scratch, true
}

// NodeValueRetained implements [index.NodeState] for a caller that keeps the
// value — a build log — and so boxes a copy.
func (s *commitNodeState) NodeValueRetained(id graph.NodeID, propID uint32) (any, bool) {
	pv, ok := s.value(id, propID)
	if !ok {
		return nil, false
	}
	return pv, true
}

func (s *commitNodeState) value(id graph.NodeID, propID uint32) (lpg.PropertyValue, bool) {
	if e, hit := s.lookup(id, propID, true); hit {
		return e.value, e.ok
	}
	var pv lpg.PropertyValue
	ok := s.g.NodeExistsAsOf(id, s.snap)
	if ok {
		pv, ok = s.g.NodePropertyIDAsOf(id, lpg.PropertyKeyID(propID), s.snap)
	}
	s.remember(commitMemoEntry{node: id, coord: propID, isValue: true, ok: ok, value: pv})
	return pv, ok
}

// NodeEligible implements [index.NodeState]: live, and carrying the label.
func (s *commitNodeState) NodeEligible(id graph.NodeID, labelID uint32) bool {
	if e, hit := s.lookup(id, labelID, false); hit {
		return e.ok
	}
	ok := s.g.NodeExistsAsOf(id, s.snap) && s.g.HasNodeLabelIDAsOf(id, lpg.LabelID(labelID), s.snap)
	s.remember(commitMemoEntry{node: id, coord: labelID, ok: ok})
	return ok
}

// armIndexCommit decides how the buffered changes of the transaction wtx reach
// the indexes. A batch that concerns an index is handed to wtx as the commit
// applier a, and delivered when the transaction publishes; any other batch is
// dropped now, because no subscriber would react to it — that path takes no lock
// and enters no bracket. It returns whether the applier was registered.
//
// Without a versioned transaction there is no publication to run inside, so the
// batch is delivered at once, resolved against the present — the behaviour of a
// graph whose versioning substrate is disarmed, which has no concurrent writer.
func armIndexCommit(a lpg.CommitApplier, buf *exec.IndexBuffer, mgr *index.Manager,
	g *lpg.Graph[string, float64], wtx lpg.WriteTx,
) bool {
	if buf == nil || buf.Len() == 0 {
		return false
	}
	if !mgr.Concerns(buf.Pending()) {
		buf.Rollback()
		return false
	}
	if g == nil || !wtx.Valid() {
		buf.Commit(mgr)
		return false
	}
	wtx.SetCommitApplier(a)
	return true
}

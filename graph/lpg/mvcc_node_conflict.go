package lpg

// mvcc_node_conflict.go — cross-store write-write conflict detection for the
// NODE as a whole (rmp #2444).
//
// # The defect this closes
//
// Every per-node store detects write-write conflicts against its OWN head:
// property writes against the property shard's delta head, label writes
// against the label shard's, node removal against the existence store's. No
// store consulted a neighbour, so a DETACH DELETE of a node carrying another
// in-flight transaction's pending PROPERTY write sailed through — where a
// second property write on the same node is correctly refused — and the
// delete's eager present-state mutations (tombstone bitmap flip, label-bitmap
// strip) then destroyed state a rollback could not restore: with BOTH
// transactions rolled back, the committed node was gone from the label scan
// for good. Found by the DST multi-session mode (seed 1, tick 345) and reduced
// to a two-transaction reproduction in
// cypher.TestExplicitTx_DeleteVsPendingWriteConflicts.
//
// # The rule, and why it is race-free
//
// The unit of conflict is the NODE, exactly as a row is PostgreSQL's unit and
// a vertex is Memgraph's: a transaction that removes a node collides with any
// pending write on that node's properties or labels, and vice versa.
//
// Each side CLAIMS first in its own store (check-and-record under that shard's
// lock — the machinery every store already has), and only then CROSS-CHECKS
// the other stores' heads. That ordering is what makes the detection race-free
// without a per-node lock: if a deleter and a property writer race, the
// deleter's death claim and the writer's delta claim are each published under
// their own shard lock BEFORE either reads the other's store, so whichever
// claim lands second is seen by the first claimant's cross-check — at least
// one of the two observes the collision and is refused. (Both may be refused
// when the interleaving is tight; a spurious double-refusal is a retry, never
// an anomaly.)
//
// A refused delete has mutated NOTHING: the claim itself is a versioned record
// stamped with the refused transaction, withdrawn by the abort machinery like
// every other aborted version.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// nodeLifeHeadFor returns the effective timestamp of the latest existence
// event (birth, death) recorded for id, or zero when the node has no record.
// Safe for concurrent use; takes the life shard's read lock.
func (g *Graph[N, W]) nodeLifeHeadFor(id graph.NodeID) uint64 {
	sh := g.nodeLifeShardFor(id)
	sh.mu.RLock()
	head := sh.headStamp(id)
	sh.mu.RUnlock()
	return head
}

// nodePropHeadFor returns the effective timestamp of the newest property
// version recorded for id, or zero when the chain is empty. Safe for
// concurrent use; takes the property shard's read lock.
func (g *Graph[N, W]) nodePropHeadFor(id graph.NodeID) uint64 {
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	head := s.headStamp(id)
	s.mu.RUnlock()
	return head
}

// nodeLabelHeadFor returns the effective timestamp of the newest label version
// recorded for id, or zero when the chain is empty. Safe for concurrent use;
// takes the label shard's read lock.
func (g *Graph[N, W]) nodeLabelHeadFor(id graph.NodeID) uint64 {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	head := sh.headStamp(id)
	sh.mu.RUnlock()
	return head
}

// crossCheckNodeLife refuses a node-scoped write (property or label) whose
// target carries an existence event this transaction must not displace — a
// concurrent transaction's pending DETACH DELETE, or a delete committed after
// this transaction began. It is the symmetric half of the delete's own
// cross-check in [Graph.removeNodeInfo]; the caller has already CLAIMED its
// write in its own shard, so the ordering argument in this file's header
// applies. A nil tx (autocommit) and an unarmed graph never conflict.
func (g *Graph[N, W]) crossCheckNodeLife(id graph.NodeID, tx *writeCtx) error {
	if !g.mvccArmed || tx == nil {
		return nil
	}
	if head := g.nodeLifeHeadFor(id); tx.conflicts(head) {
		return tx.conflictErr(mvcc.StoreNodeExistence, head)
	}
	return nil
}

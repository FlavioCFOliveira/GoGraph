package lpg

// mvcc_constraintversion.go — the per-node CONSTRAINT conflict stamp (rmp #2353).
//
// # The anomaly this store exists to refuse
//
// A NOT NULL constraint is a statement about a node's WHOLE final state: it binds
// a label to a property. But conflict detection here is per SUBSTORE — node labels
// and node properties carry separate delta chains and separate head stamps — so two
// transactions can each write one half of the invariant and never meet:
//
//	CREATE CONSTRAINT ON (n:Acct) ASSERT n.email IS NOT NULL
//	n = (:Plain {email:'x'})          -- has the property, NOT the label
//	T1: REMOVE n.email                -- property substore
//	T2: SET n:Acct                    -- label substore
//	both COMMIT -> a committed node carrying :Acct with no email.
//
// Neither transaction is wrong on its own snapshot, which is why both were
// admitted: T1 sees no constrained label on the node so it has nothing to check,
// and T2's snapshot predates T1's commit so it still sees the property. The
// violation exists only in the MERGED state. This is textbook write skew — two
// adjacent anti-dependencies — which plain Snapshot Isolation permits by
// definition, and which the project's ACID CONSISTENCY mandate does not.
//
// # Why a separate stamp, and not the alternatives
//
// EVERY reference engine avoids this anomaly by making the unit of conflict WIDER
// than the individual field:
//
//   - PostgreSQL and InnoDB version the whole ROW. The label and the property live
//     in one tuple, so any two writers to that node collide on the row version and
//     first-updater-wins settles it. The anomaly is unreachable by construction.
//   - Memgraph links every write onto a SINGLE delta chain per vertex
//     (src/storage/v2/vertex.hpp), so two writers to one vertex always conflict
//     whatever they touched.
//   - Neo4j takes a node-level lock for the label and property writes a constraint
//     covers.
//
// So the reference answer is node granularity. Adopting it wholesale — Memgraph's
// shape — was REJECTED for this project, because it raises the conflict rate for
// every workload, including the overwhelming majority that declare no existence
// constraint and cannot suffer this anomaly at all. Serialising writes while any
// existence constraint is active was rejected for the same reason at a larger
// scale: it abandons write scaling on any such schema.
//
// This store is the synthesis: node-granular conflict, applied ONLY to the nodes a
// declared existence constraint actually covers. Reference-engine semantics exactly
// where the invariant needs them, substore granularity everywhere else.
//
// # Zero cost when unconstrained, by construction rather than by a fast path
//
// Nothing in this file is reached unless the caller stamps, and the caller
// (cypher's mutationUndo.touch) stamps only when the engine allocated a
// touched-node set — which it does only when the constraint registry reports at
// least one NOT NULL constraint. A schema declaring none never calls in, so the 64
// shards stay empty structs with no maps and the write path pays nothing: not an
// allocation, not a lock, not a branch beyond the nil check the touch already had.
//
// # What this store deliberately does NOT do
//
// It carries no pre-image and takes part in no rollback: it answers the conflict
// question and nothing else, exactly like [adjVersions], whose structure this
// mirrors. Its undo is its removal, which is what clearAborted is for.

import (
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// constraintVersionShards matches the 64 used by the node-property and adjacency
// stores, for the same reason: a writer touches one shard, so the shard count is
// the ceiling on concurrent constraint checks.
const constraintVersionShards = 64

// constraintStamp is one node's last constraint-relevant write.
//
// One side, not two. [adjStamps] needs a pair because an append and an exclusive
// write have different conflict rules; here every constraint-relevant write —
// a property removal, a label gain, a node creation — can combine with every other
// to produce a violation, so they all conflict with each other and one stamp says
// it.
//
// The stamp is held either as a raw timestamp (a published write) or as the
// *[commitInfo] of the transaction that made it (still in flight), as every other
// versioned store here does: an in-flight write's effective instant is its
// transaction id until the record publishes, and reading it through the record is
// what makes that transition atomic for a concurrent checker.
type constraintStamp struct {
	info *commitInfo
	ts   uint64
}

// constraintVersionShard is one lock and the nodes it covers. The map is allocated
// on first write, so a graph with no existence constraint keeps 64 empty structs
// and no maps.
type constraintVersionShard struct {
	d  map[graph.NodeID]*constraintStamp
	mu sync.Mutex
}

// constraintVersions is the per-node constraint conflict index.
//
// # Concurrency contract
//
// Safe for concurrent use. Every method takes the shard lock for the node it
// addresses and holds it only for the map access, so two writers on different
// nodes serialise only when their nodes hash to the same shard.
type constraintVersions struct {
	shards [constraintVersionShards]constraintVersionShard
	// active is how many nodes currently carry a stamp, maintained so the sweeps
	// and the stats read can SHORT-CIRCUIT instead of walking 64 shards under 64
	// locks to discover the store is empty.
	//
	// It is not an optimisation looking for a problem: it was MEASURED. Without it
	// the reclaimer's extra pass showed up as ~0.5–1 allocs/op on
	// BenchmarkWriteScaling/mem at one writer — on a workload with NO existence
	// constraint, which must pay nothing at all. -benchmem attributes allocations
	// from every goroutine in the process, the vacuum's included, so work added on
	// the sweep path lands in the write benchmark's numbers. Bisecting the change
	// set arm by arm put the cost here and not on the write path: an arm carrying
	// only the field and the store type measured identical to the base.
	//
	// [Graph.withdrawAbortedIndexRemovals] guards itself the same way with
	// idxPendingActive; this follows that precedent rather than inventing one.
	active atomic.Int64
}

// shard selects the lock covering id.
//
// The multiply-shift mix is there because node ids are dense and sequential: the
// low bits alone would send a contiguous batch of freshly created nodes — exactly
// what a bulk CREATE produces — to the same handful of shards.
func (cv *constraintVersions) shard(id graph.NodeID) *constraintVersionShard {
	h := uint64(id) * 0x9E3779B97F4A7C15
	return &cv.shards[(h>>58)%constraintVersionShards]
}

// note records that tx made a constraint-relevant write to id, and reports the
// conflict it hit, or nil.
//
// Check and record happen in ONE critical section, unlike [adjVersions] where they
// are split. There the split is forced: an append may CREATE its source node, whose
// id does not exist until the insert has run. Here the caller already holds the
// node's id — every stamping point runs after the node exists — so the pair can be
// atomic, and it MUST be: two transactions that both checked before either stamped
// would both find the slot empty and both proceed, which is the very anomaly this
// store exists to refuse.
//
// A nil tx — a direct Go-API mutation outside any transaction — never conflicts and
// records nothing: it is committed the instant it is made, so there is no window in
// which another transaction could displace it, and charging it a stamp would leave
// an instant no record will ever publish.
func (cv *constraintVersions) note(id graph.NodeID, tx *writeCtx) error {
	if tx == nil {
		return nil
	}
	sh := cv.shard(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e := sh.d[id]
	if e != nil {
		if head := adjEffective(e.info, e.ts); tx.conflicts(head) {
			return tx.conflictErr(mvcc.StoreNodeConstraint, head)
		}
	} else {
		if sh.d == nil {
			sh.d = make(map[graph.NodeID]*constraintStamp, 8)
		}
		e = &constraintStamp{}
		sh.d[id] = e
		cv.active.Add(1)
	}
	e.info, e.ts = tx.record(), tx.txID
	return nil
}

// truncate drops every stamp at or below watermark, and reports how many it
// removed.
//
// Such a stamp can no longer refuse anything — [mvcc.Conflicts] is false for a head
// below every live transaction's start — so keeping it only costs memory. Without
// this sweep the map would grow to one entry per node ever written under an
// existence constraint and stay there for the life of the process, which the
// project's bounded-resources mandate forbids. Called by the reclaimer on the same
// watermark every other store uses.
func (cv *constraintVersions) truncate(watermark uint64) (freed int) {
	if cv.active.Load() == 0 {
		return 0
	}
	for i := range cv.shards {
		sh := &cv.shards[i]
		sh.mu.Lock()
		for id, e := range sh.d {
			if adjEffective(e.info, e.ts) <= watermark {
				delete(sh.d, id)
				freed++
			}
		}
		if len(sh.d) == 0 {
			sh.d = nil
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		cv.active.Add(-int64(freed))
	}
	return freed
}

// clearAborted drops every constraint stamp an aborted transaction set, and reports
// how many entries it removed.
//
// The stamps carry no pre-image and take no part in a reader's decision, so their
// undo is their removal. [constraintVersions.truncate] cannot reach them: it
// compares against the watermark and [mvcc.AbortedTS] is above every watermark
// there can be.
func (cv *constraintVersions) clearAborted() (freed int) {
	if cv.active.Load() == 0 {
		return 0
	}
	for i := range cv.shards {
		sh := &cv.shards[i]
		sh.mu.Lock()
		for id, e := range sh.d {
			if adjEffective(e.info, e.ts) == mvcc.AbortedTS {
				delete(sh.d, id)
				freed++
			}
		}
		if len(sh.d) == 0 {
			sh.d = nil
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		cv.active.Add(-int64(freed))
	}
	return freed
}

// len reports how many nodes currently carry a constraint stamp, for observability
// and for the reclaim tests.
func (cv *constraintVersions) len() (n int) {
	if cv.active.Load() == 0 {
		return 0
	}
	for i := range cv.shards {
		sh := &cv.shards[i]
		sh.mu.Lock()
		n += len(sh.d)
		sh.mu.Unlock()
	}
	return n
}

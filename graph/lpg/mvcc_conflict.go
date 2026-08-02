package lpg

// mvcc_conflict.go — detecting write-write conflicts as versions are pushed
// (rmp #2300). Design: docs/design-write-conflict-detection.md.
//
// # Why detection RECORDS rather than RETURNS
//
// The rule is per object: a writer may push a version onto a chain only if the
// chain's current head is visible to it. The natural place to test that is the
// push itself — which is where the head is already in hand, since it is the
// value being displaced.
//
// But the push primitives return nothing, and neither do several of their
// callers (removeNodeLabelInfo, delNodePropertyInfo, and all five per-edge side
// stores). Threading an error out of each would cascade signature changes
// through the public Go API for a failure that is not per-operation at all: a
// serialization failure aborts the WHOLE transaction, not one label write.
//
// So the conflict is RECORDED on the graph and checked once, at the point that
// can act on it. That is Memgraph's shape, not a shortcut around it —
// PrepareForWrite (memgraph/memgraph, branch master, read 2026-08-02;
// src/storage/v2/mvcc.hpp) does exactly this:
//
//	transaction->has_serialization_error = true;
//	return false;
//
// It sets a flag on the transaction and returns a bool; the operation unwinds
// and the error surfaces where the flag is read. Recording is the reference
// implementation's answer to the same problem, and it keeps one definition of
// "this transaction is doomed" rather than one per call path.
//
// # What is recorded, and what is not
//
// The FIRST conflict wins and is kept. A doomed transaction may keep running
// for a few more operations before anything checks — it is already going to
// abort, so there is nothing to protect — and the first collision is the one
// that explains why, so a later one must not overwrite it.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// noteConflict records a detected write-write conflict against the transaction
// currently writing, if this is the first one.
//
// headTS is the effective timestamp of the version the write would displace.
// It returns whether a conflict was recorded, so a caller that can cheaply
// avoid doing the doomed work may skip it.
func (g *Graph[N, W]) noteConflict(store string, headTS uint64) bool {
	if !g.mvccArmed || headTS == 0 {
		return false
	}
	snap := g.writerSnap.Load()
	if snap == nil {
		// No write transaction is open: a direct Go-API mutation outside any
		// transaction is committed the instant it is made and has no snapshot
		// to be stale against.
		return false
	}
	if !mvcc.Conflicts(headTS, snap.startTS, snap.txID) {
		return false
	}
	// First conflict wins: it is the one that explains the abort.
	g.conflict.CompareAndSwap(nil, mvcc.NewConflict(store, headTS, snap.startTS, snap.txID))
	return true
}

// TakeConflict returns and clears the serialization conflict recorded against
// the current write transaction, or nil if there was none.
//
// It is taken rather than read so the next transaction on this graph cannot
// inherit it — the record is per-graph only because the write stamp still is
// (rmp #2301 moves both to per-transaction state), and a conflict outliving its
// transaction would abort an innocent one.
//
// Safe for concurrent use.
func (g *Graph[N, W]) TakeConflict() error {
	c := g.conflict.Swap(nil)
	if c == nil {
		return nil
	}
	return c
}

// ConflictPending reports whether a serialization conflict has been recorded
// against the open write transaction, without clearing it.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ConflictPending() bool { return g.conflict.Load() != nil }

// headStamp returns the effective timestamp of the newest version recorded for
// id in this label shard, or zero when the chain is empty.
//
// The caller must hold the shard lock. Zero means nothing has written this node
// since the last reclamation, which never conflicts: reclamation only frees
// versions below the watermark, and anything below the watermark is visible to
// every live transaction.
func (sh *nodeLabelShard) headStamp(id graph.NodeID) uint64 {
	d := sh.d[id]
	if d == nil {
		return 0
	}
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}

// headStamp returns the effective timestamp of the newest version recorded for
// id in this property shard, or zero when the chain is empty.
//
// The caller must hold the shard lock. See [nodeLabelShard.headStamp] for why
// zero never conflicts.
func (s *nodePropShard) headStamp(id graph.NodeID) uint64 {
	d := s.d[id]
	if d == nil {
		return 0
	}
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}

package index

// commit_gate.go — the commits whose index decision an index build must wait out
// (rmp #2936).
//
// # The defect
//
// A committing transaction decides, once, what its buffered changes owe the
// indexes: [Manager.Concerns] either drops them (nothing registered or being built
// cares) or keeps them for delivery. An index build starts recording deliveries
// ([Manager.BeginBoundBuild]) and then takes the snapshot its backfill scans. The
// two are correct together only if every commit either reaches the recording or is
// visible in the snapshot. Two shapes break that when the decision and the
// publication are not one indivisible step with respect to the build:
//
//  1. The commit decides BEFORE the build starts recording — nothing concerns its
//     changes, so they are dropped — and publishes AFTER the build's snapshot. The
//     scan cannot see it and the recording never received it.
//  2. The commit delivers BEFORE the build starts recording — to the indexes that
//     already exist — and publishes AFTER the build's snapshot. Same outcome.
//
// Either way the new index is registered without the commit's node, for good, and
// a MERGE that trusts it creates a duplicate. An autocommit statement is immune:
// it holds the schema gate shared from before its decision until after its
// publication, and a build holds it exclusively. An explicit transaction is not: it
// is an admitted store writer from BEGIN, so taking the schema gate at COMMIT would
// invert the lock order (schema gate, then writer admission) and deadlock against a
// DDL parked behind a checkpointer's quiesce, which waits for that very writer.
//
// # The fix: wait out the decisions already taken
//
// A commit that must be covered brackets its decision: [Manager.EnterCommit]
// before it, [Manager.ExitCommit] after its publication. A build, once it is
// recording, calls [Manager.AwaitCommitDecisions], which waits until every commit
// that entered BEFORE that call has exited. A commit that enters afterwards finds
// the build in flight, so [Manager.Concerns] keeps its changes and the delivery is
// recorded. The caller then waits for the visible frontier to cover every commit
// allocated so far, and only then takes the snapshot.
//
// It waits only for the commits present when the wait begins, never for a count to
// reach zero, so a stream of new commits cannot starve the build. That is the shape
// of PostgreSQL's CREATE INDEX CONCURRENTLY, which waits for the transactions
// holding conflicting locks at one instant and not for the lock to go idle
// (postgres/postgres, REL_17_STABLE, src/backend/commands/indexcmds.c,
// DefineIndex's WaitForLockers calls; src/backend/storage/lmgr/lmgr.c,
// WaitForLockersMultiple, which collects the holders once and waits for each).
//
// # The gate
//
// The bracket is a weak hold on an [mvcc.Gate] and the wait is a strong
// acquisition released at once. The gate's weak side is striped over padded
// counters, so committers touch no shared cache line (measured for the engine's
// schema gate, docs/benchmarks/mvcc-weak-strong-gate-2026-08-07.md), and its
// strong side is exactly "wait for the holders present now": it stops new weak
// holders while it drains the existing ones, then lets them through. A commit that
// enters after the wait has seen the build, because the build was appended before
// the wait began and the gate orders every weak acquisition after the strong
// release. Builds are serialised by the caller (the schema gate), so strong
// acquisitions do not queue behind one another.

import "context"

// CommitTicket is what [Manager.EnterCommit] returns and [Manager.ExitCommit]
// takes. The zero value names no entry, and exiting it is a no-op.
type CommitTicket struct{ slot int8 }

// EnterCommit records that a commit is about to take its index decision; see the
// file comment. The commit calls [Manager.ExitCommit] with the ticket once it has
// published, aborted or rolled back, on every path. It blocks only while a build
// is waiting out the commits already entered. A nil Manager returns the zero
// ticket.
//
// Safe for concurrent use.
func (m *Manager) EnterCommit() CommitTicket {
	if m == nil {
		return CommitTicket{}
	}
	return CommitTicket{slot: int8(m.decisions.WeakLockAuto() + 2)}
}

// ExitCommit ends the entry t names. It is a no-op on the zero ticket, so a
// caller may exit unconditionally.
//
// Safe for concurrent use.
func (m *Manager) ExitCommit(t CommitTicket) {
	if m == nil || t.slot == 0 {
		return
	}
	m.decisions.WeakUnlock(int(t.slot) - 2)
}

// AwaitCommitDecisions blocks until every commit that entered ([Manager.EnterCommit])
// before this call has exited, or until ctx finishes, in which case it returns
// ctx's error. A build calls it after [Manager.BeginBoundBuild] and before taking
// its snapshot; see the file comment. It waits only for the entries present when
// it starts, so it is bounded by the longest of those commits and not by the
// commit rate.
//
// Safe for concurrent use.
func (m *Manager) AwaitCommitDecisions(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if err := m.decisions.StrongLockCtx(ctx); err != nil {
		return err
	}
	m.decisions.StrongUnlock()
	return nil
}

// HoldCommitDecisions is [Manager.AwaitCommitDecisions] that keeps new entries
// out until [Manager.ReleaseCommitDecisions]: it returns once every commit that
// entered before it has exited, and every [Manager.EnterCommit] after that blocks
// until the release. On ctx's error nothing is held.
//
// It is for a build that does not record deliveries and so cannot let a commit
// decide while it runs: CREATE CONSTRAINT, whose backing index and UNIQUE value
// set are seeded from one scan (rmp #2936). The holder must not wait, while
// holding, on anything a committer blocked in EnterCommit holds — which is
// nothing, as the engine enters before taking any gate or allocating — nor on a
// store quiesce, which waits for that committer to finish.
func (m *Manager) HoldCommitDecisions(ctx context.Context) error {
	if m == nil {
		return nil
	}
	return m.decisions.StrongLockCtx(ctx)
}

// ReleaseCommitDecisions ends a successful [Manager.HoldCommitDecisions].
func (m *Manager) ReleaseCommitDecisions() {
	if m == nil {
		return
	}
	m.decisions.StrongUnlock()
}

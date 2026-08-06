package lpg

// session.go — read-your-own-writes across transactions (rmp #2328).
//
// # What a session is, and why the module needs the concept at all
//
// Until this file, GoGraph had transactions and no sessions. That was enough while
// a commit became visible before it was acknowledged — but the commit frontier is
// CONTIGUOUS, so a commit at instant T stays invisible while any earlier allocated
// timestamp is still in flight, and [Graph.ApplyVersioned] returns before T
// publishes. The caller is then told its write committed and its very next
// transaction can start BELOW T.
//
// Two symptoms were measured at the sprint-334 head, with twelve writers on
// DISJOINT keys — no contention by construction:
//
//   - 9 of 660 read-backs after an acknowledged commit did not see it;
//   - 6 of 6 write-write conflicts had a start timestamp strictly below the
//     committing goroutine's OWN previous commit timestamp. Zero conflicts at 1, 2
//     and 4 concurrent writers; 3 at 8; 25 at 12.
//
// The second is the one that misleads: an uncontended workload reports contention,
// and the conflict rate an operator reads is inflated by writer count rather than by
// real contention.
//
// A Session is the missing piece — the thing that remembers "I committed at T".
// Every database driver has one for exactly this reason, because read-your-own-writes
// is a property of a CLIENT's sequence of operations and there is nothing else for it
// to be a property of.
//
// # Where the wait lives, and what it does not cost
//
// PostgreSQL and Memgraph both publish a commit before returning from it, so their
// committer waits. GoGraph does not: that wait is on every earlier in-flight commit,
// which is the convoy rmp #2302 and rmp #2193 removed to make writes scale. Instead
// the session carries its commit instant as a FLOOR and its NEXT operation waits for
// the frontier to reach it ([mvcc.Clock.AwaitVisible]).
//
//   - a writer that does not immediately read never waits at all;
//   - a session whose commit is already visible pays one atomic load;
//   - the snapshot is still taken at a contiguous frontier point, so it can never
//     observe a state no serial order produced. A snapshot pinned ABOVE the frontier
//     could, which is why the floor is a wait and not an assignment.
//
// # The guarantee, stated
//
// Within one Session, every operation observes every commit that Session has already
// made. Across two Sessions nothing is promised beyond snapshot isolation, which is
// the same contract a connection gives in any client-server database — and the
// reason the type is exported rather than hidden.
//
// A Session is NOT a transaction. It holds no locks, pins no versions, and costs
// nothing to keep open.

import (
	"context"
	"sync/atomic"
)

// Session is one caller's sequence of operations against a graph, and the unit
// read-your-own-writes is guaranteed within.
//
// Obtain one from [Graph.NewSession]. A Session is cheap — one word of state — so a
// server may hold one per connection, and an embedded caller one per goroutine that
// writes and then reads.
//
// The zero value is not usable. Safe for concurrent use, though a Session shared
// between goroutines only promises that each of them observes every commit ANY of
// them has made through it, which is usually more coupling than a caller wants.
type Session[N comparable, W any] struct {
	g *Graph[N, W]
	// floor is the newest instant this session has committed at. Its next operation
	// waits for the frontier to reach it. Monotonic: it only ever moves forward, so
	// two goroutines sharing a session cannot pull each other's guarantee backwards.
	floor atomic.Uint64
}

// NewSession returns a session bound to this graph, with no commit to wait for.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NewSession() *Session[N, W] { return &Session[N, W]{g: g} }

// Graph returns the graph this session operates on.
func (s *Session[N, W]) Graph() *Graph[N, W] { return s.g }

// Floor returns the instant this session must observe: the newest commit it has
// made, or zero if it has made none.
//
// Exported so a caller that opens a transaction through some other path can carry
// the guarantee forward, and so a test can assert what a session pinned.
func (s *Session[N, W]) Floor() uint64 { return s.floor.Load() }

// observe records that this session committed at ts, so its next operation waits for
// ts to become visible. It never moves the floor backwards.
func (s *Session[N, W]) observe(ts uint64) {
	for {
		cur := s.floor.Load()
		if ts <= cur || s.floor.CompareAndSwap(cur, ts) {
			return
		}
	}
}

// await blocks until this session's own commits are visible, or ctx finishes.
func (s *Session[N, W]) await(ctx context.Context) error {
	if !s.g.mvccArmed {
		return nil
	}
	return s.g.mvccClock.AwaitVisible(ctx, s.floor.Load())
}

// Await blocks until the visible frontier has reached this session's floor, so a
// snapshot taken next observes every commit the session has made.
//
// It is the WAIT alone, without the snapshot [Session.BeginReadCtx] returns. A
// caller that wants the guarantee for an operation that takes its OWN snapshot — a
// query engine running a statement, say — must not hold a second one meanwhile: an
// unused snapshot still occupies a horizon slot and pins reclamation for as long as
// it is open.
//
// A session that has committed nothing returns immediately after one atomic load.
//
// Safe for concurrent use.
func (s *Session[N, W]) Await(ctx context.Context) error { return s.await(ctx) }

// BeginRead opens a read snapshot that observes every commit this session has made.
//
// It is [Graph.BeginRead] plus the session's guarantee: if this session has committed
// at an instant the frontier has not yet reached, it waits for it. The caller MUST
// pass the result to [Graph.EndRead] exactly once, exactly as with the graph's own
// form.
//
// It cannot be cancelled; use [Session.BeginReadCtx] for a caller with a deadline.
//
// Safe for concurrent use.
func (s *Session[N, W]) BeginRead() *Snapshot {
	snap, _ := s.BeginReadCtx(context.Background())
	return snap
}

// BeginReadCtx is [Session.BeginRead] with the wait bounded by ctx.
//
// When ctx finishes first it returns a nil snapshot and ctx's error, and NOTHING is
// registered — a nil snapshot is safe to pass to [Graph.EndRead], so a caller may
// still defer it unconditionally.
//
// Safe for concurrent use.
func (s *Session[N, W]) BeginReadCtx(ctx context.Context) (*Snapshot, error) {
	if err := s.await(ctx); err != nil {
		return nil, err
	}
	return s.g.BeginRead(), nil
}

// ApplyVersioned runs fn as one write transaction that observes every commit this
// session has made, and records its own commit instant on the session.
//
// It is [Graph.ApplyVersioned] plus the session's guarantee at both ends: the
// transaction waits for this session's previous commits to become visible before it
// takes its start timestamp, and publishes its own instant into the session on the
// way out.
//
// The wait is what closes the SPURIOUS SELF-CONFLICT: without it, a session's next
// transaction can start below its own previous commit, find its own version at the
// chain head and be refused with a serialization error on a key no other transaction
// ever touched.
//
// Safe for concurrent use; each goroutine should use its own session.
func (s *Session[N, W]) ApplyVersioned(fn func(WriteTx) error) error {
	return s.ApplyVersionedCtx(context.Background(), fn)
}

// ApplyVersionedCtx is [Session.ApplyVersioned] with both the frontier wait and the
// barrier acquisition bounded by ctx.
//
// Safe for concurrent use; each goroutine should use its own session.
func (s *Session[N, W]) ApplyVersionedCtx(ctx context.Context, fn func(WriteTx) error) error {
	if err := s.await(ctx); err != nil {
		return err
	}
	ts, err := s.g.applyVersionedInstant(ctx, fn)
	// Observed BEFORE the error is returned, and unconditionally. A transaction whose
	// closure returned an error still PUBLISHES at the lpg layer — that is the
	// rolled-back-statement path, where the undo log has physically restored the
	// stored value and the chain nets out (see [Graph.endWrite]). Its instant is a
	// real published instant, so a session that skipped it would be free to start its
	// next operation below a commit it made.
	s.observe(ts)
	return err
}

// BeginVersionedTx opens a multi-statement write transaction that observes every
// commit this session has made.
//
// The caller MUST close it with exactly one [Session.EndVersionedTx], which is what
// records the transaction's instant on the session. Closing it with
// [Graph.EndVersionedTx] instead publishes correctly but does NOT advance the
// session's floor, so the session loses its guarantee from that point on.
//
// Safe for concurrent use; each goroutine should use its own session.
func (s *Session[N, W]) BeginVersionedTx() (WriteTx, error) {
	return s.BeginVersionedTxCtx(context.Background())
}

// BeginVersionedTxCtx is [Session.BeginVersionedTx] with the frontier wait bounded by
// ctx. When ctx finishes first it returns the zero transaction and ctx's error, and
// no transaction is opened.
//
// Safe for concurrent use; each goroutine should use its own session.
func (s *Session[N, W]) BeginVersionedTxCtx(ctx context.Context) (WriteTx, error) {
	if err := s.await(ctx); err != nil {
		return WriteTx{}, err
	}
	return s.g.BeginVersionedTx(), nil
}

// EndVersionedTx closes a transaction opened with [Session.BeginVersionedTx] and
// records its commit instant on the session.
//
// Idempotent for the zero transaction, exactly as [Graph.EndVersionedTx] is.
func (s *Session[N, W]) EndVersionedTx(tx WriteTx) {
	s.observe(s.g.endVersionedTxInstant(tx))
}

package cypher

// session.go — the read-your-own-writes contract for a Cypher caller (rmp #2329).
//
// # What an Engine promises, and what it does not
//
// [Engine] gives every statement SNAPSHOT ISOLATION: a read observes a consistent
// instant and never a partial transaction. It promises nothing ACROSS statements. A
// caller that commits and then reads may take its snapshot at an instant the commit
// has not reached, because a commit becomes visible when the CONTIGUOUS frontier
// advances past it, and an earlier in-flight commit can hold that frontier back.
//
// That is correct snapshot isolation and it is what a database gives an unrelated
// reader. It is not what a caller expects of ITSELF, and rmp #2328 measured the two
// symptoms: a client that wrote and then read did not always see its own write, and
// a client writing repeatedly to one key was refused with a serialization conflict
// that nothing else had contended for.
//
// # Where the wait lives, and why not on the committer
//
// rmp #2328 fixed this at the substrate with [lpg.Session], which carries the
// instant a caller committed at and waits for the frontier to reach it before the
// caller's next operation takes its snapshot. This carries that up to Cypher.
//
// The wait is on the READ side deliberately. Putting it on the committer — making
// commit return only once its own instant is visible — was evaluated and rejected in
// rmp #2328: it makes every commit wait on every earlier in-flight commit, which is
// exactly the convoy rmp #2302 and rmp #2193 removed.
//
// # Why the Engine's own methods keep the sessionless contract
//
// A session's wait is a cost, and it is only worth paying for a caller that has
// something of its own to wait for. [Engine.Run] and friends are unchanged, so an
// unrelated reader — the common case on a read-mostly workload — pays nothing. A
// caller that needs the guarantee asks for it by name.
//
// This mirrors [lpg.Session] exactly, and it mirrors how every database driver
// models the same thing: a session is the unit that remembers what you did.
//
// # Concurrency
//
// A Session is safe for concurrent use, but a session shared between goroutines is
// one logical caller: they wait on each other's commits. Give each logical client
// its own, which is what the Bolt server does per connection.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Session is a Cypher caller that observes its own committed writes.
//
// Every statement run through it still gets snapshot isolation. What it adds is the
// cross-statement guarantee the bare [Engine] does not make: a read issued after
// this session's own commit observes that commit.
//
// Obtain one with [Engine.NewSession]. A Session holds no resources and needs no
// close; it is a floor timestamp and a pointer to the engine.
type Session struct {
	eng *Engine
	ls  *lpg.Session[string, float64]
}

// NewSession returns a [Session] over this engine.
//
// It is cheap — no locks, no registration, no resources to release — so a server
// may mint one per connection and discard it when the connection ends.
//
// Safe for concurrent use.
func (e *Engine) NewSession() *Session {
	if e == nil || e.g == nil {
		return &Session{eng: e}
	}
	return &Session{eng: e, ls: e.g.NewSession()}
}

// Floor reports the instant of the latest commit this session has made, or 0 when it
// has made none. It is the timestamp every subsequent operation waits for.
//
// Exported for observability and for tests that need to bracket an observation
// between two known instants; ordinary callers never need it.
func (s *Session) Floor() uint64 {
	if s == nil || s.ls == nil {
		return 0
	}
	return s.ls.Floor()
}

// Run executes a read-only statement, observing every commit this session has made.
//
// It waits for the visible frontier to reach this session's floor and then runs
// exactly as [Engine.Run] does. On a session that has committed nothing the wait is
// a single atomic load and the statement is indistinguishable from [Engine.Run].
func (s *Session) Run(ctx context.Context, query string, params map[string]expr.Value) (*Result, error) {
	if err := s.awaitOwnWrites(ctx); err != nil {
		return nil, err
	}
	return s.eng.Run(ctx, query, params)
}

// RunInTx executes a writing statement as one autocommit transaction, observing
// every commit this session has made and recording its own.
//
// Recording its own is the half that makes the NEXT operation correct: without it a
// session's guarantee would cover only the writes it made before the last one.
func (s *Session) RunInTx(ctx context.Context, query string, params map[string]expr.Value) (*Result, error) {
	return s.eng.runInTxSession(ctx, s.lpgSession(), query, params)
}

// RunAny is [Engine.RunAny] bound to this session: it binds `any`-typed parameters
// and routes to [Session.RunInTx] or [Session.Run] by whether the query writes.
//
// It is the autocommit entry point a server uses, because a wire protocol hands it
// untyped parameters and does not tell it whether the statement writes.
func (s *Session) RunAny(ctx context.Context, query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	if queryHasWritingClause(query) {
		return s.RunInTx(ctx, query, converted)
	}
	return s.Run(ctx, query, converted)
}

// BeginTx opens a multi-statement write transaction bound to this session: it
// observes every commit the session has made, and its own commit instant is recorded
// on the session when it closes.
//
// The recording happens on Commit AND on Rollback, for the reason [lpg.Session]
// documents: a rolled-back statement still publishes at the lpg layer, so its
// instant is a real published instant and a session that skipped it could start its
// next operation below a commit it made.
func (s *Session) BeginTx(ctx context.Context) (*ExplicitTx, error) {
	return s.eng.beginTxSession(ctx, s.lpgSession())
}

// BeginReadTx opens a read-only transaction bound to this session, pinned to an
// instant at or after this session's last commit.
func (s *Session) BeginReadTx(ctx context.Context) (*ExplicitTx, error) {
	return s.eng.beginReadTxSession(ctx, s.lpgSession())
}

// lpgSession returns the substrate session, or nil for a degenerate Session over an
// engine with no graph — in which case every path below falls back to the engine's
// sessionless behaviour rather than panicking.
func (s *Session) lpgSession() *lpg.Session[string, float64] {
	if s == nil {
		return nil
	}
	return s.ls
}

// awaitOwnWrites blocks until the visible frontier has reached this session's floor,
// so a snapshot taken next observes every commit the session has made.
func (s *Session) awaitOwnWrites(ctx context.Context) error {
	if s == nil || s.ls == nil {
		return nil
	}
	// Await, not BeginReadCtx: the statement below takes its own snapshot, and
	// holding a second one meanwhile would occupy a horizon slot and pin reclamation
	// for the statement's whole duration.
	return s.ls.Await(ctx)
}

// beginReadFor takes the read snapshot for an explicit read transaction, waiting for
// sess's own writes first when there is a session. Returning the session's own
// snapshot rather than taking a second one is what keeps the wait and the snapshot
// atomic: a snapshot taken after a separate wait could still be older than the floor
// if the horizon moved in between.
func beginReadFor(ctx context.Context, e *Engine, sess *lpg.Session[string, float64]) *lpg.Snapshot {
	if sess == nil {
		return e.g.BeginRead()
	}
	snap, err := sess.BeginReadCtx(ctx)
	if err != nil || snap == nil {
		// The wait was interrupted. Fall back to an unwaited snapshot rather than
		// returning nil, which every caller of this would dereference; the handle is
		// then merely as weak as a sessionless one, not broken.
		return e.g.BeginRead()
	}
	return snap
}

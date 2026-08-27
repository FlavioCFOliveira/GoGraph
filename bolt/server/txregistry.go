package server

import (
	"fmt"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TransactionInfo describes one explicit Bolt transaction that is currently
// open. It is a point-in-time snapshot taken under the registry's lock: the
// values are copies, so holding one blocks nothing and observes no later change.
//
// TransactionInfo is safe for concurrent use because it is immutable once
// returned by [Server.Transactions].
type TransactionInfo struct {
	// StartedAt is when the BEGIN completed, from the server's clock.
	StartedAt time.Time

	// ID identifies the transaction for [Server.TerminateTransaction]. It is
	// unique for the lifetime of the server: a new transaction on the same
	// connection gets a new ID, so an ID captured from an earlier listing can
	// never terminate a later transaction that happens to reuse the connection.
	ID string

	// Principal is the authenticated identity that opened the transaction, empty
	// when the server runs without authentication and the client sent no
	// principal.
	Principal string

	// Remote is the client's network address, as the server sees it.
	Remote string

	// Mode is "w" for a writing transaction or "r" for a read-only one.
	//
	// NEITHER blocks anybody as of rmp #2305/#2306. A writing transaction used to
	// hold the engine's writer serialisation and the visibility barrier for its whole
	// lifetime, so an abandoned one was an outage and finding it was urgent; it now
	// holds only its own unpublished commit record and a reclamation-horizon slot.
	// What an abandoned writing transaction still costs is therefore version memory —
	// no version it can reach is reclaimable while it lives — not other clients'
	// progress.
	Mode string

	// State is the Bolt state machine state of the owning session, rendered for a
	// human — "TX_READY" between statements, "TX_STREAMING" while a result is
	// being drained.
	State string

	// Query is the text of the most recent statement RUN inside the transaction,
	// empty when BEGIN has not yet been followed by a RUN. It is the field that
	// answers "what is it doing?", which a counter and a log line cannot.
	Query string

	// Elapsed is how long the transaction has been open, measured at snapshot
	// time. It is provided rather than left to the caller because the server's
	// clock may be injected, and subtracting StartedAt from time.Now() would then
	// be wrong.
	Elapsed time.Duration
}

// ErrNoSuchTransaction is returned by [Server.TerminateTransaction] for an ID
// that is not open — either never seen, or already ended between a listing and
// the termination call.
var ErrNoSuchTransaction = fmt.Errorf("bolt: no such open transaction")

// txEntry is the registry's mutable record for one open transaction. Only the
// registry mutates it, under its lock; the owning session hands in the values.
type txEntry struct {
	startedAt time.Time
	// terminate asks the OWNING SESSION to roll the transaction back. It does not
	// perform the rollback itself: a Session is single-threaded by contract and is
	// not safe to touch from another goroutine, so this signals the session's
	// message loop and cancels the transaction's context to interrupt any
	// statement already running. See [Session.requestTerminate].
	terminate func()
	id        string
	principal string
	remote    string
	mode      string
	state     string
	query     string
}

// txRegistry tracks the explicit transactions currently open across every
// connection, so that an operator — or the host application embedding the server
// — can see them and end one.
//
// # Why this exists
//
// The round-3 comparative audit demonstrated a whole-server stall from a single
// abandoned BEGIN and then found that, during it, an operator gets one counter
// and one log line: no session, no principal, no query text, no elapsed time and
// no way to stop it. Neo4j ships SHOW TRANSACTIONS and TERMINATE TRANSACTIONS in
// COMMUNITY, and Memgraph has both, so the gap could not be waved away as an
// Enterprise feature (rmp #2176).
//
// txRegistry is safe for concurrent use: it is read by whichever goroutine calls
// [Server.Transactions] and written by every session's message loop.
type txRegistry struct {
	clk     clock.Clock
	entries map[string]*txEntry
	mu      sync.Mutex
	seq     uint64
}

// newTxRegistry returns an empty registry using clk for start and elapsed times.
// A nil clock falls back to the real one.
func newTxRegistry(clk clock.Clock) *txRegistry {
	if clk == nil {
		clk = clock.Real()
	}
	return &txRegistry{clk: clk, entries: make(map[string]*txEntry)}
}

// nextID mints a server-unique transaction id. The sequence number is what makes
// a stale id safe: terminating an id that has since ended cannot hit whatever
// transaction the same connection opened next.
func (r *txRegistry) nextID(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return fmt.Sprintf("%s-%d", sessionID, r.seq)
}

// register records a newly opened transaction and returns nothing: the caller
// already holds the id it minted with [txRegistry.nextID].
func (r *txRegistry) register(e *txEntry) {
	if e == nil || e.id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e.startedAt = r.clk.Now()
	r.entries[e.id] = e
}

// unregister removes a transaction that has ended. It is idempotent, because a
// transaction may be torn down by more than one path.
func (r *txRegistry) unregister(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

// update refreshes the mutable, human-facing fields of an open transaction. An
// unknown id is ignored, so a session need not check whether its transaction is
// still registered before reporting progress.
func (r *txRegistry) update(id, state, query string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return
	}
	e.state = state
	if query != "" {
		e.query = query
	}
}

// list snapshots every open transaction, ordered oldest first so the one most
// likely to be blocking others comes first.
func (r *txRegistry) list() []TransactionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clk.Now()
	out := make([]TransactionInfo, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, TransactionInfo{
			ID:        e.id,
			Principal: e.principal,
			Remote:    e.remote,
			Mode:      e.mode,
			State:     e.state,
			Query:     e.query,
			StartedAt: e.startedAt,
			Elapsed:   now.Sub(e.startedAt),
		})
	}
	// Insertion sort: the list is bounded by open transactions, which the writer
	// serialisation and Options.MaxConnections keep small.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartedAt.Before(out[j-1].StartedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// terminate asks the owning session to roll back the transaction with the given
// id, returning [ErrNoSuchTransaction] when it is not open.
//
// The request is asynchronous by necessity: the rollback runs on the owning
// session's own goroutine, because a Session is not safe for concurrent use. The
// entry is left in place for that goroutine to remove, so a second terminate for
// the same id is harmless.
func (r *txRegistry) terminate(id string) error {
	r.mu.Lock()
	e, ok := r.entries[id]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchTransaction, id)
	}
	if e.terminate != nil {
		e.terminate()
	}
	return nil
}

// Transactions returns a snapshot of every explicit transaction currently open
// on this server, oldest first.
//
// It is the diagnostic half of the pair [Server.TerminateTransaction] completes.
//
// The reason it matters CHANGED with rmp #2305/#2306, and the old reason is worth
// stating so nobody restores it: an open writing transaction used to hold the
// engine's writer serialisation and the visibility barrier for its whole lifetime,
// so "while one is open every reader waits" was literally true and an abandoned
// transaction was an outage. It no longer holds either. What an abandoned
// transaction pins now is the reclamation horizon — no version it could still read
// is freed while it lives — so the symptom is unbounded version memory rather than
// stalled clients, and finding it still requires knowing its principal, its age and
// its current statement.
//
// Transactions is safe to call from any goroutine at any time, including while
// the server is serving. The returned slice and its elements are copies.
func (s *Server) Transactions() []TransactionInfo {
	if s.txReg == nil {
		return nil
	}
	return s.txReg.list()
}

// TerminateTransaction rolls back the open transaction with the given id. It
// returns [ErrNoSuchTransaction] if no such transaction is open.
//
// It releases NO writer serialisation and no visibility barrier — rmp #2305/#2306
// retired both, exactly as [Server.Transactions] above says at length. This line
// claimed otherwise until rmp #2560, contradicting its own neighbour twenty lines
// up. What the rollback reclaims is the transaction's unpublished commit record and
// its reclamation-horizon slot.
//
// The terminated connection is told so on its next request-phase message:
// [Session.terminateTxByOperator] arms Neo.ClientError.Transaction.Terminated,
// which is deliberately distinct from the code an expired bound produces, so an
// operator termination is not reported to the client as a timeout that never
// happened.
//
// The rollback is performed by the connection that owns the transaction, on its
// own goroutine, because a [Session] is single-threaded by contract. This call
// therefore REQUESTS the rollback and returns once the request is delivered; the
// transaction's context is cancelled synchronously, so a statement already
// executing is interrupted immediately, and the rollback follows as soon as the
// owning loop observes the request. Use [Server.Transactions] to confirm it has
// gone.
//
// The rollback is atomic: it unwinds every statement of the transaction, exactly
// as a client ROLLBACK would, so no partial state is left behind.
//
// TerminateTransaction is safe to call from any goroutine.
func (s *Server) TerminateTransaction(id string) error {
	if s.txReg == nil {
		return fmt.Errorf("%w: %q", ErrNoSuchTransaction, id)
	}
	return s.txReg.terminate(id)
}

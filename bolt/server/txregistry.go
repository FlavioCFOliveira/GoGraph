package server

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TransactionInfo describes one explicit Bolt transaction that is currently
// open. The values are copies, so holding one blocks nothing and observes no
// later change.
//
// Every field of ONE TransactionInfo describes one instant: [Server.Transactions]
// takes ID, Principal, Remote, Mode and StartedAt from fields that never change
// after the transaction opens, and takes State and Query together with a single
// atomic load. A State and a Query that appear side by side here really were
// simultaneously true. Two DIFFERENT entries of the same listing need not share
// an instant — see the consistency note on [Server.Transactions].
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

// txStatus is the pair of human-facing fields that CHANGE while a transaction is
// open. It is immutable once published: a change publishes a replacement rather
// than editing the value in place, which is what lets a reader take both fields
// together with one atomic load and no lock at all.
//
// The two are one value rather than two because they are read together and must
// agree: a listing that showed a State from one instant beside a Query from
// another would describe a transaction that never existed. See the consistency
// note on [Server.Transactions].
type txStatus struct {
	state string
	query string
}

// txEntry is the registry's record for one open transaction.
//
// # Which fields are fixed and which move
//
// Everything except status is written once, before the entry is published into
// the registry's map under its lock, and never again — so a reader that holds
// the map lock reads them safely and they are exact.
//
// status is the exception: it changes as the transaction runs. It is written by
// exactly ONE goroutine, the owning session's message loop (a [Session] is
// single-threaded by contract), and read by whoever calls [Server.Transactions].
// That single-writer discipline is what makes the read-compare-store in
// [txEntry.setStatus] sound without a lock, and it is a real invariant of this
// package rather than a hope: [Session.reportTx] and [Session.registerTx] are the
// only writers and both run on that loop, while [Session.requestTerminate] — the
// one entry point that runs on a foreign goroutine — touches nothing here.
// txEntrySize is what one entry is padded to, and it is a cache-line decision
// rather than an arbitrary round number.
//
// An entry is 112 bytes without the padding, which lands it in Go's 112-byte size
// class. Allocate several in quick succession — a burst of concurrent BEGINs does
// exactly that — and consecutive entries are 112 bytes apart in one span, so two
// of them share a cache line roughly seven times in eight on a machine with
// 128-byte lines. Every message on either transaction then dirties a line the
// other's owner is reading, and the two sessions ping-pong it between cores for
// no reason at all: they share no data, only an accident of address.
//
// MEASURED (rmp #2714, Apple M4, hw.cachelinesize 128, no -race, 4096 entries and
// one goroutine per entry): unpadded, the per-message refresh scaled 1.79x from 1
// to 8 goroutines; with one entry per 4KB page it scaled 5.99x, against a floor of
// 5.80x for a private non-atomic increment on the same ladder. The whole of that
// 3.3x was address collision.
//
// 128 covers arm64's 128-byte line and x86-64's 64-byte line, and Go's 128-byte
// size class is 128-aligned, so consecutive entries land on consecutive lines. The
// cost is 16 bytes per OPEN transaction, which Options.MaxOpenTxPerPrincipal
// already bounds.
const txEntrySize = 128

// These two declarations are a two-sided compile-time assertion that txEntry is
// exactly txEntrySize bytes. Add a field without shrinking the padding and the
// first fails to compile; remove one without growing it and the second does. A
// silent drift back to a size class that shares lines is what they exist to stop.
var (
	_ [txEntrySize - unsafe.Sizeof(txEntry{})]byte
	_ [unsafe.Sizeof(txEntry{}) - txEntrySize]byte
)

type txEntry struct {
	// status and spare come FIRST and are the only fields written after the entry
	// is published. Keeping them at the head of a txEntrySize-aligned object is
	// what puts one entry's hot pair on a line of its own; see [txEntrySize].
	//
	// status holds the mutable pair. It is never nil: [newTxEntry] is the only
	// constructor and [txRegistry.register] refuses an entry whose status is unset,
	// so no reader has to check.
	status atomic.Pointer[txStatus]

	// spare is the value status held immediately BEFORE its current one, kept so
	// that an alternation between two pairs allocates nothing after the first
	// cycle. See [txEntry.setStatus] for why republishing it is safe.
	//
	// It is written and read by the owning session's message loop alone — no
	// reader ever touches it — so it is a plain field and not an atomic one.
	spare *txStatus

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

	// pad rounds the entry up to txEntrySize. It is never read.
	_ [txEntrySize - 112]byte
}

// newTxEntry builds an entry with its fixed fields and its initial status. It is
// the only constructor: the status pointer must be armed before the entry can be
// published, and going through here is what guarantees that.
func newTxEntry(id, principal, remote, mode, state string, terminate func()) *txEntry {
	e := &txEntry{
		terminate: terminate,
		id:        id,
		principal: principal,
		remote:    remote,
		mode:      mode,
	}
	e.status.Store(&txStatus{state: state})
	return e
}

// setStatus refreshes the entry's mutable pair, taking no lock of any kind. An
// empty query means "unchanged", which is how the message loop reports a message
// that is not a RUN and therefore says nothing new about the statement.
//
// It publishes NOTHING when neither field moves. That is not merely an
// optimisation: it is the answer to "is the per-message refresh needed at all?".
// The refresh is needed as a CHECK — only the message loop learns the new state,
// and it learns it once per message — but not as a WRITE. In a minimal
// BEGIN/RUN/PULL/COMMIT transaction one of the three refreshes restates what
// register already recorded, and a multi-PULL stream restates TX_STREAMING once
// per chunk; every one of those now costs a load and a comparison instead of an
// allocation and a store.
//
// # Why it allocates nothing in the shape the message loop actually produces
//
// One RUN and the PULLs that drain it alternate the state between TX_STREAMING
// and TX_READY while the statement text stays put, so the entry oscillates
// between exactly TWO pairs. spare holds the one that is not current, and a
// published txStatus is immutable FOREVER — nothing ever writes through that
// pointer again — so putting the old value back is not merely cheap, it is
// indistinguishable from publishing a fresh copy of it, even to a reader that is
// holding it at that instant.
//
// A transaction that runs a genuinely new statement each time defeats the reuse
// and allocates one 32-byte txStatus per publish, which is what
// BenchmarkTxBookkeeping_RegistryUpdateNewQuery prices. That is the honest worst
// case and it is still one small allocation against a whole round trip.
//
// # Why the read-compare-store is not a race
//
// Only the owning session's message loop writes an entry, so no second writer can
// slip between the Load and the Store. Readers only Load, and none of them can
// see spare at all. See the single-writer note on [txEntry].
func (e *txEntry) setStatus(state, query string) {
	cur := e.status.Load()
	if query == "" {
		query = cur.query
	}
	if cur.state == state && cur.query == query {
		return
	}
	next := e.spare
	if next == nil || next.state != state || next.query != query {
		next = &txStatus{state: state, query: query}
	}
	e.spare = cur
	e.status.Store(next)
}

// loadStatus returns the entry's mutable pair as it was at one instant, with one
// atomic load. The two fields always belong to the same instant; see [txStatus].
func (e *txEntry) loadStatus() (state, query string) {
	s := e.status.Load()
	return s.state, s.query
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
// # What the mutex covers, and what it deliberately does not
//
// r.mu covers the MEMBERSHIP of the map — which transactions are open — and the
// id sequence. It is taken on BEGIN (nextID, register), on transaction end
// (unregister) and by a listing, so at most three times per transaction plus
// once per operator query.
//
// It is NOT taken to refresh a transaction's state or current statement. That
// happens after every inbound Bolt message while a transaction is open, and
// routing it through one process-global mutex made the highest-frequency
// operation in the registry the one that scaled worst: measured in isolation it
// went from 8.3 ns at one goroutine to 77.5 ns at eight, a 9.36x degradation
// (rmp #2714). The mutable pair now lives in an atomic pointer on the entry
// itself and the owning session holds that entry directly, so a refresh touches
// one cache line that no other session shares and takes no lock. What that costs
// a listing is stated on [Server.Transactions].
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
	// The status guard enforces the invariant [txEntry] states rather than leaving
	// a later `&txEntry{...}` literal to trip a reader over a nil pointer: an entry
	// that did not come from [newTxEntry] is never published.
	if e == nil || e.id == "" || e.status.Load() == nil {
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

// list snapshots every open transaction, ordered oldest first so the one most
// likely to be blocking others comes first.
func (r *txRegistry) list() []TransactionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clk.Now()
	out := make([]TransactionInfo, 0, len(r.entries))
	for _, e := range r.entries {
		// One atomic load per entry, so State and Query belong to the same instant
		// even though the entry's owner may be refreshing them right now. Across
		// entries they need not: see the consistency note on [Server.Transactions].
		state, query := e.loadStatus()
		out = append(out, TransactionInfo{
			ID:        e.id,
			Principal: e.principal,
			Remote:    e.remote,
			Mode:      e.mode,
			State:     state,
			Query:     query,
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
// # Concurrency and consistency contract
//
// Transactions is safe to call from any goroutine at any time, including while
// the server is serving. The returned slice and its elements are copies, and
// nothing the caller does with them can affect the server.
//
// A listing is NOT a single global instant, and that is a deliberate trade rather
// than an oversight. It is consistent PER ENTRY and approximate ACROSS entries:
//
//   - Within one [TransactionInfo] every field belongs to one instant. ID,
//     Principal, Remote, Mode and StartedAt are fixed when the transaction opens
//     and never move; State and Query are read together with one atomic load, so
//     the pair is always one the transaction genuinely passed through.
//   - Across two entries the instants may differ by however long the listing
//     takes to walk from one to the other. A listing can therefore show a
//     combination of per-transaction states that never held simultaneously across
//     the whole server. The SET of transactions listed is exact — membership is
//     read under the registry's lock — so an entry is never invented, duplicated
//     or lost; only the moving fields of different entries may be skewed.
//
// What that bought: refreshing a transaction's state used to take a process-global
// mutex after EVERY inbound Bolt message, which measured 8.3 ns at one goroutine
// and 77.5 ns at eight — the registry's most frequent operation was also its
// worst-scaling one (rmp #2714). It now takes no lock.
//
// The weaker guarantee is sound for what this listing is for. Deciding that a
// transaction is abandoned, or long-running, or the one to terminate, is a
// judgement about ONE transaction, and each entry is internally exact. Do NOT
// build a cross-transaction invariant on a listing — "these two were in
// TX_STREAMING at the same moment" is not something a listing can establish, and
// it could not establish it before this change either, because the transactions
// were free to move on the instant the lock was released.
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

package server

import (
	"fmt"
	"sync"
)

// txQuota caps how many explicit transactions one authenticated principal may
// hold open across all of its connections at once.
//
// # Why it exists
//
// The round-3 comparative audit demonstrated an outage: one authenticated client
// sent BEGIN and stopped talking, and a 4.7 ms read became 30.001 s followed by a
// hard TransactionTimedOut — repeatable indefinitely, because bolt/ had no
// per-principal or per-IP limit of any kind (rmp #2175). At the time an open
// explicit transaction held the engine's global visibility barrier for its whole
// life, so every reader on every connection stalled behind it.
//
// That hold is GONE. rmp #2306 retired the writer serialisation BEGIN used to
// take — Engine.beginTxSession acquires none, and concurrency control is MVCC
// with per-object conflict detection (cypher/exectx.go) — and rmp #2305/#2274
// retired the read and write visibility barriers a transaction held across client
// think-time. An abandoned transaction therefore blocks neither readers nor
// writers today.
//
// What it still costs is RESOURCES, and that is what this cap now bounds. Every
// open transaction pins the reclamation horizon, so no version it could still
// read is freed while it lives, and it occupies one registry entry and one slot
// here. The companion bound is the idle reaper (Options.MaxTxIdleTime), which
// shortens how long any one of them lives; this one limits how many a single
// principal can hold at once, so the exposure does not simply move from duration
// to count.
//
// # Bounded by construction
//
// The map holds one entry per principal with at least one OPEN transaction, and
// an entry is deleted the moment its count returns to zero. Its size is
// therefore bounded by the number of concurrently open transactions, which is
// itself bounded by Options.MaxConnections — a client cannot grow it by
// authenticating under many names, because a name with no open transaction
// occupies nothing.
//
// txQuota is safe for concurrent use: unlike a Session it is shared by every
// connection.
type txQuota struct {
	open  map[string]int
	mu    sync.Mutex
	limit int
}

// newTxQuota returns a quota enforcing limit concurrently-open transactions per
// principal. A limit <= 0 disables enforcement, and the returned quota then
// records nothing at all rather than counting into a map nobody reads.
func newTxQuota(limit int) *txQuota {
	if limit <= 0 {
		return &txQuota{limit: 0}
	}
	return &txQuota{limit: limit, open: make(map[string]int)}
}

// errTxQuotaExceeded reports that a principal already holds its maximum number
// of concurrently open transactions.
type errTxQuotaExceeded struct {
	principal string
	limit     int
}

func (e *errTxQuotaExceeded) Error() string {
	return fmt.Sprintf("principal %q already holds the maximum of %d concurrently open transactions",
		e.principal, e.limit)
}

// acquire reserves one open-transaction slot for principal, or returns
// *errTxQuotaExceeded when it already holds limit of them. A disabled quota
// always succeeds.
//
// Every successful acquire must be paired with exactly one [txQuota.release].
func (q *txQuota) acquire(principal string) error {
	if q.limit <= 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.open[principal] >= q.limit {
		return &errTxQuotaExceeded{principal: principal, limit: q.limit}
	}
	q.open[principal]++
	return nil
}

// release returns one slot. It is safe to call for a principal that holds none —
// the teardown paths are deliberately idempotent — and deletes the entry once
// the count reaches zero so the map stays bounded by open transactions rather
// than by principals ever seen.
func (q *txQuota) release(principal string) {
	if q.limit <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	n := q.open[principal]
	if n <= 1 {
		delete(q.open, principal)
		return
	}
	q.open[principal] = n - 1
}

// openFor reports how many transactions principal currently holds. It exists for
// tests and for the metrics surface; production paths use acquire/release.
func (q *txQuota) openFor(principal string) int {
	if q.limit <= 0 {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.open[principal]
}

// tracked reports how many principals currently hold at least one open
// transaction — the map's live size, which the bounded-resources contract
// requires to stay proportional to open transactions and not to principals seen.
func (q *txQuota) tracked() int {
	if q.limit <= 0 {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.open)
}

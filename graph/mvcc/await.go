package mvcc

// await.go — waiting for the visible frontier to reach a known instant (rmp #2328).
//
// # The problem this exists for, measured
//
// The frontier is CONTIGUOUS: a commit at ts=100 stays invisible while any earlier
// allocated timestamp is still in flight. So a transaction that commits at 100 and
// returns success to its caller has NOT necessarily made 100 visible, and the
// caller's next snapshot can start below its own commit. Two symptoms were measured
// at the sprint-334 head, with 12 writers on DISJOINT keys — no contention by
// construction:
//
//   - 9 of 660 read-backs after an acknowledged commit did not see it;
//   - 6 of 6 write-write conflicts had a start timestamp strictly below the
//     committing goroutine's OWN previous commit timestamp. Zero conflicts at 1, 2
//     and 4 writers; 3 at 8; 25 at 12. That rate tracks the number of concurrent
//     writers, which is the frontier-lag signature, not a per-object contention one.
//
// # Where the wait goes, and why not on the committer
//
// PostgreSQL and Memgraph both make a commit visible BEFORE Commit returns —
// ProcArrayEndTransaction runs inside CommitTransaction, and Memgraph marks the
// transaction committed under the engine lock before returning. Doing the same here
// would mean every committer waits for every EARLIER in-flight commit, which is
// precisely the convoy rmp #2302 and rmp #2193 removed to make writes scale.
//
// So the wait is on the READ side: a session that has committed carries its own
// commit instant as a FLOOR, and its next snapshot waits for the frontier to reach
// it. Three properties follow, and they are why this placement was chosen:
//
//   - a writer that does not immediately read never waits, so write throughput is
//     untouched;
//   - the snapshot is still taken at a contiguous frontier point, so it can never
//     observe a state no serial order produced — which a snapshot pinned ABOVE the
//     frontier could;
//   - the cost falls exactly on the caller that needs the guarantee.
//
// # The mechanism costs nothing when nobody is waiting
//
// A waiter registers itself, and the publisher wakes waiters only while the counter
// is non-zero. On the overwhelmingly common path — a commit with no session blocked
// on it — the publish path pays one atomic load.

import "context"

// waitGate is the broadcast a waiter blocks on. It is closed and replaced whenever
// the frontier advances while at least one waiter is registered, so a waiter that
// took the channel before re-checking the frontier cannot miss the advance.
type waitGate struct {
	ch chan struct{}
}

// gate returns the current broadcast channel, installing one on first use.
func (c *Clock) gate() chan struct{} {
	if g := c.wait.Load(); g != nil {
		return g.ch
	}
	fresh := &waitGate{ch: make(chan struct{})}
	c.wait.CompareAndSwap(nil, fresh)
	return c.wait.Load().ch
}

// wakeWaiters closes the current broadcast channel and installs a fresh one.
//
// Called from [Clock.finishCommitTS] after the frontier has been stored, and only
// while waiters are registered. The store must happen first: a waiter woken by this
// close re-reads the frontier, and reading it before the store would send the waiter
// straight back to sleep on the NEW channel, missing the advance it was waiting for.
func (c *Clock) wakeWaiters() {
	fresh := &waitGate{ch: make(chan struct{})}
	if old := c.wait.Swap(fresh); old != nil {
		close(old.ch)
	}
}

// AwaitVisible blocks until every commit at or below floor is visible — that is,
// until [Clock.ReadTS] would return floor or more — or until ctx finishes.
//
// It returns nil once the frontier has reached floor, and ctx's error otherwise. A
// floor of zero, or one the frontier has already passed, returns immediately without
// registering anything.
//
// # It is bounded by the transactions it waits on, not by this call
//
// The frontier advances when the in-flight commits below floor finish, and each of
// those is a transaction that is either committing or aborting — both of which
// discharge their timestamp ([Clock.PublishCommitTS], [Clock.AbandonCommitTS]). So
// the wait terminates unless a transaction never discharges its timestamp at all,
// which is the permanent-frontier-stall condition that MVCCStats.InFlightCommits
// exists to report and that no path in the module is allowed to create. A caller
// that cannot tolerate an unbounded wait passes a ctx with a deadline, which is why
// this takes one.
//
// Safe for concurrent use.
func (c *Clock) AwaitVisible(ctx context.Context, floor uint64) error {
	// The fast path, and the one that decides whether this mechanism is affordable:
	// a session whose commit is already visible — every session on a graph with one
	// writer — pays a single atomic load and never registers.
	if floor == 0 || c.visible.Load() >= floor {
		return nil
	}
	c.waiters.Add(1)
	defer c.waiters.Add(-1)
	for {
		// The channel is taken BEFORE the frontier is re-read. The reverse order
		// loses an advance that lands between the two: the waiter would read a stale
		// frontier and then block on a channel that has already been closed and
		// replaced.
		ch := c.gate()
		if c.visible.Load() >= floor {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// AwaitQuiescent blocks until no allocated commit timestamp remains unpublished —
// until [Clock.InFlightCommits] would report zero — or until ctx finishes.
//
// # What it is for: the durability/visibility boundary (rmp #2349)
//
// Committing is two steps, and this module deliberately does NOT hold one lock
// across both: a timestamp is allocated, the WAL record carrying it is fsynced, and
// only afterwards is the timestamp published. Between the fsync and the publish a
// transaction is DURABLE BUT INVISIBLE, and any observer that reads a durability
// position and a visibility position at that moment gets two numbers describing
// DIFFERENT sets of transactions.
//
// A checkpoint is exactly such an observer, and for it the disagreement is
// unrecoverable: it takes the durable WAL offset as the prefix its image folds, and
// the image itself at a visible instant. A transaction inside the window is below
// the offset and absent from the image, so truncating that prefix discards the only
// record of an acknowledged commit.
//
// This is the wait that closes the window, and it is PostgreSQL's answer to the same
// problem. A backend there raises DELAY_CHKPT_START before inserting its commit
// record and clears it after updating pg_xact (src/backend/access/transam/xact.c,
// commit b5978350, lines 1469-1471 and 1577-1582), and CreateCheckPoint spins until
// no backend is inside that window (src/backend/access/transam/xlog.c:7695-7712)
// before it moves on. Its own comment names the cause — "xact.c does commit record
// XLOG insertion and clog update as two separate steps protected by different
// locks, but again that seems best on grounds of minimizing lock contention"
// (xlog.c:7684-7687) — and states the trade-off it chose: "it seems better to make
// checkpoint take a bit longer than to hold off insertions longer than necessary".
// That is the trade-off taken here too, which is why the wait is on the OBSERVER
// and the commit path pays nothing.
//
// Memgraph reaches the same property by the other route, and the contrast is why it
// was not copied: InMemoryStorage::CreateTransaction loads the start timestamp and
// the last durable timestamp under ONE acquisition of engine_lock_
// (src/storage/v2/inmemory/storage.cpp:2833-2844, commit b3ac3cdc), so its snapshot
// reads a mutually consistent pair by construction. That works because its commit
// publishes durability and visibility under the same engine lock — which is the
// convoy rmp #2302 and rmp #2193 removed here to make writes scale.
//
// # Termination
//
// It terminates once allocation stops, since every allocated timestamp is discharged
// by [Clock.PublishCommitTS] or [Clock.AbandonCommitTS]. A caller that observes it
// while allocation continues may loop indefinitely by design — that is the caller's
// to bound, and it is why this takes a context. The intended caller holds an
// admission gate closed, so no new timestamp can be allocated while it waits.
//
// Safe for concurrent use.
func (c *Clock) AwaitQuiescent(ctx context.Context) error {
	for {
		// Read the allocation counter FIRST. Waiting for a floor read after the
		// frontier would let an allocation that lands between the two escape the
		// wait, which is the whole failure this closes.
		allocated := c.commit.Load()
		if c.visible.Load() >= allocated {
			return nil
		}
		if err := c.AwaitVisible(ctx, allocated); err != nil {
			return err
		}
	}
}

// AwaitingVisible reports how many callers are currently blocked in
// [Clock.AwaitVisible].
//
// It is the observable form of the cost this mechanism moves onto the read side: a
// value that is persistently non-zero says sessions are waiting on the frontier
// rather than reading, which is the condition an operator would otherwise have to
// infer from latency alone.
//
// Safe for concurrent use.
func (c *Clock) AwaitingVisible() int64 { return c.waiters.Load() }

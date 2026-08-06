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

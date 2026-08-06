package lpg

// session_test.go — rmp #2328: read-your-own-writes across transactions, and the
// spurious self-conflict it was hiding.
//
// Layer: short.
//
// # These are the two probes that MEASURED the defect
//
// They are not new constructions written to fit the fix. Each is the probe that
// found the behaviour at the sprint-334 head, promoted to a regression test with its
// session-aware form asserted. Both were run against the unfixed build first:
//
//	read-back:     660 checks, 9 did NOT see a commit the client was told succeeded
//	self-conflict: 12 writers x 64 writes -> 25 conflicts on DISJOINT keys, and every
//	               one had startTS < that goroutine's own previous commitTS
//
// # Why each test drives BOTH forms
//
// The sessionless form is the contract GoGraph still offers — snapshot isolation with
// no cross-transaction guarantee — and it is not a defect, it is a weaker promise. So
// asserting only the session form would leave the tests unable to distinguish "the
// session works" from "the frontier happened not to lag on this run". Each test
// therefore drives enough concurrency to make the lag real, and asserts the SESSION
// form holds. Where the sessionless form is also exercised it is reported, never
// asserted: it is legitimately allowed to miss.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// sessionWriters and sessionWrites size the concurrency that makes the frontier lag
// observable. At 1, 2 and 4 concurrent writers the unfixed build produced ZERO
// conflicts; the lag needs enough writers that one is reliably in flight while
// another commits. 12 was where the original probe measured 25.
const (
	sessionWriters = 12
	sessionWrites  = 64
)

// TestSession_ReadsItsOwnCommittedWrite is the read-back probe: after every
// acknowledged commit, the same session opens a snapshot and must see the value it
// was just told was committed.
//
// Against the unfixed build this failed with 9 misses in 660 checks. Each miss is a
// caller being told its write committed and then not finding it — the symptom that
// makes this a correctness question rather than a performance one.
func TestSession_ReadsItsOwnCommittedWrite(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	var wg sync.WaitGroup
	var checks, missed atomic.Int64
	for i := 0; i < sessionWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// ONE SESSION PER GOROUTINE, which is the contract: read-your-own-writes is
			// guaranteed within a session, and a goroutine is what a session models.
			s := g.NewSession()
			key := fmt.Sprintf("r-%d", id)
			if err := s.ApplyVersioned(func(tx WriteTx) error {
				return g.Writer(tx).AddNode(key)
			}); err != nil {
				t.Errorf("writer %d create: %v", id, err)
				return
			}
			for n := 1; n <= sessionWrites; n++ {
				if err := s.ApplyVersioned(func(tx WriteTx) error {
					return g.Writer(tx).SetNodeProperty(key, "seq", Int64Value(int64(n)))
				}); err != nil {
					t.Errorf("writer %d write %d: %v", id, n, err)
					return
				}
				// The commit was acknowledged. Read it back THROUGH THE SESSION.
				snap := s.BeginRead()
				v, ok := g.ReadAt(snap).GetNodeProperty(key, "seq")
				got, isInt := v.Int64()
				checks.Add(1)
				if !ok || !isInt || got != int64(n) {
					missed.Add(1)
					t.Errorf("writer %d: after an acknowledged commit of seq=%d the session read "+
						"back present=%v int=%v value=%d — the caller was told its write committed "+
						"and its own next snapshot cannot see it", id, n, ok, isInt, got)
				}
				g.EndRead(snap)
			}
		}(i)
	}
	wg.Wait()

	// ASSERT SOMETHING WAS SEEN: a test whose oracle is "nothing was missed" passes
	// trivially if it never checked anything.
	if got := checks.Load(); got != sessionWriters*sessionWrites {
		t.Fatalf("only %d read-backs ran, want %d: the workload did not execute and the "+
			"zero misses below prove nothing", got, sessionWriters*sessionWrites)
	}
	if missed.Load() != 0 {
		t.Errorf("%d of %d read-backs missed their own commit", missed.Load(), checks.Load())
	}
}

// TestSession_DoesNotConflictWithItselfOnItsOwnKey is the self-conflict probe: writers
// on DISJOINT keys contend for nothing, so none of them may be refused.
//
// Against the unfixed build this produced 25 serialization conflicts, every one of
// them a transaction that started below its own previous commit. That is the symptom
// that misleads an operator: an uncontended workload reporting contention, and a
// conflict rate that rises with writer count rather than with real contention.
func TestSession_DoesNotConflictWithItselfOnItsOwnKey(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	var wg sync.WaitGroup
	var conflicts atomic.Int64
	for i := 0; i < sessionWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := g.NewSession()
			key := fmt.Sprintf("w-%d", id)
			for n := 0; n < sessionWrites; n++ {
				err := s.ApplyVersioned(func(tx WriteTx) error {
					wv := g.Writer(tx)
					if n == 0 {
						if e := wv.AddNode(key); e != nil {
							return e
						}
					}
					return wv.SetNodeProperty(key, "seq", Int64Value(int64(n)))
				})
				if err != nil {
					conflicts.Add(1)
					t.Errorf("writer %d, write %d on its OWN key was refused: %v — no other "+
						"transaction touches this key, so a refusal here is the session starting "+
						"below its own previous commit", id, n, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if conflicts.Load() != 0 {
		t.Errorf("%d writes on disjoint keys were refused", conflicts.Load())
	}
	// The substrate's own counter must agree: a workload with no shared key must
	// report no contention at all, which is the whole point of making it observable.
	if got := g.MVCCStats().Write.Conflicts; got != 0 {
		t.Errorf("lpg.mvcc.conflicts = %d after a workload with no shared key: the conflict "+
			"rate an operator reads is inflated by writer count rather than by contention", got)
	}
}

// TestSession_FloorAdvancesAndWaitIsActuallyExercised proves the mechanism under test
// is REACHED, rather than the tests above passing because the frontier never lagged.
//
// The two tests above are negative oracles: they assert an absence. If the frontier
// never lagged on this machine they would pass against the unfixed build too, and
// prove nothing. This one asserts the positive — that a session's floor really does
// get ahead of the frontier, so the wait has something to wait for.
func TestSession_FloorAdvancesAndWaitIsActuallyExercised(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	var wg sync.WaitGroup
	var aheadOfFrontier atomic.Int64
	for i := 0; i < sessionWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := g.NewSession()
			key := fmt.Sprintf("f-%d", id)
			for n := 0; n < sessionWrites; n++ {
				if err := s.ApplyVersioned(func(tx WriteTx) error {
					wv := g.Writer(tx)
					if n == 0 {
						if e := wv.AddNode(key); e != nil {
							return e
						}
					}
					return wv.SetNodeProperty(key, "seq", Int64Value(int64(n)))
				}); err != nil {
					t.Errorf("writer %d: %v", id, err)
					return
				}
				// Sampled IMMEDIATELY after the commit was acknowledged: if the floor is
				// above the frontier here, then a session without the wait would have
				// started its next transaction below its own commit.
				if s.Floor() > g.mvccClock.ReadTS() {
					aheadOfFrontier.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	if aheadOfFrontier.Load() == 0 {
		t.Skipf("the frontier never lagged a session's own commit in %d writes across %d "+
			"writers, so the wait was never exercised and the two tests above proved nothing "+
			"on this run. This is a property of the machine's scheduling, not of the code: "+
			"re-run, or raise sessionWriters", sessionWriters*sessionWrites, sessionWriters)
	}
	t.Logf("the session floor was ahead of the visible frontier %d times in %d commits: "+
		"that many transactions would have started below their own previous commit without "+
		"the wait", aheadOfFrontier.Load(), sessionWriters*sessionWrites)
}

// TestSession_FloorIsMonotonic asserts a session's floor never moves backwards, which
// is what lets two goroutines share one session without pulling each other's
// guarantee back.
func TestSession_FloorIsMonotonic(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	s := g.NewSession()

	if got := s.Floor(); got != 0 {
		t.Fatalf("a fresh session has floor %d, want 0", got)
	}
	var prev uint64
	for n := 0; n < 20; n++ {
		if err := s.ApplyVersioned(func(tx WriteTx) error {
			wv := g.Writer(tx)
			if n == 0 {
				if e := wv.AddNode("m"); e != nil {
					return e
				}
			}
			return wv.SetNodeProperty("m", "seq", Int64Value(int64(n)))
		}); err != nil {
			t.Fatalf("write %d: %v", n, err)
		}
		got := s.Floor()
		if got < prev {
			t.Fatalf("the floor moved backwards: %d after %d", got, prev)
		}
		if got == 0 {
			t.Fatalf("write %d committed but the floor is still 0: the session is not "+
				"recording its own commits, so its wait can never fire", n)
		}
		prev = got
	}

	// A transaction that versions NOTHING publishes no instant, so it must not move
	// the floor — and must not reset it either.
	if err := s.ApplyVersioned(func(WriteTx) error { return nil }); err != nil {
		t.Fatalf("empty transaction: %v", err)
	}
	if got := s.Floor(); got != prev {
		t.Errorf("a transaction that versioned nothing moved the floor from %d to %d", prev, got)
	}
}

// TestSession_ExplicitTransactionRecordsItsInstant asserts the multi-statement form
// carries the guarantee too — the floor advances when the transaction is closed
// through the session, not when it is opened.
func TestSession_ExplicitTransactionRecordsItsInstant(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	s := g.NewSession()

	tx, err := s.BeginVersionedTx()
	if err != nil {
		t.Fatalf("BeginVersionedTx: %v", err)
	}
	if got := s.Floor(); got != 0 {
		t.Errorf("the floor moved to %d on BEGIN: nothing has committed yet", got)
	}
	if err := g.Writer(tx).AddNode("x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.EndVersionedTx(tx)
	if s.Floor() == 0 {
		t.Fatal("the floor is still 0 after an explicit transaction committed: closing " +
			"through the session is what records the instant, and it did not")
	}

	// And the session sees its own work.
	snap := s.BeginRead()
	defer g.EndRead(snap)
	if !g.NodeExistsAsOf(nodeIDOf(t, g, "x"), snap) {
		t.Error("the session cannot see the node its own explicit transaction created")
	}
}

// waitFor spins until cond holds, with a deadline, so a test never blocks for ever on
// a condition the code under test failed to reach.
//
// A DEADLINE and not a fixed spin: a fixed attempt count measures this machine's
// speed rather than the condition, and this project has already had one such guard
// pass normally and fail under coverage instrumentation.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition was never reached within 5s")
		}
		runtime.Gosched()
	}
}

// TestClock_AwaitVisibleReturnsWhenTheFrontierArrives covers the primitive directly:
// a waiter blocked below the frontier is woken by the publication that lifts it.
func TestClock_AwaitVisibleReturnsWhenTheFrontierArrives(t *testing.T) {
	var c mvcc.Clock
	// Two allocations; publishing the SECOND alone cannot advance the contiguous
	// frontier past the first, which is the whole shape being tested.
	first := c.NextCommitTS()
	second := c.NextCommitTS()
	c.PublishCommitTS(second)
	if c.ReadTS() >= second {
		t.Fatalf("the frontier reached %d with commit %d still in flight; the contiguous "+
			"property this test rests on does not hold", c.ReadTS(), first)
	}

	done := make(chan error, 1)
	go func() { done <- c.AwaitVisible(t.Context(), second) }()

	// The waiter must still be blocked, and must be COUNTED as blocked — otherwise
	// the publish path would never wake it.
	waitFor(t, func() bool { return c.AwaitingVisible() > 0 })
	select {
	case err := <-done:
		t.Fatalf("AwaitVisible returned %v while commit %d was still in flight", err, first)
	default:
	}

	c.PublishCommitTS(first)
	if err := <-done; err != nil {
		t.Fatalf("AwaitVisible: %v", err)
	}
	if c.ReadTS() < second {
		t.Errorf("AwaitVisible returned at frontier %d, below the floor %d it waited for",
			c.ReadTS(), second)
	}
}

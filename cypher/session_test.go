package cypher_test

// session_test.go — rmp #2329: the read-your-own-writes guarantee, carried through
// the Cypher API.
//
// Layer: short.
//
// # What is being asserted, and why the bare Engine does not assert it
//
// [cypher.Engine] gives snapshot isolation and nothing across statements. A commit
// becomes visible when the CONTIGUOUS frontier advances past it, so an unrelated
// in-flight commit can hold it back and a caller's own write can be missing from the
// caller's own next read. That is correct snapshot isolation; it is not what a
// caller expects of itself.
//
// These gates hold a [cypher.Session] to the stronger contract, and hold the bare
// Engine to the weaker one — because if the Engine had silently gained the wait, the
// session would be free and the cost would be paid by every unrelated reader.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// sessionScalar runs a read through a session and returns its single integer column.
func sessionScalar(t *testing.T, s *cypher.Session, q string) int64 {
	t.Helper()
	res, err := s.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("session Run %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("%q returned no row (err=%v)", q, res.Err())
	}
	n, ok := res.ValueAt(0).(expr.IntegerValue)
	if !ok {
		t.Fatalf("%q returned %T, want IntegerValue", q, res.ValueAt(0))
	}
	return int64(n)
}

// TestSession_ObservesItsOwnCommittedWrites is the headline contract: write, then
// read, on the same session.
//
// It is run with concurrent unrelated writers, because that is the ONLY condition
// under which the guarantee is distinguishable. With a quiet graph a commit becomes
// visible immediately and a sessionless read would pass too; it is an unrelated
// in-flight commit holding the contiguous frontier back that produces the gap.
func TestSession_ObservesItsOwnCommittedWrites(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	stop := make(chan struct{})
	var noise sync.WaitGroup
	for w := 0; w < 4; w++ {
		noise.Add(1)
		go func(id int) {
			defer noise.Done()
			ns := eng.NewSession()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = ns.RunInTx(ctx, fmt.Sprintf("CREATE (:Noise {w: %d, n: %d})", id, n), nil)
			}
		}(w)
	}
	defer func() { close(stop); noise.Wait() }()

	s := eng.NewSession()
	for i := 0; i < 60; i++ {
		if _, err := s.RunInTx(ctx, fmt.Sprintf("CREATE (:Mine {i: %d})", i), nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if got := sessionScalar(t, s, "MATCH (n:Mine) RETURN count(n) AS n"); got != int64(i+1) {
			t.Fatalf("after committing write %d the session sees %d of its own nodes, want %d: "+
				"a caller must observe its own committed writes", i, got, i+1)
		}
	}
}

// TestSession_DoesNotConflictWithItselfOnItsOwnKey mirrors
// lpg.TestSession_DoesNotConflictWithItselfOnItsOwnKey at the Cypher layer.
//
// Twelve writers on DISJOINT keys must never conflict: nothing contends for any of
// them. A conflict here means a writer's transaction started at an instant BELOW its
// own previous commit and then found a version it could not have seen — a caller
// losing a race with itself.
func TestSession_DoesNotConflictWithItselfOnItsOwnKey(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	const writers, writes = 12, 40
	var wg sync.WaitGroup
	var conflicts atomic.Int64
	var applied atomic.Int64
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := eng.NewSession()
			key := fmt.Sprintf("k%d", id)
			if _, err := s.RunInTx(ctx, fmt.Sprintf("CREATE (:Own {k: '%s', v: 0})", key), nil); err != nil {
				t.Errorf("writer %d seed: %v", id, err)
				return
			}
			for n := 1; n <= writes; n++ {
				_, err := s.RunInTx(ctx,
					fmt.Sprintf("MATCH (x:Own {k: '%s'}) SET x.v = %d", key, n), nil)
				if err != nil {
					conflicts.Add(1)
					continue
				}
				applied.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if n := conflicts.Load(); n != 0 {
		t.Errorf("%d serialization conflicts across %d writers on DISJOINT keys: nothing "+
			"contended for any of them, so each writer lost a race with itself", n, writers)
	}
	if got, want := applied.Load(), int64(writers*writes); got != want {
		t.Errorf("%d writes applied, want %d", got, want)
	}
	// Every writer's final value must be its last write.
	for i := 0; i < writers; i++ {
		q := fmt.Sprintf("MATCH (x:Own {k: 'k%d'}) RETURN x.v AS n", i)
		if got := readScalar(t, eng, q); got != writes {
			t.Errorf("writer %d final value = %d, want %d", i, got, writes)
		}
	}
}

// TestSession_BeginTxObservesTheSessionsEarlierCommits covers the explicit-transaction
// entry point, which takes its snapshot when the transaction OPENS rather than per
// statement — so the wait has to happen there or the whole transaction reads stale.
func TestSession_BeginTxObservesTheSessionsEarlierCommits(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()
	s := eng.NewSession()

	if _, err := s.RunInTx(ctx, "CREATE (:Seed {v: 1})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tx, err := s.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec("MATCH (n:Seed) RETURN count(n) AS n", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Next() {
		t.Fatalf("no row: %v", res.Err())
	}
	if n, _ := res.ValueAt(0).(expr.IntegerValue); int64(n) != 1 {
		t.Errorf("the session's own transaction sees %d seed nodes, want 1", int64(n))
	}
	_ = res.Close()
}

// TestSession_LeavesTheEngineContractAlone pins the OTHER half of the decision: the
// bare Engine keeps the sessionless contract, so an unrelated reader pays no wait.
//
// It asserts the shape rather than the timing — a sessionless read is free to
// observe the write, and usually will on a quiet graph. What must hold is that the
// two entry points are distinct objects with distinct contracts, so a future change
// that quietly routed Engine.Run through a session would be visible here.
func TestSession_LeavesTheEngineContractAlone(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	s := eng.NewSession()
	if s.Floor() != 0 {
		t.Errorf("a fresh session has floor %d, want 0", s.Floor())
	}
	if _, err := s.RunInTx(ctx, "CREATE (:F)", nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s.Floor() == 0 {
		t.Error("the session recorded no instant for a commit it made, so its next read " +
			"has nothing to wait for and the guarantee is vacuous")
	}
	// A SECOND session is a different caller and carries none of the first's floor.
	if other := eng.NewSession(); other.Floor() != 0 {
		t.Errorf("an independent session inherited floor %d; a session is one caller, and "+
			"sharing a floor would make every caller wait on every other", other.Floor())
	}
}

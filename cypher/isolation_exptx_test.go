package cypher_test

// isolation_exptx_test.go — regression gate for ExplicitTx read-committed
// isolation (task #1412, isolation option b: whole-tx visMu.Lock).
//
// # Isolation contract after task #1412
//
// [Engine.BeginTx] now acquires the graph's transaction-visibility write lock
// (visMu via [lpg.Graph.LockBarrier]) for the whole lifetime of the explicit
// transaction. A concurrent [Engine.Run] or [lpg.Graph.View] call acquires the
// read-side of the same lock, so it BLOCKS while the explicit transaction is open
// and is released only once [ExplicitTx.Commit] or [ExplicitTx.Rollback] is
// called. Readers therefore observe either the pre-transaction state or the fully
// committed/rolled-back state — never an intermediate dirty write.
//
// The tests in this file cover:
//   - Readers block during an open ExplicitTx and observe the post-Commit state.
//   - After Rollback, readers observe the pre-transaction state (0 nodes).
//   - Across multiple Exec calls within one ExplicitTx, no intermediate count is
//     ever observable by a concurrent reader (atomic multi-statement visibility).
//
// Layer: short. Race-clean (go test -race must pass).

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countTxNodes runs MATCH (n:Tx) RETURN count(n) AS c via Engine.Run and
// returns the integer count. It fatals on query or iteration errors.
func countTxNodes(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), `MATCH (n:Tx) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("countTxNodes Run: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("countTxNodes res.Close: %v", cerr)
		}
	}()
	if !res.Next() {
		t.Fatal("countTxNodes: no row returned")
	}
	rec := res.Record()
	if err := res.Err(); err != nil {
		t.Fatalf("countTxNodes iterate: %v", err)
	}
	raw, ok := rec["c"]
	if !ok {
		t.Fatalf("countTxNodes: column 'c' absent in %v", rec)
	}
	return parseCount(t, raw)
}

// parseCount extracts an int64 from a count-query result value. Engine count
// aggregates return expr.IntegerValue (a named int64 type), so a plain int64
// type-assertion would silently fail; we use fmt.Sscan for robustness.
func parseCount(t *testing.T, raw any) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(fmt.Sprintf("%v", raw), &n); err != nil {
		t.Fatalf("parseCount: cannot parse %T(%v): %v", raw, raw, err)
	}
	return n
}

// countTxNodesQuery runs the count query, returns count and any error (no
// fatals — suitable for use in goroutines where t.Fatal is forbidden).
func countTxNodesQuery(ctx context.Context, eng *cypher.Engine) (int64, error) {
	res, err := eng.Run(ctx, `MATCH (n:Tx) RETURN count(n) AS c`, nil)
	if err != nil {
		return 0, err
	}
	defer res.Close()
	if !res.Next() {
		return 0, fmt.Errorf("no row returned")
	}
	rec := res.Record()
	raw, ok := rec["c"]
	if !ok {
		return 0, fmt.Errorf("column 'c' absent: %v", rec)
	}
	var n int64
	if _, err := fmt.Sscan(fmt.Sprintf("%v", raw), &n); err != nil {
		return 0, fmt.Errorf("cannot parse count %T(%v): %w", raw, raw, err)
	}
	return n, nil
}

// TestExplicitTx_Isolation_ReadCommitted verifies the isolation contract of a
// concurrent reader while an ExplicitTx is open.
//
// # The contract CHANGED with MVCC P4c (rmp #2274, #2290), and strengthened
//
// It used to be that a reader BLOCKED: Engine.Run took the visibility barrier's
// read side, an open ExplicitTx held its write side, and the reader waited for
// Commit and then saw the committed state. That satisfied read-committed by
// making the reader wait, and it is precisely the mechanism that starved
// readers — a 95-second analytical read plus one writer collapsed short-read
// throughput 50×, because Go's RWMutex parks every reader arriving behind a
// queued writer.
//
// A reader now takes a SNAPSHOT and does not wait. It observes the state as of
// the instant it began, so it still never sees uncommitted work — the guarantee
// the old test existed to protect — and it now gets the stronger one too: a
// stable view for its whole duration, rather than whatever the graph happened
// to hold when the barrier let it through. The subtests below assert both
// halves, and the liveness that is the point of the change.
func TestExplicitTx_Isolation_ReadCommitted(t *testing.T) {
	t.Parallel()

	t.Run("reader_does_not_block_and_sees_no_uncommitted_work", func(t *testing.T) {
		t.Parallel()

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)

		// Pre-condition: empty graph.
		if n := countTxNodes(t, eng); n != 0 {
			t.Fatalf("pre-test :Tx count = %d, want 0", n)
		}

		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}

		// CREATE inside the open transaction — applied eagerly to the live graph.
		res, err := tx.Exec(`CREATE (:Tx) RETURN count(*) AS c`, nil)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("Exec CREATE: %v", err)
		}
		_ = res.Close()

		// Concurrent reader: launched while the transaction is still open.
		// Because ExplicitTx now holds visMu.Lock, Engine.Run blocks on visMu.RLock
		// inside Graph.View and cannot proceed until Commit releases the lock.
		type readResult struct {
			count int64
			err   error
		}
		readCh := make(chan readResult, 1)
		readerStarted := make(chan struct{})

		go func() {
			close(readerStarted)
			// This no longer waits for anything: the read takes a snapshot and
			// resolves every store as of it.
			cnt, err := countTxNodesQuery(context.Background(), eng)
			readCh <- readResult{count: cnt, err: err}
		}()
		<-readerStarted

		// LIVENESS is the point of the change, so it is asserted first and
		// while the transaction is still OPEN. Before MVCC this select would
		// have taken the timeout branch every time.
		select {
		case rr := <-readCh:
			if rr.err != nil {
				t.Fatalf("concurrent Engine.Run: %v", rr.err)
			}
			// ISOLATION: the transaction has not committed, so its eagerly
			// applied CREATE must not be visible.
			if rr.count != 0 {
				t.Errorf("a reader concurrent with an OPEN transaction observed %d nodes; "+
					"want 0 — it must not see uncommitted work", rr.count)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a reader concurrent with an open transaction did not complete within 3 s: " +
				"it is still blocking on the writer, which is the reader starvation rmp #2274 " +
				"exists to remove")
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		// And a reader that starts AFTER the commit sees it.
		if n := countTxNodes(t, eng); n != 1 {
			t.Errorf("a reader started after Commit observed %d nodes; want 1", n)
		}
	})

	t.Run("reader_sees_zero_after_rollback", func(t *testing.T) {
		t.Parallel()

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)

		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		res, err := tx.Exec(`CREATE (:Tx) RETURN count(*) AS c`, nil)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("Exec CREATE: %v", err)
		}
		_ = res.Close()

		type readResult struct {
			count int64
			err   error
		}
		readCh := make(chan readResult, 1)
		readerStarted := make(chan struct{})

		go func() {
			close(readerStarted)
			cnt, err := countTxNodesQuery(context.Background(), eng)
			readCh <- readResult{count: cnt, err: err}
		}()

		<-readerStarted
		time.Sleep(20 * time.Millisecond)

		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		select {
		case rr := <-readCh:
			if rr.err != nil {
				t.Fatalf("concurrent Engine.Run: %v", rr.err)
			}
			// After rollback the undo log removes the write; reader must see 0.
			if rr.count != 0 {
				t.Errorf("reader observed %d nodes after Rollback; want 0", rr.count)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent reader did not complete within 3 s after Rollback")
		}
	})

	t.Run("multi_exec_never_leaks_intermediate_state", func(t *testing.T) {
		t.Parallel()

		// This sub-test verifies that across multiple Exec calls within one
		// ExplicitTx, a polling concurrent reader never observes an intermediate
		// count (e.g. exactly 1 node when 2 are committed atomically). Valid
		// observations are 0 (before Commit) or 2 (after Commit).

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)

		var (
			mu           sync.Mutex
			observations []int64
		)

		stopReader := make(chan struct{})
		readerStopped := make(chan struct{})

		go func() {
			defer close(readerStopped)
			for {
				select {
				case <-stopReader:
					return
				default:
				}
				cnt, err := countTxNodesQuery(context.Background(), eng)
				if err != nil {
					continue
				}
				mu.Lock()
				observations = append(observations, cnt)
				mu.Unlock()
			}
		}()

		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			close(stopReader)
			t.Fatalf("BeginTx: %v", err)
		}

		for _, q := range []string{
			`CREATE (:Tx {name:'A'}) RETURN count(*) AS c`,
			`CREATE (:Tx {name:'B'}) RETURN count(*) AS c`,
		} {
			r, execErr := tx.Exec(q, nil)
			if execErr != nil {
				_ = tx.Rollback()
				close(stopReader)
				t.Fatalf("Exec: %v", execErr)
			}
			_ = r.Close()
			// Brief pause: the reader goroutine must not see intermediate state.
			time.Sleep(5 * time.Millisecond)
		}

		if err := tx.Commit(); err != nil {
			close(stopReader)
			t.Fatalf("Commit: %v", err)
		}

		close(stopReader)
		<-readerStopped

		mu.Lock()
		defer mu.Unlock()
		// Valid counts: 0 (before or during tx) or 2 (after commit). Never 1.
		for _, cnt := range observations {
			if cnt == 1 {
				t.Errorf("concurrent reader observed intermediate state: count=1 (want only 0 or 2)")
			}
		}
	})
}

// TestExplicitTx_InTxCreateThenDeleteInvisibleToConcurrentReader is the
// regression gate for rmp #2443, found by the DST multi-session mode: a
// transaction that creates a node (CREATE or MERGE) and DETACH DELETEs it in
// the SAME transaction must expose NOTHING to a concurrent autocommit reader
// while it is open — the broken build showed a bare phantom node (no labels,
// no properties: MATCH (n) counted 1, MATCH (n:Ghost) counted 0) until the
// transaction terminated. The create,delete,create shape is covered too: the
// still-uncommitted resurrected node must be equally invisible.
func TestExplicitTx_InTxCreateThenDeleteInvisibleToConcurrentReader(t *testing.T) {
	countAll := func(t *testing.T, eng *cypher.Engine) int64 {
		t.Helper()
		res, err := eng.Run(context.Background(), `MATCH (n) RETURN count(n) AS c`, nil)
		if err != nil {
			t.Fatalf("countAll Run: %v", err)
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatal("countAll: no row returned")
		}
		v, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			t.Fatalf("countAll: not an integer: %T", res.ValueAt(0))
		}
		return int64(v)
	}

	for _, tc := range []struct {
		name       string
		statements []string
		rollback   bool
		// wantOpen is the whole-graph node count a concurrent reader must see
		// while the transaction is still open; wantAfter after it terminates.
		wantOpen, wantAfter int64
	}{
		{"CREATE+delete, commit", []string{
			`CREATE (:Ghost {name:'g'})`,
			`MATCH (n:Ghost {name:'g'}) DETACH DELETE n`,
		}, false, 0, 0},
		{"CREATE+delete, rollback", []string{
			`CREATE (:Ghost {name:'g'})`,
			`MATCH (n:Ghost {name:'g'}) DETACH DELETE n`,
		}, true, 0, 0},
		{"MERGE+delete, commit", []string{
			`MERGE (n:Ghost {name:'g'})`,
			`MATCH (n:Ghost {name:'g'}) DETACH DELETE n`,
		}, false, 0, 0},
		{"MERGE+delete, rollback", []string{
			`MERGE (n:Ghost {name:'g'})`,
			`MATCH (n:Ghost {name:'g'}) DETACH DELETE n`,
		}, true, 0, 0},
		{"create,delete,create, commit", []string{
			`CREATE (:Ghost {name:'g'})`,
			`MATCH (n:Ghost {name:'g'}) DETACH DELETE n`,
			`CREATE (:Ghost {name:'g'})`,
		}, false, 0, 1},
		{"create,delete,create, rollback", []string{
			`CREATE (:Ghost {name:'g'})`,
			`MATCH (n:Ghost {name:'g'}) DETACH DELETE n`,
			`CREATE (:Ghost {name:'g'})`,
		}, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			eng := cypher.NewEngine(g)

			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			for _, stmt := range tc.statements {
				res, err := tx.ExecAny(stmt, nil)
				if err != nil {
					t.Fatalf("in-tx %q: %v", stmt, err)
				}
				for res.Next() {
				}
				if derr := res.Err(); derr != nil {
					t.Fatalf("in-tx %q drain: %v", stmt, derr)
				}
				_ = res.Close()
			}

			// The concurrent reader: the transaction is OPEN, so its state —
			// including the intermediate create+delete — must be invisible.
			if got := countAll(t, eng); got != tc.wantOpen {
				t.Fatalf("concurrent reader during open tx: MATCH (n) count=%d, want %d (uncommitted state leaked)", got, tc.wantOpen)
			}

			if tc.rollback {
				if err := tx.Rollback(); err != nil {
					t.Fatalf("Rollback: %v", err)
				}
			} else if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if got := countAll(t, eng); got != tc.wantAfter {
				t.Fatalf("after terminal: MATCH (n) count=%d, want %d", got, tc.wantAfter)
			}
		})
	}
}

// TestExplicitTx_ConflictedRollbackLeavesNoPhantom is the second regression
// gate for rmp #2443: a transaction that CREATEd nodes and was then DOOMED by a
// serialization conflict on a contended write must, after Rollback, leave no
// trace — the broken build left each aborted create behind as a bare phantom
// node (the aborted birth+death life-record pair was tombstoned and then
// revived by the abort reclaim, and the revive won).
func TestExplicitTx_ConflictedRollbackLeavesNoPhantom(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	exec := func(tx *cypher.ExplicitTx, q string) error {
		res, err := tx.ExecAny(q, nil)
		if err != nil {
			return err
		}
		for res.Next() {
		}
		derr := res.Err()
		_ = res.Close()
		return derr
	}
	countAll := func() int64 {
		res, err := eng.Run(ctx, `MATCH (n) RETURN count(n) AS c`, nil)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatal("count: no row")
		}
		v, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			t.Fatalf("count: not an integer: %T", res.ValueAt(0))
		}
		return int64(v)
	}

	// One committed contended target.
	if _, err := eng.RunInTxAny(ctx, `CREATE (:T {name:'target', v:1})`, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A creates two nodes, B commits a write on the target, A's own write on the
	// target is then refused (first-committer-wins), and A rolls back.
	txA, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx A: %v", err)
	}
	if err := exec(txA, `CREATE (:Ghost {name:'a1'})`); err != nil {
		t.Fatalf("A create 1: %v", err)
	}
	if err := exec(txA, `CREATE (:Ghost {name:'a2'})`); err != nil {
		t.Fatalf("A create 2: %v", err)
	}
	txB, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx B: %v", err)
	}
	if err := exec(txB, `MATCH (n:T {name:'target'}) SET n.v = 2`); err != nil {
		t.Fatalf("B set: %v", err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatalf("B commit: %v", err)
	}
	if err := exec(txA, `MATCH (n:T {name:'target'}) SET n.v = 3`); err == nil {
		t.Fatal("A's contended write did not conflict; the scenario no longer exercises the doomed-rollback path")
	}
	if err := txA.Rollback(); err != nil {
		t.Fatalf("A rollback: %v", err)
	}

	if got := countAll(); got != 1 {
		t.Fatalf("after conflicted rollback: MATCH (n) count=%d, want 1 (aborted creates leaked as phantoms)", got)
	}
}

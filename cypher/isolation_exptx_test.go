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

// TestExplicitTx_Isolation_ReadCommitted verifies that the isolation contract
// after task #1412 is read-committed: concurrent Engine.Run readers block while
// an ExplicitTx is open and never observe uncommitted writes.
func TestExplicitTx_Isolation_ReadCommitted(t *testing.T) {
	t.Parallel()

	t.Run("reader_blocks_until_commit", func(t *testing.T) {
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
			// Signal that this goroutine is about to call Engine.Run. It closes
			// the channel BEFORE the call so the main goroutine knows the reader
			// is about to contend on visMu.
			close(readerStarted)
			// This call blocks on visMu.RLock until the ExplicitTx releases visMu
			// via Commit → release → UnlockBarrier.
			cnt, err := countTxNodesQuery(context.Background(), eng)
			readCh <- readResult{count: cnt, err: err}
		}()

		// Wait for the goroutine to be scheduled, then give it time to reach the
		// visMu.RLock contention point before proceeding with the commit.
		<-readerStarted
		time.Sleep(20 * time.Millisecond)

		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		select {
		case rr := <-readCh:
			if rr.err != nil {
				t.Fatalf("concurrent Engine.Run: %v", rr.err)
			}
			// The reader ran after Commit (it was blocked) and must see the committed node.
			if rr.count != 1 {
				t.Errorf("reader observed %d nodes after Commit; want 1", rr.count)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent reader did not complete within 3 s after Commit")
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

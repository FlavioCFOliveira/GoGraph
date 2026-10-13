package cypher_test

// isolation_exptx_test.go — regression gate for ExplicitTx read-committed
// isolation (task #1412, isolation option b: whole-tx visMu.Lock).
//
// # Isolation contract
//
// Task #1412 made [Engine.BeginTx] hold the graph's barrier exclusively for the
// whole transaction, so readers blocked. That is no longer the mechanism: rmp
// #2305 retired the hold, and rmp #2344 removed lpg.Graph.View. Each statement
// of an explicit transaction now holds the schema barrier SHARED for its own
// duration only ([lpg.Graph.ApplyInVersionedTx]), and every write it makes is
// stamped with the transaction's commit record, which stays unpublished until
// [ExplicitTx.Commit]. A concurrent [Engine.Run] reads at its own MVCC snapshot
// and never blocks, so it observes either the pre-transaction state or the
// fully committed state — never an intermediate dirty write.
//
// The tests in this file cover:
//   - Readers do not block during an open ExplicitTx and observe the post-Commit state.
//   - After Rollback, readers observe the pre-transaction state (0 nodes).
//   - Across multiple Exec calls within one ExplicitTx, no intermediate count is
//     ever observable by a concurrent reader (atomic multi-statement visibility).
//
// Layer: short. Race-clean (go test -race must pass).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
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

		g := lpg.New[string, float64](adjlist.Config{})
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

		// Concurrent reader: launched while the transaction is still open. It
		// reads at its own MVCC snapshot; the open transaction's commit record is
		// unpublished, so none of its writes are visible.
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

		g := lpg.New[string, float64](adjlist.Config{})
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

		g := lpg.New[string, float64](adjlist.Config{})
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
			g := lpg.New[string, float64](adjlist.Config{})
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
	g := lpg.New[string, float64](adjlist.Config{})
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

// TestExplicitTx_DeleteVsPendingWriteConflicts is the regression gate for rmp
// #2444: the NODE is the unit of write-write conflict, so a DETACH DELETE of a
// node carrying another in-flight transaction's pending property write must be
// refused with the typed serialization conflict (and symmetrically, a property
// write on a node with a pending delete must not commit). The broken build let
// the delete through silently, and after BOTH transactions rolled back the
// committed node was gone from the label scan permanently.
func TestExplicitTx_DeleteVsPendingWriteConflicts(t *testing.T) {
	newEng := func(t *testing.T) *cypher.Engine {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)
		if _, err := eng.RunInTxAny(context.Background(), `CREATE (:P {name:'victim', v:1})`, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return eng
	}
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
	countRows := func(t *testing.T, eng *cypher.Engine, q string) int64 {
		t.Helper()
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("read %q: %v", q, err)
		}
		defer func() { _ = res.Close() }()
		var n int64
		for res.Next() {
			n++
		}
		if derr := res.Err(); derr != nil {
			t.Fatalf("read %q drain: %v", q, derr)
		}
		return n
	}

	t.Run("delete of a node with a pending write is refused", func(t *testing.T) {
		eng := newEng(t)
		ctx := context.Background()
		txS, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(txS, `MATCH (n:P {name:'victim'}) SET n.v = 2`); err != nil {
			t.Fatalf("pending SET: %v", err)
		}
		txD, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		delErr := exec(txD, `MATCH (n:P {name:'victim'}) DETACH DELETE n`)
		if delErr == nil {
			// The conflict may legally surface at COMMIT instead of the statement.
			delErr = txD.Commit()
		} else {
			_ = txD.Rollback()
		}
		if !errors.Is(delErr, mvcc.ErrSerializationConflict) {
			t.Fatalf("delete over a pending write: err=%v, want ErrSerializationConflict", delErr)
		}
		_ = txS.Rollback()

		// The double rollback must leave the committed victim fully intact in
		// EVERY read path — the broken build lost it from the label scan.
		if got := countRows(t, eng, `MATCH (n:P) RETURN n.name`); got != 1 {
			t.Fatalf("label scan after double rollback: %d rows, want 1", got)
		}
		if got := countRows(t, eng, `MATCH (n:P {name:'victim'}) RETURN n`); got != 1 {
			t.Fatalf("by-name after double rollback: %d rows, want 1", got)
		}
		if got := countRows(t, eng, `MATCH (n) RETURN n`); got != 1 {
			t.Fatalf("all-scan after double rollback: %d rows, want 1", got)
		}
	})

	t.Run("write on a node with a pending delete does not commit", func(t *testing.T) {
		eng := newEng(t)
		ctx := context.Background()
		txD, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(txD, `MATCH (n:P {name:'victim'}) DETACH DELETE n`); err != nil {
			t.Fatalf("pending delete: %v", err)
		}
		txS, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		setErr := exec(txS, `MATCH (n:P {name:'victim'}) SET n.v = 3`)
		if setErr == nil {
			setErr = txS.Commit()
		} else {
			_ = txS.Rollback()
		}
		if !errors.Is(setErr, mvcc.ErrSerializationConflict) {
			t.Fatalf("write over a pending delete: err=%v, want ErrSerializationConflict", setErr)
		}
		_ = txD.Rollback()
		if got := countRows(t, eng, `MATCH (n:P) RETURN n.name`); got != 1 {
			t.Fatalf("label scan after double rollback: %d rows, want 1", got)
		}
	})
}

// TestExplicitTx_EdgeVsPendingNodeDelete extends the rmp #2444 gate to the
// edge-endpoint half: an edge CREATE whose endpoint carries another
// transaction's pending DETACH DELETE must be refused, and a DETACH DELETE of
// a node that carries another transaction's pending incident edge (either
// direction — the graph is directed) must be refused. The broken build let an
// edge commit onto a concurrently deleted endpoint.
func TestExplicitTx_EdgeVsPendingNodeDelete(t *testing.T) {
	seed := func(t *testing.T) *cypher.Engine {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)
		if _, err := eng.RunInTxAny(context.Background(),
			`CREATE (:P {name:'a'}), (:P {name:'b'})`, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return eng
	}
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
	finish := func(tx *cypher.ExplicitTx, stmtErr error) error {
		if stmtErr != nil {
			_ = tx.Rollback()
			return stmtErr
		}
		return tx.Commit()
	}

	t.Run("edge create onto a pending-deleted endpoint is refused", func(t *testing.T) {
		for _, endpoint := range []string{"a", "b"} {
			eng := seed(t)
			ctx := context.Background()
			txD, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := exec(txD, fmt.Sprintf(`MATCH (n:P {name:'%s'}) DETACH DELETE n`, endpoint)); err != nil {
				t.Fatalf("pending delete of %s: %v", endpoint, err)
			}
			txE, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			edgeErr := finish(txE, exec(txE, `MATCH (x:P {name:'a'}),(y:P {name:'b'}) CREATE (x)-[:R]->(y)`))
			if !errors.Is(edgeErr, mvcc.ErrSerializationConflict) {
				t.Fatalf("edge onto pending-deleted %q: err=%v, want ErrSerializationConflict", endpoint, edgeErr)
			}
			_ = txD.Rollback()
		}
	})

	t.Run("delete of a node with a pending incident edge is refused", func(t *testing.T) {
		// The pending edge is INCOMING at 'b' — the direction the physical
		// insert does not touch, which is exactly the case rmp #2444 closed by
		// stamping both endpoints on directed graphs.
		for _, victim := range []string{"a", "b"} {
			eng := seed(t)
			ctx := context.Background()
			txE, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := exec(txE, `MATCH (x:P {name:'a'}),(y:P {name:'b'}) CREATE (x)-[:R]->(y)`); err != nil {
				t.Fatalf("pending edge: %v", err)
			}
			txD, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			delErr := finish(txD, exec(txD, fmt.Sprintf(`MATCH (n:P {name:'%s'}) DETACH DELETE n`, victim)))
			if !errors.Is(delErr, mvcc.ErrSerializationConflict) {
				t.Fatalf("delete of %q under a pending incident edge: err=%v, want ErrSerializationConflict", victim, delErr)
			}
			_ = txE.Rollback()
		}
	})
}

// TestExplicitTx_DoomedCreateLeavesNoOrphanSlot is the rmp #2444 gate for the
// orphan-slot leak: a transaction doomed by a serialization conflict runs a
// further CREATE (refused), then rolls back — no bare node may remain. On the
// broken build the CREATE's mapper intern survived with no life record, so the
// slot read as a permanently visible unnamed node.
func TestExplicitTx_DoomedCreateLeavesNoOrphanSlot(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	if _, err := eng.RunInTxAny(ctx, `CREATE (:P {name:'target', v:1})`, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
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

	txA, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txB, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(txB, `MATCH (n:P {name:'target'}) SET n.v = 2`); err != nil {
		t.Fatal(err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatal(err)
	}
	// Doom A on the contended write, then run a CREATE on the doomed handle.
	if err := exec(txA, `MATCH (n:P {name:'target'}) SET n.v = 3`); err == nil {
		t.Fatal("contended write did not doom the transaction")
	}
	if err := exec(txA, `CREATE (:P {name:'late'})`); err == nil {
		t.Fatal("a doomed transaction accepted a further CREATE")
	}
	if err := txA.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	res, err := eng.Run(ctx, `MATCH (n) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatal("no row")
	}
	v, ok := res.ValueAt(0).(expr.IntegerValue)
	if !ok {
		t.Fatalf("not an integer: %T", res.ValueAt(0))
	}
	if int64(v) != 1 {
		t.Fatalf("after doomed-create rollback: MATCH (n) count=%d, want 1 (orphan slot leaked)", int64(v))
	}
}

// TestExplicitTx_StackedRelationshipWriteCannotCommitARolledBackRelationship is
// the audit's Cypher reproduction of rmp #2966 crossed with rmp #2965. T1
// creates a->c; T2 sets a property on a->b, which rebuilt a's adjacency entry
// with T1's uncommitted arc in it; T1 is doomed by a conflict on a node and
// rolls back. The undo removed T1's arc, the abort's withdrawal then restored
// T2's entry, which still held it, and T2's commit made the rolled-back
// relationship permanent: count 2 where 1 is right. The relationship-property
// write now claims a, so T2 is refused and the arc never leaves T1.
func TestExplicitTx_StackedRelationshipWriteCannotCommitARolledBackRelationship(t *testing.T) {
	for _, doomed := range []bool{false, true} {
		t.Run(fmt.Sprintf("doomed=%v", doomed), func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{})
			t.Cleanup(func() { _ = g.Close() })
			eng := cypher.NewEngine(g)
			ctx := context.Background()
			run := func(r *cypher.Result, err error) error {
				if err != nil {
					return err
				}
				for r.Next() {
				}
				return r.Close()
			}
			if err := run(eng.RunAny(ctx, "CREATE (a:N {id:'a'})-[:R]->(b:N {id:'b'}), (c:N {id:'c'}), (x:N {id:'x'})", nil)); err != nil {
				t.Fatal(err)
			}
			t1, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(t1.ExecAny("MATCH (a:N {id:'a'}), (c:N {id:'c'}) CREATE (a)-[:R]->(c)", nil)); err != nil {
				t.Fatal(err)
			}
			t2, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_ = run(t2.ExecAny("MATCH (:N {id:'a'})-[r:R]->(:N {id:'b'}) SET r.p = 1", nil))
			if doomed {
				t3, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_ = run(t3.ExecAny("MATCH (x:N {id:'x'}) SET x.v = 3", nil))
				if err := run(t1.ExecAny("MATCH (x:N {id:'x'}) SET x.v = 1", nil)); err == nil {
					t.Fatal("setup: T1 was not doomed by T3's pending write")
				}
				_ = t3.Rollback()
			}
			_ = t1.Rollback()
			_ = t2.Commit()
			g.ReclaimNow()
			r, err := eng.RunAny(ctx, "MATCH (:N {id:'a'})-[r:R]->(m) RETURN count(r) AS n, collect(m.id) AS ids", nil)
			if err != nil {
				t.Fatal(err)
			}
			var got any
			for r.Next() {
				got = r.Record()
			}
			_ = r.Close()
			if fmt.Sprint(got) != `map[ids:["b"] n:1]` {
				t.Errorf("the rolled-back relationship a->c is committed: %v", got)
			}
		})
	}
}

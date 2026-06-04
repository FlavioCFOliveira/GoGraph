package cypher_test

// durable_then_visible_test.go — regression tests for task #1281: the Cypher
// autocommit write path ([Engine.RunInTx]) must fsync the WAL BEFORE its
// mutations are allowed to remain visible to concurrent [lpg.Graph.View]
// readers — durable-then-visible.
//
// Before the fix the WAL fsync ([txn.Tx.CommitWALOnly]) ran later, in
// [cypher.Result.Close], AFTER the visibility barrier (visMu) was released. A
// concurrent reader could observe (and act on) a CREATE'd row, yet a crash
// before Close lost it though it had been observed — a Durability violation and
// a breach of the all-or-nothing intent. The fix moves the fsync inside the
// barrier (commitUnderBarrier): the transaction becomes durable and visible as
// one atomic step.
//
// These tests drive the PUBLIC Cypher engine. They cover:
//   - DurableThenVisible_RecoversWithoutClose: a CREATE that is visible (drained
//     from RunInTx) is already durable — recovery.Open of the WAL WITHOUT ever
//     calling Result.Close still finds the node. This is the assertion that
//     FAILS on the pre-fix code (node ABSENT) and PASSES after (node PRESENT).
//   - DurableThenVisible_ConcurrentReader: under -race, concurrent readers
//     hammering the graph during a stream of CREATEs never observe a torn or
//     partially-applied write, and every committed write is durable afterwards.
//   - WALFsyncFailure_RollsBackAndSurfaces: an injected WAL fsync failure rolls
//     the eager in-memory mutation back (not visible) AND surfaces the error —
//     never a visible-but-lost write — and releases the single-writer mutex.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// recoveredPersonNames opens dir via recovery.Open and returns the set of
// :Person node "name" properties found in the recovered graph. It is the
// durability oracle the #1281 AC turns on: a write that became visible must be
// found here even when Result.Close was never called.
func recoveredPersonNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, recOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph
	names := make(map[string]bool)
	rg.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if rg.IsTombstoned(id) {
			return true
		}
		if !rg.HasNodeLabel(key, "Person") {
			return true
		}
		if v, ok := rg.GetNodeProperty(key, "name"); ok {
			if s, sok := v.String(); sok {
				names[s] = true
			}
		}
		return true
	})
	return names
}

// TestRunInTx_DurableThenVisible_RecoversWithoutClose is the core #1281
// acceptance-criteria test.
//
// It CREATEs a node via RunInTx and drains the result so the write is visible
// to a concurrent Run reader, then DISCARDS the Result without ever calling
// Close and opens the same WAL via recovery.Open.
//
//   - Pre-fix: the fsync was deferred to Result.Close, which never ran, so the
//     WAL held no frames and the recovered graph was EMPTY — the node was
//     visible (a concurrent reader saw it) but NOT durable. This assertion
//     fails on the old code.
//   - Post-fix: the fsync runs inside the barrier before the row is visible, so
//     the recovered graph contains the node. Once visible, it is durable.
func TestRunInTx_DurableThenVisible_RecoversWithoutClose(t *testing.T) {
	eng, _, w, dir := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })
	ctx := context.Background()

	// CREATE a node and drain it through RunInTx so its writes execute and
	// become visible. We deliberately DO NOT call res.Close().
	res, err := eng.RunInTx(ctx, `CREATE (n:Person {name: "alice"})`, nil)
	if err != nil {
		t.Fatalf("RunInTx CREATE: %v", err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if derr := res.Err(); derr != nil {
		t.Fatalf("drain error: %v", derr)
	}
	// Model a CRASH before Close: a process killed with kill -9 runs no Go
	// finalizers, so the Result's leak-net finalizer must not be allowed to
	// commit the WAL behind our back. Disarm it; from here the only thing that
	// could have made the write durable is the in-barrier fsync. (Pre-fix that
	// fsync was deferred to the finalizer/Close — exactly what a crash skips —
	// so the recovered graph is empty; post-fix it already ran in the barrier.)
	runtime.SetFinalizer(res, nil)
	res = nil //nolint:ineffassign,wastedassign // drop the reference; the crash is now modelled

	// (1) A concurrent Run on the SAME engine observes the row — it is visible.
	count, err := eng.Run(ctx, `MATCH (n:Person {name: "alice"}) RETURN count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("concurrent Run: %v", err)
	}
	rows := drainRecords(t, count)
	if len(rows) != 1 || fmtAny(rows[0]["c"]) != "1" {
		t.Fatalf("concurrent reader did not observe the visible row: rows=%v", rows)
	}
	_ = count.Close()

	// (2) Recover from disk WITHOUT closing the writer (a crash flushes no
	// buffers). The in-barrier fsync (post-fix) already pushed the frames to
	// stable storage, so a fresh read handle observes them; pre-fix nothing was
	// ever appended, so recovery finds an empty graph and this assertion fails.
	names := recoveredPersonNames(t, dir)
	if !names["alice"] {
		t.Fatalf("recovered graph is missing the visible node 'alice' (visible-but-not-durable): got %v", names)
	}
}

// TestRunInTx_DurableThenVisible_ConcurrentReader is the -race assertion the AC
// asks for: a concurrent reader must never observe a write before it is durable.
//
// Post-fix the durability fsync and the visibility flip both happen inside the
// single ApplyAtomically barrier, so a Graph.View reader observes either none of
// a transaction's writes or all of them — and any write it observes is already
// durable. The test runs many concurrent MATCH readers against the live engine
// while a writer streams CREATEs; under -race it proves the in-barrier fsync
// does not introduce a torn read, and afterwards it proves every committed write
// is durable by recovering the WAL. A reader that observes a node must observe a
// CONSISTENT node (correct label + name), never a half-built one.
func TestRunInTx_DurableThenVisible_ConcurrentReader(t *testing.T) {
	eng, _, w, dir := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })
	ctx := context.Background()

	const nWrites = 60
	const nReaders = 8

	var (
		wg       sync.WaitGroup
		writeErr atomic.Pointer[error]
		readErr  atomic.Pointer[error]
		written  atomic.Int64 // highest index whose CREATE has fully returned
	)

	// Writer: stream CREATEs, each fully drained and closed (autocommit).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < nWrites; i++ {
			q := fmt.Sprintf(`CREATE (n:Person {name: "p%d", idx: %d})`, i, i)
			if err := runWrite(t, eng, q); err != nil {
				e := fmt.Errorf("CREATE %d: %w", i, err)
				writeErr.CompareAndSwap(nil, &e)
				return
			}
			written.Store(int64(i + 1))
		}
	}()

	// Readers: repeatedly count Person nodes and spot-check that any observed
	// node is fully consistent. The whole loop runs under -race against the
	// writer's in-barrier mutations.
	for r := 0; r < nReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done := false; !done; {
				if written.Load() >= nWrites {
					done = true // one final pass after the writer finishes
				}
				res, err := eng.Run(ctx, `MATCH (n:Person) RETURN n.name AS name, n.idx AS idx`, nil)
				if err != nil {
					e := fmt.Errorf("reader Run: %w", err)
					readErr.CompareAndSwap(nil, &e)
					return
				}
				for res.Next() {
					rec := res.Record()
					// A consistent node has name "p<idx>" AND idx == <idx>. A torn
					// read — the :Person label visible but a property still missing
					// or mismatched — would surface as a null/absent value or a
					// name/idx that do not agree. Post-fix the write is durable and
					// visible as one atomic step, so neither can occur.
					name, nameOK := rec["name"].(expr.StringValue)
					idx, idxOK := rec["idx"].(expr.IntegerValue)
					if !nameOK || !idxOK {
						e := fmt.Errorf("reader observed a partial Person: name=%T idx=%T", rec["name"], rec["idx"])
						readErr.CompareAndSwap(nil, &e)
						continue
					}
					if string(name) != fmt.Sprintf("p%d", int64(idx)) {
						e := fmt.Errorf("reader observed an inconsistent Person: name=%q idx=%d", string(name), int64(idx))
						readErr.CompareAndSwap(nil, &e)
					}
				}
				if derr := res.Err(); derr != nil {
					e := fmt.Errorf("reader drain: %w", derr)
					readErr.CompareAndSwap(nil, &e)
				}
				_ = res.Close()
			}
		}()
	}

	wg.Wait()
	if ep := writeErr.Load(); ep != nil {
		t.Fatalf("writer failed: %v", *ep)
	}
	if ep := readErr.Load(); ep != nil {
		t.Fatalf("reader observed inconsistency: %v", *ep)
	}

	// Every committed write must be durable: recover the WAL and confirm all
	// nWrites Person nodes are present (durable-then-visible end-to-end).
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	names := recoveredPersonNames(t, dir)
	for i := 0; i < nWrites; i++ {
		want := fmt.Sprintf("p%d", i)
		if !names[want] {
			t.Errorf("recovered graph missing durable node %q", want)
		}
	}
}

// TestRunInTx_WALFsyncFailure_RollsBackAndSurfaces injects a WAL fsync failure
// and asserts the #1281 failure contract: a write whose durability cannot be
// secured is rolled back (its eager in-memory mutation is NOT visible) and the
// error is surfaced — the engine never acknowledges a visible-but-lost write.
//
// The fsync is failed deterministically by closing the WAL writer before the
// CREATE: txn.Tx.CommitWALOnly then fails at its first Append/Sync with
// wal.ErrWriterClosed, inside the barrier, which drives commitUnderBarrier's
// fsync-failure branch (replay undo, roll back the index buffer and WAL tx).
func TestRunInTx_WALFsyncFailure_RollsBackAndSurfaces(t *testing.T) {
	quietLogs(t)
	eng, g, w, _ := walEngineWithGraph(t)
	ctx := context.Background()

	// Seed one durable node BEFORE breaking the WAL, so we can prove the
	// rollback removed ONLY the failed write and left the prior state intact.
	if err := runWrite(t, eng, `CREATE (:Person {name: "seed"})`); err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}

	// Break durability: a closed writer fails every subsequent Append and Sync.
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// The CREATE must fail: RunInTx surfaces the WAL error directly (the write is
	// neither visible nor durable, so no Result is handed back).
	res, err := eng.RunInTx(ctx, `CREATE (n:Person {name: "ghost"})`, nil)
	if err == nil {
		if res != nil {
			_ = res.Close()
		}
		t.Fatal("expected RunInTx to fail when the WAL fsync fails, got nil error")
	}
	if !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("error %v does not wrap wal.ErrWriterClosed", err)
	}

	// (1) The failed write must NOT be visible: the eager in-memory mutation was
	// rolled back inside the barrier. Only the seed node remains live.
	live := 0
	hasGhost := false
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if g.IsTombstoned(id) {
			return true
		}
		live++
		if v, ok := g.GetNodeProperty(key, "name"); ok {
			if s, sok := v.String(); sok && s == "ghost" {
				hasGhost = true
			}
		}
		return true
	})
	if hasGhost {
		t.Error("failed write 'ghost' is visible in the live graph (visible-but-not-durable)")
	}
	if live != 1 {
		t.Errorf("live node count = %d after fsync-failure rollback, want 1 (only the seed)", live)
	}

	// (2) Side-effect counters must not retain the rolled-back CREATE.
	na, nr, ea, er := g.SideEffectCounters()
	if na != 1 || nr != 0 || ea != 0 || er != 0 {
		t.Errorf("side-effect counters = (na=%d nr=%d ea=%d er=%d), want (1,0,0,0) — only the seed", na, nr, ea, er)
	}

	// (3) The single-writer mutex must have been released inside the barrier (the
	// WAL tx was rolled back), so a subsequent RunInTx does not deadlock on
	// Begin. It still fails (the WAL is closed) but must return promptly.
	done := make(chan struct{}, 1)
	go func() {
		_, _ = eng.RunInTx(ctx, `CREATE (n:Person {name: "after"})`, nil)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("subsequent RunInTx deadlocked: single-writer mutex leaked on fsync-failure path")
	}
}

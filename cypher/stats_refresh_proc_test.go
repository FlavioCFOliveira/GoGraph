package cypher_test

// stats_refresh_proc_test.go — db.stats.refresh() reachability gate (#2196).
//
// Engine.RefreshStatistics has existed since #2098 as the explicit maintenance entry
// point: planner statistics are best-effort and deliberately never maintained by a
// background goroutine, so a caller has to drive the rebuild. But it was reachable only
// from Go, which left every Bolt client — and every Cypher caller — unable to refresh
// them at all. That is the reachability half of the audit's "planner statistics are inert"
// finding, and it is separable from the larger half (giving the statistics a consumer on
// the access-path decisions), which is a no-regression-gated planner change.
//
// Layer: short.

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// statsProcEngine returns an engine over a small labelled graph with properties, so a
// refresh has something to build statistics from.
func statsProcEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 200; i++ {
		k := "s" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Stat"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return cypher.NewEngine(g)
}

// TestStatsRefreshProc_ReachableFromCypher pins that the maintenance entry point is
// callable as a procedure and reports a completed rebuild.
func TestStatsRefreshProc_ReachableFromCypher(t *testing.T) {
	t.Parallel()
	eng := statsProcEngine(t)

	res, err := eng.Run(context.Background(), `CALL db.stats.refresh()`, nil)
	if err != nil {
		t.Fatalf("CALL db.stats.refresh(): %v", err)
	}
	defer func() { _ = res.Close() }()

	rows := 0
	for res.Next() {
		rows++
		if got := res.ValueAt(0).String(); got != "true" {
			t.Errorf("ok = %s, want true — the rebuild must report success", got)
		}
		if got := res.ValueAt(1).String(); got == "" {
			t.Error("detail is empty; it must say what happened")
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if rows != 1 {
		t.Fatalf("db.stats.refresh() returned %d rows, want exactly 1 so a caller can "+
			"distinguish a completed rebuild from a no-op", rows)
	}
}

// TestStatsRefreshProc_RateLimited pins the bound that makes the capability safe to
// expose: the rebuild is an O(nodes x properties) scan reachable by any client, so a
// second call inside the window must be REFUSED as a no-op rather than run or queued.
//
// It also pins the YIELD form a real client uses. The refusal is reported in-band —
// ok=false with a reason — not as an error, because the caller did nothing wrong.
func TestStatsRefreshProc_RateLimited(t *testing.T) {
	t.Parallel()
	eng := statsProcEngine(t)

	call := func() (string, string) {
		t.Helper()
		res, err := eng.Run(context.Background(),
			`CALL db.stats.refresh() YIELD ok, detail RETURN ok, detail`, nil)
		if err != nil {
			t.Fatalf("CALL: %v", err)
		}
		var ok, detail string
		rows := 0
		for res.Next() {
			rows++
			ok, detail = res.ValueAt(0).String(), res.ValueAt(1).String()
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if rows != 1 {
			t.Fatalf("returned %d rows, want exactly 1", rows)
		}
		return ok, detail
	}

	// The first call rebuilds.
	if ok, detail := call(); ok != "true" {
		t.Fatalf("first refresh: ok=%s detail=%q, want true", ok, detail)
	}

	// Every immediately following call is refused, in-band and with a reason, and must
	// NOT be an error — the client did nothing wrong.
	for i := 0; i < 3; i++ {
		ok, detail := call()
		if ok != "false" {
			t.Fatalf("refresh %d inside the window: ok=%s, want false — an unbounded "+
				"full scan reachable by any client is an amplification vector", i, ok)
		}
		if detail == "" {
			t.Errorf("refresh %d: refusal carried no reason", i)
		}
	}
}

// TestStatsRefreshProc_HonoursCancellation pins that the procedure honours the caller's
// context: the rebuild scans the graph, so a cancelled query must abort it rather than
// run to completion, and must not publish a partial snapshot.
func TestStatsRefreshProc_HonoursCancellation(t *testing.T) {
	t.Parallel()
	eng := statsProcEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	res, err := eng.Run(ctx, `CALL db.stats.refresh()`, nil)
	if err == nil {
		for res.Next() { //nolint:revive // drain
		}
		err = res.Err()
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("a cancelled context did not abort db.stats.refresh(); the rebuild must " +
			"honour cancellation rather than run to completion")
	}
}

// TestStatsRefreshProc_DoesNotNestTheBarrier is the regression gate for a defect that
// would have DEADLOCKED a production binary (#2196).
//
// Engine.RefreshStatistics wraps its scan in Graph.View. A procedure, however, runs inside
// query execution, which is already inside Graph.View — and visMu is a non-re-entrant
// sync.RWMutex, so acquiring it again from the same goroutine hangs. The engine's
// re-entrancy guard converts that into a panic, but the guard is compiled out of ordinary
// builds (#2168 removed it from the production read path), so a plain binary would simply
// stop. The first version of this procedure had exactly that bug, and only the race-enabled
// gate caught it.
//
// The test asserts the fix behaviourally: the call completes. A nested acquisition either
// panics (debug/race build, surfaced as a query error) or hangs (ordinary build, caught by
// the test timeout), so a regression cannot pass either way.
//
// It also drives the call under CONCURRENT readers, because the barrier the procedure runs
// beneath is shared: a rebuild must not exclude readers, and readers must not starve it.
func TestStatsRefreshProc_DoesNotNestTheBarrier(t *testing.T) {
	t.Parallel()
	eng := statsProcEngine(t)

	// Concurrent readers hold the read barrier for the duration.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := eng.Run(context.Background(), `MATCH (n:Stat) RETURN count(n)`, nil)
			if err != nil {
				return
			}
			for res.Next() { //nolint:revive // drain
			}
			_ = res.Close()
		}
	}()

	res, err := eng.Run(context.Background(), `CALL db.stats.refresh()`, nil)
	if err != nil {
		close(stop)
		<-done
		t.Fatalf("db.stats.refresh() inside query execution failed: %v — a nested "+
			"Graph.View acquisition would deadlock a production binary", err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		close(stop)
		<-done
		t.Fatalf("Err: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(stop)
	<-done

	if rows != 1 {
		t.Fatalf("returned %d rows, want 1", rows)
	}
}

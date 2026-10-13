package cypher_test

// plan_cancel_churn_3010_test.go — rmp #3010 and #3011.
//
// Under heavy uncommitted churn in a label, planning a statement over it used to
// run a full MVCC correction of the label bitmap per plan site — only to read
// its cardinality — and no part of it observed the statement's context. Example
// 37's L17 measured a statement cancelled 2 ms in returning ~86 ms later (~670 ms
// under the race detector). Each scan's Init then repeated the correction, and
// an Apply re-Inits its inner scan once per outer row.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// cancelAfterFirstCheck reports no error to the statement's opening context
// check and context.Canceled to every later one, so the cancellation lands
// deterministically AFTER Run has accepted the statement and BEFORE it plans.
type cancelAfterFirstCheck struct {
	context.Context //nolint:containedctx // the type IS a context: it overrides Err only
	calls           atomic.Int32
}

func (c *cancelAfterFirstCheck) Err() error {
	if c.calls.Add(1) > 1 {
		return context.Canceled
	}
	return nil
}

// churnedEngine returns an engine whose label :P holds `committed` committed
// nodes and holders x perHolder more created by transactions left open until the
// test ends.
func churnedEngine(t *testing.T, committed, holders, perHolder int) *cypher.Engine {
	t.Helper()
	ctx := context.Background()
	g := lpg.New[string, float64](adjlist.Config{})
	e := cypher.NewEngine(g)
	if _, err := e.RunAny(ctx, "UNWIND range(1, $n) AS i CREATE (:P {i: i})", map[string]any{"n": committed}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for range holders {
		tx, err := e.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		res, err := tx.Exec("UNWIND range(1, $n) AS i CREATE (:P {i: -i})",
			map[string]expr.Value{"n": expr.IntegerValue(int64(perHolder))})
		if err != nil {
			t.Fatalf("uncommitted create: %v", err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("uncommitted create: %v", err)
		}
	}
	return e
}

// TestPlanning_UnderChurn_DoesNoLabelCorrection_3010 is the regression gate for
// rmp #3010. A statement cancelled after Run accepted it must return the context
// error without planning paying for any MVCC correction of :P.
//
// The observable is the bytes the statement allocates. One correction of :P
// samples every uncommitted node at least twice into a slice of 8-byte ids
// before it deduplicates them, so it cannot allocate less than 16 bytes per
// uncommitted node; the gate allows half of ONE such correction, a structural
// floor rather than a proportion of a measured run. Before the fix the join
// reorder's two plan-choice estimates each ran one; after it they read O(1)
// counts and the statement allocates only for parsing and planning.
func TestPlanning_UnderChurn_DoesNoLabelCorrection_3010(t *testing.T) {
	const committed, holders, perHolder = 300, 8, 2500
	e := churnedEngine(t, committed, holders, perHolder)
	const q = "MATCH (a:P), (b:P) RETURN count(*) AS c"
	// Warm the plan cache so the measurement does not include parsing.
	warm, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	_ = warm.Close()

	floor := uint64(16 * holders * perHolder / 2)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	c := &cancelAfterFirstCheck{Context: context.Background()}
	res, err := e.Run(c, q, nil)
	if err == nil {
		for res.Next() {
		}
		err = res.Err()
		_ = res.Close()
	}
	runtime.ReadMemStats(&after)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if c.calls.Load() < 2 {
		t.Fatalf("the context was read %d time(s): the cancellation never landed", c.calls.Load())
	}
	got := after.TotalAlloc - before.TotalAlloc
	t.Logf("cancelled statement allocated %d bytes (floor %d)", got, floor)
	if got >= floor {
		t.Fatalf("the cancelled statement allocated %d bytes, at least half of one MVCC correction "+
			"of :P (%d bytes): planning corrected the label bitmap", got, floor)
	}
}

// TestCartesianOverChurnedLabel_ResultUnchanged_3011 pins the answer of the
// shape whose inner scan is re-Initialised once per outer row, under churn: the
// per-statement memo (rmp #3011) must not change it, and uncommitted nodes stay
// invisible to it.
func TestCartesianOverChurnedLabel_ResultUnchanged_3011(t *testing.T) {
	const committed = 200
	e := churnedEngine(t, committed, 4, 1000)
	for _, q := range []string{
		"MATCH (a:P), (b:P) RETURN count(*) AS c",
		"MATCH (a:P), (b:P) WHERE a.i < b.i RETURN count(*) AS c",
		"MATCH (n:P) RETURN count(n) AS c",
	} {
		res, err := e.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var got int64 = -1
		for res.Next() {
			v, ok := res.Record()["c"].(expr.IntegerValue)
			if !ok {
				t.Fatalf("%s: c is %T, want an integer", q, res.Record()["c"])
			}
			got = int64(v)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var want int64
		switch q {
		case "MATCH (n:P) RETURN count(n) AS c":
			want = committed
		case "MATCH (a:P), (b:P) RETURN count(*) AS c":
			want = committed * committed
		default:
			want = committed * (committed - 1) / 2
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", q, got, want)
		}
	}
}

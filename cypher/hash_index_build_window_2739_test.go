package cypher

// hash_index_build_window_2739_test.go — rmp #2739: an explicit transaction's
// commit-time index fan-out that lands between the hash path's backfill scan and
// its registration reaches BOTH indexes CREATE INDEX registers.
//
// Layer: short.
//
// The hash path registers the user index and its numeric companion without the
// visibility barrier the btree path wraps them in (rmp #2703). The barrier is
// not what keeps a writer's fan-out out of the window on either path: an
// explicit transaction delivers its buffered changes at publication, through
// [index.Manager.ApplyBatchInState], which records them into every build in
// flight under the same shared hold of the manager's lock as the delivery
// itself; [index.Manager.FinishBuild] replays that recording into each index and
// registers it under the exclusive hold (rmp #2738). This test drives that
// window deterministically and shows the fan-out lands in it: after the backfill
// the indexes lack the transaction's nodes, the build log holds their changes,
// and after the registration both indexes carry them.
//
// It makes exactly the calls [Engine.createHashIndexLocked] makes, in its order,
// with the committing transaction placed between the backfill and the
// registration — the white-box shape of index_build_replay_test.go, deterministic
// for the same reason. Both wirings run: the persisted store (store.Open, WAL,
// durable commit) and the in-memory engine.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// hashWindowEngine returns an engine over the persisted store or over an
// in-memory graph, seeded through Cypher with 32 :L nodes carrying string values
// and 32 carrying integer values of s.
func hashWindowEngine(t *testing.T, persisted bool) *Engine {
	t.Helper()
	var e *Engine
	if persisted {
		o, err := store.Open[string, float64](t.TempDir(), store.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() {
			if err := o.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
		e = NewEngineWithOpened(o)
	} else {
		e = NewEngine(lpg.New[string, float64](adjlist.Config{}))
	}
	e.parallelBackfillEnabled = false
	execStatement(t, e, `UNWIND range(0, 31) AS i CREATE (:L {s: 'seed-' + toString(i)})`)
	execStatement(t, e, `UNWIND range(100, 131) AS i CREATE (:L {s: i})`)
	return e
}

func TestHashIndexBuild_ExplicitCommitBetweenBackfillAndRegistration_2739(t *testing.T) {
	t.Parallel()
	for _, persisted := range []bool{true, false} {
		name := "mem"
		if persisted {
			name = "store"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e := hashWindowEngine(t, persisted)
			mgr := e.g.IndexManager()
			const idxName = "ls"
			numName := numericBTreeName("L", "s")

			// createHashIndexLocked, up to the end of both backfills.
			buildLog, scanView, release, err := e.beginIndexBuild(ctx, mgr, "L", "s")
			if err != nil {
				t.Fatalf("beginIndexBuild: %v", err)
			}
			defer mgr.AbandonBuild(buildLog)
			defer release()
			idx, err := newBoundNodeHashIndex(e.g.ReadAt(nil), "L", "s")
			if err != nil {
				t.Fatalf("bind hash: %v", err)
			}
			if err := e.backfillNodeHashIndex(ctx, scanView, idx, "L", "s", nil); err != nil {
				t.Fatalf("backfill hash: %v", err)
			}
			numIdx, err := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), "L", "s")
			if err != nil {
				t.Fatalf("bind numeric companion: %v", err)
			}
			if err := e.backfillNodeBTreeIndexNumeric(ctx, scanView, numIdx, "L", "s"); err != nil {
				t.Fatalf("backfill numeric companion: %v", err)
			}
			release()

			// The window: an explicit transaction commits, through a session, after
			// both scans and before the registration. Its delivery reaches no
			// registered index.
			tx, err := e.NewSession().BeginTx(ctx)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			for _, q := range []string{`CREATE (:L {s: 'late'})`, `CREATE (:L {s: 4242})`} {
				res, err := tx.ExecAny(q, nil)
				if err != nil {
					t.Fatalf("Exec %q: %v", q, err)
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					t.Fatalf("drain %q: %v", q, err)
				}
				_ = res.Close()
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}

			// Non-vacuity: the fan-out landed IN the window. The scans did not
			// carry the nodes, and the change reached the build log. An empty log is
			// reported without stopping, so the loss it causes is reported too.
			if c := idx.Cardinality("late"); c != 0 {
				t.Fatalf("the backfill already holds 'late' (%d): the commit did not land after the scan", c)
			}
			if c := numIdx.Cardinality(4242); c != 0 {
				t.Fatalf("the companion backfill already holds 4242 (%d): the commit did not land after the scan", c)
			}
			if buildLog.Len() == 0 {
				t.Error("the build log recorded nothing: the commit's fan-out did not reach the build window")
			}

			// createHashIndexLocked's registration: both indexes under one FinishBuild.
			if err := mgr.FinishBuild(buildLog, func(reg index.RegisterFunc) error {
				if err := reg(idxName, idx); err != nil {
					return err
				}
				return reg(numName, numIdx)
			}); err != nil {
				t.Fatalf("FinishBuild: %v", err)
			}
			e.ClearPlanCache()

			if c := idx.Cardinality("late"); c != 1 {
				t.Errorf("hash index holds %d entries for 'late', want 1: the fan-out in the window was lost", c)
			}
			if c := numIdx.Cardinality(4242); c != 1 {
				t.Errorf("numeric companion holds %d entries for 4242, want 1: the fan-out in the window was lost", c)
			}
			for _, probe := range []struct{ query, op string }{
				{`MATCH (n:L) WHERE n.s = 'late' RETURN n`, "NodeByIndexSeek"},
				{`MATCH (n:L) WHERE n.s = 4242 RETURN n`, "NodeByIndex"},
			} {
				if plan := planOf(t, e, probe.query, nil); !strings.Contains(plan, probe.op) {
					t.Fatalf("%s: the row assertion would be vacuous, the plan uses no %s:\n%s", probe.query, probe.op, plan)
				}
				if got := rowsOf(t, e, probe.query, nil); got != 1 {
					t.Errorf("%s: %d rows through the index, want 1", probe.query, got)
				}
			}
			if got := rowsOf(t, e, fmt.Sprintf(`MATCH (n:L) WHERE n.s + '' = '%s' RETURN n`, "late"), nil); got != 1 {
				t.Errorf("label scan for 'late' returned %d rows, want 1", got)
			}
		})
	}
}

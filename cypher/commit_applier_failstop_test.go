package cypher

// commit_applier_failstop_test.go — an index delivery cut short by a panic
// fail-stops the engine (rmp #2931 re-audit, N2).
//
// Layer: short.
//
// The panic is injected with the engine's delivery test seam, inside the
// commit-time applier. Afterwards the engine must refuse every operation with
// [ErrEngineFailStopped] — its indexes may hold part of a commit — while still
// accepting a Rollback. The graph must agree with the durable log: on the WAL
// wiring the commit was already durable, so it is visible in memory and survives
// a reopen; on the in-memory wiring it is aborted.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// failStopLabelled counts the nodes visible now that carry label.
func failStopLabelled(g *lpg.Graph[string, float64], label string) int {
	snap := g.BeginRead()
	defer g.EndRead(snap)
	view := g.ReadAt(snap)
	var ids []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	n := 0
	for _, id := range ids {
		if view.Exists(id) && view.HasNodeLabelByID(id, label) {
			n++
		}
	}
	return n
}

func TestCommitApplier_PanicFailStopsTheEngine(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		name := "mem"
		if walBacked {
			name = "wal"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			g := lpg.New[string, float64](adjlist.Config{})
			var eng *Engine
			dir := t.TempDir()
			var wr *wal.Writer
			if walBacked {
				var err error
				wr, err = wal.Open(filepath.Join(dir, "wal"))
				if err != nil {
					t.Fatal(err)
				}
				st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
					Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
				})
				eng = NewEngineWithStore(st)
			} else {
				eng = NewEngine(g)
			}
			commitStateExec(t, eng, "CREATE INDEX ls FOR (n:P) ON (n.s)")

			// A second transaction, open across the fail-stop, must still be able to
			// roll back and must not be able to commit.
			bystander, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := bystander.ExecAny(`CREATE (:B {s: 'b'})`, nil); err != nil {
				t.Fatal(err)
			} else {
				for res.Next() {
				}
				_ = res.Close()
			}

			tx, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			res, err := tx.ExecAny(`CREATE (:P {s: 'p'})`, nil)
			if err != nil {
				t.Fatal(err)
			}
			for res.Next() {
			}
			_ = res.Close()
			eng.indexDeliveredHookForTest = func() { panic("test: index delivery") }
			cerr := tx.Commit()
			eng.indexDeliveredHookForTest = nil
			if cerr == nil {
				t.Fatal("fixture: Commit returned nil although its index delivery panicked")
			}

			for _, op := range []struct {
				name string
				err  error
			}{
				{"Run", func() error { _, err := eng.Run(ctx, "MATCH (n) RETURN count(n) AS c", nil); return err }()},
				{"RunAny write", func() error { _, err := eng.RunAny(ctx, "CREATE (:X)", nil); return err }()},
				{"RunAny DDL", func() error { _, err := eng.RunAny(ctx, "CREATE INDEX lx FOR (n:X) ON (n.s)", nil); return err }()},
				{"BeginTx", func() error { _, err := eng.BeginTx(ctx); return err }()},
				{"Explain", func() error { _, err := eng.Explain("MATCH (n) RETURN n", nil); return err }()},
				{"open tx Exec", func() error { _, err := bystander.ExecAny("CREATE (:B)", nil); return err }()},
			} {
				if !errors.Is(op.err, ErrEngineFailStopped) || !errors.Is(op.err, index.ErrIndexStateUndefined) {
					t.Errorf("%s after the fail-stop returned %v, want ErrEngineFailStopped wrapping ErrIndexStateUndefined", op.name, op.err)
				}
			}
			bystander2, _ := eng.BeginTx(ctx) // refused: nil
			if bystander2 != nil {
				t.Error("BeginTx returned a transaction on a fail-stopped engine")
			}
			if err := bystander.Commit(); !errors.Is(err, ErrEngineFailStopped) {
				t.Errorf("an open transaction's Commit after the fail-stop returned %v, want ErrEngineFailStopped", err)
			}
			if got := failStopLabelled(g, "B"); got != 0 {
				t.Errorf("the refused commit is visible: %d :B nodes", got)
			}

			visible := failStopLabelled(g, "P")
			if walBacked {
				if visible != 1 {
					t.Fatalf("%d :P nodes visible in memory, want 1: the commit was durable before its "+
						"delivery panicked, so memory must agree with the WAL", visible)
				}
				if err := wr.Close(); err != nil {
					t.Fatal(err)
				}
				rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
					Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
				})
				if err != nil {
					t.Fatalf("recovery.Open: %v", err)
				}
				if got := failStopLabelled(rec.Graph, "P"); got != 1 {
					t.Fatalf("%d :P nodes after reopening, want 1 — the same as memory held", got)
				}
				reopened := NewEngine(rec.Graph)
				if c := commitStateCount(t, reopened, "MATCH (n:P) RETURN count(n) AS c"); c != 1 {
					t.Fatalf("the reopened engine counts %d :P nodes, want 1", c)
				}
				return
			}
			if visible != 0 {
				t.Fatalf("%d :P nodes visible, want 0: the in-memory commit whose delivery panicked was not aborted", visible)
			}
		})
	}
}

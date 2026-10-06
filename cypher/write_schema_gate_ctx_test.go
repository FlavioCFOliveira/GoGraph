package cypher

// write_schema_gate_ctx_test.go — rmp #2984: an autocommit write honours its
// context while it waits for the schema gate.
//
// # The defect
//
// An autocommit write takes Engine.schemaGate shared for its whole statement, and
// it took it with the context-free WeakLockAuto. A DDL holds the gate exclusively
// for its whole backfill, so a write carrying a 50 ms deadline that arrived behind
// one waited until the DDL ended and only then looked at its context.
//
// # What is asserted
//
// The gate is held exclusively by the test itself, which is the state a DDL puts
// it in for the length of its backfill. For every autocommit write entry point, on
// the in-memory and the WAL-backed engine:
//
//   - the write returns an error matching context.DeadlineExceeded BEFORE the gate
//     is released;
//   - the write applied nothing: no node is visible once the gate is released and,
//     on the WAL-backed engine, recovery of the WAL yields no node either;
//   - a retry with no deadline, after the release, commits, which is the positive
//     control for both "nothing" checks above.
//
// The 50 ms deadline is the input under test, not a latency bound: the only
// ordering asserted is "the write returned before the gate was released". The
// backstop only keeps a regression from hanging the package; reaching it IS the
// failure.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// writeGateBackstop bounds the wait for a write that does not honour its context,
// so a regression fails the test rather than hanging it. It is not a latency bound.
const writeGateBackstop = 10 * time.Second

// writeGateEntries is every public autocommit write entry point, including the
// PROFILE form, which takes the same write path with a profiler installed.
var writeGateEntries = []struct {
	name string
	run  func(ctx context.Context, e *Engine, q string) (*Result, error)
}{
	{"RunInTx", func(ctx context.Context, e *Engine, q string) (*Result, error) {
		return e.RunInTx(ctx, q, nil)
	}},
	{"RunInTx/PROFILE", func(ctx context.Context, e *Engine, q string) (*Result, error) {
		return e.RunInTx(ctx, "PROFILE "+q, nil)
	}},
	{"RunAny", func(ctx context.Context, e *Engine, q string) (*Result, error) {
		return e.RunAny(ctx, q, nil)
	}},
	{"RunInTxAny", func(ctx context.Context, e *Engine, q string) (*Result, error) {
		return e.RunInTxAny(ctx, q, nil)
	}},
	{"Session.RunInTx", func(ctx context.Context, e *Engine, q string) (*Result, error) {
		return e.NewSession().RunInTx(ctx, q, nil)
	}},
	{"Session.RunAny", func(ctx context.Context, e *Engine, q string) (*Result, error) {
		return e.NewSession().RunAny(ctx, q, nil)
	}},
}

// writeGateExec runs q through run and drains its result.
func writeGateExec(ctx context.Context, e *Engine, run func(context.Context, *Engine, string) (*Result, error), q string) error {
	res, err := run(ctx, e, q)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	return errors.Join(res.Err(), res.Close())
}

// writeGateCount returns the number of :W nodes e sees.
func writeGateCount(t *testing.T, e *Engine) int64 {
	t.Helper()
	res, err := e.Run(context.Background(), "MATCH (n:W) RETURN count(n) AS c", nil)
	if err != nil {
		t.Fatalf("count :W: %v", err)
	}
	var c int64
	for res.Next() {
		v, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			t.Fatalf("count :W: non-integer %v", res.ValueAt(0))
		}
		c = int64(v)
	}
	if err := errors.Join(res.Err(), res.Close()); err != nil {
		t.Fatalf("count :W: %v", err)
	}
	return c
}

func TestAutocommitWrite_HonoursDeadlineBehindHeldSchemaGate(t *testing.T) {
	t.Parallel()
	for _, wiring := range []string{"memory", "wal"} {
		for _, entry := range writeGateEntries {
			t.Run(wiring+"/"+entry.name, func(t *testing.T) {
				t.Parallel()
				var (
					e   *Engine
					dir string
					w   *wal.Writer
				)
				g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
				if wiring == "memory" {
					e = NewEngine(g)
				} else {
					dir = t.TempDir()
					var err error
					w, err = wal.Open(filepath.Join(dir, "wal"))
					if err != nil {
						t.Fatalf("wal.Open: %v", err)
					}
					t.Cleanup(func() { _ = w.Close() })
					e = NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
						Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
					}))
				}
				t.Cleanup(func() { _ = e.Close() })
				const q = "CREATE (:W {k:1})"

				// Hold the gate exclusively, as a DDL does for its whole backfill.
				e.schemaGate.StrongLock()
				held := true
				release := func() {
					if held {
						held = false
						e.schemaGate.StrongUnlock()
					}
				}
				t.Cleanup(release)

				done := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
					defer cancel()
					done <- writeGateExec(ctx, e, entry.run, q)
				}()
				var werr error
				select {
				case werr = <-done:
				case <-time.After(writeGateBackstop):
					release()
					werr = <-done
					t.Fatalf("%s: the write did not return while the schema gate was held; "+
						"it returned %v only after the release", entry.name, werr)
				}
				if !errors.Is(werr, context.DeadlineExceeded) {
					t.Fatalf("%s behind a held schema gate: err = %v, want context.DeadlineExceeded",
						entry.name, werr)
				}

				release()
				if n := writeGateCount(t, e); n != 0 {
					t.Fatalf("%s: the refused write applied %d :W nodes, want 0", entry.name, n)
				}
				if w != nil {
					if err := w.Sync(); err != nil {
						t.Fatalf("wal.Sync: %v", err)
					}
					res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
						Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
					})
					if err != nil {
						t.Fatalf("recovery.Open: %v", err)
					}
					if n := writeGateCount(t, NewEngine(res.Graph)); n != 0 {
						t.Fatalf("%s: the refused write reached the WAL: %d :W nodes recovered, want 0",
							entry.name, n)
					}
				}

				// Positive control: the same write with no deadline commits.
				if err := writeGateExec(context.Background(), e, entry.run, q); err != nil {
					t.Fatalf("%s retry: %v", entry.name, err)
				}
				if n := writeGateCount(t, e); n != 1 {
					t.Fatalf("%s retry: %d :W nodes, want 1", entry.name, n)
				}
			})
		}
	}
}

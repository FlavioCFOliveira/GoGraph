package cypher_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// noopExecInTx runs q inside tx and drains its result.
func noopExecInTx(t *testing.T, tx *cypher.ExplicitTx, q string) {
	t.Helper()
	r, err := tx.Exec(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for r.Next() {
	}
	if err := r.Close(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// noopRunAuto runs q as an autocommit statement over the store-backed engine.
func noopRunAuto(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	r, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for r.Next() {
	}
	if err := r.Close(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// noopScalar runs q over g and returns its single column c.
func noopScalar(t *testing.T, g *lpg.Graph[string, float64], q string) string {
	t.Helper()
	r, err := cypher.NewEngine(g).Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer r.Close()
	out := ""
	for r.Next() {
		out += fmt.Sprint(r.Record()["c"])
	}
	if err := r.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// TestCypherWAL_NoOpWriteIsNotLoggedSoReplayMatchesMemory pins rmp #2965 round
// 5, finding R5-F1, on the Cypher commit path. An explicit transaction T1 runs
// a write that changes nothing — re-asserting a present label, setting a
// property to its own value, removing an absent label — so it holds no claim;
// an autocommit statement then changes the same object and commits first; T1
// commits. Before the fix T1 logged its no-op after the autocommit's record and
// replay re-applied it there, where it was no longer a no-op: memory held 0
// `:L` nodes and recovery 1.
func TestCypherWAL_NoOpWriteIsNotLoggedSoReplayMatchesMemory(t *testing.T) {
	cases := []struct {
		name, seed, noop, conflicting, probe string
	}{
		{"SET present label vs REMOVE label",
			"CREATE (:L {id:'x'})", "MATCH (n {id:'x'}) SET n:L", "MATCH (n {id:'x'}) REMOVE n:L",
			"MATCH (n:L) RETURN count(n) AS c"},
		{"SET same value vs REMOVE property",
			"CREATE ({id:'x', p:1})", "MATCH (n {id:'x'}) SET n.p = 1", "MATCH (n {id:'x'}) REMOVE n.p",
			"MATCH (n {id:'x'}) RETURN n.p AS c"},
		{"REMOVE absent label vs SET label",
			"CREATE ({id:'x'})", "MATCH (n {id:'x'}) REMOVE n:L", "MATCH (n {id:'x'}) SET n:L",
			"MATCH (n:L) RETURN count(n) AS c"},
		{"REMOVE absent property vs SET property",
			"CREATE ({id:'x'})", "MATCH (n {id:'x'}) REMOVE n.p", "MATCH (n {id:'x'}) SET n.p = 5",
			"MATCH (n {id:'x'}) RETURN n.p AS c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
			eng := cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, opts))
			noopRunAuto(t, eng, c.seed)
			t1, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			noopExecInTx(t, t1, c.noop)
			noopRunAuto(t, eng, c.conflicting)
			if err := t1.Commit(); err != nil {
				t.Fatalf("T1 commit: %v", err)
			}
			mem := noopScalar(t, g, c.probe)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
			if err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if rec := noopScalar(t, res.Graph, c.probe); rec != mem {
				t.Errorf("%s: acknowledged memory %q, recovered %q", c.probe, mem, rec)
			}
		})
	}
}

package cypher_test

// explicit_tx_counters_3004_test.go — regression gate for rmp #3004.
//
// A statement inside an explicit transaction must report its own update counters,
// exactly as the same statement run in autocommit does. The official driver reads
// them from the RUN's PULL SUCCESS `stats` for a transaction statement and an
// autocommit one alike (neo4j-go-driver v5.28.4, neo4j/internal/bolt/bolt5.go:
// Run and RunTx share b.run, and extractSummary consumes the same SUCCESS), and
// its own suite asserts them against a live server before the commit
// (test-stress/executors.go WriteQueryInTxExecutor: tx.Run("CREATE ()") then
// Counters().NodesCreated() == 1; neo4j/test-integration/bookmark_test.go inside
// ExecuteWrite). ExplicitTx.Exec built its adapters without counters, so every
// such statement reported nil.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func counters3004Engine(t *testing.T, durable bool) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	var eng *cypher.Engine
	if durable {
		w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		}))
		t.Cleanup(func() { _ = eng.Close(); _ = w.Close() })
	} else {
		eng = cypher.NewEngine(g)
		t.Cleanup(func() { _ = eng.Close() })
	}
	res, err := eng.RunInTx(context.Background(), "CREATE (:A {k:1})-[:R {w:1}]->(:A {k:2})", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	return eng
}

// counters3004Statements covers every counter a write clause can move: node and
// relationship creation and deletion, property set and removal on both, and
// label add and removal.
var counters3004Statements = []string{
	"CREATE (:B {x:1, y:2})",
	"MATCH (n:A {k:1}) SET n.y = 1, n:L",
	"MATCH (n:A {k:1}) REMOVE n.y, n:L",
	"MATCH (a:A {k:1}), (b:A {k:2}) CREATE (a)-[:S {v:1}]->(b)",
	"MATCH ()-[r:R]->() SET r.w = 2",
	"MATCH ()-[r:R]->() REMOVE r.w",
	"MATCH ()-[r:R]->() DELETE r",
	"MATCH (n:B) DELETE n",
}

// TestExplicitTxStatementCounters_3004 runs the same statements in autocommit on
// one engine and, one after another, inside ONE explicit transaction on a twin
// engine, and requires each explicit statement to report exactly the counters
// its autocommit twin reported — its own, not the transaction's running total.
func TestExplicitTxStatementCounters_3004(t *testing.T) {
	t.Parallel()
	for _, durable := range []bool{false, true} {
		name := "memory"
		if durable {
			name = "wal"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			auto, expl := counters3004Engine(t, durable), counters3004Engine(t, durable)
			tx, err := expl.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range counters3004Statements {
				ares, err := auto.RunInTx(ctx, q, nil)
				if err != nil {
					t.Fatalf("autocommit %s: %v", q, err)
				}
				for ares.Next() {
				}
				if err := ares.Close(); err != nil {
					t.Fatalf("autocommit %s: %v", q, err)
				}
				want := ares.Counters()
				if want == nil || *want == (exec.QueryCounters{}) {
					t.Fatalf("setup: autocommit %s reported no counters (%v)", q, want)
				}

				eres, err := tx.Exec(q, nil)
				if err != nil {
					t.Fatalf("explicit %s: %v", q, err)
				}
				for eres.Next() {
				}
				if err := eres.Close(); err != nil {
					t.Fatalf("explicit %s: %v", q, err)
				}
				got := eres.Counters()
				if got == nil {
					t.Errorf("explicit %s: no counters, want %+v", q, *want)
					continue
				}
				if *got != *want {
					t.Errorf("explicit %s: counters %+v, want %+v", q, *got, *want)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		})
	}
}

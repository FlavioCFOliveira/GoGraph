package cypher_test

// tombstone_reopen_test.go — the observable node-deletion durability bug,
// reproduced through the PUBLIC Cypher engine across store reopens. This is
// the GoGraph-level analogue of the Groadmap `rmp graph` symptom: each
// open→run→checkpoint→close block is a separate process that reopens the
// store from disk, so a non-durable tombstone resurrects the deleted node.
//
// The existing Cypher delete tests run inside a SINGLE engine, where
// AllNodesScan filters tombstoned ids regardless of durability — they
// cannot see this bug. This test discards the engine and graph between
// every step and reopens from disk before asserting.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func tombStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

func tombRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// runCypherCheckpointClose opens a Cypher engine over g+w, runs each query
// (write queries persist to the WAL on Result.Close), then writes a
// self-sufficient snapshot, truncates the WAL, and closes it — exactly one
// `rmp graph` command's lifecycle.
func runCypherCheckpointClose(t *testing.T, dir string, g *lpg.Graph[string, float64], w *wal.Writer, queries ...string) {
	t.Helper()
	store := txn.NewStoreWithOptions[string, float64](g, w, tombStoreOpts())
	eng := cypher.NewEngineWithStore(store)
	ctx := context.Background()
	for _, q := range queries {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("RunAny(%q): %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional drain to run the write to completion
		}
		if rerr := res.Err(); rerr != nil {
			_ = res.Close()
			t.Fatalf("result error for %q: %v", q, rerr)
		}
		if cerr := res.Close(); cerr != nil { // Close commits the write transaction
			t.Fatalf("Close(%q): %v", q, cerr)
		}
	}
	var mu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &mu)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// TestCypher_DeleteSurvivesReopen drives CREATE → DETACH DELETE → count
// across three store reopens and asserts the deleted node does not return.
func TestCypher_DeleteSurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	ctx := context.Background()

	// open 1: CREATE (a:Spec {key:'auth'})
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true})
	runCypherCheckpointClose(t, dir, g1, w1, `CREATE (a:Spec {key:'auth'})`)

	// open 2: MATCH (n) DETACH DELETE n
	res2, err := recovery.Open[string, float64](dir, tombRecOpts())
	if err != nil {
		t.Fatalf("open2 recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	runCypherCheckpointClose(t, dir, res2.Graph, w2, `MATCH (n) DETACH DELETE n`)

	// open 3: MATCH (n) RETURN count(n) ⇒ 0, and id(n) returns no rows.
	res3, err := recovery.Open[string, float64](dir, tombRecOpts())
	if err != nil {
		t.Fatalf("open3 recovery.Open: %v", err)
	}
	eng3 := cypher.NewEngine(res3.Graph)

	cnt, err := eng3.RunAny(ctx, `MATCH (n) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	rows := collectRecords(t, cnt)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	mustInt(t, "count(n)", rows[0]["c"], 0)

	idq, err := eng3.RunAny(ctx, `MATCH (n) RETURN id(n) AS i, labels(n) AS l`, nil)
	if err != nil {
		t.Fatalf("id query: %v", err)
	}
	if n := countRows(t, idq); n != 0 {
		t.Fatalf("MATCH (n) RETURN id(n) returned %d rows, want 0 — a stripped-label ghost survived", n)
	}
}

// TestCypher_DeleteThenRecreateAcrossReopen deletes a node in one reopen and
// re-creates the same label/property in a later reopen; the graph must hold
// exactly one live node, never zero (ghost-blocked) or two (duplicate).
func TestCypher_DeleteThenRecreateAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	ctx := context.Background()

	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true})
	runCypherCheckpointClose(t, dir, g1, w1, `CREATE (a:Spec {key:'auth'})`)

	res2, err := recovery.Open[string, float64](dir, tombRecOpts())
	if err != nil {
		t.Fatalf("open2 recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	runCypherCheckpointClose(t, dir, res2.Graph, w2, `MATCH (n {key:'auth'}) DETACH DELETE n`)

	res3, err := recovery.Open[string, float64](dir, tombRecOpts())
	if err != nil {
		t.Fatalf("open3 recovery.Open: %v", err)
	}
	w3, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open3 wal.Open: %v", err)
	}
	runCypherCheckpointClose(t, dir, res3.Graph, w3, `CREATE (a:Spec {key:'auth'})`)

	res4, err := recovery.Open[string, float64](dir, tombRecOpts())
	if err != nil {
		t.Fatalf("open4 recovery.Open: %v", err)
	}
	eng4 := cypher.NewEngine(res4.Graph)
	cnt, err := eng4.RunAny(ctx, `MATCH (n:Spec) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	rows := collectRecords(t, cnt)
	mustInt(t, "count(:Spec)", rows[0]["c"], 1)
}

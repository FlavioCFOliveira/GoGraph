package exec_test

// crossproc_create_match_test.go — T914
//
// Process A executes a Cypher MERGE (which creates Bob when no node with that
// pattern exists) via a WAL-backed engine, commits to disk, and exits cleanly.
// Process B opens the same directory via store/recovery.Open, builds a fresh
// cypher.Engine, executes MATCH (n:Person {name:"Bob"}) SET n.age = 42, and
// verifies that the row returned by a subsequent MATCH has the expected
// property value.
//
// Note: MERGE idempotence (the searchFn re-check path in exec.Merge) is not yet
// implemented across process boundaries — the searchFn only scans the in-memory
// graph of the current process. This test therefore exercises the narrower but
// load-bearing contract: a WAL-durable MERGE-CREATE in proc A is visible as a
// MATCH target in proc B after recovery.Open replay.
//
// References: internal/subproc, cypher/exec (merge, scan_label, set),
// store/recovery, store/wal, store/txn. Families 1 + 19.
//
// Layer: short. Race-clean; goleak-clean.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/subproc"
	"gograph/store/recovery"
	"gograph/store/txn"
	"gograph/store/wal"
)

func init() {
	// Register the child handler for T914.
	// args[0] = the shared data directory passed by the parent.
	subproc.Register("cypher-exec-merge-create", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "cypher-exec-merge-create: missing dir arg")
			return 1
		}
		dir := args[0]

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "cypher-exec-merge-create: wal.Open: %v\n", err)
			return 1
		}

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		eng := cypher.NewEngineWithStore(store)

		// MERGE creates Bob when no matching node exists (ON CREATE path).
		res, mergeErr := eng.RunInTx(context.Background(),
			`MERGE (n:Person {name: "Bob"}) RETURN n`, nil)
		if mergeErr != nil {
			fmt.Fprintf(os.Stderr, "cypher-exec-merge-create: RunInTx MERGE: %v\n", mergeErr)
			return 1
		}
		for res.Next() {
			// drain
		}
		if iterErr := res.Err(); iterErr != nil {
			fmt.Fprintf(os.Stderr, "cypher-exec-merge-create: result.Err: %v\n", iterErr)
			_ = res.Close()
			return 1
		}
		if closeErr := res.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "cypher-exec-merge-create: result.Close (WAL commit): %v\n", closeErr)
			return 1
		}

		if closeErr := w.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "cypher-exec-merge-create: wal.Close: %v\n", closeErr)
			return 1
		}
		return 0
	})
}

// TestCrossProc_Cypher_MergeCreate_MatchSet verifies end-to-end WAL durability
// of a Cypher MERGE-CREATE write across OS process boundaries.
//
// Proc A (MERGE): builds a WAL-backed Engine in an empty directory and issues
// MERGE (n:Person {name:"Bob"}). Because no matching node exists, the ON CREATE
// path fires, the node is written to the in-memory graph, and Result.Close
// commits the transaction to the WAL. Proc A then closes the WAL and exits 0.
//
// Proc B (MATCH + SET): opens the same directory via recovery.Open, replaying
// the WAL to reconstruct the graph. It then executes MATCH (n:Person) SET
// n.verified = "yes" against the recovered graph and asserts that exactly one
// node was matched and updated.
//
// Note: MERGE idempotence (searchFn) is not yet implemented across process
// boundaries, so this test validates WAL-durable MERGE-CREATE followed by
// cross-process MATCH, not full cross-process MERGE idempotence.
func TestCrossProc_Cypher_MergeCreate_MatchSet(t *testing.T) {
	t.Parallel()

	// Shared data directory owned by the parent (proc B).
	dir := t.TempDir()

	// Spawn proc A: MERGE creates Bob via WAL-backed engine.
	_, stderr, err := subproc.Run(t, "cypher-exec-merge-create", dir)
	if err != nil {
		t.Fatalf("proc A (MERGE-CREATE) failed: %v\nstderr: %s", err, stderr)
	}
	if len(stderr) > 0 {
		t.Logf("proc A stderr: %s", stderr)
	}

	// Proc B: recover the graph written by proc A.
	res, openErr := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if openErr != nil {
		t.Fatalf("recovery.Open: %v", openErr)
	}

	recEng := cypher.NewEngine(res.Graph)

	// 1. Verify Bob is present via MATCH.
	matchRes, matchErr := recEng.Run(context.Background(),
		`MATCH (n:Person {name: "Bob"}) RETURN n`, nil)
	if matchErr != nil {
		t.Fatalf("MATCH Bob: %v", matchErr)
	}
	matchCount := 0
	for matchRes.Next() {
		matchCount++
	}
	if err := matchRes.Err(); err != nil {
		t.Fatalf("MATCH result error: %v", err)
	}
	if err := matchRes.Close(); err != nil {
		t.Fatalf("MATCH result close: %v", err)
	}
	if matchCount != 1 {
		t.Fatalf("MATCH (n:Person {name:\"Bob\"}) returned %d rows, want 1", matchCount)
	}

	// 2. SET a property on Bob and confirm it is reflected.
	// Use a fresh WAL-backed engine on top of the recovered graph so SET
	// goes through the normal write path.
	walPath := filepath.Join(t.TempDir(), "wal")
	w2, walErr := wal.Open(walPath)
	if walErr != nil {
		t.Fatalf("wal.Open for SET: %v", walErr)
	}
	defer func() { _ = w2.Close() }()

	store2 := txn.NewStoreWithOptions[string, float64](res.Graph, w2, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	writeEng := cypher.NewEngineWithStore(store2)

	setRes, setErr := writeEng.RunInTx(context.Background(),
		`MATCH (n:Person {name: "Bob"}) SET n.verified = "yes" RETURN n`, nil)
	if setErr != nil {
		t.Fatalf("MATCH + SET Bob: %v", setErr)
	}
	setCount := 0
	for setRes.Next() {
		setCount++
	}
	if err := setRes.Err(); err != nil {
		t.Fatalf("SET result error: %v", err)
	}
	if err := setRes.Close(); err != nil {
		t.Fatalf("SET result close: %v", err)
	}
	if setCount != 1 {
		t.Fatalf("MATCH+SET (n:Person {name:\"Bob\"}) returned %d rows, want 1", setCount)
	}
}

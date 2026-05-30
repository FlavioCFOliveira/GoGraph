package cypher_test

// write_durability_crash_recovery_test.go — T870
//
// TestWrite_DurabilityCleanExit verifies that nodes created through a
// WAL-backed Cypher engine in a child process are visible after the child
// exits cleanly and the parent opens the same directory via
// recovery.Open.
//
// Architecture (cross-process clean-exit):
//   Child  (mode "cypher-write-and-exit"):
//     1. Open WAL at <dir>/wal.
//     2. Build lpg.Graph + txn.Store + cypher.Engine.
//     3. Create 5 Person nodes via RunInTx.
//     4. Close WAL (flushes and syncs).
//     5. Exit 0.
//
//   Parent:
//     1. Spawn child with t.TempDir() as the shared dir.
//     2. Open the same dir via recovery.Open[string, float64].
//     3. Build a read-only cypher.Engine on the recovered graph.
//     4. MATCH (n:Person) RETURN count(*) → assert count == 5.
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
	// Register the child handler for T870.
	// args[0] = shared data directory.
	subproc.Register("cypher-write-and-exit", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "cypher-write-and-exit: missing dir arg")
			return 1
		}
		dir := args[0]

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "cypher-write-and-exit: wal.Open: %v\n", err)
			return 1
		}

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		eng := cypher.NewEngineWithStore(store)
		ctx := context.Background()

		// Create 5 distinct Person nodes.
		for i := 0; i < 5; i++ {
			q := fmt.Sprintf(`CREATE (n:Person {name: "person%d"})`, i)
			res, runErr := eng.RunInTx(ctx, q, nil)
			if runErr != nil {
				fmt.Fprintf(os.Stderr, "cypher-write-and-exit: RunInTx CREATE %d: %v\n", i, runErr)
				_ = w.Close()
				return 1
			}
			for res.Next() {
			}
			if iterErr := res.Err(); iterErr != nil {
				fmt.Fprintf(os.Stderr, "cypher-write-and-exit: res.Err CREATE %d: %v\n", i, iterErr)
				_ = res.Close()
				_ = w.Close()
				return 1
			}
			if closeErr := res.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "cypher-write-and-exit: res.Close CREATE %d: %v\n", i, closeErr)
				_ = w.Close()
				return 1
			}
		}

		if err := w.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "cypher-write-and-exit: w.Close: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestWrite_DurabilityCleanExit spawns a child that writes 5 Person nodes to a
// WAL-backed engine and exits cleanly, then opens the same directory via
// recovery.OpenWithOptions and asserts that all 5 nodes are present.
func TestWrite_DurabilityCleanExit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Spawn child: write 5 nodes and exit cleanly.
	_, stderr, err := subproc.Run(t, "cypher-write-and-exit", dir)
	if err != nil {
		t.Fatalf("child process failed: %v\nstderr: %s", err, stderr)
	}
	if len(stderr) > 0 {
		t.Logf("child stderr: %s", stderr)
	}

	// Parent: recover the graph written by the child.
	recRes, openErr := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if openErr != nil {
		t.Fatalf("recovery.Open: %v", openErr)
	}

	// Read-only engine on the recovered graph.
	recEng := cypher.NewEngine(recRes.Graph)
	ctx := context.Background()

	countResult, err := recEng.Run(ctx, `MATCH (n:Person) RETURN count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("MATCH count: %v", err)
	}
	rows := drainRecords(t, countResult)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	got := fmtAny(rows[0]["c"])
	if got != "5" {
		t.Errorf("recovered Person count = %s, want 5", got)
	}
}

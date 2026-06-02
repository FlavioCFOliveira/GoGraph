package cypher_test

// merge_crossproc_reopen_test.go — regression coverage for the cross-process
// MERGE node-key collision (consumer-reported "MERGE collapses distinct nodes
// across a store reopen").
//
// Background. MERGE mints the internal key of a newly created node from a
// process-global counter ("__cx_merge_<hex>"). That counter resets to zero in
// every fresh OS process. Before the fix, Merge.Init never seeded the counter
// from the recovered graph and the seed scan ignored "__cx_merge_*" keys, so a
// one-process-per-command consumer (e.g. a CLI where each command is a separate
// process) re-minted "__cx_merge_1" on every command. The second process's
// MERGE then collided with the node replayed from the WAL / restored from the
// snapshot and silently overwrote it, collapsing N distinct upserts into one.
//
// This bug is invisible to any in-binary test loop because a single test
// process keeps the counter alive across iterations. The only faithful
// reproduction spawns a genuinely separate OS process per MERGE, which is what
// this test does via internal/subproc.
//
// Layer: short. Race-clean (no shared state across the process boundary).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const mergeReopenChildMode = "cypher-merge-reopen-cycle"

func mergeReopenRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func mergeReopenStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func init() {
	// Child mode: argv = [dir, query, persist]. persist is "wal" (append +
	// fsync, no snapshot, no truncate — pure WAL-replay recovery) or "snap"
	// (snapshot + fsync + WAL truncate — self-sufficient snapshot recovery).
	subproc.Register(mergeReopenChildMode, func(args []string) int {
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "merge-reopen-cycle: need dir, query, persist")
			return 2
		}
		dir, query, persist := args[0], args[1], args[2]
		res, err := recovery.Open[string, float64](dir, mergeReopenRecOpts())
		if err != nil {
			fmt.Fprintf(os.Stderr, "recovery.Open: %v\n", err)
			return 1
		}
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "wal.Open: %v\n", err)
			return 1
		}
		store := txn.NewStoreWithOptions[string, float64](res.Graph, w, mergeReopenStoreOpts())
		r, err := cypher.NewEngineWithStore(store).RunInTxAny(context.Background(), query, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "RunInTx: %v\n", err)
			return 1
		}
		for r.Next() { //nolint:revive // drain to run the write to completion
		}
		if rerr := r.Err(); rerr != nil {
			fmt.Fprintf(os.Stderr, "result err: %v\n", rerr)
			return 1
		}
		if err := r.Close(); err != nil { // Close commits the write transaction
			fmt.Fprintf(os.Stderr, "Close: %v\n", err)
			return 1
		}
		if persist == "snap" {
			cs := csr.BuildFromAdjList(res.Graph.AdjList())
			if err := snapshot.WriteSnapshotFullWithMapperCodec(filepath.Join(dir, "snapshot"), cs, res.Graph, txn.NewStringCodec()); err != nil {
				fmt.Fprintf(os.Stderr, "snapshot: %v\n", err)
				return 1
			}
		}
		if err := w.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "Sync: %v\n", err)
			return 1
		}
		if persist == "snap" {
			if _, err := w.Truncate(); err != nil {
				fmt.Fprintf(os.Stderr, "Truncate: %v\n", err)
				return 1
			}
		}
		if err := w.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "wal.Close: %v\n", err)
			return 1
		}
		return 0
	})
}

// mergeReopenReadCount reopens dir read-only and returns the integer scalar
// produced by q (the single column must be aliased "c").
func mergeReopenReadCount(t *testing.T, dir, q string) int64 {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, mergeReopenRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open(read): %v", err)
	}
	r, err := cypher.NewEngine(res.Graph).RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("read %q: %v", q, err)
	}
	rows := collectRecords(t, r)
	if len(rows) == 0 {
		return 0
	}
	iv, ok := rows[0]["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("read %q: column c is %T, want IntegerValue", q, rows[0]["c"])
	}
	return int64(iv)
}

// runMergeReopen drives three independent OS processes, each performing one
// MERGE with a distinct key, then asserts the graph holds three distinct
// nodes after a final reopen. persist selects the WAL-replay or snapshot path.
func runMergeReopen(t *testing.T, persist string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		q := fmt.Sprintf("MERGE (n:Node {key:'%s'})", k)
		_, stderr, err := subproc.Run(t, mergeReopenChildMode, dir, q, persist)
		if err != nil {
			t.Fatalf("child MERGE %s (%s) failed: %v\nstderr: %s", k, persist, err, stderr)
		}
	}

	if got := mergeReopenReadCount(t, dir, `MATCH (n:Node) RETURN count(n) AS c`); got != int64(len(keys)) {
		// Surface the surviving keys to make a regression self-explanatory.
		res, _ := recovery.Open[string, float64](dir, mergeReopenRecOpts())
		kr, _ := cypher.NewEngine(res.Graph).RunAny(context.Background(), `MATCH (n:Node) RETURN n.key AS k ORDER BY k`, nil)
		var survivors []string
		for _, row := range collectRecords(t, kr) {
			if s, ok := row["k"].(expr.StringValue); ok {
				survivors = append(survivors, string(s))
			}
		}
		t.Fatalf("[%s] MATCH (n:Node) count = %d, want %d — MERGE collapsed distinct nodes across processes; survivors=%v",
			persist, got, len(keys), survivors)
	}
}

// TestMerge_CrossProcessReopen_WAL exercises pure WAL-replay recovery: each
// process appends to and fsyncs the WAL but never snapshots or truncates.
func TestMerge_CrossProcessReopen_WAL(t *testing.T) {
	t.Parallel()
	runMergeReopen(t, "wal")
}

// TestMerge_CrossProcessReopen_Snapshot exercises self-sufficient snapshot
// recovery: each process writes a full snapshot and truncates the WAL.
func TestMerge_CrossProcessReopen_Snapshot(t *testing.T) {
	t.Parallel()
	runMergeReopen(t, "snap")
}

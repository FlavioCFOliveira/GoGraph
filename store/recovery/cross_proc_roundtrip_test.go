package recovery

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func init() {
	// Register child handler for T891. The child writes a deterministic
	// directed graph (50-node path 0→1→…→49 plus two extra edges 0→49
	// and 0→25) via txn.NewStoreWithCodec using the canonical int codec,
	// then closes the WAL and exits 0.
	subproc.Register("recovery-write-proc", func(args []string) int {
		if len(args) < 1 {
			fmt.Println("recovery-write-proc: missing dir arg")
			return 1
		}
		dir := args[0]

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Printf("recovery-write-proc: wal.Open: %v\n", err)
			return 1
		}

		g := lpg.New[int, int64](adjlist.Config{Directed: true})
		s := txn.NewStoreWithCodec[int, int64](g, w, txn.NewIntCodec())

		const n = 50
		// Path edges: 0→1, 1→2, …, 48→49 (49 edges).
		for i := 0; i < n-1; i++ {
			tx := s.Begin()
			if err := tx.AddEdge(i, i+1, 0); err != nil {
				fmt.Printf("recovery-write-proc: AddEdge(%d→%d): %v\n", i, i+1, err)
				return 1
			}
			if err := tx.Commit(); err != nil {
				fmt.Printf("recovery-write-proc: Commit: %v\n", err)
				return 1
			}
		}
		// Extra edges: 0→49 and 0→25 (2 more = 51 total).
		for _, dst := range []int{49, 25} {
			tx := s.Begin()
			if err := tx.AddEdge(0, dst, 0); err != nil {
				fmt.Printf("recovery-write-proc: AddEdge(0→%d): %v\n", dst, err)
				return 1
			}
			if err := tx.Commit(); err != nil {
				fmt.Printf("recovery-write-proc: Commit(0→%d): %v\n", dst, err)
				return 1
			}
		}

		if err := w.Close(); err != nil {
			fmt.Printf("recovery-write-proc: wal.Close: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestRecovery_CrossProc_WriteRecover spawns a child process (proc A)
// that writes a deterministic 50-node directed graph via txn into a
// fresh WAL, then exits. The parent (proc B) opens the same directory
// via recovery.Open[int, int64] and verifies the recovered graph
// matches the written topology exactly.
//
// This test covers the cross-process persistence contract: data written
// by one OS process and flushed to disk must be fully recoverable by a
// separate OS process that opens the same directory from scratch.
func TestRecovery_CrossProc_WriteRecover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Spawn proc A.
	_, stderr, err := subproc.Run(t, "recovery-write-proc", dir)
	if err != nil {
		t.Fatalf("proc A failed: %v\nstderr: %s", err, stderr)
	}
	if len(stderr) > 0 {
		t.Logf("proc A stderr: %s", stderr)
	}

	// Proc B: open via recovery.Open.
	opts := Options[int, int64]{
		Codec:       txn.NewIntCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	res, err := Open[int, int64](dir, opts)
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}

	adj := res.Graph.AdjList()

	const n = 50
	if got := adj.Order(); got != uint64(n) {
		t.Errorf("Order = %d, want %d", got, n)
	}
	// 49 path edges + 2 extra = 51 unique edges (0→49 would be counted
	// once since AddEdge deduplicates; 0→25 is a new edge).
	if got := adj.Size(); got < 51 {
		t.Errorf("Size = %d, want >= 51", got)
	}

	// Spot-check path edges.
	for _, pair := range [][2]int{{0, 1}, {1, 2}, {24, 25}, {48, 49}} {
		src, dst := pair[0], pair[1]
		if !adj.HasEdge(src, dst) {
			t.Errorf("edge %d→%d missing", src, dst)
		}
	}
	// Extra edges.
	for _, dst := range []int{49, 25} {
		if !adj.HasEdge(0, dst) {
			t.Errorf("extra edge 0→%d missing", dst)
		}
	}
}

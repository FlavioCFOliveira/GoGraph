package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/subproc"
	"gograph/store/txn"
	"gograph/store/wal"
)

func init() {
	// Register child handler for T909 / T912. The child opens a WAL at
	// args[0]/wal, creates an int-keyed graph, and writes n AddEdge ops
	// (n from args[1]) via txn.NewStoreWithCodec before syncing after
	// each commit so partial progress is durable on disk. On normal exit
	// the WAL is closed cleanly; when the parent sends SIGKILL the
	// process dies mid-write leaving a potentially torn frame.
	subproc.Register("txn-write-loop", func(args []string) int {
		if len(args) < 2 {
			fmt.Println("txn-write-loop: need <dir> <n>")
			return 1
		}
		dir := args[0]
		n, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Printf("txn-write-loop: bad n %q: %v\n", args[1], err)
			return 1
		}

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Printf("txn-write-loop: wal.Open: %v\n", err)
			return 1
		}

		g := lpg.New[int, int64](adjlist.Config{Directed: true})
		s := txn.NewStoreWithCodec[int, int64](g, w, txn.NewIntCodec())

		ctx := context.Background()
		for i := 0; i < n; i++ {
			tx := s.Begin()
			// Weight is always 0: NewStoreWithCodec has no WeightCodec
			// and rejects non-zero weights.
			if addErr := tx.AddEdge(i, i+1, 0); addErr != nil {
				fmt.Printf("txn-write-loop: AddEdge(%d→%d): %v\n", i, i+1, addErr)
				return 1
			}
			if commitErr := tx.Commit(); commitErr != nil {
				fmt.Printf("txn-write-loop: Commit: %v\n", commitErr)
				return 1
			}
			// Sync after every op so partial progress is durable.
			if syncErr := w.SyncCtx(ctx); syncErr != nil {
				fmt.Printf("txn-write-loop: Sync: %v\n", syncErr)
				return 1
			}
		}

		if closeErr := w.Close(); closeErr != nil {
			fmt.Printf("txn-write-loop: Close: %v\n", closeErr)
			return 1
		}
		return 0
	})
}

// recoveryIntOpts returns the canonical Options[int, int64] pair used
// across T909 / T912 tests so the codec construction is DRY.
func recoveryIntOpts() Options[int, int64] {
	return Options[int, int64]{
		Codec:       txn.NewIntCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
}

// TestRecovery_CrossProc_SIGKILLMidWrite verifies crash-safety of the
// WAL under SIGKILL. Two scenarios are exercised:
//
//  1. Normal exit: child writes 100 AddEdge ops and exits cleanly.
//     recovery.Open must succeed and the graph must contain all 100
//     edges.
//
//  2. SIGKILL: a second child is started writing 1000 ops; after 20 ms
//     the parent kills it with SIGKILL. recovery.Open must succeed
//     (torn-frame is tolerated via Result.TailErr); the recovered graph
//     must be consistent — no partial-frame corruption must propagate
//     as an unexpected non-ErrTornFrame error from recovery.Open.
func TestRecovery_CrossProc_SIGKILLMidWrite(t *testing.T) {
	t.Parallel()

	// --- Scenario 1: normal exit ---
	t.Run("normal_exit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		_, stderr, err := subproc.Run(t, "txn-write-loop", dir, "100")
		if err != nil {
			t.Fatalf("child failed: %v\nstderr: %s", err, stderr)
		}

		res, recErr := Open[int, int64](dir, recoveryIntOpts())
		if recErr != nil {
			t.Fatalf("recovery.Open after normal exit: %v", recErr)
		}
		adj := res.Graph.AdjList()
		if got := adj.Order(); got < 2 {
			t.Errorf("Order = %d after normal exit, want >= 2", got)
		}
		if got := adj.Size(); got != 100 {
			t.Errorf("Size = %d after normal exit, want 100", got)
		}
	})

	// --- Scenario 2: SIGKILL mid-write ---
	t.Run("sigkill", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Spawn child writing 1000 frames without using subproc.Run so
		// we can obtain the *os/exec.Cmd and kill it ourselves.
		cmd := newSubprocCmd(os.Args[0], "txn-write-loop", dir, "1000")
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start: %v", err)
		}

		// Give the child time to write some frames, then kill it.
		time.Sleep(20 * time.Millisecond)
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // non-zero exit is expected after SIGKILL.

		// Recovery must not return a primary error. A torn frame at the
		// tail is reported via Result.TailErr, not as the primary error.
		res, recErr := Open[int, int64](dir, recoveryIntOpts())
		if recErr != nil {
			t.Fatalf("recovery.Open after SIGKILL: unexpected error: %v", recErr)
		}

		// TailErr, when set, must be ErrTornFrame or wrap it.
		if res.TailErr != nil {
			if !errors.Is(res.TailErr, wal.ErrTornFrame) {
				t.Errorf("TailErr = %v, want nil or errors.Is(ErrTornFrame)", res.TailErr)
			}
		}

		// The recovered graph must be in a consistent state.
		adj := res.Graph.AdjList()
		order := adj.Order()
		size := adj.Size()
		if order > 0 && size > order*(order-1) {
			t.Errorf("impossible graph state: Order=%d Size=%d", order, size)
		}
		t.Logf("recovered after SIGKILL: Order=%d Size=%d WALOps=%d TailErr=%v",
			order, size, res.WALOps, res.TailErr)
	})
}

// newSubprocCmd returns an *exec.Cmd that will run the current test
// binary in the given subproc mode. extraArgs are passed as os.Args[1:]
// in the child; GOGRAPH_SUBPROC_MODE is set so subproc.Dispatch routes
// to the correct handler.
func newSubprocCmd(binary, mode string, extraArgs ...string) *exec.Cmd {
	cmd := exec.Command(binary, extraArgs...) //nolint:gosec // binary is os.Args[0], trusted test binary
	cmd.Env = append(os.Environ(), subproc.EnvMode+"="+mode)
	return cmd
}

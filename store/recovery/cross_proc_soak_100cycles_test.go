//go:build soak

package recovery

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"gograph/store/wal"
)

// TestRecovery_CrossProc_SIGKILLSoak runs 100 cycles of the
// write/kill/recover loop exercised by
// [TestRecovery_CrossProc_SIGKILLMidWrite]. Each cycle:
//
//  1. Spawns the "txn-write-loop" child writing 1000 AddEdge ops.
//  2. Kills it after a random delay in [5, 50] ms.
//  3. Opens the resulting WAL via recovery.Open[int, int64].
//  4. Asserts the primary error is nil and any TailErr is ErrTornFrame.
//  5. Asserts the recovered graph is in a consistent state.
//
// A fresh subdirectory under t.TempDir() is used for each cycle to
// prevent WAL contamination across cycles.
//
// Build tag: soak  (activate with go test -tags=soak ./store/recovery/...)
//
//nolint:gocyclo // soak: cycle loop with per-cycle assertions is unavoidably linear
func TestRecovery_CrossProc_SIGKILLSoak(t *testing.T) {
	const cycles = 100

	opts := recoveryIntOpts()
	baseDir := t.TempDir()

	rng := rand.New(rand.NewPCG(12345, 67890)) //nolint:gosec // not a security use

	for cycle := range cycles {
		cycleDir := fmt.Sprintf("%s/cycle_%03d", baseDir, cycle)
		if err := os.Mkdir(cycleDir, 0o750); err != nil {
			t.Fatalf("cycle %d: mkdir: %v", cycle, err)
		}

		// Random kill delay in [5, 50] ms.
		delayMs := 5 + rng.IntN(46)
		delay := time.Duration(delayMs) * time.Millisecond

		cmd := newSubprocCmd(os.Args[0], "txn-write-loop", cycleDir, "1000")
		if err := cmd.Start(); err != nil {
			t.Fatalf("cycle %d: cmd.Start: %v", cycle, err)
		}
		time.Sleep(delay)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		res, recErr := Open[int, int64](cycleDir, opts)
		if recErr != nil {
			t.Fatalf("cycle %d (delay=%dms): recovery.Open error: %v", cycle, delayMs, recErr)
		}
		if res.TailErr != nil && !errors.Is(res.TailErr, wal.ErrTornFrame) {
			t.Errorf("cycle %d: TailErr = %v, want nil or ErrTornFrame", cycle, res.TailErr)
		}

		adj := res.Graph.AdjList()
		order := adj.Order()
		size := adj.Size()
		if order > 0 && size > order*(order-1) {
			t.Errorf("cycle %d: impossible state Order=%d Size=%d", cycle, order, size)
		}
	}

	t.Logf("soak: completed %d write/kill/recover cycles", cycles)
}

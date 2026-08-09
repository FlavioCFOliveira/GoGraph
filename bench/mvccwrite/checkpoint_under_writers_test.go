package mvccwrite

// checkpoint_under_writers_test.go — what a running checkpointer costs the write
// path, and the instrument rmp #2349's fix had to be measured on.
//
// # Why this benchmark exists
//
// rmp #2349 fixed an ACID Durability defect by making the checkpoint's phase-1
// window WAIT, under the store's commit lock, until no transaction is between its
// WAL fsync and its MVCC publish. Waiting under a lock every writer needs is a cost,
// and the ticket required it to be measured rather than argued about.
//
// BenchmarkWriteScaling cannot measure it: neither of its wirings runs a
// checkpointer, so the fix is unreachable there. That is itself worth stating — the
// commit path is not touched by the fix, so parity on BenchmarkWriteScaling/wal is
// the expected result and not evidence about this cost. The cost is paid only while a
// checkpoint holds the commit lock, so the instrument has to have one running.
//
// # What it measures
//
// Engine write throughput, WAL-backed, with a checkpointer firing continuously in
// the background for the whole timed arm. The checkpoint interval is deliberately
// aggressive — far above any production cadence — so that the fraction of wall-clock
// time spent inside a phase-1 window is large enough for a change in that window's
// duration to be visible at all. A benchmark tuned to a realistic cadence would
// dilute the very quantity it exists to detect.
//
// Read it as a RATIO against the checkpointer-free arm of the same shape, which is
// reported alongside it: the interesting number is what fraction of throughput the
// checkpointer takes, not the absolute commits/s.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// cpRig is a WAL-backed engine plus the checkpointer wired the production way:
// the capture and truncate windows run under the store's own commit serialisation
// and the mapper codec makes the snapshot self-sufficient, so the WAL is genuinely
// truncated and phase 3 does real work.
type cpRig struct {
	eng  *cypher.Engine
	cp   *checkpoint.Checkpointer[string, float64]
	stop func()
}

func newCPRig(tb testing.TB, withCheckpointer bool) *cpRig {
	tb.Helper()
	dir := tb.TempDir()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	wr, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		tb.Fatalf("wal.Open: %v", err)
	}
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	r := &cpRig{eng: cypher.NewEngineWithStore(st)}
	if !withCheckpointer {
		r.stop = func() { _ = wr.Close() }
		return r
	}
	var unusedMu sync.Mutex
	r.cp = checkpoint.New[string, float64](
		checkpoint.Config{Dir: dir}, g, wr, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	r.cp.Start(ctx)
	r.stop = func() {
		cancel()
		r.cp.Stop()
		_ = wr.Close()
	}
	return r
}

// BenchmarkCheckpointUnderWriters reports engine write throughput with and without
// a checkpointer running, at several writer counts.
//
// The `cp=on` arms drive a goroutine that triggers checkpoints back to back for the
// duration of the timed section. Every trigger is a full three-phase checkpoint,
// two of whose phases take the store's commit lock — so the writers contend with it
// exactly as they would in production, only far more often.
func BenchmarkCheckpointUnderWriters(b *testing.B) {
	for _, on := range []bool{false, true} {
		name := "cp=off"
		if on {
			name = "cp=on"
		}
		b.Run(name, func(b *testing.B) {
			for _, writers := range []int{1, 4, 16} {
				b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
					r := newCPRig(b, on)
					defer r.stop()
					warmUp(b, r.eng)
					ctx := context.Background()
					perWriter := (b.N + writers - 1) / writers

					// The checkpoint driver runs only across the timed section, so
					// setup and teardown are not attributed to it.
					done := make(chan struct{})
					var driver sync.WaitGroup
					if on {
						driver.Add(1)
						go func() {
							defer driver.Done()
							for {
								select {
								case <-done:
									return
								default:
								}
								// TriggerCtx, not Trigger: a checkpointer stopped
								// under us must not park this goroutine.
								tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
								_ = r.cp.TriggerCtx(tctx)
								cancel()
							}
						}()
					}

					b.ResetTimer()
					got, err := runArm(writers, perWriter, func(writer, i int) error {
						return commit(ctx, r.eng, writer, i)
					})
					b.StopTimer()
					close(done)
					driver.Wait()

					if err != nil {
						b.Fatalf("writer failed: %v", err)
					}
					if got.commits == 0 {
						b.Fatal("no commits made")
					}
					if on {
						// Non-vacuity: an arm in which no checkpoint ran measures the
						// cp=off shape under a different name and must not be read as
						// evidence about the checkpointer.
						if n := r.cp.Stats().Checkpoints; n == 0 {
							b.Fatal("no checkpoint completed during the timed arm: this " +
								"arm measured nothing about the checkpointer")
						} else {
							b.ReportMetric(float64(n), "checkpoints")
						}
					}
					b.ReportMetric(got.commitsPerSec(), "commits/s")
					b.ReportMetric(got.nsPerCommit(), "ns/commit")
				})
			}
		})
	}
}

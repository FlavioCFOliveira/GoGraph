// Package mvccwrite is the measurement harness for sprint 334: does engine
// write throughput scale with the number of concurrent writer goroutines?
//
// It exists because the sprint's headline claim — that write throughput will
// SCALE with writer count once MVCC replaces exclusion — is worthless without
// an instrument that (a) existed before the change, (b) is validated to show
// the defect on the build that has it, and (c) keeps failing if a later change
// silently re-serialises the writers. The audit
// (docs/audit-mvcc-sole-cc-2026-08-02.md, rmp #2296) measured the entry number
// by hand; this package turns it into a reproducible artefact and a gate
// (rmp #2297).
//
// # What is measured, and why in two wirings
//
// One arm drives N goroutines, each running autocommit Cypher CREATE
// statements into its OWN key space, and reports the wall-clock throughput of
// the whole arm. The scaling factor is that throughput divided by the
// single-writer throughput of the identical workload.
//
// The two wirings serialise on different mechanisms and only one of them can
// ever benefit from group commit, so a single number would hide half the
// story:
//
//   - mem — [cypher.NewEngine] over a bare [lpg.Graph], no store. A write takes
//     cypher.Engine.writeMu (cypher/api.go:1069) and lpg.Graph.visMu
//     (graph/lpg/lpg.go:565). No WAL, no fsync, so the number isolates the
//     concurrency-control ceiling from durability entirely: what it measures is
//     the barrier and nothing else. This is the arm the sprint has to move.
//
//   - wal — [cypher.NewEngineWithStore] over a WAL-backed [txn.Store] on a temp
//     directory. Engine.writeMu is not taken (cypher/api.go:1188-1190); the
//     store's single-writer semaphore (store/txn/txn.go:444) serialises Begin
//     through WAL append, and every commit is fsynced before it becomes
//     visible. The fsync path — wal.Writer.SyncGroup, store/wal/writer.go:497,
//     whose fsync itself is at :585 — is already leader/follower coalescing, so
//     this is the arm where group commit (rmp #2193) can pay, once visMu stops
//     spanning it.
//
// Writers use disjoint `id` key spaces (writer w owns ids w<<40+i) so the arms
// measure contention on the MECHANISM and not on the data. They deliberately
// share one label, `:Account`, because the label bitmap and the count store are
// part of the mechanism under test. A deliberately-conflicting arm belongs with
// conflict detection (rmp #2300), not here — there is nothing to conflict with
// while a global lock makes write-write conflicts impossible by construction.
//
// # Entry baseline — head c97118fe, Apple M4 (10 cores: 4P+6E), darwin/arm64,
// go1.26.5, benchstat over -count=10
//
//	go test -run='^$' -bench=BenchmarkWriteScaling/mem -benchtime=200000x -benchmem -count=10 ./bench/mvccwrite/
//
//	writers   commits/s   ns/commit   scaling
//	      1   344 100 ±1%     2 906   1.000
//	      2   290 000 ±1%     3 449   0.850 ±1%
//	      4   291 500 ±2%     3 431   0.855 ±2%
//	      8   287 100 ±2%     3 483   0.842 ±2%
//	     16   284 800 ±2%     3 512   0.835 ±2%
//	     32   282 400 ±2%     3 542   0.828 ±2%
//
// Thirty-two writers on a ten-core machine deliver 0.828x the throughput of
// one. The ideal for an engine whose writers do not conflict is a rising curve;
// this one falls, and then stays flat once every writer is queued behind the
// same lock — the signature of a global exclusive lock plus the cost of
// contending for it. The measurement is the defect, stated as a number. Note
// that allocs/op is 60 at every writer count: nothing about the shape is an
// allocation effect.
//
//	go test -run='^$' -bench=BenchmarkWriteScaling/wal -benchtime=400x -benchmem -count=10 ./bench/mvccwrite/
//
//	writers   commits/s   ns/commit   scaling
//	      1     266.6 ±1%   3 751 000   1.000
//	      2     268.4 ±1%   3 725 000   1.005 ±1%
//	      4     268.0 ±1%   3 731 000   1.004 ±1%
//	      8     268.2 ±0%   3 729 000   1.004 ±0%
//	     16     268.0 ±1%   3 731 000   1.003 ±1%
//	     32     270.0 ±1%   3 703 000   1.011 ±1%
//
// The WAL arm is not falling but perfectly FLAT, and three orders of magnitude
// slower: 3.73 ms per commit, thirty-two concurrent writers delivering the same
// 268 commits/s as one. Each commit pays a whole fsync that no other writer can
// share, even though the fsync path — wal.Writer.SyncGroup — is leader/follower
// coalescing built precisely to share it. It cannot be reached because visMu
// spans the fsync while the store semaphore was released just before it
// (Tx.releaseAfterAppend, store/txn/txn.go:1366; audit §2.3, steps 9c/9d), so
// only one writer is ever inside the sync at a time.
// Flat at 1.00x across a 32x change in offered concurrency is what "group
// commit exists but cannot be reached" looks like from the outside, and it is
// the number rmp #2193 has to move.
//
// The regression gate that keeps these numbers honest is in gate_test.go.
package mvccwrite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// wiring selects which engine composition an arm measures. The two serialise on
// different mechanisms; see the package comment.
type wiring string

const (
	// wiringMem is the store-less engine: the concurrency-control ceiling with
	// no durability cost mixed in.
	wiringMem wiring = "mem"
	// wiringWAL is the WAL-backed engine: every commit durable before visible.
	wiringWAL wiring = "wal"
)

// scalingWriters is the writer-count ladder every benchmark arm walks. 32 is
// past the machine's core count on purpose: it is there to show saturation, not
// speedup.
var scalingWriters = []int{1, 2, 4, 8, 16, 32}

// rig is one engine under measurement, plus whatever must be closed after.
type rig struct {
	eng   *cypher.Engine
	close func() error
}

// newRig builds an engine in the requested wiring. The graph is a multigraph so
// the engine does not emit the non-multigraph warning, and both wirings are
// otherwise identical, so a difference between them is a difference in the
// concurrency-control path and not in the graph.
func newRig(tb testing.TB, w wiring) *rig {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	switch w {
	case wiringMem:
		return &rig{eng: cypher.NewEngine(g), close: func() error { return nil }}
	case wiringWAL:
		wr, err := wal.Open(filepath.Join(tb.TempDir(), "wal"))
		if err != nil {
			tb.Fatalf("wal.Open: %v", err)
		}
		st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		// WithQuiesce closes the WAL under the store's commit lock, so Close
		// cannot race an in-flight commit's fsync.
		db := store.New(wr, store.WithQuiesce(st.RunUnderCommitLock))
		return &rig{eng: cypher.NewEngineWithStore(st), close: db.Close}
	default:
		tb.Fatalf("unknown wiring %q", w)
		return nil
	}
}

// createStmt is the unit of work: one autocommit transaction that appends one
// node. It is the smallest write the engine can do, which is what makes it the
// right probe for a per-transaction serialiser — the more the statement itself
// costs, the more it dilutes the mechanism being measured.
const createStmt = "CREATE (n:Account {id: $id})"

// commit runs one createStmt against eng under the writer's own key space.
func commit(ctx context.Context, eng *cypher.Engine, writer, i int) error {
	res, err := eng.RunInTx(ctx, createStmt, map[string]expr.Value{
		// Disjoint per writer: 2^40 ids each, far more than any arm commits.
		"id": expr.IntegerValue(int64(writer)<<40 | int64(i)),
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// arm is the outcome of driving `writers` goroutines through `commits` units of
// work in total.
type arm struct {
	writers int
	commits int64
	elapsed time.Duration
}

func (a arm) commitsPerSec() float64 {
	if a.elapsed <= 0 {
		return 0
	}
	return float64(a.commits) / a.elapsed.Seconds()
}

func (a arm) nsPerCommit() float64 {
	if a.commits == 0 {
		return 0
	}
	return float64(a.elapsed.Nanoseconds()) / float64(a.commits)
}

// runArm drives `writers` goroutines, each performing `perWriter` units of
// `unit`, and returns the wall-clock cost of the whole arm.
//
// The unit is a closure rather than a hard-wired engine call so that the same
// measurement code can be pointed at a synthetic control workload — which is
// what proves, in gate_test.go, that the instrument can tell a concurrent
// build from a serialised one.
func runArm(writers, perWriter int, unit func(writer, i int) error) (arm, error) {
	var (
		commits atomic.Int64
		firstMu sync.Mutex
		first   error
		wg      sync.WaitGroup
	)
	// Start the writers parked on a gate so the clock covers steady-state
	// concurrency and not goroutine start-up skew.
	var gate sync.WaitGroup
	gate.Add(1)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			gate.Wait()
			for i := 0; i < perWriter; i++ {
				if err := unit(w, i); err != nil {
					firstMu.Lock()
					if first == nil {
						first = err
					}
					firstMu.Unlock()
					return
				}
				commits.Add(1)
			}
		}(w)
	}
	start := time.Now()
	gate.Done()
	wg.Wait()
	elapsed := time.Since(start)
	return arm{writers: writers, commits: commits.Load(), elapsed: elapsed}, first
}

// warmUp pays the parse and plan-cache cost once, so it does not land inside a
// timed arm.
func warmUp(tb testing.TB, eng *cypher.Engine) {
	tb.Helper()
	if err := commit(context.Background(), eng, 1<<20, 0); err != nil {
		tb.Fatalf("warm-up commit: %v", err)
	}
}

// BenchmarkWriteScaling reports commits/s, ns/commit and the scaling factor
// against the single-writer arm, for each wiring and each writer count.
//
// Scaling is reported as a custom metric so `benchstat` carries it, and it is
// computed against the writers=1 arm of the SAME wiring measured in the same
// process — sub-benchmarks run in declaration order, so the baseline is always
// available by the time the later arms need it.
func BenchmarkWriteScaling(b *testing.B) {
	for _, w := range []wiring{wiringMem, wiringWAL} {
		b.Run(string(w), func(b *testing.B) {
			var base float64 // commits/s of the writers=1 arm
			for _, writers := range scalingWriters {
				b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
					r := newRig(b, w)
					defer func() {
						if err := r.close(); err != nil {
							b.Errorf("close rig: %v", err)
						}
					}()
					warmUp(b, r.eng)
					ctx := context.Background()

					// Round up so every writer does the same amount and the
					// total is a multiple of the writer count; report against
					// the commits actually made.
					perWriter := (b.N + writers - 1) / writers

					b.ResetTimer()
					got, err := runArm(writers, perWriter, func(writer, i int) error {
						return commit(ctx, r.eng, writer, i)
					})
					b.StopTimer()
					if err != nil {
						b.Fatalf("writer failed: %v", err)
					}
					if got.commits == 0 {
						b.Fatal("no commits made")
					}

					cps := got.commitsPerSec()
					if writers == 1 {
						base = cps
					}
					b.ReportMetric(cps, "commits/s")
					b.ReportMetric(got.nsPerCommit(), "ns/commit")
					if base > 0 {
						b.ReportMetric(cps/base, "scaling")
					}
				})
			}
		})
	}
}

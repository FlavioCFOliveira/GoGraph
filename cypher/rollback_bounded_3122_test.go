package cypher_test

// rollback_bounded_3122_test.go — regression gates for rmp #3122 and #3123: a
// write statement cut by its context deadline returns promptly, and its
// rollback does not multiply the heap the statement held at the deadline.
//
// # What was wrong
//
// The undo replay retired every node the statement had created one at a time
// through the ordinary removal. Each removal copied the whole copy-on-write
// tombstone bitmap, and recorded a death, an exclusive adjacency claim and two
// write-set entries that the abort then withdrew again. Measured at v0.17.1
// with the reporter's workload — 600 committed nodes, then
// `MATCH (a), (b), (c) CREATE ()` cut by a deadline:
//
//   - 1 s deadline: 1.45 million nodes; the statement returned 8.6–9.2 s after
//     its deadline, 6.8 s of it in the replay, which copied 42 GB of bitmaps.
//     The live heap grew from 484 MB to 865 MB while the rollback ran and the
//     heap peaked at 1.6–1.8 GB, 2.6–2.8 times its size at the deadline;
//   - 5 s deadline: the statement returned 64 s after its deadline and the heap
//     peaked at 7.2 GB, 2.8 times its size at the deadline.
//
// The pipeline itself noticed the deadline within microseconds; the whole
// overrun was the rollback.
//
// # What the gates assert
//
// Both run the reporter's statement under a real context deadline, lengthened
// until one cut statement has drawn at least minIssued3122 node ids, so they
// judge the same amount of work on a fast host and under -race.
//
// The latency gate bounds the time from the deadline to the statement's return
// by maxOverrunFactor times the deadline. The rollback now visits each object
// the statement wrote a fixed number of times, so its cost is proportional to
// the work done before the deadline, which is proportional to the deadline:
// measured at this size after the fix, the overrun was 1.02–1.45 times the
// deadline without -race and 1.46–1.59 times with it. Before the fix the
// replay's cost grew with the square of the work, and at this size the overrun
// was 4.5 times the deadline under -race and 7 times without. The factor of 2.5
// sits between the two with a margin of about 1.6 on either side, for
// scheduling noise.
//
// The memory gate bounds the heap's peak between the deadline and the return by
// maxPeakFactor times the heap at the deadline. It runs with the collector's
// target at gcPercent3123 percent, so the heap tracks the live heap closely and
// the bound measures what the rollback keeps alive and allocates, not how far
// the pacer let garbage accumulate. Measured at this size, the peak was 1.87 to
// 1.90 times the heap at the deadline before the fix, and 1.01 to 1.19 times
// after it; the factor of 1.5 leaves a margin of about 1.25 on either side.
//
// Layer: short.

import (
	"context"
	"errors"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// minIssued3122 is how many node ids the judged cut statement must have
	// drawn: far above the 50 000 the tasks require, so the defect's quadratic
	// replay is unmistakable while the gate still costs about a second.
	minIssued3122 = 200_000
	// maxOverrunFactor bounds the time from the deadline to the return, as a
	// multiple of the deadline; see the file comment.
	maxOverrunFactor = 2.5
	// maxPeakFactor bounds the heap's peak during the rollback, as a multiple of
	// the heap at the deadline; see the file comment.
	maxPeakFactor = 1.5
	// gcPercent3123 is the collector target the memory gate runs under.
	gcPercent3123 = 10
)

// cutRun is one cut statement as the gates measure it.
type cutRun struct {
	deadline   time.Duration
	elapsed    time.Duration
	issued     uint64
	heapAtCut  uint64 // heap objects at the deadline
	heapPeak   uint64 // highest heap objects from the deadline to the return
	sampledCut bool   // the sampler saw the deadline before the return
}

// heapObjects reads the bytes of heap objects, live and not yet swept, without
// stopping the world.
func heapObjects(s []metrics.Sample) uint64 {
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// runCut runs the reporter's cut statement once under deadline d, sampling the
// heap every millisecond from the deadline until the statement returns.
func runCut(t *testing.T, eng *cypher.Engine, m *graph.Mapper[string], d time.Duration) cutRun {
	t.Helper()
	issued0 := issuedIDs(m)
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var (
		atCut, peak atomic.Uint64
		cut         atomic.Bool
		stop        = make(chan struct{})
		wg          sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}
		h := heapObjects(s)
		atCut.Store(h)
		peak.Store(h)
		cut.Store(true)
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			if h := heapObjects(s); h > peak.Load() {
				peak.Store(h)
			}
		}
	}()
	start := time.Now()
	err := runExpectFailure(ctx, t, eng, `MATCH (a), (b), (c) CREATE ()`)
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the cut statement ended with %v, want the deadline", err)
	}
	return cutRun{
		deadline:   d,
		elapsed:    elapsed,
		issued:     issuedIDs(m) - issued0,
		heapAtCut:  atCut.Load(),
		heapPeak:   peak.Load(),
		sampledCut: cut.Load(),
	}
}

// cutUntilLarge builds the reporter's graph and runs the cut statement until one
// statement has drawn minIssued3122 ids, and returns that statement's
// measurements. The first deadline is 100 ms; each later one is sized from the
// rate the previous statement drew ids at, with a quarter more for margin, and
// at least doubles, so the gate reaches its size in two statements on any host.
func cutUntilLarge(t *testing.T) cutRun {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	t.Cleanup(func() { _ = g.Close() })
	eng := cypher.NewEngine(g)
	mustRun3119(t, eng, `UNWIND range(1, 600) AS i CREATE (:N {i: i})`, nil)
	m := g.AdjList().Mapper()
	for d := 100 * time.Millisecond; ; {
		r := runCut(t, eng, m, d)
		t.Logf("deadline %v: %d ids, returned after %v (overrun %v), heap %d at the deadline, peak %d",
			d, r.issued, r.elapsed, r.elapsed-d, r.heapAtCut, r.heapPeak)
		if got := countNodes3119(t, eng); got != 600 {
			t.Fatalf("count(n) = %d after the rollback, want the 600 committed nodes", got)
		}
		if r.issued >= minIssued3122 {
			return r
		}
		if d > 30*time.Second {
			t.Fatalf("only %d ids drawn within %v; the gate needs %d", r.issued, d, minIssued3122)
		}
		next := 2 * d
		if r.issued > 0 {
			next = max(next, time.Duration(float64(d)*1.25*minIssued3122/float64(r.issued)))
		}
		d = next.Round(time.Millisecond)
	}
}

// TestCutCreateReturnsPromptly_3122 is the latency gate; see the file comment.
func TestCutCreateReturnsPromptly_3122(t *testing.T) {
	// Not parallel: the gate measures wall-clock time.
	r := cutUntilLarge(t)
	overrun := r.elapsed - r.deadline
	bound := time.Duration(maxOverrunFactor * float64(r.deadline))
	if overrun > bound {
		t.Errorf("a statement cut at %v after drawing %d ids returned %v after its deadline, bound %v (%.1fx the deadline)",
			r.deadline, r.issued, overrun, bound, maxOverrunFactor)
	}
}

// TestCutCreateRollbackPeakHeap_3123 is the memory gate; see the file comment.
func TestCutCreateRollbackPeakHeap_3123(t *testing.T) {
	// Not parallel: the gate reads the process heap.
	defer debug.SetGCPercent(debug.SetGCPercent(gcPercent3123))
	r := cutUntilLarge(t)
	if !r.sampledCut {
		t.Fatal("the heap sampler never observed the deadline")
	}
	bound := uint64(maxPeakFactor * float64(r.heapAtCut))
	if r.heapPeak > bound {
		t.Errorf("heap peaked at %d bytes during the rollback of %d ids, %.2fx the %d at the deadline; bound %.1fx",
			r.heapPeak, r.issued, float64(r.heapPeak)/float64(r.heapAtCut), r.heapAtCut, maxPeakFactor)
	}
}

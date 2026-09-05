package exec_test

// index_stress_test.go — concurrent stress test for IndexBuffer + index.Manager
// (task-276), and the shared driver the soak-layer variant reuses.
//
// 100 writers each enqueue a single OpAddNodeLabel for a unique NodeID and
// commit concurrently, while a pool of reader goroutines reads the label bitmap
// throughout. After all writers have finished, exactly 100 bits must be set in
// the label bitmap for label ID 1.
//
// # Why the short-layer reader count is 50 and not 1000 (rmp #2672)
//
// The reader count was 1000. Nothing measured it, and under -race it made this
// single test 97.7 % of the whole cypher/exec package: 173.57 s of a 177.6 s
// summed -race cost, against 0.080 s for the same test without -race — an
// amplification of 2170x, where every other test in the package amplifies 1x to
// 21x. The package costs 1.334 s without -race, so its entire short-layer budget
// problem was this one constant.
//
// The cost is not the readers' own instrumented work and not CPU starvation from
// spinning. It is ThreadSanitizer's per-sync-object happens-before bookkeeping:
// every reader calls label.Index.Count, which takes i.mu.RLock() on ONE shared
// sync.RWMutex, and TSan must order each acquire against every other goroutine
// participating in that object, so the cost grows superlinearly in the number of
// distinct goroutines sharing it. Measured on the reference host (Apple M4, 10
// cores, darwin/arm64, go1.27.0), all arms under -race on a quiet host:
//
//	1000 readers, shared index ................ 82.22 s
//	1000 readers, spinning but touching nothing . 0.00 s   <- rules out starvation
//	1000 readers, each on its OWN index ......... 0.24 s   <- rules out work volume
//	 10 /  25 /  50 / 100 / 200 readers, shared . 0.12 / 4.43 / 9.98 / 34.24 / 112.55 s
//
// The sweep is NOT monotonic at its top end — 200 readers read 112.55 s against
// 82.22 s at 1000 — which is why the superlinear claim above is derived from the
// 10 -> 200 arms only. Every 1000-reader figure is one draw from a very wide
// distribution: the same test measured with -count=3 gave 49.35 / 76.59 /
// 174.32 s, a 253 % spread. That is also how 173.57 s at the head of this
// comment and 82.22 s here describe the same configuration.
//
// 50 readers is the measured 9.98 s point and still oversubscribes all 10 cores
// 5x, so the concurrent read pressure the test exists to create is preserved.
// Detection power is not traded away either, and that was verified against the
// SHIPPED test rather than inferred from the sweep: with the RLock/RUnlock
// removed from label.Index.Count, the 50-reader test below reports 10
// "WARNING: DATA RACE" and fails. The detector needs only two conflicting
// accesses, so 1000 readers bought no detection power that 50 does not have.
//
// Do not restore 1000 here. The 1024-goroutine level that CLAUDE.md's
// EXTREME/MASSIVE Concurrent Ready mandate publishes (1, 8, 64, 256, 1024) is
// not dropped — it is relocated to index_stress_soak_test.go, which runs the
// same assertion at 1024 readers in the soak layer.

import (
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// shortLayerStressReaders is the number of concurrent reader goroutines the
// short-layer stress test uses. See the file comment for the measurement that
// chose 50: it is the 9.98 s point on the reader-count sweep, against 82–174 s
// at 1000, and it still oversubscribes every core 5x.
const shortLayerStressReaders = 50

// The published EXTREME-concurrency level, 1024 readers, is exercised by
// TestIndexBuffer_MassiveConcurrentStress in index_stress_soak_test.go, which
// declares its own massiveConcurrencyStressReaders constant. It is declared
// there rather than here because a constant used only from a `soak || nightly`
// file is unused in the default build, which `golangci-lint`'s unused check
// correctly rejects.

// runIndexBufferConcurrentStress drives 100 concurrent writers against a shared
// label index while reader goroutines read it continuously, then asserts that
// every writer's NodeID is present exactly once.
//
// It is shared by the short-layer test below and by the soak-layer variant, so
// the two differ ONLY in the reader count and cannot drift apart in shape.
func runIndexBufferConcurrentStress(t *testing.T, readers int) {
	t.Helper()

	if readers <= 0 {
		t.Fatalf("readers = %d, want > 0: the concurrent read exercise would be absent", readers)
	}

	mgr := index.NewManager()
	lblIdx := label.NewNodeIndex()
	if err := mgr.CreateIndex("nodes", lblIdx); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	const (
		writers = 100
		labelID = uint32(1)
	)

	// Start writers.
	var writerWg sync.WaitGroup
	writerWg.Add(writers)
	for i := 0; i < writers; i++ {
		nodeID := graph.NodeID(i + 1)
		go func() {
			defer writerWg.Done()
			buf := &exec.IndexBuffer{}
			buf.Enqueue(index.Change{
				Op:    index.OpAddNodeLabel,
				Node:  nodeID,
				Label: labelID,
			})
			buf.Commit(mgr)
		}()
	}

	// Start readers that observe the bitmap concurrently with writers.
	// They run until writers are done to maximise contention.
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer readerWg.Done()
			for {
				// The read comes FIRST so every reader is guaranteed at least
				// one Count before it can observe stop. With the select first, a
				// reader that loses the race to close(stop) performs ZERO reads
				// and the test still passes green — the concurrent read exercise
				// this test exists to create would be silently absent, and a
				// future edit to the reader count could delete it unnoticed.
				_ = lblIdx.Count(labelID)
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}

	writerWg.Wait()
	close(stop)
	readerWg.Wait()

	// All 100 unique NodeIDs must be present in the bitmap.
	bm := lblIdx.Intersect(labelID)
	got := bm.GetCardinality()
	if got != writers {
		t.Errorf("bitmap cardinality = %d, want %d", got, writers)
	}
	for i := 0; i < writers; i++ {
		nodeID := uint64(i + 1)
		if !bm.Contains(nodeID) {
			t.Errorf("NodeID %d missing from bitmap", nodeID)
		}
	}
}

func TestIndexBuffer_ConcurrentStress(t *testing.T) {
	defer goleak.VerifyNone(t)

	runIndexBufferConcurrentStress(t, shortLayerStressReaders)
}

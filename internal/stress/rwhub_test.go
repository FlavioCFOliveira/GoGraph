//go:build soak

// Package stress — T602: reader-writer hub overlap (100 readers + 16 writers, soak).
//
// Builds a star graph with a centre hub and 1e5 leaves. Then runs:
//   - 100 reader goroutines repeatedly sampling CSR snapshots of hub
//     neighbours.
//   - 16 writer goroutines adding edges to the hub concurrently.
//
// Acceptance criteria:
//  1. go test -race -tags=soak passes.
//  2. goleak clean (via TestMain).
//  3. Every snapshot read returns a valid (non-corrupted) neighbour list:
//     no duplicate NodeIDs within a single snapshot, no out-of-range IDs.
//  4. p99 read latency stays bounded (logged, informational).
package stress

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestRWHub_ReaderWriterOverlap(t *testing.T) {
	leaves := 100_000
	readDur := 5 * time.Second
	if testing.Short() {
		leaves = 500
		readDur = 200 * time.Millisecond
	}
	const (
		readers = 100
		writers = 16
	)

	defer goleak.VerifyNone(t)

	// ── Build the initial star graph ───────────────────────────────────────
	a := adjlist.New[int, int64](adjlist.Config{Directed: false, Multigraph: false})
	const hub = 0
	for leaf := 1; leaf <= leaves; leaf++ {
		if err := a.AddEdge(hub, leaf, 1); err != nil {
			t.Fatalf("initial AddEdge hub→%d: %v", leaf, err)
		}
	}

	// snapshotPtr holds the current immutable CSR snapshot published by
	// writers. Readers load it atomically so they always see a fully-built,
	// consistent CSR — never a partially-constructed one.
	var snapshotPtr atomic.Pointer[csr.CSR[int64]]
	snapshotPtr.Store(csr.BuildFromAdjList(a))

	hubID, ok := a.Mapper().Lookup(hub)
	if !ok {
		t.Fatal("hub not in mapper")
	}

	// mu guards concurrent calls to a.AddEdge + snapshotPtr.Store.
	// Writers hold the lock for the duration of one AddEdge+BuildFromAdjList
	// cycle so that readers always load a CSR that was built from a fully
	// consistent AdjList state.
	var mu sync.Mutex

	// ── 16 writers add edges to the hub ───────────────────────────────────
	var writerWg sync.WaitGroup
	writerWg.Add(writers)
	nextLeaf := atomic.Int64{}
	nextLeaf.Store(int64(leaves + 1))

	for range writers {
		go func() {
			defer writerWg.Done()
			deadline := time.Now().Add(readDur)
			for time.Now().Before(deadline) {
				leaf := int(nextLeaf.Add(1))
				mu.Lock()
				if err := a.AddEdge(hub, leaf, 1); err == nil {
					snapshotPtr.Store(csr.BuildFromAdjList(a))
				}
				mu.Unlock()
			}
		}()
	}

	// ── 100 readers verify snapshot integrity ─────────────────────────────
	var (
		readerWg   sync.WaitGroup
		readErrors atomic.Int64
	)
	var latMu sync.Mutex
	latencies := make([]int64, 0, readers*500)

	readerWg.Add(readers)
	for range readers {
		go func() {
			defer readerWg.Done()
			deadline := time.Now().Add(readDur)
			var localLat []int64
			for time.Now().Before(deadline) {
				snap := snapshotPtr.Load()
				maxID := snap.MaxNodeID()

				t0 := time.Now()
				seen := make(map[graph.NodeID]struct{})
				corrupt := false
				for nbr := range snap.NeighboursByID(hubID) {
					if nbr >= maxID {
						readErrors.Add(1)
						corrupt = true
						break
					}
					if _, dup := seen[nbr]; dup {
						readErrors.Add(1)
						corrupt = true
						break
					}
					seen[nbr] = struct{}{}
				}
				_ = corrupt
				localLat = append(localLat, time.Since(t0).Nanoseconds())
			}
			latMu.Lock()
			latencies = append(latencies, localLat...)
			latMu.Unlock()
		}()
	}

	readerWg.Wait()
	writerWg.Wait()

	if e := readErrors.Load(); e > 0 {
		t.Errorf("snapshot integrity errors: %d (out-of-range or duplicate NodeIDs)", e)
	}

	// ── p99 latency report ────────────────────────────────────────────────
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p99idx := int(float64(len(latencies)) * 0.99)
		if p99idx >= len(latencies) {
			p99idx = len(latencies) - 1
		}
		p99 := time.Duration(latencies[p99idx])
		t.Logf("NeighboursByID p99 latency: %v over %d reads", p99, len(latencies))
		if p99 > 100*time.Millisecond {
			t.Logf("WARN: p99 read latency %v exceeds 100 ms informational threshold", p99)
		}
	}
}

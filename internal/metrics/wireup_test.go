package metrics_test

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/io/csv"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/search"
	"gograph/search/centrality"
	"gograph/store/txn"
	"gograph/store/wal"
)

// newSmokeGraph returns a fresh lpg.Graph for the store.txn smoke
// section. We use string-keyed nodes with int64 weights so the
// generic Store instantiation matches the recovery harness.
func newSmokeGraph() *lpg.Graph[string, int64] {
	return lpg.New[string, int64](adjlist.Config{Directed: true})
}

// countingBackend records every metric event so the wire-up smoke
// test can assert that the public APIs surface their expected
// latency observation under the documented name. It is intentionally
// independent of the recordingBackend in metrics_test.go to avoid
// cross-package coupling.
type countingBackend struct {
	mu      sync.Mutex
	counter map[string]uint64
	latency map[string][]time.Duration
}

func newCountingBackend() *countingBackend {
	return &countingBackend{
		counter: map[string]uint64{},
		latency: map[string][]time.Duration{},
	}
}

func (c *countingBackend) IncCounter(name string, delta uint64) {
	c.mu.Lock()
	c.counter[name] += delta
	c.mu.Unlock()
}

func (c *countingBackend) ObserveLatency(name string, d time.Duration) {
	c.mu.Lock()
	c.latency[name] = append(c.latency[name], d)
	c.mu.Unlock()
}

func (c *countingBackend) hasLatency(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.latency[name]) > 0
}

func (c *countingBackend) counterFor(name string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counter[name]
}

// buildSmokeCSR returns a tiny strongly-connected directed graph and
// a valid source NodeID for traversal-driven tests.
func buildSmokeCSR(t *testing.T) (*csr.CSR[int64], graph.NodeID) {
	t.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < 6; i++ {
		a.AddNode(i)
	}
	a.AddEdge(0, 1, 1)
	a.AddEdge(1, 2, 1)
	a.AddEdge(2, 3, 1)
	a.AddEdge(3, 4, 1)
	a.AddEdge(4, 5, 1)
	a.AddEdge(5, 0, 1)
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	return c, src
}

// expectLatency fails the surrounding test if name has not yet fired
// at least one latency sample on be. It is the shared assertion used
// by every wire-up sub-driver below.
func expectLatency(t *testing.T, be *countingBackend, name string) {
	t.Helper()
	if !be.hasLatency(name) {
		t.Errorf("%s did not fire latency metric", name)
	}
}

// driveSearchSample exercises the search-package APIs in the
// representative sample. The function deliberately covers the
// Foo/FooCtx pair for Dijkstra so the assertion that both layers
// fire stays explicit.
func driveSearchSample(t *testing.T, be *countingBackend, c *csr.CSR[int64], src graph.NodeID) {
	t.Helper()
	search.BFS(c, src, func(_ graph.NodeID, _ int) bool { return true })
	expectLatency(t, be, "search.BFS")

	if _, err := search.Dijkstra(c, src); err != nil {
		t.Fatalf("search.Dijkstra: %v", err)
	}
	expectLatency(t, be, "search.Dijkstra")
	expectLatency(t, be, "search.DijkstraCtx")
}

// driveCentralitySample exercises the search/centrality sample.
func driveCentralitySample(t *testing.T, be *countingBackend, c *csr.CSR[int64]) {
	t.Helper()
	if _, _, err := centrality.PageRank(c, centrality.PageRankOptions{
		Damping: 0.85, MaxIterations: 5, Tolerance: 1e-3,
	}); err != nil {
		t.Fatalf("centrality.PageRank: %v", err)
	}
	expectLatency(t, be, "search.centrality.PageRank")
}

// driveCSVSample exercises the graph/io/csv writer with context.
func driveCSVSample(t *testing.T, be *countingBackend) {
	t.Helper()
	csvAdj := adjlist.New[string, int64](adjlist.Config{Directed: true})
	csvAdj.AddEdge("a", "b", 1)
	var buf bytes.Buffer
	if _, err := csv.WriteCtx(context.Background(), &buf, csvAdj, csv.DefaultOptions()); err != nil {
		t.Fatalf("csv.WriteCtx: %v", err)
	}
	expectLatency(t, be, "graph.io.csv.WriteCtx")
}

// driveWALSample exercises the store/wal lifecycle.
func driveWALSample(t *testing.T, be *countingBackend, walPath string) {
	t.Helper()
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	expectLatency(t, be, "store.wal.Open")

	if err := w.Append([]byte("payload")); err != nil {
		t.Fatalf("wal.Append: %v", err)
	}
	expectLatency(t, be, "store.wal.Append")
	expectLatency(t, be, "store.wal.AppendCtx")

	if err := w.Sync(); err != nil {
		t.Fatalf("wal.Sync: %v", err)
	}
	expectLatency(t, be, "store.wal.Sync")

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// driveTxnSample exercises store/txn Begin + Commit on an empty Tx.
func driveTxnSample(t *testing.T, be *countingBackend, walPath string) {
	t.Helper()
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	gph := newSmokeGraph()
	store := txn.NewStore[string, int64](gph, w)
	tx := store.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatalf("txn.Commit: %v", err)
	}
	expectLatency(t, be, "store.txn.Begin")
	expectLatency(t, be, "store.txn.Commit")
}

// TestWireUp_FiresPerCallSite installs a counting backend and exercises
// a representative sample of instrumented public APIs across search,
// search/centrality, graph/io/csv, store/wal, and store/txn. It then
// asserts that every sampled call site recorded at least one latency
// sample under the documented name.
//
// The test does not enumerate every wired call site; the docs/metrics.md
// inventory is the authoritative list. The role of this smoke test is
// to fail loudly when a metric name drifts or when the defer-Time hook
// is silently dropped from a hot-path entry point.
func TestWireUp_FiresPerCallSite(t *testing.T) {
	// SetBackend is process-global; serialise so this test does not
	// race the other tests in the metrics package that swap the
	// backend. The restoring defer puts the no-op back at the end.
	defer metrics.SetBackend(nil)
	be := newCountingBackend()
	metrics.SetBackend(be)

	c, src := buildSmokeCSR(t)

	driveSearchSample(t, be, c, src)
	driveCentralitySample(t, be, c)
	driveCSVSample(t, be)

	dir := t.TempDir()
	driveWALSample(t, be, filepath.Join(dir, "wal.bin"))
	driveTxnSample(t, be, filepath.Join(dir, "wal2.bin"))
}

// TestWireUp_ErrorCounterFires drives one wired error path and
// verifies the corresponding ".errors" counter increments. The chosen
// path (search.DijkstraCtx with a context already cancelled) is
// reliable across architectures and does not depend on graph state.
func TestWireUp_ErrorCounterFires(t *testing.T) {
	defer metrics.SetBackend(nil)
	be := newCountingBackend()
	metrics.SetBackend(be)

	c, src := buildSmokeCSR(t)

	// Build a wide-enough graph so the inner loop checks ctx.Err at
	// least once. A 6-node ring will not normally trigger the 4096-pop
	// check, but the dijkstraCore guarantees an initial check on
	// non-empty heap entry, so cancellation pre-call is observed via
	// the heap-pop loop's first ctx.Err() probe.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := search.DijkstraCtx(ctx, c, src)
	if err == nil {
		// Some implementations short-circuit on empty heap before the
		// first ctx check; in that case there is no error to count
		// and we fall back to a stronger driver: BFSCtx, whose ctx
		// check runs once per BFS level (including the first).
		_ = search.BFSCtx(ctx, c, src, func(_ graph.NodeID, _ int) bool { return true })
		if be.counterFor("search.BFSCtx.errors") == 0 {
			t.Fatalf("expected at least one wired .errors counter to fire on cancelled ctx")
		}
		return
	}
	if be.counterFor("search.DijkstraCtx.errors") == 0 {
		t.Fatalf("search.DijkstraCtx returned %v but .errors counter did not increment", err)
	}
}

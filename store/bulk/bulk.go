// Package bulk implements the bulk-loading path that bypasses the
// transactional WAL stack and writes a Tier 2 csrfile directly from
// a stream of edges.
//
// Bulk loading is the high-throughput equivalent of running many
// txn.Commit calls back-to-back. The loader pipes edges into an
// in-memory adjacency list and then writes the resulting CSR through
// [csrfile.WriteToFile]; a future revision will introduce an external
// k-way merge sort for graphs that exceed memory.
//
// # Pre-sizing
//
// When Options.MaxRows > 0 the loader treats the cap as a capacity hint
// and pre-sizes the adjacency builder and its interning table via
// [adjlist.AdjList.Reserve], eliminating most slice/map re-growths on
// the ingest hot path. Pre-sizing is a pure allocation optimisation: it
// never changes which NodeID a key receives nor the order of edges in
// the resulting CSR.
//
// # Partitioned parallel ingest
//
// For large directed loads the loader can build the adjacency in
// parallel across bounded goroutines (see [Options.Parallel]) while
// producing a result that is byte-for-byte identical to the sequential
// loader. Determinism is guaranteed by a two-phase scheme: a serial
// first phase assigns every NodeID in input order (reproducing the
// sequential interning order exactly by construction), then a parallel
// second phase builds adjacency partitioned by the source node's Mapper
// shard so that partitions write to disjoint shards with no contention
// and each source keeps its edges in input order. Undirected and
// simple-graph loads, and loads below a small threshold, use the
// sequential path because mirror/dedup edges would cross partition
// boundaries; the result is identical either way.
package bulk

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// ErrTooManyRows is returned by [Loader.Add], [Loader.AddBatch], and
// [Loader.Drain] when the configured Options.MaxRows cap is exceeded.
var ErrTooManyRows = errors.New("bulk: row cap exceeded")

// maxParallelism caps the number of build goroutines the parallel path
// will ever spawn, honouring the project's bounded-resources mandate
// regardless of the host's core count.
const maxParallelism = 16

// parallelMinEdges is the edge-count threshold below which Finalise
// always uses the sequential build. Below this size the goroutine
// fan-out/-in overhead dominates the ingest work, so parallelism would
// regress small loads. It is a var (not a const) only so the
// differential test can lower it to force the parallel path on small
// inputs; production code never mutates it.
var parallelMinEdges = 50_000

// Edge is one record the bulk loader consumes.
type Edge struct {
	Src    string
	Dst    string
	Weight int64
}

// Options configures the [Loader].
type Options struct {
	// OutputPath is the destination csrfile.
	OutputPath string
	// Directed selects the adjacency-list configuration.
	Directed bool
	// Multigraph allows parallel edges in the loaded graph.
	Multigraph bool
	// MaxRows, when > 0, caps the number of edge records the loader
	// will ingest. Add / AddBatch / Drain return [ErrTooManyRows]
	// on the row that crosses the cap. Default (0) is unbounded.
	//
	// MaxRows additionally serves as a capacity hint: when set, the
	// adjacency builder and its interning table are pre-sized to it so
	// the ingest hot path incurs far fewer slice/map re-growths.
	MaxRows int
	// ExpectNodes, when > 0, is the caller's estimate of the number of
	// DISTINCT nodes the load will produce. It pre-sizes the interning
	// table (the Mapper) to that cardinality, eliminating most of the
	// map/slice re-growth the first-encounter Intern path incurs. Unlike
	// MaxRows (an edge count), ExpectNodes sizes the node-indexed
	// interning structures correctly, so it reduces allocations without
	// the over-allocation an edge-count hint would cause. It is a pure,
	// determinism-neutral capacity hint. Leave it 0 when the node count
	// is unknown.
	ExpectNodes int
	// Parallel selects partitioned-parallel ingest for large directed
	// loads. The default (false) always uses the deterministic
	// sequential build. When true, the loader buffers edges and builds
	// the adjacency across up to GOMAXPROCS (capped at an internal
	// bound) goroutines during [Loader.Finalise], producing a result
	// byte-for-byte identical to the sequential build. Parallelism is
	// only engaged for directed loads at or above an internal edge-count
	// threshold; smaller, undirected, or simple-graph loads transparently
	// fall back to the sequential build.
	Parallel bool
}

// Loader streams edges through an in-memory adjacency list and
// writes the resulting Tier 2 csrfile when [Loader.Finalise] runs.
//
// Loader is not safe for concurrent use; callers that wish to
// parallelise ingestion either set Options.Parallel (the loader then
// fans out internally during Finalise) or partition the edge stream
// upstream and call separate Loaders, then merge.
type Loader struct {
	opts Options
	adj  *adjlist.AdjList[string, int64]
	rows int

	// buffered holds the edge stream when Options.Parallel is set; the
	// parallel build runs over this slice during Finalise. It is nil in
	// the sequential (default) mode, where edges go straight into adj.
	buffered []Edge
}

// New returns a fresh Loader.
//
// When opts.ExpectNodes > 0 the interning table is pre-sized to that
// node estimate (a pure, determinism-neutral capacity hint; see
// [Options.ExpectNodes]). When opts.Parallel is set the loader buffers
// edges for the parallel build performed by [Loader.Finalise], pre-sizing
// the edge buffer to opts.MaxRows when that cap is known.
func New(opts Options) *Loader {
	l := &Loader{
		opts: opts,
		adj:  adjlist.New[string, int64](adjlist.Config{Directed: opts.Directed, Multigraph: opts.Multigraph}),
	}
	if opts.ExpectNodes > 0 {
		// Calibrated pre-size: reserve interning capacity for the expected
		// distinct-node count. Sizing the node-indexed Mapper by a node
		// estimate is sound (no over-allocation) and determinism-neutral.
		l.adj.Mapper().Reserve(opts.ExpectNodes)
	}
	if opts.Parallel {
		// Pre-size the edge buffer from the row cap when known. The buffer
		// is edge-indexed, so MaxRows sizes it exactly — this IS a sound,
		// determinism-neutral application of the "pre-size all slices"
		// mandate. (Pre-sizing the node-indexed adjacency/interning
		// structures from an edge count is NOT sound: profiling shows the
		// dominant ingest allocation is per-source adjacency growth, driven
		// by per-node degree, which an edge-count hint cannot size without
		// over-allocating; see ExpectNodes for the calibrated knob.)
		if opts.MaxRows > 0 {
			l.buffered = make([]Edge, 0, opts.MaxRows)
		}
	}
	return l
}

// Add ingests one edge. Returns [ErrTooManyRows] when the row cap is
// exceeded.
func (l *Loader) Add(e Edge) error {
	defer metrics.Time("store.bulk.Add").Stop()
	if l.opts.MaxRows > 0 && l.rows >= l.opts.MaxRows {
		metrics.IncCounter("store.bulk.Add.errors", 1)
		return ErrTooManyRows
	}
	if l.opts.Parallel {
		// Buffer for the deterministic parallel build in Finalise.
		l.buffered = append(l.buffered, e)
		l.rows++
		return nil
	}
	if err := l.adj.AddEdge(e.Src, e.Dst, e.Weight); err != nil {
		metrics.IncCounter("store.bulk.Add.errors", 1)
		return err
	}
	l.rows++
	return nil
}

// AddBatch ingests a contiguous batch of edges. Returns [ErrTooManyRows]
// on the first edge that would cross the cap; edges accepted before
// that point remain ingested.
func (l *Loader) AddBatch(es []Edge) error {
	defer metrics.Time("store.bulk.AddBatch").Stop()
	for k := range es {
		if err := l.Add(es[k]); err != nil {
			metrics.IncCounter("store.bulk.AddBatch.errors", 1)
			return err
		}
	}
	return nil
}

// Drain consumes from ch until it is closed or ctx is cancelled.
// Returns the number of edges drained and any error from the input
// channel ([ErrTooManyRows] when the row cap is exceeded).
func (l *Loader) Drain(ctx context.Context, ch <-chan Edge) (int, error) {
	defer metrics.Time("store.bulk.Drain").Stop()
	drained := 0
	for {
		select {
		case <-ctx.Done():
			metrics.IncCounter("store.bulk.Drain.errors", 1)
			return drained, ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return drained, nil
			}
			if err := l.Add(e); err != nil {
				metrics.IncCounter("store.bulk.Drain.errors", 1)
				return drained, err
			}
			drained++
		}
	}
}

// Finalise builds the CSR from the accumulated edges and writes it
// to opts.OutputPath as a csrfile. Returns the row count, the
// resulting CSR (for chaining into search/extern), and any error.
//
// When Options.Parallel is set and the buffered load is a large
// directed graph, Finalise builds the adjacency in parallel; the
// resulting CSR and csrfile are byte-for-byte identical to the
// sequential build. The csrfile is always published atomically and
// durably by [csrfile.WriteToFile] (tmp + fsync + rename + parent
// fsync): the parallel build completes fully in memory before the
// single publication, so a crash mid-build leaves no partial csrfile.
func (l *Loader) Finalise() (int, *csr.CSR[int64], error) {
	defer metrics.Time("store.bulk.Finalise").Stop()

	if l.opts.Parallel {
		if err := l.buildBuffered(); err != nil {
			metrics.IncCounter("store.bulk.Finalise.errors", 1)
			return l.rows, nil, err
		}
	}

	c := csr.BuildFromAdjList(l.adj)
	if l.opts.OutputPath != "" {
		if _, err := csrfile.WriteToFile(l.opts.OutputPath, c); err != nil {
			metrics.IncCounter("store.bulk.Finalise.errors", 1)
			return l.rows, c, fmt.Errorf("bulk: write csrfile: %w", err)
		}
	}
	return l.rows, c, nil
}

// buildBuffered drains the buffered edge stream into l.adj. It chooses
// the deterministic parallel build when the load is large and directed,
// and otherwise the sequential build; both produce an identical adj.
func (l *Loader) buildBuffered() error {
	if l.parallelEligible() {
		return l.buildParallel()
	}
	return l.buildSequential()
}

// parallelEligible reports whether the buffered load qualifies for the
// parallel build. Parallelism is restricted to directed graphs at or
// above the size threshold: undirected mirroring and simple-graph
// dedup would route edges across partition boundaries, so those modes
// use the sequential build (which is byte-identical anyway).
func (l *Loader) parallelEligible() bool {
	if !l.opts.Directed {
		return false
	}
	if len(l.buffered) < parallelMinEdges {
		return false
	}
	return workerCount() > 1
}

// buildSequential replays the buffered edges into l.adj in input order,
// exactly as the non-buffered Add path would have. This is the ground
// truth the parallel build must match byte-for-byte.
func (l *Loader) buildSequential() error {
	for k := range l.buffered {
		e := l.buffered[k]
		if err := l.adj.AddEdge(e.Src, e.Dst, e.Weight); err != nil {
			return err
		}
	}
	return nil
}

// buildParallel performs the deterministic two-phase build.
//
// Phase 1 (serial) interns every endpoint in input order, reproducing
// the sequential Mapper's NodeID assignment exactly by construction, and
// records the resolved source NodeID for each edge. Phase 2 (parallel)
// partitions edges by the source's Mapper shard and replays each
// partition's edges, in input order, into l.adj. Because a directed
// AddEdge mutates only the source's shard, and a source belongs to
// exactly one Mapper shard, partitions write to disjoint shards: there
// is no cross-partition contention and each source keeps its edges in
// input order. The resulting adjacency — and therefore the CSR and
// csrfile — is identical to the sequential build.
func (l *Loader) buildParallel() error {
	edges := l.buffered
	mapper := l.adj.Mapper()

	// Phase 1: serial interning in input order fixes every NodeID.
	srcShard := make([]uint8, len(edges))
	for k := range edges {
		srcID := mapper.Intern(edges[k].Src)
		mapper.Intern(edges[k].Dst)
		srcShard[k] = uint8(graph.MapperShardOf(srcID))
	}

	// Partition edge indices by source shard. Each partition is a stable
	// subsequence of the input order, so per-source order is preserved.
	p := workerCount()
	parts := make([][]int, p)
	for k := range edges {
		w := int(srcShard[k]) % p
		parts[w] = append(parts[w], k)
	}

	// Phase 2: build adjacency in parallel. Partitions own disjoint
	// Mapper-shard sets (every index whose source shard maps to worker w
	// lives only in parts[w]); since a directed AddEdge touches only the
	// source node's shard, no two workers ever write the same AdjList
	// shard. Errors (e.g. ErrShardFull under a cap) are collected and the
	// first is returned.
	var wg sync.WaitGroup
	errs := make([]error, p)
	for w := 0; w < p; w++ {
		if len(parts[w]) == 0 {
			continue
		}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for _, k := range parts[w] {
				e := edges[k]
				if err := l.adj.AddEdge(e.Src, e.Dst, e.Weight); err != nil {
					errs[w] = err
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// workerCount returns the bounded number of build goroutines to use:
// GOMAXPROCS clamped to [1, maxParallelism].
func workerCount() int {
	p := runtime.GOMAXPROCS(0)
	if p < 1 {
		p = 1
	}
	if p > maxParallelism {
		p = maxParallelism
	}
	return p
}

// Rows returns the number of edges ingested so far.
func (l *Loader) Rows() int { return l.rows }

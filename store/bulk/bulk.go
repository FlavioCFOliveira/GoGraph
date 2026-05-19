// Package bulk implements the bulk-loading path that bypasses the
// transactional WAL stack and writes a Tier 2 csrfile directly from
// a stream of edges.
//
// Bulk loading is the high-throughput equivalent of running many
// txn.Commit calls back-to-back. The v1 implementation pipes
// edges into an in-memory adjacency list and then writes the
// resulting CSR through [csrfile.WriteToFile]; a future revision
// will introduce an external k-way merge sort for graphs that
// exceed memory.
package bulk

import (
	"context"
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/store/csrfile"
)

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
}

// Loader streams edges through an in-memory adjacency list and
// writes the resulting Tier 2 csrfile when [Loader.Finalise] runs.
//
// Loader is not safe for concurrent use; callers that wish to
// parallelise ingestion should partition the edge stream upstream
// and call separate Loaders, then merge — but the v1 expectation is
// a single ingest goroutine.
type Loader struct {
	opts Options
	adj  *adjlist.AdjList[string, int64]
	rows int
}

// New returns a fresh Loader.
func New(opts Options) *Loader {
	return &Loader{
		opts: opts,
		adj:  adjlist.New[string, int64](adjlist.Config{Directed: opts.Directed, Multigraph: opts.Multigraph}),
	}
}

// Add ingests one edge.
func (l *Loader) Add(e Edge) {
	l.adj.AddEdge(e.Src, e.Dst, e.Weight)
	l.rows++
}

// AddBatch ingests a contiguous batch of edges.
func (l *Loader) AddBatch(es []Edge) {
	for k := range es {
		l.adj.AddEdge(es[k].Src, es[k].Dst, es[k].Weight)
	}
	l.rows += len(es)
}

// Drain consumes from ch until it is closed or ctx is cancelled.
// Returns the number of edges drained and any error from the input
// channel (none in the v1 API; reserved for future stream sources).
func (l *Loader) Drain(ctx context.Context, ch <-chan Edge) (int, error) {
	drained := 0
	for {
		select {
		case <-ctx.Done():
			return drained, ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return drained, nil
			}
			l.Add(e)
			drained++
		}
	}
}

// Finalise builds the CSR from the accumulated edges and writes it
// to opts.OutputPath as a csrfile. Returns the row count, the
// resulting CSR (for chaining into search/extern), and any error.
func (l *Loader) Finalise() (int, *csr.CSR[int64], error) {
	c := csr.BuildFromAdjList(l.adj)
	if l.opts.OutputPath != "" {
		if _, err := csrfile.WriteToFile(l.opts.OutputPath, c); err != nil {
			return l.rows, c, fmt.Errorf("bulk: write csrfile: %w", err)
		}
	}
	return l.rows, c, nil
}

// Rows returns the number of edges ingested so far.
func (l *Loader) Rows() int { return l.rows }

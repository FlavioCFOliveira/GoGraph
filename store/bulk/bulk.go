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
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// ErrTooManyRows is returned by [Loader.Add], [Loader.AddBatch], and
// [Loader.Drain] when the configured Options.MaxRows cap is exceeded.
var ErrTooManyRows = errors.New("bulk: row cap exceeded")

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
	MaxRows int
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

// Add ingests one edge. Returns [ErrTooManyRows] when the row cap is
// exceeded.
func (l *Loader) Add(e Edge) error {
	defer metrics.Time("store.bulk.Add")()
	if l.opts.MaxRows > 0 && l.rows >= l.opts.MaxRows {
		metrics.IncCounter("store.bulk.Add.errors", 1)
		return ErrTooManyRows
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
	defer metrics.Time("store.bulk.AddBatch")()
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
	defer metrics.Time("store.bulk.Drain")()
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
func (l *Loader) Finalise() (int, *csr.CSR[int64], error) {
	defer metrics.Time("store.bulk.Finalise")()
	c := csr.BuildFromAdjList(l.adj)
	if l.opts.OutputPath != "" {
		if _, err := csrfile.WriteToFile(l.opts.OutputPath, c); err != nil {
			metrics.IncCounter("store.bulk.Finalise.errors", 1)
			return l.rows, c, fmt.Errorf("bulk: write csrfile: %w", err)
		}
	}
	return l.rows, c, nil
}

// Rows returns the number of edges ingested so far.
func (l *Loader) Rows() int { return l.rows }

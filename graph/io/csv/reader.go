// Package csv reads and writes graphs as edge lists in CSV format.
//
// The format is a simple table of columns: source, destination, and
// (optionally) a weight. Lines beginning with the comment character
// (default '#') are skipped. A header row may declare the column
// types; without it the reader assumes a fixed (src, dst[, weight])
// layout.
package csv

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"

	"gograph/graph/adjlist"
)

// Options controls Reader / Writer behaviour.
type Options struct {
	// Delimiter is the column separator; defaults to ','.
	Delimiter rune
	// Comment is the comment character; defaults to '#'.
	Comment rune
	// HasHeader skips the first line when true.
	HasHeader bool
	// Directed selects the underlying adjacency-list config.
	Directed bool
	// Multigraph allows parallel edges.
	Multigraph bool
}

// DefaultOptions returns the minimal config: comma delimiter, '#'
// comments, directed simple graph, no header.
func DefaultOptions() Options {
	return Options{Delimiter: ',', Comment: '#', Directed: true}
}

// ReadInto streams a CSV from r into an adjacency list, returning
// the loaded list and the number of rows ingested. Each row must
// have at least two fields (src, dst); a third field is parsed as
// a int64 weight.
func ReadInto(r io.Reader, opts Options) (*adjlist.AdjList[string, int64], int, error) {
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	if opts.Comment == 0 {
		opts.Comment = '#'
	}
	c := csv.NewReader(r)
	c.Comma = opts.Delimiter
	c.Comment = opts.Comment
	c.FieldsPerRecord = -1
	c.ReuseRecord = true

	a := adjlist.New[string, int64](adjlist.Config{
		Directed:   opts.Directed,
		Multigraph: opts.Multigraph,
	})
	rows := 0
	first := opts.HasHeader
	for {
		rec, err := c.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, rows, fmt.Errorf("csv row %d: %w", rows+1, err)
		}
		if first {
			first = false
			continue
		}
		if len(rec) < 2 {
			return nil, rows, fmt.Errorf("csv row %d: need at least 2 fields, got %d", rows+1, len(rec))
		}
		var w int64
		if len(rec) >= 3 && rec[2] != "" {
			pw, perr := strconv.ParseInt(rec[2], 10, 64)
			if perr != nil {
				return nil, rows, fmt.Errorf("csv row %d weight %q: %w", rows+1, rec[2], perr)
			}
			w = pw
		}
		a.AddEdge(rec[0], rec[1], w)
		rows++
	}
	return a, rows, nil
}

// Package csv reads and writes graphs as edge lists in CSV format.
//
// The format is a simple table of columns: source, destination, and
// (optionally) a weight. Lines beginning with the comment character
// (default '#') are skipped. A header row may declare the column
// types; without it the reader assumes a fixed (src, dst[, weight])
// layout.
package csv

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// DefaultMaxBytes is the default ceiling, in bytes, on the amount of
// input a reader will consume before failing with [ErrInputTooLarge].
// It guards against memory exhaustion from untrusted files (a crafted
// multi-gigabyte field, for example). A value of zero or less disables
// the cap; see [Options.MaxBytes].
//
// # Peak memory
//
// The cap bounds the number of bytes drawn from the reader, but
// [encoding/csv] does not bound the size of a single field. A hostile
// input — for example an unterminated quoted field — is buffered by the
// decoder up to MaxBytes, and the decoder's working set (raw buffer plus
// the parsed record) amplifies that to roughly 4–5× the cap. Peak
// transient RAM is therefore on the order of 4–5 × MaxBytes, not MaxBytes.
//
// DefaultMaxBytes is set to 128 MiB so that this worst-case transient
// stays well under 1 GiB even on a hostile single-token file. Callers
// importing larger trusted inputs raise [Options.MaxBytes] explicitly,
// accepting the proportionally higher peak; callers parsing untrusted
// input should keep the default or lower it further.
const DefaultMaxBytes int64 = 128 << 20 // 128 MiB

// ErrInputTooLarge is returned by [ReadInto] and [ReadIntoCtx] when the
// input stream exceeds the configured [Options.MaxBytes] ceiling. The
// reader stops drawing bytes from the input as soon as the limit is
// crossed; note, however, that a single oversized field may already have
// been buffered by [encoding/csv] up to the cap before the limit trips,
// so the decoder's peak working set is a multiple of MaxBytes (see
// [DefaultMaxBytes]).
var ErrInputTooLarge = errors.New("csv: input exceeds maximum size")

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
	// MaxBytes caps the number of bytes read from the input before the
	// reader fails with [ErrInputTooLarge]. [DefaultOptions] sets it to
	// [DefaultMaxBytes]; a value of zero or less disables the cap.
	MaxBytes int64
}

// DefaultOptions returns the minimal config: comma delimiter, '#'
// comments, directed simple graph, no header, and the [DefaultMaxBytes]
// input-size ceiling.
func DefaultOptions() Options {
	return Options{Delimiter: ',', Comment: '#', Directed: true, MaxBytes: DefaultMaxBytes}
}

// ReadInto streams a CSV from r into an adjacency list, returning
// the loaded list and the number of rows ingested. Each row must
// have at least two fields (src, dst); a third field is parsed as
// a int64 weight.
func ReadInto(r io.Reader, opts Options) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.csv.ReadInto")()
	a, rows, err := ReadIntoCtx(context.Background(), r, opts)
	if err != nil {
		metrics.IncCounter("graph.io.csv.ReadInto.errors", 1)
	}
	return a, rows, err
}

// ReadIntoCtx is the context-aware variant of [ReadInto]. ctx.Err()
// is checked every 4096 rows.
//
// On any error — a parse error, context cancellation, or the
// [ErrInputTooLarge] cap — the returned graph is nil; the import is
// all-or-nothing at the in-memory level, so a caller cannot accidentally
// commit a half-built graph. The typed error (parse error, ctx.Err(), or
// [ErrInputTooLarge]) is returned unchanged; only the graph value is
// discarded.
//
//nolint:gocyclo // csv decode + opt defaults + per-row parse + ctx tick
func ReadIntoCtx(ctx context.Context, r io.Reader, opts Options) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.csv.ReadIntoCtx")()
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	if opts.Comment == 0 {
		opts.Comment = '#'
	}
	if opts.MaxBytes > 0 {
		r = newLimitReader(r, opts.MaxBytes)
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
		if rows&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.csv.ReadIntoCtx.errors", 1)
				return nil, rows, err
			}
		}
		rec, err := c.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			metrics.IncCounter("graph.io.csv.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("csv row %d: %w", rows+1, err)
		}
		if first {
			first = false
			continue
		}
		if len(rec) < 2 {
			metrics.IncCounter("graph.io.csv.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("csv row %d: need at least 2 fields, got %d", rows+1, len(rec))
		}
		var w int64
		if len(rec) >= 3 && rec[2] != "" {
			pw, perr := strconv.ParseInt(rec[2], 10, 64)
			if perr != nil {
				metrics.IncCounter("graph.io.csv.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("csv row %d weight %q: %w", rows+1, rec[2], perr)
			}
			w = pw
		}
		if err := a.AddEdge(rec[0], rec[1], w); err != nil {
			metrics.IncCounter("graph.io.csv.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("csv row %d: %w", rows+1, err)
		}
		rows++
	}
	return a, rows, nil
}

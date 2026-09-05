// Package csv reads and writes graphs as edge lists in CSV format.
//
// The format is a simple table of columns: source, destination, and
// (optionally) a weight. Lines beginning with the comment character
// (default '#') are skipped. A header row may declare the column
// types; without it the reader assumes a fixed (src, dst[, weight])
// layout.
package csv

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

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
// Two independent bounds shape the reader's peak transient RAM:
//
//   - The byte cap (this value) bounds the total bytes drawn from the
//     reader. [encoding/csv] does not bound the size of a single field, so a
//     hostile input such as an unterminated quoted field is buffered up to
//     MaxBytes and the working set (raw buffer plus the parsed field)
//     amplifies that to roughly 4–5× the cap.
//   - A per-record field-count guard (see the internal fieldGuardReader) caps
//     the delimiter-separated fields in any one record. [encoding/csv]
//     allocates ~40 bytes of metadata per field, so without this guard a
//     single delimiter-only record would amplify its bytes ~40× — several GiB
//     at this cap. The guard bounds that per-record metadata term to a few
//     MiB regardless of MaxBytes.
//
// DefaultMaxBytes is set to 128 MiB; with both bounds in force the worst-case
// transient stays a small multiple of MaxBytes (dominated by field content,
// not by per-field metadata), so raising MaxBytes for a trusted large input
// scales peak RAM proportionally rather than pathologically. Callers parsing
// untrusted input should keep the default or lower it further.
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
//
// Options holds only scalars and is copied by value into every [ReadInto],
// [ReadIntoCtx], [Write], and [WriteCtx] call, so one Options may configure any
// number of concurrent readers and writers. It carries no synchronisation of
// its own: mutating a shared Options races the goroutines that are copying it
// into a call, so populate it before those calls start.
type Options struct {
	// MaxBytes caps the number of bytes read from the input before the
	// reader fails with [ErrInputTooLarge]. [DefaultOptions] sets it to
	// [DefaultMaxBytes].
	//
	// SECURITY: a value of zero or less DISABLES the cap entirely, so the
	// reader will consume unbounded input. Because the zero value of an
	// Options literal (a bare Options{}) leaves MaxBytes at 0, constructing
	// Options by hand for UNTRUSTED input silently opts out of the memory
	// bound. Prefer starting from [DefaultOptions] (which sets MaxBytes to
	// [DefaultMaxBytes]) and overriding the fields you need, or set an
	// explicit positive MaxBytes. Leave the cap disabled only for input you
	// fully trust.
	MaxBytes int64
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

	// SanitizeFormulae, when true, neutralises spreadsheet formula
	// injection (OWASP CSV injection, CWE-1236) on the write path. A cell
	// whose first character is one of '=', '+', '-', '@', TAB (0x09), or
	// CR (0x0D) is treated as a live formula by Excel, LibreOffice Calc,
	// and Google Sheets when the exported file is opened, enabling DDE
	// command execution or data exfiltration in the context of the human
	// who opens it. With this option set, [Write] and [WriteCtx] prefix
	// each such cell with a single apostrophe ('), the de-facto neutraliser
	// those spreadsheets honour, so the value is rendered as text.
	//
	// It is OFF by default to preserve the lossless round-trip: an
	// apostrophe-prefixed cell no longer re-imports byte-identically
	// through [ReadInto], so a graph written with the default options round
	// -trips exactly while one written with sanitisation enabled does not.
	// Enable it only when the destination is a spreadsheet and faithful
	// re-import is not required. This flag affects the writer only; the
	// reader ignores it.
	SanitizeFormulae bool
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
// csv decode + opt defaults + per-row parse + ctx tick
func ReadIntoCtx(ctx context.Context, r io.Reader, opts Options) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.csv.ReadInto").Stop()
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	if opts.Comment == 0 {
		opts.Comment = '#'
	}
	if opts.MaxBytes > 0 {
		r = newLimitReader(r, opts.MaxBytes)
	}
	// Strip a leading UTF-8 BOM (EF BB BF), emitted by Excel and other
	// Windows spreadsheet tools, before handing the stream to encoding/csv.
	// Left in place it would prefix the first node id with U+FEFF, so the
	// same logical id written without a BOM would silently fail to match.
	br := bufio.NewReader(r)
	if bom, _ := br.Peek(3); len(bom) == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	// Bound the number of fields encoding/csv may allocate per record. Without
	// it a single delimiter-only record amplifies its bytes ~40x into
	// per-field metadata, turning one accepted file into a multi-GiB OOM
	// (CWE-789/CWE-1284); see [fieldGuardReader]. The guard is byte-oriented,
	// so it engages only for a single-byte (ASCII) delimiter — the norm; a
	// multi-byte delimiter rune (application-chosen, never file-chosen) relies
	// on the byte cap alone.
	var src io.Reader = br
	if opts.Delimiter < utf8.RuneSelf {
		src = newFieldGuardReader(br, byte(opts.Delimiter))
	}
	c := csv.NewReader(src)
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

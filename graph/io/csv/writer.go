package csv

import (
	"bufio"
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"unicode/utf8"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Write streams every edge of a in src,dst,weight order to w.
// Returns the number of rows written.
func Write(w io.Writer, a *adjlist.AdjList[string, int64], opts Options) (int, error) {
	n, err := WriteCtx(context.Background(), w, a, opts)
	if err != nil {
		metrics.IncCounter("graph.io.csv.Write.errors", 1)
	}
	return n, err
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked every 4096 rows; on cancellation returns
// (rowsWritten, wrapped ctx.Err()).
//
//nolint:gocyclo // CSV write loop: header + per-source resolve + per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64], opts Options) (int, error) {
	defer metrics.Time("graph.io.csv.Write").Stop()
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	// Default the comment rune to match [ReadIntoCtx], which treats a zero
	// Comment as '#'. The edge loop force-quotes any cell whose first rune is
	// this rune, so a leading such cell is never re-read as a comment line and
	// silently dropped (rmp #2042).
	if opts.Comment == 0 {
		opts.Comment = '#'
	}
	// csv.Writer wraps its sink in a private bufio.Writer; wrapping w here lets
	// the rare force-quoted record (built manually below) share that same
	// buffered sink, so a comment-prefixed row never forces a per-row syscall.
	bw := bufio.NewWriter(w)
	cw := csv.NewWriter(bw)
	cw.Comma = opts.Delimiter

	written := 0
	if opts.HasHeader {
		if err := cw.Write([]string{"src", "dst", "weight"}); err != nil {
			metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
			return written, err
		}
	}
	maxID := uint64(a.MaxNodeID())
	// Pre-resolve every live name in one shard-batched pass so the
	// inner edge loop pays no per-node Mapper.Resolve cost.
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graphNodeID, v string) bool {
		names[uint64(id)] = v
		live[uint64(id)] = true
		return true
	})
	// row is the reused 3-cell record handed to the csv.Writer, so the hot
	// edge loop does not allocate a fresh []string per edge. csv.Writer.Write
	// consumes the slice synchronously (it does not retain it), so reuse is safe.
	row := make([]string, 3)
	// weightScratch backs the weight cell so the hot loop pays no per-edge
	// weight allocation: strconv.FormatInt heap-allocates for |weight| >= 100,
	// whereas AppendInt into a reused buffer plus an unsafe.String view over
	// it does not. The view is safe for the same reason the reused row is:
	// csv.Writer.Write copies each field into its own buffer and retains
	// neither the record nor its strings, so weightScratch is only ever
	// overwritten after Write has returned (rmp #1523).
	var weightScratch []byte
	// quoteScratch backs the manually built force-quoted record used for a row
	// whose cell begins with the comment rune; reused across rows so the
	// pathological path allocates at most once.
	var quoteScratch []byte
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		nb, ws := a.LoadEntry(graphNodeID(id))
		if len(nb) == 0 {
			continue
		}
		src := names[id]
		for i, n := range nb {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					cw.Flush()
					// Best-effort: persist the rows already emitted before
					// reporting the cancellation; the ctx error is the result.
					_ = bw.Flush()
					metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
					return written, cerr
				}
			}
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			srcCell := src
			dstCell := names[uint64(n)]
			// A weightless source (Config.Weightless) carries no weights column,
			// so ws is nil; emit "0", identical to a genuine 0 weight.
			var weight int64
			if ws != nil {
				weight = ws[i]
			}
			weightScratch = strconv.AppendInt(weightScratch[:0], weight, 10)
			//nolint:gosec // zero-copy view of weightScratch; csv.Writer.Write consumes it synchronously (see weightScratch comment)
			weightCell := unsafe.String(unsafe.SliceData(weightScratch), len(weightScratch))
			if opts.SanitizeFormulae {
				srcCell = sanitizeFormulaCell(srcCell)
				dstCell = sanitizeFormulaCell(dstCell)
				weightCell = sanitizeFormulaCell(weightCell)
			}
			if startsWithRune(srcCell, opts.Comment) ||
				startsWithRune(dstCell, opts.Comment) ||
				startsWithRune(weightCell, opts.Comment) {
				// encoding/csv.Writer quotes a field only on a delimiter, quote,
				// CR/LF, or leading space — never on a leading comment rune — so
				// a cell that merely begins with it would be written bare. On
				// read a bare leading such cell makes encoding/csv treat the
				// whole line as a comment and silently drop the edge (rmp #2042).
				// Emit the record with every field explicitly quoted; the reader
				// recovers it verbatim. Flush any csv-buffered rows first so the
				// manual record keeps its place in the stream.
				cw.Flush()
				if err := cw.Error(); err != nil {
					metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
					return written, err
				}
				quoteScratch = appendForceQuotedRecord(quoteScratch[:0], opts.Delimiter, srcCell, dstCell, weightCell)
				if _, err := bw.Write(quoteScratch); err != nil {
					metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
					return written, err
				}
			} else {
				row[0], row[1], row[2] = srcCell, dstCell, weightCell
				if err := cw.Write(row); err != nil {
					metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
					return written, err
				}
			}
			written++
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
		return written, err
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
		return written, err
	}
	return written, nil
}

// startsWithRune reports whether s begins with the rune r: a cheap first-byte
// test for an ASCII r (the common case) and a full rune decode otherwise. An
// empty s or a zero r never matches.
func startsWithRune(s string, r rune) bool {
	if s == "" || r == 0 {
		return false
	}
	if r < utf8.RuneSelf {
		return s[0] == byte(r)
	}
	fr, _ := utf8.DecodeRuneInString(s)
	return fr == r
}

// appendForceQuotedRecord appends to dst a CSV record whose every field is
// explicitly double-quoted per RFC 4180 (wrapped in quotes, embedded quotes
// doubled), fields separated by delim and the record terminated by '\n'. The
// delimiter and terminator mirror encoding/csv.Writer's defaults (Comma, no
// UseCRLF), and encoding/csv reads the quoted record back verbatim, so it is
// used for the rare row whose bare leading cell would otherwise be dropped as
// a comment line on read (rmp #2042).
func appendForceQuotedRecord(dst []byte, delim rune, cells ...string) []byte {
	for i, c := range cells {
		if i > 0 {
			dst = utf8.AppendRune(dst, delim)
		}
		dst = append(dst, '"')
		for j := 0; j < len(c); j++ {
			b := c[j]
			dst = append(dst, b)
			if b == '"' {
				dst = append(dst, '"')
			}
		}
		dst = append(dst, '"')
	}
	return append(dst, '\n')
}

// sanitizeFormulaCell neutralises a spreadsheet formula-injection payload
// (OWASP CSV injection, CWE-1236) by prefixing a single apostrophe when the
// cell's first byte is one of the formula-trigger characters honoured by
// Excel, LibreOffice Calc, and Google Sheets: '=', '+', '-', '@', TAB
// (0x09), or CR (0x0D). Spreadsheets render an apostrophe-prefixed cell as
// literal text rather than evaluating it as a formula. An empty cell is
// returned unchanged. This is applied on the write path only when
// [Options.SanitizeFormulae] is set; see that field for the round-trip
// trade-off.
func sanitizeFormulaCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

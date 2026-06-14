package csv

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Write streams every edge of a in src,dst,weight order to w.
// Returns the number of rows written.
func Write(w io.Writer, a *adjlist.AdjList[string, int64], opts Options) (int, error) {
	defer metrics.Time("graph.io.csv.Write")()
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
	defer metrics.Time("graph.io.csv.WriteCtx")()
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	cw := csv.NewWriter(w)
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
					metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
					return written, cerr
				}
			}
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			srcCell := src
			dstCell := names[uint64(n)]
			weightCell := strconv.FormatInt(ws[i], 10)
			if opts.SanitizeFormulae {
				srcCell = sanitizeFormulaCell(srcCell)
				dstCell = sanitizeFormulaCell(dstCell)
				weightCell = sanitizeFormulaCell(weightCell)
			}
			if err := cw.Write([]string{srcCell, dstCell, weightCell}); err != nil {
				metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
				return written, err
			}
			written++
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		metrics.IncCounter("graph.io.csv.WriteCtx.errors", 1)
		return written, err
	}
	return written, nil
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

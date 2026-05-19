package csv

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"

	"gograph/graph/adjlist"
)

// Write streams every edge of a in src,dst,weight order to w.
// Returns the number of rows written.
func Write(w io.Writer, a *adjlist.AdjList[string, int64], opts Options) (int, error) {
	return WriteCtx(context.Background(), w, a, opts)
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked every 4096 rows; on cancellation returns
// (rowsWritten, wrapped ctx.Err()).
//
//nolint:gocyclo // CSV write loop: header + per-source resolve + per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64], opts Options) (int, error) {
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	cw := csv.NewWriter(w)
	cw.Comma = opts.Delimiter

	written := 0
	if opts.HasHeader {
		if err := cw.Write([]string{"src", "dst", "weight"}); err != nil {
			return written, err
		}
	}
	maxID := uint64(a.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		nb, ws := a.LoadEntry(graphNodeID(id))
		if len(nb) == 0 {
			continue
		}
		src, ok := a.Mapper().Resolve(graphNodeID(id))
		if !ok {
			continue
		}
		for i, n := range nb {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					cw.Flush()
					return written, cerr
				}
			}
			dst, ok := a.Mapper().Resolve(n)
			if !ok {
				continue
			}
			if err := cw.Write([]string{src, dst, strconv.FormatInt(ws[i], 10)}); err != nil {
				return written, err
			}
			written++
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return written, err
	}
	return written, nil
}

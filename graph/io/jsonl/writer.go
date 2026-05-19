package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"io"

	"gograph/graph"
	"gograph/graph/adjlist"
)

// Write streams every node and edge of a to w as JSON Lines. Nodes
// come first, then edges, so that on-read every endpoint is known
// before its referencing edge.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
	return WriteCtx(context.Background(), w, a)
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked every 4096 records; on cancellation flushes the buffer
// and returns (recordsWritten, wrapped ctx.Err()).
//
//nolint:gocyclo // JSONL write: per-node and per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
	bw := bufio.NewWriterSize(w, 64*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	written := 0

	maxID := uint64(a.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		name, ok := a.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		if err := enc.Encode(Record{Type: "node", ID: name}); err != nil {
			return written, err
		}
		written++
	}
	for id := uint64(0); id < maxID; id++ {
		src, ok := a.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					_ = bw.Flush()
					return written, cerr
				}
			}
			dst, ok := a.Mapper().Resolve(n)
			if !ok {
				continue
			}
			if err := enc.Encode(Record{Type: "edge", Src: src, Dst: dst, Weight: ws[i]}); err != nil {
				return written, err
			}
			written++
		}
	}
	if err := bw.Flush(); err != nil {
		return written, err
	}
	return written, nil
}

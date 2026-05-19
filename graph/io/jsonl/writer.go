package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"io"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/internal/metrics"
)

// Write streams every node and edge of a to w as JSON Lines. Nodes
// come first, then edges, so that on-read every endpoint is known
// before its referencing edge.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
	defer metrics.Time("graph.io.jsonl.Write")()
	n, err := WriteCtx(context.Background(), w, a)
	if err != nil {
		metrics.IncCounter("graph.io.jsonl.Write.errors", 1)
	}
	return n, err
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked every 4096 records; on cancellation flushes the buffer
// and returns (recordsWritten, wrapped ctx.Err()).
//
//nolint:gocyclo // JSONL write: per-node and per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
	defer metrics.Time("graph.io.jsonl.WriteCtx")()
	bw := bufio.NewWriterSize(w, 64*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	written := 0

	maxID := uint64(a.MaxNodeID())
	// Pre-resolve every live name in one shard-batched pass so the
	// inner edge loop pays no per-node Mapper.Resolve cost.
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graph.NodeID, v string) bool {
		names[uint64(id)] = v
		live[uint64(id)] = true
		return true
	})
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		if err := enc.Encode(Record{Type: "node", ID: names[id]}); err != nil {
			metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
			return written, err
		}
		written++
	}
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		src := names[id]
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					_ = bw.Flush()
					metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
					return written, cerr
				}
			}
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			if err := enc.Encode(Record{Type: "edge", Src: src, Dst: names[uint64(n)], Weight: ws[i]}); err != nil {
				metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
				return written, err
			}
			written++
		}
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
		return written, err
	}
	return written, nil
}

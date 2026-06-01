// Package dot writes graphs in the Graphviz DOT format
// (https://graphviz.org/doc/info/lang.html). Useful for quick visual
// inspection — pipe the output through 'dot -Tsvg' or 'dot -Tpng'.
package dot

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

var _ = io.Discard

// Write streams a DOT document representing a to w. The header
// uses 'digraph' for directed graphs and 'graph' for undirected.
// Edge weights are emitted as a label="..." attribute when non-zero.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) error {
	defer metrics.Time("graph.io.dot.Write")()
	err := WriteCtx(context.Background(), w, a)
	if err != nil {
		metrics.IncCounter("graph.io.dot.Write.errors", 1)
	}
	return err
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked once per source vertex; on cancellation flushes the buffer
// and returns the wrapped ctx.Err.
//
//nolint:gocyclo // DOT write: header + per-source resolve + per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64]) error {
	defer metrics.Time("graph.io.dot.WriteCtx")()
	bw := bufio.NewWriterSize(w, 64*1024)
	edgeOp := "->"
	header := "digraph G {\n"
	if !a.Directed() {
		header = "graph G {\n"
		edgeOp = "--"
	}
	if _, err := bw.WriteString(header); err != nil {
		metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
		return err
	}
	maxID := uint64(a.MaxNodeID())
	seenEdge := func(srcID, dstID graph.NodeID) bool {
		// For undirected graphs, emit (u, v) only when u <= v to
		// avoid duplicate output for the mirrored pair.
		if a.Directed() {
			return true
		}
		return uint64(srcID) <= uint64(dstID)
	}
	// Pre-resolve every live name in one shard-batched pass so the
	// inner edge loop pays no per-node Mapper.Resolve cost (each
	// Resolve previously took a shard RLock; for dense graphs this
	// dominated the writer wall-clock).
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graph.NodeID, v string) bool {
		names[uint64(id)] = v
		live[uint64(id)] = true
		return true
	})
	for id := uint64(0); id < maxID; id++ {
		if err := ctx.Err(); err != nil {
			_ = bw.Flush()
			metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
			return err
		}
		if !live[id] {
			continue
		}
		srcName := names[id]
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if !seenEdge(graph.NodeID(id), n) {
				continue
			}
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			dstName := names[uint64(n)]
			label := ""
			if ws[i] != 0 {
				label = fmt.Sprintf(` [label=%q]`, strconv.FormatInt(ws[i], 10))
			}
			line := fmt.Sprintf("  %s %s %s%s;\n", quote(srcName), edgeOp, quote(dstName), label)
			if _, err := bw.WriteString(line); err != nil {
				metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
				return err
			}
		}
	}
	if _, err := bw.WriteString("}\n"); err != nil {
		metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
		return err
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
		return err
	}
	return nil
}

// quote escapes a DOT identifier when it contains characters
// outside the safe alphabet; otherwise returns it unchanged.
func quote(s string) string {
	if isSimpleID(s) {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func isSimpleID(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

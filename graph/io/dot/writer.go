// Package dot writes graphs in the Graphviz DOT format
// (https://graphviz.org/doc/info/lang.html). Useful for quick visual
// inspection — pipe the output through 'dot -Tsvg' or 'dot -Tpng'.
package dot

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"gograph/graph"
	"gograph/graph/adjlist"
)

var _ = io.Discard

// Write streams a DOT document representing a to w. The header
// uses 'digraph' for directed graphs and 'graph' for undirected.
// Edge weights are emitted as a label="..." attribute when non-zero.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) error {
	return WriteCtx(context.Background(), w, a)
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked once per source vertex; on cancellation flushes the buffer
// and returns the wrapped ctx.Err.
//
//nolint:gocyclo // DOT write: header + per-source resolve + per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64]) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	edgeOp := "->"
	header := "digraph G {\n"
	if !a.Directed() {
		header = "graph G {\n"
		edgeOp = "--"
	}
	if _, err := bw.WriteString(header); err != nil {
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
	for id := uint64(0); id < maxID; id++ {
		if err := ctx.Err(); err != nil {
			_ = bw.Flush()
			return err
		}
		srcName, ok := a.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if !seenEdge(graph.NodeID(id), n) {
				continue
			}
			dstName, ok := a.Mapper().Resolve(n)
			if !ok {
				continue
			}
			label := ""
			if ws[i] != 0 {
				label = fmt.Sprintf(` [label=%q]`, fmt.Sprintf("%d", ws[i]))
			}
			line := fmt.Sprintf("  %s %s %s%s;\n", quote(srcName), edgeOp, quote(dstName), label)
			if _, err := bw.WriteString(line); err != nil {
				return err
			}
		}
	}
	if _, err := bw.WriteString("}\n"); err != nil {
		return err
	}
	return bw.Flush()
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

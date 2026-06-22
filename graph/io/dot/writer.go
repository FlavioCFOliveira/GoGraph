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
	// appeared[id] records whether a vertex was emitted as the source or
	// target of any visible edge; vertices that never appear are written
	// as bare node statements below so isolated nodes are not lost.
	appeared := make([]bool, maxID)
	// scratch is the reused per-edge line buffer: the edge statement is
	// assembled here with append + strconv.AppendInt for the weight, so the
	// hot loop pays no per-edge fmt.Sprintf / FormatInt allocation (only the
	// node-name quoting still allocates, on the string path).
	var scratch []byte
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
			// A weightless source (Config.Weightless) carries no weights column,
			// so ws is nil; treat the absent weight as 0 (no label), identical to
			// a genuine 0 weight.
			var weight int64
			if ws != nil {
				weight = ws[i]
			}
			scratch = scratch[:0]
			scratch = append(scratch, "  "...)
			scratch = append(scratch, quote(srcName)...)
			scratch = append(scratch, ' ')
			scratch = append(scratch, edgeOp...)
			scratch = append(scratch, ' ')
			scratch = append(scratch, quote(dstName)...)
			if weight != 0 {
				// Byte-identical to fmt.Sprintf(` [label=%q]`, …): the weight is
				// a digit string, so %q just wraps it in double quotes.
				scratch = append(scratch, ` [label="`...)
				scratch = strconv.AppendInt(scratch, weight, 10)
				scratch = append(scratch, '"', ']')
			}
			scratch = append(scratch, ';', '\n')
			if _, err := bw.Write(scratch); err != nil {
				metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
				return err
			}
			appeared[id] = true
			appeared[uint64(n)] = true
		}
	}
	// Emit a bare node statement for every live vertex that no edge
	// referenced, so isolated vertices (no incident edges) survive the
	// write. DOT and Graphviz both accept bare node statements.
	for id := uint64(0); id < maxID; id++ {
		if err := ctx.Err(); err != nil {
			_ = bw.Flush()
			metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
			return err
		}
		if !live[id] || appeared[id] {
			continue
		}
		if _, err := fmt.Fprintf(bw, "  %s;\n", quote(names[id])); err != nil {
			metrics.IncCounter("graph.io.dot.WriteCtx.errors", 1)
			return err
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

// dotReservedKeywords is the set of DOT language reserved words. Per the
// Graphviz DOT grammar (https://graphviz.org/doc/info/lang.html) these are
// case-independent keywords that "may not be used as identifiers" unless
// quoted. Matched against strings.ToLower(id) so every casing is covered.
var dotReservedKeywords = map[string]struct{}{
	"node":     {},
	"edge":     {},
	"graph":    {},
	"digraph":  {},
	"subgraph": {},
	"strict":   {},
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
	// An id whose lowercase form is a DOT reserved keyword must be quoted,
	// otherwise Graphviz reinterprets e.g. 'node -> safe;' as a default-
	// attribute statement rather than an edge from a vertex named 'node',
	// silently corrupting the export (#1489).
	if _, reserved := dotReservedKeywords[strings.ToLower(s)]; reserved {
		return false
	}
	return true
}

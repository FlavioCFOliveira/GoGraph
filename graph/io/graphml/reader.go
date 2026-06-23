// Package graphml reads and writes graphs in the GraphML XML
// dialect (http://graphml.graphdrawing.org/). v1 supports the
// commonly-encountered shape: <node id="...">, <edge source="..."
// target="..."> with an optional <data key="..."> carrying an int64
// weight under a <key id="..." attr.name="weight" .../>
// declaration.
package graphml

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// DefaultMaxBytes is the default ceiling, in bytes, on the amount of
// input the default read entry points will consume before failing with
// [ErrInputTooLarge]. It guards against memory exhaustion from untrusted
// documents (a crafted multi-gigabyte GraphML file, for example). The
// capped variants ([ReadIntoCappedCtx], [ReadWithPropsCappedCtx]) accept
// an explicit ceiling; a value of zero or less disables the cap.
//
// # Peak memory
//
// The cap bounds the number of bytes drawn from the reader, but
// [encoding/xml] does not bound the size of a single token. A hostile
// input — for example an unterminated attribute value or chardata run —
// is buffered by the decoder up to maxBytes, and the decoder's working
// set amplifies that to roughly 3–4× the cap. Peak transient RAM is
// therefore on the order of 3–4 × maxBytes, not maxBytes.
//
// DefaultMaxBytes is set to 128 MiB so that this worst-case transient
// stays well under 1 GiB even on a hostile single-token document. Callers
// importing larger trusted documents pass an explicit ceiling to the
// capped variants, accepting the proportionally higher peak; callers
// parsing untrusted input should keep the default or lower it further.
const DefaultMaxBytes int64 = 128 << 20 // 128 MiB

// ErrInputTooLarge is returned by the read functions when the input
// stream exceeds the configured byte ceiling. The decoder stops drawing
// bytes from the input as soon as the limit is crossed; note, however,
// that a single oversized token may already have been buffered by
// [encoding/xml] up to the cap before the limit trips, so the decoder's
// peak working set is a multiple of the cap (see [DefaultMaxBytes]).
var ErrInputTooLarge = errors.New("graphml: input exceeds maximum size")

// keyDecl mirrors a <key> declaration in a GraphML document.
type keyDecl struct {
	ID       string `xml:"id,attr"`
	For      string `xml:"for,attr"`
	AttrName string `xml:"attr.name,attr"`
	AttrType string `xml:"attr.type,attr"`
}

// nodeElement mirrors a <node> element. Data carries any <data> children
// for typed-property support (see [ReadWithPropsCtx]).
type nodeElement struct {
	ID   string        `xml:"id,attr"`
	Data []dataElement `xml:"data"`
}

// dataElement mirrors a <data key="..."> with text content.
type dataElement struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

// edgeElement mirrors an <edge>.
type edgeElement struct {
	Source string        `xml:"source,attr"`
	Target string        `xml:"target,attr"`
	Data   []dataElement `xml:"data"`
}

// graphElement mirrors a <graph>.
type graphElement struct {
	EdgeDefault string        `xml:"edgedefault,attr"`
	Nodes       []nodeElement `xml:"node"`
	Edges       []edgeElement `xml:"edge"`
}

// docElement mirrors a <graphml> document.
type docElement struct {
	XMLName xml.Name       `xml:"graphml"`
	Keys    []keyDecl      `xml:"key"`
	Graphs  []graphElement `xml:"graph"`
}

// ReadInto parses a GraphML document from r into an adjacency list.
// Returns the loaded list, the number of edges added, and an error
// on parse failure.
func ReadInto(r io.Reader) (*adjlist.AdjList[string, int64], int, error) {
	a, n, err := ReadIntoCtx(context.Background(), r)
	if err != nil {
		metrics.IncCounter("graph.io.graphml.ReadInto.errors", 1)
	}
	return a, n, err
}

// ReadIntoCtx is the context-aware variant of [ReadInto]. ctx.Err()
// is checked every 4096 edges. The input is capped at [DefaultMaxBytes];
// use [ReadIntoCappedCtx] for an explicit ceiling.
//
// On any error — a parse error, context cancellation, or the
// [ErrInputTooLarge] cap — the returned graph is nil; the import is
// all-or-nothing at the in-memory level, so a caller cannot accidentally
// commit a half-built graph. The typed error is returned unchanged; only
// the graph value is discarded.
func ReadIntoCtx(ctx context.Context, r io.Reader) (*adjlist.AdjList[string, int64], int, error) {
	return ReadIntoCappedCtx(ctx, r, DefaultMaxBytes)
}

// ReadIntoCappedCtx is [ReadIntoCtx] with an explicit input-size
// ceiling. When maxBytes > 0 the decoder fails with [ErrInputTooLarge]
// the moment consumption exceeds the limit, before the whole document
// is buffered; a value of zero or less disables the cap.
//
// On any error the returned graph is nil (see [ReadIntoCtx]); the import
// is all-or-nothing at the in-memory level.
//
//nolint:gocyclo // GraphML decode + key lookup + per-edge parse + ctx tick
func ReadIntoCappedCtx(ctx context.Context, r io.Reader, maxBytes int64) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.graphml.ReadInto")()
	if maxBytes > 0 {
		r = newLimitReader(r, maxBytes)
	}
	dec := xml.NewDecoder(r)
	var doc docElement
	if err := dec.Decode(&doc); err != nil {
		metrics.IncCounter("graph.io.graphml.ReadIntoCtx.errors", 1)
		return nil, 0, fmt.Errorf("graphml: parse: %w", err)
	}
	if len(doc.Graphs) == 0 {
		return adjlist.New[string, int64](adjlist.Config{Directed: true}), 0, nil
	}
	weightKey := findWeightKey(doc.Keys)
	g := doc.Graphs[0]
	a := adjlist.New[string, int64](adjlist.Config{Directed: g.EdgeDefault != "undirected"})
	for _, n := range g.Nodes {
		if err := a.AddNode(n.ID); err != nil {
			metrics.IncCounter("graph.io.graphml.ReadIntoCtx.errors", 1)
			return nil, 0, fmt.Errorf("graphml: AddNode(%q): %w", n.ID, err)
		}
	}
	added := 0
	for _, e := range g.Edges {
		if added&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.graphml.ReadIntoCtx.errors", 1)
				return nil, added, err
			}
		}
		var w int64
		for _, d := range e.Data {
			if d.Key == weightKey && weightKey != "" {
				v, perr := strconv.ParseInt(d.Value, 10, 64)
				if perr != nil {
					metrics.IncCounter("graph.io.graphml.ReadIntoCtx.errors", 1)
					return nil, added, fmt.Errorf("graphml: edge (%q,%q) weight %q: %w", e.Source, e.Target, d.Value, perr)
				}
				w = v
			}
		}
		if err := a.AddEdge(e.Source, e.Target, w); err != nil {
			metrics.IncCounter("graph.io.graphml.ReadIntoCtx.errors", 1)
			return nil, added, fmt.Errorf("graphml: AddEdge(%q, %q): %w", e.Source, e.Target, err)
		}
		added++
	}
	return a, added, nil
}

func findWeightKey(keys []keyDecl) string {
	for _, k := range keys {
		if k.AttrName == "weight" && (k.For == "edge" || k.For == "") {
			return k.ID
		}
	}
	return ""
}

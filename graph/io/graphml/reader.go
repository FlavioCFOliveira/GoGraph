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
// The reader streams the document element by element: it folds each
// <node>/<edge> into the graph as that element is decoded rather than
// materialising the whole document as a struct-per-element DOM first. Peak
// transient memory therefore tracks the resulting graph — itself bounded by
// the input, and the input by this cap — plus a single element's working set,
// not a multiple of the input inflated by a retained DOM. (An earlier revision
// decoded the whole document with dec.Decode; a 128 MiB file of millions of
// tiny <edge/>/<node/>/<key/> tags then forced 2.2–3.4 GiB of transient heap.
// See ReadIntoCappedCtx.) [encoding/xml] still does not bound the size of a
// single token, so a pathological oversized token (an unterminated attribute
// value or chardata run) is buffered up to maxBytes with the decoder's usual
// ~3–4× working-set factor; that single-token transient — not a whole-document
// DOM — is now the worst case.
//
// DefaultMaxBytes is set to 128 MiB. Callers importing larger trusted
// documents pass an explicit ceiling to the capped variants; callers parsing
// untrusted input should keep the default or lower it further.
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

// The reader streams the document with dec.Token and dec.DecodeElement one
// <key>/<node>/<edge> at a time (see streamGraphMLFirstGraph); it never
// materialises a whole-document DOM, so there is no aggregate document struct.
// The <graphml> root and <graph> container are handled positionally by the
// token loop, and edgedefault is read from the <graph> start-element attribute.

// maxKeyDecls bounds the number of <key> declarations a document may carry.
// Keys are the schema (one per typed property) and, unlike <node>/<edge>, they
// are retained in a lookup index rather than folded into the output graph, so
// an unbounded <key/> flood would grow that index without limit — the
// memory-amplification vector for keys. A real GraphML document declares a
// handful of keys; 65536 is far above any legitimate schema while rejecting a
// flood. Exceeding it fails with [ErrTooManyKeys].
const maxKeyDecls = 1 << 16

// ErrTooManyKeys is returned when a document declares more than [maxKeyDecls]
// <key> elements — a schema flood used to amplify memory.
var ErrTooManyKeys = errors.New("graphml: too many <key> declarations")

// streamGraphMLFirstGraph tokenises dec and drives the callbacks as each
// element is decoded, retaining at most one <node>/<edge> element at a time
// plus the (capped) <key> declarations — never the whole document. Per the
// GraphML spec (and this package's own writer) every <key> precedes <graph>,
// so onGraph fires with the fully-collected keys before any onNode/onEdge; a
// <key> that appears after <graph> in malformed input is simply not seen by the
// key index (its typed <data> is treated as an unknown key, exactly as the
// prior DOM reader treated an unresolved key). Only the first <graph> is
// processed, matching the prior reader's doc.Graphs[0] behaviour. onGraph is
// not called when the document contains no <graph>. Decode/token (parse) errors
// are wrapped as "graphml: parse: %w"; callback errors are returned verbatim so
// callers can attach their own context. ctx is checked every 4096 edges.
func streamGraphMLFirstGraph(
	ctx context.Context,
	dec *xml.Decoder,
	onGraph func(keys []keyDecl, directed bool) error,
	onNode func(n *nodeElement) error,
	onEdge func(e *edgeElement) error,
) error {
	// The document root must be <graphml> (the prior DOM reader pinned this via
	// its root struct tag); position the decoder just inside it so a non-graphml
	// or deeply-nested foreign root is rejected rather than mined for <node>s.
	if err := expectGraphMLRoot(dec); err != nil {
		return err
	}
	var keys []keyDecl
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil // clean end, no <graph>
		}
		if err != nil {
			return fmt.Errorf("graphml: parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				if len(keys) >= maxKeyDecls {
					return ErrTooManyKeys
				}
				var k keyDecl
				if err := dec.DecodeElement(&k, &t); err != nil {
					return fmt.Errorf("graphml: parse: %w", err)
				}
				keys = append(keys, k)
			case "graph":
				directed := true
				for _, a := range t.Attr {
					if a.Name.Local == "edgedefault" {
						directed = a.Value != "undirected"
					}
				}
				if err := onGraph(keys, directed); err != nil {
					return err
				}
				// Stream this graph's children (only the first graph is
				// processed, matching the prior reader's doc.Graphs[0])...
				if err := streamGraphChildren(ctx, dec, onNode, onEdge); err != nil {
					return err
				}
				// ...then consume the remaining <graphml> children up to
				// </graphml>, discarding them, so the input byte cap
				// (limitReader) is still enforced over the WHOLE document exactly
				// as the prior whole-document decode was. Tokens are discarded, so
				// this stays O(1) memory even for a large trailing <graph>.
				return drainToGraphMLEnd(dec)
			default:
				// A <graphml> child that is neither <key> nor <graph> (e.g.
				// <desc>): skip its whole subtree so a nested <node>/<edge> is
				// not misread as graph content.
				if err := dec.Skip(); err != nil {
					return fmt.Errorf("graphml: parse: %w", err)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "graphml" {
				return nil // end of document with no <graph>
			}
		}
	}
}

// drainToGraphMLEnd consumes the remaining children of the open <graphml>
// element up to and including its </graphml> end tag, discarding every token.
// It is called after the first <graph> has been folded so the input byte cap is
// enforced over the whole root element (a document that overflows the cap in its
// trailing bytes is still rejected), while a large trailing <graph> is skipped
// in O(1) memory rather than materialised. A single oversized trailing token is
// still bounded by the byte cap on the underlying reader.
func drainToGraphMLEnd(dec *xml.Decoder) error {
	depth := 0 // nesting below the (already-open) <graphml> root
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("graphml: parse: %w", err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return nil // </graphml>: the root is closed
			}
			depth--
		}
	}
}

// expectGraphMLRoot advances dec to the first StartElement and requires it to
// be <graphml>, matching the prior DOM reader whose root struct tag pinned the
// root. Leading ProcInst / Comment / CharData (the <?xml?> declaration,
// comments, whitespace) are skipped. A foreign or deeply-nested non-graphml
// root fails with a parse error before any content is folded.
func expectGraphMLRoot(dec *xml.Decoder) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("graphml: parse: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "graphml" {
				return fmt.Errorf("graphml: parse: unexpected root element <%s>, want <graphml>", se.Name.Local)
			}
			return nil
		}
	}
}

// streamGraphChildren consumes the children of the current <graph> element,
// decoding one <node>/<edge> at a time and folding it via the callbacks, until
// the matching </graph> end tag. See [streamGraphMLFirstGraph].
func streamGraphChildren(
	ctx context.Context,
	dec *xml.Decoder,
	onNode func(n *nodeElement) error,
	onEdge func(e *edgeElement) error,
) error {
	edgeCount := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			// EOF here means an unterminated <graph>; report it as a parse error
			// rather than a silent success.
			return fmt.Errorf("graphml: parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "node":
				var n nodeElement
				if err := dec.DecodeElement(&n, &t); err != nil {
					return fmt.Errorf("graphml: parse: %w", err)
				}
				if err := onNode(&n); err != nil {
					return err
				}
			case "edge":
				if edgeCount&0xFFF == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				var e edgeElement
				if err := dec.DecodeElement(&e, &t); err != nil {
					return fmt.Errorf("graphml: parse: %w", err)
				}
				if err := onEdge(&e); err != nil {
					return err
				}
				edgeCount++
			default:
				if err := dec.Skip(); err != nil {
					return fmt.Errorf("graphml: parse: %w", err)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "graph" {
				return nil
			}
		}
	}
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
	defer metrics.Time("graph.io.graphml.ReadInto").Stop()
	if maxBytes > 0 {
		r = newLimitReader(r, maxBytes)
	}
	dec := xml.NewDecoder(r)

	var (
		a         *adjlist.AdjList[string, int64]
		weightKey string
		added     int
	)
	err := streamGraphMLFirstGraph(ctx, dec,
		func(keys []keyDecl, directed bool) error {
			weightKey = findWeightKey(keys)
			a = adjlist.New[string, int64](adjlist.Config{Directed: directed})
			return nil
		},
		func(n *nodeElement) error {
			if err := a.AddNode(n.ID); err != nil {
				return fmt.Errorf("graphml: AddNode(%q): %w", n.ID, err)
			}
			return nil
		},
		func(e *edgeElement) error {
			var w int64
			for _, d := range e.Data {
				if d.Key == weightKey && weightKey != "" {
					v, perr := strconv.ParseInt(d.Value, 10, 64)
					if perr != nil {
						return fmt.Errorf("graphml: edge (%q,%q) weight %q: %w", e.Source, e.Target, d.Value, perr)
					}
					w = v
				}
			}
			if err := a.AddEdge(e.Source, e.Target, w); err != nil {
				return fmt.Errorf("graphml: AddEdge(%q, %q): %w", e.Source, e.Target, err)
			}
			added++
			return nil
		},
	)
	if err != nil {
		metrics.IncCounter("graph.io.graphml.ReadIntoCtx.errors", 1)
		return nil, added, err
	}
	// No <graph> element: an empty directed graph (matches the prior
	// len(doc.Graphs) == 0 behaviour).
	if a == nil {
		return adjlist.New[string, int64](adjlist.Config{Directed: true}), 0, nil
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

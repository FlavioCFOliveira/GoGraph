package graphml

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Write streams a GraphML document representing a to w. The output
// includes a single <graph> with directed or undirected edgedefault
// inferred from a, a <key for=edge attr.name=weight attr.type=long>
// declaration, and one <node>/<edge> per node and edge.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) error {
	defer metrics.Time("graph.io.graphml.Write")()
	err := WriteCtx(context.Background(), w, a)
	if err != nil {
		metrics.IncCounter("graph.io.graphml.Write.errors", 1)
	}
	return err
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked at the start of node and edge encoding; on cancellation
// returns the wrapped ctx.Err.
//
//nolint:gocyclo // GraphML write: XML header + key declaration + graph open + nodes + edges + close
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64]) error {
	defer metrics.Time("graph.io.graphml.WriteCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("graph.io.graphml.WriteCtx.errors", 1)
		return err
	}
	if _, err := io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")

	root := xml.StartElement{
		Name: xml.Name{Local: "graphml"},
		Attr: []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: "http://graphml.graphdrawing.org/xmlns"}},
	}
	if err := enc.EncodeToken(root); err != nil {
		return err
	}
	if err := enc.EncodeElement(struct {
		XMLName  xml.Name `xml:"key"`
		ID       string   `xml:"id,attr"`
		For      string   `xml:"for,attr"`
		AttrName string   `xml:"attr.name,attr"`
		AttrType string   `xml:"attr.type,attr"`
	}{ID: "w", For: "edge", AttrName: "weight", AttrType: "long"}, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	dir := "directed"
	if !a.Directed() {
		dir = "undirected"
	}
	graphStart := xml.StartElement{
		Name: xml.Name{Local: "graph"},
		Attr: []xml.Attr{
			{Name: xml.Name{Local: "id"}, Value: "G"},
			{Name: xml.Name{Local: "edgedefault"}, Value: dir},
		},
	}
	if err := enc.EncodeToken(graphStart); err != nil {
		return err
	}
	maxID := uint64(a.MaxNodeID())
	if err := encodeNodes(enc, a, maxID); err != nil {
		return err
	}
	if err := encodeEdges(enc, a, maxID, nil); err != nil {
		return err
	}
	if err := enc.EncodeToken(graphStart.End()); err != nil {
		return err
	}
	if err := enc.EncodeToken(root.End()); err != nil {
		return err
	}
	return enc.Flush()
}

func encodeNodes(enc *xml.Encoder, a *adjlist.AdjList[string, int64], _ uint64) error {
	var encErr error
	a.Mapper().Walk(func(_ graph.NodeID, name string) bool {
		if err := encodeNode(enc, name); err != nil {
			encErr = err
			return false
		}
		return true
	})
	return encErr
}

// encodeEdges emits one <edge> element per adjacency slot whose both
// endpoints are interned and not in dead. dead holds the tombstoned
// NodeIDs of an [lpg.Graph]-backed export (nil when the caller has no
// tombstone knowledge, e.g. the plain adjacency writer).
func encodeEdges(enc *xml.Encoder, a *adjlist.AdjList[string, int64], maxID uint64, dead map[graph.NodeID]struct{}) error {
	// Pre-resolve every live name in one shard-batched pass so the
	// inner edge loop pays no per-node Mapper.Resolve cost.
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graph.NodeID, v string) bool {
		names[uint64(id)] = v
		live[uint64(id)] = true
		return true
	})
	// Tombstoned endpoints are logically removed: clear them from the
	// live set so neither direction of an incident edge is emitted.
	for id := range dead {
		if uint64(id) < maxID {
			live[uint64(id)] = false
		}
	}
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		src := names[id]
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			// A weightless source (Config.Weightless) carries no weights column,
			// so ws is nil; emit the zero weight, identical to a genuine 0.
			var weight int64
			if ws != nil {
				weight = ws[i]
			}
			if err := encodeEdge(enc, src, names[uint64(n)], weight); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeNode(enc *xml.Encoder, id string) error {
	// Fail fast on node ids XML 1.0 cannot represent rather than letting
	// encoding/xml silently coerce them to U+FFFD (see ErrInvalidXMLChar).
	if err := validateXMLText(id); err != nil {
		return fmt.Errorf("graphml: node id %q: %w", id, err)
	}
	return enc.EncodeElement(struct {
		XMLName xml.Name `xml:"node"`
		ID      string   `xml:"id,attr"`
	}{ID: id}, xml.StartElement{Name: xml.Name{Local: "node"}})
}

func encodeEdge(enc *xml.Encoder, src, dst string, weight int64) error {
	type dataElem struct {
		XMLName xml.Name `xml:"data"`
		Key     string   `xml:"key,attr"`
		Value   string   `xml:",chardata"`
	}
	type edgeElem struct {
		XMLName xml.Name `xml:"edge"`
		Source  string   `xml:"source,attr"`
		Target  string   `xml:"target,attr"`
		Data    dataElem
	}
	return enc.EncodeElement(edgeElem{
		Source: src, Target: dst,
		// strconv.FormatInt is byte-identical to fmt.Sprintf("%d", …) for an
		// int64 but avoids the fmt reflection/pp-buffer allocation; the result
		// string itself is the encoding/xml chardata floor.
		Data: dataElem{Key: "w", Value: strconv.FormatInt(weight, 10)},
	}, xml.StartElement{Name: xml.Name{Local: "edge"}})
}

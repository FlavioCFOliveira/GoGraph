package graphml

import (
	"encoding/xml"
	"fmt"
	"io"

	"gograph/graph"
	"gograph/graph/adjlist"
)

// Write streams a GraphML document representing a to w. The output
// includes a single <graph> with directed or undirected edgedefault
// inferred from a, a <key for=edge attr.name=weight attr.type=long>
// declaration, and one <node>/<edge> per node and edge.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) error {
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
	if err := encodeEdges(enc, a, maxID); err != nil {
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

func encodeNodes(enc *xml.Encoder, a *adjlist.AdjList[string, int64], maxID uint64) error {
	for id := uint64(0); id < maxID; id++ {
		name, ok := a.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		if err := encodeNode(enc, name); err != nil {
			return err
		}
	}
	return nil
}

func encodeEdges(enc *xml.Encoder, a *adjlist.AdjList[string, int64], maxID uint64) error {
	for id := uint64(0); id < maxID; id++ {
		src, ok := a.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			dst, ok := a.Mapper().Resolve(n)
			if !ok {
				continue
			}
			if err := encodeEdge(enc, src, dst, ws[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeNode(enc *xml.Encoder, id string) error {
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
		Data: dataElem{Key: "w", Value: fmt.Sprintf("%d", weight)},
	}, xml.StartElement{Name: xml.Name{Local: "edge"}})
}

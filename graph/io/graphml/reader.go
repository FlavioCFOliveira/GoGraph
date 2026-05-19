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
	"fmt"
	"io"
	"strconv"

	"gograph/graph/adjlist"
)

// keyDecl mirrors a <key> declaration in a GraphML document.
type keyDecl struct {
	ID       string `xml:"id,attr"`
	For      string `xml:"for,attr"`
	AttrName string `xml:"attr.name,attr"`
	AttrType string `xml:"attr.type,attr"`
}

// nodeElement mirrors a <node> element.
type nodeElement struct {
	ID string `xml:"id,attr"`
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
	return ReadIntoCtx(context.Background(), r)
}

// ReadIntoCtx is the context-aware variant of [ReadInto]. ctx.Err()
// is checked every 4096 edges; on cancellation returns
// (partialAdj, edgesAdded, wrapped ctx.Err()).
//
//nolint:gocyclo // GraphML decode + key lookup + per-edge parse + ctx tick
func ReadIntoCtx(ctx context.Context, r io.Reader) (*adjlist.AdjList[string, int64], int, error) {
	dec := xml.NewDecoder(r)
	var doc docElement
	if err := dec.Decode(&doc); err != nil {
		return nil, 0, fmt.Errorf("graphml: parse: %w", err)
	}
	if len(doc.Graphs) == 0 {
		return adjlist.New[string, int64](adjlist.Config{Directed: true}), 0, nil
	}
	weightKey := findWeightKey(doc.Keys)
	g := doc.Graphs[0]
	a := adjlist.New[string, int64](adjlist.Config{Directed: g.EdgeDefault != "undirected"})
	for _, n := range g.Nodes {
		a.AddNode(n.ID)
	}
	added := 0
	for _, e := range g.Edges {
		if added&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return a, added, err
			}
		}
		var w int64
		for _, d := range e.Data {
			if d.Key == weightKey && weightKey != "" {
				v, err := strconv.ParseInt(d.Value, 10, 64)
				if err == nil {
					w = v
				}
			}
		}
		a.AddEdge(e.Source, e.Target, w)
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

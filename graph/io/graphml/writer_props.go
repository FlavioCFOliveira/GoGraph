package graphml

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
)

// graphMLAttrType maps a [lpg.PropertyKind] to the GraphML attr.type
// string used in a <key> declaration.
func graphMLAttrType(k lpg.PropertyKind) string {
	switch k {
	case lpg.PropInt64:
		return "long"
	case lpg.PropFloat64:
		return "double"
	case lpg.PropBool:
		return "boolean"
	case lpg.PropTime, lpg.PropBytes, lpg.PropString:
		return "string"
	default:
		return "string"
	}
}

// serialisePropertyValue serialises v to its GraphML text representation.
// For PropTime: RFC3339Nano. For PropBytes: base64 standard encoding.
// For PropFloat64: 'g' format preserving NaN/Inf. Other kinds fall back to
// fmt.Sprintf.
func serialisePropertyValue(v lpg.PropertyValue) string {
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return s
	case lpg.PropInt64:
		i, _ := v.Int64()
		return strconv.FormatInt(i, 10)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case lpg.PropBool:
		b, _ := v.Bool()
		if b {
			return "true"
		}
		return "false"
	case lpg.PropTime:
		t, _ := v.Time()
		return t.UTC().Format(time.RFC3339Nano)
	case lpg.PropBytes:
		b, _ := v.Bytes()
		return base64.StdEncoding.EncodeToString(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// deserialisePropertyValue converts the text s to a [lpg.PropertyValue]
// according to the GraphML attr.type in attrType.
func deserialisePropertyValue(attrType, s string) (lpg.PropertyValue, error) {
	switch attrType {
	case "long", "int":
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("graphml: parse long %q: %w", s, err)
		}
		return lpg.Int64Value(i), nil
	case "double", "float":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("graphml: parse double %q: %w", s, err)
		}
		return lpg.Float64Value(f), nil
	case "boolean":
		switch s {
		case "true", "1":
			return lpg.BoolValue(true), nil
		case "false", "0":
			return lpg.BoolValue(false), nil
		default:
			return lpg.PropertyValue{}, fmt.Errorf("graphml: parse boolean %q: unrecognised value", s)
		}
	default:
		// "string" or any unknown type — attempt temporal/bytes heuristics
		// only when the stored metadata hints at them; default is plain string.
		return lpg.StringValue(s), nil
	}
}

// propKeyDecl is the metadata kept per property <key> declaration.
type propKeyDecl struct {
	attrName string
	attrType string
	forElem  string // "node" or "edge"
}

// WriteWithProps writes a GraphML document to w for the LPG g.
// A <key> declaration is emitted for every property key encountered
// across all nodes, with attr.type set to the GraphML equivalent
// of the first value seen for that key. Properties are serialised as
// <data> child elements of their respective <node> elements.
// Edge weights are written with the standard id="w" key.
func WriteWithProps(w io.Writer, g *lpg.Graph[string, int64]) error {
	defer metrics.Time("graph.io.graphml.WriteWithProps")()
	err := WriteWithPropsCtx(context.Background(), w, g)
	if err != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithProps.errors", 1)
	}
	return err
}

// WriteWithPropsCtx is the context-aware variant of [WriteWithProps].
// ctx.Err() is checked before the node and edge encoding phases.
//
//nolint:gocyclo // GraphML typed-property write: key scan + XML emit + node/edge loop
func WriteWithPropsCtx(ctx context.Context, w io.Writer, g *lpg.Graph[string, int64]) error {
	defer metrics.Time("graph.io.graphml.WriteWithPropsCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
		return err
	}

	a := g.AdjList()

	// First pass: collect all property keys and their GraphML types
	// by walking every node's property bag.
	// keyMeta maps property-key name → propKeyDecl (first-seen type wins).
	keyMeta := make(map[string]propKeyDecl)
	a.Mapper().Walk(func(_ graph.NodeID, name string) bool {
		props := g.NodeProperties(name)
		for k, v := range props {
			if _, seen := keyMeta[k]; !seen {
				keyMeta[k] = propKeyDecl{
					attrName: k,
					attrType: graphMLAttrType(v.Kind()),
					forElem:  "node",
				}
			}
		}
		return true
	})

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

	// Emit the standard edge-weight <key>.
	if err := enc.EncodeElement(struct {
		XMLName  xml.Name `xml:"key"`
		ID       string   `xml:"id,attr"`
		For      string   `xml:"for,attr"`
		AttrName string   `xml:"attr.name,attr"`
		AttrType string   `xml:"attr.type,attr"`
	}{ID: "w", For: "edge", AttrName: "weight", AttrType: "long"}, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}

	// Emit one <key> per discovered node-property.
	// Use a deterministic ordering: sort by key name length then
	// lexicographically to avoid map-iteration non-determinism.
	propKeyNames := sortedKeys(keyMeta)
	for _, kn := range propKeyNames {
		decl := keyMeta[kn]
		if err := enc.EncodeElement(struct {
			XMLName  xml.Name `xml:"key"`
			ID       string   `xml:"id,attr"`
			For      string   `xml:"for,attr"`
			AttrName string   `xml:"attr.name,attr"`
			AttrType string   `xml:"attr.type,attr"`
		}{ID: propKeyID(kn), For: decl.forElem, AttrName: decl.attrName, AttrType: decl.attrType},
			xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
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

	if err := ctx.Err(); err != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
		return err
	}

	// Emit <node> elements with <data> children for each property.
	var encErr error
	a.Mapper().Walk(func(_ graph.NodeID, name string) bool {
		nodeStart := xml.StartElement{Name: xml.Name{Local: "node"},
			Attr: []xml.Attr{{Name: xml.Name{Local: "id"}, Value: name}}}
		if encErr = enc.EncodeToken(nodeStart); encErr != nil {
			return false
		}
		props := g.NodeProperties(name)
		for _, kn := range propKeyNames {
			v, ok := props[kn]
			if !ok {
				continue
			}
			encErr = encodeDataElem(enc, propKeyID(kn), serialisePropertyValue(v))
			if encErr != nil {
				return false
			}
		}
		encErr = enc.EncodeToken(nodeStart.End())
		return encErr == nil
	})
	if encErr != nil {
		return encErr
	}

	if err := ctx.Err(); err != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
		return err
	}

	// Emit <edge> elements using the same batched-name pattern as the
	// plain writer.
	if err := encodeEdges(enc, a, uint64(a.MaxNodeID())); err != nil {
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

// encodeDataElem emits a single <data key="k">value</data> token sequence.
func encodeDataElem(enc *xml.Encoder, key, value string) error {
	type dataElem struct {
		XMLName xml.Name `xml:"data"`
		Key     string   `xml:"key,attr"`
		Value   string   `xml:",chardata"`
	}
	return enc.EncodeElement(dataElem{Key: key, Value: value},
		xml.StartElement{Name: xml.Name{Local: "data"}})
}

// propKeyID returns the <key id="..."> value for a property-key name.
// We prefix with "p_" to avoid collision with the reserved "w" edge-weight key.
func propKeyID(name string) string { return "p_" + name }

// sortedKeys returns the map keys in a stable lexicographic order so
// the GraphML output is deterministic regardless of map-iteration order.
func sortedKeys(m map[string]propKeyDecl) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion-sort: key count is typically small (< 64 properties).
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

// ReadWithProps parses a GraphML document from r and returns an
// [lpg.Graph] with typed node properties derived from <key>
// declarations and <data> elements. Edge weights are read via the
// standard "weight" key. The second return value is the number of
// edges added.
func ReadWithProps(r io.Reader) (*lpg.Graph[string, int64], int, error) {
	defer metrics.Time("graph.io.graphml.ReadWithProps")()
	g, n, err := ReadWithPropsCtx(context.Background(), r)
	if err != nil {
		metrics.IncCounter("graph.io.graphml.ReadWithProps.errors", 1)
	}
	return g, n, err
}

// ReadWithPropsCtx is the context-aware variant of [ReadWithProps].
//
//nolint:gocyclo // GraphML typed-property read: key index + node props + edge decode + ctx tick
func ReadWithPropsCtx(ctx context.Context, r io.Reader) (*lpg.Graph[string, int64], int, error) {
	defer metrics.Time("graph.io.graphml.ReadWithPropsCtx")()
	dec := xml.NewDecoder(r)
	var doc docElement
	if err := dec.Decode(&doc); err != nil {
		metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
		return nil, 0, fmt.Errorf("graphml: parse: %w", err)
	}

	// Index <key> declarations by id so data elements can be resolved
	// in O(1). Keep track of the weight key separately.
	keyIndex := make(map[string]keyDecl, len(doc.Keys))
	for _, k := range doc.Keys {
		keyIndex[k.ID] = k
	}
	weightKey := findWeightKey(doc.Keys)

	if len(doc.Graphs) == 0 {
		return lpg.New[string, int64](adjlist.Config{Directed: true}), 0, nil
	}
	gr := doc.Graphs[0]
	cfg := adjlist.Config{Directed: gr.EdgeDefault != "undirected"}
	g := lpg.New[string, int64](cfg)

	// Add nodes and decode their properties.
	for _, n := range gr.Nodes {
		if err := g.AddNode(n.ID); err != nil {
			metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
			return g, 0, fmt.Errorf("graphml: AddNode(%q): %w", n.ID, err)
		}
		// Re-use the nodeElement's inline Data field when the XML
		// decoder populates it (the docElement struct unmarshals
		// <data> children of <node> via xml:"data").
		for _, d := range n.Data {
			decl, ok := keyIndex[d.Key]
			if !ok {
				continue
			}
			// Skip the edge-weight key if it appears on a node.
			if d.Key == weightKey {
				continue
			}
			pv, err := deserialisePropertyValue(decl.AttrType, d.Value)
			if err != nil {
				metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
				return g, 0, fmt.Errorf("graphml: node %q key %q: %w", n.ID, decl.AttrName, err)
			}
			if err := g.SetNodeProperty(n.ID, decl.AttrName, pv); err != nil {
				metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
				return g, 0, fmt.Errorf("graphml: SetNodeProperty(%q, %q): %w", n.ID, decl.AttrName, err)
			}
		}
	}

	// Add edges.
	added := 0
	for _, e := range gr.Edges {
		if added&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
				return g, added, err
			}
		}
		var w int64
		for _, d := range e.Data {
			if d.Key == weightKey && weightKey != "" {
				v, err := strconv.ParseInt(d.Value, 10, 64)
				if err != nil {
					metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
					return g, added, fmt.Errorf("graphml: edge (%q,%q) weight %q: %w", e.Source, e.Target, d.Value, err)
				}
				w = v
			}
		}
		if err := g.AdjList().AddEdge(e.Source, e.Target, w); err != nil {
			metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
			return g, added, fmt.Errorf("graphml: AddEdge(%q, %q): %w", e.Source, e.Target, err)
		}
		added++
	}
	return g, added, nil
}

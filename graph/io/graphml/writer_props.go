package graphml

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrInvalidXMLChar is returned by [WriteWithProps] and [WriteWithPropsCtx]
// when a node id, property key, or string property value contains a
// character that XML 1.0 cannot represent — the C0 control characters
// other than tab, newline, and carriage return (U+0000–U+0008, U+000B,
// U+000C, U+000E–U+001F), or an invalid UTF-8 byte.
//
// Go's [encoding/xml] silently substitutes such characters with the
// Unicode replacement character (U+FFFD), which destroys the original
// bytes irreversibly. To preserve integrity the writer fails fast with
// this typed error rather than emitting corrupted output; callers needing
// to carry arbitrary bytes through a property should use the JSONL
// encoder, which escapes control characters losslessly, or store the
// payload as a [lpg.BytesValue].
var ErrInvalidXMLChar = errors.New("graphml: value contains a character not representable in XML 1.0")

// xmlValidChar reports whether r is in the XML 1.0 Char production, the
// exact set [encoding/xml] would emit verbatim. Characters outside it are
// replaced with U+FFFD by the encoder, so the writer rejects them.
func xmlValidChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

// validateXMLText returns [ErrInvalidXMLChar] if s contains any rune that
// XML 1.0 cannot represent, or an invalid UTF-8 byte (which the encoder
// would also coerce to U+FFFD). A literal, well-formed U+FFFD already
// present in s is permitted — it is valid XML and round-trips faithfully.
func validateXMLText(s string) error {
	for i, r := range s {
		if r == utf8.RuneError {
			// Ranging yields RuneError both for a genuine U+FFFD (3 bytes)
			// and for an invalid UTF-8 byte (1 byte). Only the latter is
			// lossy, so distinguish them by the decoded width.
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				return fmt.Errorf("%w: invalid UTF-8 at byte offset %d", ErrInvalidXMLChar, i)
			}
			continue
		}
		if !xmlValidChar(r) {
			return fmt.Errorf("%w: U+%04X at byte offset %d", ErrInvalidXMLChar, r, i)
		}
	}
	return nil
}

// graphMLAttrType maps a [lpg.PropertyKind] to the GraphML attr.type
// string used in a <key> declaration. The standard GraphML types are
// used for Int64, Float64, Bool, and String. For Time, Bytes, and List
// we use non-standard but self-describing type tags ("time", "bytes",
// "list") so that the round-trip restores the original PropertyKind
// rather than silently degrading to PropString.
func graphMLAttrType(k lpg.PropertyKind) string {
	switch k {
	case lpg.PropInt64:
		return "long"
	case lpg.PropFloat64:
		return "double"
	case lpg.PropBool:
		return "boolean"
	case lpg.PropString:
		return "string"
	case lpg.PropTime:
		return "time"
	case lpg.PropBytes:
		return "bytes"
	case lpg.PropList:
		return "list"
	default:
		return "string"
	}
}

// serialisePropertyValue serialises v to its GraphML text representation.
// For PropTime: RFC3339Nano. For PropBytes: base64 standard encoding.
// For PropFloat64: 'g' format, with the non-finite values rendered as the
// XML-Schema xs:double lexical forms "INF", "-INF", and "NaN" so that
// conformant GraphML parsers (NetworkX, the Java stack) accept them — Go's
// native "+Inf"/"-Inf" text is rejected by xs:double. Other kinds fall
// back to fmt.Sprintf.
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
		switch {
		case math.IsNaN(f):
			return "NaN"
		case math.IsInf(f, 1):
			return "INF"
		case math.IsInf(f, -1):
			return "-INF"
		}
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
	case lpg.PropList:
		elems, _ := v.List()
		// Encode as a JSON array of [kindString, encodedValueString] pairs.
		// This mirrors the JSONL wire format so the two encoders stay in sync.
		pairs := make([][2]string, len(elems))
		for i, elem := range elems {
			pairs[i] = [2]string{graphMLAttrType(elem.Kind()), serialisePropertyValue(elem)}
		}
		b, _ := json.Marshal(pairs)
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// deserialisePropertyValue converts the text s to a [lpg.PropertyValue]
// according to the GraphML attr.type in attrType.
//
// Standard types ("long", "int", "double", "float", "boolean", "string") are
// handled as per the GraphML specification. Non-standard type tags written by
// [graphMLAttrType] ("time", "bytes", "list") are decoded back to their
// original PropertyKind so that round-trips are lossless.
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
	case "time":
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("graphml: parse time %q: %w", s, err)
		}
		return lpg.TimeValue(t), nil
	case "bytes":
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("graphml: parse bytes %q: %w", s, err)
		}
		return lpg.BytesValue(b), nil
	case "list":
		// The value is a JSON array of [attrTypeString, encodedValueString] pairs.
		var pairs [][2]string
		if err := json.Unmarshal([]byte(s), &pairs); err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("graphml: parse list %q: %w", s, err)
		}
		elems := make([]lpg.PropertyValue, len(pairs))
		for i, p := range pairs {
			elem, err := deserialisePropertyValue(p[0], p[1])
			if err != nil {
				return lpg.PropertyValue{}, fmt.Errorf("graphml: list[%d]: %w", i, err)
			}
			elems[i] = elem
		}
		return lpg.ListValue(elems), nil
	default:
		// "string" or any unknown type — treat as plain string.
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
//
// Tombstoned nodes — those removed via [lpg.Graph.RemoveNode] — are
// excluded, together with every incident edge and every <key>
// declaration only their properties would justify, so an export→import
// round trip never resurrects deleted data.
func WriteWithProps(w io.Writer, g *lpg.Graph[string, int64]) error {
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
	defer metrics.Time("graph.io.graphml.WriteWithProps").Stop()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
		return err
	}

	a := g.AdjList()

	// The Mapper retains tombstoned (logically removed) ids for NodeID
	// stability; exporting them would resurrect deleted nodes on
	// re-import. Build the removed set once so every pass below skips
	// them — and their incident edges — with an O(1) lookup.
	var dead map[graph.NodeID]struct{}
	if ids := g.TombstonedIDs(); len(ids) > 0 {
		dead = make(map[graph.NodeID]struct{}, len(ids))
		for _, id := range ids {
			dead[id] = struct{}{}
		}
	}

	// First pass: collect all property keys and their GraphML types
	// by walking every node's property bag.
	// keyMeta maps property-key name → propKeyDecl (first-seen type wins).
	keyMeta := make(map[string]propKeyDecl)
	a.Mapper().Walk(func(id graph.NodeID, name string) bool {
		if _, gone := dead[id]; gone {
			return true
		}
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
		if err := validateXMLText(kn); err != nil {
			metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
			return fmt.Errorf("graphml: property key %q: %w", kn, err)
		}
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
	a.Mapper().Walk(func(id graph.NodeID, name string) bool {
		if _, gone := dead[id]; gone {
			return true
		}
		if encErr = validateXMLText(name); encErr != nil {
			encErr = fmt.Errorf("graphml: node id %q: %w", name, encErr)
			return false
		}
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
			sv := serialisePropertyValue(v)
			if encErr = validateXMLText(sv); encErr != nil {
				encErr = fmt.Errorf("graphml: node %q property %q: %w", name, kn, encErr)
				return false
			}
			encErr = encodeDataElem(enc, propKeyID(kn), sv)
			if encErr != nil {
				return false
			}
		}
		encErr = enc.EncodeToken(nodeStart.End())
		return encErr == nil
	})
	if encErr != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
		return encErr
	}

	if err := ctx.Err(); err != nil {
		metrics.IncCounter("graph.io.graphml.WriteWithPropsCtx.errors", 1)
		return err
	}

	// Emit <edge> elements using the same batched-name pattern as the
	// plain writer, skipping any edge incident to a tombstoned node.
	if err := encodeEdges(enc, a, uint64(a.MaxNodeID()), dead); err != nil {
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
	g, n, err := ReadWithPropsCtx(context.Background(), r)
	if err != nil {
		metrics.IncCounter("graph.io.graphml.ReadWithProps.errors", 1)
	}
	return g, n, err
}

// ReadWithPropsCtx is the context-aware variant of [ReadWithProps]. The
// input is capped at [DefaultMaxBytes]; use [ReadWithPropsCappedCtx] for
// an explicit ceiling.
//
// On any error — a parse error, context cancellation, or the
// [ErrInputTooLarge] cap — the returned graph is nil; the import is
// all-or-nothing at the in-memory level, so a caller cannot accidentally
// commit a half-built graph. The typed error is returned unchanged; only
// the graph value is discarded.
func ReadWithPropsCtx(ctx context.Context, r io.Reader) (*lpg.Graph[string, int64], int, error) {
	return ReadWithPropsCappedCtx(ctx, r, DefaultMaxBytes)
}

// ReadWithPropsCappedCtx is [ReadWithPropsCtx] with an explicit
// input-size ceiling. When maxBytes > 0 the decoder fails with
// [ErrInputTooLarge] the moment consumption exceeds the limit, before
// the whole document is buffered; a value of zero or less disables the
// cap.
//
// On any error the returned graph is nil (see [ReadWithPropsCtx]); the
// import is all-or-nothing at the in-memory level.
//
//nolint:gocyclo // GraphML typed-property read: key index + node props + edge decode + ctx tick
func ReadWithPropsCappedCtx(ctx context.Context, r io.Reader, maxBytes int64) (*lpg.Graph[string, int64], int, error) {
	defer metrics.Time("graph.io.graphml.ReadWithProps").Stop()
	if maxBytes > 0 {
		r = newLimitReader(r, maxBytes)
	}
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
			return nil, 0, fmt.Errorf("graphml: AddNode(%q): %w", n.ID, err)
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
				return nil, 0, fmt.Errorf("graphml: node %q key %q: %w", n.ID, decl.AttrName, err)
			}
			if err := g.SetNodeProperty(n.ID, decl.AttrName, pv); err != nil {
				metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
				return nil, 0, fmt.Errorf("graphml: SetNodeProperty(%q, %q): %w", n.ID, decl.AttrName, err)
			}
		}
	}

	// Add edges.
	added := 0
	for _, e := range gr.Edges {
		if added&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
				return nil, added, err
			}
		}
		var w int64
		for _, d := range e.Data {
			if d.Key == weightKey && weightKey != "" {
				v, err := strconv.ParseInt(d.Value, 10, 64)
				if err != nil {
					metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
					return nil, added, fmt.Errorf("graphml: edge (%q,%q) weight %q: %w", e.Source, e.Target, d.Value, err)
				}
				w = v
			}
		}
		if err := g.AdjList().AddEdge(e.Source, e.Target, w); err != nil {
			metrics.IncCounter("graph.io.graphml.ReadWithPropsCtx.errors", 1)
			return nil, added, fmt.Errorf("graphml: AddEdge(%q, %q): %w", e.Source, e.Target, err)
		}
		added++
	}
	return g, added, nil
}

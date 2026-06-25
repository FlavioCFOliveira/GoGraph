package jsonl

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Write streams every node and edge of a to w as JSON Lines. Nodes
// come first, then edges, so that on-read every endpoint is known
// before its referencing edge.
func Write(w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
	n, err := WriteCtx(context.Background(), w, a)
	if err != nil {
		metrics.IncCounter("graph.io.jsonl.Write.errors", 1)
	}
	return n, err
}

// WriteCtx is the context-aware variant of [Write]. ctx.Err() is
// checked every 4096 records; on cancellation flushes the buffer
// and returns (recordsWritten, wrapped ctx.Err()).
//
//nolint:gocyclo // JSONL write: per-node and per-edge encode + ctx tick
func WriteCtx(ctx context.Context, w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
	defer metrics.Time("graph.io.jsonl.Write").Stop()
	bw := bufio.NewWriterSize(w, 64*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	written := 0

	maxID := uint64(a.MaxNodeID())
	// Pre-resolve every live name in one shard-batched pass so the
	// inner edge loop pays no per-node Mapper.Resolve cost.
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graph.NodeID, v string) bool {
		names[uint64(id)] = v
		live[uint64(id)] = true
		return true
	})
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		if err := enc.Encode(Record{Type: "node", ID: names[id]}); err != nil {
			metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
			return written, err
		}
		written++
	}
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		src := names[id]
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					_ = bw.Flush()
					metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
					return written, cerr
				}
			}
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			// A weightless source (Config.Weightless) carries no weights column,
			// so ws is nil; emit the zero weight, identical to a genuine 0.
			var weight int64
			if ws != nil {
				weight = ws[i]
			}
			if err := enc.Encode(Record{Type: "edge", Src: src, Dst: names[uint64(n)], Weight: weight}); err != nil {
				metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
				return written, err
			}
			written++
		}
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("graph.io.jsonl.WriteCtx.errors", 1)
		return written, err
	}
	return written, nil
}

// WriteWithProps streams the full contents of an [lpg.Graph] to w as
// JSON Lines. Output order is: all node records, then all edge
// records, then all property records. This ordering ensures that
// [ReadWithProps] can reconstruct the graph in a single pass.
//
// Tombstoned nodes — those removed via [lpg.Graph.RemoveNode] — are
// excluded, together with every edge and property record referencing
// them, so an export→import round trip never resurrects deleted data.
func WriteWithProps(w io.Writer, g *lpg.Graph[string, int64]) (int, error) {
	n, err := WriteWithPropsCtx(context.Background(), w, g)
	if err != nil {
		metrics.IncCounter("graph.io.jsonl.WriteWithProps.errors", 1)
	}
	return n, err
}

// WriteWithPropsCtx is the context-aware variant of [WriteWithProps].
// ctx.Err() is checked every 4096 records.
//
//nolint:gocyclo // JSONL write: node/edge/property phases + ctx tick
func WriteWithPropsCtx(ctx context.Context, w io.Writer, g *lpg.Graph[string, int64]) (int, error) {
	defer metrics.Time("graph.io.jsonl.WriteWithProps").Stop()
	bw := bufio.NewWriterSize(w, 64*1024)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	written := 0

	a := g.AdjList()
	maxID := uint64(a.MaxNodeID())

	// Pre-resolve live node names in one pass to avoid repeated
	// Mapper.Resolve calls in the hot loops.
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graph.NodeID, v string) bool {
		names[uint64(id)] = v
		live[uint64(id)] = true
		return true
	})
	// The Mapper retains tombstoned (logically removed) ids for NodeID
	// stability; exporting them would resurrect deleted nodes on
	// re-import. Clear them from the live set once so the node, edge,
	// and property phases below skip the node and every incident edge
	// at zero per-record cost.
	for _, id := range g.TombstonedIDs() {
		if uint64(id) < maxID {
			live[uint64(id)] = false
		}
	}

	// Phase 1: node records.
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		if err := enc.Encode(Record{Type: "node", ID: names[id]}); err != nil {
			metrics.IncCounter("graph.io.jsonl.WriteWithPropsCtx.errors", 1)
			return written, err
		}
		written++
	}

	// Phase 2: edge records.
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		src := names[id]
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					_ = bw.Flush()
					metrics.IncCounter("graph.io.jsonl.WriteWithPropsCtx.errors", 1)
					return written, cerr
				}
			}
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			// A weightless source (Config.Weightless) carries no weights column,
			// so ws is nil; emit the zero weight, identical to a genuine 0.
			var weight int64
			if ws != nil {
				weight = ws[i]
			}
			if err := enc.Encode(Record{Type: "edge", Src: src, Dst: names[uint64(n)], Weight: weight}); err != nil {
				metrics.IncCounter("graph.io.jsonl.WriteWithPropsCtx.errors", 1)
				return written, err
			}
			written++
		}
	}

	// Phase 3: property records.
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		nodeKey := names[id]
		props := g.NodeProperties(nodeKey)
		for propName, pv := range props {
			if written&0xFFF == 0 {
				if cerr := ctx.Err(); cerr != nil {
					_ = bw.Flush()
					metrics.IncCounter("graph.io.jsonl.WriteWithPropsCtx.errors", 1)
					return written, cerr
				}
			}
			kindStr, valStr := encodePropertyValue(pv)
			if err := enc.Encode(Record{
				Type:  "property",
				ID:    nodeKey,
				Key:   propName,
				Value: valStr,
				Kind:  kindStr,
			}); err != nil {
				metrics.IncCounter("graph.io.jsonl.WriteWithPropsCtx.errors", 1)
				return written, err
			}
			written++
		}
	}

	if err := bw.Flush(); err != nil {
		metrics.IncCounter("graph.io.jsonl.WriteWithPropsCtx.errors", 1)
		return written, err
	}
	return written, nil
}

// encodePropertyValue serialises pv into a (kind, value) pair of strings
// suitable for embedding in a [Record]. The inverse operation is
// [decodePropertyValue] in reader.go.
//
// For PropList the value field is a JSON array where each element is a
// two-element JSON array [kindString, encodedValueString]. Nested lists
// are encoded recursively.
//
// PropFloat64 is written with Go's strconv 'g' format, so the non-finite
// values appear as the strings "+Inf", "-Inf", and "NaN". Because the
// value is always carried as a JSON string (never a bare JSON number),
// this sidesteps JSON's prohibition on non-finite numerics and round-trips
// losslessly within GoGraph ([decodePropertyValue] parses them back via
// strconv.ParseFloat). External consumers expecting a numeric float should
// be aware these three values are non-numeric string tokens; see
// docs/io.md.
func encodePropertyValue(pv lpg.PropertyValue) (kind, value string) {
	switch pv.Kind() {
	case lpg.PropString:
		s, _ := pv.String()
		return "string", s
	case lpg.PropInt64:
		i, _ := pv.Int64()
		return "int64", strconv.FormatInt(i, 10)
	case lpg.PropFloat64:
		f, _ := pv.Float64()
		return "float64", strconv.FormatFloat(f, 'g', -1, 64)
	case lpg.PropBool:
		b, _ := pv.Bool()
		return "bool", strconv.FormatBool(b)
	case lpg.PropTime:
		t, _ := pv.Time()
		// Format in the value's own location so a non-UTC offset round-trips
		// faithfully (instant AND offset survive), instead of silently
		// normalising to UTC (#1769). RFC3339Nano renders the offset or "Z".
		return "time", t.Format(time.RFC3339Nano)
	case lpg.PropBytes:
		b, _ := pv.Bytes()
		return "bytes", base64.StdEncoding.EncodeToString(b)
	case lpg.PropList:
		elems, _ := pv.List()
		// Encode as a JSON array of [kindString, encodedValueString] pairs.
		pairs := make([][2]string, len(elems))
		for i, elem := range elems {
			k, v := encodePropertyValue(elem)
			pairs[i] = [2]string{k, v}
		}
		b, _ := json.Marshal(pairs)
		return "list", string(b)
	default:
		// Zero or unknown kind: emit as empty string; readers will fail
		// gracefully on the unknown kind tag.
		return "unknown", ""
	}
}

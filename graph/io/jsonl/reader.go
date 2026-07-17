// Package jsonl reads and writes graphs in newline-delimited JSON
// (NDJSON / JSON Lines) format.
//
// Records have one of three shapes:
//
//	{"type": "node", "id": "alice"}
//	{"type": "edge", "src": "alice", "dst": "bob", "weight": 7}
//	{"type": "property", "id": "alice", "key": "age", "value": "30", "kind": "int64"}
//
// The 'weight' field is optional and defaults to 0.
// Property records are produced and consumed by [WriteWithProps] / [ReadWithProps].
package jsonl

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrUnknownType is returned by [ReadInto], [ReadIntoCtx],
// [ReadWithProps], and [ReadWithPropsCtx] when a record's "type"
// field contains a value that is not one of the recognised literals
// ("node", "edge", "property").
var ErrUnknownType = errors.New("jsonl: unknown record type")

// DefaultMaxBytes is the default ceiling, in bytes, on the amount of
// input the default read entry points will consume before failing with
// [ErrInputTooLarge]. It guards against memory exhaustion from untrusted
// files (a crafted multi-gigabyte line, for example). The capped
// variants ([ReadIntoCappedCtx], [ReadWithPropsCappedCtx]) accept an
// explicit ceiling; a value of zero or less disables the cap.
//
// # Why 1 GiB (8x the CSV/GraphML default)
//
// Unlike a struct-per-element DOM parser, this reader is streaming: it
// consumes one line at a time under a per-line 16 MiB token cap and folds each
// record straight into the graph, so peak transient memory tracks the output
// graph plus one record — there is no small-input-to-huge-heap amplification
// class here (contrast the GraphML full-DOM amplification that motivated its
// 128 MiB default). The higher default therefore bounds only the total input
// bytes, not a multiple of them, and does not expose an OOM. Callers parsing
// untrusted input on a memory-constrained host may still pass a lower explicit
// cap to the capped variants.
const DefaultMaxBytes int64 = 1 << 30 // 1 GiB

// ErrInputTooLarge is returned by the read functions when the input
// stream exceeds the configured byte ceiling. The reader fails as soon
// as the limit is crossed, before the offending line is fully buffered,
// so allocation stays bounded.
var ErrInputTooLarge = errors.New("jsonl: input exceeds maximum size")

// ErrLineTooLong is returned when a single JSON-Lines record exceeds the
// 16 MiB scanner token limit. It wraps the underlying [bufio.ErrTooLong]
// so callers can distinguish a single over-long line (this sentinel) from
// the aggregate byte cap ([ErrInputTooLarge]) with [errors.Is]; the
// condition is bounded and non-silent.
var ErrLineTooLong = errors.New("jsonl: line exceeds maximum size")

// translateScanErr maps the [bufio.Scanner] "token too long" error to the
// typed [ErrLineTooLong] sentinel so callers can match it with
// [errors.Is]. Every other error — including the byte-cap
// [ErrInputTooLarge] surfaced through limitReader, and [io.EOF] — passes
// through unchanged.
func translateScanErr(err error) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("%w: %w", ErrLineTooLong, err)
	}
	return err
}

// Record is the wire shape of a JSON-Lines event.
//
// ID, Src, Dst, and Key are pointers so the reader can tell an ABSENT field
// (nil) from one that is PRESENT but empty (non-nil, ""). The engine accepts
// an empty-string node id, edge endpoint, and property key, so those must
// round-trip; a genuinely missing field is a malformed record and is still
// rejected (rmp #2043). A plain string with omitempty conflated the two: the
// empty value was dropped on write and re-read as absent, and the record was
// wrongly rejected on re-import.
type Record struct {
	Type   string  `json:"type"`
	ID     *string `json:"id,omitempty"`     // node key — used by "node" and "property" records
	Src    *string `json:"src,omitempty"`    // edge source
	Dst    *string `json:"dst,omitempty"`    // edge destination
	Weight int64   `json:"weight,omitempty"` // edge weight (defaults to 0)
	Key    *string `json:"key,omitempty"`    // property key — used by "property" records
	Value  string  `json:"value,omitempty"`  // property value serialised as a string
	Kind   string  `json:"kind,omitempty"`   // property kind: "string","int64","float64","bool","time","bytes"

	// Labels carries the node's labels on "node" records (#1793). Omitted
	// when empty so label-less files stay byte-identical and older files
	// (which never wrote the field) decode to a nil slice — back-compatible.
	Labels []string `json:"labels,omitempty"`
}

// ReadInto consumes a JSON Lines stream from r and builds an
// adjacency list. Node records pre-intern endpoints; edge records
// add the edge with optional weight.
func ReadInto(r io.Reader, cfg adjlist.Config) (*adjlist.AdjList[string, int64], int, error) {
	a, n, err := ReadIntoCtx(context.Background(), r, cfg)
	if err != nil {
		metrics.IncCounter("graph.io.jsonl.ReadInto.errors", 1)
	}
	return a, n, err
}

// ReadIntoCtx is the context-aware variant of [ReadInto]. ctx.Err()
// is checked every 4096 rows. The input is capped at [DefaultMaxBytes];
// use [ReadIntoCappedCtx] for an explicit ceiling.
//
// On any error — a parse error, context cancellation, or the
// [ErrInputTooLarge] cap — the returned graph is nil; the import is
// all-or-nothing at the in-memory level, so a caller cannot accidentally
// commit a half-built graph. The typed error is returned unchanged; only
// the graph value is discarded.
func ReadIntoCtx(ctx context.Context, r io.Reader, cfg adjlist.Config) (*adjlist.AdjList[string, int64], int, error) {
	return ReadIntoCappedCtx(ctx, r, cfg, DefaultMaxBytes)
}

// ReadIntoCappedCtx is [ReadIntoCtx] with an explicit input-size
// ceiling. When maxBytes > 0 the reader fails with [ErrInputTooLarge]
// the moment consumption exceeds the limit, before the offending line
// is fully buffered; a value of zero or less disables the cap.
//
//nolint:gocyclo // JSONL decode + per-row parse + node/edge dispatch + ctx tick
func ReadIntoCappedCtx(ctx context.Context, r io.Reader, cfg adjlist.Config, maxBytes int64) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.jsonl.ReadInto").Stop()
	if maxBytes > 0 {
		r = newLimitReader(r, maxBytes)
	}
	a := adjlist.New[string, int64](cfg)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	rows := 0
	for sc.Scan() {
		if rows&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, err
			}
		}
		rows++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// A failed unmarshal on the final token can be the symptom
			// of a truncated read: when the byte cap trips mid-line the
			// scanner delivers the partial bytes, then surfaces the cap
			// error via sc.Err(). Prefer the cap error so the cause is
			// reported faithfully rather than a misleading JSON error.
			if scErr := sc.Err(); errors.Is(scErr, ErrInputTooLarge) {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, scErr
			}
			metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("jsonl row %d: %w", rows, err)
		}
		switch rec.Type {
		case "node":
			// A nil ID is an absent field (malformed); a non-nil pointer to ""
			// is a present empty id, which the engine accepts (rmp #2043).
			if rec.ID == nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: node missing id", rows)
			}
			if err := a.AddNode(*rec.ID); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: AddNode: %w", rows, err)
			}
		case "edge":
			if rec.Src == nil || rec.Dst == nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: edge missing src/dst", rows)
			}
			if err := a.AddEdge(*rec.Src, *rec.Dst, rec.Weight); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: AddEdge: %w", rows, err)
			}
		default:
			metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("jsonl row %d: %w %q", rows, ErrUnknownType, rec.Type)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
		return nil, rows, translateScanErr(err)
	}
	return a, rows, nil
}

// ReadWithProps consumes a JSON Lines stream from r and builds a
// labelled property graph. It handles "node", "edge", and "property"
// record types. Property records must appear after the "node" record
// for the referenced ID.
func ReadWithProps(r io.Reader, cfg adjlist.Config) (*lpg.Graph[string, int64], int, error) {
	g, n, err := ReadWithPropsCtx(context.Background(), r, cfg)
	if err != nil {
		metrics.IncCounter("graph.io.jsonl.ReadWithProps.errors", 1)
	}
	return g, n, err
}

// ReadWithPropsCtx is the context-aware variant of [ReadWithProps].
// ctx.Err() is checked every 4096 rows. The input is capped at
// [DefaultMaxBytes]; use [ReadWithPropsCappedCtx] for an explicit
// ceiling.
//
// On any error — a parse error, context cancellation, or the
// [ErrInputTooLarge] cap — the returned graph is nil; the import is
// all-or-nothing at the in-memory level, so a caller cannot accidentally
// commit a half-built graph. The typed error is returned unchanged; only
// the graph value is discarded.
func ReadWithPropsCtx(ctx context.Context, r io.Reader, cfg adjlist.Config) (*lpg.Graph[string, int64], int, error) {
	return ReadWithPropsCappedCtx(ctx, r, cfg, DefaultMaxBytes)
}

// ReadWithPropsCappedCtx is [ReadWithPropsCtx] with an explicit
// input-size ceiling. When maxBytes > 0 the reader fails with
// [ErrInputTooLarge] the moment consumption exceeds the limit, before
// the offending line is fully buffered; a value of zero or less
// disables the cap.
//
//nolint:gocyclo // JSONL decode + node/edge/property dispatch + kind decode + ctx tick
func ReadWithPropsCappedCtx(ctx context.Context, r io.Reader, cfg adjlist.Config, maxBytes int64) (*lpg.Graph[string, int64], int, error) {
	defer metrics.Time("graph.io.jsonl.ReadWithProps").Stop()
	if maxBytes > 0 {
		r = newLimitReader(r, maxBytes)
	}
	g := lpg.New[string, int64](cfg)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	rows := 0
	for sc.Scan() {
		if rows&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, err
			}
		}
		rows++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// A failed unmarshal on the final token can be the symptom
			// of a truncated read: when the byte cap trips mid-line the
			// scanner delivers the partial bytes, then surfaces the cap
			// error via sc.Err(). Prefer the cap error so the cause is
			// reported faithfully rather than a misleading JSON error.
			if scErr := sc.Err(); errors.Is(scErr, ErrInputTooLarge) {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, scErr
			}
			metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
			return nil, rows, fmt.Errorf("jsonl row %d: %w", rows, err)
		}
		switch rec.Type {
		case "node":
			// A nil ID is an absent field (malformed); a non-nil pointer to ""
			// is a present empty id, which the engine accepts (rmp #2043).
			if rec.ID == nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: node missing id", rows)
			}
			if err := g.AddNode(*rec.ID); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: AddNode: %w", rows, err)
			}
			// Restore node labels (#1793). Absent on label-less / older files.
			for _, lbl := range rec.Labels {
				if err := g.SetNodeLabel(*rec.ID, lbl); err != nil {
					metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
					return nil, rows, fmt.Errorf("jsonl row %d: SetNodeLabel(%q): %w", rows, lbl, err)
				}
			}
		case "edge":
			if rec.Src == nil || rec.Dst == nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: edge missing src/dst", rows)
			}
			if err := g.AddEdge(*rec.Src, *rec.Dst, rec.Weight); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: AddEdge: %w", rows, err)
			}
		case "property":
			// nil id or key is an absent field (malformed); a present empty id
			// or key is accepted and round-trips (rmp #2043).
			if rec.ID == nil || rec.Key == nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: property missing id/key", rows)
			}
			pv, err := decodePropertyValue(rec.Kind, rec.Value)
			if err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: property %q: %w", rows, *rec.Key, err)
			}
			if err := g.SetNodeProperty(*rec.ID, *rec.Key, pv); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: SetNodeProperty: %w", rows, err)
			}
		default:
			metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
			return nil, rows, fmt.Errorf("jsonl row %d: %w %q", rows, ErrUnknownType, rec.Type)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		metrics.IncCounter("graph.io.jsonl.ReadWithPropsCtx.errors", 1)
		return nil, rows, translateScanErr(err)
	}
	return g, rows, nil
}

// decodePropertyValue reconstructs a [lpg.PropertyValue] from its
// wire kind tag and value string. The encoding mirrors [encodePropertyValue].
// maxListNestingDepth bounds how deeply a "list" property value may nest. A
// list is a JSON array of [kind, encodedValue] pairs, and a nested list
// re-encodes its children as an escaped string, so a legitimate document nests
// only a handful of levels. The recursive decode previously had no explicit
// guard: depth was bounded only implicitly by the ~2x-per-level JSON-escaping
// growth against the input byte cap — fragile, and it would relax if the byte
// cap were raised. This explicit ceiling fail-stops a crafted payload before it
// can exhaust the stack. 64 is far above any legitimate nesting.
const maxListNestingDepth = 64

// ErrListTooDeep is returned when a "list" property value nests deeper than
// [maxListNestingDepth].
var ErrListTooDeep = errors.New("jsonl: list property nesting too deep")

func decodePropertyValue(kind, value string) (lpg.PropertyValue, error) {
	return decodePropertyValueDepth(kind, value, 0)
}

func decodePropertyValueDepth(kind, value string, depth int) (lpg.PropertyValue, error) {
	switch kind {
	case "string":
		return lpg.StringValue(value), nil
	case "int64":
		i, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("int64: %w", err)
		}
		return lpg.Int64Value(i), nil
	case "float64":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("float64: %w", err)
		}
		return lpg.Float64Value(f), nil
	case "bool":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("bool: %w", err)
		}
		return lpg.BoolValue(b), nil
	case "time":
		t, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("time: %w", err)
		}
		return lpg.TimeValue(t), nil
	case "bytes":
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("bytes: %w", err)
		}
		return lpg.BytesValue(b), nil
	case "list":
		if depth >= maxListNestingDepth {
			return lpg.PropertyValue{}, ErrListTooDeep
		}
		// The value is a JSON array of [kindString, encodedValueString] pairs.
		var pairs [][2]string
		if err := json.Unmarshal([]byte(value), &pairs); err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("list: %w", err)
		}
		elems := make([]lpg.PropertyValue, len(pairs))
		for i, p := range pairs {
			elem, err := decodePropertyValueDepth(p[0], p[1], depth+1)
			if err != nil {
				return lpg.PropertyValue{}, fmt.Errorf("list[%d]: %w", i, err)
			}
			elems[i] = elem
		}
		return lpg.ListValue(elems), nil
	default:
		return lpg.PropertyValue{}, fmt.Errorf("unknown property kind %q", kind)
	}
}

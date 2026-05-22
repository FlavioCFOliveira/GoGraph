// Package jsonl reads and writes graphs in newline-delimited JSON
// (NDJSON / JSON Lines) format.
//
// Records have one of two shapes:
//
//	{"type": "node", "id": "alice"}
//	{"type": "edge", "src": "alice", "dst": "bob", "weight": 7}
//
// The 'weight' field is optional and defaults to 0.
package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gograph/graph/adjlist"
	"gograph/internal/metrics"
)

// Record is the wire shape of a JSON-Lines event.
type Record struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Src    string `json:"src,omitempty"`
	Dst    string `json:"dst,omitempty"`
	Weight int64  `json:"weight,omitempty"`
}

// ReadInto consumes a JSON Lines stream from r and builds an
// adjacency list. Node records pre-intern endpoints; edge records
// add the edge with optional weight.
func ReadInto(r io.Reader, cfg adjlist.Config) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.jsonl.ReadInto")()
	a, n, err := ReadIntoCtx(context.Background(), r, cfg)
	if err != nil {
		metrics.IncCounter("graph.io.jsonl.ReadInto.errors", 1)
	}
	return a, n, err
}

// ReadIntoCtx is the context-aware variant of [ReadInto]. ctx.Err()
// is checked every 4096 rows; on cancellation returns
// (partialAdj, rowsConsumed, wrapped ctx.Err()).
//
//nolint:gocyclo // JSONL decode + per-row parse + node/edge dispatch + ctx tick
func ReadIntoCtx(ctx context.Context, r io.Reader, cfg adjlist.Config) (*adjlist.AdjList[string, int64], int, error) {
	defer metrics.Time("graph.io.jsonl.ReadIntoCtx")()
	a := adjlist.New[string, int64](cfg)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	rows := 0
	for sc.Scan() {
		if rows&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return a, rows, err
			}
		}
		rows++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("jsonl row %d: %w", rows, err)
		}
		switch rec.Type {
		case "node":
			if rec.ID == "" {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: node missing id", rows)
			}
			if err := a.AddNode(rec.ID); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: AddNode: %w", rows, err)
			}
		case "edge":
			if rec.Src == "" || rec.Dst == "" {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: edge missing src/dst", rows)
			}
			if err := a.AddEdge(rec.Src, rec.Dst, rec.Weight); err != nil {
				metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
				return nil, rows, fmt.Errorf("jsonl row %d: AddEdge: %w", rows, err)
			}
		default:
			metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
			return nil, rows, fmt.Errorf("jsonl row %d: unknown type %q", rows, rec.Type)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		metrics.IncCounter("graph.io.jsonl.ReadIntoCtx.errors", 1)
		return nil, rows, err
	}
	return a, rows, nil
}

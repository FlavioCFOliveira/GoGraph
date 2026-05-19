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
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gograph/graph/adjlist"
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
	a := adjlist.New[string, int64](cfg)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	rows := 0
	for sc.Scan() {
		rows++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, rows, fmt.Errorf("jsonl row %d: %w", rows, err)
		}
		switch rec.Type {
		case "node":
			if rec.ID == "" {
				return nil, rows, fmt.Errorf("jsonl row %d: node missing id", rows)
			}
			a.AddNode(rec.ID)
		case "edge":
			if rec.Src == "" || rec.Dst == "" {
				return nil, rows, fmt.Errorf("jsonl row %d: edge missing src/dst", rows)
			}
			a.AddEdge(rec.Src, rec.Dst, rec.Weight)
		default:
			return nil, rows, fmt.Errorf("jsonl row %d: unknown type %q", rows, rec.Type)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, rows, err
	}
	return a, rows, nil
}

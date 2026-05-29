package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"gograph/cypher/parser"
	"gograph/cypher/sema"
)

// maxQueryBodyBytes bounds the POST /query request body.
const maxQueryBodyBytes = 1 << 20 // 1 MiB

// writeKeywordRE mirrors the engine's own writing-clause heuristic
// (CREATE/MERGE/SET/REMOVE/DELETE/DETACH as standalone words). A query
// that matches is treated as a writer and serialised under the exclusive
// lock; a non-match is a reader and runs under the shared lock. The match
// is intentionally permissive: a false positive merely over-serialises a
// read, which is safe, whereas a missed write would not be — so the set
// must cover every writing clause the engine recognises.
var writeKeywordRE = regexp.MustCompile(`(?i)\b(CREATE|MERGE|SET|REMOVE|DELETE|DETACH)\b`)

func isWriteQuery(q string) bool { return writeKeywordRE.MatchString(q) }

// queryRequest is the POST /query request body. Params is optional.
type queryRequest struct {
	Query  string         `json:"query"`
	Params map[string]any `json:"params"`
}

// queryResponse is the POST /query success body: the output column names
// and one object per row keyed by those columns.
type queryResponse struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

// handleQuery runs an arbitrary Cypher statement (read or write) through
// Engine.RunAny. Writers take the exclusive lock, readers the shared lock;
// the lock is held across RunAny, the full drain, and Close, because a
// write holds the store's single-writer mutex until the result is closed
// and a reader's plan-building must not observe a concurrent write.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds the 1 MiB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing required field \"query\"")
		return
	}

	if isWriteQuery(req.Query) {
		s.mu.Lock()
		defer s.mu.Unlock()
	} else {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}

	res, err := s.ds.engine.RunAny(r.Context(), req.Query, req.Params)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	defer func() { _ = res.Close() }() // runs before the lock is released

	cols := res.Columns()
	rows := make([]map[string]any, 0, 16)
	for res.Next() {
		rec := res.Record()
		if len(rec) == 0 {
			continue // synthetic empty row from a write-only statement
		}
		row := make(map[string]any, len(cols))
		for _, c := range cols {
			row[c] = jsonValue(rec[c])
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		writeQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, queryResponse{Columns: cols, Rows: rows})
}

// writeQueryError maps a Cypher engine error to the right HTTP status.
// Parse-phase rejections (a genuine syntax error, or valid syntax for a
// feature the engine does not support) are client faults reported as 400.
// Post-parse semantic faults (undefined variable, type error, bad
// parameter type) are unprocessable, reported as 422. Cancellation or a
// deadline is reported as unavailable (503); anything else is a runtime
// failure (500).
func writeQueryError(w http.ResponseWriter, err error) {
	var (
		pe  *parser.ParseError   // syntax error
		pse *parser.SemaError    // valid syntax, unsupported feature
		se  *sema.SemanticError  // undefined variable, type/scope error
		pte *sema.ParamTypeError // parameter has the wrong type
	)
	switch {
	case errors.As(err, &pe), errors.As(err, &pse):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.As(err, &se), errors.As(err, &pte):
		writeError(w, http.StatusUnprocessableEntity, "semantic", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "runtime", err.Error())
	}
}

// handleSeed loads the deterministic fixture under the exclusive lock (it
// is a bulk write). It is idempotent: the response reports whether data
// was actually written.
func (s *Server) handleSeed(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seeded, err := seedFixture(s.ds.txnStore)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "seed failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seeded": seeded, "status": "ok"})
}

// statsResponse is the GET /stats body: node counts by type label and
// edge counts by relationship type.
type statsResponse struct {
	Nodes map[string]int64 `json:"nodes"`
	Edges map[string]int64 `json:"edges"`
}

// handleStats counts nodes per type label and edges per relationship type.
// The whole sweep runs under a single shared lock so the counts form one
// consistent snapshot and no concurrent write can mutate the structures
// the count queries read.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes := make(map[string]int64, len(nodeTypeLabels))
	for _, lbl := range nodeTypeLabels {
		n, err := s.countOne(ctx, "MATCH (n:"+lbl+") RETURN count(n) AS n")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "runtime", "stats: "+err.Error())
			return
		}
		nodes[lbl] = n
	}
	edges := make(map[string]int64, len(relTypes))
	for _, rt := range relTypes {
		n, err := s.countOne(ctx, "MATCH ()-[r:"+rt+"]->() RETURN count(r) AS n")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "runtime", "stats: "+err.Error())
			return
		}
		edges[rt] = n
	}
	writeJSON(w, http.StatusOK, statsResponse{Nodes: nodes, Edges: edges})
}

// countOne runs a single count query and returns the integer in column
// "n". The result is drained and closed before returning. The caller holds
// the shared lock for the duration of the surrounding sweep.
func (s *Server) countOne(ctx context.Context, query string) (int64, error) {
	res, err := s.ds.engine.RunAny(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()

	var n int64
	for res.Next() {
		rec := res.Record()
		v, ok := rec["n"]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case int64:
			n = x
		case int:
			n = int64(x)
		default:
			if iv, ok := jsonValue(v).(int64); ok {
				n = iv
			}
		}
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

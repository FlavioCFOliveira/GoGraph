package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
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
// Engine.RunAny. Writers take the exclusive hold, readers the shared hold;
// the hold is kept across RunAny, the full drain, and Result.Close, because
// a write holds the store's single-writer mutex until the result is closed
// and a reader's plan-building must not observe a concurrent write. A
// request that arrives after the store has been closed is rejected with
// 503 rather than admitted onto the closing WAL.
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

	write := isWriteQuery(req.Query)
	release, err := s.ds.acquire(write)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	defer release()

	// Time the whole engine call (plan-build, drain, Close) as one volatile
	// latency sample. Recording it is lock-free and never affects the
	// deterministic response body.
	start := time.Now()
	defer func() { s.ds.metrics.recordQuery(time.Since(start), write) }()

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
// parameter type) are unprocessable, reported as 422. A constraint
// violation (a write that would break a UNIQUE or NOT NULL invariant) is a
// conflict with existing state, reported as 409. Cancellation or a deadline
// is reported as unavailable (503); anything else is a runtime failure (500).
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
	case errors.Is(err, exec.ErrConstraintViolation):
		// A UNIQUE/NOT NULL violation: the write conflicts with existing data
		// and was rolled back atomically. 409 Conflict is the semantic match.
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "runtime", err.Error())
	}
}

// seedRequest is the optional POST /seed request body. An empty body (or no
// body at all) loads the small deterministic fixture; supplying any of the
// scale fields grows it with a seeded synthetic layer (see synth.go). The
// fields mirror the -scale-* command-line flags.
type seedRequest struct {
	ScaleComponents int   `json:"scale_components"`
	ScaleTasks      int   `json:"scale_tasks"`
	ScaleDevelopers int   `json:"scale_developers"`
	ScaleSeed       int64 `json:"scale_seed"`
}

// scale converts the request fields into a synthScale.
func (r seedRequest) scale() synthScale {
	return synthScale{
		components: r.ScaleComponents,
		tasks:      r.ScaleTasks,
		developers: r.ScaleDevelopers,
		seed:       r.ScaleSeed,
	}
}

// handleSeed loads the fixture under the exclusive hold (it is a bulk write).
// The request body may carry optional scale fields; an empty body loads the
// small deterministic fixture. It is idempotent: the response reports whether
// data was actually written and, for a scaled seed, the synthetic dimensions
// requested. A seed that arrives after the store has been closed is rejected
// with 503.
func (s *Server) handleSeed(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	var req seedRequest
	// An empty body is valid and means "default fixture". Only a malformed
	// non-empty body is an error.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds the 1 MiB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	scale := req.scale()
	if err := scale.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	release, err := s.ds.acquire(true)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	defer release()

	start := time.Now()
	// seed loads the fixture and, on the first seed, declares the schema so its
	// backing indexes are backfilled from the just-seeded graph (see
	// dataStore.seed). Both run under the exclusive hold already held here.
	seeded, err := s.ds.seed(r.Context(), scale)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "seed failed: "+err.Error())
		return
	}
	if seeded {
		s.ds.metrics.recordSeed(time.Since(start))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seeded":           seeded,
		"status":           "ok",
		"scale_components": scale.components,
		"scale_tasks":      scale.tasks,
		"scale_developers": scale.developers,
	})
}

// statsResponse is the GET /stats body. Nodes and Edges are deterministic
// FACTS — counts reproducible for a fixed seed and scale — kept apart from the
// volatile Telemetry, which varies per run and per machine. A consumer that
// only cares about the graph shape can read Nodes/Edges and ignore Telemetry,
// the JSON analogue of the "# " telemetry convention the non-server examples
// use. Telemetry is a pointer so a test asserting on facts can still confirm
// the block is present without depending on any value inside it.
type statsResponse struct {
	Nodes     map[string]int64 `json:"nodes"`
	Edges     map[string]int64 `json:"edges"`
	Telemetry *telemetryBody   `json:"telemetry"`
}

// handleStats counts nodes per type label and edges per relationship type, then
// attaches volatile telemetry (live heap, request counters, recent latencies).
// The whole count sweep runs under a single shared hold so the counts form one
// consistent snapshot and no concurrent write can mutate the structures the
// count queries read. A request that arrives after the store has been closed is
// rejected with 503.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()
	release, err := s.ds.acquire(false)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}

	nodes := make(map[string]int64, len(nodeTypeLabels))
	edges := make(map[string]int64, len(relTypes))
	var totalElements int64
	for _, lbl := range nodeTypeLabels {
		n, err := s.countOne(ctx, "MATCH (n:"+lbl+") RETURN count(n) AS n")
		if err != nil {
			release()
			writeError(w, http.StatusInternalServerError, "runtime", "stats: "+err.Error())
			return
		}
		nodes[lbl] = n
		totalElements += n
	}
	for _, rt := range relTypes {
		n, err := s.countOne(ctx, "MATCH ()-[r:"+rt+"]->() RETURN count(r) AS n")
		if err != nil {
			release()
			writeError(w, http.StatusInternalServerError, "runtime", "stats: "+err.Error())
			return
		}
		edges[rt] = n
		totalElements += n
	}
	// Release the read hold before reading memory: snapshotTelemetry forces a
	// GC, which has no need of the store hold and must not extend it.
	release()
	s.ds.metrics.recordStats(time.Since(start))
	tel := s.ds.metrics.snapshotTelemetry(totalElements)
	writeJSON(w, http.StatusOK, statsResponse{Nodes: nodes, Edges: edges, Telemetry: &tel})
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

// schemaResponse is the GET /schema body: the graph's live schema surfaced by
// the built-in db.* introspection procedures. Every field is a deterministic
// FACT for a fixed seed — the label/relationship-type/property-key sets in use
// and the declared indexes and constraints — so a test can pin them. The sets
// are sorted so the JSON is stable regardless of the procedures' internal
// enumeration order.
type schemaResponse struct {
	Labels            []string           `json:"labels"`
	RelationshipTypes []string           `json:"relationship_types"`
	PropertyKeys      []string           `json:"property_keys"`
	Indexes           []schemaIndex      `json:"indexes"`
	Constraints       []schemaConstraint `json:"constraints"`
}

// schemaIndex is one row of CALL db.indexes(). A UNIQUE constraint's backing
// index is reported here under its reserved __uniq__<Label>.<prop> name
// alongside any plain CREATE INDEX; both are shown faithfully.
type schemaIndex struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// schemaConstraint is one row of CALL db.constraints().
type schemaConstraint struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Label    string `json:"label"`
	Property string `json:"property"`
}

// handleSchema returns the live schema via the db.* introspection procedures:
// db.labels(), db.relationshipTypes(), db.propertyKeys(), db.indexes() and
// db.constraints(). The whole sweep runs under one shared hold so the five
// procedure calls observe one consistent snapshot of the schema. A request
// that arrives after the store has been closed is rejected with 503.
func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	release, err := s.ds.acquire(false)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	defer release()

	labels, err := s.procColumn(ctx, "CALL db.labels()", "label")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "schema: "+err.Error())
		return
	}
	relationshipTypes, err := s.procColumn(ctx, "CALL db.relationshipTypes()", "relationshipType")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "schema: "+err.Error())
		return
	}
	propertyKeys, err := s.procColumn(ctx, "CALL db.propertyKeys()", "propertyKey")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "schema: "+err.Error())
		return
	}
	indexes, err := s.procIndexes(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "schema: "+err.Error())
		return
	}
	constraints, err := s.procConstraints(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime", "schema: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, schemaResponse{
		Labels:            labels,
		RelationshipTypes: relationshipTypes,
		PropertyKeys:      propertyKeys,
		Indexes:           indexes,
		Constraints:       constraints,
	})
}

// procColumn runs a single-column introspection procedure and returns the
// sorted, distinct string values of column col. The caller holds the shared
// hold for the surrounding sweep.
func (s *Server) procColumn(ctx context.Context, query, col string) ([]string, error) {
	res, err := s.ds.engine.RunAny(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	out := make([]string, 0, 8)
	for res.Next() {
		if v, ok := res.Record()[col]; ok {
			out = append(out, cellString(v))
		}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// procIndexes runs CALL db.indexes() and returns its rows sorted by name.
func (s *Server) procIndexes(ctx context.Context) ([]schemaIndex, error) {
	res, err := s.ds.engine.RunAny(ctx, "CALL db.indexes()", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	out := make([]schemaIndex, 0, 4)
	for res.Next() {
		rec := res.Record()
		out = append(out, schemaIndex{Name: cellString(rec["name"]), Type: cellString(rec["type"])})
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// procConstraints runs CALL db.constraints() and returns its rows sorted by name.
func (s *Server) procConstraints(ctx context.Context) ([]schemaConstraint, error) {
	res, err := s.ds.engine.RunAny(ctx, "CALL db.constraints()", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	out := make([]schemaConstraint, 0, 4)
	for res.Next() {
		rec := res.Record()
		out = append(out, schemaConstraint{
			Name:     cellString(rec["name"]),
			Type:     cellString(rec["type"]),
			Label:    cellString(rec["label"]),
			Property: cellString(rec["property"]),
		})
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// cellString renders a Cypher record cell (an expr.Value) as its plain string,
// or "" when it is null or not a string. The db.* procedures yield string
// columns, so this recovers the underlying Go string.
func cellString(v any) string {
	if sv, ok := jsonValue(v).(string); ok {
		return sv
	}
	return ""
}

// explainRequest is the POST /explain request body. Params is optional.
type explainRequest struct {
	Query  string         `json:"query"`
	Params map[string]any `json:"params"`
}

// explainResponse is the POST /explain success body: the rendered physical
// plan and whether it uses an index seek. UsesIndexSeek is a deterministic
// FACT (it depends only on the query and the declared schema, not on data or
// timing); Plan is human-readable diagnostic detail.
type explainResponse struct {
	Plan          string `json:"plan"`
	UsesIndexSeek bool   `json:"uses_index_seek"`
}

// handleExplain returns the physical plan the engine would choose for a query,
// without executing it. It reports whether the plan uses a NodeByIndexSeek —
// the operator that proves the equality predicate is served by the constraint
// or index declared at startup rather than a full NodeByLabelScan. The plan
// reflects current index availability, so the sweep runs under a shared hold
// to exclude a concurrent write mutating the structures the planner reads. A
// request that arrives after the store has been closed is rejected with 503.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	var req explainRequest
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
	bound, err := cypher.BindParams(req.Params)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid params: "+err.Error())
		return
	}

	release, err := s.ds.acquire(false)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	defer release()

	plan, err := s.ds.engine.Explain(req.Query, bound)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, explainResponse{
		Plan:          plan,
		UsesIndexSeek: strings.Contains(plan, "NodeByIndexSeek"),
	})
}

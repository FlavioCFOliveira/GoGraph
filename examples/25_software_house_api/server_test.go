package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer opens a fresh store in a temp dir, optionally seeds it,
// mounts the handlers on an httptest server, and returns the server plus a
// keep-alive-free client. Cleanup closes everything so the goleak check in
// TestMain stays green.
func newTestServer(t *testing.T, seed bool) (*httptest.Server, *http.Client) {
	t.Helper()
	ds, err := openStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if seed {
		if _, err := seedFixture(ds.txnStore); err != nil {
			t.Fatalf("seedFixture: %v", err)
		}
	}
	srv := newServer(ds, "127.0.0.1:0")
	ts := httptest.NewServer(srv.http.Handler)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(func() {
		ts.Close()
		client.CloseIdleConnections()
		_ = ds.Close()
	})
	return ts, client
}

// do issues a request and returns the status code and raw body.
func do(t *testing.T, c *http.Client, method, url, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestSeedEndpointIdempotent(t *testing.T) {
	ts, c := newTestServer(t, false)

	st, raw := do(t, c, http.MethodPost, ts.URL+"/seed", "")
	if st != http.StatusOK {
		t.Fatalf("first seed status = %d, want 200 (%s)", st, raw)
	}
	var first map[string]any
	mustJSON(t, raw, &first)
	if first["seeded"] != true {
		t.Errorf("first seed: seeded = %v, want true", first["seeded"])
	}

	st, raw = do(t, c, http.MethodPost, ts.URL+"/seed", "")
	if st != http.StatusOK {
		t.Fatalf("second seed status = %d, want 200", st)
	}
	var second map[string]any
	mustJSON(t, raw, &second)
	if second["seeded"] != false {
		t.Errorf("second seed: seeded = %v, want false (idempotent)", second["seeded"])
	}
}

func TestStatsEndpoint(t *testing.T) {
	ts, c := newTestServer(t, true)
	st, raw := do(t, c, http.MethodGet, ts.URL+"/stats", "")
	if st != http.StatusOK {
		t.Fatalf("stats status = %d, want 200 (%s)", st, raw)
	}
	var sr statsResponse
	mustJSON(t, raw, &sr)
	wantNodes := map[string]int64{
		typeRepository: 1, typeModule: 5, typeComponent: 12, typeTask: 14,
		typeSprint: 2, typeWorkflowState: 4, typeDeveloper: 6, typeTeam: 2,
	}
	for k, want := range wantNodes {
		if sr.Nodes[k] != want {
			t.Errorf("stats nodes[%s] = %d, want %d", k, sr.Nodes[k], want)
		}
	}
	wantEdges := map[string]int64{
		relContains: 17, relDependsOn: 17, relSubtaskOf: 2, relNext: 4,
		relBlocks: 3, relHasState: 14, relInSprint: 14, relMemberOf: 6,
		relAssignedTo: 16, relTouches: 13,
	}
	for k, want := range wantEdges {
		if sr.Edges[k] != want {
			t.Errorf("stats edges[%s] = %d, want %d", k, sr.Edges[k], want)
		}
	}
}

func TestQueryRead(t *testing.T) {
	ts, c := newTestServer(t, true)
	body := `{"query":"MATCH (d:Developer {key:$dev})-[a:ASSIGNED_TO]->(t:Task) WHERE t.status <> 'done' AND a.state <> 'done' RETURN t.key AS task ORDER BY task","params":{"dev":"dev:alice"}}`
	st, raw := do(t, c, http.MethodPost, ts.URL+"/query", body)
	if st != http.StatusOK {
		t.Fatalf("query status = %d, want 200 (%s)", st, raw)
	}
	var qr queryResponse
	mustJSON(t, raw, &qr)
	if len(qr.Columns) != 1 || qr.Columns[0] != "task" {
		t.Errorf("columns = %v, want [task]", qr.Columns)
	}
	if len(qr.Rows) != 2 { // alice has WS-9 and WS-13 open
		t.Errorf("rows = %d, want 2 (%s)", len(qr.Rows), raw)
	}
}

func TestQueryWritePersists(t *testing.T) {
	ts, c := newTestServer(t, true)

	// Developer count before the write.
	before := developerCount(t, c, ts.URL)

	body := `{"query":"CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'}) RETURN d.key AS key"}`
	st, raw := do(t, c, http.MethodPost, ts.URL+"/query", body)
	if st != http.StatusOK {
		t.Fatalf("write status = %d, want 200 (%s)", st, raw)
	}
	var qr queryResponse
	mustJSON(t, raw, &qr)
	if len(qr.Rows) != 1 || qr.Rows[0]["key"] != "dev:zoe" {
		t.Errorf("write rows = %v, want one row key=dev:zoe", qr.Rows)
	}

	if after := developerCount(t, c, ts.URL); after != before+1 {
		t.Errorf("Developer count after write = %d, want %d", after, before+1)
	}
}

func TestQueryErrorStatuses(t *testing.T) {
	ts, c := newTestServer(t, true)
	cases := []struct {
		name, body string
		want       int
		wantKind   string
	}{
		{"syntax error", `{"query":"MATCH (("}`, http.StatusBadRequest, "bad_request"},
		{"unsupported feature", `{"query":"FOREACH (x IN [1] | CREATE (:T))"}`, http.StatusBadRequest, "bad_request"},
		{"unknown function", `{"query":"RETURN nope(1)"}`, http.StatusUnprocessableEntity, "semantic"},
		{"malformed json", `{"query":`, http.StatusBadRequest, "bad_request"},
		{"missing query", `{"params":{}}`, http.StatusBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, raw := do(t, c, http.MethodPost, ts.URL+"/query", tc.body)
			if st != tc.want {
				t.Fatalf("status = %d, want %d (%s)", st, tc.want, raw)
			}
			var eb errorBody
			mustJSON(t, raw, &eb)
			if eb.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", eb.Kind, tc.wantKind)
			}
			if eb.Error == "" {
				t.Error("error message is empty")
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts, c := newTestServer(t, true)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/query"},
		{http.MethodPost, "/stats"},
		{http.MethodPut, "/seed"},
		{http.MethodDelete, "/query"},
	}
	for _, tc := range cases {
		st, _ := do(t, c, tc.method, ts.URL+tc.path, "")
		if st != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want 405", tc.method, tc.path, st)
		}
	}
}

func TestOversizedBody(t *testing.T) {
	ts, c := newTestServer(t, true)
	big := `{"query":"` + strings.Repeat("x", maxQueryBodyBytes+1) + `"}`
	st, _ := do(t, c, http.MethodPost, ts.URL+"/query", big)
	if st != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", st)
	}
}

// developerCount returns the Developer node count via GET /stats.
func developerCount(t *testing.T, c *http.Client, baseURL string) int64 {
	t.Helper()
	st, raw := do(t, c, http.MethodGet, baseURL+"/stats", "")
	if st != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", st)
	}
	var sr statsResponse
	mustJSON(t, raw, &sr)
	return sr.Nodes[typeDeveloper]
}

func mustJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode JSON %q: %v", raw, err)
	}
}

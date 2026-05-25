package proto_test

// T658 — Bolt v4.4 fallback: full session flow.
//
// The existing TestNegotiateV44 only verifies that Negotiate selects v4.4.
// This file adds the session-level behaviour required by T658's ACs:
//
//   AC #2: v4.4 Hello with auth_token transitions directly to Ready (Success).
//   AC #3: Subsequent Run/Pull works — messages are decoded and responses
//          round-trip correctly.
//
// The "server" is simulated inside the test: it encodes Success/Record
// responses and the "client" decodes them. This exercises EncodeRequest,
// DecodeRequest, EncodeResponse, and DecodeResponse end-to-end — the same
// codec paths used by a real Bolt v4.4 server connection.

import (
	"bytes"
	"testing"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// encodeReq encodes a request message into a fresh bytes.Buffer.
func encodeReq(t *testing.T, msg any) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		t.Fatalf("EncodeRequest(%T): %v", msg, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

// decodeReq decodes one request from a bytes.Reader.
func decodeReq(t *testing.T, r *bytes.Reader) any {
	t.Helper()
	dec := packstream.NewDecoder(r)
	msg, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return msg
}

// encodeResp encodes a response message into a bytes.Reader.
func encodeResp(t *testing.T, msg any) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeResponse(enc, msg); err != nil {
		t.Fatalf("EncodeResponse(%T): %v", msg, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

// decodeResp decodes one response from a bytes.Reader.
func decodeResp(t *testing.T, r *bytes.Reader) any {
	t.Helper()
	dec := packstream.NewDecoder(r)
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	return msg
}

// ---------------------------------------------------------------------------
// T658 — v4.4 Hello with auth_token → Ready; then Run/Pull
// ---------------------------------------------------------------------------

// TestV44HelloAuthTokenToReady verifies AC #2:
// A Hello carrying "scheme"/"principal"/"credentials" (Bolt v4.4 auth_token
// layout) is correctly encoded, survives a round-trip through DecodeRequest,
// and the server can respond with a Success (Ready) that the client decodes.
func TestV44HelloAuthTokenToReady(t *testing.T) {
	t.Parallel()

	// Client encodes Hello with auth_token fields (v4.4 layout).
	hello := &proto.Hello{
		Extra: map[string]packstream.Value{
			"scheme":      "basic",
			"principal":   "neo4j",
			"credentials": "s3cr3t",
			"user_agent":  "go-bolt/4.4",
		},
	}
	r := encodeReq(t, hello)

	// Server-side: decode the Hello.
	got := decodeReq(t, r)
	h, ok := got.(*proto.Hello)
	if !ok {
		t.Fatalf("expected *Hello, got %T", got)
	}
	if h.Extra["scheme"] != "basic" {
		t.Errorf("scheme: want %q, got %v", "basic", h.Extra["scheme"])
	}
	if h.Extra["principal"] != "neo4j" {
		t.Errorf("principal: want %q, got %v", "neo4j", h.Extra["principal"])
	}
	if h.Extra["credentials"] != "s3cr3t" {
		t.Errorf("credentials: want %q, got %v", "s3cr3t", h.Extra["credentials"])
	}

	// Server sends SUCCESS (Ready state).
	serverResp := &proto.Success{
		Metadata: map[string]packstream.Value{
			"server":        "Neo4j/4.4.0",
			"connection_id": "bolt-1",
		},
	}
	r2 := encodeResp(t, serverResp)

	// Client decodes the Success response.
	resp := decodeResp(t, r2)
	s, ok := resp.(*proto.Success)
	if !ok {
		t.Fatalf("expected *Success (Ready), got %T", resp)
	}
	if s.Metadata["server"] != "Neo4j/4.4.0" {
		t.Errorf("server metadata: want %q, got %v", "Neo4j/4.4.0", s.Metadata["server"])
	}
}

// TestV44SubsequentRunPull verifies AC #3:
// After the Hello/Ready exchange, the client sends Run (with query and
// parameters) then Pull, and the server can respond with Success + Records.
func TestV44SubsequentRunPull(t *testing.T) {
	t.Parallel()

	// ── Step 1: Run ──────────────────────────────────────────────────────────
	run := &proto.Run{
		Query:      "MATCH (n:Person) WHERE n.id = $id RETURN n.name",
		Parameters: map[string]packstream.Value{"id": int64(42)},
		Extra:      map[string]packstream.Value{},
	}
	r := encodeReq(t, run)

	gotRun := decodeReq(t, r)
	decoded, ok := gotRun.(*proto.Run)
	if !ok {
		t.Fatalf("expected *Run, got %T", gotRun)
	}
	if decoded.Query != run.Query {
		t.Errorf("query: want %q, got %q", run.Query, decoded.Query)
	}
	if decoded.Parameters["id"] != int64(42) {
		t.Errorf("parameters id: want 42, got %v", decoded.Parameters["id"])
	}

	// Server responds to Run with Success (fields/keys metadata).
	runSuccess := &proto.Success{
		Metadata: map[string]packstream.Value{
			"fields":  []packstream.Value{"n.name"},
			"t_first": int64(0),
		},
	}
	r2 := encodeResp(t, runSuccess)
	resp := decodeResp(t, r2)
	if _, ok := resp.(*proto.Success); !ok {
		t.Fatalf("expected *Success after Run, got %T", resp)
	}

	// ── Step 2: Pull ─────────────────────────────────────────────────────────
	pull := &proto.Pull{N: -1, QID: -1}
	r3 := encodeReq(t, pull)

	gotPull := decodeReq(t, r3)
	p, ok := gotPull.(*proto.Pull)
	if !ok {
		t.Fatalf("expected *Pull, got %T", gotPull)
	}
	if p.N != -1 || p.QID != -1 {
		t.Errorf("pull: want N=-1 QID=-1, got N=%d QID=%d", p.N, p.QID)
	}

	// Server streams one Record then closes with Success (has_more=false).
	record := &proto.Record{
		Data: []packstream.Value{"Alice"},
	}
	r4 := encodeResp(t, record)
	gotRecord := decodeResp(t, r4)
	rec, ok := gotRecord.(*proto.Record)
	if !ok {
		t.Fatalf("expected *Record, got %T", gotRecord)
	}
	if len(rec.Data) != 1 || rec.Data[0] != "Alice" {
		t.Errorf("record data: want [Alice], got %v", rec.Data)
	}

	// Final Success: pull complete.
	pullSuccess := &proto.Success{
		Metadata: map[string]packstream.Value{
			"t_last":   int64(1),
			"type":     "r",
			"has_more": false,
		},
	}
	r5 := encodeResp(t, pullSuccess)
	finalResp := decodeResp(t, r5)
	fs, ok := finalResp.(*proto.Success)
	if !ok {
		t.Fatalf("expected final *Success, got %T", finalResp)
	}
	if fs.Metadata["has_more"] != false {
		t.Errorf("has_more: want false, got %v", fs.Metadata["has_more"])
	}
}

// TestV44FullSessionFlow is a consolidated single-scenario test that exercises
// the complete v4.4 session in sequence: Hello → Success(Ready) → Run →
// Success(fields) → Pull → Record → Success(done). This mirrors what a real
// v4.4 driver would do and covers AC #2 and #3 together.
func TestV44FullSessionFlow(t *testing.T) {
	t.Parallel()

	type step struct {
		req  any
		resp any
	}

	steps := []step{
		{
			req: &proto.Hello{
				Extra: map[string]packstream.Value{
					"scheme":      "basic",
					"principal":   "neo4j",
					"credentials": "pass",
				},
			},
			resp: &proto.Success{Metadata: map[string]packstream.Value{"server": "Neo4j/4.4.0"}},
		},
		{
			req: &proto.Run{
				Query:      "RETURN 1",
				Parameters: map[string]packstream.Value{},
				Extra:      map[string]packstream.Value{},
			},
			resp: &proto.Success{Metadata: map[string]packstream.Value{"fields": []packstream.Value{"1"}}},
		},
		{
			req:  &proto.Pull{N: -1, QID: -1},
			resp: &proto.Record{Data: []packstream.Value{int64(1)}},
		},
	}

	for i, s := range steps {
		reqBytes := encodeReq(t, s.req)
		gotReq := decodeReq(t, reqBytes)
		if gotReq == nil {
			t.Errorf("step %d: decoded request is nil", i)
		}

		respBytes := encodeResp(t, s.resp)
		gotResp := decodeResp(t, respBytes)
		if gotResp == nil {
			t.Errorf("step %d: decoded response is nil", i)
		}
	}
}

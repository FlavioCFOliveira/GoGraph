package proto_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// encodeRequest encodes a request message to bytes via a bytes.Buffer.
func encodeRequest(t *testing.T, msg any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		t.Fatalf("EncodeRequest(%T): %v", msg, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

// decodeRequest decodes a request from raw bytes.
func decodeRequest(t *testing.T, raw []byte) any {
	t.Helper()
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return msg
}

// encodeResponse encodes a response message to bytes.
func encodeResponse(t *testing.T, msg any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeResponse(enc, msg); err != nil {
		t.Fatalf("EncodeResponse(%T): %v", msg, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

// decodeResponse decodes a response from raw bytes.
func decodeResponse(t *testing.T, raw []byte) any {
	t.Helper()
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	return msg
}

// ---------------------------------------------------------------------------
// Request message round-trips
// ---------------------------------------------------------------------------

func TestHelloRoundTrip(t *testing.T) {
	want := &proto.Hello{
		Extra: map[string]packstream.Value{
			"scheme":      "basic",
			"principal":   "neo4j",
			"credentials": "secret",
		},
	}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Hello)
	if !ok {
		t.Fatalf("expected *Hello, got %T", decodeRequest(t, raw))
	}
	if got.Extra["scheme"] != want.Extra["scheme"] {
		t.Errorf("scheme: want %v, got %v", want.Extra["scheme"], got.Extra["scheme"])
	}
}

func TestLogonRoundTrip(t *testing.T) {
	want := &proto.Logon{
		Auth: map[string]packstream.Value{
			"scheme":      "basic",
			"principal":   "neo4j",
			"credentials": "password",
		},
	}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Logon)
	if !ok {
		t.Fatalf("expected *Logon, got %T", decodeRequest(t, raw))
	}
	if len(got.Auth) != len(want.Auth) {
		t.Errorf("auth len: want %d, got %d", len(want.Auth), len(got.Auth))
	}
}

func TestLogoffRoundTrip(t *testing.T) {
	raw := encodeRequest(t, &proto.Logoff{})
	got := decodeRequest(t, raw)
	if _, ok := got.(*proto.Logoff); !ok {
		t.Fatalf("expected *Logoff, got %T", got)
	}
}

func TestGoodbyeRoundTrip(t *testing.T) {
	raw := encodeRequest(t, &proto.Goodbye{})
	got := decodeRequest(t, raw)
	if _, ok := got.(*proto.Goodbye); !ok {
		t.Fatalf("expected *Goodbye, got %T", got)
	}
}

func TestResetRoundTrip(t *testing.T) {
	raw := encodeRequest(t, &proto.Reset{})
	got := decodeRequest(t, raw)
	if _, ok := got.(*proto.Reset); !ok {
		t.Fatalf("expected *Reset, got %T", got)
	}
}

func TestRunRoundTrip(t *testing.T) {
	want := &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: map[string]packstream.Value{"id": int64(1)},
		Extra:      map[string]packstream.Value{"mode": "r"},
	}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Run)
	if !ok {
		t.Fatalf("expected *Run, got %T", decodeRequest(t, raw))
	}
	if got.Query != want.Query {
		t.Errorf("query: want %q, got %q", want.Query, got.Query)
	}
	if got.Parameters["id"] != int64(1) {
		t.Errorf("params id: got %v", got.Parameters["id"])
	}
}

func TestPullRoundTrip(t *testing.T) {
	want := &proto.Pull{N: 100, QID: -1}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Pull)
	if !ok {
		t.Fatalf("expected *Pull, got %T", decodeRequest(t, raw))
	}
	if got.N != want.N || got.QID != want.QID {
		t.Errorf("want N=%d QID=%d, got N=%d QID=%d", want.N, want.QID, got.N, got.QID)
	}
}

func TestDiscardRoundTrip(t *testing.T) {
	want := &proto.Discard{N: -1, QID: 42}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Discard)
	if !ok {
		t.Fatalf("expected *Discard, got %T", decodeRequest(t, raw))
	}
	if got.N != want.N || got.QID != want.QID {
		t.Errorf("want N=%d QID=%d, got N=%d QID=%d", want.N, want.QID, got.N, got.QID)
	}
}

func TestBeginRoundTrip(t *testing.T) {
	want := &proto.Begin{
		Extra: map[string]packstream.Value{"bookmarks": []packstream.Value{"bm1", "bm2"}},
	}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Begin)
	if !ok {
		t.Fatalf("expected *Begin, got %T", decodeRequest(t, raw))
	}
	if len(got.Extra) != len(want.Extra) {
		t.Errorf("extra len: want %d, got %d", len(want.Extra), len(got.Extra))
	}
}

func TestCommitRoundTrip(t *testing.T) {
	raw := encodeRequest(t, &proto.Commit{})
	if _, ok := decodeRequest(t, raw).(*proto.Commit); !ok {
		t.Fatal("expected *Commit")
	}
}

func TestRollbackRoundTrip(t *testing.T) {
	raw := encodeRequest(t, &proto.Rollback{})
	if _, ok := decodeRequest(t, raw).(*proto.Rollback); !ok {
		t.Fatal("expected *Rollback")
	}
}

func TestRouteRoundTrip(t *testing.T) {
	want := &proto.Route{
		Routing:   map[string]packstream.Value{"address": "localhost:7687"},
		Bookmarks: []packstream.Value{"bm1"},
		DB:        "neo4j",
	}
	raw := encodeRequest(t, want)
	got, ok := decodeRequest(t, raw).(*proto.Route)
	if !ok {
		t.Fatalf("expected *Route, got %T", decodeRequest(t, raw))
	}
	if got.Routing["address"] != want.Routing["address"] {
		t.Errorf("routing address: want %v, got %v", want.Routing["address"], got.Routing["address"])
	}
	if got.DB != want.DB {
		t.Errorf("DB: want %v, got %v", want.DB, got.DB)
	}
}

// ---------------------------------------------------------------------------
// Response message round-trips
// ---------------------------------------------------------------------------

func TestSuccessRoundTrip(t *testing.T) {
	want := &proto.Success{
		Metadata: map[string]packstream.Value{"server": "Neo4j/5.0"},
	}
	raw := encodeResponse(t, want)
	got, ok := decodeResponse(t, raw).(*proto.Success)
	if !ok {
		t.Fatalf("expected *Success, got %T", decodeResponse(t, raw))
	}
	if got.Metadata["server"] != want.Metadata["server"] {
		t.Errorf("server: want %v, got %v", want.Metadata["server"], got.Metadata["server"])
	}
}

func TestFailureRoundTrip(t *testing.T) {
	want := &proto.Failure{
		Code:    "Neo.ClientError.Statement.SyntaxError",
		Message: "Invalid syntax",
	}
	raw := encodeResponse(t, want)
	got, ok := decodeResponse(t, raw).(*proto.Failure)
	if !ok {
		t.Fatalf("expected *Failure, got %T", decodeResponse(t, raw))
	}
	if got.Code != want.Code || got.Message != want.Message {
		t.Errorf("got Code=%q Message=%q", got.Code, got.Message)
	}
}

func TestIgnoredRoundTrip(t *testing.T) {
	raw := encodeResponse(t, &proto.Ignored{})
	if _, ok := decodeResponse(t, raw).(*proto.Ignored); !ok {
		t.Fatal("expected *Ignored")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	want := &proto.Record{
		Data: []packstream.Value{int64(1), "hello", nil},
	}
	raw := encodeResponse(t, want)
	got, ok := decodeResponse(t, raw).(*proto.Record)
	if !ok {
		t.Fatalf("expected *Record, got %T", decodeResponse(t, raw))
	}
	if len(got.Data) != len(want.Data) {
		t.Errorf("data len: want %d, got %d", len(want.Data), len(got.Data))
	}
}

// ---------------------------------------------------------------------------
// Handshake tests
// ---------------------------------------------------------------------------

// buildHandshake constructs the 20-byte client handshake payload.
func buildHandshake(magic uint32, versions [][4]byte) []byte {
	buf := make([]byte, 20)
	binary.BigEndian.PutUint32(buf[:4], magic)
	for i, v := range versions {
		if i >= 4 {
			break
		}
		copy(buf[4+i*4:], v[:])
	}
	return buf
}

func TestNegotiateV54(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		// Client sends magic + v5.4 offer.
		// Bolt wire slot format (big-endian): [0x00, minor_range, minor, major].
		payload := buildHandshake(proto.Magic, [][4]byte{
			{0, 0, 4, 5}, // [pad, range=0, minor=4, major=5]
		})
		_, _ = client.Write(payload)
		// Read back 4-byte response.
		resp := make([]byte, 4)
		_, _ = io.ReadFull(client, resp)
	}()

	ctx := context.Background()
	v, err := proto.Negotiate(ctx, server)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if v.Major != 5 || v.Minor != 4 {
		t.Errorf("want v5.4, got v%d.%d", v.Major, v.Minor)
	}
}

func TestNegotiateV44(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		// Client offers v4.4 only.
		// Bolt wire slot format (big-endian): [0x00, minor_range, minor, major].
		payload := buildHandshake(proto.Magic, [][4]byte{
			{0, 0, 4, 4}, // [pad, range=0, minor=4, major=4]
		})
		_, _ = client.Write(payload)
		resp := make([]byte, 4)
		_, _ = io.ReadFull(client, resp)
	}()

	ctx := context.Background()
	v, err := proto.Negotiate(ctx, server)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if v.Major != 4 || v.Minor != 4 {
		t.Errorf("want v4.4, got v%d.%d", v.Major, v.Minor)
	}
}

func TestNegotiateNoMatch(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		// Client offers v3.0 only — not supported.
		// Bolt wire slot format (big-endian): [0x00, minor_range, minor, major].
		payload := buildHandshake(proto.Magic, [][4]byte{
			{0, 0, 0, 3}, // [pad, range=0, minor=0, major=3]
		})
		_, _ = client.Write(payload)
		resp := make([]byte, 4)
		_, _ = io.ReadFull(client, resp)
	}()

	ctx := context.Background()
	_, err := proto.Negotiate(ctx, server)
	if !errors.Is(err, proto.ErrNoCommonVersion) {
		t.Errorf("expected ErrNoCommonVersion, got %v", err)
	}
}

func TestNegotiateBadMagic(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		// Bolt wire slot format (big-endian): [0x00, minor_range, minor, major].
		payload := buildHandshake(0xDEADBEEF, [][4]byte{{0, 0, 4, 5}}) // v5.4
		_, _ = client.Write(payload)
		// The server closes without writing a response on bad magic — drain anyway.
		buf := make([]byte, 4)
		_, _ = io.ReadFull(client, buf)
	}()

	ctx := context.Background()
	_, err := proto.Negotiate(ctx, server)
	if !errors.Is(err, proto.ErrBadMagic) {
		t.Errorf("expected ErrBadMagic, got %v", err)
	}
}

func TestNegotiateContextCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Do not write anything from the client — the server must time out.
	errCh := make(chan error, 1)
	go func() {
		_, err := proto.Negotiate(ctx, server)
		errCh <- err
	}()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error due to context timeout, got nil")
	}
}

// ---------------------------------------------------------------------------
// Chunking tests
// ---------------------------------------------------------------------------

func TestChunkingRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		// An empty message (size 0) is intentionally NOT a round-trip case:
		// WriteMessage emits only a bare 00 00 sentinel for it, which is wire-
		// identical to a Bolt 4.1+ NOOP keep-alive. ReadMessage now silently
		// discards such a standalone 00 00 (see TestChunkingNOOPSilentlyDiscarded
		// and #1485), so an empty message does not survive the round trip — by
		// design, because real Bolt messages always carry at least a struct
		// header byte.
		{"one_byte", 1},
		{"max_single_chunk", 65534},
		{"exact_max_chunk", 65535},
		{"two_chunks", 65535 * 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := make([]byte, tc.size)
			for i := range msg {
				msg[i] = byte(i % 251)
			}

			var buf bytes.Buffer
			cw := proto.NewChunkedWriter(&buf)
			if err := cw.WriteMessage(msg); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}

			cr := proto.NewChunkedReader(&buf)
			got, err := cr.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
			if !bytes.Equal(got, msg) {
				t.Errorf("size mismatch: want %d bytes, got %d bytes", len(msg), len(got))
			}
		})
	}
}

func TestChunkingCorruptShortPayload(t *testing.T) {
	// Write a chunk header claiming 100 bytes but only provide 50.
	var buf bytes.Buffer
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], 100)
	buf.Write(hdr[:])
	buf.Write(make([]byte, 50)) // only 50 bytes, not 100

	cr := proto.NewChunkedReader(&buf)
	_, err := cr.ReadMessage()
	if err == nil {
		t.Fatal("expected error for truncated chunk, got nil")
	}
}

// TestChunkingNOOPSilentlyDiscarded pins the #1485 framing contract: a
// standalone 00 00 chunk (a NOOP keep-alive with no preceding payload) is
// skipped by ReadMessage rather than surfaced as a zero-length message.
func TestChunkingNOOPSilentlyDiscarded(t *testing.T) {
	// A bare NOOP followed by a real one-byte message. ReadMessage must skip
	// the NOOP and return the real message.
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x00}) // NOOP
	cw := proto.NewChunkedWriter(&buf)
	if err := cw.WriteMessage([]byte{0xAB}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	cr := proto.NewChunkedReader(&buf)
	got, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage after NOOP: %v", err)
	}
	if !bytes.Equal(got, []byte{0xAB}) {
		t.Errorf("ReadMessage after NOOP = %v; want [0xAB] (NOOP must be skipped, not returned)", got)
	}
}

// TestChunkingMultipleNOOPsThenMessage verifies that several consecutive NOOPs
// are all discarded before the next real message is returned.
func TestChunkingMultipleNOOPsThenMessage(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) // three NOOPs
	cw := proto.NewChunkedWriter(&buf)
	if err := cw.WriteMessage([]byte("hi")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	cr := proto.NewChunkedReader(&buf)
	got, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage after NOOPs: %v", err)
	}
	if !bytes.Equal(got, []byte("hi")) {
		t.Errorf("ReadMessage after NOOPs = %q; want %q", got, "hi")
	}
}

// TestChunkingNOOPOnlyThenEOF verifies that a NOOP with nothing after it is
// skipped and the reader then reports a clean io.EOF (not an empty message).
func TestChunkingNOOPOnlyThenEOF(t *testing.T) {
	cr := proto.NewChunkedReader(bytes.NewReader([]byte{0x00, 0x00}))
	_, err := cr.ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadMessage on NOOP-only stream = %v; want io.EOF", err)
	}
}

func TestChunkingCleanEOF(t *testing.T) {
	// Empty buffer → clean EOF before any message bytes.
	cr := proto.NewChunkedReader(bytes.NewReader(nil))
	_, err := cr.ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestChunkingContextDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// Set the connection deadline from context before reading.
		if deadline, ok := ctx.Deadline(); ok {
			_ = server.SetDeadline(deadline)
		}
		cr := proto.NewChunkedReader(server)
		_, err := cr.ReadMessage()
		errCh <- err
	}()

	// Do not write anything — server must time out.
	err := <-errCh
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestChunkingMultipleMessages(t *testing.T) {
	var buf bytes.Buffer
	cw := proto.NewChunkedWriter(&buf)

	msgs := [][]byte{
		[]byte("first message"),
		[]byte("second message"),
		make([]byte, 70000),
	}
	for i := range msgs[2] {
		msgs[2][i] = byte(i % 199)
	}

	for _, m := range msgs {
		if err := cw.WriteMessage(m); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	cr := proto.NewChunkedReader(&buf)
	for i, want := range msgs {
		got, err := cr.ReadMessage()
		if err != nil {
			t.Fatalf("message %d ReadMessage: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("message %d: length mismatch (want %d, got %d)", i, len(want), len(got))
		}
	}
}

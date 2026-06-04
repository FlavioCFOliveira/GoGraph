package server_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// boltTestClient wraps the low-level Bolt wire protocol for test convenience.
// It is NOT safe for concurrent use; each test must own its own instance.
type boltTestClient struct {
	conn net.Conn
	cr   *proto.ChunkedReader
	cw   *proto.ChunkedWriter
}

// newBoltTestClient dials addr and returns a client ready to negotiate.
// The caller must call c.close(t) when done.
func newBoltTestClient(t *testing.T, addr string) *boltTestClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("boltTestClient dial %s: %v", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return &boltTestClient{
		conn: conn,
		cr:   proto.NewChunkedReader(conn),
		cw:   proto.NewChunkedWriter(conn),
	}
}

// negotiate performs the 20-byte Bolt client handshake, offering version 5.0
// in slot 0 and zeros in slots 1–3. It returns the negotiated Version.
//
// Bolt wire slot format (big-endian): [0x00, minor_range, minor, major].
// Bolt wire response format (big-endian): [0x00, 0x00, minor, major].
func (c *boltTestClient) negotiate(t *testing.T) proto.Version {
	t.Helper()
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	// Slot 0: version 5.0 — [pad=0x00, minor_range=0, minor=0, major=5]
	buf[4] = 0 // pad
	buf[5] = 0 // minor_range
	buf[6] = 0 // minor
	buf[7] = 5 // major
	// Slots 1–3: zero (not offered)
	if _, err := c.conn.Write(buf[:]); err != nil {
		t.Fatalf("negotiate write: %v", err)
	}
	var resp [4]byte
	if _, err := io.ReadFull(c.conn, resp[:]); err != nil {
		t.Fatalf("negotiate read response: %v", err)
	}
	// Response format: [0x00, 0x00, minor, major].
	if resp[3] == 0 && resp[2] == 0 {
		t.Fatal("server rejected version negotiation")
	}
	return proto.Version{Major: resp[3], Minor: resp[2]}
}

// hello sends a HELLO with scheme="none" and reads back the SUCCESS.
func (c *boltTestClient) hello(t *testing.T) *proto.Success {
	t.Helper()
	c.sendRequest(t, &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "test",
			"credentials": "",
			"agent":       "test/1.0",
		},
	})
	return c.recvSuccess(t)
}

// run sends RUN with query and params and reads the SUCCESS.
func (c *boltTestClient) run(t *testing.T, query string, params map[string]any) *proto.Success {
	t.Helper()
	psParams := make(map[string]packstream.Value, len(params))
	for k, v := range params {
		psParams[k] = v
	}
	c.sendRequest(t, &proto.Run{
		Query:      query,
		Parameters: psParams,
		Extra:      map[string]interface{}{},
	})
	return c.recvSuccess(t)
}

// pullAll sends PULL {n:-1} and reads all RECORD messages followed by the
// final SUCCESS. It returns the records and the final SUCCESS.
func (c *boltTestClient) pullAll(t *testing.T) (records [][]packstream.Value, _ *proto.Success) {
	t.Helper()
	c.sendRequest(t, &proto.Pull{N: -1, QID: -1})
	for {
		msg := c.recvResponse(t)
		switch m := msg.(type) {
		case *proto.Record:
			records = append(records, m.Data)
		case *proto.Success:
			return records, m
		case *proto.Failure:
			t.Fatalf("pullAll received FAILURE: code=%s message=%s", m.Code, m.Message)
		default:
			t.Fatalf("pullAll: unexpected message type %T", msg)
		}
	}
}

// begin sends BEGIN and reads the SUCCESS.
func (c *boltTestClient) begin(t *testing.T) *proto.Success {
	t.Helper()
	c.sendRequest(t, &proto.Begin{Extra: map[string]interface{}{}})
	return c.recvSuccess(t)
}

// commit sends COMMIT and reads the SUCCESS.
func (c *boltTestClient) commit(t *testing.T) *proto.Success {
	t.Helper()
	c.sendRequest(t, &proto.Commit{})
	return c.recvSuccess(t)
}

// rollback sends ROLLBACK and reads the SUCCESS.
func (c *boltTestClient) rollback(t *testing.T) *proto.Success {
	t.Helper()
	c.sendRequest(t, &proto.Rollback{})
	return c.recvSuccess(t)
}

// route sends ROUTE and reads the SUCCESS.
func (c *boltTestClient) route(t *testing.T) *proto.Success {
	t.Helper()
	c.sendRequest(t, &proto.Route{
		Routing:   map[string]packstream.Value{},
		Bookmarks: nil,
		DB:        nil,
	})
	return c.recvSuccess(t)
}

// goodbye sends GOODBYE. No response is expected.
func (c *boltTestClient) goodbye(t *testing.T) {
	t.Helper()
	c.sendRequest(t, &proto.Goodbye{})
}

// close closes the underlying connection.
func (c *boltTestClient) close(t *testing.T) {
	t.Helper()
	if err := c.conn.Close(); err != nil {
		t.Logf("boltTestClient close: %v", err)
	}
}

// recvFailure reads a response and asserts it is a FAILURE, returning it.
func (c *boltTestClient) recvFailure(t *testing.T) *proto.Failure {
	t.Helper()
	msg := c.recvResponse(t)
	f, ok := msg.(*proto.Failure)
	if !ok {
		t.Fatalf("expected *proto.Failure, got %T", msg)
	}
	return f
}

// ─────────────────────────────────────────────────────────────────────────────
// Low-level helpers
// ─────────────────────────────────────────────────────────────────────────────

// sendRequest encodes msg as a PackStream request and writes it as one chunked
// Bolt message.
func (c *boltTestClient) sendRequest(t *testing.T, msg any) {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		t.Fatalf("sendRequest encode %T: %v", msg, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("sendRequest flush %T: %v", msg, err)
	}
	if err := c.cw.WriteMessage(buf.Bytes()); err != nil {
		t.Fatalf("sendRequest write %T: %v", msg, err)
	}
}

// recvResponse reads one raw chunked message and decodes it as a Bolt
// response (Success, Failure, Ignored, or Record).
func (c *boltTestClient) recvResponse(t *testing.T) any {
	t.Helper()
	raw, err := c.cr.ReadMessage()
	if err != nil {
		t.Fatalf("recvResponse read: %v", err)
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		t.Fatalf("recvResponse decode: %v", err)
	}
	return msg
}

// recvSuccess reads the next response and asserts it is a SUCCESS.
func (c *boltTestClient) recvSuccess(t *testing.T) *proto.Success {
	t.Helper()
	msg := c.recvResponse(t)
	s, ok := msg.(*proto.Success)
	if !ok {
		if f, isFail := msg.(*proto.Failure); isFail {
			t.Fatalf("expected *proto.Success, got FAILURE: code=%s message=%s", f.Code, f.Message)
		}
		t.Fatalf("expected *proto.Success, got %T", msg)
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────────────
// startTestServer helper
// ─────────────────────────────────────────────────────────────────────────────

// startTestServer starts a Server on a random port with the given options.
// It registers a t.Cleanup that cancels the server context and drains the
// Serve goroutine. Goroutine leak checking is handled by goleak.Find in
// TestMain after all servers have been shut down.
//
//nolint:gocritic // hugeParam: test helper takes Options by value to mirror the public NewServer signature; not a hot path.
func startTestServer(t *testing.T, opts server.Options) string {
	t.Helper()
	eng := newEngine(t)
	if opts.ConnTimeout == 0 {
		opts.ConnTimeout = 5 * time.Second
	}
	// Test servers run without credentials by default. The production server
	// is secure-by-default and refuses a nil Auth handler, so opt in here with
	// the explicit NoAuthHandler{} value unless the caller supplied a real
	// handler (e.g. the panic-boundary test).
	if opts.Auth == nil {
		opts.Auth = server.NoAuthHandler{}
	}
	srv, err := server.NewServer(eng, opts)
	if err != nil {
		t.Fatalf("startTestServer NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startTestServer listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, ln)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("startTestServer: Serve goroutine did not exit in cleanup")
		}
	})

	// Give the server a brief moment to start accepting.
	time.Sleep(10 * time.Millisecond)
	return addr
}

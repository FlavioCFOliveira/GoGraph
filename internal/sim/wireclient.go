package sim

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// WireClient speaks the REAL Bolt v5 wire protocol over a [SimConn]: the 20-byte
// version handshake, then chunked PackStream request/response messages encoded
// and decoded with the genuine [github.com/FlavioCFOliveira/GoGraph/bolt/proto]
// and [github.com/FlavioCFOliveira/GoGraph/bolt/packstream] codecs (it does NOT
// reimplement the wire format). It drives well-formed requests for the honest,
// overload, and slow-consumer actors and decodes RECORD/SUCCESS/FAILURE/IGNORED
// responses.
//
// # Lock-step determinism
//
// In single-connection use the client writes one request and blocks reading the
// server's complete terminal response (SUCCESS or FAILURE, after any RECORDs).
// Because exactly one logical exchange is in flight and the SimConn buffer holds
// it whole, the byte stream — and therefore the decoded response — is a pure
// function of the request, so a given seed replays the op stream and the
// responses identically.
//
// # Concurrency contract
//
// A WireClient is NOT safe for concurrent use; the Bolt protocol is itself
// single-flight per connection (one request, then its response). The concurrent
// harness gives each goroutine its own WireClient on its own SimConn.
type WireClient struct {
	conn *SimConn
	cr   *proto.ChunkedReader
	cw   *proto.ChunkedWriter
	clk  clock.Clock
	ver  proto.Version
}

// NewWireClient wraps conn with chunked reader/writer framing. clk is retained
// for deadline-bearing operations; conn and clk must be non-nil.
func NewWireClient(conn *SimConn, clk clock.Clock) *WireClient {
	return &WireClient{
		conn: conn,
		cr:   proto.NewChunkedReader(conn),
		cw:   proto.NewChunkedWriter(conn),
		clk:  clk,
	}
}

// Handshake performs the 20-byte Bolt client handshake, offering versions 5.6
// down to 5.0 across the four slots, and records the negotiated version. It
// returns an error if the server rejects negotiation (responds with 0.0) or an
// I/O error occurs.
func (c *WireClient) Handshake(_ context.Context) (proto.Version, error) {
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	// Slot 0 offers 5.6 with a minor range down to 5.0:
	// [pad=0x00, minor_range=6, minor=6, major=5].
	buf[4], buf[5], buf[6], buf[7] = 0, 6, 6, 5
	// Slot 1 offers 4.4 as a fallback: [0x00, 0, 4, 4].
	buf[8], buf[9], buf[10], buf[11] = 0, 0, 4, 4
	// Slots 2–3 zero (not offered).
	if _, err := c.conn.Write(buf[:]); err != nil {
		return proto.Version{}, fmt.Errorf("sim: handshake write: %w", err)
	}
	var resp [4]byte
	if _, err := io.ReadFull(c.conn, resp[:]); err != nil {
		return proto.Version{}, fmt.Errorf("sim: handshake read: %w", err)
	}
	if resp[2] == 0 && resp[3] == 0 {
		return proto.Version{}, fmt.Errorf("sim: server rejected version negotiation")
	}
	c.ver = proto.Version{Major: resp[3], Minor: resp[2]}
	return c.ver, nil
}

// Version reports the negotiated protocol version (zero before Handshake).
func (c *WireClient) Version() proto.Version { return c.ver }

// Connect drives the full ready-to-query handshake: the wire handshake, a HELLO,
// and — when the negotiated version is Bolt 5.1+ (which defers authentication to
// a dedicated LOGON message) — a LOGON. It returns an error if any step does not
// produce a SUCCESS, leaving the session ready for RUN. It is the convenience
// path the honest, overload, and slow-consumer actors use; the BoltAbuser drives
// the lower-level primitives directly.
func (c *WireClient) Connect(ctx context.Context) error {
	if _, err := c.Handshake(ctx); err != nil {
		return err
	}
	helloResp, err := c.Hello(nil)
	if err != nil {
		return err
	}
	if f, ok := helloResp.(*proto.Failure); ok {
		return fmt.Errorf("sim: HELLO failed: %s %s", f.Code, f.Message)
	}
	if c.deferredAuth() {
		logonResp, err := c.Logon()
		if err != nil {
			return err
		}
		if f, ok := logonResp.(*proto.Failure); ok {
			return fmt.Errorf("sim: LOGON failed: %s %s", f.Code, f.Message)
		}
	}
	return nil
}

// deferredAuth reports whether the negotiated version defers authentication to a
// LOGON message (Bolt 5.1 and later), mirroring the server's split.
func (c *WireClient) deferredAuth() bool {
	return c.ver.Major > 5 || (c.ver.Major == 5 && c.ver.Minor >= 1)
}

// Hello sends a HELLO with scheme="none" (the NoAuth server admits it) and
// returns the response message (typically *proto.Success). For Bolt 5.1+ the
// server defers auth to a LOGON message; this client targets the inline
// (<=5.0-style) HELLO auth the NoAuth handler accepts, which the server honours
// across the supported versions in the DST harness.
func (c *WireClient) Hello(extra map[string]packstream.Value) (any, error) {
	if extra == nil {
		extra = map[string]packstream.Value{
			"scheme":      "none",
			"principal":   "sim",
			"credentials": "",
			"user_agent":  "gograph-sim/3.0",
		}
	}
	return c.Request(&proto.Hello{Extra: extra})
}

// Logon sends a LOGON with scheme="none" for the Bolt 5.1+ deferred-auth path
// and returns the response.
func (c *WireClient) Logon() (any, error) {
	return c.Request(&proto.Logon{Auth: map[string]packstream.Value{"scheme": "none"}})
}

// Run sends a RUN for query with params and returns the response (a *proto.Success
// carrying the field metadata, or a *proto.Failure). It does NOT pull records;
// follow with [WireClient.PullAll] or [WireClient.Pull].
func (c *WireClient) Run(query string, params map[string]any) (any, error) {
	ps, err := toPackstreamParams(params)
	if err != nil {
		return nil, err
	}
	return c.Request(&proto.Run{
		Query:      query,
		Parameters: ps,
		Extra:      map[string]packstream.Value{},
	})
}

// PullAll sends PULL {n:-1} and reads every RECORD up to the terminal SUCCESS or
// FAILURE, returning the records and the terminal message. A FAILURE terminates
// the pull with the records gathered so far.
func (c *WireClient) PullAll() (records []*proto.Record, terminal any, err error) {
	if err := c.send(&proto.Pull{N: -1, QID: -1}); err != nil {
		return nil, nil, err
	}
	for {
		msg, err := c.recv()
		if err != nil {
			return records, nil, err
		}
		switch m := msg.(type) {
		case *proto.Record:
			records = append(records, m)
		case *proto.Success, *proto.Failure, *proto.Ignored:
			return records, m, nil
		default:
			return records, nil, fmt.Errorf("sim: PullAll: unexpected message %T", msg)
		}
	}
}

// Pull sends PULL {n:n} and reads up to n RECORDs plus the terminal message. It
// is used by the SlowConsumer, which pulls in small batches with deliberate
// stalls between calls.
func (c *WireClient) Pull(n int64) (records []*proto.Record, terminal any, err error) {
	if err := c.send(&proto.Pull{N: n, QID: -1}); err != nil {
		return nil, nil, err
	}
	for {
		msg, err := c.recv()
		if err != nil {
			return records, nil, err
		}
		switch m := msg.(type) {
		case *proto.Record:
			records = append(records, m)
		case *proto.Success, *proto.Failure, *proto.Ignored:
			return records, m, nil
		default:
			return records, nil, fmt.Errorf("sim: Pull: unexpected message %T", msg)
		}
	}
}

// Begin / Commit / Rollback / Reset / Route drive the explicit-transaction and
// session-control messages; each returns the server's response.

// Begin sends BEGIN and returns the response.
func (c *WireClient) Begin() (any, error) {
	return c.Request(&proto.Begin{Extra: map[string]packstream.Value{}})
}

// Commit sends COMMIT and returns the response.
func (c *WireClient) Commit() (any, error) { return c.Request(&proto.Commit{}) }

// Rollback sends ROLLBACK and returns the response.
func (c *WireClient) Rollback() (any, error) { return c.Request(&proto.Rollback{}) }

// Reset sends RESET and returns the response.
func (c *WireClient) Reset() (any, error) { return c.Request(&proto.Reset{}) }

// Goodbye sends GOODBYE. No response is expected; the server tears the session
// down.
func (c *WireClient) Goodbye() error { return c.send(&proto.Goodbye{}) }

// Request is the LOCK-STEP primitive: it sends one request and reads exactly one
// response message back. For messages whose reply is a single SUCCESS/FAILURE
// (HELLO, LOGON, RUN, BEGIN, COMMIT, ROLLBACK, RESET, ROUTE) this is the full
// terminal exchange. It returns the decoded *proto.Success, *proto.Failure, or
// *proto.Ignored.
func (c *WireClient) Request(msg any) (any, error) {
	if err := c.send(msg); err != nil {
		return nil, err
	}
	return c.recv()
}

// Close closes the underlying connection.
func (c *WireClient) Close() error { return c.conn.Close() }

// Conn returns the underlying SimConn, for callers that need a hard reset
// (CloseWithError) to model an abrupt disconnect.
func (c *WireClient) Conn() *SimConn { return c.conn }

// send encodes msg as a PackStream request and writes it as one chunked Bolt
// message.
func (c *WireClient) send(msg any) error {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		return fmt.Errorf("sim: encode %T: %w", msg, err)
	}
	if err := enc.Flush(); err != nil {
		return fmt.Errorf("sim: flush %T: %w", msg, err)
	}
	if err := c.cw.WriteMessage(buf.Bytes()); err != nil {
		return fmt.Errorf("sim: write %T: %w", msg, err)
	}
	return nil
}

// recv reads one chunked message and decodes it as a Bolt response.
func (c *WireClient) recv() (any, error) {
	raw, err := c.cr.ReadMessage()
	if err != nil {
		return nil, err
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		return nil, fmt.Errorf("sim: decode response: %w", err)
	}
	return msg, nil
}

// WriteRaw writes raw bytes directly to the connection, bypassing chunked
// framing. It is the seam the BoltAbuser uses to emit deliberately malformed
// wire bytes (bad handshakes, truncated chunks, garbage opcodes) the framed
// send path would never produce.
func (c *WireClient) WriteRaw(p []byte) (int, error) { return c.conn.Write(p) }

// WriteChunkedRaw writes payload as one well-framed chunked message regardless
// of whether payload decodes to a valid Bolt message. The BoltAbuser uses it to
// deliver garbage opcodes and wrong-state messages that are correctly framed but
// semantically invalid, exercising the server's message-level (not framing-level)
// rejection.
func (c *WireClient) WriteChunkedRaw(payload []byte) error {
	return c.cw.WriteMessage(payload)
}

// RecvRaw reads one chunked message and returns its raw bytes without decoding,
// for the abuser to inspect a FAILURE the standard decoder would also handle.
func (c *WireClient) RecvRaw() ([]byte, error) { return c.cr.ReadMessage() }

// Recv reads and decodes the next response message; exported for actors that
// read a server-initiated response outside the Request/Pull helpers.
func (c *WireClient) Recv() (any, error) { return c.recv() }

// toPackstreamParams converts a simulator parameter map to the packstream value
// map a RUN expects. The supported kinds mirror [toExprParams]; an unsupported
// kind is a loud error rather than a silent coercion.
func toPackstreamParams(params map[string]any) (map[string]packstream.Value, error) {
	if len(params) == 0 {
		return map[string]packstream.Value{}, nil
	}
	out := make(map[string]packstream.Value, len(params))
	for k, v := range params {
		switch t := v.(type) {
		case string:
			out[k] = t
		case int64:
			out[k] = t
		case int:
			out[k] = int64(t)
		case float64:
			out[k] = t
		case bool:
			out[k] = t
		case []int64:
			lst := make([]packstream.Value, len(t))
			for i, e := range t {
				lst[i] = e
			}
			out[k] = lst
		default:
			return nil, fmt.Errorf("sim: wire param %q: unsupported type %T", k, v)
		}
	}
	return out, nil
}

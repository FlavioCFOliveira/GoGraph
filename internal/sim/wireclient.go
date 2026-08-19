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

// HandshakeOffering performs the 20-byte Bolt client handshake offering exactly
// the given versions, one per slot, and records the negotiated version. It is the
// explicit-offer counterpart of [WireClient.Handshake], which always leads with
// 5.6: a probe that must reach the pre-5.1 INLINE-auth path (credentials on
// HELLO) or a specific entity layout has to be able to withhold the newer
// versions, because the server correctly picks the highest it is offered.
//
// At most four versions may be offered (the preamble has four slots); each is
// offered with a minor RANGE of zero, i.e. that exact version and no fallback.
// It returns an error when the server rejects negotiation (responds 0.0).
func (c *WireClient) HandshakeOffering(_ context.Context, offers ...proto.Version) (proto.Version, error) {
	if len(offers) == 0 || len(offers) > 4 {
		return proto.Version{}, fmt.Errorf("sim: handshake offers %d versions, want 1..4", len(offers))
	}
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	for i, v := range offers {
		off := 4 + i*4
		// Slot layout: [pad=0x00, minor_range=0, minor, major].
		buf[off], buf[off+1], buf[off+2], buf[off+3] = 0, 0, v.Minor, v.Major
	}
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

// LogonWith sends a LOGON carrying the given auth token and returns the
// response. It is the credential-bearing counterpart of [WireClient.Logon],
// used by the auth-surface scenario to present a right or a wrong password to a
// server whose [github.com/FlavioCFOliveira/GoGraph/bolt/server.AuthHandler]
// actually validates one (rmp #2481).
func (c *WireClient) LogonWith(auth map[string]packstream.Value) (any, error) {
	return c.Request(&proto.Logon{Auth: auth})
}

// Logoff sends a LOGOFF and returns the response. A successful LOGOFF
// de-authorises the connection: every subsequent query-bearing or
// transaction-finalising message must be refused until a fresh LOGON
// re-authenticates (the CWE-306 gate).
func (c *WireClient) Logoff() (any, error) {
	return c.Request(&proto.Logoff{})
}

// basicAuthToken builds the auth map a Bolt driver sends for the "basic" scheme.
func basicAuthToken(principal, credentials string) map[string]packstream.Value {
	return map[string]packstream.Value{
		"scheme":      "basic",
		"principal":   principal,
		"credentials": credentials,
	}
}

// ConnectAs negotiates a version if one is not negotiated yet and then
// authenticates with the given basic-scheme credentials, returning the response
// that carried the authentication decision so the caller can adjudicate it.
// Unlike [WireClient.Connect] it does NOT treat a FAILURE as an error — a refused
// credential is the very outcome an auth probe is measuring — so the returned
// error is reserved for transport failures.
//
// A caller that needs a SPECIFIC version negotiates it first with
// [WireClient.HandshakeOffering]; ConnectAs then keeps it, because re-sending a
// preamble on a negotiated connection is not a second handshake — it is 20 bytes
// of garbage arriving where the server expects a chunked message.
func (c *WireClient) ConnectAs(ctx context.Context, principal, credentials string) (any, error) {
	if c.ver == (proto.Version{}) {
		if _, err := c.Handshake(ctx); err != nil {
			return nil, err
		}
	}
	return c.AuthenticateAs(principal, credentials)
}

// AuthenticateAs sends the credential-bearing message(s) for the ALREADY
// negotiated version and returns the response that carried the decision. The
// credentials travel on the message the version puts them on: LOGON for Bolt
// >= 5.1 (which authenticates separately from a credential-less HELLO), HELLO
// itself for <= 5.0.
func (c *WireClient) AuthenticateAs(principal, credentials string) (any, error) {
	if !c.deferredAuth() {
		token := basicAuthToken(principal, credentials)
		token["user_agent"] = "gograph-sim/3.0"
		return c.Hello(token)
	}
	helloResp, err := c.Hello(map[string]packstream.Value{"user_agent": "gograph-sim/3.0"})
	if err != nil {
		return nil, err
	}
	if f, ok := helloResp.(*proto.Failure); ok {
		return f, nil
	}
	return c.LogonWith(basicAuthToken(principal, credentials))
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
	return c.drainExchange("PullAll", &proto.Pull{N: -1, QID: -1})
}

// Pull sends PULL {n:n} and reads up to n RECORDs plus the terminal message. It
// is used by the SlowConsumer, which pulls in small batches with deliberate
// stalls between calls, and by the paging arms of rmp #2484.
func (c *WireClient) Pull(n int64) (records []*proto.Record, terminal any, err error) {
	return c.drainExchange("Pull", &proto.Pull{N: n, QID: -1})
}

// Discard sends DISCARD {n:n, qid:-1} and reads to the terminal reply, returning
// any RECORDs that arrived along the way and the terminal message.
//
// The record slice exists to be asserted EMPTY. DISCARD's contract is that the
// rows are dropped server-side rather than delivered, so a non-empty slice here
// is the defect; a helper that could not observe a stray RECORD could not tell
// DISCARD from PULL. n <= 0 discards the whole remaining stream; n > 0 discards up
// to n rows and the terminal SUCCESS reports has_more for the remainder
// (bolt/server/session.go handleDiscard).
//
// Nothing in the harness sent DISCARD before rmp #2484: every call site used
// PULL -1, so the whole discard path of the session — including its
// statement-error guard and its own has_more accounting — was driven by no
// scenario at all.
func (c *WireClient) Discard(n int64) (records []*proto.Record, terminal any, err error) {
	return c.drainExchange("Discard", &proto.Discard{N: n, QID: -1})
}

// PullQID sends PULL {n:n, qid:qid} with an EXPLICIT qid and reads to the terminal
// reply. It exists so a scenario can address a stream by qid rather than by the
// implicit current-stream -1.
//
// A qid >= 0 is expected to be REFUSED: this server keeps exactly one open stream
// per session (RUN always reports qid = -1, and a second RUN while streaming is an
// illegal transition), so handlePull answers any non-negative qid with
// Neo.ClientError.Request.Invalid / "no such query: qid N"
// (bolt/server/session.go:1240-1243).
func (c *WireClient) PullQID(n, qid int64) (records []*proto.Record, terminal any, err error) {
	return c.drainExchange("PullQID", &proto.Pull{N: n, QID: qid})
}

// DiscardQID sends DISCARD {n:n, qid:qid} with an EXPLICIT qid and reads to the
// terminal reply. As with [WireClient.PullQID], a qid >= 0 is expected to be
// refused with the same code and message shape
// (bolt/server/session.go:1421-1424).
func (c *WireClient) DiscardQID(n, qid int64) (records []*proto.Record, terminal any, err error) {
	return c.drainExchange("DiscardQID", &proto.Discard{N: n, QID: qid})
}

// drainExchange sends one stream-consuming message and reads until the terminal
// reply, accumulating every RECORD that arrives first. It is the single body
// behind PullAll/Pull/Discard/PullQID/DiscardQID so those five differ only in the
// message they put on the wire.
//
// IGNORED counts as terminal, not as an error: it is the Bolt-correct reply to a
// request-phase message on a FAILED session, and a caller that treated it as a
// transport fault could not distinguish "the session is poisoned" from "the
// connection broke".
func (c *WireClient) drainExchange(label string, msg any) (records []*proto.Record, terminal any, err error) {
	if err := c.send(msg); err != nil {
		return nil, nil, err
	}
	for {
		m, err := c.recv()
		if err != nil {
			return records, nil, err
		}
		switch t := m.(type) {
		case *proto.Record:
			records = append(records, t)
		case *proto.Success, *proto.Failure, *proto.Ignored:
			return records, t, nil
		default:
			return records, nil, fmt.Errorf("sim: %s: unexpected message %T", label, m)
		}
	}
}

// Begin / Commit / Rollback / Reset / Route drive the explicit-transaction and
// session-control messages; each returns the server's response.

// Begin sends BEGIN with no extras and returns the response. The server then
// applies its own defaults: mode "w" and server.Options.DefaultTxTimeout as the
// transaction's total lifetime bound.
func (c *WireClient) Begin() (any, error) { return c.BeginMode("") }

// BeginMode sends BEGIN carrying the transaction access mode and returns the
// response. It exists so a scenario can open a READ-ONLY explicit transaction over
// the genuine wire — the Mode field server.Server.Transactions reports, and the
// branch that takes cypher's lock-free BeginReadTx path instead of BeginTx.
//
// # The wire spelling, as VERIFIED in bolt/server/session.go handleBegin
//
// The key is "mode" in the BEGIN extras and the value is a PackStream string. Only
// the exact string "r" selects read-only; handleBegin reads
//
//	if v, ok := m.Extra["mode"]; ok {
//	        if modeStr, ok := v.(string); ok && modeStr == "r" { mode = "r" }
//	}
//
// so every other value — "w", a misspelling, a non-string, or an absent key —
// leaves the default "w". A caller asking for "w" is therefore asking for the
// default explicitly rather than selecting a second behaviour, and an unknown mode
// is silently a write transaction rather than an error.
//
// An EMPTY mode OMITS the key entirely rather than sending "mode": "", so
// [WireClient.Begin] delegating here is byte-identical on the wire to the empty
// extras map it sent before, not merely equivalent in the server's eventual
// decision.
func (c *WireClient) BeginMode(mode string) (any, error) {
	extra := map[string]packstream.Value{}
	if mode != "" {
		extra["mode"] = mode
	}
	return c.Request(&proto.Begin{Extra: extra})
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
//
// Every PackStream scalar and composite kind an embedder can bind is covered
// (rmp #2462): NULL, Boolean, Integer, Float, String, List, and Map — the last
// two recursively, so a nested list-of-maps binds correctly. The Bolt server
// hands the decoded map straight to the engine (bolt/server/session.go passes
// map[string]any(m.Parameters) to RunAny), so this conversion IS the wire
// contract for parameter binding, and it is the path on which a
// literal/parameter divergence actually reaches a driver user.
func toPackstreamParams(params map[string]any) (map[string]packstream.Value, error) {
	if len(params) == 0 {
		return map[string]packstream.Value{}, nil
	}
	out := make(map[string]packstream.Value, len(params))
	for k, v := range params {
		pv, err := toPackstreamValue(v)
		if err != nil {
			return nil, fmt.Errorf("sim: wire param %q: %w", k, err)
		}
		out[k] = pv
	}
	return out, nil
}

// toPackstreamValue converts one simulator parameter value to its PackStream
// representation, recursing into lists and maps. An unsupported kind is a loud
// error rather than a silent coercion, so a workload bug surfaces instead of
// binding a wrong value.
func toPackstreamValue(v any) (packstream.Value, error) {
	switch t := v.(type) {
	case nil:
		// PackStream NULL. Typed as a nil packstream.Value so the encoder takes
		// its WriteNull arm rather than a typed-nil interface.
		return nil, nil
	case string:
		return t, nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return t, nil
	case bool:
		return t, nil
	case []int64:
		lst := make([]packstream.Value, len(t))
		for i, e := range t {
			lst[i] = e
		}
		return lst, nil
	case []string:
		lst := make([]packstream.Value, len(t))
		for i, e := range t {
			lst[i] = e
		}
		return lst, nil
	case []any:
		lst := make([]packstream.Value, len(t))
		for i, e := range t {
			ev, err := toPackstreamValue(e)
			if err != nil {
				return nil, fmt.Errorf("list element %d: %w", i, err)
			}
			lst[i] = ev
		}
		return lst, nil
	case map[string]any:
		m := make(map[string]packstream.Value, len(t))
		for mk, e := range t {
			ev, err := toPackstreamValue(e)
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", mk, err)
			}
			m[mk] = ev
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unsupported type %T", v)
	}
}

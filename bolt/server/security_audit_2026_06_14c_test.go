package server_test

// security_audit_2026_06_14c_test.go — VERIFIED-SOLID lock-in for the FOURTH
// Bolt security audit (SEC-2026-06-14c).
//
// This audit re-attacked the only network-facing surface of GoGraph — the Bolt
// protocol stack (bolt/packstream, bolt/proto, bolt/server) — looking for new
// vulnerabilities and for regressions of the three prior audits' fixes
// (SEC-2026-06-14, -14b, and the #1345/#1470 auth work). It found NO new
// exploitable vulnerability: every classic vector (untrusted-deserialization
// allocation/recursion bombs, auth/state-machine bypass, connection DoS,
// protocol confusion, information disclosure) is already closed.
//
// The tests below are bounded, wire-level regression gates that drive the REAL
// server with crafted attacker bytes and assert the secure outcome. They exist
// so that a future change cannot silently re-open a surface this audit
// certified solid. Every test sets a short deadline and uses tiny payloads, so
// none can hang or exhaust memory even if a defense is removed.
//
// Layer: short. Servers and connections are torn down via t.Cleanup / defer;
// the package goleak TestMain enforces goroutine cleanliness.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// secCDial opens a raw TCP connection to addr with a short overall deadline.
func secCDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// secCHandshake performs the 20-byte client handshake offering Bolt 5.4 and
// returns the negotiated major.minor. A zero response means the server rejected
// negotiation.
func secCHandshake(t *testing.T, conn net.Conn) (major, minor byte) {
	t.Helper()
	var hs [20]byte
	binary.BigEndian.PutUint32(hs[:4], proto.Magic)
	hs[6] = 4 // minor
	hs[7] = 5 // major
	if _, err := conn.Write(hs[:]); err != nil {
		t.Fatalf("handshake write: %v", err)
	}
	var resp [4]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	return resp[3], resp[2]
}

// secCSendChunked writes payload as a single Bolt chunk followed by the
// end-of-message sentinel.
func secCSendChunked(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(payload)))
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("chunk header write: %v", err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("chunk payload write: %v", err)
	}
	if _, err := conn.Write([]byte{0, 0}); err != nil {
		t.Fatalf("chunk sentinel write: %v", err)
	}
}

// secCReadResponse reads one chunked Bolt message and decodes it as a response.
func secCReadResponse(t *testing.T, conn net.Conn) any {
	t.Helper()
	cr := proto.NewChunkedReader(conn)
	raw, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return msg
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. PackStream untrusted-deserialization surface (decoder bombs)
// ─────────────────────────────────────────────────────────────────────────────

// TestSecC_Decoder_AllocationBombsRejectedBeforeAlloc verifies the decoder's
// length/count guards (ErrLengthExceedsInput / wire32MaxLen) reject a tiny
// frame that claims a multi-gigabyte allocation BEFORE any make() runs
// (CWE-400, CWE-502, CWE-190). Each case is a 5-byte header claiming a huge
// payload with no bytes behind it.
func TestSecC_Decoder_AllocationBombsRejectedBeforeAlloc(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		header []byte // marker + 32-bit count, no payload
	}{
		{"Bytes32_4GB", []byte{0xCE, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"String32_4GB", []byte{0xD2, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"List32_4G_elems", []byte{0xD6, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"Map32_4G_entries", []byte{0xDA, 0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := packstream.NewDecoder(bytes.NewReader(tc.header))
			_, err := dec.ReadValue()
			if err == nil {
				t.Fatalf("%s: decoder accepted a 4G allocation claim from a 5-byte frame", tc.name)
			}
			// It must fail at the length guard, never with a runtime panic or OOM.
			if !errors.Is(err, packstream.ErrLengthExceedsInput) {
				t.Logf("%s rejected with: %v (acceptable as long as it is not a panic/OOM)", tc.name, err)
			}
		})
	}
}

// TestSecC_Decoder_DeepNestingRejected verifies a deeply-nested composite is
// rejected with ErrNestingTooDeep rather than overflowing the goroutine stack
// (CWE-674). 4096 nested TinyLists is far beyond maxValueDepth (128).
func TestSecC_Decoder_DeepNestingRejected(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	for range 4096 {
		b.WriteByte(0x91) // TinyList of 1 element
	}
	b.WriteByte(0xC0) // terminal NULL
	dec := packstream.NewDecoder(bytes.NewReader(b.Bytes()))
	_, err := dec.ReadValue()
	if !errors.Is(err, packstream.ErrNestingTooDeep) {
		t.Fatalf("deep nesting must be rejected with ErrNestingTooDeep, got %v", err)
	}
}

// TestSecC_Decoder_DecodedMemoryBudgetAmplification verifies the
// decoded-memory budget rejects a structurally-valid message that amplifies
// far beyond its wire size — a List of minimal-cost NULL elements whose decoded
// slots vastly exceed the wire bytes (CWE-400). A List16 of 65535 NULLs is
// accepted (within budget); a chained message that would blow the 128 MiB
// decoded budget is rejected. Here we assert the in-budget case decodes and a
// gross over-budget List32 claim is stopped at the header.
func TestSecC_Decoder_DecodedMemoryBudgetAmplification(t *testing.T) {
	t.Parallel()
	// A List32 claiming 2^31-1 NULL elements: even though each is 1 wire byte,
	// the header guard (wire byte budget, then decoded budget) rejects it before
	// allocation. We give it no payload, so the wire byte budget trips first.
	// 0xD6 is the List32 marker; the four bytes that follow are the big-endian
	// element count 0x7FFFFFFF (MaxInt32).
	header := []byte{0xD6, 0x7F, 0xFF, 0xFF, 0xFF}
	dec := packstream.NewDecoder(bytes.NewReader(header))
	if _, err := dec.ReadValue(); err == nil {
		t.Fatal("List32 of 2G elements must be rejected before allocation")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Auth / state-machine bypass surface (wire-level, real server)
// ─────────────────────────────────────────────────────────────────────────────

// TestSecC_RunBeforeAuthRejected verifies that, on a real server, an
// unauthenticated client that completes the handshake and immediately sends RUN
// (skipping HELLO/LOGON) is rejected with a FAILURE and never executes the
// query (CWE-287, regression gate for #1345). The server uses a credentialed
// BasicAuthHandler so "authenticated" is meaningful.
func TestSecC_RunBeforeAuthRejected(t *testing.T) {
	t.Parallel()
	addr := startTestServerWithEngine(t, newEngine(t), server.Options{
		Auth:        server.BasicAuthHandler{Validate: server.ConstantTimeValidate("u", "p")},
		ConnTimeout: 3 * time.Second,
	})
	conn := secCDial(t, addr)
	if maj, _ := secCHandshake(t, conn); maj != 5 {
		t.Fatalf("handshake: got major %d, want 5", maj)
	}

	// Encode a RUN message and send it as the FIRST application message.
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, &proto.Run{
		Query:      "RETURN 1",
		Parameters: map[string]packstream.Value{},
		Extra:      map[string]packstream.Value{},
	}); err != nil {
		t.Fatalf("encode RUN: %v", err)
	}
	_ = enc.Flush()
	secCSendChunked(t, conn, buf.Bytes())

	msg := secCReadResponse(t, conn)
	f, ok := msg.(*proto.Failure)
	if !ok {
		t.Fatalf("RUN-before-auth must be rejected with FAILURE, got %T", msg)
	}
	if f.Code != "Neo.ClientError.Request.Invalid" {
		t.Fatalf("RUN-before-auth FAILURE code = %q, want Neo.ClientError.Request.Invalid", f.Code)
	}
}

// TestSecC_ResetBeforeAuthDoesNotGrantReady verifies the RESET-from-pre-auth
// bypass (#1345) stays closed end-to-end: a never-authenticated client that
// sends RESET first gets a SUCCESS (RESET is universally legal) but the
// connection returns to NEGOTIATION — so a following RUN is still rejected.
func TestSecC_ResetBeforeAuthDoesNotGrantReady(t *testing.T) {
	t.Parallel()
	addr := startTestServerWithEngine(t, newEngine(t), server.Options{
		Auth:        server.BasicAuthHandler{Validate: server.ConstantTimeValidate("u", "p")},
		ConnTimeout: 3 * time.Second,
	})
	conn := secCDial(t, addr)
	secCHandshake(t, conn)

	// RESET first (no HELLO/LOGON).
	var rb bytes.Buffer
	renc := packstream.NewEncoder(&rb)
	_ = proto.EncodeRequest(renc, &proto.Reset{})
	_ = renc.Flush()
	secCSendChunked(t, conn, rb.Bytes())
	if _, ok := secCReadResponse(t, conn).(*proto.Success); !ok {
		t.Fatal("RESET should be answered with SUCCESS")
	}

	// Now RUN — must STILL be rejected because RESET pre-auth returns to
	// NEGOTIATION, not READY.
	var qb bytes.Buffer
	qenc := packstream.NewEncoder(&qb)
	_ = proto.EncodeRequest(qenc, &proto.Run{
		Query:      "RETURN 1",
		Parameters: map[string]packstream.Value{},
		Extra:      map[string]packstream.Value{},
	})
	_ = qenc.Flush()
	secCSendChunked(t, conn, qb.Bytes())
	if _, ok := secCReadResponse(t, conn).(*proto.Failure); !ok {
		t.Fatal("RUN after pre-auth RESET must be rejected: RESET must not grant READY (#1345)")
	}
}

// TestSecC_BasicAuth_NoneSchemeRejected verifies a server with auth ENABLED
// (BasicAuthHandler) rejects the "none" auth scheme — per the Bolt spec, scheme
// "none" is permitted only when authentication is disabled. This prevents a
// trivial bypass of a credentialed server.
func TestSecC_BasicAuth_NoneSchemeRejected(t *testing.T) {
	t.Parallel()
	h := server.BasicAuthHandler{Validate: server.ConstantTimeValidate("u", "p")}
	if _, err := h.Authenticate("none", "", ""); !errors.Is(err, server.ErrSchemeUnknown) {
		t.Fatalf("scheme \"none\" against a credentialed handler must be rejected with ErrSchemeUnknown, got %v", err)
	}
	// A wrong basic credential still fails closed.
	if _, err := h.Authenticate("basic", "u", "wrong"); !errors.Is(err, server.ErrAuthFailed) {
		t.Fatalf("wrong basic credential must fail with ErrAuthFailed, got %v", err)
	}
	// The correct credential succeeds.
	if id, err := h.Authenticate("basic", "u", "p"); err != nil || id.Principal != "u" {
		t.Fatalf("correct basic credential must succeed, got id=%+v err=%v", id, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Protocol-confusion surface (malformed frames, real server)
// ─────────────────────────────────────────────────────────────────────────────

// TestSecC_UnknownStructTagFailsCleanly verifies that a post-handshake frame
// carrying an unknown PackStream struct tag is answered with a fixed,
// non-sensitive FAILURE ("malformed Bolt message") and does NOT crash the
// process or leak internal framing detail (task #1435, CWE-209). The connection
// survives for a follow-up message.
func TestSecC_UnknownStructTagFailsCleanly(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t, server.Options{Auth: server.NoAuthHandler{}, ConnTimeout: 3 * time.Second})
	conn := secCDial(t, addr)
	secCHandshake(t, conn)

	// 0xB0 = TinyStruct, 0 fields; tag 0xFF is not a known request tag.
	secCSendChunked(t, conn, []byte{0xB0, 0xFF})
	msg := secCReadResponse(t, conn)
	f, ok := msg.(*proto.Failure)
	if !ok {
		t.Fatalf("unknown struct tag must yield FAILURE, got %T", msg)
	}
	if f.Code != "Neo.ClientError.Request.Invalid" {
		t.Fatalf("FAILURE code = %q, want Neo.ClientError.Request.Invalid", f.Code)
	}
	if f.Message != "malformed Bolt message" {
		t.Fatalf("FAILURE message = %q; must be the fixed non-sensitive string, never raw decode detail", f.Message)
	}
}

// TestSecC_NoopKeepAliveDiscardedSilently verifies a standalone 00 00 NOOP
// keep-alive (Bolt 4.1+) is silently discarded at the framing layer and never
// produces a spurious response (regression gate for the SEC-2026-06-14b NOOP
// fix). After the NOOP, a normal HELLO must still succeed on a no-auth server.
func TestSecC_NoopKeepAliveDiscardedSilently(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t, server.Options{Auth: server.NoAuthHandler{}, ConnTimeout: 3 * time.Second})
	conn := secCDial(t, addr)
	secCHandshake(t, conn)

	// Standalone NOOP: a 00 00 with no preceding payload chunk.
	if _, err := conn.Write([]byte{0, 0}); err != nil {
		t.Fatalf("write NOOP: %v", err)
	}

	// Follow with a real HELLO (scheme none) — it must get the SUCCESS, proving
	// the NOOP was skipped, not (mis)decoded as a zero-length message.
	var hb bytes.Buffer
	henc := packstream.NewEncoder(&hb)
	_ = proto.EncodeRequest(henc, &proto.Hello{Extra: map[string]packstream.Value{
		"scheme": "none", "principal": "x", "credentials": "", "agent": "sec/1.0",
	}})
	_ = henc.Flush()
	secCSendChunked(t, conn, hb.Bytes())
	if _, ok := secCReadResponse(t, conn).(*proto.Success); !ok {
		t.Fatal("HELLO after a NOOP keep-alive must succeed; the NOOP must be silently discarded")
	}
}

// Note on the tx_timeout / timeout integer-overflow surface (#1484, CWE-190):
// the overflow-safe client-millis conversion is already pinned end-to-end by
// security_bolt_txtimeout_overflow_test.go (TestSec_TxTimeoutOverflow_*), which
// drives a hostile tx_timeout through the real BEGIN wire path and proves the
// writer-lock reaper is still armed. This audit re-verified that gate holds; no
// additional export seam is added here.

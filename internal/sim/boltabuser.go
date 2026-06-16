package sim

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// AbuseFamily identifies one class of Bolt wire-protocol violation the
// [BoltAbuser] emits. The set is fixed so a unit test can assert every family is
// reachable and exercised.
type AbuseFamily int

// Bolt abuse families.
const (
	// AbuseBadHandshake sends an invalid version-handshake preamble (wrong magic).
	AbuseBadHandshake AbuseFamily = iota
	// AbuseNoCommonVersion offers only versions the server does not support, so
	// negotiation must fail (server responds 0.0 and closes).
	AbuseNoCommonVersion
	// AbuseTruncatedChunk sends a chunk header advertising more bytes than follow,
	// then closes — a partial/truncated message.
	AbuseTruncatedChunk
	// AbuseOversizedChunk sends a single well-framed message whose total size
	// exceeds the server's MaxMessageBytes cap (but is bounded by the harness).
	AbuseOversizedChunk
	// AbusePullBeforeRun sends PULL immediately after auth, with no preceding RUN
	// (wrong session state).
	AbusePullBeforeRun
	// AbuseRunBeforeLogon sends RUN before authenticating (wrong session state on
	// a deferred-auth version, or before HELLO on inline-auth).
	AbuseRunBeforeLogon
	// AbuseGarbageOpcode sends a correctly-framed message carrying an unknown
	// struct tag (a garbage opcode).
	AbuseGarbageOpcode
	// AbuseDuplicateHello sends two HELLOs back to back (a duplicate/interleaved
	// marker for an already-progressed session).
	AbuseDuplicateHello
)

// abuseFamilyCount is the number of distinct abuse families. It MUST equal the
// number of AbuseFamily constants; the unit test asserts every family is
// reachable and produces an acceptable outcome.
const abuseFamilyCount = 8

// String renders an AbuseFamily for reports.
func (f AbuseFamily) String() string {
	switch f {
	case AbuseBadHandshake:
		return "BadHandshake"
	case AbuseNoCommonVersion:
		return "NoCommonVersion"
	case AbuseTruncatedChunk:
		return "TruncatedChunk"
	case AbuseOversizedChunk:
		return "OversizedChunk"
	case AbusePullBeforeRun:
		return "PullBeforeRun"
	case AbuseRunBeforeLogon:
		return "RunBeforeLogon"
	case AbuseGarbageOpcode:
		return "GarbageOpcode"
	case AbuseDuplicateHello:
		return "DuplicateHello"
	default:
		return fmt.Sprintf("AbuseFamily(%d)", int(f))
	}
}

// AbuseOutcome records how the server responded to one abuse attempt. Exactly
// one of GotFailure or GotClose is the expected acceptable result; a third
// outcome (a normal SUCCESS where a violation was sent, or a hang) is a defect
// the checker flags. The Family and the seed that chose it are retained so a
// finding is reproducible.
type AbuseOutcome struct {
	Family     AbuseFamily
	GotFailure bool   // server replied with a typed FAILURE
	GotClose   bool   // server closed the connection cleanly (or it became unreadable)
	FailureMsg string // populated when GotFailure
}

// Acceptable reports whether the outcome is one the robustness contract allows:
// a typed FAILURE or a clean connection close. Anything else (no terminal
// response, or an unexpected SUCCESS) is a violation.
func (o AbuseOutcome) Acceptable() bool { return o.GotFailure || o.GotClose }

// BoltAbuser emits protocol-level wire abuse over a [SimConn] and classifies the
// server's response. Each abuse runs on its own fresh connection in LOCK-STEP
// (send the violation, then block reading the terminal response or observing the
// close), so a given seed reproduces the exact violation and the exact server
// reaction. The server must respond with a typed FAILURE or close the connection
// cleanly — never panic, never leak a goroutine, never corrupt state.
//
// # Concurrency contract
//
// BoltAbuser is stateless and its [BoltAbuser.Abuse] method may be called from
// any goroutine, but each call drives one connection it owns end-to-end.
type BoltAbuser struct{}

// Name returns the abuser's identifier.
func (BoltAbuser) Name() string { return "BoltAbuser" }

// PickFamily chooses an abuse family from the seed. It draws exactly one int so
// the workload draw stream is stable.
func (BoltAbuser) PickFamily(seed *Seed) AbuseFamily {
	return AbuseFamily(seed.IntN(abuseFamilyCount))
}

// Abuse opens a fresh connection to srv, emits the chosen abuse family over the
// wire, and returns the classified outcome. The connection is always closed
// before return, so no goroutine or handle leaks regardless of how the server
// reacted. An error is returned only for a harness-level failure (e.g. the
// listener is closed), never for an expected server FAILURE/close.
func (a BoltAbuser) Abuse(srv *SimServer, family AbuseFamily) (AbuseOutcome, error) {
	conn, err := srv.DialConn()
	if err != nil {
		return AbuseOutcome{}, err
	}
	defer func() { _ = conn.Close() }()

	out := AbuseOutcome{Family: family}
	switch family {
	case AbuseBadHandshake:
		a.abuseBadHandshake(conn, &out)
	case AbuseNoCommonVersion:
		a.abuseNoCommonVersion(conn, &out)
	case AbuseTruncatedChunk:
		a.abuseTruncatedChunk(conn, &out)
	case AbuseOversizedChunk:
		a.abuseOversizedChunk(conn, &out)
	case AbusePullBeforeRun:
		a.abusePullBeforeRun(conn, &out)
	case AbuseRunBeforeLogon:
		a.abuseRunBeforeLogon(conn, &out)
	case AbuseGarbageOpcode:
		a.abuseGarbageOpcode(conn, &out)
	case AbuseDuplicateHello:
		a.abuseDuplicateHello(conn, &out)
	default:
		return out, fmt.Errorf("sim: unknown abuse family %d", int(family))
	}
	return out, nil
}

// classifyClose marks the outcome as a clean close when err indicates the peer
// closed or the connection became unreadable (EOF, reset). A nil err is not a
// close.
func classifyClose(err error, out *AbuseOutcome) {
	if err == nil {
		return
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, ErrSimConnClosed) {
		out.GotClose = true
	}
}

// readTerminal reads one framed response message and classifies it as a FAILURE
// or, on a read error, a clean close.
func (BoltAbuser) readTerminal(conn *SimConn, out *AbuseOutcome) {
	cr := proto.NewChunkedReader(conn)
	raw, err := cr.ReadMessage()
	if err != nil {
		classifyClose(err, out)
		return
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		// A garbage or undecodable terminal is treated as a close-class outcome:
		// the server did not hand us a usable SUCCESS, which is acceptable.
		out.GotClose = true
		return
	}
	if f, ok := msg.(*proto.Failure); ok {
		out.GotFailure = true
		out.FailureMsg = f.Code + ": " + f.Message
	}
}

// writeHandshake writes the standard 20-byte preamble offering 5.6..5.0 and
// reads the 4-byte response, returning the negotiated version bytes and whether
// negotiation succeeded.
func (BoltAbuser) writeHandshake(conn *SimConn) (ok bool, readErr error) {
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	buf[4], buf[5], buf[6], buf[7] = 0, 6, 6, 5
	if _, err := conn.Write(buf[:]); err != nil {
		return false, err
	}
	var resp [4]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return false, err
	}
	return resp[2] != 0 || resp[3] != 0, nil
}

// helloLogon performs HELLO then (for 5.1+) LOGON over a freshly-negotiated
// connection, so the abuser can reach an authenticated state before sending a
// wrong-state message. It returns the chunked reader/writer for further use.
func (a BoltAbuser) helloLogon(conn *SimConn) (*proto.ChunkedReader, *proto.ChunkedWriter, error) {
	ok, err := a.writeHandshake(conn)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("sim: handshake rejected")
	}
	cr := proto.NewChunkedReader(conn)
	cw := proto.NewChunkedWriter(conn)
	hello := &proto.Hello{Extra: map[string]packstream.Value{
		"scheme": "none", "principal": "sim", "credentials": "", "user_agent": "gograph-sim/3.0",
	}}
	if err := writeFramed(cw, hello); err != nil {
		return nil, nil, err
	}
	if _, err := cr.ReadMessage(); err != nil {
		return nil, nil, err
	}
	// Always send LOGON: on 5.1+ it completes auth; on the negotiated 5.6 it is
	// required. (The negotiated version is 5.x>=1 because writeHandshake offers
	// 5.6 first.)
	logon := &proto.Logon{Auth: map[string]packstream.Value{"scheme": "none"}}
	if err := writeFramed(cw, logon); err != nil {
		return nil, nil, err
	}
	if _, err := cr.ReadMessage(); err != nil {
		return nil, nil, err
	}
	return cr, cw, nil
}

// ── abuse families ──────────────────────────────────────────────────────────

func (BoltAbuser) abuseBadHandshake(conn *SimConn, out *AbuseOutcome) {
	// Wrong magic preamble: the server must reject and close without reading a
	// message loop.
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], 0xDEADBEEF) // not proto.Magic
	if _, err := conn.Write(buf[:]); err != nil {
		classifyClose(err, out)
		return
	}
	// The server closes on a bad preamble; the read returns EOF/close.
	var resp [4]byte
	_, err := io.ReadFull(conn, resp[:])
	classifyClose(err, out)
}

func (a BoltAbuser) abuseNoCommonVersion(conn *SimConn, out *AbuseOutcome) {
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	// Offer only Bolt 9.9 in every slot — unsupported.
	for slot := 0; slot < 4; slot++ {
		off := 4 + slot*4
		buf[off], buf[off+1], buf[off+2], buf[off+3] = 0, 0, 9, 9
	}
	if _, err := conn.Write(buf[:]); err != nil {
		classifyClose(err, out)
		return
	}
	var resp [4]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		classifyClose(err, out)
		return
	}
	// A 0.0 response is the negotiation-failure signal; the server then closes.
	if resp[2] == 0 && resp[3] == 0 {
		out.GotClose = true
	}
}

func (a BoltAbuser) abuseTruncatedChunk(conn *SimConn, out *AbuseOutcome) {
	if ok, err := a.writeHandshake(conn); err != nil || !ok {
		classifyClose(err, out)
		return
	}
	// Advertise a 100-byte chunk but send only 10 bytes, then close. The server's
	// framed reader blocks for the rest; our close delivers EOF mid-chunk, which
	// it must treat as a clean disconnect (not a panic).
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], 100)
	_, _ = conn.Write(hdr[:])
	_, _ = conn.Write(make([]byte, 10))
	_ = conn.Close()
	// We do not expect a response; the close is the (acceptable) outcome and the
	// assertion is purely that the server did not panic/leak (checked by goleak).
	out.GotClose = true
}

func (a BoltAbuser) abuseOversizedChunk(conn *SimConn, out *AbuseOutcome) {
	if ok, err := a.writeHandshake(conn); err != nil || !ok {
		classifyClose(err, out)
		return
	}
	cw := proto.NewChunkedWriter(conn)
	// A single message larger than the server's DefaultMaxMessageBytes (16 MiB)
	// must be rejected by the framed reader's cumulative-size guard. The payload
	// is bounded by the harness (17 MiB) so the abuser itself stays bounded.
	payload := make([]byte, 17<<20)
	// Make the first byte a plausible struct header so the reader is reading a
	// "message"; the size guard trips before decode regardless.
	payload[0] = 0xB1
	payload[1] = proto.TagHello
	_ = cw.WriteMessage(payload)
	a.readTerminal(conn, out)
}

func (a BoltAbuser) abusePullBeforeRun(conn *SimConn, out *AbuseOutcome) {
	cr, cw, err := a.helloLogon(conn)
	if err != nil {
		classifyClose(err, out)
		return
	}
	// PULL with no preceding RUN: illegal in READY state.
	if err := writeFramed(cw, &proto.Pull{N: -1, QID: -1}); err != nil {
		classifyClose(err, out)
		return
	}
	a.readTerminalWith(cr, out)
}

func (a BoltAbuser) abuseRunBeforeLogon(conn *SimConn, out *AbuseOutcome) {
	ok, err := a.writeHandshake(conn)
	if err != nil || !ok {
		classifyClose(err, out)
		return
	}
	cr := proto.NewChunkedReader(conn)
	cw := proto.NewChunkedWriter(conn)
	// RUN before HELLO/LOGON: illegal pre-auth.
	run := &proto.Run{Query: "RETURN 1", Parameters: map[string]packstream.Value{}, Extra: map[string]packstream.Value{}}
	if err := writeFramed(cw, run); err != nil {
		classifyClose(err, out)
		return
	}
	a.readTerminalWith(cr, out)
}

func (a BoltAbuser) abuseGarbageOpcode(conn *SimConn, out *AbuseOutcome) {
	ok, err := a.writeHandshake(conn)
	if err != nil || !ok {
		classifyClose(err, out)
		return
	}
	cw := proto.NewChunkedWriter(conn)
	// A correctly-framed message carrying an unknown struct tag (0x55): valid
	// framing, garbage opcode. The server must reject it, not panic.
	garbage := []byte{0xB0, 0x55} // struct header (tiny-struct, 0 fields) + unknown tag 0x55
	_ = cw.WriteMessage(garbage)
	a.readTerminal(conn, out)
}

func (a BoltAbuser) abuseDuplicateHello(conn *SimConn, out *AbuseOutcome) {
	cr, cw, err := a.helloLogon(conn)
	if err != nil {
		classifyClose(err, out)
		return
	}
	// A second HELLO after the session already authenticated is an illegal
	// transition.
	hello := &proto.Hello{Extra: map[string]packstream.Value{"scheme": "none"}}
	if err := writeFramed(cw, hello); err != nil {
		classifyClose(err, out)
		return
	}
	a.readTerminalWith(cr, out)
}

// readTerminalWith reads one framed response from an existing reader and
// classifies it (FAILURE vs close), like readTerminal but reusing cr.
func (BoltAbuser) readTerminalWith(cr *proto.ChunkedReader, out *AbuseOutcome) {
	raw, err := cr.ReadMessage()
	if err != nil {
		classifyClose(err, out)
		return
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		out.GotClose = true
		return
	}
	if f, ok := msg.(*proto.Failure); ok {
		out.GotFailure = true
		out.FailureMsg = f.Code + ": " + f.Message
	}
}

// writeFramed encodes msg as a PackStream request and writes it as one chunked
// Bolt message via cw.
func writeFramed(cw *proto.ChunkedWriter, msg any) error {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		return err
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	return cw.WriteMessage(buf.Bytes())
}

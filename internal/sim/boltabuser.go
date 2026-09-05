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
	// AbuseLogoffThenRun authenticates, sends LOGOFF, then sends a WRITE RUN on
	// the de-authorised connection. The CWE-306 gate must refuse it.
	AbuseLogoffThenRun
	// AbuseCommitAfterLogoff authenticates, opens an explicit transaction with a
	// write in it, sends LOGOFF (legal from TX_READY, and it leaves the session in
	// TX_READY but unauthenticated), then sends COMMIT. The transaction-finalising
	// gate must refuse it, so the write never becomes durable.
	AbuseCommitAfterLogoff
	// AbuseBadCredentials presents a WRONG password on the authenticating message
	// and then attempts a write. It is the one family that needs a server whose
	// AuthHandler actually validates credentials — see [AbuseFamily.NeedsCredentialAuth].
	AbuseBadCredentials
)

// abuseAnyServerFamilyCount is the number of families that any SimServer must
// refuse, whatever its [github.com/FlavioCFOliveira/GoGraph/bolt/server.AuthHandler]
// is: the wire-protocol violations plus the two post-LOGOFF gates, which are
// enforced by the session's own `authenticated` flag and so bite even under
// [github.com/FlavioCFOliveira/GoGraph/bolt/server.NoAuthHandler]. The families
// are ordered so these occupy [0, abuseAnyServerFamilyCount).
const abuseAnyServerFamilyCount = 10

// abuseFamilyCount is the number of distinct abuse families, credential-dependent
// ones included. It MUST equal the number of AbuseFamily constants, which
// TestRenderersCoverEveryValue pins through [AbuseFamily.String]. Acceptability is
// asserted per family, but in two places rather than one: the loops in
// boltabuser_test.go and phase3_soak_test.go cover [0, abuseAnyServerFamilyCount)
// against a NoAuth server, and TestBoltAbuser_CredentialFamilyNeedsRealAuth covers
// the credential-dependent remainder against a validating one.
const abuseFamilyCount = 11

// NeedsCredentialAuth reports whether driving this family proves anything only
// against a server whose AuthHandler rejects a wrong credential. Against
// NoAuthHandler, [AbuseBadCredentials] is admitted — correctly, since that
// handler admits everything — so a battery that ran it there would be asserting
// the absence of a check nobody installed.
func (f AbuseFamily) NeedsCredentialAuth() bool { return f >= abuseAnyServerFamilyCount }

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
	case AbuseLogoffThenRun:
		return "LogoffThenRun"
	case AbuseCommitAfterLogoff:
		return "CommitAfterLogoff"
	case AbuseBadCredentials:
		return "BadCredentials"
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
	FailureMsg string // populated when GotFailure
	Family     AbuseFamily
	GotFailure bool // server replied with a typed FAILURE
	GotClose   bool // server closed the connection cleanly (or it became unreadable)
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
// the workload draw stream is stable. Only the families every server refuses are
// drawn (see [AbuseFamily.NeedsCredentialAuth]): the random abuser runs against
// the NoAuth SimServer the bad-actors scenario builds, where a wrong-credential
// attempt is legitimately admitted and would be scored as a violation of nothing.
func (BoltAbuser) PickFamily(seed *Seed) AbuseFamily {
	return AbuseFamily(seed.IntN(abuseAnyServerFamilyCount))
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
	case AbuseLogoffThenRun:
		a.abuseLogoffThenRun(conn, &out)
	case AbuseCommitAfterLogoff:
		a.abuseCommitAfterLogoff(conn, &out)
	case AbuseBadCredentials:
		a.abuseBadCredentials(conn, &out)
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

// ── post-LOGOFF and credential families (rmp #2481) ─────────────────────────

// simAuthPrincipal / simAuthPassword are the sole credentials the auth-surface
// scenario's server accepts, and simAuthWrongPassword the one it must refuse.
// They live here, next to the abuse families that present them, so the abuser
// needs no configuration to be driven against [NewSimServerAuth].
const (
	simAuthPrincipal     = "sim-operator"
	simAuthPassword      = "correct-horse-battery-staple"
	simAuthWrongPassword = "correct-horse-battery-stapl" // G101: a deliberately WRONG credential for a refusal probe.
)

// abuseGhostLabel is the label the post-LOGOFF write families try to create. A
// node carrying it must never exist and its CREATE must never reach the WAL: it
// is the sentinel the auth oracle looks for (see [checkBoltAuthSurface]).
const abuseGhostLabel = "AbuseGhost"

// abuseGhostCreate is the write statement the de-authorised families attempt.
const abuseGhostCreate = "CREATE (:" + abuseGhostLabel + " {n: 1})"

// abuseWriteRun builds the RUN carrying [abuseGhostCreate].
func abuseWriteRun() *proto.Run {
	return &proto.Run{
		Query:      abuseGhostCreate,
		Parameters: map[string]packstream.Value{},
		Extra:      map[string]packstream.Value{},
	}
}

// exchange writes one request and decodes exactly one response message. It is the
// lock-step primitive the multi-step families are built from: every step's reply
// is consumed before the next request is sent, so a given family always produces
// the same wire trace.
func (BoltAbuser) exchange(cr *proto.ChunkedReader, cw *proto.ChunkedWriter, msg any) (any, error) {
	if err := writeFramed(cw, msg); err != nil {
		return nil, err
	}
	raw, err := cr.ReadMessage()
	if err != nil {
		return nil, err
	}
	return proto.DecodeResponse(packstream.NewDecoder(bytes.NewReader(raw)))
}

// drainPull sends PULL(-1) and consumes every RECORD up to the terminal message,
// which it returns. A stream left un-drained would leave the session in
// STREAMING, where the next request is refused for the wrong reason.
func (a BoltAbuser) drainPull(cr *proto.ChunkedReader, cw *proto.ChunkedWriter) (any, error) {
	resp, err := a.exchange(cr, cw, &proto.Pull{N: -1, QID: -1})
	if err != nil {
		return nil, err
	}
	for {
		if _, isRecord := resp.(*proto.Record); !isRecord {
			return resp, nil
		}
		raw, err := cr.ReadMessage()
		if err != nil {
			return nil, err
		}
		if resp, err = proto.DecodeResponse(packstream.NewDecoder(bytes.NewReader(raw))); err != nil {
			return nil, err
		}
	}
}

// classifyAbuseResponse records a decoded response in out: a FAILURE is the
// expected refusal, anything else leaves the outcome unacceptable so the caller's
// checker reports it.
func classifyAbuseResponse(resp any, out *AbuseOutcome) {
	if f, ok := resp.(*proto.Failure); ok {
		out.GotFailure = true
		out.FailureMsg = f.Code + ": " + f.Message
	}
}

// abuseLogoffThenRun authenticates, de-authorises with LOGOFF, then attempts a
// WRITE. The session-level authentication gate (CWE-306, task #1345) must refuse
// the RUN even though the state machine left the connection in READY.
func (a BoltAbuser) abuseLogoffThenRun(conn *SimConn, out *AbuseOutcome) {
	cr, cw, err := a.helloLogon(conn)
	if err != nil {
		classifyClose(err, out)
		return
	}
	logoffResp, err := a.exchange(cr, cw, &proto.Logoff{})
	if err != nil {
		classifyClose(err, out)
		return
	}
	if _, ok := logoffResp.(*proto.Success); !ok {
		// LOGOFF itself is legal from READY; a refusal here is a defect, and
		// leaving the outcome unacceptable is how it is reported.
		return
	}
	resp, err := a.exchange(cr, cw, abuseWriteRun())
	if err != nil {
		classifyClose(err, out)
		return
	}
	classifyAbuseResponse(resp, out)
}

// abuseCommitAfterLogoff opens an explicit transaction holding a write, then
// LOGOFFs (legal from TX_READY, leaving the session in TX_READY but
// unauthenticated) and attempts to COMMIT. The transaction-finalising gate must
// refuse it, so the write never becomes durable.
func (a BoltAbuser) abuseCommitAfterLogoff(conn *SimConn, out *AbuseOutcome) {
	cr, cw, err := a.helloLogon(conn)
	if err != nil {
		classifyClose(err, out)
		return
	}
	for _, step := range []any{&proto.Begin{Extra: map[string]packstream.Value{}}, abuseWriteRun()} {
		resp, stepErr := a.exchange(cr, cw, step)
		if stepErr != nil {
			classifyClose(stepErr, out)
			return
		}
		if _, ok := resp.(*proto.Success); !ok {
			// BEGIN and the in-transaction RUN are both legal for an authenticated
			// session; a refusal means the pre-condition of this family was never
			// reached, and the unacceptable outcome reports it.
			return
		}
	}
	if term, drainErr := a.drainPull(cr, cw); drainErr != nil {
		classifyClose(drainErr, out)
		return
	} else if _, ok := term.(*proto.Success); !ok {
		return
	}
	if logoffResp, logoffErr := a.exchange(cr, cw, &proto.Logoff{}); logoffErr != nil {
		classifyClose(logoffErr, out)
		return
	} else if _, ok := logoffResp.(*proto.Success); !ok {
		return
	}
	resp, err := a.exchange(cr, cw, &proto.Commit{})
	if err != nil {
		classifyClose(err, out)
		return
	}
	classifyAbuseResponse(resp, out)
}

// abuseBadCredentials presents [simAuthWrongPassword] on the authenticating
// message. A FIRST authentication that fails terminates the connection, so the
// acceptable outcome is the FAILURE (Unauthorized) and then a close; a SUCCESS
// means the server admitted a wrong password.
//
// It is meaningful ONLY against a server whose AuthHandler validates credentials
// (see [AbuseFamily.NeedsCredentialAuth]).
func (a BoltAbuser) abuseBadCredentials(conn *SimConn, out *AbuseOutcome) {
	ok, err := a.writeHandshake(conn)
	if err != nil || !ok {
		classifyClose(err, out)
		return
	}
	cr := proto.NewChunkedReader(conn)
	cw := proto.NewChunkedWriter(conn)
	// The negotiated version is 5.6 (writeHandshake offers it first), so auth is
	// deferred to LOGON: HELLO carries only driver metadata.
	helloResp, err := a.exchange(cr, cw, &proto.Hello{Extra: map[string]packstream.Value{
		"user_agent": "gograph-sim/3.0",
	}})
	if err != nil {
		classifyClose(err, out)
		return
	}
	if _, isSuccess := helloResp.(*proto.Success); !isSuccess {
		classifyAbuseResponse(helloResp, out)
		return
	}
	resp, err := a.exchange(cr, cw, &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": simAuthPrincipal, "credentials": simAuthWrongPassword,
	}})
	if err != nil {
		classifyClose(err, out)
		return
	}
	classifyAbuseResponse(resp, out)
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

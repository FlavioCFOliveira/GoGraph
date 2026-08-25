package sim

// bolt_auth_surface.go — the Bolt authentication surface under simulation
// (rmp #2481).
//
// # The gap this closes
//
// Every SimServer in the harness was built with
// [github.com/FlavioCFOliveira/GoGraph/bolt/server.NoAuthHandler], a handler that
// admits every client by construction. So the credential surface was driven by no
// DST scenario at all: a wrong password was never presented, LOGOFF was never
// sent, re-authentication never happened, and the no-statement-without-LOGON
// invariant — the CWE-306 gate that `bolt/server/session.go` enforces on RUN,
// BEGIN, COMMIT, ROLLBACK and ROUTE — had no wire-level oracle. Worse, an
// assertion made against NoAuthHandler is not merely weak, it is vacuous: it
// would be asserting the absence of a check nobody installed.
//
// This scenario therefore builds its server with a REAL validator
// ([NewSimServerAuth] + BasicAuthHandler over
// [github.com/FlavioCFOliveira/GoGraph/bolt/server.ConstantTimeValidate]) and
// drives the whole surface over the genuine wire in lock-step.
//
// # The oracle: the WAL is the witness, not the reply
//
// A refusal that is only observed as a FAILURE message proves the server SAID no.
// It does not prove nothing happened. The load-bearing oracle here is therefore
// the durable log: the server is backed by a real [SimStore] — a WAL-backed
// [github.com/FlavioCFOliveira/GoGraph/store/txn.Store] over a [SimDisk] — and
// every arm is bracketed between two readings of
// [github.com/FlavioCFOliveira/GoGraph/store/wal.Writer.Stats]. A refused arm must
// leave the frame and byte counters EXACTLY where it found them, and the sentinel
// node its statement would have created must be absent from the graph.
//
// That oracle is worthless unless the counter can move, so two arms are ADMIT
// arms: an honest authenticated write, and a write after re-authenticating
// following LOGOFF. Both must advance the WAL counters. This is the
// assert-something-was-seen discipline: the same instrument that reports "no
// frames appended" is shown, in the same run, reporting frames appended.
//
// # The one thing the seed does not reach: WAL BYTE totals
//
// The frame COUNT is a pure function of the seed; the byte count is not, and the
// difference was measured rather than assumed. A created node's hidden internal
// key is minted by cypher/exec as "__cx_"+hex(n) from a PROCESS-GLOBAL counter
// (see cypher/exec/create_node.go), and that key travels inside the WAL frame. Its
// hex width therefore depends on how many nodes every OTHER test in the process
// created first, so the same seed can produce frames of different sizes: measured
// 183 bytes for the honest write when the run stood alone and 186 when it followed
// the rest of the package, because the key gained one hex digit in each of three
// frames.
//
// Two consequences, both deliberate. The refused arms still assert bytes+0 — zero
// is zero at any width — so the load-bearing oracle is untouched. And the
// determinism test compares every seed-reachable field but skips the byte delta of
// an ADMIT arm, because asserting it would be asserting the process history. The
// same limitation, from the same counter, is documented for the schema-mutation
// scenario in schema_mutation.go.
//
// # Both authentication entry points
//
// The credentials ride on different messages depending on the negotiated
// version, and the two are separate code paths in the server (`handleHello`
// authenticates inline, `handleLogon` authenticates deferred). Covering one would
// leave half the surface untested, so the wrong-password probe runs twice: once
// on a negotiated 5.6 (LOGON) and once on a negotiated 4.4 (HELLO).

import (
	"context"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// Bolt failure codes the auth surface must produce. They are spelled out here so
// the oracle asserts the EXACT code a driver sees, not merely "some failure":
// mapping an unknown scheme onto Unauthorized (or a de-authorised RUN onto a
// security code) would change what a client is told and is a defect the
// generic-failure assertion would miss.
const (
	// authCodeUnauthorized is returned for a credential that does not validate.
	authCodeUnauthorized = "Neo.ClientError.Security.Unauthorized"
	// authCodeProviderFailed is returned for an auth scheme the handler does not
	// implement.
	authCodeProviderFailed = "Neo.ClientError.Security.AuthProviderFailed"
	// authCodeRequestInvalid is returned by the session's illegal-transition path,
	// which is what the unauthenticated RUN/BEGIN/COMMIT/ROLLBACK gates use.
	authCodeRequestInvalid = "Neo.ClientError.Request.Invalid"
)

// simAuthUnknownScheme is a scheme no handler in the module implements, so
// BasicAuthHandler must answer [server.ErrSchemeUnknown] rather than treating it
// as a bad password.
const simAuthUnknownScheme = "kerberos"

// authHonestLabel is the label the ADMIT arms write under. It is distinct from
// [abuseGhostLabel] so the oracle can tell "the write that must never happen"
// from "the write that proves the instrument works" by label alone.
const authHonestLabel = "AuthHonest"

// boltAuthCreate builds the write statement an arm attempts under the given
// label, parameterised by name so each arm writes a distinguishable node.
func boltAuthCreate(label, name string) string {
	return fmt.Sprintf("CREATE (:%s {name: %q})", label, name)
}

// BoltAuthArm is one probe of the authentication surface: what it did, whether
// the server admitted it, the failure code it was told, and the WAL counters
// bracketing it.
type BoltAuthArm struct {
	// Name identifies the arm in a violation message.
	Name string
	// WantCode is the failure code a REFUSE arm must be told. It is empty for an
	// ADMIT arm.
	WantCode string
	// GotCode is the failure code actually received ("" when none was).
	GotCode string
	// Detail carries any harness-level note (e.g. the connection closed before a
	// reply, which is an acceptable refusal shape for a terminated session).
	Detail string
	// GotMessage is the FAILURE's message text, and WantStateInMessage the session
	// state the refusal must name.
	//
	// They exist because the authentication gate and the state-machine gate return
	// the SAME code — failTransition's Neo.ClientError.Request.Invalid — so a code
	// match cannot tell which one refused. That matters: if LOGOFF's target state
	// regressed (state.go's TX_READY/READY cases), commit-after-logoff would be
	// refused by the STATE check one line above the auth check, the arm would still
	// see Request.Invalid, and the CWE-306 gate would be untested while the
	// scenario stayed green. failTransition reports the ORIGIN state in its
	// message, which is exactly the discriminator: a refusal by the auth gate names
	// the LEGAL state the session was in (READY / TX_READY), a refusal by the state
	// gate names FAILED or the wrong state. An empty WantStateInMessage waives the
	// check for arms whose refusal is not a failTransition (a credential rejection).
	GotMessage         string
	WantStateInMessage string
	// FramesBefore/FramesAfter bracket the arm with [wal.Stats.Frames].
	FramesBefore, FramesAfter uint64
	// BytesBefore/BytesAfter bracket the arm with [wal.Stats.Bytes].
	BytesBefore, BytesAfter uint64
	// Admit records the arm's INTENT: true when the server is expected to accept
	// the operation and append to the WAL, false when it must refuse it and
	// append nothing.
	Admit bool
	// Accepted records what actually happened: the server answered SUCCESS to the
	// arm's decisive message.
	Accepted bool
}

// framesAppended reports how many WAL frames the arm appended. The receiver is a
// pointer because a BoltAuthArm is 104 bytes and these are called per arm in the
// adjudicator and the renderer.
func (a *BoltAuthArm) framesAppended() uint64 { return a.FramesAfter - a.FramesBefore }

// bytesAppended reports how many WAL bytes the arm appended.
func (a *BoltAuthArm) bytesAppended() uint64 { return a.BytesAfter - a.BytesBefore }

// BoltAuthEvidence is everything one [RunBoltAuthSurface] observed. It is a plain
// value so the checkers are pure functions of it — which is what lets a test
// perturb one field and prove the corresponding check fires.
type BoltAuthEvidence struct {
	// Arms are the probes in the order they ran.
	Arms []BoltAuthArm
	// GhostNodes is how many nodes carrying [abuseGhostLabel] exist at the end:
	// the count of writes that a de-authorised or unauthenticated session managed
	// to apply. It must be zero.
	GhostNodes int
	// HonestNodes is how many nodes carrying [authHonestLabel] exist at the end:
	// the writes the properly authenticated arms committed. It must equal the
	// number of ADMIT arms, which is what proves the harness could write at all.
	HonestNodes int
	// RecoveredGhosts / RecoveredHonest are the same two censuses taken after a
	// crash and a real WAL replay, so a frame that was appended but not yet
	// visible in the live engine cannot hide from the oracle.
	RecoveredGhosts int
	RecoveredHonest int
	// Seed is the seed the run was built from.
	Seed uint64
}

// boltAuthValidator is the handler the scenario's server authenticates through:
// exactly one principal and one password, compared in constant time.
func boltAuthValidator() server.AuthHandler {
	return server.BasicAuthHandler{
		Validate: server.ConstantTimeValidate(simAuthPrincipal, simAuthPassword),
	}
}

// boltAuthSeedMix decorrelates the SimDisk sub-seed from the scenario seed, so
// the disk's fault draw stream is independent of anything else derived from it.
const boltAuthSeedMix = 0xA1_7B_0C_5E

// RunBoltAuthSurface drives the whole authentication surface once against a
// WAL-backed server whose AuthHandler validates credentials, and returns the
// evidence. It is bit-reproducible from seed: every arm is a fixed lock-step
// script on its own connection, and the only seeded component is the SimDisk the
// WAL lives on.
//
// The returned error is reserved for harness failures (the store would not open,
// the listener refused a dial); a refused credential or a rejected statement is
// EVIDENCE, not an error.
func RunBoltAuthSurface(ctx context.Context, seed uint64) (BoltAuthEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^boltAuthSeedMix), 0) // faultRate 0: this scenario faults nothing
	cfg := durableStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return BoltAuthEvidence{}, fmt.Errorf("sim: bolt-auth open store: %w", err)
	}
	srv, err := NewSimServerAuth(st.Engine(), clock.Real(), boltAuthValidator())
	if err != nil {
		_ = st.Close()
		return BoltAuthEvidence{}, fmt.Errorf("sim: bolt-auth server: %w", err)
	}

	ev := BoltAuthEvidence{Seed: seed}
	runner := &boltAuthRunner{srv: srv, st: st, ev: &ev}
	if err := runner.driveAll(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return BoltAuthEvidence{}, err
	}

	// Census the live engine before tearing anything down.
	ghosts, honest, err := boltAuthCensus(ctx, srv)
	if err != nil {
		_ = srv.Close()
		st.Crash()
		return BoltAuthEvidence{}, err
	}
	ev.GhostNodes, ev.HonestNodes = ghosts, honest

	// Then crash (drop the engine, keep the SimDisk image) and reopen through
	// real recovery. A gate that leaked a frame into the WAL without making it
	// visible in the live engine would be invisible to the live census and
	// surface only here.
	_ = srv.Close()
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return BoltAuthEvidence{}, fmt.Errorf("sim: bolt-auth reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	rGhosts, rHonest, err := boltAuthCensusEngine(ctx, st2)
	if err != nil {
		return BoltAuthEvidence{}, err
	}
	ev.RecoveredGhosts, ev.RecoveredHonest = rGhosts, rHonest
	return ev, nil
}

// boltAuthRunner threads the server, the store and the accumulating evidence
// through the arms. It exists so each arm is a short method that brackets itself
// with the WAL counters and cannot forget to.
type boltAuthRunner struct {
	srv *SimServer
	st  *SimStore
	ev  *BoltAuthEvidence
}

// walCounters reads the live WAL frame/byte counters.
func (r *boltAuthRunner) walCounters() (frames, bytes uint64) {
	s := r.st.WAL().Stats()
	return s.Frames, s.Bytes
}

// arm runs one probe, bracketing it between two readings of the WAL counters and
// appending the result to the evidence. body reports whether the server accepted
// the arm's decisive message, plus the failure code it was told and any note.
func (r *boltAuthRunner) arm(name, wantCode, wantState string, admit bool, body func() (accepted bool, code, message, detail string, err error)) error {
	a := BoltAuthArm{Name: name, WantCode: wantCode, WantStateInMessage: wantState, Admit: admit}
	a.FramesBefore, a.BytesBefore = r.walCounters()
	accepted, code, message, detail, err := body()
	if err != nil {
		return fmt.Errorf("sim: bolt-auth arm %q: %w", name, err)
	}
	a.FramesAfter, a.BytesAfter = r.walCounters()
	a.Accepted, a.GotCode, a.GotMessage, a.Detail = accepted, code, message, detail
	r.ev.Arms = append(r.ev.Arms, a)
	return nil
}

// driveAll runs every arm in a fixed order. REFUSE arms come first so any frame
// they leaked is attributed to them and not masked by a later honest write.
func (r *boltAuthRunner) driveAll(ctx context.Context) error {
	steps := []func(context.Context) error{
		r.armWrongPasswordLogon,
		r.armWrongPasswordHelloInline,
		r.armUnknownScheme,
		r.armRunBeforeLogon,
		r.armLogoffThenRun,
		r.armCommitAfterLogoff,
		r.armRollbackAfterLogoff,
		r.armReauthWrongPassword,
		r.armRouteAfterLogoff,
		r.armLogoffInTxStreaming,
		r.armResetAfterLogoffWithOpenTx,
		r.armSecondMessageAfterRefusal,
		// ADMIT arms last: they are the non-vacuity witnesses.
		r.armHonestWrite,
		r.armReauthThenWrite,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// dial opens a client and negotiates nothing yet. The caller closes it.
func (r *boltAuthRunner) dial() (*WireClient, error) { return r.srv.Dial() }

// failureCode extracts the code from a response, returning "" when the response
// is not a FAILURE.
func failureCode(resp any) string {
	if f, ok := resp.(*proto.Failure); ok {
		return f.Code
	}
	return ""
}

// failureMessage extracts the message from a response, returning "" when the
// response is not a FAILURE. It is what makes a refusal attributable to a
// specific gate: see [BoltAuthArm.WantStateInMessage].
func failureMessage(resp any) string {
	if f, ok := resp.(*proto.Failure); ok {
		return f.Message
	}
	return ""
}

// isSuccess reports whether resp is a SUCCESS.
func isSuccess(resp any) bool {
	_, ok := resp.(*proto.Success)
	return ok
}

// isIgnored reports whether resp is an IGNORED.
//
// IGNORED is the Bolt-correct reply to a request-phase message arriving on a
// FAILED session, and `bolt/server/session.go`'s dispatch emits it for exactly
// that case. It must therefore be classified as a REFUSAL: treating "not a
// SUCCESS and not a FAILURE" as "the statement executed" would make the DST
// report an ACID violation for behaviour the protocol prescribes. The arms here
// do not currently reach it — the server gates that branch on `s.authenticated`,
// which every de-authorised arm has cleared — so this is a guard against a false
// positive the next widening of that gate would otherwise produce.
func isIgnored(resp any) bool {
	_, ok := resp.(*proto.Ignored)
	return ok
}

// armWrongPasswordLogon presents a wrong password on LOGON (negotiated 5.6, the
// deferred-auth path) and then attempts a write on the same connection. The
// credential must be refused with Unauthorized, and the follow-on write must not
// reach the WAL: a first-authentication failure makes the session DEFUNCT, so the
// write is answered by a close rather than a reply, which is why the arm accepts
// either shape as the refusal and pins the CODE on the credential message.
func (r *boltAuthRunner) armWrongPasswordLogon(ctx context.Context) error {
	return r.arm("logon-wrong-password", authCodeUnauthorized, "", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthNegotiateDeferred(ctx, c); err != nil {
			return false, "", "", "", err
		}
		resp, err := c.ConnectAs(ctx, simAuthPrincipal, simAuthWrongPassword)
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ADMITTED a wrong password", nil
		}
		// The session is DEFUNCT; the write attempt must not be executed. Its
		// outcome is recorded as a note, never as the arm's verdict.
		detail := "write after refused credential: "
		if _, runErr := c.Run(boltAuthCreate(abuseGhostLabel, "logon-wrong-password"), nil); runErr != nil {
			detail += "connection closed"
		} else if _, term, pullErr := c.PullAll(); pullErr != nil {
			detail += "connection closed mid-stream"
		} else if code := failureCode(term); code != "" {
			detail += code
		} else if isIgnored(term) {
			detail += "IGNORED (session is FAILED)"
		} else {
			return true, "", "", "server EXECUTED a write after refusing the credential", nil
		}
		return false, failureCode(resp), failureMessage(resp), detail, nil
	})
}

// armWrongPasswordHelloInline presents a wrong password on HELLO over a
// negotiated Bolt 4.4, the inline-auth path `handleHello` takes. It is a distinct
// server code path from LOGON and must reach the same verdict.
func (r *boltAuthRunner) armWrongPasswordHelloInline(ctx context.Context) error {
	return r.arm("hello-inline-wrong-password", authCodeUnauthorized, "", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		ver, err := c.HandshakeOffering(ctx, proto.Version{Major: 4, Minor: 4})
		if err != nil {
			return false, "", "", "", err
		}
		if ver.Major != 4 || ver.Minor != 4 {
			return false, "", "", "", fmt.Errorf("negotiated %d.%d, want 4.4", ver.Major, ver.Minor)
		}
		resp, err := c.ConnectAs(ctx, simAuthPrincipal, simAuthWrongPassword)
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ADMITTED a wrong password on inline HELLO auth", nil
		}
		// The inline path sets StateDefunct too, so the write must not execute. Its
		// outcome is a note, never the arm's verdict — the verdict is the credential's.
		detail := "inline HELLO auth refused; write after it: "
		if _, runErr := c.Run(boltAuthCreate(abuseGhostLabel, "hello-inline-wrong-password"), nil); runErr != nil {
			detail += "connection closed"
		} else if _, term, pullErr := c.PullAll(); pullErr != nil {
			detail += "connection closed mid-stream"
		} else if code := failureCode(term); code != "" {
			detail += code
		} else if isIgnored(term) {
			detail += "IGNORED (session is FAILED)"
		} else {
			return true, "", "", "server EXECUTED a write after refusing an inline HELLO credential", nil
		}
		return false, failureCode(resp), failureMessage(resp), detail, nil
	})
}

// armUnknownScheme presents a scheme the handler does not implement. It must be
// told AuthProviderFailed — a different code from a bad password, because a
// driver distinguishes "your provider is misconfigured" from "your password is
// wrong".
func (r *boltAuthRunner) armUnknownScheme(ctx context.Context) error {
	return r.arm("logon-unknown-scheme", authCodeProviderFailed, "", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthNegotiateDeferred(ctx, c); err != nil {
			return false, "", "", "", err
		}
		helloResp, err := c.Hello(map[string]packstream.Value{"user_agent": "gograph-sim/3.0"})
		if err != nil {
			return false, "", "", "", err
		}
		if !isSuccess(helloResp) {
			return false, failureCode(helloResp), failureMessage(helloResp), "HELLO itself was refused", nil
		}
		resp, err := c.LogonWith(map[string]packstream.Value{
			"scheme": simAuthUnknownScheme, "principal": simAuthPrincipal, "credentials": simAuthPassword,
		})
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ADMITTED an unknown auth scheme", nil
		}
		return false, failureCode(resp), failureMessage(resp), "", nil
	})
}

// armRunBeforeLogon sends a WRITE before authenticating at all: the
// no-statement-without-LOGON invariant in its purest form.
func (r *boltAuthRunner) armRunBeforeLogon(ctx context.Context) error {
	return r.arm("run-before-logon", authCodeRequestInvalid, "state AUTHENTICATION", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthNegotiateDeferred(ctx, c); err != nil {
			return false, "", "", "", err
		}
		helloResp, err := c.Hello(map[string]packstream.Value{"user_agent": "gograph-sim/3.0"})
		if err != nil {
			return false, "", "", "", err
		}
		if !isSuccess(helloResp) {
			return false, failureCode(helloResp), failureMessage(helloResp), "HELLO itself was refused", nil
		}
		resp, err := c.Run(boltAuthCreate(abuseGhostLabel, "run-before-logon"), nil)
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ACCEPTED a RUN before LOGON", nil
		}
		return false, failureCode(resp), failureMessage(resp), "", nil
	})
}

// armLogoffThenRun authenticates, de-authorises with LOGOFF, then attempts a
// write. LOGOFF leaves the state machine in READY, so only the session's
// `authenticated` gate stands between the client and the write.
func (r *boltAuthRunner) armLogoffThenRun(ctx context.Context) error {
	return r.arm("logoff-then-run", authCodeRequestInvalid, "state READY", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		logoffResp, err := c.Logoff()
		if err != nil {
			return false, "", "", "", err
		}
		if !isSuccess(logoffResp) {
			return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF from READY was refused", nil
		}
		resp, err := c.Run(boltAuthCreate(abuseGhostLabel, "logoff-then-run"), nil)
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ACCEPTED a RUN after LOGOFF", nil
		}
		return false, failureCode(resp), failureMessage(resp), "", nil
	})
}

// armCommitAfterLogoff opens an explicit transaction holding a write, LOGOFFs
// (legal from TX_READY, and it leaves the session in TX_READY with the
// transaction still open) and attempts COMMIT. This is the arm the WAL oracle was
// built for: the write is already staged in the engine, and only the
// transaction-finalising gate stops it becoming durable.
func (r *boltAuthRunner) armCommitAfterLogoff(ctx context.Context) error {
	return r.arm("commit-after-logoff", authCodeRequestInvalid, "state TX_READY", false, func() (bool, string, string, string, error) {
		return boltAuthStagedTxThenLogoff(ctx, r, "commit-after-logoff", func(c *WireClient) (any, error) {
			return c.Commit()
		})
	})
}

// armRollbackAfterLogoff is armCommitAfterLogoff's sibling: ROLLBACK is gated the
// same way, so a de-authorised client cannot drive the transaction's other
// terminal transition either.
func (r *boltAuthRunner) armRollbackAfterLogoff(ctx context.Context) error {
	return r.arm("rollback-after-logoff", authCodeRequestInvalid, "state TX_READY", false, func() (bool, string, string, string, error) {
		return boltAuthStagedTxThenLogoff(ctx, r, "rollback-after-logoff", func(c *WireClient) (any, error) {
			return c.Rollback()
		})
	})
}

// boltAuthStagedTxThenLogoff authenticates, stages a write inside an explicit
// transaction, sends LOGOFF, then applies finish (COMMIT or ROLLBACK) and
// classifies the reply.
func boltAuthStagedTxThenLogoff(ctx context.Context, r *boltAuthRunner, name string, finish func(*WireClient) (any, error)) (bool, string, string, string, error) {
	c, err := r.dial()
	if err != nil {
		return false, "", "", "", err
	}
	defer func() { _ = c.Close() }()
	if err := boltAuthLogin(ctx, c); err != nil {
		return false, "", "", "", err
	}
	beginResp, err := c.Begin()
	if err != nil {
		return false, "", "", "", err
	}
	if !isSuccess(beginResp) {
		return false, failureCode(beginResp), failureMessage(beginResp), "BEGIN was refused for an authenticated session", nil
	}
	runResp, err := c.Run(boltAuthCreate(abuseGhostLabel, name), nil)
	if err != nil {
		return false, "", "", "", err
	}
	if !isSuccess(runResp) {
		return false, failureCode(runResp), failureMessage(runResp), "in-transaction RUN was refused for an authenticated session", nil
	}
	if _, term, err := c.PullAll(); err != nil {
		return false, "", "", "", err
	} else if !isSuccess(term) {
		return false, failureCode(term), failureMessage(term), "in-transaction PULL was refused", nil
	}
	logoffResp, err := c.Logoff()
	if err != nil {
		return false, "", "", "", err
	}
	if !isSuccess(logoffResp) {
		return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF from TX_READY was refused", nil
	}
	resp, err := finish(c)
	if err != nil {
		return false, "", "", "", err
	}
	if isSuccess(resp) {
		return true, "", "", "server FINALISED a transaction for a de-authorised session", nil
	}
	return false, failureCode(resp), failureMessage(resp), "", nil
}

// armReauthWrongPassword de-authorises a good session and then presents a WRONG
// password. This is the RE-authentication path (`firstAuth` false in
// `handleLogon`), which is a different branch from a first LOGON: it does not
// terminate the connection, it fails the session. Either way the write that
// follows must be refused.
func (r *boltAuthRunner) armReauthWrongPassword(ctx context.Context) error {
	return r.arm("reauth-wrong-password", authCodeUnauthorized, "", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		if logoffResp, err := c.Logoff(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(logoffResp) {
			return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF was refused", nil
		}
		resp, err := c.LogonWith(basicAuthToken(simAuthPrincipal, simAuthWrongPassword))
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ADMITTED a wrong password on re-authentication", nil
		}
		detail := "write after refused re-auth: "
		if _, runErr := c.Run(boltAuthCreate(abuseGhostLabel, "reauth-wrong-password"), nil); runErr != nil {
			detail += "connection closed"
		} else if _, term, pullErr := c.PullAll(); pullErr != nil {
			detail += "connection closed mid-stream"
		} else if code := failureCode(term); code != "" {
			detail += code
		} else if isIgnored(term) {
			detail += "IGNORED (session is FAILED)"
		} else {
			return true, "", "", "server EXECUTED a write after a refused re-authentication", nil
		}
		return false, failureCode(resp), failureMessage(resp), detail, nil
	})
}

// armRouteAfterLogoff completes the roster of auth-gated verbs. ROUTE is the
// fifth verb the CWE-306 gate covers (RUN, BEGIN, COMMIT, ROLLBACK, ROUTE) and it
// is the ONE whose violation neither the WAL counter nor the ghost census could
// ever see: a leaked ROUTE writes nothing. It therefore needs its own assertion,
// or the invariant this file names is covered four-fifths and claimed whole.
func (r *boltAuthRunner) armRouteAfterLogoff(ctx context.Context) error {
	return r.arm("route-after-logoff", authCodeRequestInvalid, "state READY", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		if logoffResp, err := c.Logoff(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(logoffResp) {
			return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF was refused", nil
		}
		resp, err := c.Request(&proto.Route{})
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ANSWERED a ROUTE for a de-authorised session", nil
		}
		return false, failureCode(resp), failureMessage(resp), "", nil
	})
}

// armLogoffInTxStreaming asserts the guard that lets PULL and DISCARD run WITHOUT
// an authentication gate of their own.
//
// Unlike RUN/BEGIN/COMMIT/ROLLBACK/ROUTE, `handlePull` and `handleDiscard` carry no
// `!s.authenticated` check. That is safe for exactly one reason: the only thing
// that can clear the flag is LOGOFF, and LOGOFF is an illegal transition in the
// STREAMING states, so a session cannot become de-authorised while a stream is
// open. The whole safety of two ungated handlers rests on that edge, and no test
// at any level drove it — so it is driven here, from TX_STREAMING (a partially
// pulled in-transaction result), and the refusal must name the streaming state.
func (r *boltAuthRunner) armLogoffInTxStreaming(ctx context.Context) error {
	return r.arm("logoff-in-tx-streaming", authCodeRequestInvalid, "state TX_STREAMING", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		if beginResp, err := c.Begin(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(beginResp) {
			return false, failureCode(beginResp), failureMessage(beginResp), "BEGIN was refused", nil
		}
		// A multi-row read leaves the stream OPEN after a partial PULL, which is what
		// puts the session in TX_STREAMING.
		if runResp, err := c.Run("UNWIND [1,2,3] AS x RETURN x", nil); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(runResp) {
			return false, failureCode(runResp), failureMessage(runResp), "in-transaction RUN was refused", nil
		}
		recs, term, err := c.Pull(1)
		if err != nil {
			return false, "", "", "", err
		}
		if len(recs) != 1 {
			return false, "", "", "", fmt.Errorf("partial PULL returned %d records, want 1", len(recs))
		}
		if !isSuccess(term) {
			return false, failureCode(term), failureMessage(term), "partial PULL was refused", nil
		}
		resp, err := c.Logoff()
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ACCEPTED a LOGOFF mid-stream: the guard that lets PULL/DISCARD run ungated is gone", nil
		}
		return false, failureCode(resp), failureMessage(resp), "", nil
	})
}

// armResetAfterLogoffWithOpenTx drives the reclamation limb of RESET, which no
// test at any level had reached: every existing RESET test runs with NO
// transaction open, so the rollback + accounting release inside `handleReset` was
// exercised nowhere.
//
// The sequence is the one an abandoning client produces: authenticate, stage a
// write inside an explicit transaction, LOGOFF (leaving the session in TX_READY,
// unauthenticated, transaction still open), then RESET. RESET must succeed,
// discard the staged write, and — because the session is unauthenticated — return
// the connection to NEGOTIATION, from which a bare LOGON is illegal and a fresh
// HELLO starts a FIRST authentication again.
//
// It is recorded as an ADMIT arm for RESET (RESET is legal and must succeed) whose
// WAL bracket must nevertheless stay at zero: the staged write is discarded, not
// committed, so this is the one arm where an admitted message must append NOTHING.
// That combination is expressed by driving it as a REFUSE-shaped arm on the
// FOLLOW-UP message — the post-RESET LOGON, which must be refused as illegal.
func (r *boltAuthRunner) armResetAfterLogoffWithOpenTx(ctx context.Context) error {
	return r.arm("reset-after-logoff-open-tx", authCodeRequestInvalid, "state NEGOTIATION", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		if beginResp, err := c.Begin(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(beginResp) {
			return false, failureCode(beginResp), failureMessage(beginResp), "BEGIN was refused", nil
		}
		if runResp, err := c.Run(boltAuthCreate(abuseGhostLabel, "reset-after-logoff-open-tx"), nil); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(runResp) {
			return false, failureCode(runResp), failureMessage(runResp), "in-transaction RUN was refused", nil
		}
		if _, term, err := c.PullAll(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(term) {
			return false, failureCode(term), failureMessage(term), "in-transaction PULL was refused", nil
		}
		if logoffResp, err := c.Logoff(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(logoffResp) {
			return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF from TX_READY was refused", nil
		}
		resetResp, err := c.Reset()
		if err != nil {
			return false, "", "", "", err
		}
		if !isSuccess(resetResp) {
			return false, failureCode(resetResp), failureMessage(resetResp), "RESET on a de-authorised session with an open transaction was refused", nil
		}
		// An unauthenticated RESET returns the connection to NEGOTIATION, so a bare
		// LOGON must be refused: the client has to start over with HELLO.
		resp, err := c.LogonWith(basicAuthToken(simAuthPrincipal, simAuthPassword))
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(resp) {
			return true, "", "", "server ACCEPTED a bare LOGON after an unauthenticated RESET: the connection did not return to NEGOTIATION", nil
		}
		return false, failureCode(resp), failureMessage(resp), "RESET reclaimed the transaction and de-authorised the connection", nil
	})
}

// armSecondMessageAfterRefusal asserts the FAILED-state scoping rule: an
// UNAUTHENTICATED failed session must be answered with a hard FAILURE, never with
// the softened IGNORED that a still-authenticated failed session receives.
//
// `dispatch` scopes its soft-IGNORE with `state == StateFailed && s.authenticated`
// precisely so a de-authorised client cannot pipeline messages and have them
// quietly swallowed. Dropping the `&& s.authenticated` half would break no other
// arm — every one of them stops at its first refusal — so the rule is asserted
// here by sending a SECOND message after the refusal.
func (r *boltAuthRunner) armSecondMessageAfterRefusal(ctx context.Context) error {
	return r.arm("second-message-after-refusal", authCodeRequestInvalid, "state FAILED", false, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		if logoffResp, err := c.Logoff(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(logoffResp) {
			return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF was refused", nil
		}
		// First refusal: this moves the session to FAILED while unauthenticated.
		first, err := c.Run(boltAuthCreate(abuseGhostLabel, "second-message-first"), nil)
		if err != nil {
			return false, "", "", "", err
		}
		if isSuccess(first) {
			return true, "", "", "server ACCEPTED the first post-LOGOFF write", nil
		}
		// Second message on the now-FAILED, unauthenticated session: it must be a
		// hard FAILURE naming FAILED, not an IGNORED.
		second, err := c.Run(boltAuthCreate(abuseGhostLabel, "second-message-second"), nil)
		if err != nil {
			return false, "", "", "", err
		}
		if isIgnored(second) {
			return false, "", "", "", fmt.Errorf("an unauthenticated FAILED session answered IGNORED; the soft-IGNORE must be scoped to authenticated sessions")
		}
		if isSuccess(second) {
			return true, "", "", "server ACCEPTED a write on a FAILED unauthenticated session", nil
		}
		return false, failureCode(second), failureMessage(second), "second message hard-failed rather than being ignored", nil
	})
}

// armHonestWrite is the first non-vacuity witness: correct credentials, one
// committed write, WAL counters that MUST advance. Without it the whole oracle
// would be a set of assertions that pass when the server is dead.
func (r *boltAuthRunner) armHonestWrite(ctx context.Context) error {
	return r.arm("honest-write", "", "", true, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		return boltAuthCommitWrite(c, "honest-write")
	})
}

// armReauthThenWrite is the second non-vacuity witness and the recovery half of
// the LOGOFF contract: after LOGOFF a FRESH, CORRECT LOGON must restore the
// session's ability to write. A server that refused every post-LOGOFF write —
// including the legitimate one — would satisfy every REFUSE arm above and fail
// here.
func (r *boltAuthRunner) armReauthThenWrite(ctx context.Context) error {
	return r.arm("reauth-then-write", "", "", true, func() (bool, string, string, string, error) {
		c, err := r.dial()
		if err != nil {
			return false, "", "", "", err
		}
		defer func() { _ = c.Close() }()
		if err := boltAuthLogin(ctx, c); err != nil {
			return false, "", "", "", err
		}
		if logoffResp, err := c.Logoff(); err != nil {
			return false, "", "", "", err
		} else if !isSuccess(logoffResp) {
			return false, failureCode(logoffResp), failureMessage(logoffResp), "LOGOFF was refused", nil
		}
		logonResp, err := c.LogonWith(basicAuthToken(simAuthPrincipal, simAuthPassword))
		if err != nil {
			return false, "", "", "", err
		}
		if !isSuccess(logonResp) {
			return false, failureCode(logonResp), failureMessage(logonResp), "re-authentication with the CORRECT password was refused", nil
		}
		return boltAuthCommitWrite(c, "reauth-then-write")
	})
}

// boltAuthDeferredVersion is the version every LOGON-path arm negotiates
// EXPLICITLY. Leaving it to [WireClient.Handshake] — which offers 5.6 down to 5.0
// and 4.4 — would make the arm's code path depend on what the server happens to
// pick: negotiate below 5.1 and the credentials ride on HELLO instead, so
// `handleLogon`'s deferred-auth branch goes untested while every oracle stays
// green. That is precisely the vacuity this scenario exists to remove, so the
// version is pinned and a mismatch is a harness error.
var boltAuthDeferredVersion = proto.Version{Major: 5, Minor: 6}

// boltAuthNegotiateDeferred negotiates [boltAuthDeferredVersion] and fails if the
// server agreed to anything else.
func boltAuthNegotiateDeferred(ctx context.Context, c *WireClient) error {
	ver, err := c.HandshakeOffering(ctx, boltAuthDeferredVersion)
	if err != nil {
		return err
	}
	if ver != boltAuthDeferredVersion {
		return fmt.Errorf("negotiated %d.%d, want %d.%d (the deferred-auth LOGON path)",
			ver.Major, ver.Minor, boltAuthDeferredVersion.Major, boltAuthDeferredVersion.Minor)
	}
	return nil
}

// boltAuthLogin drives handshake + HELLO + LOGON with the correct credentials on
// the pinned deferred-auth version, returning a harness error if the honest path
// does not authenticate.
func boltAuthLogin(ctx context.Context, c *WireClient) error {
	if err := boltAuthNegotiateDeferred(ctx, c); err != nil {
		return err
	}
	resp, err := c.ConnectAs(ctx, simAuthPrincipal, simAuthPassword)
	if err != nil {
		return err
	}
	if !isSuccess(resp) {
		return fmt.Errorf("honest authentication refused: %v", resp)
	}
	return nil
}

// boltAuthCommitWrite runs one auto-commit write under [authHonestLabel] and
// drains it, reporting whether it succeeded.
func boltAuthCommitWrite(c *WireClient, name string) (bool, string, string, string, error) {
	resp, err := c.Run(boltAuthCreate(authHonestLabel, name), nil)
	if err != nil {
		return false, "", "", "", err
	}
	if !isSuccess(resp) {
		return false, failureCode(resp), failureMessage(resp), "authenticated write was refused", nil
	}
	_, term, err := c.PullAll()
	if err != nil {
		return false, "", "", "", err
	}
	if !isSuccess(term) {
		return false, failureCode(term), failureMessage(term), "authenticated write did not complete", nil
	}
	return true, "", "", "", nil
}

// boltAuthCensus counts the ghost and honest nodes in the LIVE engine, over the
// wire on a properly authenticated connection (so the census itself is subject to
// the same gate it is measuring).
func boltAuthCensus(ctx context.Context, srv *SimServer) (ghosts, honest int, err error) {
	c, err := srv.Dial()
	if err != nil {
		return 0, 0, fmt.Errorf("sim: bolt-auth census dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := boltAuthLogin(ctx, c); err != nil {
		return 0, 0, fmt.Errorf("sim: bolt-auth census login: %w", err)
	}
	ghosts, err = boltAuthCountLabel(c, abuseGhostLabel)
	if err != nil {
		return 0, 0, err
	}
	honest, err = boltAuthCountLabel(c, authHonestLabel)
	if err != nil {
		return 0, 0, err
	}
	return ghosts, honest, nil
}

// boltAuthCountLabel returns the number of nodes carrying label, read over the
// wire.
func boltAuthCountLabel(c *WireClient, label string) (int, error) {
	recs, err := wireQuery(c, fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", label), nil)
	if err != nil {
		return 0, fmt.Errorf("sim: bolt-auth count %s: %w", label, err)
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return 0, fmt.Errorf("sim: bolt-auth count %s: got %d records, want 1 with 1 field", label, len(recs))
	}
	n, ok := recs[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("sim: bolt-auth count %s: got %T, want int64", label, recs[0].Data[0])
	}
	return int(n), nil
}

// boltAuthCensusEngine counts the two labels directly on a reopened store's
// engine, after recovery has replayed the WAL.
func boltAuthCensusEngine(ctx context.Context, st *SimStore) (ghosts, honest int, err error) {
	count := func(label string) (int, error) {
		n, qErr := scalarCountViaEngine(ctx, st.Engine(), fmt.Sprintf("MATCH (n:%s) RETURN count(n)", label))
		if qErr != nil {
			return 0, fmt.Errorf("sim: bolt-auth recovered count %s: %w", label, qErr)
		}
		return int(n), nil
	}
	if ghosts, err = count(abuseGhostLabel); err != nil {
		return 0, 0, err
	}
	if honest, err = count(authHonestLabel); err != nil {
		return 0, 0, err
	}
	return ghosts, honest, nil
}

// ── oracles ─────────────────────────────────────────────────────────────────

// boltAuthExpectedArms is the arm roster the run must produce, in order. It is
// pinned so a dropped or renamed arm is a violation rather than a silent
// reduction in coverage: an oracle over an empty roster passes trivially.
var boltAuthExpectedArms = []string{
	"logon-wrong-password",
	"hello-inline-wrong-password",
	"logon-unknown-scheme",
	"run-before-logon",
	"logoff-then-run",
	"commit-after-logoff",
	"rollback-after-logoff",
	"reauth-wrong-password",
	"route-after-logoff",
	"logoff-in-tx-streaming",
	"reset-after-logoff-open-tx",
	"second-message-after-refusal",
	"honest-write",
	"reauth-then-write",
}

// checkBoltAuthSurface adjudicates the evidence against the authentication
// contract. Every violation names the arm and the counters, so a failure is
// diagnosable from the report alone.
func checkBoltAuthSurface(e BoltAuthEvidence) []Violation {
	var v []Violation
	for i := range e.Arms {
		v = append(v, checkBoltAuthArm(&e.Arms[i])...)
	}
	if e.GhostNodes != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<bolt-auth-ghost-census>",
			Message: fmt.Sprintf("%d node(s) labelled %s exist in the live engine: a refused statement was executed",
				e.GhostNodes, abuseGhostLabel),
		})
	}
	if e.RecoveredGhosts != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bolt-auth-ghost-recovered>",
			Message: fmt.Sprintf("%d node(s) labelled %s survived WAL replay: a refused statement reached the durable log",
				e.RecoveredGhosts, abuseGhostLabel),
		})
	}
	if want := boltAuthAdmitArms(e); e.HonestNodes != want {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bolt-auth-honest-census>",
			Message: fmt.Sprintf("live engine holds %d node(s) labelled %s, want %d (one per ADMIT arm)",
				e.HonestNodes, authHonestLabel, want),
		})
	}
	if e.RecoveredHonest != e.HonestNodes {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bolt-auth-honest-recovered>",
			Message: fmt.Sprintf("WAL replay recovered %d node(s) labelled %s, but %d were acknowledged live",
				e.RecoveredHonest, authHonestLabel, e.HonestNodes),
		})
	}
	return v
}

// checkBoltAuthArm adjudicates one arm: the verdict, the code, and the WAL
// bracket. It takes a pointer only because the value is 104 bytes; it mutates
// nothing.
func checkBoltAuthArm(a *BoltAuthArm) []Violation {
	var v []Violation
	switch {
	case a.Admit && !a.Accepted:
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bolt-auth:" + a.Name + ">",
			Message: fmt.Sprintf("legitimate arm %q was REFUSED (code %q, %s)", a.Name, a.GotCode, a.Detail),
		})
	case !a.Admit && a.Accepted:
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<bolt-auth:" + a.Name + ">",
			Message: fmt.Sprintf("arm %q was ADMITTED but must be refused: %s", a.Name, a.Detail),
		})
	case !a.Admit && a.GotCode != a.WantCode:
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bolt-auth:" + a.Name + ">",
			Message: fmt.Sprintf("arm %q refused with code %q, want %q (%s)", a.Name, a.GotCode, a.WantCode, a.Detail),
		})
	}
	// Gate attribution: the refusal must have come from the AUTHENTICATION gate,
	// which failTransition identifies by naming the (legal) state the session was
	// in. A refusal naming any other state came from the state machine instead, and
	// the CWE-306 gate went untested behind an identical failure code.
	if !a.Admit && a.WantStateInMessage != "" && !strings.Contains(a.GotMessage, a.WantStateInMessage) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bolt-auth:" + a.Name + ">",
			Message: fmt.Sprintf("arm %q was refused from the wrong state: message %q does not name %q, so the refusal came from the state machine and not from the authentication gate",
				a.Name, a.GotMessage, a.WantStateInMessage),
		})
	}
	if !a.Admit {
		// The load-bearing oracle: a refusal must leave the durable log untouched.
		if n := a.framesAppended(); n != 0 {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: "<bolt-auth:" + a.Name + ">",
				Message: fmt.Sprintf("refused arm %q appended %d WAL frame(s) (%d -> %d)",
					a.Name, n, a.FramesBefore, a.FramesAfter),
			})
		}
		if n := a.bytesAppended(); n != 0 {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: "<bolt-auth:" + a.Name + ">",
				Message: fmt.Sprintf("refused arm %q appended %d WAL byte(s) (%d -> %d)",
					a.Name, n, a.BytesBefore, a.BytesAfter),
			})
		}
		return v
	}
	// An ADMIT arm must reach the log: this is what makes the counter a live
	// instrument rather than a constant zero.
	if a.framesAppended() == 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bolt-auth:" + a.Name + ">",
			Message: fmt.Sprintf("admitted arm %q appended NO WAL frame (frames stuck at %d): the write was acknowledged without reaching the log",
				a.Name, a.FramesBefore),
		})
	}
	return v
}

// boltAuthAdmitArms counts the arms whose intent was to be admitted.
func boltAuthAdmitArms(e BoltAuthEvidence) int {
	n := 0
	for i := range e.Arms {
		if e.Arms[i].Admit {
			n++
		}
	}
	return n
}

// checkBoltAuthSurfaceNonVacuity proves the run actually exercised the surface,
// so a green verdict cannot come from a battery that quietly did nothing: the
// full arm roster ran, refusals and admissions both occurred, the WAL counter was
// observed MOVING, and all three distinct failure codes were seen.
func checkBoltAuthSurfaceNonVacuity(e BoltAuthEvidence) []Violation {
	var v []Violation
	seen := make(map[string]bool, len(e.Arms))
	for i := range e.Arms {
		seen[e.Arms[i].Name] = true
	}
	for _, want := range boltAuthExpectedArms {
		if !seen[want] {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<bolt-auth-nonvacuity>",
				Message: fmt.Sprintf("arm %q did not run: the surface was not fully driven", want),
			})
		}
	}
	var admitted, refused int
	var framesFromAdmit uint64
	codes := make(map[string]bool)
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.Admit {
			admitted++
			framesFromAdmit += a.framesAppended()
			continue
		}
		refused++
		if a.GotCode != "" {
			codes[a.GotCode] = true
		}
	}
	if admitted == 0 || refused == 0 {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<bolt-auth-nonvacuity>",
			Message: fmt.Sprintf("run observed %d admitted and %d refused arm(s); both must be non-zero or the oracle cannot fail", admitted, refused),
		})
	}
	if framesFromAdmit == 0 {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<bolt-auth-nonvacuity>",
			Message: "no arm appended a WAL frame: the frame counter is not a live instrument, so \"no frames appended\" proves nothing",
		})
	}
	for _, want := range []string{authCodeUnauthorized, authCodeProviderFailed, authCodeRequestInvalid} {
		if !codes[want] {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<bolt-auth-nonvacuity>",
				Message: fmt.Sprintf("failure code %q was never observed: that refusal path was not exercised", want),
			})
		}
	}
	return v
}

// String renders the evidence for a report: one line per arm with its verdict and
// WAL delta, then the censuses.
func (e BoltAuthEvidence) String() string {
	out := fmt.Sprintf("bolt-auth evidence (seed %d, %d arms):", e.Seed, len(e.Arms))
	for i := range e.Arms {
		a := &e.Arms[i]
		verdict := "REFUSED"
		if a.Accepted {
			verdict = "ADMITTED"
		}
		out += fmt.Sprintf("\n  %-28s %-8s code=%-45q frames+%d bytes+%d",
			a.Name, verdict, a.GotCode, a.framesAppended(), a.bytesAppended())
		if a.Detail != "" {
			out += " (" + a.Detail + ")"
		}
	}
	out += fmt.Sprintf("\n  census: ghosts=%d honest=%d | after recovery: ghosts=%d honest=%d",
		e.GhostNodes, e.HonestNodes, e.RecoveredGhosts, e.RecoveredHonest)
	return out
}

// ── scenario ────────────────────────────────────────────────────────────────

// boltAuthDefaultSeed is the catalogue default for [ScenarioBoltAuth].
const boltAuthDefaultSeed = 0xB017_A17

// boltAuthSurfaceScenario drives the authentication surface against a server that
// genuinely validates credentials, with the WAL as the witness that a refusal
// wrote nothing. See the file comment for what it covers and why the ADMIT arms
// are load-bearing.
func boltAuthSurfaceScenario() Scenario {
	return Scenario{
		Name:        ScenarioBoltAuth,
		Description: "Bolt auth surface: wrong credentials, unknown scheme, LOGOFF gates, re-auth — no statement and no WAL frame without a successful LOGON",
		Mode:        ModeDeterministic,
		DefaultSeed: boltAuthDefaultSeed,
		run:         runBoltAuthSurfaceScenario,
	}
}

// runBoltAuthSurfaceScenario is the scenario entry point: collect the evidence,
// then adjudicate it against both the contract and the non-vacuity gate.
func runBoltAuthSurfaceScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltAuthSurface(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltAuthSurface(ev), checkBoltAuthSurfaceNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return boltAuthReport(ScenarioBoltAuth, seed, v), nil
}

// boltAuthReport wraps violations in a scenario report.
func boltAuthReport(name string, seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   name,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt auth surface>"},
		Violations: v,
	}
}

package sim

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestBoltAuthSurface_Clean drives the whole authentication surface against a
// credential-validating server and asserts both the contract and the non-vacuity
// gate are satisfied.
func TestBoltAuthSurface_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltAuthSurface(context.Background(), boltAuthDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltAuthSurface: %v", err)
	}
	for _, v := range checkBoltAuthSurface(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltAuthSurfaceNonVacuity(ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	if t.Failed() {
		t.Log(ev.String())
	}
}

// TestBoltAuthSurface_Deterministic asserts the same seed produces the same
// evidence, which is what makes a failure replayable.
//
// It compares field by field rather than by rendering, because ONE field is not
// reachable from the seed: the byte delta of an ADMIT arm. A created node's hidden
// key is "__cx_"+hex(n) from a process-global counter in cypher/exec, and that key
// sits inside the WAL frame, so the frame's SIZE depends on how many nodes the rest
// of the process created first — measured 183 bytes standing alone and 186 after
// the rest of the package, the key having gained one hex digit in three frames.
// The frame COUNT, every verdict, every code and every census are seed-pure and are
// compared; a refused arm's byte delta is compared too, since zero is zero at any
// width. Asserting an admitted arm's byte total would be asserting the process
// history, which is how this test failed in the full-suite run that found the
// limitation.
func TestBoltAuthSurface_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x5EED_A17
	first, err := RunBoltAuthSurface(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltAuthSurface(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(first.Arms) != len(second.Arms) {
		t.Fatalf("arm counts differ: %d vs %d", len(first.Arms), len(second.Arms))
	}
	for i := range first.Arms {
		a, b := &first.Arms[i], &second.Arms[i]
		if a.Name != b.Name || a.Admit != b.Admit || a.Accepted != b.Accepted ||
			a.GotCode != b.GotCode || a.GotMessage != b.GotMessage || a.Detail != b.Detail ||
			a.framesAppended() != b.framesAppended() {
			t.Errorf("arm %d diverged across two runs of seed %d:\n first=%+v\nsecond=%+v", i, seed, *a, *b)
		}
		// A refused arm appended nothing in both runs; that is width-independent.
		if !a.Admit && a.bytesAppended() != b.bytesAppended() {
			t.Errorf("refused arm %q byte delta diverged: %d vs %d", a.Name, a.bytesAppended(), b.bytesAppended())
		}
	}
	if first.GhostNodes != second.GhostNodes || first.HonestNodes != second.HonestNodes ||
		first.RecoveredGhosts != second.RecoveredGhosts || first.RecoveredHonest != second.RecoveredHonest {
		t.Errorf("censuses diverged: first=%+v second=%+v", first, second)
	}
}

// TestBoltAuthSurface_ScenarioPasses drives the catalogue scenario end to end, so
// the registry wiring — not just the collector — is covered.
func TestBoltAuthSurface_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBoltAuth)
	if !ok {
		t.Fatalf("scenario %q not in catalogue", ScenarioBoltAuth)
	}
	rep, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("scenario run: %v", err)
	}
	if rep != nil {
		t.Errorf("scenario failed:\n%s", rep.String())
	}
}

// cleanAuthEvidence returns a hand-built evidence value that both checkers pass,
// so a perturbation test can attribute any violation to the field it changed.
// Every arm in the roster is present, refusals append nothing, and the two ADMIT
// arms append frames.
func cleanAuthEvidence() BoltAuthEvidence {
	// Per refusing arm: the code it must be told and, where the refusal comes from
	// the AUTHENTICATION gate rather than a credential check, the session state its
	// message must name (see [BoltAuthArm.WantStateInMessage]).
	type refusal struct{ code, state string }
	refusals := map[string]refusal{
		"logon-wrong-password":         {authCodeUnauthorized, ""},
		"hello-inline-wrong-password":  {authCodeUnauthorized, ""},
		"logon-unknown-scheme":         {authCodeProviderFailed, ""},
		"run-before-logon":             {authCodeRequestInvalid, "state AUTHENTICATION"},
		"logoff-then-run":              {authCodeRequestInvalid, "state READY"},
		"commit-after-logoff":          {authCodeRequestInvalid, "state TX_READY"},
		"rollback-after-logoff":        {authCodeRequestInvalid, "state TX_READY"},
		"reauth-wrong-password":        {authCodeUnauthorized, ""},
		"route-after-logoff":           {authCodeRequestInvalid, "state READY"},
		"logoff-in-tx-streaming":       {authCodeRequestInvalid, "state TX_STREAMING"},
		"reset-after-logoff-open-tx":   {authCodeRequestInvalid, "state NEGOTIATION"},
		"second-message-after-refusal": {authCodeRequestInvalid, "state FAILED"},
	}
	ev := BoltAuthEvidence{Seed: 1}
	var frames, bytes uint64
	for _, name := range boltAuthExpectedArms {
		ref, refuse := refusals[name]
		a := BoltAuthArm{
			Name: name, FramesBefore: frames, BytesBefore: bytes,
			FramesAfter: frames, BytesAfter: bytes,
		}
		if refuse {
			a.WantCode, a.GotCode = ref.code, ref.code
			a.WantStateInMessage = ref.state
			if ref.state != "" {
				a.GotMessage = "illegal message *proto.Run in " + ref.state
			} else {
				a.GotMessage = "Authentication failed."
			}
		} else {
			a.Admit, a.Accepted = true, true
			frames, bytes = frames+4, bytes+180
			a.FramesAfter, a.BytesAfter = frames, bytes
		}
		ev.Arms = append(ev.Arms, a)
	}
	ev.HonestNodes, ev.RecoveredHonest = boltAuthAdmitArms(ev), boltAuthAdmitArms(ev)
	return ev
}

// armIndex returns the index of the named arm. It PANICS when the arm is absent
// rather than calling t.Fatalf: every caller is a mutate closure running inside a
// t.Run subtest, and testing requires Fatalf to be called on the goroutine of the
// test it belongs to — failing the parent from a subtest's goroutine is undefined.
// A missing arm is a defect in the hand-built fixture, not a test outcome, so a
// panic is the honest signal and it names the arm.
func armIndex(ev *BoltAuthEvidence, name string) int {
	for i := range ev.Arms {
		if ev.Arms[i].Name == name {
			return i
		}
	}
	panic("sim: test fixture has no arm named " + name)
}

// TestBoltAuthSurface_OracleCanFail is the falsifiability proof for the checkers:
// the clean evidence passes, and every single-field perturbation of it is caught.
// An oracle that cannot fail proves nothing, so each mutation below names the
// defect it stands for.
func TestBoltAuthSurface_OracleCanFail(t *testing.T) {
	t.Parallel()

	base := cleanAuthEvidence()
	if v := checkBoltAuthSurface(base); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %d violation(s): %v", len(v), v)
	}
	if v := checkBoltAuthSurfaceNonVacuity(base); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltAuthEvidence)
		wantSub string
	}{
		{
			name: "wrong password admitted",
			mutate: func(e *BoltAuthEvidence) {
				e.Arms[armIndex(e, "logon-wrong-password")].Accepted = true
			},
			wantSub: "must be refused",
		},
		{
			name: "refused arm leaked a WAL frame",
			mutate: func(e *BoltAuthEvidence) {
				e.Arms[armIndex(e, "commit-after-logoff")].FramesAfter++
			},
			wantSub: "appended 1 WAL frame",
		},
		{
			name: "refused arm leaked WAL bytes",
			mutate: func(e *BoltAuthEvidence) {
				e.Arms[armIndex(e, "logoff-then-run")].BytesAfter += 64
			},
			wantSub: "appended 64 WAL byte",
		},
		{
			name: "unknown scheme mapped onto the wrong code",
			mutate: func(e *BoltAuthEvidence) {
				e.Arms[armIndex(e, "logon-unknown-scheme")].GotCode = authCodeUnauthorized
			},
			wantSub: "want \"Neo.ClientError.Security.AuthProviderFailed\"",
		},
		{
			name: "legitimate write refused",
			mutate: func(e *BoltAuthEvidence) {
				i := armIndex(e, "honest-write")
				e.Arms[i].Accepted = false
				e.Arms[i].GotCode = authCodeRequestInvalid
			},
			wantSub: "was REFUSED",
		},
		{
			name: "admitted write never reached the WAL",
			mutate: func(e *BoltAuthEvidence) {
				i := armIndex(e, "reauth-then-write")
				e.Arms[i].FramesAfter = e.Arms[i].FramesBefore
			},
			wantSub: "appended NO WAL frame",
		},
		{
			name: "the state machine refused it, not the authentication gate",
			mutate: func(e *BoltAuthEvidence) {
				e.Arms[armIndex(e, "commit-after-logoff")].GotMessage = "illegal message *proto.Commit in state FAILED"
			},
			wantSub: "came from the state machine and not from the authentication gate",
		},
		{
			name:    "a refused statement executed",
			mutate:  func(e *BoltAuthEvidence) { e.GhostNodes = 1 },
			wantSub: "exist in the live engine",
		},
		{
			name:    "a refused statement reached the durable log",
			mutate:  func(e *BoltAuthEvidence) { e.RecoveredGhosts = 1 },
			wantSub: "survived WAL replay",
		},
		{
			name:    "an honest write vanished from the engine",
			mutate:  func(e *BoltAuthEvidence) { e.HonestNodes-- },
			wantSub: "one per ADMIT arm",
		},
		{
			name:    "an acknowledged write did not survive recovery",
			mutate:  func(e *BoltAuthEvidence) { e.RecoveredHonest-- },
			wantSub: "acknowledged live",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanAuthEvidence()
			tc.mutate(&ev)
			v := checkBoltAuthSurface(ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the oracle cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltAuthSurface_NonVacuityCanFail proves the non-vacuity gate itself is
// falsifiable: a battery that skipped an arm, never admitted anything, or never
// moved the WAL counter must be reported rather than pass quietly.
func TestBoltAuthSurface_NonVacuityCanFail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*BoltAuthEvidence)
		wantSub string
	}{
		{
			name:    "an arm silently stopped running",
			mutate:  func(e *BoltAuthEvidence) { e.Arms = e.Arms[1:] },
			wantSub: "did not run",
		},
		{
			name: "nothing was ever admitted",
			mutate: func(e *BoltAuthEvidence) {
				kept := e.Arms[:0]
				for _, a := range e.Arms {
					if !a.Admit {
						kept = append(kept, a)
					}
				}
				e.Arms = kept
			},
			wantSub: "both must be non-zero",
		},
		{
			name: "the WAL counter never moved",
			mutate: func(e *BoltAuthEvidence) {
				for i := range e.Arms {
					e.Arms[i].FramesAfter = e.Arms[i].FramesBefore
				}
			},
			wantSub: "not a live instrument",
		},
		{
			name: "a refusal path was never exercised",
			mutate: func(e *BoltAuthEvidence) {
				e.Arms[armIndex(e, "logon-unknown-scheme")].GotCode = ""
			},
			wantSub: "was never observed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanAuthEvidence()
			tc.mutate(&ev)
			v := checkBoltAuthSurfaceNonVacuity(ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the non-vacuity gate cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// anyViolationMentions reports whether any violation message contains sub.
func anyViolationMentions(vs []Violation, sub string) bool {
	for _, v := range vs {
		if strings.Contains(v.Message, sub) {
			return true
		}
	}
	return false
}

// TestBoltAuthSurface_NoAuthAdmitsTheWrongPassword is the CONTROL for the whole
// scenario. Every refusal above is only evidence of a working credential check if
// the identical wire exchange is ADMITTED when the check is absent — otherwise the
// refusals could be coming from anything else in the stack (the state machine, the
// framing, a typo in the harness). Driving the same wrong password against a
// NoAuthHandler-backed server must succeed, which pins the refusals on the
// AuthHandler and nothing else.
func TestBoltAuthSurface_NoAuthAdmitsTheWrongPassword(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.ConnectAs(context.Background(), simAuthPrincipal, simAuthWrongPassword)
	if err != nil {
		t.Fatalf("ConnectAs: %v", err)
	}
	if !isSuccess(resp) {
		t.Fatalf("NoAuthHandler refused a credential: got %#v — the control is broken, so the scenario's refusals prove nothing", resp)
	}
	// And it really is authenticated: the write goes through. The verdict must be
	// read, not just the transport error — boltAuthCommitWrite reports a REFUSED
	// write as (false, code, detail, nil), so checking only err would let a server
	// that refuses every write pass this control.
	ok, code, _, detail, err := boltAuthCommitWrite(c, "control")
	if err != nil {
		t.Fatalf("control write: %v", err)
	}
	if !ok {
		t.Fatalf("control write was refused (code %q, %s): the control cannot show that a NoAuth server admits writes", code, detail)
	}
}

// TestBoltAuthSurface_RealAuthRefusesTheWrongPassword is the other half of the
// control: the identical exchange against the credential-validating server must be
// refused with Unauthorized.
func TestBoltAuthSurface_RealAuthRefusesTheWrongPassword(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServerAuth(SimEngineForServer(), clock.Real(), boltAuthValidator())
	if err != nil {
		t.Fatalf("NewSimServerAuth: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.ConnectAs(context.Background(), simAuthPrincipal, simAuthWrongPassword)
	if err != nil {
		t.Fatalf("ConnectAs: %v", err)
	}
	if code := failureCode(resp); code != authCodeUnauthorized {
		t.Fatalf("wrong password answered with %#v (code %q), want a FAILURE with %q", resp, code, authCodeUnauthorized)
	}
	// The correct password on a fresh connection must still work, so the handler
	// refuses the wrong credential rather than refusing everything.
	good, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = good.Close() }()
	if err := boltAuthLogin(context.Background(), good); err != nil {
		t.Fatalf("correct password refused: %v", err)
	}
}

// TestBoltAbuser_CredentialFamilyNeedsRealAuth pins the reason
// [AbuseFamily.NeedsCredentialAuth] exists: the bad-credential family is refused
// by a validating server and ADMITTED by a NoAuth one. Were it drawn by
// [BoltAbuser.PickFamily] into the NoAuth bad-actors scenario, it would report an
// unacceptable outcome for entirely correct behaviour.
func TestBoltAbuser_CredentialFamilyNeedsRealAuth(t *testing.T) {
	defer goleak.VerifyNone(t)

	abuser := BoltAbuser{}

	authSrv, err := NewSimServerAuth(SimEngineForServer(), clock.Real(), boltAuthValidator())
	if err != nil {
		t.Fatalf("NewSimServerAuth: %v", err)
	}
	defer func() { _ = authSrv.Close() }()
	out, err := abuser.Abuse(authSrv, AbuseBadCredentials)
	if err != nil {
		t.Fatalf("Abuse(BadCredentials) against a validating server: %v", err)
	}
	if !out.Acceptable() {
		t.Errorf("validating server did not refuse BadCredentials: %+v", out)
	}
	if !strings.Contains(out.FailureMsg, authCodeUnauthorized) {
		t.Errorf("BadCredentials refused with %q, want %q", out.FailureMsg, authCodeUnauthorized)
	}

	noAuthSrv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = noAuthSrv.Close() }()
	out, err = abuser.Abuse(noAuthSrv, AbuseBadCredentials)
	if err != nil {
		t.Fatalf("Abuse(BadCredentials) against NoAuth: %v", err)
	}
	if out.Acceptable() {
		t.Errorf("NoAuthHandler refused a credential (%+v): if it did, the family would be safe to draw and NeedsCredentialAuth would be pointless", out)
	}

	// And PickFamily must never draw it, whatever the seed.
	for i := 0; i < 500; i++ {
		if fam := abuser.PickFamily(NewSeed(uint64(i))); fam.NeedsCredentialAuth() {
			t.Fatalf("PickFamily(seed %d) drew %s, which needs a credential-validating server", i, fam)
		}
	}
}

// TestBoltAbuser_PostLogoffFamiliesBiteUnderNoAuth proves the two LOGOFF families
// are correctly classified as any-server: the gate they exercise is the session's
// own `authenticated` flag, so it must refuse them even when the handler admits
// every credential.
func TestBoltAbuser_PostLogoffFamiliesBiteUnderNoAuth(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	abuser := BoltAbuser{}
	for _, fam := range []AbuseFamily{AbuseLogoffThenRun, AbuseCommitAfterLogoff} {
		out, err := abuser.Abuse(srv, fam)
		if err != nil {
			t.Fatalf("Abuse(%s): %v", fam, err)
		}
		if !out.GotFailure {
			t.Errorf("Abuse(%s) under NoAuth: got %+v, want a typed FAILURE", fam, out)
		}
		if !strings.Contains(out.FailureMsg, authCodeRequestInvalid) {
			t.Errorf("Abuse(%s): failure %q, want code %q", fam, out.FailureMsg, authCodeRequestInvalid)
		}
	}
	// Nothing the two families attempted may have been applied.
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	n, err := boltAuthCountLabel(c, abuseGhostLabel)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if n != 0 {
		t.Errorf("%d ghost node(s) exist after the post-LOGOFF families: a refused write was applied", n)
	}
}

// TestSimServerAuth_RejectsNilHandler pins the fail-closed contract: a SimServer
// cannot be built without an auth handler.
func TestSimServerAuth_RejectsNilHandler(t *testing.T) {
	t.Parallel()
	if _, err := NewSimServerAuth(SimEngineForServer(), clock.Real(), nil); err == nil {
		t.Fatal("NewSimServerAuth(nil handler) succeeded, want ErrNoAuthHandler")
	}
}

// TestWireClientHandshakeOffering_SelectsTheOfferedVersion asserts the explicit
// offer really withholds the newer versions — the property the inline-auth arm
// depends on, since the server always picks the highest version it is offered.
func TestWireClientHandshakeOffering_SelectsTheOfferedVersion(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	for _, want := range []proto.Version{{Major: 4, Minor: 4}, {Major: 5, Minor: 0}, {Major: 5, Minor: 6}} {
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		got, err := c.HandshakeOffering(context.Background(), want)
		if err != nil {
			t.Fatalf("HandshakeOffering(%d.%d): %v", want.Major, want.Minor, err)
		}
		if got != want {
			t.Errorf("offered only %d.%d, negotiated %d.%d", want.Major, want.Minor, got.Major, got.Minor)
		}
		_ = c.Close()
	}
	// An empty or oversized offer list is a harness error, not a silent handshake.
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.HandshakeOffering(context.Background()); err == nil {
		t.Error("HandshakeOffering() with no versions succeeded, want an error")
	}
	if _, err := c.HandshakeOffering(context.Background(), make([]proto.Version, 5)...); err == nil {
		t.Error("HandshakeOffering() with 5 versions succeeded, want an error")
	}
}

// TestSimServerAuth_UsesTheGivenHandler is a guard against the parameterisation
// being silently dropped: a handler that refuses EVERYTHING must make even the
// scenario's own credentials fail.
func TestSimServerAuth_UsesTheGivenHandler(t *testing.T) {
	defer goleak.VerifyNone(t)

	refuseAll := server.BasicAuthHandler{
		Validate: func(string, string) error { return server.ErrAuthFailed },
	}
	srv, err := NewSimServerAuth(SimEngineForServer(), clock.Real(), refuseAll)
	if err != nil {
		t.Fatalf("NewSimServerAuth: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.ConnectAs(context.Background(), simAuthPrincipal, simAuthPassword)
	if err != nil {
		t.Fatalf("ConnectAs: %v", err)
	}
	if code := failureCode(resp); code != authCodeUnauthorized {
		t.Fatalf("refuse-all handler answered %#v, want %q: Options.Auth was not honoured", resp, authCodeUnauthorized)
	}
}

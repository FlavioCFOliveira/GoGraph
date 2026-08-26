package sim

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestBoltCertRotation_Clean drives the rotation surface and asserts the contract
// and the non-vacuity gate both hold.
func TestBoltCertRotation_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltCertRotation(context.Background(), certRotationDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltCertRotation: %v", err)
	}
	for _, v := range checkBoltCertRotation(&ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltCertRotationNonVacuity(&ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	if t.Failed() {
		t.Log(ev.String())
	}
}

// TestBoltCertRotation_Deterministic asserts a seed reproduces its run exactly,
// including the seed-chosen torn-prefix length.
func TestBoltCertRotation_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x5EED_CE27
	first, err := RunBoltCertRotation(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltCertRotation(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// The temp-directory path appears in the absent-key error, so compare the
	// evidence field by field rather than through the rendered form, which embeds
	// a path that legitimately differs per run.
	if len(first.Steps) != len(second.Steps) {
		t.Fatalf("step counts differ: %d vs %d", len(first.Steps), len(second.Steps))
	}
	for i := range first.Steps {
		a, b := first.Steps[i], second.Steps[i]
		if a.Name != b.Name || a.ServedCN != b.ServedCN || a.ReloadOK != b.ReloadOK ||
			a.Handshook != b.Handshook || a.KeyBytes != b.KeyBytes {
			t.Errorf("step %d diverged:\n first=%+v\nsecond=%+v", i, a, b)
		}
	}
}

// TestBoltCertRotation_KeyMaterialIsSeedStable proves the fixture generator is
// deterministic — the property the whole scenario's replayability rests on — and
// that successive pairs from one seed are genuinely DIFFERENT certificates, so a
// "rotation" is not installing the same bytes twice.
func TestBoltCertRotation_KeyMaterialIsSeedStable(t *testing.T) {
	t.Parallel()

	gen := func(seed uint64) (simCertPair, simCertPair) {
		s := NewSeed(seed)
		a, err := newSimCertPair(s, "A")
		if err != nil {
			t.Fatalf("pair A: %v", err)
		}
		b, err := newSimCertPair(s, "B")
		if err != nil {
			t.Fatalf("pair B: %v", err)
		}
		return a, b
	}
	a1, b1 := gen(42)
	a2, b2 := gen(42)
	if !bytes.Equal(a1.CertPEM, a2.CertPEM) || !bytes.Equal(a1.KeyPEM, a2.KeyPEM) {
		t.Error("the same seed produced different material for pair A: the fixture is not reproducible")
	}
	if !bytes.Equal(b1.CertPEM, b2.CertPEM) {
		t.Error("the same seed produced different material for pair B")
	}
	if bytes.Equal(a1.CertPEM, b1.CertPEM) || bytes.Equal(a1.KeyPEM, b1.KeyPEM) {
		t.Error("two successive pairs from one seed are identical: a rotation between them would change nothing")
	}
	other, _ := gen(43)
	if bytes.Equal(other.CertPEM, a1.CertPEM) {
		t.Error("different seeds produced identical material: the seed does not reach the key generator")
	}
}

// TestBoltCertRotation_HandshakeOracleCanFail is the falsifiability proof for the
// verifier itself. Every step in the clean run reports handshake-ok, which is only
// meaningful if the handshake can fail: here it is pointed at a certificate issued
// for a DIFFERENT name, which crypto/tls must reject on name verification.
func TestBoltCertRotation_HandshakeOracleCanFail(t *testing.T) {
	defer goleak.VerifyNone(t)

	dir := t.TempDir()
	s := NewSeed(7)
	wrong, err := newSimCertPairForHost(s, "wrong-host", "not-"+certRotationHost)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	certPath := filepath.Join(dir, "wrong.crt")
	keyPath := filepath.Join(dir, "wrong.key")
	if err := os.WriteFile(certPath, wrong.CertPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, wrong.KeyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	r, err := server.NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	cn, err := certRotationVerify(r)
	if err == nil {
		t.Fatal("certRotationVerify accepted a certificate issued for another host: the handshake oracle cannot fail, so every handshake-ok above proves nothing")
	}
	if cn != "wrong-host" {
		t.Errorf("served CN %q, want %q (the CN must still be reported when the handshake fails)", cn, "wrong-host")
	}
}

// cleanRotationEvidence returns a hand-built evidence value both checkers pass.
func cleanRotationEvidence() BoltCertRotationEvidence {
	const full = 119
	live := "rotation-B"
	steps := []CertRotationStep{
		{Name: "initial-load", WantCN: "rotation-A", ServedCN: "rotation-A", WantReloadOK: true, ReloadOK: true, Handshook: true, KeyBytes: full, KeyWantBytes: full},
		{Name: "clean-rotation", WantCN: live, ServedCN: live, WantReloadOK: true, ReloadOK: true, Handshook: true, KeyBytes: full, KeyWantBytes: full},
		{Name: "torn-key", WantCN: live, ServedCN: live, Handshook: true, ReloadErr: "torn", KeyBytes: full / 2, KeyWantBytes: full},
		{Name: "garbled-key", WantCN: live, ServedCN: live, Handshook: true, ReloadErr: "garbled", KeyBytes: full, KeyWantBytes: full},
		{Name: "absent-key", WantCN: live, ServedCN: live, Handshook: true, ReloadErr: "absent", KeyBytes: 0, KeyWantBytes: 0},
		{Name: "mismatched-pair", WantCN: live, ServedCN: live, Handshook: true, ReloadErr: "mismatch", KeyBytes: full, KeyWantBytes: full},
		{Name: "watch-reports-failure", WantCN: live, ServedCN: live, Handshook: true, ReloadErr: "torn", KeyBytes: full / 2, KeyWantBytes: full},
		{Name: "rotation-completed", WantCN: "rotation-C", ServedCN: "rotation-C", WantReloadOK: true, ReloadOK: true, Handshook: true, KeyBytes: full, KeyWantBytes: full},
		{Name: "expired-leaf", WantCN: "rotation-C", ServedCN: "rotation-C", Handshook: true, ReloadErr: "expired", WantValidityRefused: true, ValidityRefused: true, KeyBytes: full, KeyWantBytes: full},
		{Name: "not-yet-valid-leaf", WantCN: "rotation-C", ServedCN: "rotation-C", Handshook: true, ReloadErr: "not yet valid", WantValidityRefused: true, ValidityRefused: true, KeyBytes: full, KeyWantBytes: full},
	}
	return BoltCertRotationEvidence{
		Steps:              steps,
		UnloadedGetErr:     "not loaded",
		InitialLoadTornErr: "torn",
		WatchErrors:        []string{"load X509 key pair: tls: failed to find any PEM data in key input"},
		Seed:               1,
	}
}

// rotationStep returns a pointer to the named step. Like armIndex in
// bolt_auth_surface_test.go it PANICS rather than calling t.Fatalf, because every
// caller runs inside a t.Run subtest closure and Fatalf must not be called for a
// test other than the one whose goroutine it runs on.
func rotationStep(e *BoltCertRotationEvidence, name string) *CertRotationStep {
	for i := range e.Steps {
		if e.Steps[i].Name == name {
			return &e.Steps[i]
		}
	}
	panic("sim: test fixture has no step named " + name)
}

// TestBoltCertRotation_OracleCanFail perturbs the clean evidence one field at a
// time and requires every mutation to be caught.
func TestBoltCertRotation_OracleCanFail(t *testing.T) {
	t.Parallel()

	base := cleanRotationEvidence()
	if v := checkBoltCertRotation(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %v", v)
	}
	if v := checkBoltCertRotationNonVacuity(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %v", v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltCertRotationEvidence)
		check   func(*BoltCertRotationEvidence) []Violation
		wantSub string
	}{
		{
			name:    "a broken rotation was accepted",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "torn-key").ReloadOK = true },
			check:   checkBoltCertRotation,
			wantSub: "ACCEPTED broken material",
		},
		{
			name:    "a valid rotation was refused",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "clean-rotation").ReloadOK = false },
			check:   checkBoltCertRotation,
			wantSub: "Reload of a VALID pair failed",
		},
		{
			name:    "a failed rotation swapped the live certificate anyway",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "mismatched-pair").ServedCN = "rotation-C" },
			check:   checkBoltCertRotation,
			wantSub: "certificate in service is",
		},
		{
			name:    "the live certificate stopped handshaking",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "absent-key").Handshook = false },
			check:   checkBoltCertRotation,
			wantSub: "no longer completes a TLS handshake",
		},
		{
			name:    "an unloaded reloader served something",
			mutate:  func(e *BoltCertRotationEvidence) { e.UnloadedGetErr = "" },
			check:   checkBoltCertRotation,
			wantSub: "instead of refusing",
		},
		{
			name:    "the mandatory initial load did not fail closed",
			mutate:  func(e *BoltCertRotationEvidence) { e.InitialLoadTornErr = "" },
			check:   checkBoltCertRotation,
			wantSub: "ACCEPTED a torn key",
		},
		{
			name:    "a background rotation failed with no operator signal",
			mutate:  func(e *BoltCertRotationEvidence) { e.WatchErrors = nil },
			check:   checkBoltCertRotation,
			wantSub: "no operator-visible signal",
		},
		{
			name:    "a step silently stopped running",
			mutate:  func(e *BoltCertRotationEvidence) { e.Steps = e.Steps[1:] },
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "did not run",
		},
		{
			name: "no rotation ever failed",
			mutate: func(e *BoltCertRotationEvidence) {
				for i := range e.Steps {
					e.Steps[i].ReloadOK = true
				}
			},
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "both must be non-zero",
		},
		{
			name: "the certificate in service never changed",
			mutate: func(e *BoltCertRotationEvidence) {
				for i := range e.Steps {
					e.Steps[i].ServedCN = "rotation-A"
				}
			},
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "distinct certificate",
		},
		{
			name:    "the torn arm truncated nothing",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "torn-key").KeyBytes = 119 },
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "nothing was truncated",
		},
		{
			name:    "the garbled arm duplicated the torn one",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "garbled-key").KeyBytes = 40 },
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "duplicated the torn one",
		},
		{
			name:    "the absent arm left the file in place",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "absent-key").KeyBytes = 119 },
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "was not removed",
		},
		{
			name:    "an expired pair was swapped into service",
			mutate:  func(e *BoltCertRotationEvidence) { rotationStep(e, "expired-leaf").ReloadOK = true },
			check:   checkBoltCertRotation,
			wantSub: "ACCEPTED broken material",
		},
		{
			name: "the expired pair was refused for the wrong reason",
			mutate: func(e *BoltCertRotationEvidence) {
				rotationStep(e, "expired-leaf").ValidityRefused = false
			},
			check:   checkBoltCertRotation,
			wantSub: "NOT refused for its validity window",
		},
		{
			name: "a parse fault was reported as a validity refusal",
			mutate: func(e *BoltCertRotationEvidence) {
				rotationStep(e, "torn-key").ValidityRefused = true
			},
			check:   checkBoltCertRotation,
			wantSub: "reported as one",
		},
		{
			name: "no validity refusal was observed anywhere",
			mutate: func(e *BoltCertRotationEvidence) {
				for i := range e.Steps {
					e.Steps[i].ValidityRefused = false
				}
			},
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "refused for its VALIDITY WINDOW",
		},
		{
			name: "the not-yet-valid arm installed torn material",
			mutate: func(e *BoltCertRotationEvidence) {
				rotationStep(e, "not-yet-valid-leaf").KeyBytes = 40
			},
			check:   checkBoltCertRotationNonVacuity,
			wantSub: "must install INTACT material",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanRotationEvidence()
			tc.mutate(&ev)
			v := tc.check(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the oracle cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltCertRotation_ScenarioPasses drives the catalogue scenario.
func TestBoltCertRotation_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBoltCertRotation)
	if !ok {
		t.Fatalf("scenario %q not in catalogue", ScenarioBoltCertRotation)
	}
	rep, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("scenario run: %v", err)
	}
	if rep != nil {
		t.Errorf("scenario failed:\n%s", rep.String())
	}
}

// TestBoltCertRotation_LeavesNoTempDir pins the temp-hygiene contract: the
// projection directory must not outlive the run.
func TestBoltCertRotation_LeavesNoTempDir(t *testing.T) {
	defer goleak.VerifyNone(t)

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "sim-cert-rotation-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if _, err := RunBoltCertRotation(context.Background(), certRotationDefaultSeed); err != nil {
		t.Fatalf("RunBoltCertRotation: %v", err)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "sim-cert-rotation-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(after) > len(before) {
		t.Errorf("run left %d temp directory(ies) behind: %v", len(after)-len(before), after)
	}
}

// TestBoltCertRotation_ServedLeafParses is a belt-and-braces check that the
// generated certificates are well formed x509 (the handshake would also catch it,
// but this attributes a fixture defect to the fixture).
func TestBoltCertRotation_ServedLeafParses(t *testing.T) {
	t.Parallel()

	p, err := newSimCertPair(NewSeed(9), "parse-check")
	if err != nil {
		t.Fatalf("newSimCertPair: %v", err)
	}
	block, rest := pem.Decode(p.CertPEM)
	if block == nil {
		t.Fatal("generated certificate is not valid PEM")
	}
	if len(rest) != 0 {
		t.Errorf("generated certificate PEM has %d trailing bytes", len(rest))
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if leaf.Subject.CommonName != "parse-check" {
		t.Errorf("CN %q, want %q", leaf.Subject.CommonName, "parse-check")
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != certRotationHost {
		t.Errorf("DNSNames %v, want [%s]", leaf.DNSNames, certRotationHost)
	}
}

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// validityTestHost is the SAN every pair below carries and the name the
// verifying client asks for. A modern TLS client ignores the Common Name for
// name verification, so the SAN is what makes the handshake oracle meaningful.
const validityTestHost = "reload.test.local"

// writeValidityPair generates a self-signed ECDSA leaf for [validityTestHost]
// with the given validity window and writes the (cert, key) pair to
// dir/<name>.crt and dir/<name>.key, returning the two paths.
//
// The window is a parameter because that is the whole point: a fixture whose
// NotAfter is already past is how an EXPIRED rotation is expressed without
// waiting for real time to pass.
func writeValidityPair(t *testing.T, dir, cn string, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              []string{validityTestHost},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	certPath := filepath.Join(dir, cn+".crt")
	keyPath := filepath.Join(dir, cn+".key")
	writeFileOrFail(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFileOrFail(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath
}

// writeFileOrFail writes content to path and bumps its mtime one second into the
// future, so the reloader's mtime short-circuit cannot mistake a rewrite for "no
// change" on a coarse-grained filesystem.
func writeFileOrFail(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	stamp := time.Now().Add(time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// copyPairOnto overwrites the (cert, key) files the reloader watches with the
// contents of another pair — the file-level equivalent of a cert-manager
// rotation landing on disk.
func copyPairOnto(t *testing.T, dstCert, dstKey, srcCert, srcKey string) {
	t.Helper()
	for _, m := range []struct{ dst, src string }{{dstCert, srcCert}, {dstKey, srcKey}} {
		b, err := os.ReadFile(m.src)
		if err != nil {
			t.Fatalf("read %s: %v", m.src, err)
		}
		writeFileOrFail(t, m.dst, b)
	}
}

// handshakeServed completes a real TLS 1.3 handshake against whatever the
// reloader currently serves, evaluated at the instant at, and returns the served
// leaf's Common Name.
//
// A completed handshake proves three things a pointer comparison cannot: the
// served certificate parses, its name matches, and its private key genuinely
// corresponds to the public key inside it. It is the oracle the acceptance
// criteria ask for — "still handshaking", not merely "still installed".
func handshakeServed(t *testing.T, r *CertReloader, at time.Time) (string, error) {
	t.Helper()
	cert, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: validityTestHost})
	if err != nil {
		return "", fmt.Errorf("GetCertificate: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return "", errors.New("served certificate carries no DER")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("parse served leaf: %w", err)
	}
	cn := leaf.Subject.CommonName

	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	srvErr := make(chan error, 1)
	go func() {
		tlsSrv := tls.Server(serverConn, &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: r.GetCertificate,
			Time:           func() time.Time { return at },
		})
		defer func() { _ = tlsSrv.Close() }()
		if hsErr := tlsSrv.Handshake(); hsErr != nil {
			srvErr <- hsErr
			return
		}
		_, wErr := tlsSrv.Write([]byte{'k'})
		srvErr <- wErr
	}()

	tlsCli := tls.Client(clientConn, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: validityTestHost,
		RootCAs:    roots,
		Time:       func() time.Time { return at },
	})
	if hsErr := tlsCli.Handshake(); hsErr != nil {
		<-srvErr
		return cn, fmt.Errorf("client handshake: %w", hsErr)
	}
	var b [1]byte
	if _, rErr := io.ReadFull(tlsCli, b[:]); rErr != nil {
		<-srvErr
		return cn, fmt.Errorf("read after handshake: %w", rErr)
	}
	if sErr := <-srvErr; sErr != nil {
		return cn, fmt.Errorf("server handshake: %w", sErr)
	}
	return cn, nil
}

// TestCertReloader_ReloadRefusesLeafOutsideValidityWindow is the rmp #2557
// regression: rotating to a leaf that cannot serve must not evict the one that
// can. Before the fix Reload validated only the PAIRING, so both arms below
// returned nil, swapped the doomed leaf in, and every subsequent handshake
// failed — the previous, working certificate gone.
//
// Each arm asserts all three halves of the contract: the refusal is reported,
// the served certificate is still the original one, and it STILL COMPLETES A
// HANDSHAKE.
func TestCertReloader_ReloadRefusesLeafOutsideValidityWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name                string
		notBefore, notAfter time.Time
		wantErrContains     string
	}{
		{
			name:            "expired",
			notBefore:       now.Add(-48 * time.Hour),
			notAfter:        now.Add(-1 * time.Hour),
			wantErrContains: "expired at",
		},
		{
			name:            "not yet valid",
			notBefore:       now.Add(1 * time.Hour),
			notAfter:        now.Add(48 * time.Hour),
			wantErrContains: "is not valid until",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			certPath, keyPath := writeValidityPair(t, dir, "live", now.Add(-time.Hour), now.Add(time.Hour))
			r, err := NewCertReloader(certPath, keyPath, func(error) {})
			if err != nil {
				t.Fatalf("NewCertReloader: %v", err)
			}
			live, err := r.GetCertificate(nil)
			if err != nil {
				t.Fatalf("GetCertificate(live): %v", err)
			}
			if cn, hsErr := handshakeServed(t, r, now); hsErr != nil || cn != "live" {
				t.Fatalf("baseline handshake: cn=%q err=%v; the arm proves nothing unless the live pair works first", cn, hsErr)
			}

			// The rotation lands: a well-formed, correctly paired, but
			// unusable certificate replaces the files on disk.
			rotCert, rotKey := writeValidityPair(t, dir, "rotated", tc.notBefore, tc.notAfter)
			copyPairOnto(t, certPath, keyPath, rotCert, rotKey)

			err = r.Reload()
			if err == nil {
				t.Fatal("Reload ACCEPTED a leaf outside its validity window: the live certificate was evicted for one that cannot handshake")
			}
			if !errors.Is(err, ErrCertOutsideValidity) {
				t.Errorf("Reload error does not wrap ErrCertOutsideValidity, so a caller cannot tell a doomed renewal from unreadable material: %v", err)
			}
			if got := err.Error(); !strings.Contains(got, tc.wantErrContains) {
				t.Errorf("Reload error %q does not name the window boundary (want substring %q)", got, tc.wantErrContains)
			}
			if after, _ := r.GetCertificate(nil); after != live {
				t.Error("Reload swapped the live certificate anyway")
			}
			cn, hsErr := handshakeServed(t, r, now)
			if hsErr != nil {
				t.Fatalf("the served certificate no longer completes a handshake after the refused rotation: %v", hsErr)
			}
			if cn != "live" {
				t.Errorf("certificate in service is %q, want %q", cn, "live")
			}
		})
	}
}

// TestCertReloader_ValidityIsEvaluatedAgainstTheInjectedClock is the control for
// the test above: it proves the refusal is a real comparison against a clock and
// not an unconditional rejection of the second load.
//
// The rotated pair's window lies ENTIRELY IN THE PAST of real wall time — the
// same fixture shape the expired arm refuses — yet with the clock pinned inside
// that window the swap is accepted and the new certificate handshakes at that
// same instant. Only a reloader that reads the injected clock can pass both.
func TestCertReloader_ValidityIsEvaluatedAgainstTheInjectedClock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A window in the past, and an instant inside it.
	past := time.Now().Add(-72 * time.Hour)
	pinned := past.Add(24 * time.Hour)

	certPath, keyPath := writeValidityPair(t, dir, "live", past, past.Add(48*time.Hour))
	r, err := NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	r.SetClock(clock.NewFake(pinned))
	live, _ := r.GetCertificate(nil)

	rotCert, rotKey := writeValidityPair(t, dir, "rotated", past, past.Add(48*time.Hour))
	copyPairOnto(t, certPath, keyPath, rotCert, rotKey)
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload refused a leaf that IS valid at the injected instant %s: %v", pinned.UTC().Format(time.RFC3339), err)
	}
	after, _ := r.GetCertificate(nil)
	if after == live {
		t.Fatal("Reload did not swap a valid pair")
	}
	cn, hsErr := handshakeServed(t, r, pinned)
	if hsErr != nil {
		t.Fatalf("the rotated certificate does not handshake at the injected instant: %v", hsErr)
	}
	if cn != "rotated" {
		t.Errorf("certificate in service is %q, want %q", cn, "rotated")
	}

	// And the mirror: with the clock left at real time the SAME material is
	// refused, so the acceptance above is attributable to the clock alone.
	r2, err := NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader(second): %v", err)
	}
	againCert, againKey := writeValidityPair(t, dir, "rotated-again", past, past.Add(48*time.Hour))
	copyPairOnto(t, certPath, keyPath, againCert, againKey)
	if err := r2.Reload(); !errors.Is(err, ErrCertOutsideValidity) {
		t.Fatalf("the same expired material was accepted against the REAL clock: err=%v", err)
	}
}

// TestCertReloader_InitialLoadIsNotGatedOnValidity locks the deliberate
// asymmetry documented on Reload: the validity check protects a WORKING
// certificate from being replaced, and at construction there is none to protect.
//
// Refusing here would make a host whose clock has not yet synchronised unable to
// start at all — strictly worse than serving material its correctly-clocked
// clients may well accept — so the initial load stays gated on read/parse/pair
// only. If that decision is ever revisited, this test is what must change with
// it, deliberately.
func TestCertReloader_InitialLoadIsNotGatedOnValidity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	certPath, keyPath := writeValidityPair(t, dir, "expired-at-boot", now.Add(-48*time.Hour), now.Add(-time.Hour))
	r, err := NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader refused an expired pair at the INITIAL load: %v", err)
	}
	if _, err := r.GetCertificate(nil); err != nil {
		t.Fatalf("GetCertificate after an accepted initial load: %v", err)
	}
}

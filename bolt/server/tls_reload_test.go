package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestPair generates a self-signed ECDSA leaf cert and writes
// the (cert.pem, key.pem) pair to dir, returning their paths and
// the common name used in the cert so the test can later assert
// which cert is currently in service.
func writeTestPair(t *testing.T, dir, cn string) (string, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPath := filepath.Join(dir, fmt.Sprintf("%s.crt", cn))
	keyPath := filepath.Join(dir, fmt.Sprintf("%s.key", cn))
	certPEM, _ := os.Create(certPath)
	keyPEM, _ := os.Create(keyPath)
	defer func() { _ = certPEM.Close() }()
	defer func() { _ = keyPEM.Close() }()
	_ = pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	_ = pem.Encode(keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPath, keyPath
}

func TestCertReloader_InitialLoadAndGetCertificate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := writeTestPair(t, dir, "initial")
	r, err := NewCertReloader(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	got, err := r.GetCertificate(nil)
	if err != nil || got == nil {
		t.Fatalf("GetCertificate(initial): cert=%v err=%v", got, err)
	}
}

func TestCertReloader_ReloadSwapsCertificate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := writeTestPair(t, dir, "v1")
	r, err := NewCertReloader(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	v1, _ := r.GetCertificate(nil)

	// Overwrite the same paths with a fresh cert (different
	// SerialNumber); the test pair generator uses time.Now's
	// nanoseconds for the serial so two consecutive calls produce
	// distinguishable certs.
	time.Sleep(20 * time.Millisecond)  // ensure mtime advances on coarse-grained filesystems
	_, _ = writeTestPair(t, dir, "v1") // overwrites cert and key in place
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	v2, _ := r.GetCertificate(nil)
	if v1 == v2 {
		t.Fatalf("Reload did not swap the *tls.Certificate pointer")
	}
}

func TestCertReloader_ReloadParseFailureKeepsLiveCert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := writeTestPair(t, dir, "good")
	r, err := NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	good, _ := r.GetCertificate(nil)

	// Corrupt the cert file in place; Reload must surface the parse
	// error and leave the live certificate untouched.
	if err := os.WriteFile(certPath, []byte("not a PEM cert"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	// Force the mtime detection to pick the change up immediately.
	now := time.Now().Add(time.Second)
	_ = os.Chtimes(certPath, now, now)

	if err := r.Reload(); err == nil {
		t.Fatal("Reload of corrupt cert returned nil; want error")
	}
	stillGood, _ := r.GetCertificate(nil)
	if stillGood != good {
		t.Fatal("Reload mutated the live cert on a parse failure")
	}
}

func TestCertReloader_InitialLoadFailsWhenFilesMissing(t *testing.T) {
	t.Parallel()
	_, err := NewCertReloader("/does/not/exist.crt", "/does/not/exist.key", nil)
	if err == nil {
		t.Fatal("NewCertReloader with missing files returned nil; want error")
	}
}

package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestGoleak_Sessions_AllTransitions starts a fresh bolt/server.Server, drives
// 64 concurrent sessions through every reachable Bolt v5 state-machine
// transition, then verifies goleak.VerifyNone after shutdown.
//
// Acceptance criteria (T624):
//  1. go test -race ./bolt/server/... passes.
//  2. goleak clean at teardown.
//  3. TLS hot-reload watcher goroutine terminates on Shutdown.
//  4. Bookmark store and routing-table publisher exit cleanly.
//  5. Layer: short.
//
// State-machine transitions exercised per session:
//   - wire handshake (20-byte preamble + 4-byte response)
//   - HELLO → READY
//   - RUN (auto-commit) → STREAMING → PULL(all) → READY
//   - ROUTE → READY
//   - BEGIN → TX_READY → RUN → TX_STREAMING → PULL(all) → TX_READY → COMMIT → READY
//   - BEGIN → TX_READY → RUN → TX_STREAMING → PULL(all) → TX_READY → ROLLBACK → READY
//   - GOODBYE → DEFUNCT
//
// Not parallel: goleak.IgnoreCurrent captures a goroutine snapshot at call
// time; parallel siblings in flight would pollute the snapshot. The
// package-level shared server (started in TestMain) is not used so that this
// test controls the server lifecycle exactly.
func TestGoleak_Sessions_AllTransitions(t *testing.T) {
	const numSessions = 64

	// Capture the baseline goroutine set before starting the test server so
	// that runtime goroutines spawned at process start are excluded.
	opts := []goleak.Option{goleak.IgnoreCurrent()}

	eng := newEngine(t)
	srv, err := server.NewServer(eng, server.Options{ConnTimeout: 5 * time.Second, Auth: server.NoAuthHandler{}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()

	serveCtx, serveCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(serveCtx, ln)
	}()
	// Give the server a moment to enter Accept before connecting clients.
	time.Sleep(10 * time.Millisecond)

	// Drive numSessions concurrent sessions.
	done := make(chan struct{}, numSessions)
	for i := range numSessions {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			sessionAllTransitions(t, addr, i)
		}()
	}
	for range numSessions {
		<-done
	}

	// Cancel the listener so Serve returns cleanly.
	serveCancel()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		t.Log("TestGoleak_Sessions_AllTransitions: Serve did not return in 5 s")
	}

	// Brief settling time for OS-level teardown.
	time.Sleep(20 * time.Millisecond)

	if err := goleak.Find(opts...); err != nil {
		t.Errorf("goroutine leak after server shutdown: %v", err)
	}
}

// TestGoleak_TLSReloadWatcher_TerminatesOnStop verifies that a
// CertReloader.Watch goroutine exits cleanly when its stop channel is closed,
// satisfying T624 AC3 (TLS hot-reload watcher terminates on Shutdown).
//
// Not parallel: goleak.IgnoreCurrent captures a baseline snapshot.
func TestGoleak_TLSReloadWatcher_TerminatesOnStop(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := goLeakWriteTestPair(t, dir, "goleak-watcher")

	r, err := server.NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	// Snapshot goroutines already alive before the watcher starts.
	opts := []goleak.Option{goleak.IgnoreCurrent()}

	stop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		r.Watch(10*time.Millisecond, stop)
	}()

	// Let the watcher complete at least two reload ticks.
	time.Sleep(40 * time.Millisecond)

	// Shut down the watcher (mirrors the signal sent by a real Shutdown hook).
	close(stop)
	select {
	case <-watchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch goroutine did not exit after stop channel was closed")
	}

	if err := goleak.Find(opts...); err != nil {
		t.Errorf("goroutine leak after TLS watcher stopped: %v", err)
	}
}

// sessionAllTransitions dials addr and drives a single Bolt v5 session
// through every reachable state-machine transition, then sends GOODBYE.
func sessionAllTransitions(t *testing.T, addr string, _ int) {
	t.Helper()
	c := newBoltTestClient(t, addr)
	defer c.close(t)

	// NEGOTIATION → wire handshake → NEGOTIATION
	c.negotiate(t)

	// NEGOTIATION → HELLO → READY
	c.hello(t)

	// READY → RUN (auto-commit) → STREAMING → PULL(all) → READY
	c.run(t, "MATCH (n) RETURN n", nil)
	c.pullAll(t)

	// READY → ROUTE → READY
	c.route(t)

	// READY → BEGIN → TX_READY → RUN → TX_STREAMING → PULL → TX_READY → COMMIT → READY
	c.begin(t)
	c.run(t, "MATCH (n) RETURN n", nil)
	c.pullAll(t)
	c.commit(t)

	// READY → BEGIN → TX_READY → RUN → TX_STREAMING → PULL → TX_READY → ROLLBACK → READY
	c.begin(t)
	c.run(t, "MATCH (n) RETURN n", nil)
	c.pullAll(t)
	c.rollback(t)

	// READY → GOODBYE → DEFUNCT
	c.goodbye(t)
}

// goLeakWriteTestPair generates a self-signed ECDSA P-256 certificate and key
// in dir, returning the paths to the PEM files. Used only by tests in this
// file to avoid depending on the internal writeTestPair helper that lives in
// package server (not server_test).
func goLeakWriteTestPair(t *testing.T, dir, cn string) (certPath, keyPath string) {
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
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPath = filepath.Join(dir, fmt.Sprintf("%s.crt", cn))
	keyPath = filepath.Join(dir, fmt.Sprintf("%s.key", cn))

	certFile, err := os.Create(certPath) // #nosec G304 — temp dir, test-only
	if err != nil {
		t.Fatalf("os.Create cert: %v", err)
	}
	defer func() { _ = certFile.Close() }()
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("pem.Encode cert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("x509.MarshalECPrivateKey: %v", err)
	}
	keyFile, err := os.Create(keyPath) // #nosec G304 — temp dir, test-only
	if err != nil {
		t.Fatalf("os.Create key: %v", err)
	}
	defer func() { _ = keyFile.Close() }()
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("pem.Encode key: %v", err)
	}
	return certPath, keyPath
}

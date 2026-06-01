package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// generateSelfSigned creates an ephemeral ECDSA self-signed TLS certificate
// and returns a *tls.Config suitable for use as a server TLS config. The
// certificate is not written to disk.
func generateSelfSigned(t *testing.T) *tls.Config {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	var certPEM, keyPEM bytes.Buffer
	if err := pem.Encode(&certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encode cert PEM: %v", err)
	}
	if err := pem.Encode(&keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key PEM: %v", err)
	}

	tlsCert, err := tls.X509KeyPair(certPEM.Bytes(), keyPEM.Bytes())
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}
}

// TestServer_TLS starts a server with a self-signed certificate, connects with
// a TLS dialer using InsecureSkipVerify=true, performs the Bolt handshake, sends
// HELLO, and verifies the SUCCESS response.
func TestServer_TLS(t *testing.T) {
	eng := newEngine(t)
	tlsCfg := generateSelfSigned(t)

	srv, err := server.NewServer(eng, server.Options{
		TLSConfig:   tlsCfg,
		ConnTimeout: 5 * time.Second,
		Auth:        server.NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, ln)
	}()

	// Drain the Serve goroutine on cleanup. Goroutine leak checking is handled
	// by goleak.Find in TestMain after all servers have been shut down.
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("warning: Serve goroutine did not exit in cleanup")
		}
	})

	// Give the server a moment to start accepting.
	time.Sleep(10 * time.Millisecond)

	// Dial with TLS, skipping certificate verification (self-signed cert).
	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // self-signed test cert; not a production path
	})
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer tlsConn.Close() //nolint:errcheck

	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))

	// Force TLS handshake (implicitly done by first I/O, but explicit is cleaner).
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}

	// Bolt protocol negotiation.
	boltHandshake(t, tlsConn)

	// HELLO exchange.
	success := sendHello(t, tlsConn)

	if success.Metadata == nil {
		t.Fatal("SUCCESS metadata is nil")
	}
	if _, ok := success.Metadata["server"]; !ok {
		t.Error("SUCCESS metadata missing 'server'")
	}

	// Shutdown the server.
	tlsConn.Close() //nolint:errcheck
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

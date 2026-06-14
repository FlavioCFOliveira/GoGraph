package server_test

// security_tls_floor_test.go — DEFENSE LOCK-IN for the TLS version floor
// (security audit, transport-security cluster).
//
// default_tls_test.go pins the SHAPE of DefaultTLSConfig (MinVersion >= TLS 1.2,
// AEAD/ECDHE-only cipher list, MaxVersion unset, no InsecureSkipVerify). This
// file adds the missing END-TO-END negative test: a live listener wrapped with
// DefaultTLSConfig must actually REFUSE a ClientHello that caps itself at
// TLS 1.0 or TLS 1.1, and must still accept a modern (TLS 1.2+) client. A config
// whose floor is silently lowered would pass the shape test if the const were
// edited in lockstep, but would fail here because a real downgraded handshake
// would succeed.
//
// Layer: short. The server is torn down via Shutdown; the TLS dials are bounded
// by deadlines.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// secBoltHardenedTLSConfig returns the production DefaultTLSConfig populated
// with an ephemeral self-signed certificate, so a live listener exercises the
// real hardened floor rather than a test-local config.
func secBoltHardenedTLSConfig(t *testing.T) *tls.Config {
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
	cfg := server.DefaultTLSConfig() // the real hardened baseline under test
	cfg.Certificates = []tls.Certificate{{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}}
	return cfg
}

// secBoltStartTLSServer starts a Bolt server on a random port wrapped with cfg,
// returning its address. Teardown is registered via t.Cleanup.
func secBoltStartTLSServer(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	srv, err := server.NewServer(newEngine(t), server.Options{
		TLSConfig:   cfg,
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
	go func() { serveErr <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("secBoltStartTLSServer: Serve did not exit in cleanup")
		}
	})
	time.Sleep(10 * time.Millisecond)
	return addr
}

// TestSec_Bolt_TLSLegacyVersionsRefused asserts the hardened floor refuses a
// client that caps itself at TLS 1.0 or TLS 1.1. The dial must fail at the
// handshake — the server must not negotiate a sub-1.2 session.
func TestSec_Bolt_TLSLegacyVersionsRefused(t *testing.T) {
	t.Parallel()

	addr := secBoltStartTLSServer(t, secBoltHardenedTLSConfig(t))

	cases := []struct {
		name string
		max  uint16
	}{
		{"TLS_1_0", tls.VersionTLS10},
		{"TLS_1_1", tls.VersionTLS11},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dialer := &tls.Dialer{Config: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // self-signed test cert; verification is not what is under test
				MinVersion:         tc.max,
				MaxVersion:         tc.max, // force the legacy version only
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = conn.Close()
				t.Fatalf("%s handshake succeeded against a TLS 1.2-floor server; want refusal", tc.name)
			}
			// A protocol-version error is the expected failure. We do not pin the
			// exact message (it varies by Go version); the contract is that the
			// handshake did NOT complete.
			t.Logf("%s correctly refused: %v", tc.name, err)
		})
	}
}

// TestSec_Bolt_TLSModernVersionAccepted is the safety pin: a client offering up
// to TLS 1.3 must complete the handshake and negotiate >= TLS 1.2, proving the
// floor refuses only legacy versions and does not break legitimate clients.
func TestSec_Bolt_TLSModernVersionAccepted(t *testing.T) {
	t.Parallel()

	addr := secBoltStartTLSServer(t, secBoltHardenedTLSConfig(t))

	dialer := &tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // self-signed test cert; not the property under test
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("modern TLS client refused by hardened server: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("dialed connection is %T, want *tls.Conn", conn)
	}
	if v := tlsConn.ConnectionState().Version; v < tls.VersionTLS12 {
		t.Fatalf("negotiated TLS version 0x%04x, want >= TLS 1.2", v)
	}
}

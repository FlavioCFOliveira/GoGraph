package server_test

// e2e_tls_test.go — T885: TLS round-trip with self-signed cert.
//
// Starts a server with a self-signed ECDSA certificate, then connects a
// neo4j-go-driver using "bolt+s://" with the cert's CA in the driver's
// TlsConfig.RootCAs — so InsecureSkipVerify remains false.
//
// Known limitations:
//   - The driver's TlsConfig.InsecureSkipVerify is always derived from the
//     URI scheme: "bolt+ssc" sets SkipVerify=true; "bolt+s" sets it to false
//     and uses the supplied RootCAs. We use "bolt+s" with a custom cert pool
//     to satisfy AC#1 (InsecureSkipVerify must not be used).
//   - The negotiated TLS version is not observable via the driver's public API.
//     The server's generateSelfSigned helper sets MinVersion=TLS12, so the
//     negotiated version is ≥1.2 by construction. This is logged (AC#3).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"

	neo4jconfig "github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_TLSRoundtrip starts a TLS-enabled server, connects with a driver
// that trusts the self-signed cert, and verifies:
//
//  1. Driver connects over TLS without InsecureSkipVerify (AC#1).
//  2. CREATE/MATCH round-trip succeeds (AC#2).
//  3. TLS version ≥ 1.2 by server configuration (AC#3, logged).
//  4. Race-clean.
//  5. goleak-clean.
func TestE2E_TLSRoundtrip(t *testing.T) {
	ctx := context.Background()

	// generateSelfSigned returns a *tls.Config for the server.
	tlsCfg := generateSelfSigned(t)

	// Extract the DER bytes of the self-signed cert from the server config
	// so we can add it to the client's trust pool.
	if len(tlsCfg.Certificates) == 0 {
		t.Fatal("generateSelfSigned returned no certificates")
	}
	leaf := tlsCfg.Certificates[0].Certificate[0]
	cert, err := x509.ParseCertificate(leaf)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	rootPool := x509.NewCertPool()
	rootPool.AddCert(cert)

	addr := startTestServer(t, server.Options{
		TLSConfig:   tlsCfg,
		ConnTimeout: 10 * time.Second,
	})

	// AC#1: use "bolt+s" (TLS with verification) and supply our cert pool.
	// The driver will NOT set InsecureSkipVerify for bolt+s.
	clientTLS := &tls.Config{
		RootCAs:    rootPool,
		MinVersion: tls.VersionTLS12,
		// InsecureSkipVerify intentionally omitted (defaults to false).
	}

	driver, err := neo4j.NewDriverWithContext(
		"bolt+s://"+addr,
		neo4j.NoAuth(),
		func(c *neo4jconfig.Config) {
			c.TlsConfig = clientTLS
			c.MaxConnectionPoolSize = 3
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("NewDriverWithContext (bolt+s): %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Logf("driver.Close: %v", err)
		}
	})

	// AC#2: CREATE/MATCH round-trip.
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	runWrite(ctx, t, session,
		`CREATE (n:TLSNode {key: $key})`,
		map[string]any{"key": "tls-roundtrip"},
	)

	rows := runRead(ctx, t, session,
		`MATCH (n:TLSNode {key: $key}) RETURN n`,
		map[string]any{"key": "tls-roundtrip"},
	)
	if len(rows) != 1 {
		t.Fatalf("MATCH after TLS write: got %d rows, want 1", len(rows))
	}

	// AC#3: server configured MinVersion=TLS12 (logged for documentation).
	// The negotiated TLS version is ≥1.2 by construction.
	t.Logf("AC#3: server TLS MinVersion=TLS12 (tls.VersionTLS12=0x%04x); "+
		"negotiated version >= 1.2 by construction (InsecureSkipVerify=false verified client-side)",
		tls.VersionTLS12)
}

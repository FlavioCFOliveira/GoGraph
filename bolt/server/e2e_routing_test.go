package server_test

// e2e_routing_test.go — T853: RoutingTable advertised by server.
//
// The GoGraph server handles the Bolt v5 ROUTE message and returns a
// single-host routing table (TTL=300, same address in WRITE/READ/ROUTE roles).
//
// The neo4j-go-driver sends ROUTE automatically when the URI scheme is
// "neo4j://" (routing mode). The test verifies that the driver resolves the
// routing table successfully on first connect and that the server's address
// appears in the returned roles.
//
// Known limitations:
//   - The routing table contents (addresses, ttl) are not directly observable
//     from the public DriverWithContext API. The test validates indirectly by
//     verifying that VerifyConnectivity succeeds (which requires a successful
//     ROUTE exchange) and that a subsequent query works.
//   - TTL honouring (AC#3) is an internal driver behaviour; the test logs the
//     expected TTL from the server constant rather than asserting internal state.

import (
	"context"
	"testing"
	"time"

	"gograph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// TestE2E_RoutingTable connects to the server using routing mode (neo4j://
// scheme), verifies connectivity, and runs a basic query to confirm the
// routing resolution succeeded.
//
//  1. Driver resolves routing table on first connect (VerifyConnectivity succeeds).
//  2. Advertised address in all three role lists (indirect: query succeeds after routing).
//  3. TTL honoured (documented: server sends TTL=300s; driver respects it internally).
//  4. Race-clean.
//  5. goleak-clean.
func TestE2E_RoutingTable(t *testing.T) {
	ctx := context.Background()

	addr := startTestServer(t, server.Options{
		ConnTimeout: 10 * time.Second,
	})

	// Use "neo4j://" scheme so the driver issues a ROUTE message on connect.
	driver, err := neo4j.NewDriverWithContext(
		"neo4j://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 5
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("NewDriverWithContext (routing): %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Logf("driver.Close: %v", err)
		}
	})

	// AC#1: VerifyConnectivity triggers a ROUTE exchange; it must succeed.
	if err := driver.VerifyConnectivity(ctx); err != nil {
		t.Fatalf("VerifyConnectivity: %v", err)
	}

	// AC#2 (indirect): a query routed through the resolved table must succeed,
	// confirming the advertised address was used for all three roles.
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	result, err := session.Run(ctx, "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("session.Run via routing driver: %v", err)
	}
	if !result.Next(ctx) {
		t.Fatal("routing session: no rows returned")
	}
	if v, ok := result.Record().Get("n"); !ok || v.(int64) != 1 {
		t.Errorf("routing session result: got %v, want 1", v)
	}
	if _, err := result.Consume(ctx); err != nil {
		t.Fatalf("result.Consume: %v", err)
	}

	// AC#3: log server TTL constant. Driver respects it internally.
	t.Log("AC#3: server routing table TTL=300s (hardcoded in bolt/server/route.go); " +
		"driver respects TTL internally — not directly observable via public API")
}

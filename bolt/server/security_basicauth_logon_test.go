package server_test

// security_basicauth_logon_test.go — REGRESSION GATE for SECURITY-GAP #1470
// (security audit, Bolt/auth cluster), plus a positive control proving
// BasicAuthHandler itself works.
//
// Background. Bolt 5.1 split authentication out of HELLO into a separate LOGON
// message: a 5.1+ client sends a credential-less HELLO first (carrying only
// driver metadata), then a LOGON carrying the actual credentials. The
// neo4j-go-driver negotiates the highest mutual version — this server advertises
// up to v5.6 — so a real driver always uses the split HELLO/LOGON flow.
//
// Before the fix, this server's handleHello ran the configured AuthHandler
// against the HELLO's (absent) credentials regardless of the negotiated version.
// Against a credentialed BasicAuthHandler the empty HELLO therefore FAILED
// authentication and the connection was torn down (DEFUNCT) before the client's
// LOGON — carrying the correct credentials — was ever read, so a
// correctly-configured client could NOT connect.
//
// The fix (task #1470) gates HELLO authentication on the negotiated Bolt
// version: on Bolt >= 5.1 the server does not authenticate at HELLO, advances to
// the pre-LOGON StateAuthentication, and authenticates the credential-bearing
// LOGON instead; on Bolt <= 5.0 it keeps the inline-HELLO flow. The test below
// drives the real neo4j-go-driver (which always uses >= 5.1) and asserts that
// correct credentials now connect and run a query, while the negative control
// and the Bolt 5.0-style HELLO-carried-credentials control still hold.
//
// Layer: short. The server and driver are torn down via t.Cleanup; every call
// is bounded by a context deadline.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// secBoltCredentials are the credentials the test server's BasicAuthHandler
// accepts. The driver is configured with exactly these, so any connection
// failure is attributable to the HELLO/LOGON flow, not to wrong credentials.
const (
	secBoltUser = "alice"
	secBoltPass = "correct-horse-battery-staple"
)

// secBoltBasicAuthServer starts a server whose only accepted credentials are
// secBoltUser/secBoltPass via a constant-time BasicAuthHandler, and returns its
// address.
func secBoltBasicAuthServer(t *testing.T) string {
	t.Helper()
	return startTestServerWithEngine(t, newEngine(t), server.Options{
		Auth:        server.BasicAuthHandler{Validate: server.ConstantTimeValidate(secBoltUser, secBoltPass)},
		ConnTimeout: 5 * time.Second,
	})
}

// TestSec_Bolt_BasicAuth_RealDriverLogonFlow is the REGRESSION GATE for #1470.
//
// The fix gates HELLO authentication on the negotiated Bolt version: a real
// neo4j-go-driver (>= 5.1) sends a credential-less HELLO then a credential-bearing
// LOGON, and the server now authenticates the LOGON. With the CORRECT credentials,
// VerifyConnectivity must succeed and a "RETURN 1 AS n" query must yield int64(1).
// This fails on the unfixed code (correct credentials were rejected at HELLO).
func TestSec_Bolt_BasicAuth_RealDriverLogonFlow(t *testing.T) {
	t.Parallel()

	addr := secBoltBasicAuthServer(t)

	// The driver is given the CORRECT credentials.
	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.BasicAuth(secBoltUser, secBoltPass, ""),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 2
			c.ConnectionAcquisitionTimeout = 3 * time.Second
			c.SocketConnectTimeout = 3 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// With the fix, the split credential-less HELLO + credential-bearing LOGON
	// flow authenticates the correct credentials, so connectivity succeeds.
	if err := driver.VerifyConnectivity(ctx); err != nil {
		t.Fatalf("correct credentials must connect after #1470 fix: %v", err)
	}

	// And a query then runs on the authenticated connection, returning int64(1).
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	rows := runRead(ctx, t, session, "RETURN 1 AS n", nil)
	if len(rows) != 1 {
		t.Fatalf("RETURN 1 AS n: got %d rows, want 1", len(rows))
	}
	if n, ok := rows[0]["n"].(int64); !ok || n != 1 {
		t.Fatalf("RETURN 1 AS n: got %v (%T), want int64(1)", rows[0]["n"], rows[0]["n"])
	}
}

// TestSec_Bolt_BasicAuth_WrongCredentialsRejected is the negative control: the
// same server must reject WRONG credentials. This pins that the fail-closed
// behaviour is intact regardless of #1470 — the gap is that CORRECT credentials
// are also rejected, never that wrong ones are admitted.
func TestSec_Bolt_BasicAuth_WrongCredentialsRejected(t *testing.T) {
	t.Parallel()

	addr := secBoltBasicAuthServer(t)
	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.BasicAuth(secBoltUser, "the-wrong-password", ""),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 2
			c.ConnectionAcquisitionTimeout = 3 * time.Second
			c.SocketConnectTimeout = 3 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := driver.VerifyConnectivity(ctx); err == nil {
		t.Fatal("wrong credentials must be rejected (fail-closed), but connectivity succeeded")
	}
}

// TestSec_Bolt_BasicAuth_HelloCarriedCredentialsSucceeds is the POSITIVE
// CONTROL: it proves BasicAuthHandler itself authenticates correctly when the
// credentials arrive in the HELLO (the Bolt 5.0-style flow). Driving the wire
// directly with the low-level boltTestClient lets the test put the credentials
// in HELLO — exactly what #1470's split-flow drivers do NOT do — so a SUCCESS
// here isolates the defect to the HELLO-vs-LOGON timing rather than to the
// handler's credential checking.
func TestSec_Bolt_BasicAuth_HelloCarriedCredentialsSucceeds(t *testing.T) {
	t.Parallel()

	addr := secBoltBasicAuthServer(t)

	c := newBoltTestClient(t, addr)
	defer c.close(t)

	c.negotiate(t)

	// HELLO carrying the correct credentials (Bolt 5.0-style): scheme "basic".
	c.sendRequest(t, &proto.Hello{Extra: map[string]interface{}{
		"scheme":      "basic",
		"principal":   secBoltUser,
		"credentials": secBoltPass,
		"agent":       "sec-test/1.0",
	}})
	success := c.recvSuccess(t) // fails the test if a FAILURE comes back
	if success.Metadata == nil {
		t.Fatal("HELLO-with-credentials SUCCESS metadata is nil")
	}

	// And a query then runs, proving the connection is authenticated and usable.
	c.run(t, "RETURN 1 AS n", nil)
	records, _ := c.pullAll(t)
	if len(records) != 1 {
		t.Fatalf("expected 1 record from RETURN 1, got %d", len(records))
	}
	if len(records[0]) != 1 {
		t.Fatalf("expected 1 field, got %d", len(records[0]))
	}
	if v, ok := records[0][0].(int64); !ok || v != 1 {
		t.Fatalf("RETURN 1 record = %v (%T), want int64(1)", records[0][0], records[0][0])
	}
}

package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// newWireRoundTripServer starts a SimServer over a real bolt/server with a fresh
// finite-cap engine and registers teardown. It returns the server.
func newWireRoundTripServer(t *testing.T) *SimServer {
	t.Helper()
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("SimServer.Close: %v", err)
		}
	})
	return srv
}

// TestWire_HandshakeHelloRunPull is the task-1547/1548 acceptance round-trip: a
// real bolt/server accepts a SimConn and completes handshake + HELLO + RUN +
// PULL entirely in-memory, and the data created over the wire is read back over
// the wire. It proves the genuine wire path runs with no OS socket.
func TestWire_HandshakeHelloRunPull(t *testing.T) {
	t.Parallel()
	srv := newWireRoundTripServer(t)

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Write a node over the wire.
	runResp, err := c.Run(tmplCreatePerson, map[string]any{"name": "Ada", "age": int64(36)})
	if err != nil {
		t.Fatalf("RUN create: %v", err)
	}
	if f, ok := runResp.(*proto.Failure); ok {
		t.Fatalf("RUN create FAILURE: %s %s", f.Code, f.Message)
	}
	if _, term, err := c.PullAll(); err != nil {
		t.Fatalf("PULL create: %v", err)
	} else if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("PULL create FAILURE: %s %s", f.Code, f.Message)
	}

	// Read it back over the wire.
	if _, err := c.Run("MATCH (n:Person) RETURN n.name, n.age", nil); err != nil {
		t.Fatalf("RUN match: %v", err)
	}
	records, term, err := c.PullAll()
	if err != nil {
		t.Fatalf("PULL match: %v", err)
	}
	if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("PULL match FAILURE: %s %s", f.Code, f.Message)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if len(records[0].Data) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(records[0].Data))
	}
	if name, _ := records[0].Data[0].(string); name != "Ada" {
		t.Fatalf("name column: got %v, want Ada", records[0].Data[0])
	}
}

// TestWire_ExplicitTransactionRoundTrip drives BEGIN/RUN/COMMIT over the wire and
// confirms the committed write is visible to a subsequent autocommit read.
func TestWire_ExplicitTransactionRoundTrip(t *testing.T) {
	t.Parallel()
	srv := newWireRoundTripServer(t)
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if resp, err := c.Begin(); err != nil {
		t.Fatalf("BEGIN: %v", err)
	} else if f, ok := resp.(*proto.Failure); ok {
		t.Fatalf("BEGIN FAILURE: %s %s", f.Code, f.Message)
	}
	if _, err := c.Run(tmplCreatePerson, map[string]any{"name": "Grace", "age": int64(40)}); err != nil {
		t.Fatalf("RUN in tx: %v", err)
	}
	if _, _, err := c.PullAll(); err != nil {
		t.Fatalf("PULL in tx: %v", err)
	}
	if resp, err := c.Commit(); err != nil {
		t.Fatalf("COMMIT: %v", err)
	} else if f, ok := resp.(*proto.Failure); ok {
		t.Fatalf("COMMIT FAILURE: %s %s", f.Code, f.Message)
	}

	if _, err := c.Run("MATCH (n:Person {name:$name}) RETURN count(n)", map[string]any{"name": "Grace"}); err != nil {
		t.Fatalf("RUN verify: %v", err)
	}
	records, _, err := c.PullAll()
	if err != nil {
		t.Fatalf("PULL verify: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 count row, got %d", len(records))
	}
	if n, _ := records[0].Data[0].(int64); n != 1 {
		t.Fatalf("committed node not visible: count=%v", records[0].Data[0])
	}
}

package server_test

// default_timeouts_reach_test.go — rmp #2807.
//
// rmp #2807 set the four server-side timeout defaults to 0, which means
// DISABLED. This file is the end-to-end evidence for that: what a client
// actually experiences on a server the embedder configured with nothing at all.
//
// Every clause is paired with a CONTROL arm running the same script against a
// server whose bound is set to a small positive value. Without the control the
// disabled arm would prove nothing — "the transaction was still open after a
// second" passes just as well when the harness cannot observe a reclaim at all.
//
// Layer: short. The waits are sub-second and the controls are 200 ms bounds.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

const (
	// reachControlBound is the bound each control arm sets. It must be well
	// below reachSilence so the control's reclaim lands inside the window.
	reachControlBound = 200 * time.Millisecond
	// reachSilence is how long the client says nothing. Long enough for the
	// control arm's bounds to fire several times over, short enough for the
	// short test layer.
	reachSilence = 1500 * time.Millisecond
	// reachCeiling bounds how long a control arm waits to observe its reclaim
	// before declaring the harness broken.
	reachCeiling = 10 * time.Second
)

// TestDefaults_NoWallClockBoundOnAnIdleConnectionOrItsTransaction is the gate
// for two of the three "no bound" claims: an idle CONNECTION is not torn down,
// and the explicit TRANSACTION it left open is not reclaimed.
//
// The disabled arm runs a server built from a zero-value Options — the real
// default configuration, with ConnTimeout, MaxTxIdleTime and DefaultTxTimeout
// all 0 — opens a transaction, then falls silent for reachSilence. Afterwards
// the transaction must still be open AND the connection must still work.
//
// The control arm is the same script against a server with all three bounds at
// reachControlBound, where the transaction MUST be gone. It is what proves the
// harness can see a reclaim at all, and it is 7.5 times shorter than the silence
// the disabled arm survives.
func TestDefaults_NoWallClockBoundOnAnIdleConnectionOrItsTransaction(t *testing.T) {
	t.Parallel()

	t.Run("control-bounded-server-reclaims", func(t *testing.T) {
		t.Parallel()
		srv, addr := startTestServerVerbatimHandle(t, server.Options{
			ConnTimeout:      reachControlBound,
			MaxTxIdleTime:    reachControlBound,
			DefaultTxTimeout: reachControlBound,
		})
		client := newBoltTestClient(t, addr)
		defer client.close(t)
		openOneTransaction(t, client, srv)

		start := time.Now()
		for {
			if len(srv.Transactions()) == 0 {
				t.Logf("control: reclaimed after %v with every bound at %v", time.Since(start), reachControlBound)
				return
			}
			if time.Since(start) > reachCeiling {
				t.Fatalf("with ConnTimeout, MaxTxIdleTime and DefaultTxTimeout all at %v the transaction was STILL "+
					"open after %v; this harness cannot observe a reclaim, so the disabled arm below would be vacuous",
					reachControlBound, time.Since(start))
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("default-server-does-not-reclaim", func(t *testing.T) {
		t.Parallel()
		// Verbatim: no helper-substituted ConnTimeout. This is exactly what an
		// embedder writing server.Options{Auth: ...} gets.
		srv, addr := startTestServerVerbatimHandle(t, server.Options{})
		client := newBoltTestClient(t, addr)
		defer client.close(t)
		openOneTransaction(t, client, srv)

		time.Sleep(reachSilence)

		if open := srv.Transactions(); len(open) != 1 {
			t.Fatalf("after %v of client silence a DEFAULT-configured server lists %d open transactions; want 1. "+
				"The four timeout defaults are disabled (rmp #2807), so nothing on a timer may reclaim it",
				reachSilence, len(open))
		}

		// The connection must also still be usable. A read deadline armed at
		// time.Now().Add(0) — the hazard of defaulting ConnTimeout to zero —
		// would have torn this connection down long before now.
		client.run(t, "RETURN 1 AS n", nil)
		if records, _ := client.pullAll(t); len(records) != 1 {
			t.Fatalf("after %v of silence the connection returned %d records for `RETURN 1`; want 1", reachSilence, len(records))
		}
		client.rollback(t)
	})
}

// openOneTransaction drives a client through BEGIN plus one statement and
// asserts the server registry lists exactly one open transaction, so a later
// "still open" or "now gone" clause is not reading an empty registry.
func openOneTransaction(t *testing.T, client *boltTestClient, srv *server.Server) {
	t.Helper()
	client.negotiate(t)
	client.hello(t)
	client.begin(t)
	client.run(t, "CREATE (:ReachProbe {v: 1})", nil)
	client.pullAll(t)
	if open := srv.Transactions(); len(open) != 1 {
		t.Fatalf("before the silence the registry lists %d open transactions; want exactly 1", len(open))
	}
}

// TestDefaults_NoWallClockBoundOnAnAutocommitStatement is the gate for the third
// claim: an autocommit RUN on a default-configured server carries no deadline.
//
// The control arm sets DefaultStatementTimeout to ONE NANOSECOND. The context
// that bound installs has already expired by the time the engine reads it, so
// every autocommit statement fails — deterministically, with no sleeping and no
// contrived slow query. That is what proves the bound is live and that this
// harness sees it fire.
//
// The disabled arm runs the identical statement on a zero-value Options server
// and it must succeed.
func TestDefaults_NoWallClockBoundOnAnAutocommitStatement(t *testing.T) {
	t.Parallel()

	t.Run("control-one-nanosecond-bound-fails-the-statement", func(t *testing.T) {
		t.Parallel()
		_, addr := startTestServerVerbatimHandle(t, server.Options{
			DefaultStatementTimeout: time.Nanosecond,
		})
		client := newBoltTestClient(t, addr)
		defer client.close(t)
		client.negotiate(t)
		client.hello(t)

		client.sendRequest(t, &proto.Run{
			Query:      "RETURN 1 AS n",
			Parameters: map[string]packstream.Value{},
			Extra:      map[string]interface{}{},
		})
		switch m := client.recvResponse(t).(type) {
		case *proto.Failure:
			t.Logf("control: RUN failed as expected with code=%s", m.Code)
		case *proto.Success:
			client.sendRequest(t, &proto.Pull{N: -1, QID: -1})
			for {
				switch p := client.recvResponse(t).(type) {
				case *proto.Failure:
					t.Logf("control: PULL failed as expected with code=%s", p.Code)
					return
				case *proto.Success:
					t.Fatal("a DefaultStatementTimeout of one nanosecond let `RETURN 1` complete; " +
						"the bound is not being applied, so the disabled arm below would be vacuous")
				case *proto.Record:
					// keep draining until SUCCESS or FAILURE
				default:
					t.Fatalf("unexpected message %T", p)
				}
			}
		default:
			t.Fatalf("unexpected reply to RUN: %T", m)
		}
	})

	t.Run("default-server-runs-the-statement", func(t *testing.T) {
		t.Parallel()
		_, addr := startTestServerVerbatimHandle(t, server.Options{})
		client := newBoltTestClient(t, addr)
		defer client.close(t)
		client.negotiate(t)
		client.hello(t)
		client.run(t, "RETURN 1 AS n", nil)
		if records, _ := client.pullAll(t); len(records) != 1 {
			t.Fatalf("a DEFAULT-configured server returned %d records for `RETURN 1`; want 1", len(records))
		}
	})
}

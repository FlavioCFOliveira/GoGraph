package server_test

// default_timeouts_reach_test.go — rmp #2806.
//
// rmp #2806 raised [server.DefaultMaxTxIdleTime] from 5 s to 30 minutes, which
// moved it PAST [server.DefaultConnTimeout] (30 s). That reorders the two
// reclamation paths a silent client can take, and the godoc on
// DefaultMaxTxIdleTime now says so, so the claim is pinned here rather than left
// as prose.
//
// Layer: short. The bounds are coarse — the test asserts an order of magnitude
// apart, not scheduler behaviour.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestReclaim_ConnTimeoutBeatsALongerIdleBound pins WHICH bound reclaims a client
// that opens an explicit transaction and then stops sending bytes, when the idle
// bound is longer than the connection's read deadline — the ordering the raised
// defaults produce (MaxTxIdleTime 30 min > ConnTimeout 30 s).
//
// The read deadline is armed once per [proto.ChunkedReader.ReadMessage] call
// (bolt/server/serve.go, the reader goroutine), and Bolt NOOP keep-alive chunks
// are consumed inside that call (bolt/proto/chunking.go), so they cannot push it
// forward. A client that sends nothing the message loop can dispatch therefore
// trips ConnTimeout, and the teardown calls Session.Close, which counts the
// transaction abandoned and rolls it back — reclaiming the MVCC snapshot and the
// reclamation-horizon slot without the idle reaper ever running.
//
// The idle bound here is an hour, three orders of magnitude above the ceiling
// asserted below, so a pass cannot be the idle reaper's doing.
func TestReclaim_ConnTimeoutBeatsALongerIdleBound(t *testing.T) {
	t.Parallel()
	const (
		connBound = 400 * time.Millisecond
		idleBound = time.Hour
		ceiling   = 10 * time.Second
	)
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout:      connBound,
		MaxTxIdleTime:    idleBound,
		DefaultTxTimeout: idleBound,
	})

	client := newBoltTestClient(t, addr)
	defer client.close(t)
	client.negotiate(t)
	client.hello(t)
	client.begin(t)
	client.run(t, "CREATE (:ReclaimProbe {v: 1})", nil)
	client.pullAll(t)

	// Non-vacuity: the transaction must actually be open, or an empty registry
	// below would prove nothing at all.
	if open := srv.Transactions(); len(open) != 1 {
		t.Fatalf("before the silence the registry lists %d open transactions; want exactly 1 "+
			"(without it the reclaim clause below is vacuous)", len(open))
	}

	// From here the client sends nothing.
	start := time.Now()
	var elapsed time.Duration
	for {
		if len(srv.Transactions()) == 0 {
			elapsed = time.Since(start)
			break
		}
		if time.Since(start) > ceiling {
			t.Fatalf("the transaction was still open %v after the client fell silent, with "+
				"ConnTimeout %v and MaxTxIdleTime %v; the connection deadline did not reclaim it",
				time.Since(start), connBound, idleBound)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if elapsed >= idleBound {
		t.Fatalf("reclaimed after %v, at or beyond the idle bound %v", elapsed, idleBound)
	}
	t.Logf("reclaimed after %v (ConnTimeout %v, MaxTxIdleTime %v)", elapsed, connBound, idleBound)
}

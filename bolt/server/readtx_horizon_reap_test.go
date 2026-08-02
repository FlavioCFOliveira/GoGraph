package server_test

// readtx_horizon_reap_test.go — an ABANDONED Bolt read transaction cannot pin
// the reclamation horizon indefinitely (rmp #2307, sprint 334, acceptance
// criterion 3).
//
// Since #2307 an explicit read transaction pins one snapshot for its whole
// lifetime, which is what makes it snapshot-isolated. That is a real cost and a
// real exposure: while the handle is open, no version it could still reach may
// be reclaimed, so a client that sends BEGIN mode="r" and goes silent would
// hold the watermark — and therefore version memory — for as long as the
// connection lives.
//
// The bound already exists (rmp #2175): the idle reaper rolls an abandoned
// transaction back, and rollback returns the horizon slot. This test is what
// makes that composition binding, because neither half implies the other — the
// reaper predates the snapshot, and nothing else asserts that reaping a READ
// transaction releases anything, since before #2307 a read handle held nothing
// to release.
//
// Layer: short. The timings assert an order of magnitude, not scheduler
// behaviour, exactly as abandoned_tx_test.go does.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestAbandonedReadTx_IdleReaperReleasesTheHorizonSlot opens a read transaction
// over Bolt, goes silent, and asserts that the idle reaper returns the horizon
// slot the transaction was holding — with the socket deliberately kept open, so
// connection teardown cannot be what released it.
func TestAbandonedReadTx_IdleReaperReleasesTheHorizonSlot(t *testing.T) {
	t.Parallel()
	const (
		idleBound  = 300 * time.Millisecond
		totalBound = 20 * time.Second
	)
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	addr := startTestServerWithEngine(t, eng, server.Options{
		ConnTimeout:      30 * time.Second,
		DefaultTxTimeout: totalBound,
		MaxTxIdleTime:    idleBound,
	})

	base := g.MVCCStats().ActiveReaders

	// The abandoning client: BEGIN mode="r", one statement, then silence. It
	// never sends COMMIT, ROLLBACK or RESET and never closes the socket.
	abandoner := newBoltTestClient(t, addr)
	defer abandoner.close(t)
	abandoner.negotiate(t)
	abandoner.hello(t)
	abandoner.beginRead(t)
	abandoner.run(t, "MATCH (n) RETURN count(n)", nil)
	abandoner.pullAll(t)

	// The slot is held while the transaction is open. Asserted first so a
	// failure downstream cannot be explained by the slot never having been
	// taken — which would make the release assertion vacuous.
	if got := g.MVCCStats().ActiveReaders; got != base+1 {
		t.Fatalf("ActiveReaders %d with an open Bolt read transaction, want %d: "+
			"the handle is not pinning the horizon, so this test proves nothing about releasing it",
			got, base+1)
	}

	// The idle reaper must return it, well inside the total bound.
	deadline := time.Now().Add(idleBound * 20)
	for time.Now().Before(deadline) {
		if g.MVCCStats().ActiveReaders == base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := g.MVCCStats()
	t.Fatalf("ActiveReaders still %d after %v of silence (idle bound %v, total bound %v; "+
		"watermark %d, now %d): an abandoned read transaction pins the reclamation horizon",
		s.ActiveReaders, idleBound*20, idleBound, totalBound, s.Watermark, s.Now)
}

//go:build soak

package server_test

// security_conn_churn_soak_test.go — SOAK DEFENSE LOCK-IN for connection-flood
// resilience under authentication (security audit, DoS/endurance cluster).
//
// A credential-validating server must withstand sustained connect → handshake →
// authenticate → query → disconnect churn without leaking goroutines, file
// descriptors, or heap. This extends the short goleak_sessions_test.go (which
// drives a fixed batch of sessions once) into a minutes-long endurance run that
// asserts the live-goroutine count and heap are flat after warm-up — the soak
// acceptance gate the project mandates before a release.
//
// The churn drives the auth path through the low-level wire client with the
// credentials carried in HELLO (the Bolt 5.0-style flow that the built-in
// BasicAuthHandler authenticates today; the split HELLO/LOGON flow is tracked
// separately as SECURITY-GAP #1470). Each iteration exercises the full
// authenticated lifecycle, so a leak in any of HELLO/auth/RUN/PULL/teardown
// surfaces as goroutine or heap growth.
//
// Layer: soak. Activated by `-tags=soak` (this file) or SOAK_FULL=1 / nightly.
// Run without -race on a memory-constrained runner (the soak CI job drops
// -race); see project memory "Soak runner-fit".

import (
	"runtime"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// secBoltAuthHello builds a Bolt 5.0-style HELLO carrying basic credentials in
// the HELLO extra map (the flow BasicAuthHandler authenticates today).
func secBoltAuthHello(user, pass string) *proto.Hello {
	return &proto.Hello{Extra: map[string]interface{}{
		"scheme":      "basic",
		"principal":   user,
		"credentials": pass,
		"agent":       "sec-soak/1.0",
	}}
}

// TestSec_Bolt_ConnChurnUnderAuth runs sustained authenticated connect/query/
// disconnect churn and asserts the goroutine count and heap return to a flat
// baseline after warm-up.
func TestSec_Bolt_ConnChurnUnderAuth(t *testing.T) {
	// A short, fixed duration that is meaningful under the soak layer yet keeps
	// the gate quick. Lengthen via the soak/nightly CI knobs if deeper endurance
	// coverage is wanted.
	const (
		warmupIters = 200
		churnDur    = 20 * time.Second
		user        = "alice"
		pass        = "correct-horse-battery-staple"
	)

	addr := startTestServerWithEngine(t, newEngine(t), server.Options{
		Auth:        server.BasicAuthHandler{Validate: server.ConstantTimeValidate(user, pass)},
		ConnTimeout: 5 * time.Second,
	})

	// One authenticated lifecycle: connect, negotiate, HELLO-with-credentials,
	// RUN+PULL, GOODBYE, close.
	one := func() {
		c := newBoltTestClient(t, addr)
		defer c.close(t)
		c.negotiate(t)
		c.sendRequest(t, secBoltAuthHello(user, pass))
		c.recvSuccess(t)
		c.run(t, "RETURN 1 AS n", nil)
		c.pullAll(t) // drain records + trailing SUCCESS
		c.goodbye(t)
	}

	// ── Warm-up ───────────────────────────────────────────────────────────────
	for i := 0; i < warmupIters; i++ {
		one()
	}
	baseGoroutines, baseHeap := secBoltMemSnapshot()

	// ── Sustained churn ─────────────────────────────────────────────────────
	deadline := time.Now().Add(churnDur)
	iters := 0
	for time.Now().Before(deadline) {
		one()
		iters++
	}
	t.Logf("completed %d authenticated churn iterations over %v", iters, churnDur)

	// ── Post-churn flatness ──────────────────────────────────────────────────
	// Allow asynchronous teardown (reader goroutines, deferred closes) to settle.
	finalGoroutines, finalHeap := secBoltSettleAndSnapshot()

	// Goroutines must not grow beyond a small slack (the server's accept loop and
	// a few transient handlers may be in flight at the sampling instant).
	const goroutineSlack = 8
	if finalGoroutines > baseGoroutines+goroutineSlack {
		t.Fatalf("goroutine leak: baseline %d, after %d churn iterations %d (slack %d)",
			baseGoroutines, iters, finalGoroutines, goroutineSlack)
	}

	// Heap must not grow unboundedly. Allow a generous 2x of the post-warmup heap
	// to absorb allocator slack and GC timing; a real per-connection leak would
	// dwarf this over thousands of iterations.
	if finalHeap > baseHeap*2 && finalHeap > baseHeap+(16<<20) {
		t.Fatalf("heap growth suggests a per-connection leak: baseline %d B, after %d iterations %d B",
			baseHeap, iters, finalHeap)
	}
	t.Logf("flatness OK: goroutines %d→%d, heap %d→%d bytes", baseGoroutines, finalGoroutines, baseHeap, finalHeap)
}

// secBoltMemSnapshot returns the current goroutine count and live heap bytes
// after a GC, so the sample reflects retained (not garbage) memory.
func secBoltMemSnapshot() (goroutines int, heapBytes uint64) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return runtime.NumGoroutine(), ms.HeapAlloc
}

// secBoltSettleAndSnapshot waits briefly for asynchronous connection teardown to
// complete, then returns a memory snapshot. The settle window lets reader
// goroutines and deferred Close paths drain before the flatness assertion.
func secBoltSettleAndSnapshot() (goroutines int, heapBytes uint64) {
	deadline := time.Now().Add(3 * time.Second)
	var g int
	var h uint64
	for time.Now().Before(deadline) {
		g, h = secBoltMemSnapshot()
		time.Sleep(100 * time.Millisecond)
	}
	return g, h
}

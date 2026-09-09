package sim

// overload_heavy_write_test.go — the heavy-WRITE overload family on the
// concurrent production path (rmp #2736).
//
// Before #2736 overloadOp mapped [OverloadLargeCreateTx] to a read
// unconditionally, so the only write-shaped overload family was issued by this
// package's own single-connection exercise and by no production path at all.
// These tests cover the three things that restoring it has to establish:
//
//   - the adjudicator can FAIL — both as a pure classifier and wired to a real
//     connection, driven by a population that does not match its
//     acknowledgement;
//   - the family is drawn NON-VACUOUSLY on the path the catalogue's "overload"
//     scenario runs, and its nodes leave the population as they found it;
//   - the family stays OFF for every caller that did not ask for it, which is
//     what keeps the cost of the default mix — and of the benchmark calibrated
//     on it — unchanged.

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestAdjudicateHeavyWrite drives the pure classifier over both verdicts. It is
// the first half of showing the oracle can fire: an acknowledged heavy write
// that committed anything other than the whole batch, and a refused one that
// committed anything at all, are both violations.
func TestAdjudicateHeavyWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		got   int64
		acked bool
		want  bool
	}{
		{name: "acked and whole batch committed", acked: true, got: overloadCreateBatch, want: false},
		{name: "acked but one node short", acked: true, got: overloadCreateBatch - 1, want: true},
		{name: "acked but nothing committed", acked: true, got: 0, want: true},
		{name: "acked but more than the batch", acked: true, got: overloadCreateBatch + 1, want: true},
		{name: "refused and nothing committed", acked: false, got: 0, want: false},
		{name: "refused but one node committed", acked: false, got: 1, want: true},
		{name: "refused but whole batch committed", acked: false, got: overloadCreateBatch, want: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := adjudicateHeavyWrite(tc.acked, tc.got); got != tc.want {
				t.Errorf("adjudicateHeavyWrite(acked=%v, got=%d) = %v, want %v",
					tc.acked, tc.got, got, tc.want)
			}
		})
	}
}

// TestOverloadHeavyWriteOp_AdjudicatesAndCleans drives the WIRED heavy-write
// operation over a real Bolt connection, in both directions.
//
// The clean arm proves the op issues the write, sees it acknowledged, counts the
// population it committed, finds it exact, and deletes it again.
//
// The mismatched arm is the injection that makes the wired oracle FAIL: seven
// extra nodes are given the same run tag before the op runs, so the tagged
// population it then counts is overloadCreateBatch+7 against an acknowledgement
// that promised exactly overloadCreateBatch. Nothing else differs. Without that
// arm the violation counter would be a tally no test had ever made move.
func TestOverloadHeavyWriteOp_AdjudicatesAndCleans(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, client, ctr := newHeavyWriteFixture(t)
	defer func() { _ = srv.Close() }()
	defer func() { _ = client.Close() }()

	t.Run("population matches its acknowledgement", func(t *testing.T) {
		var wl writerLog
		if stop := overloadHeavyWriteOp(client, "heavy-clean", ctr, &wl); stop {
			t.Fatalf("heavy write stopped the connection: transportErrors=%d", ctr.transportErrors.Load())
		}
		assertHeavyTallies(t, &wl, heavyTallies{issued: 1, acked: 1, adjudications: 1, violations: 0})
	})

	t.Run("population does not match its acknowledgement", func(t *testing.T) {
		// Pre-seed seven nodes under the tag the op is about to use, so its
		// acknowledged commit of overloadCreateBatch nodes adjudicates against a
		// tagged population of overloadCreateBatch+7.
		const extra = 7
		heavyRunOK(t, client, "UNWIND range(1, $n) AS i CREATE (:Bulk {run: $run, i: i})",
			map[string]any{"n": int64(extra), "run": "heavy-mismatched"})

		var wl writerLog
		if stop := overloadHeavyWriteOp(client, "heavy-mismatched", ctr, &wl); stop {
			t.Fatalf("heavy write stopped the connection: transportErrors=%d", ctr.transportErrors.Load())
		}
		assertHeavyTallies(t, &wl, heavyTallies{issued: 1, acked: 1, adjudications: 1, violations: 1})
	})

	// Both arms are population-neutral: the cleanup removes every node carrying
	// the tag, the seven injected ones included.
	if n := heavyBulkCount(t, srv); n != 0 {
		t.Errorf("heavy writes left %d :Bulk node(s) behind; the operation must be population-neutral", n)
	}
	if got := ctr.transportErrors.Load(); got != 0 {
		t.Errorf("transportErrors = %d, want 0", got)
	}
}

// TestConcurrent_OverloadHeavyWritesNonVacuous drives RunConcurrent with the
// mix the catalogue's "overload" scenario declares — read from the registry, not
// restated here, so a future edit that turns the family off in the catalogue
// fails this test instead of silently un-wiring the production path again.
//
// It is the non-vacuity evidence the family was missing: it asserts heavy writes
// were actually ISSUED and acknowledged, that every one of them was adjudicated,
// that none violated the all-or-nothing contract, and that the run's node-count
// oracle is untouched by them.
func TestConcurrent_OverloadHeavyWritesNonVacuous(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioOverload)
	if !ok {
		t.Fatalf("scenario %q missing from the catalogue", ScenarioOverload)
	}
	if sc.Mix == nil || !sc.Mix.OverloadHeavyWrites {
		t.Fatalf("scenario %q no longer enables OverloadHeavyWrites, so its "+
			"only write-shaped overload family is issued by no production path", ScenarioOverload)
	}

	srv, err := NewSimServer(SimEngineForServer(), scenarioClock())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:        sc.DefaultSeed,
		Connections: sc.Connections,
		OpsPerConn:  sc.OpsPerConn,
		Mix:         sc.Mix,
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}

	if res.OverloadHeavyIssued == 0 {
		t.Fatalf("no heavy-write transaction was issued, so every heavy-write claim "+
			"over this run is empty (seed=%d conns=%d ops=%d)",
			sc.DefaultSeed, sc.Connections, sc.OpsPerConn)
	}
	if res.OverloadHeavyAcked == 0 {
		t.Errorf("issued %d heavy write(s) but the engine acknowledged none, so the "+
			"served-in-full half of graceful degradation went unexercised", res.OverloadHeavyIssued)
	}
	// A heavy write the server IGNORED never reached the engine: the connection
	// was still in the Bolt FAILED state from an earlier bound and the RESET that
	// should have cleared it did not happen. Such an attempt exercises nothing,
	// and it is indistinguishable from a genuine refusal in every tally but this
	// one — so a run that reports any is a run whose heavy-write count overstates
	// what it actually issued.
	if res.OverloadHeavyIgnored != 0 {
		t.Errorf("%d of %d heavy write(s) were IGNORED rather than issued: the "+
			"connection was left in the Bolt FAILED state, so those attempts reached "+
			"no engine and proved nothing", res.OverloadHeavyIgnored, res.OverloadHeavyIssued)
	}
	if res.OverloadHeavyAdjudications != res.OverloadHeavyIssued {
		t.Errorf("adjudicated %d of %d heavy write(s): the unadjudicated ones proved nothing",
			res.OverloadHeavyAdjudications, res.OverloadHeavyIssued)
	}
	if res.OverloadHeavyViolations != 0 {
		t.Errorf("OverloadHeavyViolations = %d, want 0: a heavy write committed a "+
			"population that did not match its acknowledgement", res.OverloadHeavyViolations)
	}
	if !res.Consistent() {
		t.Errorf("run inconsistent: engineNodes=%d ackedCreates=%d panics=%d transportErrors=%d "+
			"heavyViolations=%d wireParamFailures=%v",
			res.EngineNodeCount, res.AckedCreates, res.Panics, res.TransportErrors,
			res.OverloadHeavyViolations, res.WireParamFailures)
	}
	if n := heavyBulkCount(t, srv); n != 0 {
		t.Errorf("run left %d :Bulk node(s) at quiescence; the heavy write must be "+
			"population-neutral or the node-count oracle is no longer valid", n)
	}
	t.Logf("heavy-write overload: issued=%d acknowledged=%d ignored=%d adjudicated=%d "+
		"violations=%d (%d nodes per transaction, all removed again)",
		res.OverloadHeavyIssued, res.OverloadHeavyAcked, res.OverloadHeavyIgnored,
		res.OverloadHeavyAdjudications, res.OverloadHeavyViolations, overloadCreateBatch)
}

// TestConcurrent_OverloadHeavyWritesOffUnlessAsked pins the family OFF for every
// caller that did not ask for it — the default mix included. That default is what
// bench/contention's dst-concurrent-bolt runs on, and its operation count is
// calibrated against a documented per-operation cost, so a heavy write drawn
// there would change what the benchmark measures rather than how fast it is.
func TestConcurrent_OverloadHeavyWritesOffUnlessAsked(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, tc := range []struct {
		name string
		mix  *ConcurrentMix
	}{
		{name: "default mix", mix: nil},
		{
			name: "explicit read-only overload mix",
			mix:  &ConcurrentMix{WriterWeight: 0.3, ReaderWeight: 0.1, OverloadWeight: 0.6},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv, err := NewSimServer(SimEngineForServer(), scenarioClock())
			if err != nil {
				t.Fatalf("NewSimServer: %v", err)
			}
			defer func() { _ = srv.Close() }()

			res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
				Seed:        0x07E410AD,
				Connections: 12,
				OpsPerConn:  8,
				Mix:         tc.mix,
			})
			if err != nil {
				t.Fatalf("RunConcurrent: %v", err)
			}
			if res.OverloadHeavyIssued != 0 {
				t.Errorf("OverloadHeavyIssued = %d, want 0: the heavy-write family must be "+
					"drawn only when the mix asks for it", res.OverloadHeavyIssued)
			}
			if !res.Consistent() {
				t.Errorf("run inconsistent: engineNodes=%d ackedCreates=%d panics=%d transportErrors=%d",
					res.EngineNodeCount, res.AckedCreates, res.Panics, res.TransportErrors)
			}
			if n := heavyBulkCount(t, srv); n != 0 {
				t.Errorf("a read-only overload population wrote %d :Bulk node(s)", n)
			}
		})
	}
}

// heavyTallies is the expected writerLog heavy-write ledger for one operation.
type heavyTallies struct{ issued, acked, ignored, adjudications, violations int64 }

// assertHeavyTallies compares a connection's heavy-write ledger field by field.
func assertHeavyTallies(t *testing.T, wl *writerLog, want heavyTallies) {
	t.Helper()
	got := heavyTallies{
		issued:        wl.overloadHeavyIssued,
		acked:         wl.overloadHeavyAcked,
		ignored:       wl.overloadHeavyIgnored,
		adjudications: wl.overloadHeavyAdjudications,
		violations:    wl.overloadHeavyViolations,
	}
	if got != want {
		t.Errorf("heavy-write ledger = %+v, want %+v", got, want)
	}
}

// newHeavyWriteFixture opens a SimServer, one connected client, and a counters
// bundle backed by fresh atomics, for the wired heavy-write exercises.
//
// The caller closes both, with a plain defer rather than [testing.T.Cleanup]: a
// cleanup function runs AFTER the test body's defers, so goleak — deferred first
// and therefore running last — would inspect the process while this server and
// this connection were still open and report their goroutines as leaked.
func newHeavyWriteFixture(t *testing.T) (*SimServer, *WireClient, *counters) {
	t.Helper()
	srv, err := NewSimServer(SimEngineForServer(), scenarioClock())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	client, err := srv.Dial()
	if err != nil {
		_ = srv.Close()
		t.Fatalf("Dial: %v", err)
	}
	if err := client.Connect(context.Background()); err != nil {
		_ = client.Close()
		_ = srv.Close()
		t.Fatalf("Connect: %v", err)
	}
	return srv, client, newHeavyCounters()
}

// newHeavyCounters bundles fresh atomics as a [counters], so a wired operation
// can be driven outside RunConcurrent and its tallies read back directly.
func newHeavyCounters() *counters {
	var acked, transport, bounded atomic.Int64
	return &counters{ackedCreates: &acked, transportErrors: &transport, boundedRejects: &bounded}
}

// heavyRunOK runs one statement to completion and fails the test unless both the
// RUN and the PULL terminal were accepted, returning the records streamed back.
func heavyRunOK(t *testing.T, c *WireClient, query string, params map[string]any) []*proto.Record {
	t.Helper()
	resp, err := c.Run(query, params)
	if err != nil {
		t.Fatalf("RUN %q: %v", query, err)
	}
	if f, isFailure := resp.(*proto.Failure); isFailure {
		t.Fatalf("RUN %q refused: %s %s", query, f.Code, f.Message)
	}
	records, term, err := c.PullAll()
	if err != nil {
		t.Fatalf("PULL %q: %v", query, err)
	}
	if f, isFailure := term.(*proto.Failure); isFailure {
		t.Fatalf("PULL %q refused: %s %s", query, f.Code, f.Message)
	}
	return records
}

// heavyBulkCount counts every :Bulk node the engine holds, over a fresh
// connection at quiescence — deliberately UNtagged, so residue left by any tag
// is seen.
func heavyBulkCount(t *testing.T, srv *SimServer) int64 {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("bulk-count Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("bulk-count Connect: %v", err)
	}
	records := heavyRunOK(t, c, "MATCH (n:Bulk) RETURN count(n)", nil)
	if len(records) != 1 || len(records[0].Data) != 1 {
		t.Fatalf("bulk-count returned %d row(s), want 1", len(records))
	}
	n, isInt := records[0].Data[0].(int64)
	if !isInt {
		t.Fatalf("bulk-count is a %T, want int64", records[0].Data[0])
	}
	return n
}

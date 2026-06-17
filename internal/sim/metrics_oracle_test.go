package sim

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestMetricsOracle_HonestWorkload drives a write-heavy workload and asserts the
// engine's exported RunInTx observation count matches the oracle's write count,
// the error counter matches (zero for an honest workload), and the goroutine
// count returns to baseline. The metrics backend is global, so this test does
// NOT run in parallel.
func TestMetricsOracle_HonestWorkload(t *testing.T) {
	defer goleak.VerifyNone(t)

	res, err := RunWithMetricsOracle(context.Background(), 0x5217E, 300, WriteHeavyWorkload)
	if err != nil {
		t.Fatalf("RunWithMetricsOracle: %v", err)
	}
	if !res.Consistent() {
		t.Fatalf("metrics inconsistent with oracle accounting:\n%s", res.String())
	}
	if res.ExpectedWrites == 0 {
		t.Errorf("write-heavy workload issued zero writes; check is vacuous")
	}
	if res.ExpectedWriteErrors != 0 {
		t.Errorf("honest workload had %d write errors, want 0", res.ExpectedWriteErrors)
	}
}

// TestMetricsOracle_MalformedWorkload drives a 100%-malformed workload: every
// write statement is rejected, so the engine's RunInTx error counter delta must
// equal the write count, and the oracle stays consistent.
func TestMetricsOracle_MalformedWorkload(t *testing.T) {
	defer goleak.VerifyNone(t)

	res, err := RunWithMetricsOracle(context.Background(), 0xBAD, 200, BadActorWorkload)
	if err != nil {
		t.Fatalf("RunWithMetricsOracle: %v", err)
	}
	if !res.Consistent() {
		t.Fatalf("metrics inconsistent for malformed workload:\n%s", res.String())
	}
	gotErrors := res.After.RunInTxErrors - res.Before.RunInTxErrors
	if gotErrors != res.ExpectedWriteErrors {
		t.Errorf("malformed: engine error delta %d != oracle accounting %d", gotErrors, res.ExpectedWriteErrors)
	}
	// A bad-actor mix includes a MalformedSender, so at least one write must have
	// been rejected — otherwise the error-accounting path is untested.
	if res.ExpectedWriteErrors == 0 {
		t.Errorf("bad-actor workload produced no write errors over 200 ops; error accounting untested")
	}
}

// TestMetricsOracle_SwarmGoroutineBaseline wires the oracle into a concurrent
// swarm and asserts the reliability bound: the goroutine count returns to its
// baseline after the swarm joins every worker. Global metrics sink -> serial.
//
// Gated to the soak layer: it runs a 16-run multi-worker swarm over the
// read-heavy workload, which is minutes-long under -race. The short-layer
// TestPhase5_EndToEndShort asserts the same goroutine-baseline bound
// (mres.Consistent) at a smaller scale on every PR.
func TestMetricsOracle_SwarmGoroutineBaseline(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sw, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 0x5EED, Scenario: ScenarioReadHeavy, Workers: 4, Runs: 16,
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}
	// A small slack absorbs runtime bookkeeping goroutines that a -race build may
	// leave transiently parked; the swarm's own workers are all joined by Run.
	res, mres, err := RunSwarmWithMetricsOracle(context.Background(), sw, 4)
	if err != nil {
		t.Fatalf("RunSwarmWithMetricsOracle: %v", err)
	}
	if res.Runs != 16 {
		t.Errorf("swarm ran %d, want 16", res.Runs)
	}
	if !mres.Consistent() {
		t.Errorf("metrics goroutine-baseline bound violated after swarm:\n%s", mres.String())
	}
}

// TestMetricsOracle_CatchesMiscount asserts the Check arithmetic catches a
// metric that under/over-counts vs the oracle: feeding a deliberately wrong
// expected-write count must produce a discrepancy.
func TestMetricsOracle_CatchesMiscount(t *testing.T) {
	o := &MetricsOracle{backend: newRecordingBackend()}
	before := MetricsSnapshot{RunInTxCount: 100, Goroutines: 10}
	after := MetricsSnapshot{RunInTxCount: 150, Goroutines: 10} // 50 writes observed

	// Oracle accounts 50 writes -> consistent.
	r0 := o.Check(before, after, 50, 0, 0)
	if !r0.Consistent() {
		t.Errorf("Check flagged a matching count:\n%s", r0.String())
	}
	// A miscounting metric (engine recorded 50 but oracle expected 60) -> caught.
	r := o.Check(before, after, 60, 0, 0)
	if r.Consistent() {
		t.Errorf("Check failed to catch a write miscount")
	}
}

// TestMetricsOracle_CatchesLeak asserts the goroutine-bound catches a leak: an
// after-snapshot whose goroutine count exceeds the before by more than the slack
// is a discrepancy.
func TestMetricsOracle_CatchesLeak(t *testing.T) {
	o := &MetricsOracle{backend: newRecordingBackend()}
	before := MetricsSnapshot{Goroutines: 10}
	after := MetricsSnapshot{Goroutines: 15} // +5 leaked

	r1 := o.Check(before, after, 0, 0, 0)
	if r1.Consistent() {
		t.Errorf("Check failed to catch a goroutine leak of 5")
	}
	// Within slack -> not flagged.
	r2 := o.Check(before, after, 0, 0, 8)
	if !r2.Consistent() {
		t.Errorf("Check flagged a leak within slack:\n%s", r2.String())
	}
}

package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestConcurrent_MixedWorkloadQuiesces drives N concurrent connections through a
// mixed actor workload to quiescence and asserts the eventual-consistency
// oracle: the engine's node count equals the acknowledged creates, with no
// panics and no unexpected transport errors. goleak verifies zero leaked
// goroutines after teardown.
func TestConcurrent_MixedWorkloadQuiesces(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:        0xBEEF,
		Connections: 16,
		OpsPerConn:  20,
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}

	if res.Panics != 0 {
		t.Errorf("recovered %d panics across connection goroutines (want 0)", res.Panics)
	}
	if res.TransportErrors != 0 {
		t.Errorf("%d unexpected transport errors (want 0)", res.TransportErrors)
	}
	if !res.Consistent() {
		t.Errorf("eventual oracle inconsistent: engine node count %d != acked creates %d (panics=%d transport=%d)",
			res.EngineNodeCount, res.AckedCreates, res.Panics, res.TransportErrors)
	}
	if res.AckedCreates == 0 {
		t.Error("no writes were acknowledged — the workload did not exercise the write path")
	}
}

// TestConcurrent_ContextCancellationDrains proves a cancelled context stops the
// connections promptly and the harness still drains every goroutine (goleak
// clean) — no goroutine outlives the call.
func TestConcurrent_ContextCancellationDrains(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately; connections must stop at their next op boundary
	// and the harness must still join them all.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	res, err := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        0xCA11,
		Connections: 32,
		OpsPerConn:  100,
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}
	if res.Panics != 0 {
		t.Errorf("recovered %d panics under cancellation (want 0)", res.Panics)
	}
	// Even under cancellation the oracle must hold: only acknowledged creates are
	// counted, and a cancelled-but-acknowledged write is still present.
	if !res.Consistent() {
		t.Errorf("oracle inconsistent under cancellation: engine %d != acked %d",
			res.EngineNodeCount, res.AckedCreates)
	}
}

// TestConcurrent_NoLeakAcrossManyRuns repeats short concurrent runs and verifies
// goleak stays clean across the whole battery, catching a leak that only appears
// under connection churn.
func TestConcurrent_NoLeakAcrossManyRuns(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < 8; i++ {
		srv, err := NewSimServer(SimEngineForServer(), clock.Real())
		if err != nil {
			t.Fatalf("run %d NewSimServer: %v", i, err)
		}
		res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
			Seed:        uint64(i) * 0x9E3779B9,
			Connections: 8,
			OpsPerConn:  10,
		})
		if err != nil {
			t.Fatalf("run %d RunConcurrent: %v", i, err)
		}
		if !res.Consistent() {
			t.Errorf("run %d inconsistent: engine %d != acked %d (panics=%d transport=%d)",
				i, res.EngineNodeCount, res.AckedCreates, res.Panics, res.TransportErrors)
		}
		if err := srv.Close(); err != nil {
			t.Errorf("run %d Close: %v", i, err)
		}
	}
}

package contention

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// driveSmoke runs a workload's Setup, issues n operations on one goroutine and
// tears it down, failing the test on any error. It is a smoke driver, not a
// measurement: it exists so a workload that stops constructing, or starts
// returning errors on its own fixture, fails in the ordinary test layer instead
// of only being noticed in a multi-minute sweep.
func driveSmoke(t *testing.T, w Workload, n int) {
	t.Helper()
	op, teardown, err := w.Setup(t.TempDir())
	if err != nil {
		t.Fatalf("%s: Setup: %v", w.Name, err)
	}
	t.Cleanup(func() {
		if teardown == nil {
			return
		}
		if err := teardown(); err != nil {
			t.Errorf("%s: teardown: %v", w.Name, err)
		}
	})
	ctx := context.Background()
	for i := range n {
		if err := op(ctx, 0, i); err != nil {
			t.Fatalf("%s: op %d: %v", w.Name, i, err)
		}
	}
}

// TestRound2WorkloadsDrive is the construction smoke test for the workloads
// added by rmp #2690. Every one of them reaches a surface no earlier workload
// touched, so a silent construction failure would show up as a MISSING row in
// the sweep rather than as an error, and a missing row reads exactly like a
// surface nobody thought to cover.
func TestRound2WorkloadsDrive(t *testing.T) {
	for _, tc := range []struct {
		w   Workload
		ops int
	}{
		{dstConcurrentWorkload(), 2},
		{dstMVCCSessionsWorkload(), 2},
		{generationWorkload(), 4096},
		{indexManagerWorkload(), 4096},
		{metricsWorkload(), 4096},
	} {
		t.Run(tc.w.Name, func(t *testing.T) { driveSmoke(t, tc.w, tc.ops) })
	}
}

// diskSmokeOps is how many commits the controlled-pair check issues on each
// arm.
//
// It is sized from the injector's own rate: at one fault in
// [dstDiskFaultRate]'s 512 syncs the faulted arm poisons its writer within the
// first few hundred commits, so a few thousand is ample for both directions of
// the assertion. It is not sized to be representative of a measurement window;
// this is a property check, not a measurement.
const diskSmokeOps = 5000

// TestDiskArmsAreAControlledPair proves the two simulated-disk arms differ in
// exactly the way they claim to, in BOTH directions.
//
// Both arms deliberately swallow [sim.ErrSimFault] — an operation the simulated
// disk refused is the injector working, not the module failing — so both report
// zero errors in the sweep whether faults fired or not. Without this test the
// faulted arm could stop faulting entirely (a changed seed, a WAL that stopped
// syncing, an error no longer wrapped) and the inventory would go on citing
// fault-injection coverage it no longer has; and the clean arm could start
// faulting and quietly stop being a control.
func TestDiskArmsAreAControlledPair(t *testing.T) {
	clean, cleanFaults := dstDiskWorkloadWithFaultCount(dstDiskCleanName, 0)
	driveSmoke(t, clean, diskSmokeOps)
	if got := cleanFaults.Load(); got != 0 {
		t.Errorf("%s: %d injected faults over %d commits; the control arm must not fault",
			clean.Name, got, diskSmokeOps)
	}

	faulted, faults := dstDiskWorkloadWithFaultCount(dstDiskFaultName, dstDiskFaultRate)
	driveSmoke(t, faulted, diskSmokeOps)
	got := faults.Load()
	if got == 0 {
		t.Fatalf("%s: injected 0 faults over %d commits at rate %v; the arm is vacuous",
			faulted.Name, diskSmokeOps, dstDiskFaultRate)
	}
	t.Logf("%s: %d of %d commits met an injected fault", faulted.Name, got, diskSmokeOps)
}

// TestCeilingArmsNameRealWorkloads is the check that keeps a ceiling ratio
// honest at the cheapest possible price.
//
// A ceiling arm carries the identity of the base workload it replicates, and it
// looks that base up by name. Rename or retire a workload and the arm silently
// becomes [unresolvedArm] — a Workload that still has a name, still appears in
// [ByName], and fails only when a probe already has hours invested in it. This
// test resolves every arm at build time instead, and drives a handful of
// operations through each so a fixture that stopped constructing is caught here
// rather than in the middle of a campaign.
func TestCeilingArmsNameRealWorkloads(t *testing.T) {
	arms := ceilingArms()
	if len(arms) == 0 {
		t.Fatal("no ceiling arms registered")
	}
	for _, arm := range arms {
		t.Run(arm.Name, func(t *testing.T) {
			if arm.Surface == "unresolved ceiling arm" {
				t.Fatalf("%s: names a workload that is not in the registry", arm.Name)
			}
			base, ok := ByName(strings.TrimSuffix(arm.Name, ceilingSuffix))
			if !ok {
				t.Fatalf("%s: base workload %q not found", arm.Name,
					strings.TrimSuffix(arm.Name, ceilingSuffix))
			}
			// An arm that does a different amount of work than its base is not
			// a ceiling for it; the ratio between them would then be partly
			// the op-count difference.
			if arm.Ops != base.Ops {
				t.Errorf("%s: Ops = %d, base %s has %d; a ceiling arm must do the same work",
					arm.Name, arm.Ops, base.Name, base.Ops)
			}
			driveSmoke(t, arm, ceilingArmSmokeOps)
		})
	}
}

// ceilingArmSmokeOps is how many operations the arm smoke test issues. It is
// small: this is a construction check, not a measurement.
const ceilingArmSmokeOps = 64

// TestCeilingArmsAreNotSwept guards the one property that keeps the sweep's
// scaling table a table of things the module actually does: a ceiling arm
// replicates a fixture in a way no caller would, so a sweep that walked one
// would publish a throughput number for a program that does not exist.
func TestCeilingArmsAreNotSwept(t *testing.T) {
	swept := make(map[string]bool)
	for _, w := range All() {
		swept[w.Name] = true
	}
	for _, arm := range ceilingArms() {
		if swept[arm.Name] {
			t.Errorf("%s is in All(); ceiling arms must be reachable only through ByName", arm.Name)
		}
	}
}

// driveSmokeConcurrent runs a workload's Setup once and then issues
// opsPerWorker operations from each of workers goroutines against that ONE
// fixture, failing the test on any error.
//
// It is the concurrent counterpart of [driveSmoke], and the distinction is not
// cosmetic: [driveSmoke] drives a single goroutine, so it cannot reach any
// property that only exists while several of the harness's workers share the
// workload's fixture — which for dst-concurrent-bolt is the whole point of the
// arm. It remains a smoke driver, not a measurement.
func driveSmokeConcurrent(t *testing.T, w Workload, workers, opsPerWorker int) {
	t.Helper()
	op, teardown, err := w.Setup(t.TempDir())
	if err != nil {
		t.Fatalf("%s: Setup: %v", w.Name, err)
	}
	t.Cleanup(func() {
		if teardown == nil {
			return
		}
		if err := teardown(); err != nil {
			t.Errorf("%s: teardown: %v", w.Name, err)
		}
	})

	ctx := context.Background()
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range opsPerWorker {
				if err := op(ctx, worker, i); err != nil {
					errs[worker] = err
					return
				}
			}
		}()
	}
	wg.Wait()

	for worker, err := range errs {
		if err != nil {
			t.Errorf("%s: worker %d: %v", w.Name, worker, err)
		}
	}
}

// dstConcurrentSmokeWorkers and dstConcurrentSmokeOps size the shared-fixture
// smoke run.
//
// Sized to the base rate of the event, not for spectacle: the cross-talk this
// test exists to catch appears from TWO simultaneous callers upward, so four
// workers makes detection overwhelming while keeping the run near a tenth of a
// second — one operation costs roughly 2.6 ms.
const (
	dstConcurrentSmokeWorkers = 4
	dstConcurrentSmokeOps     = 4
)

// TestDstConcurrentBoltSharesOneServerCleanly drives dst-concurrent-bolt the way
// the sweep drives it — several workers against ONE shared [sim.SimServer] — and
// requires every operation to succeed.
//
// This is the arm-level regression gate for rmp #2728. That defect was invisible
// to [TestRound2WorkloadsDrive] by construction: it issues its operations on one
// goroutine, so the arm's defining property — the sharing — was never exercised
// in the test layer at all, and a fixture that only misbehaves when two callers
// overlap could not fail anything. The failure it now catches arrives through
// the Bolt parameter matrix, which [dstConcurrentOp] began asserting once rmp
// #2728 made the probe's fixture private and the oracle therefore sharable.
func TestDstConcurrentBoltSharesOneServerCleanly(t *testing.T) {
	driveSmokeConcurrent(t, dstConcurrentWorkload(), dstConcurrentSmokeWorkers, dstConcurrentSmokeOps)
}

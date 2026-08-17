package sim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
)

// durableTestSeeds are a small, fixed spread of run seeds so the concurrent-
// durable tests exercise several fault ordinals and role/op sub-streams without
// depending on wall-clock timing.
var durableTestSeeds = []uint64{0xD4B1E_C0117, 0x1, 0x9E3779B9, 0xC0FFEE, 0xBADF00D}

// -----------------------------------------------------------------------------
// RunConcurrent acked/issued/failed name extension
// -----------------------------------------------------------------------------

// TestRunConcurrent_CapturesAckedNames pins the RunConcurrent extension: on a
// clean in-memory run every writer op is acknowledged, so the acked-name set
// equals the acked-create count, is a subset of the issued names, and no name is
// classified failed. This guards the name-capture plumbing the durable-commit
// scenarios depend on independently of any durability fault.
func TestRunConcurrent_CapturesAckedNames(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:        0xBEEF,
		Connections: 8,
		OpsPerConn:  12,
		Mix:         &ConcurrentMix{WriterWeight: 1.0},
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}
	if res.Panics != 0 || res.TransportErrors != 0 {
		t.Fatalf("clean run had panics=%d transport=%d", res.Panics, res.TransportErrors)
	}
	if got, want := len(res.AckedNames), int(res.AckedCreates); got != want {
		t.Fatalf("len(AckedNames)=%d, want AckedCreates=%d", got, want)
	}
	if len(res.AckedNames) == 0 {
		t.Fatal("no writer names acknowledged — the write path was not exercised")
	}
	if got, want := len(res.IssuedNames), 8*12; got != want {
		t.Fatalf("len(IssuedNames)=%d, want %d (one per writer op)", got, want)
	}
	if len(res.FailedNames) != 0 {
		t.Fatalf("clean run reported %d failed names, want 0", len(res.FailedNames))
	}
	// Acked ⊆ issued, and acked names are unique (a set with no duplicates).
	acked, issued := toSet(res.AckedNames), toSet(res.IssuedNames)
	if len(acked) != len(res.AckedNames) {
		t.Fatalf("acked names not unique: %d names, %d distinct", len(res.AckedNames), len(acked))
	}
	if extra := setMinus(acked, issued); len(extra) > 0 {
		t.Fatalf("acked names not a subset of issued: %v", extra)
	}
}

// -----------------------------------------------------------------------------
// ST2 — durable-commit-crash
// -----------------------------------------------------------------------------

// TestST2_DurableCommitCrash_Scenario runs the registered ST2 scenario across a
// spread of seeds and asserts each passes (nil report): every acknowledged
// commit survives the crash (acked ⊆ recovered), no phantom (recovered ⊆
// issued), every client-observed failure is absent, no torn CREATE, and the
// injected fault demonstrably bit (non-vacuity).
func TestST2_DurableCommitCrash_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := durableCommitCrashScenario()
	for _, seed := range durableTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("ST2 seed %#x run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("ST2 seed %#x violation:\n%s", seed, report)
		}
	}
}

// TestST2_FaultBitesAndNothingLost asserts the fault-variant SET relations
// directly (richer than the nil-report check): a non-empty prefix of commits is
// acked and durable, the poisoned suffix explicitly fails and is absent, and the
// recovered set is exactly acked ⊆ recovered ⊆ issued.
func TestST2_FaultBitesAndNothingLost(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	r, err := runDurableCommitCrash(ctx, 0xF00D_1234, true)
	if err != nil {
		t.Fatalf("runDurableCommitCrash: %v", err)
	}
	if len(r.acked) == 0 {
		t.Fatal("no commit acked before the fault")
	}
	if len(r.failed) == 0 {
		t.Fatal("the injected mid-flight fault did not produce any explicit failure")
	}
	if len(r.acked) >= len(r.issued) {
		t.Fatalf("fault did not lose the poisoned commit: acked=%d issued=%d", len(r.acked), len(r.issued))
	}
	if v := r.violations(true); len(v) > 0 {
		t.Fatalf("ST2 fault-variant invariant breach: %v", v)
	}
	// Every acked name is durable; every failed name is gone.
	if miss := setMinus(r.acked, r.recovered); len(miss) > 0 {
		t.Fatalf("acked commits lost after recovery: %v", miss)
	}
	if resurrected := setIntersect(r.failed, r.recovered); len(resurrected) > 0 {
		t.Fatalf("failed commits resurrected after recovery: %v", resurrected)
	}
}

// TestST2_FaultFreeQuiescenceSmoke is the smoke variant: with no fault every
// commit is acknowledged and durable, so the recovered set equals the acked set
// exactly (and no failures were observed).
func TestST2_FaultFreeQuiescenceSmoke(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	r, err := runDurableCommitCrash(ctx, 0x5A1E_5A1E, false)
	if err != nil {
		t.Fatalf("runDurableCommitCrash (fault-free): %v", err)
	}
	if len(r.failed) != 0 {
		t.Fatalf("fault-free run reported %d failures, want 0", len(r.failed))
	}
	if len(r.acked) == 0 {
		t.Fatal("fault-free run acked nothing")
	}
	if miss := setMinus(r.acked, r.recovered); len(miss) > 0 {
		t.Fatalf("acked ⊄ recovered on a clean run: %v", miss)
	}
	if extra := setMinus(r.recovered, r.acked); len(extra) > 0 {
		t.Fatalf("recovered ≠ acked on a clean quiescence crash (surplus %v)", extra)
	}
}

// TestST2_NoLeakAcrossRuns repeats the ST2 scenario to catch a teardown-only
// goroutine leak that a single run would miss.
func TestST2_NoLeakAcrossRuns(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := durableCommitCrashScenario()
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		if _, err := sc.Run(ctx, durableTestSeeds[i]); err != nil {
			cancel()
			t.Fatalf("iteration %d: %v", i, err)
		}
		cancel()
	}
}

// -----------------------------------------------------------------------------
// ST3 — checkpoint-teardown
// -----------------------------------------------------------------------------

// TestST3_CheckpointTeardown_Scenario runs the registered ST3 scenario (a
// background checkpointer racing committers, torn down via the crash-safe
// store.DB order) across seeds and asserts each passes: every acknowledged
// commit survives the teardown (recovered ⊇ acked, i.e. no ErrWriterClosed ever
// surfaced as an ack), no phantom, no torn CREATE.
func TestST3_CheckpointTeardown_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := checkpointTeardownScenario()
	for _, seed := range durableTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("ST3 seed %#x run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("ST3 seed %#x violation:\n%s", seed, report)
		}
	}
}

// TestST3_FaultAtTeardown injects a one-shot fsync fault at the teardown
// boundary (landing on the final checkpoint or a racing commit) and asserts the
// durability invariant still holds — every acknowledged commit survives — or the
// reopen fail-stops on the corruption (a valid durability response, never silent
// loss). Either way there is no hang, no leak, and no lost ack.
func TestST3_FaultAtTeardown(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, seed := range durableTestSeeds[:3] {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		r, err := runCheckpointTeardown(ctx, seed, true)
		cancel()
		if err != nil {
			// A fail-stop on the reopen is a legitimate durability response to the
			// injected fault (the alternative to silent loss). Anything else is a
			// harness failure.
			if strings.Contains(err.Error(), "reopen") {
				t.Logf("ST3 fault seed %#x fail-stopped on reopen (valid durability response): %v", seed, err)
				continue
			}
			t.Fatalf("ST3 fault seed %#x harness error: %v", seed, err)
		}
		if v := r.violations(false); len(v) > 0 {
			t.Fatalf("ST3 fault seed %#x invariant breach: %v", seed, v)
		}
	}
}

// TestST3_CheckpointerStopTerminates is the design's open-hypothesis probe:
// while a controller hammers checkpoints back-to-back (so a RunCheckpoint is in
// flight), Checkpointer.Stop() must always return promptly, never deadlock. A
// watchdog fails the test if Stop does not join within the deadline. Run under
// -race -count=2 this exercises the Stop-vs-in-flight-checkpoint interleaving.
func TestST3_CheckpointerStopTerminates(t *testing.T) {
	defer goleak.VerifyNone(t)
	for iter := 0; iter < 3; iter++ {
		disk := NewSimDisk(NewSeed(uint64(iter)+1), 0)
		st, err := OpenSimStore(disk, fullStackStoreConfig())
		if err != nil {
			t.Fatalf("iter %d OpenSimStore: %v", iter, err)
		}
		// Seed some content so a checkpoint snapshots a non-empty graph.
		for i := 0; i < 10; i++ {
			runWrite(t, st, "CREATE (:Person {name:'seed', age:1})")
		}

		var unusedMu sync.Mutex
		cp := checkpoint.New[string, float64](
			checkpoint.Config{Dir: st.Config().dir}, st.graph, st.wlog, &unusedMu,
			checkpoint.WithCommitSerialiser[string, float64](st.store.RunUnderCommitLock),
			checkpoint.WithMapperCodec[string, float64](st.store.Codec()),
			checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend[string, float64]{disk: disk}),
			checkpoint.WithConstraintSpecs[string, float64](st.engine.ConstraintSpecsForSnapshot),
			checkpoint.WithIndexSpecs[string, float64](st.engine.IndexSpecsForSnapshot),
		)
		cpCtx, cpCancel := context.WithCancel(context.Background())
		cp.Start(cpCtx)

		stopHammer := make(chan struct{})
		var hwg sync.WaitGroup
		hwg.Add(1)
		go func() {
			defer hwg.Done()
			for {
				select {
				case <-stopHammer:
					return
				default:
				}
				if err := cp.Trigger(); err != nil {
					return // ErrCheckpointerStopped
				}
			}
		}()

		// Wait until at least one checkpoint has completed, so the loop is actively
		// checkpointing when Stop races it.
		deadline := time.Now().Add(20 * time.Second)
		for cp.Stats().Checkpoints == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("iter %d: no checkpoint completed within 20s", iter)
			}
			time.Sleep(time.Millisecond)
		}

		stopped := make(chan struct{})
		go func() { cp.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Fatalf("iter %d: Checkpointer.Stop did not return within 15s during in-flight checkpoints (deadlock)", iter)
		}

		close(stopHammer)
		hwg.Wait()
		cpCancel()
		if err := st.Close(); err != nil {
			t.Fatalf("iter %d st.Close: %v", iter, err)
		}
	}
}

// -----------------------------------------------------------------------------
// ST7 — readtx-isolation
// -----------------------------------------------------------------------------

// TestST7_ReadTxIsolation_Scenario runs the registered ST7 scenario across
// seeds: read-only transactions never observe a partial transaction under
// concurrent writers and a mid-read crash of other actors, and the recovered
// graph after a store crash holds a whole number of committed batches with a
// working read-only transaction.
func TestST7_ReadTxIsolation_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := readTxIsolationScenario()
	for _, seed := range durableTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("ST7 seed %#x run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("ST7 seed %#x violation:\n%s", seed, report)
		}
	}
}

// TestST7_NoLeakAcrossRuns repeats ST7 to catch a teardown-only leak (reader
// goroutines, the writer goroutine, or the reopened store).
func TestST7_NoLeakAcrossRuns(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := readTxIsolationScenario()
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		if _, err := sc.Run(ctx, durableTestSeeds[i]); err != nil {
			cancel()
			t.Fatalf("iteration %d: %v", i, err)
		}
		cancel()
	}
}

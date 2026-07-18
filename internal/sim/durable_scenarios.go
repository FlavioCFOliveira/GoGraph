package sim

// durable_scenarios.go adds the concurrent-durable cluster of the DST storage
// battery (rmp sprint 270): scenarios that own the REAL durability stack — a
// [SimDisk] plus an [OpenSimStore] store — and drive concurrent Bolt commits,
// a background checkpointer, and read-only transactions across a crash or a
// durable teardown, then reopen through real recovery and assert the ACID
// invariants at the NAME granularity the count-based oracle cannot see.
//
// # ST1 resolution: group-commit folds into ST2
//
// ST1 (assert group-commit fsync coalescing) has NO reachable surface through
// the Cypher engine and therefore none through this DST harness. The engine
// serialises every write commit — including the WAL fsync — under one exclusive
// visibility barrier (cypher/api.go execUnderBarrier -> commitUnderBarrier ->
// txn.Tx.CommitWALOnly -> wal.Writer.SyncGroup), so through the engine path
// SyncGroup is ALWAYS a solo leader with zero followers: the multi-member
// coalescing / fail-all behaviour is unreachable here. That behaviour is a pure
// store-layer property and is already unit-tested directly, with many goroutines
// committing through one txn.Store, in store/wal/syncgroup_test.go and
// store/txn/group_commit_durability_test.go.
//
// ST1 therefore folds into ST2: [durableCommitCrashScenario] DRIVES solo-leader
// SyncGroup on EVERY durable commit (adapter RunWrite -> RunInTx ->
// CommitWALOnly -> SyncGroup), and now additionally exercises it under a
// concurrent mid-flight fsync fault and crash recovery. What the DST adds over
// the store-layer tests is the end-to-end path (real Bolt wire -> engine ->
// store -> WAL -> SimDisk) under a crash, not the coalescing arithmetic.
//
// # ST7 note: read-committed, not snapshot, isolation
//
// [Engine.BeginReadTx] provides READ-COMMITTED isolation, not snapshot
// isolation: each ExecAny takes its OWN per-statement lpg.Graph.View, so a read
// observes the latest committed state and a later read in the same transaction
// MAY observe a concurrent writer's commit (cypher/exectx.go BeginReadTx). The
// literal "a read transaction does not observe a concurrent commit" is therefore
// NOT a property this engine has — true snapshot isolation is the deferred
// copy-on-write epic (#1671). [readTxIsolationScenario] instead certifies the
// isolation guarantee the engine DOES provide — no dirty / partial-transaction
// reads under concurrency — and that it survives a crash of the store (recovery
// restores a whole number of committed batches, never a torn one).

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
)

// Standard names for the concurrent-durable scenarios (sprint 270).
const (
	// ScenarioDurableCommitCrash is ST2 (which subsumes ST1): concurrent durable
	// Bolt commits with a mid-flight WAL fsync fault, then a crash and recovery.
	ScenarioDurableCommitCrash = "durable-commit-crash"
	// ScenarioCheckpointTeardown is ST3: a background checkpointer racing
	// concurrent committers, torn down via the crash-safe store.DB shutdown
	// order.
	ScenarioCheckpointTeardown = "checkpoint-teardown"
	// ScenarioReadTxIsolation is ST7: read-only transactions under concurrent
	// writers and a mid-read crash of other actors.
	ScenarioReadTxIsolation = "readtx-isolation"
)

// Bounded budgets for the concurrent-durable scenarios. They are SHORT-layer
// sized (12 connections, 16 ops each ≈ 192 create attempts) so a run stays well
// under the per-package 60 s ceiling even under -race, while producing enough
// committed transactions that the seed-chosen fault ordinal fires mid-flight.
const (
	durableConns   = 12
	durableOps     = 16
	durableWriters = 1.0 // all-writer mix: every op is a durable commit
)

// durableCommitFaultMin / durableCommitFaultSpan bound the seed-chosen Sync
// ordinal the ST2 fault fires on. The window [8, 8+32) sits comfortably below
// the ~192 commits a run issues, so a prefix of at least 7 commits is durably
// acked before the fault and a large suffix fails after it — the mid-flight
// fault the scenario needs (a clean-quiescence crash would test almost nothing).
const (
	durableCommitFaultMin  = 8
	durableCommitFaultSpan = 32
)

// durableDiskSeedMix decorrelates the disk fault sub-seed from the workload
// seed so the fault ordinal is a distinct, reproducible function of the run seed
// without sharing low-order bits with the connection/op sub-stream.
const durableDiskSeedMix uint64 = 0x5715_D157_0DD_F00D

// durableStoreConfig is the WAL-only durable layout ST2 and ST7 drive: a
// directed multigraph (openCypher additive-CREATE semantics, matching the
// SimServer engine) recovered via the WAL-only path ([recovery.ReplayWAL]).
func durableStoreConfig() simStoreConfig {
	return simStoreConfig{
		graphConfig: adjlist.Config{Directed: true, Multigraph: true},
	}
}

// fullStackStoreConfig is the checkpoint-backed layout ST3 drives: the WAL lives
// at db/wal and a published snapshot sits at db/snapshot, so recovery goes
// through the full snapshot+WAL path ([recovery.OpenFS]).
func fullStackStoreConfig() simStoreConfig {
	return simStoreConfig{
		graphConfig: adjlist.Config{Directed: true, Multigraph: true},
		dir:         "db",
	}
}

// -----------------------------------------------------------------------------
// ST2 — concurrent durable commits + mid-flight fault + crash recovery
// -----------------------------------------------------------------------------

// durableCommitCrashScenario is ST2 (subsuming ST1). It owns a real WAL-backed
// store on a [SimDisk], serves it over the genuine Bolt wire, drives many
// concurrent writer connections whose every commit is a solo-leader
// [wal.Writer.SyncGroup], arms a deterministic mid-flight fsync fault at a
// seed-chosen commit ordinal so one commit's fsync is poisoned (its client sees
// a wire FAILURE, never an ack; the WAL discards the un-synced suffix), then
// crashes (drop the engine, keep the SimDisk image — never a graceful flush) and
// reopens through real recovery. It asserts the durability/atomicity invariants
// as SETS of node names — never equality under the fault — so a durable-but-ack-
// lost commit stays legal. It is concurrent (leak/no-panic guarded), not
// bit-reproducible.
func durableCommitCrashScenario() Scenario {
	return Scenario{
		Name:        ScenarioDurableCommitCrash,
		Description: "concurrent durable Bolt commits + mid-flight WAL fsync fault + crash recovery (acked⊆recovered⊆issued; failures absent)",
		Mode:        ModeConcurrent,
		DefaultSeed: 0xD4B1E_C0117,
		Connections: durableConns,
		OpsPerConn:  durableOps,
		Mix:         &ConcurrentMix{WriterWeight: durableWriters},
		run: func(ctx context.Context, seed uint64) (*SimReport, error) {
			r, err := runDurableCommitCrash(ctx, seed, true)
			if err != nil {
				return nil, err
			}
			if v := r.violations(true); len(v) > 0 {
				return durableReport(seed, v), nil
			}
			return nil, nil
		},
	}
}

// durableCommitResult is the outcome of one ST2 run: the four name sets the
// invariants compare, plus whether the injected fault was armed. All are
// deduplicated sets so the subset checks are exact.
type durableCommitResult struct {
	acked     map[string]struct{}
	issued    map[string]struct{}
	failed    map[string]struct{}
	recovered map[string]struct{}
	partial   []string // recovered nodes missing their age property (torn CREATE)
	fault     bool
}

// runDurableCommitCrash performs one ST2 run and returns the collected sets. It
// owns and tears down every resource (store, server, reopened store); the only
// state that outlives it is the returned sets. When faultEnabled is true it arms
// a one-shot Sync fault at a seed-chosen ordinal.
func runDurableCommitCrash(ctx context.Context, seed uint64, faultEnabled bool) (durableCommitResult, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0: only the armed one-shot fault fires
	cfg := durableStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return durableCommitResult{}, fmt.Errorf("sim: ST2 open store: %w", err)
	}
	srv, err := newSimServerWithLogger(st.Engine(), clock.Real(), quietSimLogger())
	if err != nil {
		_ = st.Close()
		return durableCommitResult{}, fmt.Errorf("sim: ST2 server: %w", err)
	}

	// Arm the mid-flight fsync fault at a seed-chosen commit ordinal, from the
	// disk sub-seed, immediately before issuing so the ordinal counts from the
	// first workload commit. The WHICH commit is the ordinal-th is
	// non-deterministic (interleaving); that a fault fires at that ordinal is
	// deterministic (the hybrid model).
	if faultEnabled {
		fs := NewSeed(seed ^ durableDiskSeedMix)
		k := int64(durableCommitFaultMin + fs.IntN(durableCommitFaultSpan))
		disk.ArmSyncFaultAt(k)
	}

	// Drive the concurrent writers to completion. RunConcurrent joins every
	// client goroutine internally (crash protocol steps a+b: stop issuing at the
	// bounded op count, then wg.Wait) before returning the per-name sets.
	res, runErr := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        seed,
		Connections: durableConns,
		OpsPerConn:  durableOps,
		Mix:         &ConcurrentMix{WriterWeight: durableWriters},
	})
	if runErr != nil {
		_ = srv.Close()
		st.Crash()
		return durableCommitResult{}, fmt.Errorf("sim: ST2 concurrent run: %w", runErr)
	}

	// Crash protocol (order is load-bearing):
	//   (c) join the server goroutines — flushes nothing (the server owns no
	//       Closer over the WAL), so no acknowledged-but-not-durable frame is
	//       made durable here;
	//   (d) crash the SimDisk (drop the engine, keep the byte image) — never a
	//       graceful SimStore.Close, which would flush and defeat the crash.
	// Every client goroutine is already joined (RunConcurrent) and the server is
	// now joined, so no in-flight fsync can still resolve after the crash.
	_ = srv.Close()
	st.Crash()

	// (e) reopen through real recovery, (f) read the recovered graph.
	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return durableCommitResult{}, fmt.Errorf("sim: ST2 reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()

	recovered, partial, err := recoveredPersonNames(ctx, st2.Engine())
	if err != nil {
		return durableCommitResult{}, fmt.Errorf("sim: ST2 read recovered graph: %w", err)
	}

	return durableCommitResult{
		acked:     toSet(res.AckedNames),
		issued:    toSet(res.IssuedNames),
		failed:    toSet(res.FailedNames),
		recovered: recovered,
		partial:   partial,
		fault:     faultEnabled,
	}, nil
}

// violations checks the ST2 invariants and returns any breach. The checks are
// order-independent set relations, never equality under a fault:
//
//   - Durability: acked ⊆ recovered — every acknowledged commit survived (a
//     durable-but-ack-lost commit only makes recovered LARGER, never smaller,
//     which is legal and not asserted against).
//   - No phantom: recovered ⊆ issued — recovery invented no node.
//   - Atomicity of failures: failed ∩ recovered = ∅ — a commit whose client saw
//     an explicit FAILURE applied nothing durable.
//   - Atomicity of survivors: no recovered node is missing its age property (no
//     torn CREATE was resurrected).
//
// When faultReq is true it additionally asserts non-vacuity — the fault actually
// bit: at least one commit was acked, at least one commit explicitly failed, and
// not every issued commit was acked — so a run that silently stopped faulting
// cannot pass by asserting nothing.
func (r durableCommitResult) violations(faultReq bool) []Violation {
	var v []Violation
	for _, missing := range setMinus(r.acked, r.recovered) {
		v = append(v, Violation{
			Kind:    ViolationACIDDurability,
			Op:      "<durable-commit>",
			Message: fmt.Sprintf("acknowledged commit %q missing after recovery (acked=%d recovered=%d)", missing, len(r.acked), len(r.recovered)),
		})
	}
	for _, phantom := range setMinus(r.recovered, r.issued) {
		v = append(v, Violation{
			Kind:    ViolationACIDConsistency,
			Op:      "<phantom>",
			Message: fmt.Sprintf("recovered node %q was never issued (phantom write; recovered=%d issued=%d)", phantom, len(r.recovered), len(r.issued)),
		})
	}
	for _, resurrected := range setIntersect(r.failed, r.recovered) {
		v = append(v, Violation{
			Kind:    ViolationACIDAtomicity,
			Op:      "<failed-resurrected>",
			Message: fmt.Sprintf("commit %q the client saw FAIL is present after recovery (uncommitted state leaked in)", resurrected),
		})
	}
	for _, torn := range r.partial {
		v = append(v, Violation{
			Kind:    ViolationACIDAtomicity,
			Op:      "<torn-create>",
			Message: fmt.Sprintf("recovered node %q lacks its age property (a torn transaction was resurrected)", torn),
		})
	}
	if faultReq {
		switch {
		case len(r.acked) == 0:
			v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<non-vacuity>", Message: "no commit was acknowledged — the workload did not exercise the durable write path"})
		case len(r.failed) == 0:
			v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<non-vacuity>", Message: "no commit explicitly failed — the injected mid-flight fault did not bite"})
		case len(r.acked) >= len(r.issued):
			v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<non-vacuity>", Message: fmt.Sprintf("every issued commit was acked (acked=%d issued=%d) — the fault did not lose the poisoned commit", len(r.acked), len(r.issued))})
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// ST3 — background Checkpointer racing committers + durable teardown
// -----------------------------------------------------------------------------

// checkpointTeardownThreshold is how many WAL syncs (durable commits) must land
// before the teardown fires, so the crash-safe store.DB shutdown genuinely races
// a fleet of in-flight commits (the remaining ~180) rather than starting on an
// almost-empty store.
const checkpointTeardownThreshold = 4

// checkpointTeardownScenario is ST3. It runs a background
// [checkpoint.Checkpointer] against a full-stack durable store while concurrent
// writers commit over the Bolt wire, then tears the store down through the
// crash-safe [store.DB] shutdown order (final checkpoint -> stop checkpointer ->
// close WAL under the commit-lock quiesce) — racing the close against a fleet of
// in-flight commits. It reopens through real recovery and asserts every
// acknowledged commit survived (recovered ⊇ acked) with no phantom, that the
// checkpointer goroutine joined (goleak, in the test wrapper), and that no
// wal.ErrWriterClosed ever surfaced as an acked commit (guaranteed by the
// inflight drain, observed as acked ⊆ recovered). It is concurrent, not
// bit-reproducible.
func checkpointTeardownScenario() Scenario {
	return Scenario{
		Name:        ScenarioCheckpointTeardown,
		Description: "background checkpointer racing committers, torn down via store.DB crash-safe order (recovered ⊇ acked; no ErrWriterClosed into an ack)",
		Mode:        ModeConcurrent,
		DefaultSeed: 0xC4EC_0117,
		Connections: durableConns,
		OpsPerConn:  durableOps,
		Mix:         &ConcurrentMix{WriterWeight: durableWriters},
		run: func(ctx context.Context, seed uint64) (*SimReport, error) {
			r, err := runCheckpointTeardown(ctx, seed, false)
			if err != nil {
				return nil, err
			}
			if v := r.violations(false); len(v) > 0 {
				return durableReport(seed, v), nil
			}
			return nil, nil
		},
	}
}

// checkpointTeardownResult reuses the ST2 set shape: the acked/issued/recovered
// name sets the teardown-durability invariants compare. (failed is unused here —
// ST3 asserts a superset, not failure-absence.)
type checkpointTeardownResult struct {
	acked     map[string]struct{}
	issued    map[string]struct{}
	recovered map[string]struct{}
	partial   []string
}

// violations checks the ST3 teardown-durability invariants: acked ⊆ recovered
// (no acknowledged commit lost across the checkpoint-race + durable teardown,
// which is exactly the "no ErrWriterClosed leaks into an ack" guarantee the
// inflight drain provides), recovered ⊆ issued (no phantom), and no torn CREATE
// resurrected. requireAcked additionally asserts at least one commit was acked
// (non-vacuity) — kept off for the fault variant, where an early poison can
// legitimately leave the acked set empty.
func (r checkpointTeardownResult) violations(requireAcked bool) []Violation {
	var v []Violation
	for _, missing := range setMinus(r.acked, r.recovered) {
		v = append(v, Violation{
			Kind:    ViolationACIDDurability,
			Op:      "<teardown-durability>",
			Message: fmt.Sprintf("acknowledged commit %q lost across the durable teardown (acked=%d recovered=%d)", missing, len(r.acked), len(r.recovered)),
		})
	}
	for _, phantom := range setMinus(r.recovered, r.issued) {
		v = append(v, Violation{
			Kind:    ViolationACIDConsistency,
			Op:      "<phantom>",
			Message: fmt.Sprintf("recovered node %q was never issued (phantom write)", phantom),
		})
	}
	for _, torn := range r.partial {
		v = append(v, Violation{
			Kind:    ViolationACIDAtomicity,
			Op:      "<torn-create>",
			Message: fmt.Sprintf("recovered node %q lacks its age property (a torn transaction was resurrected)", torn),
		})
	}
	if requireAcked && len(r.acked) == 0 {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<non-vacuity>", Message: "no commit was acknowledged before the teardown — the race did not exercise durable commits"})
	}
	return v
}

// runCheckpointTeardown performs one ST3 run. A background checkpointer is
// hammered by a controller goroutine so checkpoints fire back-to-back while
// concurrent committers write; once a handful of commits are durable the store
// is torn down through the crash-safe [store.DB] order, racing the WAL close
// against the still-running committers. When faultAtTeardown is true a one-shot
// fsync fault is armed at the instant of teardown so it lands on the final
// checkpoint or a racing commit; the teardown's own close error is then
// tolerated (durability of already-acked commits does not depend on the final
// checkpoint or a clean close). It returns the acked/issued/recovered sets.
func runCheckpointTeardown(ctx context.Context, seed uint64, faultAtTeardown bool) (checkpointTeardownResult, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0)
	cfg := fullStackStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return checkpointTeardownResult{}, fmt.Errorf("sim: ST3 open store: %w", err)
	}
	srv, err := newSimServerWithLogger(st.Engine(), clock.Real(), quietSimLogger())
	if err != nil {
		_ = st.Close()
		return checkpointTeardownResult{}, fmt.Errorf("sim: ST3 server: %w", err)
	}

	// Background checkpointer wired with the SAME seams SimStore.Checkpoint uses:
	// the store's commit serialiser (which also drains in-flight group commits),
	// the mapper codec, the SimDisk snapshot backend, and the engine's constraint
	// / index specs — so a checkpoint that truncates the WAL prefix cannot lose a
	// constraint or index. Config carries no MaxAge, so the loop fires only on an
	// explicit Trigger (driven by the controller below), keeping the cadence
	// controlled rather than wall-clock-dependent.
	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](
		checkpoint.Config{Dir: cfg.dir}, st.graph, st.wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.store.Codec()),
		checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend{disk: disk}),
		checkpoint.WithConstraintSpecs[string, float64](st.engine.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](st.engine.IndexSpecsForSnapshot),
	)
	cpCtx, cpCancel := context.WithCancel(context.Background())
	defer cpCancel()
	cp.Start(cpCtx)

	// The crash-safe teardown owner: final checkpoint (best-effort) -> stop the
	// checkpointer (so no later WAL call can race) -> close the WAL under the
	// store's commit-lock quiesce (so an in-flight commit finishes durably before
	// the flush, and any commit that starts after gets a clean ErrWriterClosed).
	db := store.New(st.wlog,
		store.WithCheckpointer(cp),
		store.WithFinalCheckpoint(),
		store.WithQuiesce(st.store.RunUnderCommitLock),
	)

	// Committers: concurrent writer connections over the Bolt wire, launched in a
	// goroutine so the teardown can race them mid-flight.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	var (
		res            ConcurrentResult
		runErr         error
		committersDone = make(chan struct{})
	)
	go func() {
		res, runErr = RunConcurrent(runCtx, srv, ConcurrentConfig{
			Seed:        seed,
			Connections: durableConns,
			OpsPerConn:  durableOps,
			Mix:         &ConcurrentMix{WriterWeight: durableWriters},
		})
		close(committersDone)
	}()

	// Controller: hammer checkpoints back-to-back so a snapshot publish + WAL
	// prefix-truncate races the committers continuously. Trigger returns
	// ErrCheckpointerStopped once db.Close stops the loop, which ends the loop.
	stopTrigger := make(chan struct{})
	var ctrlWG sync.WaitGroup
	ctrlWG.Add(1)
	go func() {
		defer ctrlWG.Done()
		for {
			select {
			case <-stopTrigger:
				return
			default:
			}
			if err := cp.Trigger(); err != nil {
				return // ErrCheckpointerStopped: the loop is gone
			}
		}
	}()

	// Wait (condition-bounded, no time.Sleep coordination) for a handful of
	// commits to become durable so the teardown truly races in-flight commits.
	waitForSyncProgress(disk, checkpointTeardownThreshold, time.Now().Add(30*time.Second))

	// Optionally arm a one-shot fsync fault at the teardown boundary: the NEXT
	// sync (the final checkpoint's, or a racing commit's) faults.
	if faultAtTeardown {
		disk.ArmSyncFaultAt(disk.SyncCount() + 1)
	}

	// Tear down, RACING the in-flight committers + checkpointer. The close error
	// is tolerated when a fault was injected (a poisoned WAL or a failed final
	// checkpoint does not endanger already-acked commits).
	closeErr := db.CloseCtx(ctx)
	if closeErr != nil && !faultAtTeardown {
		// A clean teardown must not error.
		close(stopTrigger)
		ctrlWG.Wait()
		runCancel()
		<-committersDone
		_ = srv.Close()
		return checkpointTeardownResult{}, fmt.Errorf("sim: ST3 clean teardown close: %w", closeErr)
	}

	// Drain everything the crash-safe order did not already join.
	close(stopTrigger)
	ctrlWG.Wait()
	runCancel()
	<-committersDone
	if runErr != nil {
		_ = srv.Close()
		return checkpointTeardownResult{}, fmt.Errorf("sim: ST3 concurrent run: %w", runErr)
	}
	_ = srv.Close()

	// Reopen through real recovery (snapshot + WAL suffix, or WAL-only if no
	// snapshot published) and read the recovered graph.
	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return checkpointTeardownResult{}, fmt.Errorf("sim: ST3 reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()

	recovered, partial, err := recoveredPersonNames(ctx, st2.Engine())
	if err != nil {
		return checkpointTeardownResult{}, fmt.Errorf("sim: ST3 read recovered graph: %w", err)
	}

	return checkpointTeardownResult{
		acked:     toSet(res.AckedNames),
		issued:    toSet(res.IssuedNames),
		recovered: recovered,
		partial:   partial,
	}, nil
}

// -----------------------------------------------------------------------------
// ST7 — read-only transaction isolation under concurrency + crash
// -----------------------------------------------------------------------------

// readTxBatch is the atomic write unit ST7 commits: a single transaction that
// creates readTxBatch Person nodes. A read-only transaction that ever observes a
// node count NOT divisible by this is a dirty / partial-transaction read (an
// Isolation breach), so the batch size is the isolation oracle.
const readTxBatch = 5

// readTxBatches / readTxReaders bound the ST7 workload (short-layer). Half the
// reader goroutines are cancelled mid-run to model a crash of other actors while
// the survivors keep reading.
const (
	readTxBatches = 120
	readTxReaders = 8
)

// readTxIsolationScenario is ST7. Against a real durable store it opens
// read-only transactions ([Engine.BeginReadTx]) that repeatedly count nodes
// while a writer commits atomic batches concurrently, and asserts NO read ever
// observes a partial transaction (the count is always a multiple of the batch —
// read-committed isolation). Half the readers are abruptly cancelled mid-run
// (modelling a crash of other actors) while the survivors keep reading
// consistently. It then crashes the store itself, reopens through recovery, and
// asserts recovery restored a whole number of committed batches (never a torn
// one) and that a fresh read-only transaction works on the recovered engine. It
// is concurrent (leak/no-panic guarded), not bit-reproducible.
//
// It does NOT assert snapshot isolation (a read observing a fixed point-in-time
// for the transaction's whole lifetime): BeginReadTx is read-committed by design
// (see the ST7 note at the top of this file), so a later read MAY observe a
// concurrent commit — asserting otherwise would test behaviour this engine does
// not have.
func readTxIsolationScenario() Scenario {
	return Scenario{
		Name:        ScenarioReadTxIsolation,
		Description: "read-only tx: no dirty/partial reads under concurrent writers + a mid-read crash of other actors, preserved across a store crash",
		Mode:        ModeConcurrent,
		DefaultSeed: 0x8EAD_7811,
		Connections: readTxReaders,
		run:         runReadTxIsolation,
	}
}

// runReadTxIsolation performs one ST7 run and returns a report (nil == passed)
// or a harness error. It drives the concurrency+isolation phase, then the
// crash+recovery phase, asserting the read-committed and atomicity invariants.
func runReadTxIsolation(ctx context.Context, seed uint64) (*SimReport, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0)
	cfg := durableStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: ST7 open store: %w", err)
	}
	eng := st.Engine()

	// --- Phase A: concurrency + isolation. ---
	var (
		dirty        dirtyObs
		readErr      errObs
		wg           sync.WaitGroup
		done         = make(chan struct{})
		victimCtx    context.Context
		cancelVictim context.CancelFunc
	)
	victimCtx, cancelVictim = context.WithCancel(ctx)
	defer cancelVictim()

	// Reader goroutines: half are "victims" bound to victimCtx (cancelled
	// mid-run, modelling a crashed actor); half are "survivors" on the run ctx
	// that must keep reading consistently. Every read must be a whole batch.
	for i := 0; i < readTxReaders; i++ {
		readerCtx := ctx
		if i%2 == 0 {
			readerCtx = victimCtx
		}
		wg.Add(1)
		go func(rctx context.Context) {
			defer wg.Done()
			readTxLoop(rctx, eng, done, &dirty, &readErr)
		}(readerCtx)
	}

	// Writer: commit readTxBatches atomic batches of readTxBatch nodes each,
	// through the engine's autocommit durable write path. Count acked batches.
	var ackedBatches int
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < readTxBatches; i++ {
			if ctx.Err() != nil {
				return
			}
			q := fmt.Sprintf(
				"CREATE (:Person {name:'b%d-0', age:0}),(:Person {name:'b%d-1', age:1}),(:Person {name:'b%d-2', age:2}),(:Person {name:'b%d-3', age:3}),(:Person {name:'b%d-4', age:4})",
				i, i, i, i, i)
			r, werr := eng.RunInTxAny(ctx, q, nil)
			if werr != nil {
				readErr.set(fmt.Errorf("writer batch %d: %w", i, werr))
				return
			}
			for r.Next() {
			}
			drainErr := r.Err()
			_ = r.Close()
			if drainErr != nil {
				readErr.set(fmt.Errorf("writer batch %d drain: %w", i, drainErr))
				return
			}
			ackedBatches++
			// Cancel the victim readers roughly midway, modelling a crash of other
			// actors while the survivors keep reading.
			if i == readTxBatches/2 {
				cancelVictim()
			}
		}
	}()

	<-writerDone
	close(done)
	if !waitWGTimeout(&wg, 30*time.Second) {
		st.Crash()
		return nil, fmt.Errorf("sim: ST7 reader goroutines did not drain within 30s (possible deadlock)")
	}

	if e := readErr.get(); e != nil {
		st.Crash()
		return nil, fmt.Errorf("sim: ST7 phase-A error: %w", e)
	}
	if d, bad := dirty.get(); bad {
		st.Crash()
		return &SimReport{
			Seed:     seed,
			FailedOp: Op{Kind: OpMatch, Cypher: "MATCH (n:Person) RETURN count(n)"},
			Violations: []Violation{{
				Kind:    ViolationACIDIsolation,
				Op:      "<read-tx>",
				Message: fmt.Sprintf("a read-only transaction observed count=%d, not a multiple of the batch size %d (a partial transaction leaked across the isolation barrier)", d, readTxBatch),
			}},
		}, nil
	}

	// --- Phase B: crash + recovery (durability + atomicity). ---
	st.Crash() // SIGKILL: drop the engine, keep the SimDisk WAL image.
	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: ST7 reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()

	recCount, err := scalarCountViaEngine(ctx, st2.Engine(), "MATCH (n:Person) RETURN count(n)")
	if err != nil {
		return nil, fmt.Errorf("sim: ST7 recovered count: %w", err)
	}
	var v []Violation
	if recCount%int64(readTxBatch) != 0 {
		v = append(v, Violation{
			Kind:    ViolationACIDAtomicity,
			Op:      "<recovery>",
			Message: fmt.Sprintf("recovered node count %d is not a multiple of the batch size %d (a torn transaction survived recovery)", recCount, readTxBatch),
		})
	}
	if lo := int64(ackedBatches) * int64(readTxBatch); recCount < lo {
		v = append(v, Violation{
			Kind:    ViolationACIDDurability,
			Op:      "<recovery>",
			Message: fmt.Sprintf("recovered %d nodes < %d acknowledged (%d batches lost after crash)", recCount, lo, ackedBatches),
		})
	}
	if hi := int64(readTxBatches) * int64(readTxBatch); recCount > hi {
		v = append(v, Violation{
			Kind:    ViolationACIDConsistency,
			Op:      "<recovery>",
			Message: fmt.Sprintf("recovered %d nodes > %d ever issued (phantom state after crash)", recCount, hi),
		})
	}

	// A fresh read-only transaction must work on the recovered engine and see a
	// whole number of batches — the read-tx path is intact post-crash.
	rtx, err := st2.Engine().BeginReadTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("sim: ST7 recovered BeginReadTx: %w", err)
	}
	postCount, postErr := readTxCount(rtx)
	_ = rtx.Rollback()
	if postErr != nil {
		return nil, fmt.Errorf("sim: ST7 recovered read-tx count: %w", postErr)
	}
	if postCount%int64(readTxBatch) != 0 {
		v = append(v, Violation{
			Kind:    ViolationACIDIsolation,
			Op:      "<recovered-read-tx>",
			Message: fmt.Sprintf("recovered read-only transaction observed count=%d, not a multiple of %d", postCount, readTxBatch),
		})
	}

	if len(v) > 0 {
		return durableReport(seed, v), nil
	}
	return nil, nil
}

// readTxLoop repeatedly opens a read-only transaction and counts Person nodes
// until done is closed or its context is cancelled, recording a dirty read (a
// count not divisible by the batch) or a read error. It performs TWO counts per
// transaction to exercise read-committed isolation across statements — the two
// MAY differ (a concurrent commit landed between them, which is legal), but each
// individually must be a whole batch (never a partial transaction).
func readTxLoop(ctx context.Context, eng *cypher.Engine, done <-chan struct{}, dirty *dirtyObs, readErr *errObs) {
	for {
		select {
		case <-done:
			return
		default:
		}
		tx, err := eng.BeginReadTx(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // a cancelled victim is an expected, clean stop
			}
			readErr.set(fmt.Errorf("BeginReadTx: %w", err))
			return
		}
		c1, e1 := readTxCount(tx)
		c2, e2 := readTxCount(tx)
		_ = tx.Rollback()
		if e1 != nil || e2 != nil {
			if ctx.Err() != nil {
				return // cancelled mid-read: expected
			}
			readErr.set(fmt.Errorf("read-tx count: %w", firstErr(e1, e2)))
			return
		}
		if c1%int64(readTxBatch) != 0 {
			dirty.set(c1)
		}
		if c2%int64(readTxBatch) != 0 {
			dirty.set(c2)
		}
	}
}

// readTxCount runs one count statement inside the read-only transaction and
// returns the scalar. It never commits or mutates.
func readTxCount(tx *cypher.ExplicitTx) (int64, error) {
	res, err := tx.ExecAny("MATCH (n:Person) RETURN count(n)", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var c int64
	if res.Next() {
		if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			c = int64(iv)
		}
	}
	return c, res.Err()
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// recoveredPersonNames reads every recovered Person node's name and reports any
// whose age property is absent (a torn CREATE). The name set is the durability /
// phantom oracle; the partial slice is the atomicity witness.
func recoveredPersonNames(ctx context.Context, eng *cypher.Engine) (names map[string]struct{}, partial []string, err error) {
	res, err := eng.Run(ctx, "MATCH (n:Person) RETURN n.name, n.age", nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = res.Close() }()
	names = make(map[string]struct{})
	for res.Next() {
		s, ok := res.ValueAt(0).(expr.StringValue)
		if !ok {
			continue // a node without a name is off-model; skip it
		}
		name := string(s)
		names[name] = struct{}{}
		if _, ok := res.ValueAt(1).(expr.IntegerValue); !ok {
			partial = append(partial, name)
		}
	}
	return names, partial, res.Err()
}

// scalarCountViaEngine runs a single-column count query on the read path and
// returns the scalar.
func scalarCountViaEngine(ctx context.Context, eng *cypher.Engine, query string) (int64, error) {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var c int64
	if res.Next() {
		if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			c = int64(iv)
		}
	}
	return c, res.Err()
}

// waitForSyncProgress blocks until the disk has performed at least atLeast syncs
// or the deadline passes, yielding the processor between checks. It is a bounded
// condition wait (no time.Sleep coordination): it gates a teardown on durable
// progress so the race is against genuinely in-flight commits.
func waitForSyncProgress(disk *SimDisk, atLeast int64, deadline time.Time) {
	for disk.SyncCount() < atLeast {
		if time.Now().After(deadline) {
			return
		}
		runtime.Gosched()
	}
}

// waitWGTimeout waits for wg with a deadline, reporting whether it drained in
// time (false signals a probable deadlock the caller surfaces as a hard error).
func waitWGTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// durableReport renders a set of violations as a SimReport for a concurrent-
// durable scenario.
func durableReport(seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<durable recovery>"},
		Violations: v,
	}
}

// toSet builds a set from a slice, deduplicating.
func toSet(xs []string) map[string]struct{} {
	s := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		s[x] = struct{}{}
	}
	return s
}

// setMinus returns the elements of a not present in b.
func setMinus(a, b map[string]struct{}) []string {
	var out []string
	for x := range a {
		if _, ok := b[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}

// setIntersect returns the elements present in both a and b.
func setIntersect(a, b map[string]struct{}) []string {
	var out []string
	for x := range a {
		if _, ok := b[x]; ok {
			out = append(out, x)
		}
	}
	return out
}

// dirtyObs records the first dirty-read value observed (a count not divisible by
// the batch), safe for concurrent use.
type dirtyObs struct {
	mu   sync.Mutex
	val  int64
	seen bool
}

func (d *dirtyObs) set(v int64) {
	d.mu.Lock()
	if !d.seen {
		d.seen = true
		d.val = v
	}
	d.mu.Unlock()
}

func (d *dirtyObs) get() (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.val, d.seen
}

// errObs records the first error observed across goroutines, safe for
// concurrent use.
type errObs struct {
	err error
	mu  sync.Mutex
}

func (e *errObs) set(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *errObs) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

package sim

// production_profile.go — the named production-profile scenario (rmp #2441):
// one command that simulates a realistic multi-client production environment
// over the DURABLE store and reports MVCC, concurrency, and durability
// evidence.
//
// The profile combines every role this sprint built, at production-like
// concurrency, over the checkpoint-backed SimDisk store, in CRASH CYCLES:
//
//	cycle: open the durable store → serve it over the real Bolt wire → drive a
//	mixed population (contended and disjoint transactional writers with mixed
//	transaction sizes, atomic-batch writers, during-run isolation readers,
//	same-connection RYOW probes, plain writers/readers and overload traffic) →
//	CHECKPOINT while that traffic is in flight → join every client and the
//	server → CRASH the disk (a HOST crash, [SimDisk.CrashHost]) → reopen through real recovery
//	(snapshot + WAL tail) → adjudicate → commit post-recovery beacons.
//
// The run then forces ONE pure-snapshot crossing (rmp #2468): a checkpoint that
// folds every committed op and empties the WAL, a crash, and a reopen that
// replays nothing.
//
// # Why the checkpoint is in the cycle (rmp #2469)
//
// Without it every recovery re-derives the MVCC clock from a COMPLETE WAL, so
// the case that matters is never entered: the prefix carrying the early
// timestamps truncated away, leaving the snapshot manifest's recorded instant as
// the only durable record of them. The checkpoint runs CONCURRENTLY with the
// MVCC traffic — a checkpoint taken over a quiesced store proves nothing about a
// production one — and the overlap is measured (the MVCC clock advances by one
// per published commit) rather than assumed.
//
// The durability oracle is TRANSACTION-GRANULAR, using the rmp #2439 ledgers
// accumulated across cycles: every acknowledged transaction marker and every
// acknowledged autocommit name must be recovered (acked ⊆ recovered); every
// refused marker and explicitly-failed name must be absent; and the recovered
// set must contain nothing the run never issued (recovered ⊆ issued). The
// contended counters carry across cycles: each cycle's recovered counter value
// must equal the accumulated acknowledged increments — zero lost updates
// across crashes. The during-run oracles (rmp #2440) run inside every cycle
// and must stay silent.

import (
	"context"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// ScenarioProductionProfile is the catalogue key of the production-profile
// scenario.
const ScenarioProductionProfile = "production-profile"

// productionProfileCheckpointLead is how many durable syncs the cycle waits for
// before taking its checkpoint, so the checkpoint lands INSIDE the traffic
// rather than before it starts or after it drains. It is deliberately small
// relative to the op budget: the point is a checkpoint with commits still
// arriving, not a checkpoint at a particular commit.
const productionProfileCheckpointLead = 6

// productionProfileWaitBudget bounds the wait for that durable progress. It is a
// liveness backstop, never a correctness knob: on expiry the cycle checkpoints
// anyway and the overlap gate below reports what was actually measured.
const productionProfileWaitBudget = 30 * time.Second

// productionProfileConfig sizes one profile run. The short-layer defaults keep
// the whole scenario well inside the package's 60s budget; the soak layer
// runs the full-scale configuration (see production_profile_soak_test.go).
type productionProfileConfig struct {
	connections int
	opsPerConn  int
	cycles      int
	counters    int
}

// shortProductionProfile is the short-layer size: enough concurrency and
// enough cycles to exercise every role, conflict, and two real crash+recovery
// boundaries.
func shortProductionProfile() productionProfileConfig {
	return productionProfileConfig{connections: 24, opsPerConn: 8, cycles: 2, counters: 2}
}

// productionProfileMix is the role population: transactional writers with
// mixed sizes (contended and disjoint), atomic-batch writers, during-run
// oracle readers, RYOW probes, plain writers and readers, and overload
// traffic.
func productionProfileMix() *ConcurrentMix {
	return &ConcurrentMix{
		WriterWeight:      0.10,
		ReaderWeight:      0.10,
		OverloadWeight:    0.05,
		TxWriterWeight:    0.20,
		TxContendedWeight: 0.25,
		BatchWriterWeight: 0.15,
		IsoReaderWeight:   0.10,
		RYOWWriterWeight:  0.05,
	}
}

// productionProfileScenario builds the catalogue entry. The run override owns
// the whole multi-cycle crash protocol, so the scenario is ModeConcurrent for
// reporting purposes but never dispatches through the generic runner.
func productionProfileScenario() Scenario {
	return Scenario{
		Name: ScenarioProductionProfile,
		Description: "production profile: mixed transactional/batch/reader/overload population over the durable store, " +
			"with crash cycles, checkpoints taken inside live traffic, transaction-granular durability adjudication, " +
			"and MVCC clock/sequence recovery across the snapshot boundary",
		Mode:        ModeConcurrent,
		DefaultSeed: 0x9600D0C5,
		Connections: shortProductionProfile().connections,
		OpsPerConn:  shortProductionProfile().opsPerConn,
		run: func(ctx context.Context, seed uint64) (*SimReport, error) {
			return runProductionProfile(ctx, seed, shortProductionProfile())
		},
	}
}

// productionProfileCycleEvidence is what one crash cycle measured about its
// checkpoint and the recovery that followed it. It is kept so the run — and a
// test — can prove the cycle entered the cases it claims rather than assuming
// it did.
type productionProfileCycleEvidence struct {
	cycle int
	// commitsDuringCheckpoint is how far the MVCC clock advanced across the
	// checkpoint call: one instant per published commit, so a positive value is
	// MEASURED evidence that MVCC traffic overlapped the checkpoint.
	commitsDuringCheckpoint uint64
	// clientsRunningAfterCheckpoint records that the client population had not
	// drained when the checkpoint returned. It is the coarser half of the same
	// question, and it holds even in the cycle where no commit happened to land
	// inside the window.
	clientsRunningAfterCheckpoint bool
	// walBeforeCheckpoint / walAfterCheckpoint are the durable WAL byte lengths
	// either side of the checkpoint: the prefix it reclaimed is the record it
	// removed from the next recovery's reach.
	walBeforeCheckpoint int64
	walAfterCheckpoint  int64
	// snapshotInstant is the MVCC instant the published image recorded.
	snapshotInstant uint64
	// recovery is what the post-crash reopen measured (rmp #2469).
	recovery mvccRecoveryEvidence
	// substrate is the MVCC-substrate telemetry watched across this cycle's
	// RECOVERED store (rmp #2470): read once the reopen is complete and again
	// after the post-recovery commits, both at genuinely quiescent points.
	//
	// It is scoped to ONE store instance deliberately. A crash replaces the graph
	// wholesale and the substrate's counters — commits, the watermark, the vacuum's
	// pass count — restart at zero with it, so folding readings from either side of
	// a crash into one record would show the watermark moving BACKWARDS and the
	// commit count collapsing, and would report the harness's own bookkeeping as an
	// isolation defect.
	substrate mvccSubstrateEvidence
}

// productionProfileEvidence is what one profile run measured, across its crash
// cycles and its forced pure-snapshot crossing.
type productionProfileEvidence struct {
	cycles []productionProfileCycleEvidence
	// boundary is the measured pure-snapshot crossing (rmp #2468).
	boundary snapshotBoundary
	// crossing is the MVCC clock and sequence evidence of the recovery that
	// crossing produced — the pure-snapshot case, which no MVCC scenario entered
	// before rmp #2469.
	crossing mvccRecoveryEvidence
}

// runProductionProfile executes the profile once and returns a report (nil ==
// passed) or a harness error.
func runProductionProfile(ctx context.Context, seed uint64, size productionProfileConfig) (*SimReport, error) {
	report, _, err := runProductionProfileEvidence(ctx, seed, size)
	return report, err
}

// runProductionProfileEvidence is the body of [runProductionProfile], returning
// the measurements as well as the verdict so a test can assert the run was not
// vacuous — that a checkpoint really overlapped live MVCC traffic, that a
// recovery really went through snapshot-plus-WAL-tail, and that one really went
// through the snapshot alone.
func runProductionProfileEvidence(ctx context.Context, seed uint64, size productionProfileConfig) (*SimReport, productionProfileEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0)
	// FULL-STACK, not WAL-only: the cycle checkpoints, so the WAL prefix is
	// truncated and recovery goes through the snapshot+WAL path (rmp #2469).
	cfg := fullStackStoreConfig()
	var ev productionProfileEvidence

	// Ledgers accumulated across every cycle (the durable store persists, so
	// the oracle must too).
	var (
		ackedSet    = make(map[string]bool)
		refusedSet  = make(map[string]bool)
		issuedSet   = make(map[string]bool)
		expectedCtr = make([]int64, size.counters)
		violations  []Violation
	)
	fail := func(op, format string, args ...any) {
		violations = append(violations, Violation{
			Kind: ViolationACIDDurability, Op: op, Message: fmt.Sprintf(format, args...),
		})
	}

	for cycle := 0; cycle < size.cycles && len(violations) == 0; cycle++ {
		st, err := OpenSimStore(disk, cfg)
		if err != nil {
			return nil, ev, fmt.Errorf("sim: production profile cycle %d open: %w", cycle, err)
		}
		srv, err := newSimServerWithLogger(st.Engine(), clock.Real(), quietSimLogger())
		if err != nil {
			_ = st.Close()
			return nil, ev, fmt.Errorf("sim: production profile cycle %d server: %w", cycle, err)
		}

		// The client population runs in the background so the checkpoint below
		// lands while transactions are in flight. RunConcurrent stops at its
		// bounded op count and joins every client goroutine before it returns.
		var (
			res            ConcurrentResult
			runErr         error
			committersDone = make(chan struct{})
		)
		go func() {
			defer close(committersDone)
			res, runErr = RunConcurrent(ctx, srv, ConcurrentConfig{
				Seed:              seed + uint64(cycle), // a fresh sub-population per cycle
				Connections:       size.connections,
				OpsPerConn:        size.opsPerConn,
				ContendedCounters: size.counters,
				Mix:               productionProfileMix(),
			})
		}()

		cyc, cpErr := productionProfileCheckpoint(disk, st, cycle, committersDone)

		// Crash protocol (order load-bearing, see runDurableCommitCrash): join
		// the clients, join the server, then crash the disk without a graceful
		// close, so no acknowledged-but-unsynced frame is made durable.
		<-committersDone
		if cpErr != nil {
			_ = srv.Close()
			st.Crash()
			return nil, ev, fmt.Errorf("sim: production profile cycle %d checkpoint: %w", cycle, cpErr)
		}
		if runErr != nil {
			_ = srv.Close()
			st.Crash()
			return nil, ev, fmt.Errorf("sim: production profile cycle %d run: %w", cycle, runErr)
		}

		// In-cycle health: no panics, no transport faults, ledger conserved,
		// during-run oracles silent, quiescence verification clean.
		if res.Panics != 0 || res.TransportErrors != 0 {
			fail("cycle health", "cycle %d: panics=%d transportErrors=%d", cycle, res.Panics, res.TransportErrors)
		}
		if !res.TxConserved() {
			fail("tx conservation", "cycle %d: issued=%d committed=%d conflicted=%d rolledBack=%d failed=%d ambiguous=%d",
				cycle, res.TxIssued, res.TxCommitted, res.TxConflicted, res.TxRolledBack, res.TxFailed, res.TxAmbiguous)
		}
		if res.TxMissingAcked != 0 || res.TxPhantomRefused != 0 {
			fail("tx quiescence", "cycle %d: missingAcked=%d phantomRefused=%d", cycle, res.TxMissingAcked, res.TxPhantomRefused)
		}
		if res.IsoMonotonicViolations != 0 || res.IsoRYOWViolations != 0 || res.IsoBatchViolations != 0 {
			fail("during-run oracle", "cycle %d: monotonic=%d ryow=%d batch=%d",
				cycle, res.IsoMonotonicViolations, res.IsoRYOWViolations, res.IsoBatchViolations)
		}

		// Accumulate the transaction-granular ledgers.
		for _, m := range res.TxMarkersAcked {
			ackedSet[m] = true
			issuedSet[m] = true
		}
		for _, m := range res.TxMarkersRefused {
			refusedSet[m] = true
			issuedSet[m] = true
		}
		for _, n := range res.AckedNames {
			ackedSet[n] = true
			issuedSet[n] = true
		}
		for _, n := range res.IssuedNames {
			issuedSet[n] = true
		}
		for _, n := range res.FailedNames {
			refusedSet[n] = true
		}
		for k, v := range res.ContendedAcked {
			expectedCtr[k] += v
		}
		for k := range expectedCtr {
			if res.ContendedFinal[k] != expectedCtr[k] {
				fail("lost update", "cycle %d: counter %d final=%d, accumulated acked=%d",
					cycle, k, res.ContendedFinal[k], expectedCtr[k])
			}
			issuedSet[wireCounterName(k)] = true
		}

		// The clients are joined; join the server, then crash the disk without a
		// graceful close.
		_ = srv.Close()
		st.Crash()

		// Reopen through real recovery — a published snapshot plus the WAL tail
		// the checkpoint did not fold — and adjudicate at transaction
		// granularity: acked ⊆ recovered ⊆ issued, refused ∩ recovered = ∅.
		st2, err := OpenSimStore(disk, cfg)
		if err != nil {
			return nil, ev, fmt.Errorf("sim: production profile cycle %d recovery: %w", cycle, err)
		}
		// Measured BEFORE any post-recovery commit: the image this recovery read
		// and the clock and sequence it came up on (rmp #2469).
		cyc.recovery, err = measureMVCCRecovery(disk, st2, fmt.Sprintf("cycle %d recovery", cycle))
		if err != nil {
			_ = st2.Close()
			return nil, ev, err
		}
		// The recovered store is quiescent here: the reopen has completed and no
		// client has been pointed at it yet (rmp #2470).
		cyc.substrate.label = fmt.Sprintf("cycle %d recovered-store substrate", cycle)
		cyc.substrate.observe(st2.Graph(), "post-recovery, before any commit", true)
		recovered, partial, err := recoveredPersonNames(ctx, st2.Engine())
		if err != nil {
			_ = st2.Close()
			return nil, ev, fmt.Errorf("sim: production profile cycle %d recovered read: %w", cycle, err)
		}
		// Batch markers carry no age property by design, so the torn-CREATE
		// witness only applies to names the age-bearing templates created.
		_ = partial
		for name := range ackedSet {
			if _, ok := recovered[name]; !ok {
				fail("durability", "cycle %d: acknowledged %q did not survive recovery", cycle, name)
			}
		}
		for name := range refusedSet {
			if _, ok := recovered[name]; ok {
				fail("atomicity", "cycle %d: refused %q present after recovery", cycle, name)
			}
		}
		for name := range recovered {
			if !issuedSet[name] {
				fail("phantom", "cycle %d: recovered %q was never issued", cycle, name)
			}
		}
		// The recovered counters must hold the accumulated acknowledged
		// increments — zero lost updates ACROSS the crash boundary.
		for k := range expectedCtr {
			v, err := engineScalar(ctx, st2, tmplWireCounterRead, map[string]any{"name": wireCounterName(k)})
			if err != nil {
				_ = st2.Close()
				return nil, ev, fmt.Errorf("sim: production profile cycle %d counter read: %w", cycle, err)
			}
			if v != expectedCtr[k] {
				fail("lost update", "cycle %d post-recovery: counter %d recovered=%d, accumulated acked=%d",
					cycle, k, v, expectedCtr[k])
			}
		}

		// Post-recovery commits, then adjudicate the MVCC clock and the
		// transaction sequence against what the image carried (rmp #2469).
		if err := cyc.recovery.observePostRecoveryCommits(ctx, disk, st2, mvccPostRecoveryBeacons); err != nil {
			_ = st2.Close()
			return nil, ev, err
		}
		violations = append(violations, checkMVCCRecovery(int64(cycle), &cyc.recovery)...)
		// The beacons have returned, so the recovered store is quiescent again:
		// in-flight commits must have drained back to zero, the watermark must have
		// followed the commits that just published, and the integrity counters must
		// still be zero (rmp #2470).
		cyc.substrate.observe(st2.Graph(), "post-recovery, after beacon commits", true)
		violations = append(violations, checkMVCCSubstrate(int64(cycle), &cyc.substrate)...)
		ev.cycles = append(ev.cycles, cyc)
		_ = st2.Close()
	}

	// ONE forced pure-snapshot crossing (rmp #2468): a checkpoint that folds
	// every committed op and empties the WAL, a crash, and a reopen that replays
	// nothing — so the clock floor it comes up on can only have been reconciled
	// from the instant the manifest recorded. No crash cycle can reach this
	// state: which crash lands after which checkpoint, and how many WAL bytes it
	// leaves, are properties of the seed.
	if len(violations) == 0 {
		if err := crossProductionProfileSnapshot(ctx, disk, cfg, &violations, int64(size.cycles), &ev); err != nil {
			return nil, ev, err
		}
	}

	// Non-vacuity: the run must have ENTERED the three cases it adjudicates.
	// Each of these would otherwise pass by never happening.
	if len(violations) == 0 {
		var overlapped, tails int
		for i := range ev.cycles {
			c := &ev.cycles[i]
			if c.commitsDuringCheckpoint > 0 {
				overlapped++
			}
			if c.recovery.snapshotPlusWALTail() {
				tails++
			}
		}
		if overlapped == 0 {
			fail("checkpoint overlap", "no cycle published a commit while its checkpoint ran: the checkpoints were"+
				" taken over a quiesced store, so nothing was proven about a concurrent one")
		}
		if tails == 0 {
			fail("snapshot+WAL-tail recovery", "no cycle recovered through a snapshot PLUS a replayed WAL tail:"+
				" every recovery took a single path, so the two were never both exercised")
		}
		if !ev.crossing.pureSnapshot() {
			fail("pure-snapshot recovery", "the forced crossing did not produce a pure-snapshot recovery (%s)",
				ev.crossing.summary())
		}
	}

	if len(violations) > 0 {
		return &SimReport{
			Scenario:   ScenarioProductionProfile,
			Mode:       ModeConcurrent,
			Seed:       seed,
			Violations: violations,
			FailedOp:   Op{Kind: OpMatch, Cypher: "<production profile adjudication>"},
		}, ev, nil
	}
	return nil, ev, nil
}

// productionProfileCheckpoint takes ONE checkpoint while the client population
// is still driving the store, and measures the overlap rather than assuming it.
//
// The wait for durable progress puts the checkpoint inside the traffic instead
// of ahead of it; the MVCC clock either side of the call counts the commits that
// were published while it ran; and the non-blocking read of committersDone
// records that the population had not drained. The WAL lengths either side are
// the prefix the checkpoint removed from the next recovery's reach.
func productionProfileCheckpoint(disk *SimDisk, st *SimStore, cycle int, committersDone <-chan struct{}) (productionProfileCycleEvidence, error) {
	cyc := productionProfileCycleEvidence{cycle: cycle}
	dir := st.Config().dir

	deadline := time.Now().Add(productionProfileWaitBudget)
	waitForSyncProgress(disk, disk.SyncCount()+productionProfileCheckpointLead, deadline)

	var err error
	if cyc.walBeforeCheckpoint, err = simWALSize(disk, dir); err != nil {
		return cyc, fmt.Errorf("WAL size before checkpoint: %w", err)
	}
	clockBefore := st.ClockNow()
	if cpErr := st.Checkpoint(); cpErr != nil {
		return cyc, cpErr
	}
	cyc.commitsDuringCheckpoint = st.ClockNow() - clockBefore
	select {
	case <-committersDone:
	default:
		cyc.clientsRunningAfterCheckpoint = true
	}
	if cyc.walAfterCheckpoint, err = simWALSize(disk, dir); err != nil {
		return cyc, fmt.Errorf("WAL size after checkpoint: %w", err)
	}
	if cyc.snapshotInstant, _, err = simSnapshotInstant(disk, dir); err != nil {
		return cyc, err
	}
	return cyc, nil
}

// crossProductionProfileSnapshot performs the run's single forced crossing of
// the snapshot boundary and adjudicates both halves of it: that the recovery
// really was snapshot-sourced ([checkSnapshotSourcedRecovery], rmp #2468), and
// that the MVCC clock and the transaction sequence it came up on were reconciled
// against the image ([checkMVCCRecovery], rmp #2469).
func crossProductionProfileSnapshot(
	ctx context.Context,
	disk *SimDisk,
	cfg simStoreConfig,
	violations *[]Violation,
	tick int64,
	ev *productionProfileEvidence,
) error {
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return fmt.Errorf("sim: production profile crossing open: %w", err)
	}
	// crossSnapshotBoundaryOn crashes st and returns its replacement.
	st2, boundary, err := crossSnapshotBoundaryOn(disk, st, "production profile pure-snapshot crossing")
	if err != nil {
		return err
	}
	defer func() { _ = st2.Close() }()
	ev.boundary = boundary
	*violations = append(*violations, checkSnapshotSourcedRecovery(tick, boundary)...)

	ev.crossing, err = measureMVCCRecovery(disk, st2, "pure-snapshot crossing")
	if err != nil {
		return err
	}
	if err := ev.crossing.observePostRecoveryCommits(ctx, disk, st2, mvccPostRecoveryBeacons); err != nil {
		return err
	}
	*violations = append(*violations, checkMVCCRecovery(tick, &ev.crossing)...)
	return nil
}

// engineScalar reads one single-scalar value through an engine (autocommit).
func engineScalar(ctx context.Context, st *SimStore, query string, params map[string]any) (int64, error) {
	adapter := NewEngineAdapter(st.Engine())
	res, err := adapter.Run(ctx, query, params)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var v int64
	if res.Next() {
		if n, ok := res.IntAt(0); ok {
			v = n
		}
	}
	return v, res.Err()
}

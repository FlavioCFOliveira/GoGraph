package sim

// mvcc_substrate_arms.go — the workloads that put the MVCC substrate under the
// pressure [checkMVCCSubstrate] adjudicates (rmp #2470).
//
// Three arms, each aimed at a property the telemetry reports and nothing else
// in the simulator reads:
//
//  1. CHURN — repeated writes to a small object set, sized to cross the debt
//     threshold that wakes the vacuum. This is the only arm that can show
//     reclamation happening at all, because below the threshold the vacuum never
//     runs; and it is the only one that can observe a chain-depth distribution,
//     because only a sweep publishes one.
//  2. ABORT-HEAVY — forced serialization conflicts, asserting the refused
//     transactions' versions are WITHDRAWN rather than accumulating.
//  3. CHECKPOINT QUIESCENCE — a checkpoint over live traffic, asserting the
//     commit-quiescence boundary it takes really did drain.
//
// # These arms are NOT byte-for-byte deterministic, and that is stated rather than hidden
//
// Every other scenario in this package is a pure function of its seed. This one
// cannot be: the vacuum is a BACKGROUND goroutine ([lpg.Graph.vacuumLoop]), so
// how many passes have run and how many records are live at the instant a
// sample is taken depend on the Go scheduler. The committed DATA is still a
// pure function of the seed, and the arms below assert it.
//
// The oracle is built to survive that. Every clause in [checkMVCCSubstrate] is
// a BOUND, a MONOTONICITY, or a MUST-BE-ZERO — none of them an exact value — so
// the adjudication is scheduling-independent even though the readings are not.
// A run that asserted `passes == 17` would be flaky; one that asserts the
// vacuum released SOMETHING once churn had woken it is not.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// Substrate-arm workload templates. The churned objects are Person nodes with a
// single integer property, so each write versions one property on one object
// and the version count is a direct function of the round count.
const (
	tmplSubstrateSeed  = "CREATE (n:Person {name:$name, val:$v})"
	tmplSubstrateChurn = "MATCH (n:Person {name:$name}) SET n.val=$v"
)

// substrateObjectName names the i'th churned object.
func substrateObjectName(i int) string { return fmt.Sprintf("mvcc-substrate-%d", i) }

// MVCCSubstrateConfig parameterises a substrate-telemetry run.
type MVCCSubstrateConfig struct {
	// Seed is the master seed; the committed data is a pure function of it.
	Seed uint64
	// Objects is how many distinct nodes the churn spreads over (< 1 normalises
	// to 8). Fewer objects concentrate the versions onto deeper chains.
	Objects int
	// Rounds is how many committed writes the churn performs (< 1 normalises to
	// mvccSubstrateShortRounds). It must be large enough to carry the
	// reclamation debt past the vacuum's wake threshold, or the run proves
	// nothing about reclamation — which [checkMVCCSubstrateNonVacuity] enforces
	// rather than assumes.
	Rounds int
	// SampleEvery is how often, in rounds, the substrate is sampled (< 1
	// normalises to mvccSubstrateSampleEvery). Sampling DURING churn is what
	// catches a chain-depth distribution at all: the histogram describes the
	// last complete sweep and reads empty once everything has been released.
	SampleEvery int
	// Checkpoints, when positive, publishes that many checkpoints spread through
	// the churn, exercising the commit-quiescence boundary under live traffic.
	// Requires a full-stack store, which this scenario always opens.
	Checkpoints int
}

// mvccSubstrate defaults. The round count is set from measurement: the vacuum's
// wake threshold on this configuration is 4096 records, one write versions one
// property record, and the short arm clears it with margin while staying well
// inside the short layer's budget.
const (
	mvccSubstrateShortRounds = 6000
	mvccSubstrateSampleEvery = 50
	mvccSubstrateObjects     = 8
)

func (c *MVCCSubstrateConfig) normalise() {
	if c.Objects < 1 {
		c.Objects = mvccSubstrateObjects
	}
	if c.Rounds < 1 {
		c.Rounds = mvccSubstrateShortRounds
	}
	if c.SampleEvery < 1 {
		c.SampleEvery = mvccSubstrateSampleEvery
	}
}

// MVCCSubstrateResult summarises a substrate-telemetry run.
//
// The counts are reported so a caller can assert the run was not vacuous
// without re-deriving them, and so a green run logs what it actually measured
// rather than merely that it passed.
type MVCCSubstrateResult struct {
	// Violations holds the adjudication findings; empty on a clean run.
	Violations []Violation
	// Summary is the folded evidence rendered for a log line.
	Summary string
	// Commits, Aborts and Conflicts are the write outcomes over the run.
	Commits, Aborts, Conflicts uint64
	// MaxVersionRecords is the high-water mark of live version records, against
	// the two bounds the substrate publishes for it.
	MaxVersionRecords int64
	Bound, Ceiling    int64
	// CrossedBound records that the run put the substrate under reclamation
	// pressure: a reading at or above the vacuum's wake threshold, or a sweep
	// having run. See [mvccSubstrateEvidence.reclamationPressured] for why the
	// first witness alone aliases.
	CrossedBound bool
	// WatermarkFrom and WatermarkTo bracket the reclamation watermark's travel.
	WatermarkFrom, WatermarkTo uint64
	// Sweeps and ReclaimedRecords are the vacuum's progress.
	Sweeps           uint64
	ReclaimedRecords int64
	// DeepestChain is the greatest retained chain depth any sweep reported, and
	// ChainSamples how many readings carried a non-empty distribution.
	DeepestChain uint64
	ChainSamples int
	// DeepestUnpinnedChain is the greatest retained depth observed with NO
	// snapshot registered, over UnpinnedChainSamples such readings. It, and not
	// DeepestChain, is what the depth bound is adjudicated on.
	DeepestUnpinnedChain uint64
	UnpinnedChainSamples int
	// PeakActiveSnapshots is the most snapshots registered at any reading.
	PeakActiveSnapshots int
	// CheckpointsRun is how many checkpoints the run published, each of which
	// took the commit-quiescence boundary.
	CheckpointsRun int
}

// Clean reports whether the run finished with no violations.
func (r *MVCCSubstrateResult) Clean() bool { return len(r.Violations) == 0 }

// RunMVCCSubstrateChurn drives version churn past the vacuum's wake threshold
// and adjudicates the MVCC substrate's own telemetry against it.
//
// It samples throughout the churn rather than only at the end, because two of
// the four properties are not observable at rest: the chain-depth histogram is
// published by a sweep and reset by the next one, and the vacuum exits once it
// has nothing left to free. It finishes at a genuine quiescent point — every
// transaction committed, nothing in flight — which is the only point at which
// in-flight commits returning to zero is a fair question.
//
// # Concurrency contract
//
// Runs entirely on the calling goroutine. The vacuum sweeps on its own
// goroutine, which is what makes the telemetry scheduling-dependent; see the
// file comment for why the oracle is nevertheless stable.
func RunMVCCSubstrateChurn(ctx context.Context, cfg MVCCSubstrateConfig) (*MVCCSubstrateResult, error) {
	cfg.normalise()

	disk := NewSimDisk(NewSeed(cfg.Seed^diskSeedMix), 0)
	st, err := OpenSimStore(disk, fullStackStoreConfig())
	if err != nil {
		return nil, fmt.Errorf("sim: open substrate-churn store: %w", err)
	}
	defer func() { _ = st.Close() }()

	ev := newMVCCSubstrateEvidence(fmt.Sprintf("MVCC substrate churn (seed %d, %d objects, %d rounds)",
		cfg.Seed, cfg.Objects, cfg.Rounds))
	res := &MVCCSubstrateResult{}

	sess := st.Engine().NewSession()
	// A fresh store has committed nothing; the baseline is taken before the seed
	// writes so the run's commit and watermark deltas start from a true zero.
	ev.observe(st.Graph(), "baseline (empty store)", true)

	for i := 0; i < cfg.Objects; i++ {
		if err := substrateExec(ctx, sess, tmplSubstrateSeed,
			map[string]any{"name": substrateObjectName(i), "v": int64(0)}); err != nil {
			return nil, fmt.Errorf("sim: seed substrate object %d: %w", i, err)
		}
	}
	ev.observe(st.Graph(), "after seed", true)

	// Checkpoints are spread through the churn rather than bunched, so at least
	// one lands with the vacuum awake and versions live.
	var nextCheckpoint, checkpointStride int
	if cfg.Checkpoints > 0 {
		checkpointStride = cfg.Rounds / (cfg.Checkpoints + 1)
		if checkpointStride < 1 {
			checkpointStride = 1
		}
		nextCheckpoint = checkpointStride
	}

	for r := 0; r < cfg.Rounds; r++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := substrateObjectName(r % cfg.Objects)
		if err := substrateExec(ctx, sess, tmplSubstrateChurn,
			map[string]any{"name": name, "v": int64(r)}); err != nil {
			return nil, fmt.Errorf("sim: churn round %d: %w", r, err)
		}
		// Sampled mid-churn, which is the only window in which a sweep's
		// chain-depth distribution is still published.
		if (r+1)%cfg.SampleEvery == 0 {
			ev.observe(st.Graph(), fmt.Sprintf("churn round %d", r+1), false)
		}
		if cfg.Checkpoints > 0 && res.CheckpointsRun < cfg.Checkpoints && r+1 >= nextCheckpoint {
			if err := observeCheckpointQuiescence(st, ev, fmt.Sprintf("checkpoint %d at round %d",
				res.CheckpointsRun+1, r+1)); err != nil {
				return nil, err
			}
			res.CheckpointsRun++
			nextCheckpoint += checkpointStride
		}
	}

	// The terminal quiescent point: this goroutine is the only writer and it has
	// returned from its last commit, so nothing is in flight by construction.
	ev.observe(st.Graph(), "terminal quiescence", true)

	res.Violations = append(res.Violations, checkMVCCSubstrate(int64(cfg.Rounds), ev)...)
	res.Violations = append(res.Violations, checkMVCCSubstrateNonVacuity(int64(cfg.Rounds), ev)...)
	fillMVCCSubstrateResult(res, ev)
	return res, nil
}

// fillMVCCSubstrateResult copies the folded measurements out for the caller.
func fillMVCCSubstrateResult(res *MVCCSubstrateResult, ev *mvccSubstrateEvidence) {
	res.Summary = ev.summary()
	res.Commits, res.Aborts, res.Conflicts = ev.commits(), ev.aborts(), ev.conflicts()
	res.MaxVersionRecords = ev.maxTotal
	res.Bound, res.Ceiling = ev.last.bound, ev.last.ceiling
	res.CrossedBound = ev.reclamationPressured()
	res.WatermarkFrom, res.WatermarkTo = ev.first.watermark, ev.last.watermark
	res.Sweeps, res.ReclaimedRecords = ev.sweeps(), ev.reclaimedRecords()
	res.DeepestChain, res.ChainSamples = ev.deepest, ev.chainSamples
	res.DeepestUnpinnedChain, res.UnpinnedChainSamples = ev.deepestUnpinned, ev.unpinnedChainSamples
	res.PeakActiveSnapshots = ev.maxActiveSnapshots
}

// substrateExec runs one statement in its own committed transaction.
func substrateExec(ctx context.Context, sess *cypher.Session, q string, params map[string]any) error {
	tx, err := sess.BeginTx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecAny(q, params); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// observeCheckpointQuiescence publishes a checkpoint and brackets it with a
// substrate reading on each side (rmp #2470, acceptance criterion 3).
//
// # The contract this pins, read from the checkpointer rather than assumed
//
// A non-blocking checkpoint's phase 1 runs under the store's commit serialiser
// with writer admission CLOSED, and the first thing it does there is
// `Checkpointer.awaitCommitQuiescence`, which blocks until no transaction sits
// between its WAL fsync and its MVCC publish. That is what makes the durable
// WAL offset and the visible MVCC instant it takes next describe the SAME set
// of transactions (rmp #2349). So the reading taken immediately after the
// checkpoint returns must show no commit allocated and unpublished: the
// boundary either drained or the checkpoint failed.
//
// # The timeout fail-stop, and why it is pinned by contract rather than provoked
//
// `commitQuiesceTimeout` is 30 seconds and is a FAIL-STOP, not a schedule: it
// bounds a wait for transactions that are already past their fsync and owe only
// in-memory work, with admission closed so none can be added. Reaching it means
// an instant was allocated and never discharged — a PERMANENT frontier stall.
// Provoking it deterministically would require holding a transaction between
// those two points, and the only seam that could do so lives in `graph/lpg`,
// outside this package's remit. What IS asserted here is the observable the
// timeout exists to report: [lpg.MVCCStats.InFlightCommits] at the boundary. A
// non-zero reading after a returned checkpoint is the same defect the timeout
// would eventually catch, caught earlier and without a 30-second wait.
func observeCheckpointQuiescence(st *SimStore, ev *mvccSubstrateEvidence, label string) error {
	ev.observe(st.Graph(), label+" (before)", false)
	if err := st.Checkpoint(); err != nil {
		return fmt.Errorf("sim: substrate arm %s: %w", label, err)
	}
	// Quiescent by the checkpointer's own contract: phase 1 drained the commit
	// path under closed admission, and this goroutine is the only writer.
	ev.observe(st.Graph(), label+" (after)", true)
	return nil
}

// RunMVCCSubstrateAborts is the ABORT-HEAVY arm: it drives the contended
// lost-update scenario, which produces typed serialization conflicts, and reads
// the substrate at the drain point to assert the refused transactions' versions
// were WITHDRAWN rather than left to accumulate (rmp #2470, criterion 2).
//
// # Why conflicts and not rollbacks
//
// [mvcc.WriteCounts.Aborts] counts transactions the SUBSTRATE refused, not
// transactions a client rolled back. GoGraph's explicit rollback is served by
// the statement undo log, so a voluntary rollback publishes its inverses and is
// counted as a commit — measured, 50 rollbacks produced `commits +49, aborts 0`.
// Only a serialization conflict reaches the aborted-version path, which is why
// this arm drives contention rather than rollback.
//
// # What "withdrawn" means here
//
// Withdrawal is SYNCHRONOUS: [lpg.Graph.abortWake] calls `withdrawAbortedNow`
// before returning, because a present-time read takes the stored value directly
// and the aborted transaction's writes must be out of it by then. So the
// assertion is not "the vacuum eventually freed them" but the stronger "they
// are not in the live count at the drain point at all" — which is why this arm
// can adjudicate without waiting on a background sweep, and why it does not
// need to cross the vacuum's wake threshold to be meaningful.
func RunMVCCSubstrateAborts(ctx context.Context, cfg MVCCContentionConfig) (*MVCCSubstrateResult, *MVCCContentionResult, error) {
	ev := newMVCCSubstrateEvidence(fmt.Sprintf("MVCC substrate abort arm (seed %d, %d sessions, %d counters)",
		cfg.Seed, cfg.Sessions, cfg.Counters))
	res := &MVCCSubstrateResult{}

	// The contention scenario owns its store; OnQuiesce is the hook that lets
	// the substrate be read at the drain point, after every open transaction has
	// been rolled back and before the store is closed.
	cfg.OnQuiesce = func(st *SimStore) {
		ev.observe(st.Graph(), "contention drain point", true)
	}
	// A baseline cannot be taken before the store exists, so the run's own
	// counters are the baseline: they start at zero on a fresh store.
	cont, err := RunMVCCContention(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if !ev.measured {
		return nil, nil, errors.New("sim: the contention quiesce hook never fired, so the substrate was" +
			" never read at the drain point")
	}

	res.Violations = append(res.Violations, checkMVCCSubstrate(int64(cfg.Ticks), ev)...)
	res.Violations = append(res.Violations, checkMVCCAbortWithdrawal(int64(cfg.Ticks), ev, cont)...)
	fillMVCCSubstrateResult(res, ev)
	return res, cont, nil
}

// checkMVCCAbortWithdrawal adjudicates the abort-heavy arm.
//
// Every clause is a way an abort could look handled and not be:
//
//   - the arm produced no conflict at all, so "aborted versions are withdrawn"
//     was answered about zero aborted versions — the vacuous shape this sprint
//     has already found six of;
//   - the substrate's abort counter disagrees with the client-visible refusals,
//     so one of the two is not counting what it claims to;
//   - live version records at the drain point are consistent with the refused
//     transactions' versions still being held, which is the leak
//     `mvcc_abort_reclaim` exists to close and which no isolation OUTCOME check
//     would notice.
func checkMVCCAbortWithdrawal(tick int64, e *mvccSubstrateEvidence, cont *MVCCContentionResult) []Violation {
	const op = "MVCC aborted-version withdrawal"
	var out []Violation
	fail := func(kind ViolationKind, format string, args ...any) {
		out = append(out, Violation{
			Kind: kind, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", e.label) + fmt.Sprintf(format, args...),
		})
	}

	if !e.measured {
		fail(ViolationACIDAtomicity, "the substrate was never read at the drain point")
		return out
	}
	// Non-vacuity FIRST: a bound on aborted versions is trivially satisfied by a
	// run that aborted nothing.
	if cont.TxConflicted == 0 {
		fail(ViolationOracleDeviation, "the contention arm produced NO serialization conflict (%d committed),"+
			" so nothing was aborted and the withdrawal clause below adjudicated an empty set",
			cont.TxCommitted)
		return out
	}
	aborts := e.last.write.Aborts
	if aborts == 0 {
		fail(ViolationACIDAtomicity, "the client observed %d typed serialization refusal(s) but the substrate"+
			" counted ZERO aborts: a refusal the substrate does not count is one an operator cannot see",
			cont.TxConflicted)
		return out
	}
	if conflicts := e.last.write.Conflicts; conflicts == 0 {
		fail(ViolationOracleDeviation, "the substrate counted %d abort(s) but attributed NONE of them to a"+
			" write-write conflict, so the refusals cannot be shown to be the contention this arm drove",
			aborts)
	}

	// The withdrawal itself. Each refused transaction wrote the contended
	// counter and its own control key before being refused, so had its versions
	// been retained the live count would carry at least one record per abort on
	// top of the committed working set. The committed set is bounded by the
	// objects the scenario creates, which is small and known.
	committed := int64(cont.Counters + cont.Sessions)
	if e.last.total > committed+int64(aborts) {
		fail(ViolationACIDAtomicity, "live version records at the drain point are %d, more than the committed"+
			" working set (%d objects) plus one record per abort (%d): the refused transactions' versions look"+
			" RETAINED rather than withdrawn, which both leaks memory and leaves a stored value no reader may"+
			" take at face value",
			e.last.total, committed, aborts)
	}
	return out
}

package sim

// codec_matrix_run.go — the two scenarios every codec arm runs, and the
// ErrNoWeightCodec negative probe (rmp #2473). See codec_matrix.go for what the
// matrix is and why the Cypher-driven oracles cannot be reused for it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The two scenario names, used in evidence and in violation messages.
const (
	codecScenarioCrashStorm = "crash-storm"
	codecScenarioUpgrade    = "upgrade"
)

// -----------------------------------------------------------------------------
// Scenario 1 — crash DURING the snapshot publish, once per codec pair
// -----------------------------------------------------------------------------

// codecCrashCycle is what ONE crash-storm cycle MEASURED. It mirrors
// [checkpointStormCycle], minus the concurrency fields: a typed store is
// single-writer (see [simTypedStore]'s concurrency contract), so there is no
// raced-publish evidence to record and none is claimed.
type codecCrashCycle struct {
	// cpErr is the error the interrupted checkpoint returned. A nil means the
	// publish completed and the window was never entered.
	cpErr error
	// window is the point in the publish protocol this cycle interrupted.
	window publishWindow
	// renameFaults / renameWritebacks are the per-cycle deltas of the armed
	// SimDisk rename primitives' fire counts — the evidence that the arm matched
	// a real rename instead of being silently ignored.
	renameFaults     int64
	renameWritebacks int64
	// liveBefore / bakBefore are the durable image the crash left, read BEFORE
	// recovery touched it; liveAfter / bakAfter are the same names after the
	// reopen. Together they identify which recovery branch ran.
	liveBefore, bakBefore bool
	liveAfter, bakAfter   bool
}

// promoted reports whether this cycle's reopen ran recovery's interrupted-
// publish repair: the live snapshot absent and a backup stranded before, the
// live snapshot present and the backup gone after. No other recovery path
// produces that transition.
func (c codecCrashCycle) promoted() bool {
	return !c.liveBefore && c.bakBefore && c.liveAfter && !c.bakAfter
}

// runCrashStorm drives the three snapshot-publish crash windows over this
// arm's codec pair, accumulating one durability ledger across the cycles
// because every cycle reopens the SAME durable image.
//
// It reuses the window machinery of checkpoint_crash_storm.go verbatim
// ([checkpointStormWindows], [publishWindow.arm]): those arms are pure SimDisk
// primitives keyed by path, so they are codec-agnostic in fact and not merely
// by assumption.
func (a codecArmOf[N, W]) runCrashStorm(
	ctx context.Context, seed uint64, size codecMatrixSize,
) (codecArmEvidence, []Violation, error) {
	ev := codecArmEvidence{arm: a.label, scenario: codecScenarioCrashStorm}
	disk := NewSimDisk(NewSeed(seed^codecMatrixDiskSeedMix), 0)
	cfg := fullStackStoreConfig()
	snapDir := cfg.dir + "/" + simSnapshotName
	led := newCodecLedger()
	ords := &codecOrdinals{}

	var v []Violation
	for i, window := range checkpointStormWindows() {
		if err := ctx.Err(); err != nil {
			return ev, nil, err
		}
		cyc, cycV, err := a.crashStormCycle(ctx, disk, cfg, snapDir, led, ords, &ev, size, window)
		if err != nil {
			return ev, nil, fmt.Errorf("[%s] cycle %d (%s): %w", a.label, i, window, err)
		}
		if cyc.cpErr != nil {
			ev.windowsEntered++
		}
		if cyc.promoted() {
			ev.promotes++
		}
		v = append(v, cycV...)
		// The window is a precondition of the cycle, not a verdict on the
		// engine: a checkpoint that SUCCEEDED means the crash did not land
		// mid-publish, so the cycle exercised less than it planned. That is a
		// non-vacuity fact, recorded above via windowsEntered and adjudicated by
		// checkCodecCrashStormShape.
		if len(v) > 0 {
			return ev, v, nil
		}
	}
	ev.ran = true
	return ev, v, nil
}

// crashStormCycle drives one open → write → clean checkpoint → write →
// interrupted checkpoint → crash → reopen → adjudicate cycle over the shared
// durable image. It owns and tears down every resource it creates.
func (a codecArmOf[N, W]) crashStormCycle(
	ctx context.Context, disk *SimDisk, cfg simStoreConfig, snapDir string,
	led *codecLedger, ords *codecOrdinals, ev *codecArmEvidence,
	size codecMatrixSize, window publishWindow,
) (codecCrashCycle, []Violation, error) {
	cyc := codecCrashCycle{window: window}

	st, err := openSimTypedStore(disk, cfg, a.codec, a.wcodec)
	if err != nil {
		return cyc, nil, fmt.Errorf("open store: %w", err)
	}

	// A CLEAN checkpoint first: the interrupted publish below can only ARCHIVE a
	// live snapshot that already exists, so without this the stranded-backup
	// window is unreachable. It also truncates the WAL prefix, which is what
	// makes a lost snapshot a genuine loss of acknowledged commits.
	if err := a.writePhase(ctx, st, led, ords, size); err != nil {
		st.Crash()
		return cyc, nil, fmt.Errorf("write phase: %w", err)
	}
	if err := st.Checkpoint(); err != nil {
		st.Crash()
		return cyc, nil, fmt.Errorf("clean checkpoint: %w", err)
	}
	// Read the mapper layout out of the durable image the CLEAN publish left,
	// before the interrupted one can disturb it.
	format, mbytes, err := readSimMapperFormat(disk, snapDir)
	if err != nil {
		st.Crash()
		return cyc, nil, fmt.Errorf("read mapper header: %w", err)
	}
	if format != 0 {
		ev.mapperFormat, ev.mapperBytes = format, mbytes
	}

	// More commits, so the interrupted checkpoint has a fresh WAL prefix to fold
	// and the crash has acknowledged state to lose if the publish is mishandled.
	if err := a.writePhase(ctx, st, led, ords, size); err != nil {
		st.Crash()
		return cyc, nil, fmt.Errorf("second write phase: %w", err)
	}

	// The INTERRUPTED checkpoint.
	faultsBefore, writebacksBefore := disk.RenameFaultCount(), disk.RenameWritebackCount()
	window.arm(disk, snapDir)
	cyc.cpErr = st.Checkpoint()
	cyc.renameFaults = disk.RenameFaultCount() - faultsBefore
	cyc.renameWritebacks = disk.RenameWritebackCount() - writebacksBefore

	// Crash: no graceful close, so no acknowledged-but-unsynced frame is made
	// durable and every not-yet-fsync'd dirent is revoked.
	st.Crash()

	// The durable image the crash left, read BEFORE recovery touches it.
	cyc.liveBefore = disk.Exists(snapDir + "/manifest.json")
	cyc.bakBefore = disk.Exists(snapDir + ".bak/manifest.json")

	st2, err := openSimTypedStore(disk, cfg, a.codec, a.wcodec)
	if err != nil {
		return cyc, nil, fmt.Errorf("reopen after crash: %w", err)
	}
	defer func() { _ = st2.Close() }()
	cyc.liveAfter = disk.Exists(snapDir + "/manifest.json")
	cyc.bakAfter = disk.Exists(snapDir + ".bak/manifest.json")

	if !st2.clean {
		return cyc, []Violation{{
			Kind: ViolationACIDDurability, Op: "<codec-corrupt-reopen>",
			Message: fmt.Sprintf("[%s] the reopen after the %s publish window reported corruption",
				a.label, window),
		}}, nil
	}
	// A crash-storm reopen loads the snapshot AND replays the WAL tail on top,
	// so an individual edge's provenance is not determined: codecPhaseMixed.
	return cyc, a.checkRecovered(int64(window), codecPhaseMixed, st2.graph, led, ev), nil
}

// checkCodecCrashStormShape is the crash-storm half of the NON-VACUITY gate. It
// asserts only that the run reached the machinery it set out to exercise —
// every planned publish window entered, and recovery's promote repair driven at
// least once — never that the engine behaved correctly, which the verdict gate
// owns.
func checkCodecCrashStormShape(ev []codecArmEvidence) []Violation {
	var v []Violation
	want := len(checkpointStormWindows())
	for i := range ev {
		e := &ev[i]
		if e.scenario != codecScenarioCrashStorm || !e.ran {
			continue
		}
		if e.windowsEntered != want {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<window-not-entered>",
				Message: fmt.Sprintf(
					"arm %s entered %d of %d publish windows: an armed fault that never fired leaves"+
						" the cycle adjudicating a crash it did not land", e.arm, e.windowsEntered, want),
			})
		}
		if e.promotes == 0 {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<promote-unexercised>",
				Message: fmt.Sprintf(
					"arm %s never drove recovery's snapshot-promote repair, so the interrupted-publish"+
						" path was not reached on this codec pair", e.arm),
			})
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// Scenario 2 — the upgrade boundary, then the snapshot boundary
// -----------------------------------------------------------------------------

// runUpgrade crosses TWO boundaries on one durable image, per codec pair:
//
//  1. the UPGRADE boundary — write, close gracefully so every acknowledged
//     commit is durable, then reopen the same image through real recovery. No
//     snapshot exists yet, so this half runs the WAL-only core
//     ([recovery.ReplayWAL]) and adjudicates the codec on the WAL op log alone.
//  2. the SNAPSHOT boundary — write more, publish ONE checkpoint that folds
//     every committed op, MEASURE the WAL going to zero, crash, and reopen.
//     With an emptied WAL there is nothing left to replay, so every key that
//     comes back can only have come through mapper.bin — which for a non-string
//     arm is the version-2 byte-mapper, read by ReadMapperBytes and decoded by
//     [snapshot.ApplyMapperToGraphWithCodec].
//
// The second crossing is adjudicated by [checkSnapshotSourcedRecovery], the
// same adjudicator the string-keyed snapshot-codec scenario uses, so the claim
// "this came from the snapshot" rests on measurements rather than on intent.
func (a codecArmOf[N, W]) runUpgrade(
	ctx context.Context, seed uint64, size codecMatrixSize,
) (codecArmEvidence, []Violation, error) {
	ev := codecArmEvidence{arm: a.label, scenario: codecScenarioUpgrade}
	disk := NewSimDisk(NewSeed(seed^codecMatrixDiskSeedMix), 0)
	cfg := fullStackStoreConfig()
	snapDir := cfg.dir + "/" + simSnapshotName
	led := newCodecLedger()
	ords := &codecOrdinals{}

	// --- Boundary 1: write, graceful close, reopen. ---
	st, err := openSimTypedStore(disk, cfg, a.codec, a.wcodec)
	if err != nil {
		return ev, nil, fmt.Errorf("[%s] open store: %w", a.label, err)
	}
	if err := a.writePhase(ctx, st, led, ords, size); err != nil {
		_ = st.Close()
		return ev, nil, fmt.Errorf("[%s] write phase: %w", a.label, err)
	}
	if err := st.Close(); err != nil {
		return ev, nil, fmt.Errorf("[%s] close before upgrade boundary: %w", a.label, err)
	}

	reopened, err := openSimTypedStore(disk, cfg, a.codec, a.wcodec)
	if err != nil {
		return ev, nil, fmt.Errorf("[%s] reopen across the upgrade boundary: %w", a.label, err)
	}
	if !reopened.clean {
		_ = reopened.Close()
		return ev, []Violation{{
			Kind: ViolationACIDDurability, Op: "<codec-upgrade-corrupt>",
			Message: fmt.Sprintf("[%s] a cleanly closed image reported corruption on reopen", a.label),
		}}, nil
	}
	// No snapshot exists yet, so this reopen ran the WAL-only recovery core and
	// every weight came back through the arm's own txn.WeightCodec.
	if v := a.checkRecovered(0, codecPhaseWAL, reopened.graph, led, &ev); len(v) > 0 {
		_ = reopened.Close()
		return ev, v, nil
	}

	// --- Boundary 2: more writes, one folding checkpoint, crash, reopen. ---
	if err := a.writePhase(ctx, reopened, led, ords, size); err != nil {
		_ = reopened.Close()
		return ev, nil, fmt.Errorf("[%s] post-upgrade write phase: %w", a.label, err)
	}

	boundary, err := a.crossCodecSnapshotBoundary(disk, cfg, snapDir, reopened, &ev)
	if err != nil {
		return ev, nil, fmt.Errorf("[%s] snapshot boundary: %w", a.label, err)
	}
	ev.boundary = boundary

	final, err := openSimTypedStore(disk, cfg, a.codec, a.wcodec)
	if err != nil {
		return ev, nil, fmt.Errorf("[%s] reopen after the snapshot boundary: %w", a.label, err)
	}
	defer func() { _ = final.Close() }()
	ev.boundary.walOpsReplayed = final.schema.walOps

	if !final.clean {
		return ev, []Violation{{
			Kind: ViolationACIDDurability, Op: "<codec-snapshot-corrupt>",
			Message: fmt.Sprintf("[%s] the reopen after the snapshot boundary reported corruption", a.label),
		}}, nil
	}
	// The snapshot-sourced verdict runs FIRST: without it, everything the
	// read-back below finds could have been served by a WAL replay, and the
	// mapper would not have been proven to carry anything at all.
	if v := checkSnapshotSourcedRecovery(1, ev.boundary); len(v) > 0 {
		return ev, v, nil
	}
	// checkSnapshotSourcedRecovery above has just PROVEN the WAL was emptied and
	// replayed zero ops, which is what entitles this call to claim the stricter
	// snapshot-only phase rather than assuming it.
	v := a.checkRecovered(1, codecPhaseSnapshotOnly, final.graph, led, &ev)
	ev.ran = true
	return ev, v, nil
}

// crossCodecSnapshotBoundary publishes ONE checkpoint that folds every
// committed op, measures the durable WAL image on both sides of it, records the
// mapper layout the publish wrote, and then crashes without a graceful close.
// It is the codec-generic sibling of [crossSnapshotBoundaryOn], which is bound
// to the string-keyed [SimStore].
func (a codecArmOf[N, W]) crossCodecSnapshotBoundary(
	disk *SimDisk, cfg simStoreConfig, snapDir string, st *simTypedStore[N, W], ev *codecArmEvidence,
) (snapshotBoundary, error) {
	b := snapshotBoundary{label: fmt.Sprintf("%s codec snapshot boundary", a.label)}

	before, err := simWALSize(disk, cfg.dir)
	if err != nil {
		st.Crash()
		return b, fmt.Errorf("measure WAL before: %w", err)
	}
	b.walBefore = before

	if err := st.Checkpoint(); err != nil {
		st.Crash()
		return b, fmt.Errorf("folding checkpoint: %w", err)
	}

	after, err := simWALSize(disk, cfg.dir)
	if err != nil {
		st.Crash()
		return b, fmt.Errorf("measure WAL after: %w", err)
	}
	b.walAfter = after
	b.snapshotPublished = disk.Exists(snapDir + "/manifest.json")

	format, mbytes, err := readSimMapperFormat(disk, snapDir)
	if err != nil {
		st.Crash()
		return b, fmt.Errorf("read mapper header: %w", err)
	}
	if format != 0 {
		ev.mapperFormat, ev.mapperBytes = format, mbytes
	}

	// Crash the HOST ([SimStore.Crash] is [SimDisk.CrashHost]): the reopen that
	// follows must reconstruct everything from the snapshot, because the WAL
	// prefix it would otherwise have replayed is gone.
	st.Crash()
	b.crossed = true
	return b, nil
}

// -----------------------------------------------------------------------------
// The whole matrix
// -----------------------------------------------------------------------------

// codecMatrixResult is one full sweep of the matrix: the per-arm evidence, the
// VERDICT violations, and — kept SEPARATE, never merged — the non-vacuity
// violations.
//
// The separation is the contract. Verdict violations say the engine is wrong;
// vacuity violations say the run did not exercise enough to have found out.
// Merging them is how a suite ends up with guards that report an uninformative
// run as a defect, and a defect as an uninformative run.
type codecMatrixResult struct {
	evidence []codecArmEvidence
	verdict  []Violation
	vacuity  []Violation
}

// runCodecMatrix runs every arm through both scenarios at the given size.
//
// It returns an error only for a harness fault. An engine defect lands in
// [codecMatrixResult.verdict]; a run that exercised too little lands in
// [codecMatrixResult.vacuity].
func runCodecMatrix(ctx context.Context, seed uint64, size codecMatrixSize) (codecMatrixResult, error) {
	arms := codecMatrixArms()
	var out codecMatrixResult

	for i, arm := range arms {
		armSeed := seed + uint64(i)*0x1001
		for _, run := range []struct {
			name string
			fn   func(context.Context, uint64, codecMatrixSize) (codecArmEvidence, []Violation, error)
		}{
			{codecScenarioCrashStorm, arm.runCrashStorm},
			{codecScenarioUpgrade, arm.runUpgrade},
		} {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			ev, v, err := run.fn(ctx, armSeed, size)
			if err != nil {
				return out, fmt.Errorf("codec matrix: arm %s, scenario %s: %w", arm.name(), run.name, err)
			}
			out.evidence = append(out.evidence, ev)
			out.verdict = append(out.verdict, v...)
			out.verdict = append(out.verdict, checkCodecMapperFormat(int64(i), arm, &ev)...)
		}
	}

	// Vacuity is adjudicated ONLY for a run that otherwise passed. An arm that
	// hit a verdict violation stops early by design, so its window count and its
	// recovered-node count are consequences of the defect, not independent
	// evidence about how much the run exercised. Asking the shape question of a
	// failed run would report the same defect twice — once as a defect and once
	// as an uninformative run — which is exactly the conflation the two gates
	// exist to keep apart.
	if len(out.verdict) > 0 {
		return out, nil
	}
	out.vacuity = append(out.vacuity, checkCodecMatrixNonVacuity(arms, out.evidence)...)
	out.vacuity = append(out.vacuity, checkCodecCrashStormShape(out.evidence)...)
	return out, nil
}

// -----------------------------------------------------------------------------
// Negative probe — ErrNoWeightCodec
// -----------------------------------------------------------------------------

// codecWeightCodecProbe is what the [txn.ErrNoWeightCodec] probe MEASURED
// against a store built WITHOUT a weight codec. Each field pins one branch of
// the engine's actual behaviour rather than an expectation of it.
type codecWeightCodecProbe struct {
	// nonZeroErr is what Tx.AddEdge returned for a NON-ZERO weight.
	nonZeroErr error
	// zeroErr is what Tx.AddEdge returned for a ZERO weight. The engine accepts
	// this and buffers an unweighted record, so a non-nil here is a change of
	// contract.
	zeroErr error
	// withHandleErr is what Tx.AddEdgeWithHandle returned for a ZERO weight.
	// That path requires a weight codec UNCONDITIONALLY, unlike AddEdge, and the
	// asymmetry is the reason this field exists separately.
	withHandleErr error
	// zeroEdgeRecovered reports whether the zero-weight edge — the one call the
	// engine accepted — survived a crash and came back on reopen.
	zeroEdgeRecovered bool
	// committed reports whether the transaction carrying the accepted
	// zero-weight edge committed cleanly.
	committed bool
}

// probeNoWeightCodec drives [txn.ErrNoWeightCodec] deliberately and pins what
// the engine ACTUALLY does, rather than asserting what it ought to do.
//
// The store is built by hand — [txn.NewStoreWithCodec], which wires a key codec
// and NO weight codec — because [openSimTypedStore] always supplies both and so
// can never reach this branch. Everything else is the real stack: a real WAL on
// a SimDisk, a real crash, a real reopen.
//
// Three behaviours are measured, and the third is the one worth having:
//
//   - AddEdge with a NON-ZERO weight returns ErrNoWeightCodec;
//   - AddEdge with a ZERO weight is ACCEPTED, buffering an unweighted record;
//   - AddEdgeWithHandle returns ErrNoWeightCodec even for a ZERO weight, because
//     that entry point requires a codec unconditionally.
//
// The accepted zero-weight edge is then carried across a crash, so the probe
// also establishes that the weight-codec-less path is durable and not merely
// non-erroring.
func probeNoWeightCodec(ctx context.Context, seed uint64) (codecWeightCodecProbe, error) {
	var p codecWeightCodecProbe
	disk := NewSimDisk(NewSeed(seed^codecMatrixDiskSeedMix), 0)
	cfg := simulatorStoreConfig()

	g, _, _, err := recoverSimGraph(disk, cfg, txn.NewInt64Codec(), txn.WeightCodec[int64](nil))
	if err != nil {
		return p, fmt.Errorf("sim: no-weight-codec probe: recover: %w", err)
	}
	wlog, err := wal.OpenFS(simWALFS{disk: disk}, walPathFor(cfg.dir))
	if err != nil {
		return p, fmt.Errorf("sim: no-weight-codec probe: WAL OpenFS: %w", err)
	}
	// NewStoreWithCodec is the constructor that leaves the weight codec unset;
	// it is the only way to reach the ErrNoWeightCodec branch at all.
	store := txn.NewStoreWithCodec(g, wlog, txn.NewInt64Codec())

	tx, err := store.BeginCtx(ctx)
	if err != nil {
		_ = wlog.Close()
		return p, fmt.Errorf("sim: no-weight-codec probe: begin: %w", err)
	}
	for _, k := range []int64{1, 2, 3} {
		if err := tx.AddNode(k); err != nil {
			_ = tx.Rollback()
			_ = wlog.Close()
			return p, fmt.Errorf("sim: no-weight-codec probe: add node %d: %w", k, err)
		}
	}
	p.nonZeroErr = tx.AddEdge(1, 2, 42)
	p.zeroErr = tx.AddEdge(1, 2, 0)
	p.withHandleErr = tx.AddEdgeWithHandle(2, 3, 0, 7)

	commitErr := tx.Commit()
	p.committed = commitErr == nil
	if closeErr := wlog.Close(); closeErr != nil {
		return p, fmt.Errorf("sim: no-weight-codec probe: close WAL: %w", closeErr)
	}
	if commitErr != nil {
		//nolint:nilerr // commitErr is recorded as p.committed=false at codec_matrix_run.go:505 and adjudicated as a violation by checkNoWeightCodecContract at codec_matrix_run.go:551
		return p, nil
	}

	// Reopen through real recovery. The replay needs a weight codec to decode
	// weighted records; there are none in this image, so the int64 codec is
	// supplied purely to satisfy the signature and the unweighted record must
	// come back regardless.
	g2, _, _, err := recoverSimGraph(disk, cfg, txn.NewInt64Codec(), txn.NewInt64WeightCodec())
	if err != nil {
		return p, fmt.Errorf("sim: no-weight-codec probe: reopen: %w", err)
	}
	_, p.zeroEdgeRecovered = g2.EdgeWeight(1, 2)
	return p, nil
}

// checkNoWeightCodecContract adjudicates a [codecWeightCodecProbe] against the
// contract [txn.ErrNoWeightCodec] documents. It is a VERDICT gate: it holds
// however little else the run did.
func checkNoWeightCodecContract(p codecWeightCodecProbe) []Violation {
	var v []Violation
	add := func(op, format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op, Message: fmt.Sprintf(format, args...),
		})
	}
	if !errors.Is(p.nonZeroErr, txn.ErrNoWeightCodec) {
		add("<no-weight-codec>",
			"AddEdge with a NON-ZERO weight on a store with no weight codec returned %v, want ErrNoWeightCodec",
			p.nonZeroErr)
	}
	if p.zeroErr != nil {
		add("<zero-weight-rejected>",
			"AddEdge with a ZERO weight on a store with no weight codec returned %v, want nil"+
				" (the documented contract accepts it and buffers an unweighted record)", p.zeroErr)
	}
	if !errors.Is(p.withHandleErr, txn.ErrNoWeightCodec) {
		add("<no-weight-codec-handle>",
			"AddEdgeWithHandle with a ZERO weight returned %v, want ErrNoWeightCodec"+
				" (that entry point requires a weight codec unconditionally)", p.withHandleErr)
	}
	if !p.committed {
		add("<zero-weight-commit-failed>",
			"the transaction carrying the accepted zero-weight edge did not commit")
		return v
	}
	if !p.zeroEdgeRecovered {
		add("<zero-weight-edge-lost>",
			"the accepted zero-weight edge did not survive a crash and reopen:"+
				" the weight-codec-less path is not durable")
	}
	return v
}

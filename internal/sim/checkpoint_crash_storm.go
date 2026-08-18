package sim

// checkpoint_crash_storm.go — crash DURING the checkpoint publish (rmp #2465,
// closing backlog #1827).
//
// Every checkpoint the DST published before this scenario ran to completion:
// [SimStore.Checkpoint] is synchronous and [Simulator.maybeCheckpoint] treats
// any checkpoint error as a hard run failure, so a checkpoint was never
// INTERRUPTED. That left the whole interrupted-publish half of the durability
// contract unexercised — most importantly recovery's snapshot-promote repair
// (store/recovery, the "interrupted-publish repair" block), which was dead code
// under simulation.
//
// The snapshot publish protocol (store/snapshot) is a five-step crash-atomic
// swap:
//
//	write+fsync components -> fsync staging dir -> archive rename (live ->
//	live.bak) -> publish rename (staging -> live) -> fsync the parent dir
//
// The backup exists so that at EVERY instant at least one complete snapshot is
// on disk. This scenario crashes inside that sequence at three seed-ordered
// points and requires the reopen to lose nothing:
//
//	stranded-backup — the parent fsync fails and the crash keeps only the
//	    ARCHIVE rename, so the live name is gone and the previous snapshot is
//	    stranded at live.bak. This is the window recovery's promote repair
//	    exists for, and the only one that reaches it.
//	publish-rename — the publish rename itself fails, so the publish path's own
//	    best-effort archive-restore (Rename(bak, dir)) runs and the live
//	    snapshot must come back intact.
//	archive-rename — the archive rename fails, so the publish aborts before the
//	    live snapshot is touched at all.
//
// Two things make the scenario worth more than the assertion it carries. First,
// the crash lands mid-publish while concurrent Bolt committers are still
// writing: the publish is phase 2 of the checkpoint and holds no commit lock,
// so the interrupted window is genuinely raced. Second, the run does not assume
// the window was entered — it MEASURES it: the interrupted checkpoint must
// return the injected fault, the armed disk primitives must report having
// fired, the durable image is read before and after the reopen, and the
// stranded-backup cycle must show the exact transition only the promote repair
// produces (backup-only on disk before the reopen, live-only after it).
//
// The durability oracle is the standard one the other storage scenarios use,
// accumulated across cycles: acked ⊆ recovered ⊆ issued, with every explicitly
// failed commit absent and no torn CREATE resurrected.

import (
	"context"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// ScenarioCheckpointCrashStorm is the catalogue key of the crash-during-
// checkpoint-publish scenario.
const ScenarioCheckpointCrashStorm = "checkpoint-crash-storm"

// checkpointStormDiskSeedMix decorrelates the disk sub-seed from the workload
// seed, so the seed-chosen crash ordinal is a reproducible function of the run
// seed without sharing low-order bits with the connection/op sub-stream.
const checkpointStormDiskSeedMix uint64 = 0x2465_C9A5_4570_9111

// publishWindow names the point inside the snapshot publish protocol at which
// one cycle interrupts its checkpoint. Every window is produced by an armed
// [SimDisk] primitive, so "the window was entered" is a measured fact (the
// checkpoint returns [ErrSimFault] and the primitive's fire count moves), never
// an assumption about timing.
type publishWindow int

const (
	// windowStrandedBackup fails the post-publish parent-directory fsync and
	// selects the write-back branch for the ARCHIVE rename, so the crash keeps
	// the archive but loses the publish. Recovery must promote the stranded
	// backup; it is the only window that reaches that repair.
	windowStrandedBackup publishWindow = iota
	// windowPublishRename fails the publish rename. The publish path restores
	// the archive it just made, and the write-back arm on the live name lets
	// that restore survive the crash — so recovery must find an intact live
	// snapshot and never reach the promote repair.
	windowPublishRename
	// windowArchiveRename fails the archive rename, aborting the publish before
	// the live snapshot is touched. The previous snapshot's dirent is already
	// durable (its own publish fsynced it), so it survives the crash untouched.
	windowArchiveRename
)

// String renders a publish window for reports and evidence.
func (w publishWindow) String() string {
	switch w {
	case windowStrandedBackup:
		return "stranded-backup"
	case windowPublishRename:
		return "publish-rename"
	case windowArchiveRename:
		return "archive-rename"
	default:
		return fmt.Sprintf("publishWindow(%d)", int(w))
	}
}

// checkpointStormWindows is the fixed set of windows one run drives, one cycle
// each. All three must be entered for the run to be non-vacuous.
func checkpointStormWindows() []publishWindow {
	return []publishWindow{windowStrandedBackup, windowPublishRename, windowArchiveRename}
}

// arm installs the disk primitives that produce this window on the next
// snapshot publish into snapDir. Every arm is one-shot and path-keyed, so it
// targets exactly one step of the protocol and disarms itself.
func (w publishWindow) arm(disk *SimDisk, snapDir string) {
	switch w {
	case windowStrandedBackup:
		// The archive rename reaches stable storage; the publish rename does
		// not, because the fsync that would have made it durable fails.
		disk.ArmRenameWritebackForPath(snapDir + ".bak")
		disk.ArmParentDirSyncFaultForPath(snapDir)
		// Pin the publish rename to the ROLLED-BACK branch. Since rmp #2514 a
		// crash inside a rename picks between its two legal outcomes from the
		// seed, so a publish rename left un-fsync'd survives some crashes — the
		// stranded-backup state is reachable by default but no longer certain,
		// and this cycle asserts it unconditionally. Pinning selects the branch;
		// it does not create it.
		disk.ArmRenameRollbackForPath(snapDir)
	case windowPublishRename:
		// The publish rename fails; the write-back arm is consumed by the
		// archive-restore rename the publish path issues in response, which is
		// what must survive the crash.
		disk.ArmRenameFaultForPath(snapDir)
		disk.ArmRenameWritebackForPath(snapDir)
	case windowArchiveRename:
		disk.ArmRenameFaultForPath(snapDir + ".bak")
	}
}

// checkpointStormConfig sizes one run. The short-layer default keeps the whole
// scenario inside the package's 60 s budget while issuing enough commits that
// the seed-chosen crash ordinal lands with writers still in flight; the soak
// layer runs the full-scale configuration.
type checkpointStormConfig struct {
	connections int
	opsPerConn  int
}

// shortCheckpointStorm is the short-layer size: enough concurrency that the
// interrupted publish is genuinely raced by committers, and enough ops that the
// two checkpoints of every cycle fire mid-flight.
func shortCheckpointStorm() checkpointStormConfig {
	return checkpointStormConfig{connections: 8, opsPerConn: 24}
}

// The seed-chosen durable-commit ordinals a cycle waits for. The CLEAN
// checkpoint fires once a small prefix is durable (it must publish a live
// snapshot for the next one to archive); the INTERRUPTED checkpoint then fires
// at a seed-chosen ordinal in [min, min+span), so the crash lands at a
// seed-chosen point in the commit stream rather than a fixed one. Both sit far
// below the commits a cycle issues, so writers are still in flight.
const (
	checkpointStormCleanSyncs = 4
	checkpointStormFaultMin   = 8
	checkpointStormFaultSpan  = 12
)

// checkpointStormWaitBudget bounds each condition wait on durable progress. It
// is a watchdog, not a coordination primitive: the waits return as soon as the
// commit count is reached (see [waitForSyncProgress]).
const checkpointStormWaitBudget = 30 * time.Second

// checkpointStormCycle is what one cycle MEASURED. Every field is read from the
// durable image or from a [SimDisk] fire counter, so the oracles adjudicate
// facts rather than an assumption that the machinery ran.
type checkpointStormCycle struct {
	// cpErr is the error the interrupted checkpoint returned. A nil here means
	// the publish completed and the window was never entered.
	cpErr error
	// window is the point in the publish protocol this cycle interrupted.
	window publishWindow
	// renameFaults / renameWritebacks are the per-cycle deltas of the armed
	// [SimDisk] rename primitives' fire counts: the reachability evidence that
	// the arms matched a real rename instead of being silently ignored.
	renameFaults     int64
	renameWritebacks int64
	// renameRollbacks is the per-cycle delta of the crash-outcome arm that pins
	// a not-yet-durable rename to its rolled-back branch (rmp #2514). Same role
	// as the two above: an arm that never matched a rename is a silent no-op,
	// and the stranded-backup window depends on this one firing.
	renameRollbacks int64
	// syncsDuringCheckpoint is how many durable commits landed while the
	// interrupted checkpoint was running. It is the concurrency evidence: the
	// publish is lock-free, so a positive value proves the window was raced by
	// live committers rather than entered on a quiesced store.
	syncsDuringCheckpoint int64
	// liveBeforeReopen / bakBeforeReopen are the durable image the crash left,
	// read before recovery touched it; liveAfterReopen / bakAfterReopen are the
	// same two names after the reopen. Together they identify which recovery
	// branch ran (see [checkpointStormCycle.promoted]).
	liveBeforeReopen bool
	bakBeforeReopen  bool
	liveAfterReopen  bool
	bakAfterReopen   bool
}

// promoted reports whether this cycle's reopen ran recovery's interrupted-
// publish repair. The crash must have left the live snapshot absent and a
// backup stranded, and the reopen must have left the live snapshot present and
// the backup gone — the exact transition the promote rename produces, and one
// no other recovery path can produce (every other path leaves both names
// exactly as it found them).
func (c checkpointStormCycle) promoted() bool {
	return !c.liveBeforeReopen && c.bakBeforeReopen && c.liveAfterReopen && !c.bakAfterReopen
}

// checkpointStormEvidence is the whole run's measured evidence, handed to tests
// so their assertions read numbers the run actually produced.
type checkpointStormEvidence struct {
	cycles []checkpointStormCycle
	// acked / issued / failed / recovered are the accumulated durability
	// ledgers at the end of the last cycle.
	acked, issued, failed, recovered int
}

// checkpointStormOptions parameterises one run. The zero value is NOT the
// scenario's configuration; use [defaultCheckpointStormOptions].
type checkpointStormOptions struct {
	// windows overrides the fixed window plan. Nil selects
	// [checkpointStormWindows], the scenario's own plan; a test supplies a
	// DEGENERATE plan to prove the terminal non-vacuity gate is really wired
	// into the run.
	windows []publishWindow
	// discardStrandedBackup is the SENSITIVITY SEAM: when true, the stranded
	// backup is deleted from the durable image — durably, via
	// [SimDisk.ArmRemoveWritebackForPath], because this is damage rather than an
	// unlink the engine issued — after the crash and before the
	// reopen, so recovery has neither a live snapshot nor a backup to promote
	// while the earlier clean checkpoint has already truncated the WAL prefix
	// those commits lived in. That is a genuine lost acknowledged commit — not
	// a doctored oracle input — and the durability oracle must fire. Never set
	// it in a run that is asserting correct behaviour.
	discardStrandedBackup bool
}

// defaultCheckpointStormOptions is the scenario's own configuration: the fixed
// window plan and an undamaged durable image.
func defaultCheckpointStormOptions() checkpointStormOptions {
	return checkpointStormOptions{}
}

// plan returns the window plan this run drives.
func (o checkpointStormOptions) plan() []publishWindow {
	if o.windows != nil {
		return o.windows
	}
	return checkpointStormWindows()
}

// checkpointCrashStormScenario builds the catalogue entry. The run override
// owns the whole multi-cycle crash protocol, so the scenario is ModeConcurrent
// for reporting purposes but never dispatches through the generic runner.
func checkpointCrashStormScenario() Scenario {
	return Scenario{
		Name: ScenarioCheckpointCrashStorm,
		Description: "crash DURING the checkpoint snapshot publish at three points of the crash-atomic swap, racing concurrent " +
			"committers, then recovery (acked⊆recovered⊆issued; a stranded backup is promoted, never a half-published snapshot)",
		Mode:        ModeConcurrent,
		DefaultSeed: 0x2465_5701,
		Connections: shortCheckpointStorm().connections,
		OpsPerConn:  shortCheckpointStorm().opsPerConn,
		run: func(ctx context.Context, seed uint64) (*SimReport, error) {
			return runCheckpointCrashStorm(ctx, seed, shortCheckpointStorm())
		},
	}
}

// runCheckpointCrashStorm performs one run at the given size with the
// scenario's own configuration.
func runCheckpointCrashStorm(ctx context.Context, seed uint64, size checkpointStormConfig) (*SimReport, error) {
	_, report, err := runCheckpointCrashStormWith(ctx, seed, size, defaultCheckpointStormOptions())
	return report, err
}

// checkpointStormLedger accumulates the durability ledgers across cycles. The
// sets are cumulative because each cycle reopens the SAME durable image, so a
// later cycle's recovered graph legitimately contains every earlier cycle's
// acknowledged commits.
type checkpointStormLedger struct {
	acked     map[string]struct{}
	issued    map[string]struct{}
	failed    map[string]struct{}
	recovered map[string]struct{}
}

// newCheckpointStormLedger returns an empty ledger.
func newCheckpointStormLedger() *checkpointStormLedger {
	return &checkpointStormLedger{
		acked:     make(map[string]struct{}),
		issued:    make(map[string]struct{}),
		failed:    make(map[string]struct{}),
		recovered: make(map[string]struct{}),
	}
}

// absorb folds one cycle's concurrent-run result into the ledger. res is taken
// by pointer only because [ConcurrentResult] is a wide value; it is not mutated.
func (l *checkpointStormLedger) absorb(res *ConcurrentResult) {
	for _, n := range res.AckedNames {
		l.acked[n] = struct{}{}
		l.issued[n] = struct{}{}
	}
	for _, n := range res.IssuedNames {
		l.issued[n] = struct{}{}
	}
	for _, n := range res.FailedNames {
		l.failed[n] = struct{}{}
	}
}

// runCheckpointCrashStormWith performs one run and returns the measured
// evidence alongside the report (nil == passed). Tests use it to assert on what
// the run actually exercised and to drive the sensitivity seams.
//
//nolint:gocyclo // one cycle is one linear crash protocol: open, serve, race a clean then an interrupted checkpoint, crash, reopen, adjudicate.
func runCheckpointCrashStormWith(
	ctx context.Context, seed uint64, size checkpointStormConfig, opts checkpointStormOptions,
) (*checkpointStormEvidence, *SimReport, error) {
	disk := NewSimDisk(NewSeed(seed^checkpointStormDiskSeedMix), 0)
	cfg := fullStackStoreConfig()
	snapDir := cfg.dir + "/" + simSnapshotName
	faultSeed := NewSeed(seed ^ checkpointStormDiskSeedMix)
	ledger := newCheckpointStormLedger()
	ev := &checkpointStormEvidence{}

	for i, window := range opts.plan() {
		if err := ctx.Err(); err != nil {
			return ev, nil, err
		}
		cyc, res, err := runCheckpointStormCycle(ctx, &checkpointStormCycleEnv{
			disk: disk, cfg: cfg, snapDir: snapDir, seed: seed + uint64(i),
			faultSeed: faultSeed, size: size, window: window, opts: opts,
			recovered: ledger.recovered,
		})
		ev.cycles = append(ev.cycles, cyc)
		if err != nil {
			return ev, nil, fmt.Errorf("sim: checkpoint-crash-storm cycle %d (%s): %w", i, window, err)
		}
		ledger.absorb(&res.ConcurrentResult)

		if v := checkCheckpointStormCycle(int64(i), &cyc, ledger, res.partial); len(v) > 0 {
			return ev, durableReport(ScenarioCheckpointCrashStorm, ModeConcurrent, seed, v), nil
		}
	}

	ev.acked, ev.issued = len(ledger.acked), len(ledger.issued)
	ev.failed, ev.recovered = len(ledger.failed), len(ledger.recovered)

	// Assert-something-was-seen: the run must have entered every window it
	// planned, proved each arm fired, raced the publish with live committers,
	// and driven the promote repair at least once.
	if v := checkCheckpointStormNonVacuity(ev, ledger); len(v) > 0 {
		return ev, durableReport(ScenarioCheckpointCrashStorm, ModeConcurrent, seed, v), nil
	}
	return ev, nil, nil
}

// checkpointStormCycleEnv is the invariant environment one cycle runs in. It is
// a parameter object so the cycle signature stays readable.
type checkpointStormCycleEnv struct {
	disk      *SimDisk
	faultSeed *Seed
	recovered map[string]struct{}
	snapDir   string
	cfg       simStoreConfig
	opts      checkpointStormOptions
	size      checkpointStormConfig
	seed      uint64
	window    publishWindow
}

// checkpointStormCycleResult carries the concurrent run's ledgers plus the
// torn-CREATE witness the recovered read produced.
type checkpointStormCycleResult struct {
	ConcurrentResult
	partial []string
}

// runCheckpointStormCycle drives one open → serve → clean checkpoint →
// interrupted checkpoint → crash → reopen cycle over the shared durable image,
// folding the recovered names into env.recovered. It owns and tears down every
// resource it creates.
func runCheckpointStormCycle(
	ctx context.Context, env *checkpointStormCycleEnv,
) (checkpointStormCycle, checkpointStormCycleResult, error) {
	cyc := checkpointStormCycle{window: env.window}
	var out checkpointStormCycleResult

	st, err := OpenSimStore(env.disk, env.cfg)
	if err != nil {
		return cyc, out, fmt.Errorf("open store: %w", err)
	}
	srv, err := newSimServerWithLogger(st.Engine(), clock.Real(), quietSimLogger())
	if err != nil {
		_ = st.Close()
		return cyc, out, fmt.Errorf("server: %w", err)
	}

	// Committers run in the background so both checkpoints below race live
	// in-flight commits. RunConcurrent stops at its bounded op count and joins
	// every client goroutine internally before it returns.
	var (
		res            ConcurrentResult
		runErr         error
		committersDone = make(chan struct{})
	)
	go func() {
		defer close(committersDone)
		res, runErr = RunConcurrent(ctx, srv, ConcurrentConfig{
			Seed:        env.seed,
			Connections: env.size.connections,
			OpsPerConn:  env.size.opsPerConn,
			Mix:         &ConcurrentMix{WriterWeight: durableWriters},
		})
	}()

	// A CLEAN checkpoint first: the interrupted publish below can only archive
	// a live snapshot that already exists, so without this the stranded-backup
	// window is unreachable. It also truncates the WAL prefix, which is what
	// makes a lost snapshot a genuine loss of acknowledged commits — the reason
	// the promote repair exists at all.
	deadline := time.Now().Add(checkpointStormWaitBudget)
	waitForSyncProgress(env.disk, checkpointStormCleanSyncs, deadline)
	if err := st.Checkpoint(); err != nil {
		<-committersDone
		_ = srv.Close()
		st.Crash()
		return cyc, out, fmt.Errorf("clean checkpoint: %w", err)
	}

	// The INTERRUPTED checkpoint, at a seed-chosen point in the commit stream.
	target := int64(checkpointStormFaultMin + env.faultSeed.IntN(checkpointStormFaultSpan))
	waitForSyncProgress(env.disk, target, deadline)
	faultsBefore, writebacksBefore := env.disk.RenameFaultCount(), env.disk.RenameWritebackCount()
	rollbacksBefore := env.disk.RenameRollbackCount()
	syncsBefore := env.disk.SyncCount()
	env.window.arm(env.disk, env.snapDir)
	cyc.cpErr = st.Checkpoint()
	cyc.syncsDuringCheckpoint = env.disk.SyncCount() - syncsBefore
	cyc.renameFaults = env.disk.RenameFaultCount() - faultsBefore
	cyc.renameWritebacks = env.disk.RenameWritebackCount() - writebacksBefore
	cyc.renameRollbacks = env.disk.RenameRollbackCount() - rollbacksBefore

	// Crash protocol (order is load-bearing, see runDurableCommitCrash): join
	// the clients, join the server — neither flushes the WAL — then crash the
	// disk without a graceful close, so no acknowledged-but-unsynced frame is
	// made durable and every not-yet-fsync'd dirent is revoked.
	<-committersDone
	_ = srv.Close()
	st.Crash()
	if runErr != nil {
		return cyc, out, fmt.Errorf("concurrent run: %w", runErr)
	}

	// The durable image the crash left, read BEFORE recovery touches it.
	cyc.liveBeforeReopen = env.disk.Exists(env.snapDir + "/manifest.json")
	cyc.bakBeforeReopen = env.disk.Exists(env.snapDir + ".bak/manifest.json")

	// Sensitivity seam only: destroy the stranded backup so the commits the
	// clean checkpoint folded into it are genuinely unrecoverable.
	if env.opts.discardStrandedBackup {
		// DAMAGE to the durable image, not an unlink the engine issued, so the
		// removal is declared durable: since rmp #2536 an ordinary removal stays
		// reversible until its parent directory is fsync'd, and a LATER cycle's
		// crash on this shared disk could otherwise put the backup back and
		// silently un-arm the seam.
		env.disk.ArmRemoveWritebackForPath(env.snapDir + ".bak")
		_ = env.disk.RemoveAll(env.snapDir + ".bak")
		cyc.bakBeforeReopen = env.disk.Exists(env.snapDir + ".bak/manifest.json")
	}

	st2, err := OpenSimStore(env.disk, env.cfg)
	if err != nil {
		return cyc, out, fmt.Errorf("reopen after crash: %w", err)
	}
	defer func() { _ = st2.Close() }()
	cyc.liveAfterReopen = env.disk.Exists(env.snapDir + "/manifest.json")
	cyc.bakAfterReopen = env.disk.Exists(env.snapDir + ".bak/manifest.json")

	recovered, partial, err := recoveredPersonNames(ctx, st2.Engine())
	if err != nil {
		return cyc, out, fmt.Errorf("read recovered graph: %w", err)
	}
	// The recovered set is cumulative across cycles: this cycle's image
	// contains every earlier cycle's durable commits too.
	for name := range recovered {
		env.recovered[name] = struct{}{}
	}
	out = checkpointStormCycleResult{ConcurrentResult: res, partial: partial}
	return cyc, out, nil
}

// checkCheckpointStormCycle adjudicates one cycle: the window really was
// entered, and the durability contract held across the crash.
func checkCheckpointStormCycle(
	tick int64, cyc *checkpointStormCycle, ledger *checkpointStormLedger, partial []string,
) []Violation {
	var v []Violation
	add := func(kind ViolationKind, op, format string, args ...any) {
		v = append(v, Violation{Kind: kind, Op: op, Tick: tick, Message: fmt.Sprintf(format, args...)})
	}

	// The window is a precondition of everything below: a checkpoint that
	// SUCCEEDED never entered the publish window, so the cycle proved nothing.
	if cyc.cpErr == nil {
		add(ViolationOracleDeviation, "<window-not-entered>",
			"the %s checkpoint completed successfully — the armed publish fault never fired, so the crash did not land mid-publish",
			cyc.window)
		return v
	}

	// Durability: every acknowledged commit must survive, from this cycle and
	// every earlier one.
	for _, missing := range setMinus(ledger.acked, ledger.recovered) {
		add(ViolationACIDDurability, "<publish-window-crash>",
			"acknowledged commit %q lost across a crash in the %s publish window (acked=%d recovered=%d)",
			missing, cyc.window, len(ledger.acked), len(ledger.recovered))
	}
	for _, phantom := range setMinus(ledger.recovered, ledger.issued) {
		add(ViolationACIDConsistency, "<phantom>",
			"recovered node %q was never issued (a half-published snapshot leaked state; recovered=%d issued=%d)",
			phantom, len(ledger.recovered), len(ledger.issued))
	}
	for _, resurrected := range setIntersect(ledger.failed, ledger.recovered) {
		add(ViolationACIDAtomicity, "<failed-resurrected>",
			"commit %q the client saw FAIL is present after recovery", resurrected)
	}
	for _, torn := range partial {
		add(ViolationACIDAtomicity, "<torn-create>",
			"recovered node %q lacks its age property (a torn transaction was resurrected)", torn)
	}
	return v
}

// checkCheckpointStormNonVacuity is the terminal assert-something-was-seen
// gate. It refuses a run that passed by exercising nothing: every planned
// window must have been entered with its armed primitive firing, the publish
// must have been raced by live committers, the promote repair must have run,
// and the workload must have acknowledged commits for the durability oracle to
// have had anything to check.
func checkCheckpointStormNonVacuity(ev *checkpointStormEvidence, ledger *checkpointStormLedger) []Violation {
	var v []Violation
	add := func(op, format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op, Message: fmt.Sprintf(format, args...),
		})
	}

	if len(ledger.acked) == 0 {
		add("<non-vacuity>", "no commit was acknowledged — the durability oracle had nothing to adjudicate")
	}

	seen := make(map[publishWindow]bool, len(ev.cycles))
	promotes, raced := 0, 0
	for i, c := range ev.cycles {
		seen[c.window] = true
		if c.syncsDuringCheckpoint > 0 {
			raced++
		}
		if c.promoted() {
			promotes++
		}
		// Each window's arms must have FIRED, not merely been set: an arm whose
		// path never matched is a silent no-op that would leave the cycle
		// asserting a crash window it never entered.
		switch c.window {
		case windowStrandedBackup:
			if c.renameWritebacks == 0 {
				add("<arm-not-fired>", "cycle %d (%s): no rename write-back fired, so the archive rename was not made durable and the backup could not be stranded", i, c.window)
			}
			if !c.promoted() {
				add("<promote-not-reached>",
					"cycle %d (%s): recovery did not run the snapshot-promote repair (live/backup on disk before the reopen: %v/%v, after: %v/%v; want false/true then true/false)",
					i, c.window, c.liveBeforeReopen, c.bakBeforeReopen, c.liveAfterReopen, c.bakAfterReopen)
			}
		case windowPublishRename:
			if c.renameFaults == 0 || c.renameWritebacks == 0 {
				add("<arm-not-fired>", "cycle %d (%s): renameFaults=%d renameWritebacks=%d, want both positive (the publish rename must fail and its archive-restore must survive)", i, c.window, c.renameFaults, c.renameWritebacks)
			}
			if !c.liveAfterReopen {
				add("<restore-lost>", "cycle %d (%s): no live snapshot after recovery — the publish path's archive restore did not survive the crash", i, c.window)
			}
		case windowArchiveRename:
			if c.renameFaults == 0 {
				add("<arm-not-fired>", "cycle %d (%s): no rename fault fired, so the archive rename was not interrupted", i, c.window)
			}
			if !c.liveBeforeReopen {
				add("<live-snapshot-lost>", "cycle %d (%s): the live snapshot did not survive a publish that aborted before touching it", i, c.window)
			}
		}
	}
	for _, w := range checkpointStormWindows() {
		if !seen[w] {
			add("<window-unexercised>", "the %s publish window was never entered by this run", w)
		}
	}
	if promotes == 0 {
		add("<promote-unexercised>", "no cycle drove recovery's snapshot-promote repair — the interrupted-publish path is still unexercised")
	}
	if raced == 0 {
		add("<publish-not-raced>", "no interrupted checkpoint had a durable commit land while it ran — the publish window was never raced by live committers")
	}
	return v
}

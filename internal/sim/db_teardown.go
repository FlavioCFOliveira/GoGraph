package sim

// db_teardown.go — the composed-teardown variant oracles (rmp #2475): what
// [store.DB.CloseCtx] does when the context it is handed is ALREADY CANCELLED,
// and what N goroutines closing the same DB simultaneously observe.
//
// # What the harness closed, and what it never closed
//
// Every durable scenario in this package tears its store down the same way —
// db.CloseCtx(ctx), exactly once, from one goroutine, with a live context
// (durable_scenarios.go, runCheckpointTeardown). store/db.go documents two
// further properties that nothing here exercised:
//
//   - the context bounds ONLY the optional final checkpoint (step 1). Steps 2
//     (stop the checkpoint goroutine) and 3 (close the WAL) run to completion
//     regardless of ctx, "abandoning them on ctx cancellation would reintroduce
//     the goroutine/file-handle leak this type exists to prevent";
//   - Close is idempotent and safe under concurrent callers: the teardown body
//     runs once under a [sync.Once] and every caller observes the same result,
//     "never a spurious [wal.ErrWriterClosed] from a double WAL close".
//
// Both were VERIFIED against store/db.go before this file was written, not taken
// from a description of it: the ordering above is closeOnce0's three steps in
// source order, and the publication is a sync.Once whose error is written and
// read under a mutex (store/db.go, DB.CloseCtx).
//
// # Why the identity of the error is the assertion, not its class
//
// store/db_test.go already drives 8 concurrent Close/CloseCtx callers, but over a
// CLEAN teardown: every caller observes nil, so "all callers agreed" is satisfied
// by the zero value and would remain satisfied by an implementation that re-ran
// the body per caller and happened to fail the same way twice. rmp #2472
// established the distinction that matters — a class check (errors.Is) cannot
// tell one published value from N re-derived ones — so the arm here CONSTRUCTS a
// failing teardown (a one-shot fsync fault on the WAL close's own fsync) and
// requires every caller to observe the SAME VALUE. The quiesce callback wraps the
// WAL-close error freshly on each invocation, so a body that ran twice yields two
// distinct values and the identity clause fires; a second wal.Close would in
// addition return [wal.ErrWriterClosed], which is the independent double-close
// signature the arm counts.
//
// # Acked-implies-recoverable is the invariant, not a teardown quirk
//
// Every arm ends by reopening the SimDisk image through real recovery and
// requiring every commit acknowledged BEFORE the teardown to be present. A
// teardown that loses an acknowledged commit is a durability defect whatever the
// route — a cancelled context, a racing closer, or a failed close fsync — so the
// clause is unconditional and classified [ViolationACIDDurability].
//
// # What a cancelled context actually does to step 1 — measured, not assumed
//
// [checkpoint.Checkpointer.TriggerCtx] submits the request in a select that has
// both the (buffered) submit and ctx.Done() ready when the context is already
// cancelled, so Go's uniform choice decides whether the final checkpoint is
// submitted and folded or abandoned. The arm therefore REPORTS whether the fold
// happened (CheckpointsBefore/CheckpointsAfter, TriggerCtxErrors) as a witness
// and never asserts it: an oracle pinning either outcome would be flaky by
// construction. What IS asserted is what the contract actually promises — the
// cancellation must not become the close's error, must not be counted as a
// final-checkpoint failure, and must not stop steps 2 and 3.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The arm names, used in evidence and in the non-vacuity gate's arm-specific
// clause.
const (
	// ArmDBTeardownCancelledCtx is the cancelled-context arm: a single
	// CloseCtx whose context is cancelled before the call.
	ArmDBTeardownCancelledCtx = "cancelled-ctx"
	// ArmDBTeardownConcurrentClosers is the N-goroutine arm: many callers race
	// into the same teardown.
	ArmDBTeardownConcurrentClosers = "concurrent-closers"
	// ArmDBTeardownInFlightCommit is the boundary arm: the closers run while a
	// commit is parked inside its WAL fsync.
	ArmDBTeardownInFlightCommit = "in-flight-commit"
)

// dbTeardownDiskSeedMix decorrelates this scenario's SimDisk sub-stream from the
// run seed's other consumers, so arming a fault here never perturbs another
// scenario's reproducible fault stream.
const dbTeardownDiskSeedMix uint64 = 0x0DB7_EA12_D0_C105E

// dbTeardownLabel is the node label every acknowledged key carries, so the
// recovered graph can be interrogated per key rather than by count.
const dbTeardownLabel = "DBTeardown"

// dbTeardownInFlightKey is the key of the commit the boundary arm parks inside
// its fsync, and dbTeardownPostCloseKey the key of the commit attempted after the
// teardown. They are named rather than repeated as literals because the arm both
// COMMITS them and later interrogates the recovered graph for them: two literals
// that drifted apart would leave the recovery check silently inspecting a key
// nobody ever wrote.
const (
	dbTeardownInFlightKey  = "inflight-0000"
	dbTeardownPostCloseKey = "post-close-0000"
)

// Bounded, short-layer sizes for the teardown arms.
const (
	// dbTeardownCommits is how many transactions are acknowledged before the
	// liveness checkpoint. Each is two ops (AddNode + SetNodeLabel), so the
	// published snapshot carries a real committed prefix for the reopen to find.
	dbTeardownCommits = 8
	// dbTeardownWALCommits is how many further transactions are acknowledged
	// AFTER that checkpoint, and it is what stops the durability clause being
	// answered by the snapshot alone. MEASURED: with only the pre-checkpoint
	// commits the reopened WAL image was 0 bytes and recovery replayed 0 WAL ops,
	// so every acked key came back from the snapshot and a WAL close that flushed
	// nothing would have passed. These commits live ONLY in the WAL suffix, which
	// [DBTeardownEvidence.RecoveredWALOps] then requires recovery to have replayed.
	dbTeardownWALCommits = 4
	// dbTeardownClosers is the default concurrent-closer count. The task fixes a
	// floor of 8 ([dbTeardownMinClosers]); 16 is comfortably above it and still
	// costs nothing against an in-memory disk.
	dbTeardownClosers = 16
	// dbTeardownMinClosers is the concurrency floor the non-vacuity gate holds
	// the concurrent arm to: fewer than this and "they all agreed" says little.
	dbTeardownMinClosers = 8
	// dbTeardownBlockWindow is how long the in-flight arm waits before deciding
	// the closers are genuinely parked on the quiesce. It mirrors the 50 ms
	// window store/db_quiesce_test.go uses for the same observation.
	dbTeardownBlockWindow = 50 * time.Millisecond
	// dbTeardownJoinTimeout bounds every wait on a goroutine this file owns, so
	// a regression that deadlocks the teardown fails the run instead of hanging
	// the package until the test binary's own timeout.
	dbTeardownJoinTimeout = 30 * time.Second
)

// DBTeardownConfig parameterises one teardown run. The zero value is not
// meaningful: [RunDBTeardown] normalises Arm and Closers, but the arm-selecting
// flags are deliberately explicit.
type DBTeardownConfig struct {
	// Seed is the master seed for the SimDisk sub-stream.
	Seed uint64
	// Arm names the variant under test; it is carried into the evidence so the
	// non-vacuity gate can apply the concurrency floor to the arm that claims
	// concurrency. Empty defaults to [ArmDBTeardownConcurrentClosers].
	Arm string
	// Closers is how many goroutines call Close/CloseCtx simultaneously (< 1
	// normalises to [dbTeardownClosers]). Two further SERIAL calls always follow
	// them — a Close and then a CloseCtx — which is how the "Close after
	// CloseCtx" and "CloseCtx after Close" orderings are pinned by the same
	// identity clause.
	Closers int
	// CancelBeforeClose cancels the context handed to CloseCtx before any closer
	// runs. It also forces EVERY concurrent closer onto CloseCtx: with the
	// io.Closer Close() in the mix a background context could win the sync.Once
	// and the arm would silently stop testing cancellation.
	CancelBeforeClose bool
	// FinalCheckpoint wires [store.WithFinalCheckpoint], so step 1 of the
	// teardown exists at all and the context has something to bound.
	//
	// It belongs to the cancelled-context arm and to no other, which was MEASURED
	// rather than assumed: the final checkpoint folds the whole WAL suffix into a
	// fresh snapshot and truncates the WAL to nothing, so with it enabled the
	// reopen replayed 0 WAL ops and every acknowledged key came back from the
	// snapshot. An arm whose subject is the WAL CLOSE therefore leaves it off, so
	// its durability clause is answered by the WAL the close secured.
	FinalCheckpoint bool
	// FaultOnClose arms a one-shot fsync fault on the next Sync, which is the
	// WAL close's own fsync, so the teardown produces a NON-NIL error whose
	// identity is a discriminating observation. It is mutually exclusive with
	// InFlightCommit (both claim the next Sync ordinal) and is normally combined
	// with FinalCheckpoint disabled, so the fault cannot be eaten by a
	// checkpoint's fsync.
	FaultOnClose bool
	// InFlightCommit parks one commit inside its WAL fsync (a [SyncGate]) and
	// keeps it there while the closers run, so the teardown genuinely races an
	// in-flight writer rather than an idle store.
	InFlightCommit bool
	// SkipCheckpointerHandoff is the SENSITIVITY seam: the DB is built WITHOUT
	// [store.WithCheckpointer], so nothing joins the checkpoint goroutine. It
	// reproduces the leak the composed teardown exists to prevent, which is how
	// the join clause is proved to fire. The run stops the loop itself
	// afterwards, so the seam leaves no goroutine behind.
	SkipCheckpointerHandoff bool
	// Probe, when non-nil, is called after every closer has returned and BEFORE
	// the run stops anything the DB did not stop itself. It is the seam that
	// lets a test observe the goroutine population at the instant the teardown
	// claims the checkpoint loop is joined. It must not touch the store.
	Probe func()
}

// DBTeardownEvidence is what one teardown run OBSERVED. It carries measurements
// and no verdict, so the adjudicators below are pure functions of it and can be
// falsified by a doctored value rather than by hoping a real run misbehaves.
type DBTeardownEvidence struct {
	// Arm is the variant that produced this evidence.
	Arm string
	// CloseErr renders the value every caller was expected to observe (the first
	// closer's), and CloseErrNil records whether it was nil.
	CloseErr string
	// PostCloseTriggerErr is what a checkpoint request returned after the
	// teardown, and PostCloseCommitErr what a commit attempt returned.
	PostCloseTriggerErr string
	PostCloseCommitErr  string
	// InFlightCommitErr is the error of the commit that was parked inside its
	// fsync while the closers ran (empty when it was acknowledged).
	InFlightCommitErr string
	// AckedKeys are the keys of the commits acknowledged BEFORE the teardown,
	// and MissingAckedKeys those the reopen could not find. MissingAckedKeys
	// non-empty is a durability defect.
	AckedKeys        []string
	MissingAckedKeys []string
	// Closers is the number of concurrent callers, SerialClosers the number of
	// serial calls made after them (always 2: a Close then a CloseCtx).
	Closers       int
	SerialClosers int
	// DistinctCloseErrs is how many distinct error VALUES the callers observed.
	// It must be 1: the teardown publishes one result.
	DistinctCloseErrs int
	// WriterClosedClosers counts callers whose error carried
	// [wal.ErrWriterClosed] — the signature of a second WAL close reaching a
	// caller.
	WriterClosedClosers int
	// TeardownBodyRuns is how many times the quiesce callback (and therefore the
	// WAL close inside it) was invoked. The sync.Once must make it exactly 1.
	TeardownBodyRuns int64
	// CloseCalls, CloseErrorMetric, FinalCheckpointErrorMetric and
	// TriggerCtxErrors are the DB's and the checkpointer's OWN instrumentation,
	// read independently of everything above: how many calls reached CloseCtx,
	// how often the teardown recorded an error (inside the Once), how often the
	// final checkpoint's error was classified as a genuine failure rather than a
	// benign cancellation, and how often TriggerCtx returned an error.
	CloseCalls                 uint64
	CloseErrorMetric           uint64
	FinalCheckpointErrorMetric uint64
	TriggerCtxErrors           uint64
	// CheckpointsBefore / CheckpointsAfter bracket the teardown, so the witness
	// can report whether step 1 actually folded under a cancelled context.
	CheckpointsBefore uint64
	CheckpointsAfter  uint64
	// WALBytes is the size of the durable WAL image at reopen and
	// SnapshotPublished whether a snapshot manifest sits beside it. Together they
	// are the shape floor: an assertion about recovery over an absent durable
	// image would be satisfied by definition.
	WALBytes int
	// AckedCommits is how many commits were acknowledged before the teardown, and
	// AckedAfterCheckpoint how many of those landed after the last checkpoint and
	// therefore live only in the WAL suffix.
	AckedCommits         int
	AckedAfterCheckpoint int
	// RecoveredWALOps is how many WAL ops the reopen's recovery actually
	// replayed. It is the independent proof that the durability clause was
	// answered by the WAL and not by the snapshot underneath it.
	RecoveredWALOps int
	// CtxCancelled records that the context handed to CloseCtx was already
	// cancelled, and CloseErrIsCtx that the close returned that cancellation.
	CtxCancelled  bool
	CloseErrIsCtx bool
	// CloseErrNil, CloseErrIsSimFault report the published value's shape.
	CloseErrNil        bool
	CloseErrIsSimFault bool
	// FaultArmed records that a one-shot fsync fault was armed on the WAL close.
	FaultArmed bool
	// FinalCheckpoint records that step 1 was wired at all.
	FinalCheckpoint bool
	// CheckpointerOwned records that the DB was given the checkpointer to stop
	// (false only on the sensitivity seam).
	CheckpointerOwned bool
	// LoopAliveBeforeClose is the liveness probe taken BEFORE the teardown: a
	// checkpoint request succeeded, so there was a goroutine to join.
	LoopAliveBeforeClose bool
	// LoopStoppedAfterClose is the join verdict: after the teardown a checkpoint
	// request returned [checkpoint.ErrCheckpointerStopped]. A WAL-closed error
	// instead means the loop was still alive and touched a closed WAL — the exact
	// failure store.DB exists to prevent.
	LoopStoppedAfterClose bool
	// PostCloseCommitRefused is whether a commit attempted after the teardown was
	// refused with [wal.ErrWriterClosed], which is how "the WAL is closed" is
	// observed from outside.
	PostCloseCommitRefused bool
	// PostCloseKeyRecovered is whether the key of that refused commit was found
	// in the reopened graph — an unacknowledged write that became durable.
	PostCloseKeyRecovered bool
	// InFlightGated / GateFired record that the boundary arm parked a commit
	// inside its fsync and that the rendezvous actually happened.
	InFlightGated bool
	GateFired     bool
	// CloseBlockedOnInFlight is the ordering observation: no closer had returned
	// while the commit was still parked.
	CloseBlockedOnInFlight bool
	// SnapshotPublished is whether a snapshot manifest exists on the disk.
	SnapshotPublished bool
	// ReopenClean is whether the reopen's recovery found no genuine corruption.
	ReopenClean bool
}

// String renders the evidence for a failure message or a test log.
func (e *DBTeardownEvidence) String() string {
	return fmt.Sprintf("db-teardown[%s]: closers=%d(+%d serial) calls=%d bodyRuns=%d distinctErrs=%d writerClosed=%d "+
		"err=%q ctxCancelled=%t errIsCtx=%t faultArmed=%t errIsFault=%t loopAlive=%t loopStopped=%t postCloseTrigger=%q commitRefused=%t "+
		"acked=%d(%d post-checkpoint) missing=%d postCloseKeyRecovered=%t cpOwned=%t inFlight=%t gateFired=%t closeBlocked=%t "+
		"checkpoints=%d->%d triggerErrs=%d closeErrMetric=%d finalCpErrMetric=%d walBytes=%d walOpsReplayed=%d snapshot=%t clean=%t",
		e.Arm, e.Closers, e.SerialClosers, e.CloseCalls, e.TeardownBodyRuns, e.DistinctCloseErrs, e.WriterClosedClosers,
		e.CloseErr, e.CtxCancelled, e.CloseErrIsCtx, e.FaultArmed, e.CloseErrIsSimFault,
		e.LoopAliveBeforeClose, e.LoopStoppedAfterClose, e.PostCloseTriggerErr, e.PostCloseCommitRefused,
		e.AckedCommits, e.AckedAfterCheckpoint, len(e.MissingAckedKeys), e.PostCloseKeyRecovered, e.CheckpointerOwned,
		e.InFlightGated, e.GateFired, e.CloseBlockedOnInFlight,
		e.CheckpointsBefore, e.CheckpointsAfter, e.TriggerCtxErrors, e.CloseErrorMetric, e.FinalCheckpointErrorMetric,
		e.WALBytes, e.RecoveredWALOps, e.SnapshotPublished, e.ReopenClean)
}

// FinalCheckpointFolded reports whether step 1 actually took a checkpoint during
// the teardown. It is a WITNESS: under a cancelled context either outcome is
// legal (see the file comment), so it is logged and never adjudicated.
func (e *DBTeardownEvidence) FinalCheckpointFolded() bool {
	return e.CheckpointsAfter > e.CheckpointsBefore
}

// dbTeardownEnv is one durable stack under test: a SimDisk, a full-stack store
// on it, a started checkpointer, and the composed [store.DB] that owns their
// teardown.
type dbTeardownEnv struct {
	disk     *SimDisk
	st       *SimStore
	cp       *checkpoint.Checkpointer[string, float64]
	cpCancel context.CancelFunc
	db       *store.DB
	cfg      simStoreConfig
	// bodyRuns counts invocations of the quiesce callback, which is the teardown
	// body's only route to the WAL close.
	bodyRuns atomic.Int64
}

// quiesce is the [store.WithQuiesce] callback. It drains in-flight writers
// through the store's commit lock exactly as production wiring does, counts the
// invocation, and wraps a non-nil error in a FRESHLY allocated value — which is
// what makes "every caller observed the same value" a discriminating assertion
// rather than a tautology over a shared sentinel.
func (e *dbTeardownEnv) quiesce(fn func() error) error {
	e.bodyRuns.Add(1)
	if err := e.st.store.RunUnderCommitLock(fn); err != nil {
		return fmt.Errorf("sim: db-teardown WAL close: %w", err)
	}
	return nil
}

// stopLoop stops the checkpoint goroutine and releases its context. It is
// idempotent ([checkpoint.Checkpointer.Stop] is), so it is safe after a teardown
// that already stopped the loop.
func (e *dbTeardownEnv) stopLoop() {
	if e.cp != nil {
		e.cp.Stop()
	}
	if e.cpCancel != nil {
		e.cpCancel()
	}
}

// openDBTeardownEnv stands up the durable stack: a store on a fresh SimDisk, a
// checkpointer started over it with the same seams [SimStore.Checkpoint] uses,
// and the composed DB wired per cfg.
func openDBTeardownEnv(cfg DBTeardownConfig) (*dbTeardownEnv, error) {
	scfg := fullStackStoreConfig()
	disk := NewSimDisk(NewSeed(cfg.Seed^dbTeardownDiskSeedMix), 0) // faultRate 0: only the armed one-shot fault fires
	st, err := OpenSimStore(disk, scfg)
	if err != nil {
		return nil, fmt.Errorf("sim: db-teardown open store: %w", err)
	}

	// Config carries no MaxAge, so the loop fires only on an explicit trigger:
	// the arm controls the cadence and no background fsync can consume the
	// ordinal an armed fault is waiting on.
	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](
		checkpoint.Config{Dir: scfg.dir}, st.graph, st.wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.store.Codec()),
		checkpoint.WithWeightCodec[string, float64](st.store.WeightCodec()),
		checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend[string, float64]{disk: disk}),
		checkpoint.WithConstraintSpecs[string, float64](st.engine.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](st.engine.IndexSpecsForSnapshot),
	)
	cpCtx, cpCancel := context.WithCancel(context.Background())
	cp.Start(cpCtx)

	env := &dbTeardownEnv{disk: disk, st: st, cp: cp, cpCancel: cpCancel, cfg: scfg}
	opts := []store.Option{store.WithQuiesce(env.quiesce)}
	if !cfg.SkipCheckpointerHandoff {
		opts = append(opts, store.WithCheckpointer(cp))
	}
	if cfg.FinalCheckpoint {
		opts = append(opts, store.WithFinalCheckpoint())
	}
	env.db = store.New(st.wlog, opts...)
	return env, nil
}

// dbTeardownCommit commits one two-op transaction (AddNode + SetNodeLabel) and
// returns the key it acknowledged. It goes through the transaction layer rather
// than the Cypher engine so the arm controls exactly what a commit is: one
// transaction, two ops, one SyncGroup round.
func dbTeardownCommit(ctx context.Context, env *dbTeardownEnv, key string) error {
	tx, err := env.st.store.BeginCtx(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", key, err)
	}
	if aerr := tx.AddNode(key); aerr != nil {
		_ = tx.Rollback()
		return fmt.Errorf("add node %s: %w", key, aerr)
	}
	if lerr := tx.SetNodeLabel(key, dbTeardownLabel); lerr != nil {
		_ = tx.Rollback()
		return fmt.Errorf("label node %s: %w", key, lerr)
	}
	return tx.Commit()
}

// RunDBTeardown drives one teardown variant end to end: acknowledge a durable
// prefix, optionally park a commit inside its fsync, run cfg.Closers concurrent
// Close/CloseCtx callers plus two serial ones, probe what the teardown left
// behind, and reopen the SimDisk image through real recovery.
//
// It installs the recording metrics backend for the duration (the same global
// sink [MetricsOracle] and the group-commit oracle use) and restores the no-op
// default before returning, so it must run SERIALLY: the caller must not run
// concurrent metrics-emitting work.
func RunDBTeardown(ctx context.Context, cfg DBTeardownConfig) (DBTeardownEvidence, error) {
	if cfg.Arm == "" {
		cfg.Arm = ArmDBTeardownConcurrentClosers
	}
	if cfg.Closers < 1 {
		cfg.Closers = dbTeardownClosers
	}
	if cfg.FaultOnClose && cfg.InFlightCommit {
		return DBTeardownEvidence{}, fmt.Errorf("sim: db-teardown: FaultOnClose and InFlightCommit both claim the next Sync ordinal")
	}

	ev := DBTeardownEvidence{
		Arm:               cfg.Arm,
		Closers:           cfg.Closers,
		SerialClosers:     2,
		CtxCancelled:      cfg.CancelBeforeClose,
		FaultArmed:        cfg.FaultOnClose,
		FinalCheckpoint:   cfg.FinalCheckpoint,
		CheckpointerOwned: !cfg.SkipCheckpointerHandoff,
		InFlightGated:     cfg.InFlightCommit,
	}

	env, err := openDBTeardownEnv(cfg)
	if err != nil {
		return ev, err
	}
	// The loop is stopped on every exit path, including the seam that leaves the
	// DB unable to stop it, so no arm can leak the goroutine into another test.
	defer env.stopLoop()

	// A durable prefix: every one of these is acknowledged before anything is
	// torn down, and every one of them must survive the reopen.
	for i := range dbTeardownCommits {
		key := fmt.Sprintf("acked-%04d", i)
		if cerr := dbTeardownCommit(ctx, env, key); cerr != nil {
			return ev, fmt.Errorf("sim: db-teardown acked commit: %w", cerr)
		}
		ev.AckedKeys = append(ev.AckedKeys, key)
	}
	ev.AckedCommits = len(ev.AckedKeys)

	// Liveness probe: a checkpoint through the live loop. It proves there IS a
	// goroutine to join (without it the join verdict would be satisfied by the
	// absence of a loop) and publishes a snapshot, so the reopen goes through the
	// full snapshot+WAL path rather than WAL-only.
	if terr := env.cp.Trigger(); terr == nil {
		ev.LoopAliveBeforeClose = true
	}
	ev.CheckpointsBefore = env.cp.Stats().Checkpoints

	// A committed suffix that exists ONLY in the WAL. Without it the checkpoint
	// above has already folded every acked key into the snapshot, the reopened
	// WAL image is empty, and "every acknowledged commit was recovered" is
	// answered by the snapshot however the WAL was closed.
	for i := range dbTeardownWALCommits {
		key := fmt.Sprintf("wal-acked-%04d", i)
		if cerr := dbTeardownCommit(ctx, env, key); cerr != nil {
			return ev, fmt.Errorf("sim: db-teardown post-checkpoint commit: %w", cerr)
		}
		ev.AckedKeys = append(ev.AckedKeys, key)
		ev.AckedAfterCheckpoint++
	}
	ev.AckedCommits = len(ev.AckedKeys)

	// The boundary arm: park one commit inside its WAL fsync and leave it there
	// while the closers run.
	var (
		gate         *SyncGate
		inFlightDone chan error
	)
	if cfg.InFlightCommit {
		gate = env.disk.ArmSyncGateAt(1)
		inFlightDone = make(chan error, 1)
		go func() { inFlightDone <- dbTeardownCommit(ctx, env, dbTeardownInFlightKey) }()
		select {
		case <-gate.Reached():
		case <-ctx.Done():
			gate.Release()
			<-inFlightDone
			return ev, ctx.Err()
		case <-time.After(dbTeardownJoinTimeout):
			gate.Release()
			<-inFlightDone
			return ev, fmt.Errorf("sim: db-teardown: the in-flight commit never reached its gated fsync")
		}
	}

	// The fault arm: the next Sync is the WAL close's own fsync (no committer is
	// running and the checkpoint loop has no ticker), so this makes step 3 fail
	// deterministically and gives the identity clause a non-nil value to pin.
	if cfg.FaultOnClose {
		env.disk.ArmSyncFaultAt(1)
	}

	rb := newRecordingBackend()
	metrics.SetBackend(rb)

	closeCtx := ctx
	if cfg.CancelBeforeClose {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		closeCtx = cancelled
	}

	errs, blocked := runDBTeardownClosers(closeCtx, env.db, cfg, gate)
	ev.CloseBlockedOnInFlight = blocked

	// Two SERIAL follow-ups, in both orders relative to the concurrent callers:
	// a plain Close (the io.Closer entry point) and then a CloseCtx with a LIVE
	// context. Both must return the value the single teardown published, which is
	// how "Close after CloseCtx" and "CloseCtx after Close" are pinned by the
	// same identity clause.
	errs = append(errs, env.db.Close(), env.db.CloseCtx(context.Background()))

	metrics.SetBackend(nil)
	ev.CloseCalls = rb.latencyCount("store.DB.Close")
	ev.CloseErrorMetric = rb.counter("store.DB.Close.errors")
	ev.FinalCheckpointErrorMetric = rb.counter("store.DB.Close.finalCheckpointErrors")
	ev.TriggerCtxErrors = rb.counter("store.checkpoint.TriggerCtx.errors")
	ev.CheckpointsAfter = env.cp.Stats().Checkpoints
	ev.TeardownBodyRuns = env.bodyRuns.Load()

	if gate != nil {
		ev.GateFired = gate.Fired()
		select {
		case ierr := <-inFlightDone:
			if ierr != nil {
				ev.InFlightCommitErr = ierr.Error()
			}
		case <-time.After(dbTeardownJoinTimeout):
			return ev, fmt.Errorf("sim: db-teardown: the in-flight commit never returned after the teardown")
		}
	}

	ev.DistinctCloseErrs = countDistinctErrors(errs)
	for _, cerr := range errs {
		if errors.Is(cerr, wal.ErrWriterClosed) {
			ev.WriterClosedClosers++
		}
	}
	first := errs[0]
	ev.CloseErrNil = first == nil
	ev.CloseErr = errText(first)
	ev.CloseErrIsCtx = errors.Is(first, context.Canceled) || errors.Is(first, context.DeadlineExceeded)
	ev.CloseErrIsSimFault = errors.Is(first, ErrSimFault)

	if cfg.Probe != nil {
		cfg.Probe()
	}

	// What the teardown left behind, observed from outside: a checkpoint request
	// must find the loop gone, and a commit must find the WAL closed. The trigger
	// probe is taken BEFORE this run stops anything the DB did not stop itself —
	// otherwise the sensitivity seam would report the join it exists to prevent.
	// On the seam the loop is still alive here, so the request reaches a live loop
	// that Syncs a CLOSED WAL: the swallowed-error hazard store.DB documents.
	terr := env.cp.Trigger()
	ev.PostCloseTriggerErr = errText(terr)
	ev.LoopStoppedAfterClose = errors.Is(terr, checkpoint.ErrCheckpointerStopped)
	env.stopLoop() // a no-op unless the seam left the loop running

	perr := dbTeardownCommit(ctx, env, dbTeardownPostCloseKey)
	ev.PostCloseCommitErr = errText(perr)
	ev.PostCloseCommitRefused = errors.Is(perr, wal.ErrWriterClosed)

	walPath := walPathFor(env.cfg.dir)
	if env.disk.Exists(walPath) {
		image, rerr := env.disk.ReadFile(walPath)
		if rerr != nil {
			return ev, fmt.Errorf("sim: db-teardown read WAL image: %w", rerr)
		}
		ev.WALBytes = len(image)
	}
	ev.SnapshotPublished = hasDurableSnapshot(env.disk, env.cfg.dir)

	// Reopen through real recovery over the surviving byte image.
	re, err := OpenSimStore(env.disk, env.cfg)
	if err != nil {
		return ev, fmt.Errorf("sim: db-teardown reopen: %w", err)
	}
	defer func() { _ = re.Close() }()
	ev.ReopenClean = re.Clean()
	ev.RecoveredWALOps = re.WALOps()
	for _, key := range ev.AckedKeys {
		if !re.graph.HasNodeLabel(key, dbTeardownLabel) {
			ev.MissingAckedKeys = append(ev.MissingAckedKeys, key)
		}
	}
	if gate != nil && ev.InFlightCommitErr == "" {
		// The in-flight commit was acknowledged, so it is an acked commit like
		// any other and recovery must find it.
		if !re.graph.HasNodeLabel(dbTeardownInFlightKey, dbTeardownLabel) {
			ev.MissingAckedKeys = append(ev.MissingAckedKeys, dbTeardownInFlightKey)
		}
		ev.AckedKeys = append(ev.AckedKeys, dbTeardownInFlightKey)
		ev.AckedCommits++
	}
	ev.PostCloseKeyRecovered = re.graph.HasNodeLabel(dbTeardownPostCloseKey, dbTeardownLabel)
	return ev, nil
}

// runDBTeardownClosers launches cfg.Closers goroutines that all call into the
// same teardown at once, and reports their errors in call order plus — for the
// boundary arm — whether none of them had returned while a commit was still
// parked inside its fsync.
//
// The callers are released from a barrier rather than started one by one, so
// they genuinely race the sync.Once instead of arriving in sequence.
func runDBTeardownClosers(closeCtx context.Context, db *store.DB, cfg DBTeardownConfig, gate *SyncGate) (errs []error, blockedOnInFlight bool) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	out := make([]error, cfg.Closers)
	var returned atomic.Int64

	ready.Add(cfg.Closers)
	done.Add(cfg.Closers)
	for i := range cfg.Closers {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			// Mix the io.Closer entry point with the ctx-aware one so both are
			// proved to funnel through the same sync.Once — except on the
			// cancelled arm, where a background-context Close winning the Once
			// would silently stop the arm from testing cancellation at all.
			if cfg.CancelBeforeClose || i%2 == 1 {
				out[i] = db.CloseCtx(closeCtx)
				returned.Add(1)
				return
			}
			out[i] = db.Close()
			returned.Add(1)
		}(i)
	}
	ready.Wait()
	close(start)

	if gate != nil {
		// The commit is parked inside its fsync and holds its in-flight
		// registration, so a quiescing teardown must still be draining. Give the
		// closers a bounded window to (wrongly) return before releasing it.
		time.Sleep(dbTeardownBlockWindow)
		blockedOnInFlight = returned.Load() == 0
		gate.Release()
	}
	done.Wait()
	return out, blockedOnInFlight
}

// errText renders an error for the evidence, mapping nil to the empty string.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// countDistinctErrors returns how many distinct error VALUES appear in errs,
// comparing by identity rather than by class or by message: two values that
// merely wrap the same sentinel with the same text are distinct here, which is
// exactly what a per-caller re-derivation of the teardown would produce.
func countDistinctErrors(errs []error) int {
	var representatives []error
	for _, err := range errs {
		known := false
		for _, rep := range representatives {
			if identicalError(rep, err) {
				known = true
				break
			}
		}
		if !known {
			representatives = append(representatives, err)
		}
	}
	return len(representatives)
}

// identicalError reports whether a and b are the SAME error value. Comparing two
// interface values with == panics when their dynamic type is not comparable, so
// the type is checked first; a non-comparable dynamic type is reported as
// distinct, which fails the identity clause loudly rather than crashing the run.
func identicalError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	// The == is the ASSERTION, not an oversight. errors.Is answers the CLASS
	// question, and the class is exactly what cannot tell one published value from
	// N values re-derived per caller — the distinction this file exists to pin.
	//nolint:errorlint // identity is the property under test; errors.Is would defeat it.
	return a == b
}

// checkDBTeardownNonVacuity is the SEPARATE coverage precondition, kept apart
// from [checkDBTeardown] for the reason rmp #2470 established: an uninformative
// run must not read as a faulty one. It asserts the run had the SHAPE a teardown
// adjudication needs — commits actually acknowledged, something durable
// underneath them, a checkpoint loop that was actually running, the calls the arm
// claims to have made, and (on the arm that pins error identity) a non-nil
// published value.
//
// It is shape-only by design and says nothing about what the teardown did. A
// violation here means the RUN proved nothing, not that [store.DB] is broken.
func checkDBTeardownNonVacuity(e *DBTeardownEvidence) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<db-teardown non-vacuity>", Message: msg + " — " + e.String()})
	}
	if e.AckedCommits == 0 {
		add("no commit was acknowledged before the teardown: acked-implies-recoverable is satisfied by the empty set")
	}
	if e.AckedAfterCheckpoint == 0 {
		add("no commit was acknowledged after the last checkpoint: the whole acked set was already in the snapshot before the teardown began, so recovering it says nothing about the teardown")
	}
	if e.RecoveredWALOps == 0 && !e.FinalCheckpointFolded() {
		add("recovery replayed no WAL op and the teardown folded no final checkpoint: the commits acknowledged after the last checkpoint are unaccounted for, so this run says nothing about what the teardown secured")
	}
	if !e.LoopAliveBeforeClose {
		add("the checkpoint loop was not running before the teardown: \"the goroutine was joined\" is satisfied by there being no goroutine to join")
	}
	if want := uint64(e.Closers + e.SerialClosers); e.CloseCalls != want {
		add(fmt.Sprintf("the DB observed %d Close call(s); the arm made %d: the run did not exercise the concurrency it reports", e.CloseCalls, want))
	}
	if e.Arm == ArmDBTeardownConcurrentClosers && e.Closers < dbTeardownMinClosers {
		add(fmt.Sprintf("only %d concurrent closer(s), need >= %d for a statement about racing callers", e.Closers, dbTeardownMinClosers))
	}
	if e.FaultArmed && e.CloseErrNil {
		add("the armed fault never reached the WAL close: every caller observed nil, so the error-identity clause is satisfied by the zero value and proves nothing")
	}
	if e.InFlightGated && !e.GateFired {
		add("the fsync rendezvous never fired: no commit was in flight when the closers ran")
	}
	return v
}

// checkDBTeardown adjudicates the composed-teardown contract over one run. It is
// a PURE function of the observed evidence, which is what lets the sensitivity
// test falsify every clause by handing it a doctored value.
//
// The clauses, in the order a failure is most usefully read:
//
//   - the teardown body ran exactly once, by the arm's own count and by the DB's
//     own error counter;
//   - every caller observed the same error VALUE, and none of them was handed a
//     spurious [wal.ErrWriterClosed] from a second WAL close;
//   - the checkpoint goroutine was joined and the WAL was closed — steps 2 and 3,
//     which a cancelled context must not be able to skip;
//   - a cancelled context did not become the close's error, nor a counted
//     final-checkpoint failure;
//   - the published error matches what the arm injected (nothing, or the armed
//     fsync fault);
//   - a close racing an in-flight commit waited for it, and that commit was
//     acknowledged rather than failed by the teardown;
//   - and, above everything, recovery found every acknowledged commit and none of
//     the refused one.
func checkDBTeardown(e *DBTeardownEvidence) []Violation {
	var v []Violation
	add := func(kind ViolationKind, msg string) {
		v = append(v, Violation{Kind: kind, Op: "<db-teardown>", Message: msg + " — " + e.String()})
	}

	if e.TeardownBodyRuns != 1 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the teardown body ran %d time(s); the sync.Once must run it exactly once — a second run flushes and fsyncs a WAL another caller already closed",
			e.TeardownBodyRuns))
	}
	if e.CloseErrorMetric > 1 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the DB's own error counter incremented %d times for one teardown: the guarded body ran more than once", e.CloseErrorMetric))
	}
	if e.DistinctCloseErrs != 1 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d distinct error VALUES across %d caller(s); every caller must observe the one value the teardown published, not an equal-looking one re-derived per call",
			e.DistinctCloseErrs, e.Closers+e.SerialClosers))
	}
	if e.WriterClosedClosers != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d caller(s) received wal.ErrWriterClosed: that is the signature of a second WAL close reaching a caller, which the sync.Once exists to prevent",
			e.WriterClosedClosers))
	}
	if !e.LoopStoppedAfterClose {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the checkpoint loop was still alive after the teardown returned (post-close trigger = %q): step 2 did not join the goroutine, so the loop outlives the shutdown intent and can still Sync/Truncate a closed WAL",
			e.PostCloseTriggerErr))
	}
	if !e.PostCloseCommitRefused {
		add(ViolationACIDDurability, fmt.Sprintf(
			"a commit attempted after the teardown was not refused with wal.ErrWriterClosed (got %q): step 3 did not close the WAL",
			e.PostCloseCommitErr))
	}
	if e.CtxCancelled && e.CloseErrIsCtx {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"CloseCtx returned the context's own cancellation (%q): ctx bounds ONLY the final checkpoint, and what Close returns is the WAL-close outcome",
			e.CloseErr))
	}
	if e.CtxCancelled && e.FinalCheckpointErrorMetric != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the cancelled final checkpoint was counted as a genuine failure (store.DB.Close.finalCheckpointErrors=%d): a context error on step 1 is benign and must not be recorded as one",
			e.FinalCheckpointErrorMetric))
	}
	if !e.FaultArmed && !e.CloseErrNil {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"a fault-free teardown returned %q; nothing in this arm can legitimately fail the WAL close", e.CloseErr))
	}
	if e.FaultArmed && !e.CloseErrNil && !e.CloseErrIsSimFault {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the published error %q does not carry the injected fsync fault: the failure came from somewhere other than the WAL close this arm faulted", e.CloseErr))
	}
	if e.InFlightGated && !e.CloseBlockedOnInFlight {
		add(ViolationACIDDurability,
			"a closer returned while a commit was parked inside its WAL fsync: the teardown did not drain in-flight writers, so wal.Close raced the commit's own fsync")
	}
	if e.InFlightGated && e.InFlightCommitErr != "" {
		add(ViolationACIDDurability, fmt.Sprintf(
			"the commit that was in flight at the teardown failed with %q: a quiescing close must let it finish, not fail it", e.InFlightCommitErr))
	}
	if len(e.MissingAckedKeys) > 0 {
		add(ViolationACIDDurability, fmt.Sprintf(
			"%d acknowledged commit(s) did not survive the teardown and reopen (%v): a commit acknowledged before a close is durable whatever the close then did",
			len(e.MissingAckedKeys), e.MissingAckedKeys))
	}
	if e.PostCloseKeyRecovered {
		add(ViolationACIDAtomicity,
			"the transaction refused after the teardown was found in the recovered graph: a write nobody was told had committed became durable")
	}
	if !e.ReopenClean {
		add(ViolationACIDDurability,
			"the reopen's recovery reported genuine corruption: the teardown left an image recovery cannot trust")
	}
	return v
}

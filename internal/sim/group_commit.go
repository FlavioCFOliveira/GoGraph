package sim

// group_commit.go — the WAL group-commit oracle (rmp #2471): a coverage
// precondition on fsync COALESCING, and an end-to-end assertion of the FAIL-ALL
// branch that coalescing makes necessary.
//
// # What was measured but never asserted
//
// [wal.Writer.SyncGroup] implements PostgreSQL-XLogFlush-style commit
// coalescing: one elected leader flushes and fsyncs once, covering its own and
// every concurrently-buffered committer's frames, and those followers return
// durable without any I/O of their own. The durable-commit scenario has driven
// that path since sprint 270 and its own file comment records a MEASURED
// follower rate — 480 SyncGroup calls, 16 of them followers, rmp #2347 — but
// nothing in the simulator ever READ the counter.
//
// A measurement in a comment is not a gate. A regression that reverted the
// engine to serialising its commits would restore the solo-leader behaviour the
// comment was written to refute: every commit would pay its own fsync (halving
// write throughput), the fail-all branch would stop being reachable through the
// engine at all, and every existing scenario would still pass, because a
// solo-leader SyncGroup is durable and correct — merely slower, and merely
// un-covering the hardest branch in the commit path. [checkGroupCommitCoverage]
// is the gate that would fail instead.
//
// # The counter is a FOLLOWER counter, and that was verified rather than assumed
//
// The oracle reads store.wal.SyncGroup.coalesced, which
// [wal.Writer.syncToLocked] increments on the durable-already fast path. That
// path is shared with [wal.Writer.SyncBuffered], which the txn layer calls for
// an EMPTY commit (store/txn/txn.go: no sequence minted, nothing of its own to
// acknowledge) — so a workload of empty commits could in principle drive the
// counter up without a single genuine follower, which would make a bare
// `coalesced > 0` gate satisfiable by a run that coalesced nothing.
//
// It does not happen here, and that is measured rather than argued: the
// single-committer control arm ([RunGroupCommitCoalescing] at Connections=1)
// drives the same all-writer workload through the same stack and reads
// coalesced=0. Every increment this oracle sees under concurrency is therefore a
// real follower. The control arm is retained as a permanent sensitivity proof —
// [TestGroupCommit_SoloCommitterHasNoFollowers] — precisely so that this stops
// being true loudly rather than quietly.
//
// # Why the fail-all arm does not go through the engine
//
// Part (b) asserts that when the leader's fsync fails, EVERY member of that
// commit group receives the durability fail-stop and none is acked. Through the
// engine the membership of a group is a scheduling accident — the measured rate
// is 16 followers in 480 commits, so a run may produce a group of one — and an
// oracle that asserts a property of "the group" must first be able to CONSTRUCT
// a group of more than one member. [RunGroupCommitFailAll] therefore drives
// [wal.Writer] directly over a [SimDisk], holding the leader inside its fsync
// with [SimDisk.ArmSyncGateAt] while the followers arrive. That is the only way
// the multi-member group is deterministic rather than lucky.
//
// It is still an end-to-end assertion in the sense that matters: the real WAL
// writer, the real poison-and-truncate path, and a real recovery over the
// resulting byte image, with the fault delivered by the same SimDisk primitive
// the crash scenarios use.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The group-commit metric names, verified against store/wal/writer.go rather
// than taken from a description of it.
const (
	// metricSyncGroupCoalesced counts SyncGroup calls that returned durable
	// WITHOUT an fsync of their own — the follower fast path in
	// wal.Writer.syncToLocked.
	metricSyncGroupCoalesced = "store.wal.SyncGroup.coalesced"
	// metricSyncGroupLeader counts completed leader rounds: one increment per
	// successful group fsync (wal.Writer.leadGroupSyncLocked).
	metricSyncGroupLeader = "store.wal.SyncGroup.leader"
	// metricSyncGroupErrors counts SyncGroup calls that returned an error,
	// including every member failed by a leader's poison.
	metricSyncGroupErrors = "store.wal.SyncGroup.errors"
)

// groupCommitFloors are the coverage preconditions the non-vacuity gate holds a
// coalescing run to BEFORE any rate is adjudicated. A rate computed over a
// handful of commits is noise, and a rate computed over one committer is not a
// rate at all.
const (
	// groupCommitMinCommitters is the concurrency floor the task fixes: fewer
	// than this and the workload cannot be said to have offered the writer an
	// opportunity to coalesce.
	groupCommitMinCommitters = 8
	// groupCommitMinRounds is the floor on observed SyncGroup rounds (leaders +
	// followers). It is set well below what the configured workload produces
	// (measured: see the test) so it fails a run that stopped committing, not a
	// run that merely scheduled differently.
	groupCommitMinRounds = 64
)

// GroupCommitConfig parameterises a coalescing run.
type GroupCommitConfig struct {
	// Seed is the master seed for the workload and the disk sub-stream.
	Seed uint64
	// Connections is how many concurrent Bolt writer connections commit. The
	// coverage gate requires at least [groupCommitMinCommitters]; the control arm
	// deliberately sets 1 to prove the gate fires.
	Connections int
	// OpsPerConn is how many ops each connection issues (< 1 normalises to
	// groupCommitDefaultOps).
	OpsPerConn int
}

// groupCommitDefaultConns / groupCommitDefaultOps size the short-layer arm. They
// mirror the shape of the rmp #2347 measurement (12 concurrent writers) so the
// gate observes the same path that measurement did.
const (
	groupCommitDefaultConns = 12
	groupCommitDefaultOps   = 40
)

// GroupCommitEvidence is what a coalescing run OBSERVED. It holds measurements
// and no verdict, so a test can log what happened and the adjudicators can work
// on numbers rather than on a claim.
type GroupCommitEvidence struct {
	// Committers is the concurrency the run actually drove.
	Committers int
	// Coalesced is the follower count: SyncGroup calls that returned durable
	// without an fsync of their own.
	Coalesced uint64
	// Leaders is the number of completed group fsync rounds.
	Leaders uint64
	// Errors is the number of SyncGroup calls that failed.
	Errors uint64
	// Acked is how many commits the Bolt clients saw acknowledged, which is the
	// independent, engine-side count of durable commits — deliberately NOT
	// derived from the same metrics the gate reads.
	Acked int
}

// Rounds is the total number of SyncGroup calls the run observed: every call
// either coalesced onto another round, led one, or failed.
func (e GroupCommitEvidence) Rounds() uint64 { return e.Coalesced + e.Leaders + e.Errors }

// FollowerRate is the fraction of observed rounds that took the follower fast
// path. It is reported for evidence, never asserted against a threshold: the
// rate is a scheduling outcome and pinning it would be flaky.
func (e GroupCommitEvidence) FollowerRate() float64 {
	if e.Rounds() == 0 {
		return 0
	}
	return float64(e.Coalesced) / float64(e.Rounds())
}

// String renders the evidence for a failure message or a test log.
func (e GroupCommitEvidence) String() string {
	return fmt.Sprintf("group-commit: committers=%d rounds=%d leaders=%d followers=%d errors=%d acked=%d followerRate=%.4f",
		e.Committers, e.Rounds(), e.Leaders, e.Coalesced, e.Errors, e.Acked, e.FollowerRate())
}

// RunGroupCommitCoalescing drives cfg.Connections concurrent Bolt writer
// connections through a real WAL-backed store on a [SimDisk] and returns what
// the group-commit counters observed.
//
// It installs the recording metrics backend for the duration (the same
// test-side sink [MetricsOracle] uses) and restores the no-op default before
// returning. Because that sink is GLOBAL it must run SERIALLY: the caller must
// not run concurrent metrics-emitting work.
func RunGroupCommitCoalescing(ctx context.Context, cfg GroupCommitConfig) (GroupCommitEvidence, error) {
	if cfg.Connections < 1 {
		cfg.Connections = groupCommitDefaultConns
	}
	if cfg.OpsPerConn < 1 {
		cfg.OpsPerConn = groupCommitDefaultOps
	}

	disk := NewSimDisk(NewSeed(cfg.Seed^durableDiskSeedMix), 0) // faultRate 0: this arm injects nothing
	st, err := OpenSimStore(disk, durableStoreConfig())
	if err != nil {
		return GroupCommitEvidence{}, fmt.Errorf("sim: group-commit open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	srv, err := newSimServerWithLogger(st.Engine(), clock.Real(), quietSimLogger())
	if err != nil {
		return GroupCommitEvidence{}, fmt.Errorf("sim: group-commit server: %w", err)
	}

	// Install the sink AFTER the store and server are built, so the counters
	// carry the workload's rounds and not the WAL open's own sync.
	rb := newRecordingBackend()
	metrics.SetBackend(rb)
	defer metrics.SetBackend(nil)

	res, runErr := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        cfg.Seed,
		Connections: cfg.Connections,
		OpsPerConn:  cfg.OpsPerConn,
		Mix:         &ConcurrentMix{WriterWeight: durableWriters},
	})
	_ = srv.Close()
	if runErr != nil {
		return GroupCommitEvidence{}, fmt.Errorf("sim: group-commit concurrent run: %w", runErr)
	}

	return GroupCommitEvidence{
		Committers: cfg.Connections,
		Coalesced:  rb.counter(metricSyncGroupCoalesced),
		Leaders:    rb.counter(metricSyncGroupLeader),
		Errors:     rb.counter(metricSyncGroupErrors),
		Acked:      len(toSet(res.AckedNames)),
	}, nil
}

// checkGroupCommitNonVacuity is the SEPARATE coverage precondition, kept apart
// from [checkGroupCommitCoverage] for the reason rmp #2470 established: an
// uninformative run must not read as a faulty one. It asserts the population was
// non-trivial — enough concurrent committers to have an opportunity to coalesce,
// enough observed rounds for a rate to mean anything, and commits actually
// acknowledged — BEFORE any statement about coalescing is made.
//
// A violation here means the RUN proved nothing, not that the WRITER is broken.
func checkGroupCommitNonVacuity(e GroupCommitEvidence) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: "<group-commit non-vacuity>", Message: msg + " — " + e.String()})
	}
	if e.Committers < groupCommitMinCommitters {
		add(fmt.Sprintf("only %d concurrent committer(s), need >= %d: the workload never offered the writer an opportunity to coalesce",
			e.Committers, groupCommitMinCommitters))
	}
	if e.Rounds() < groupCommitMinRounds {
		add(fmt.Sprintf("only %d SyncGroup round(s), need >= %d: too few commits for the coalescing observation to carry any weight",
			e.Rounds(), groupCommitMinRounds))
	}
	if e.Acked == 0 {
		add("no commit was acknowledged: the workload did not exercise the durable write path at all")
	}
	return v
}

// checkGroupCommitCoverage is the coverage-precondition gate on group-commit
// coalescing: under concurrent committers the WAL must have coalesced at least
// one commit onto another's fsync.
//
// It is deliberately a FLOOR (`> 0`) and not a rate. How many followers a run
// produces is a scheduling outcome — the reference measurement is 16 in 480 —
// and an oracle that demanded a percentage would be flaky. What must never
// happen is the count going to ZERO under concurrency, because that is the
// signature of the regression this gate exists for: commits serialised again,
// every one paying its own fsync, and the fail-all branch un-covered.
//
// The caller must run [checkGroupCommitNonVacuity] first; this function assumes
// the population was already shown to be non-trivial and speaks only about the
// writer.
func checkGroupCommitCoverage(e GroupCommitEvidence) []Violation {
	if e.Coalesced > 0 {
		return nil
	}
	return []Violation{{
		Kind: ViolationOracleDeviation,
		Op:   "<group-commit coalescing>",
		Message: fmt.Sprintf(
			"no SyncGroup call took the follower fast path under %d concurrent committers: every commit paid its own fsync, "+
				"so group-commit coalescing has regressed to solo-leader and the fail-all branch is no longer covered end to end — %s",
			e.Committers, e.String()),
	}}
}

// -----------------------------------------------------------------------------
// The fail-all arm
// -----------------------------------------------------------------------------

// groupCommitFailAllPath is the WAL the fail-all arm writes on the SimDisk.
const groupCommitFailAllPath = "group_commit_failall.wal"

// groupCommitSettle is how long the arm lets the followers reach their park
// point after each has entered SyncGroup. The assertions do not DEPEND on them
// being parked — a member that has not parked yet reads the sticky poison and
// fails identically — so this only widens the window in which the interesting
// interleaving occurs; it is not a synchronisation device.
const groupCommitSettle = 20 * time.Millisecond

// groupCommitFailAllMembers is how many committers form the group. It is
// comfortably above the 1 that would make "fail-all" vacuous.
const groupCommitFailAllMembers = 8

// groupCommitFailAllFsyncs is how many Sync calls the disk sees for one FAILED
// group, and it is 2 rather than the 1 an "one group, one fsync" oracle would
// predict. MEASURED, then traced to the source rather than explained away:
//
//  1. the leader's group fsync, which returns the armed fault; and
//  2. a second fsync issued by [wal.Writer.poison] AFTER it truncates the
//     un-synced suffix, so the REDUCED file size reaches stable storage — without
//     it a host crash could revert the file to its pre-truncation length and
//     resurrect the discarded frames.
//
// The count is still the cross-check it was meant to be: it is independent of
// the WAL's own counters, and a member that had led a round of its own would
// push it to 3 or more. What it is not is a count of group rounds — that is
// [GroupCommitFailAllResult.PoisonedRounds].
const groupCommitFailAllFsyncs = 2

// GroupCommitFailAllResult is what the fail-all arm observed: the per-member
// outcome of one commit group whose shared fsync failed.
type GroupCommitFailAllResult struct {
	// Members is how many committers were in the group.
	Members int
	// Acked is how many members' SyncGroup returned nil. It MUST be zero: the
	// shared fsync failed, so no member may believe it committed.
	Acked int
	// Failed is how many members received an error.
	Failed int
	// DurabilityClass is how many of those errors satisfy
	// errors.Is(err, wal.ErrDurabilityFailed) — the class a member needs to tell
	// a durability fail-stop from a conflict of its own.
	DurabilityClass int
	// Leaders is store.wal.SyncGroup.leader over the arm: completed leader
	// rounds. A failed round completes none, so a genuine single-group fail-all
	// leaves this at zero.
	Leaders uint64
	// PoisonedRounds is wal.Stats.SyncFailed: how many group rounds ended in a
	// poison. Exactly ONE, for a group of many members, is the direct statement
	// that they shared a single leader's fsync rather than each paying its own.
	PoisonedRounds uint64
	// FsyncAttempts is the number of Sync calls the SimDisk saw after arming,
	// counted independently of the WAL's own counters. It is expected to be
	// groupCommitFailAllFsyncs — see that constant for the decomposition, which
	// was MEASURED rather than assumed.
	FsyncAttempts int64
	// GateFired reports that the rendezvous actually held the leader inside its
	// fsync. Without it the members would have serialised and there would have
	// been no group to fail.
	GateFired bool
	// PriorDurable / GroupDurable are what recovery found afterwards: the frames
	// of the commit acknowledged BEFORE the group (which must survive) and of the
	// group itself (which must be gone).
	PriorDurable, GroupDurable int
}

// String renders the result for a failure message or a test log.
func (r GroupCommitFailAllResult) String() string {
	return fmt.Sprintf("group-commit fail-all: members=%d acked=%d failed=%d durabilityClass=%d leaders=%d poisonedRounds=%d fsyncAttempts=%d gateFired=%t priorDurable=%d groupDurable=%d",
		r.Members, r.Acked, r.Failed, r.DurabilityClass, r.Leaders, r.PoisonedRounds, r.FsyncAttempts, r.GateFired, r.PriorDurable, r.GroupDurable)
}

// RunGroupCommitFailAll constructs one genuine multi-member commit group whose
// shared fsync fails, and reports what every member saw.
//
// The protocol is deterministic by construction:
//
//  1. One frame is appended and synced successfully — the PRIOR commit, which
//     recovery must still find afterwards.
//  2. Every member appends its frame, so all of them are buffered before any
//     SyncGroup runs and a single leader's flush covers them all.
//  3. Member 0 calls SyncGroup and becomes the leader. Its fsync is both GATED
//     (so it stays inside the call) and armed to FAIL.
//  4. Once the gate reports the leader is parked inside the fsync, members 1..n-1
//     call SyncGroup and find a leader active.
//  5. The gate is released, the leader's fsync returns the fault, the writer
//     poisons and discards the whole un-synced suffix, and every member fails.
//
// Step 3 is why [SimDisk.ArmSyncGateAt] exists: against an in-memory disk the
// leader's fsync window is otherwise far too short for a follower to reach it.
func RunGroupCommitFailAll(ctx context.Context, seed uint64) (GroupCommitFailAllResult, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0: only the armed one-shot fault fires
	w, err := wal.OpenFS(simWALFS{disk: disk}, groupCommitFailAllPath)
	if err != nil {
		return GroupCommitFailAllResult{}, fmt.Errorf("sim: fail-all open WAL: %w", err)
	}

	// (1) A prior commit, durably acknowledged before the group exists. It is the
	// control for the other half of the poison contract: the failed suffix is
	// discarded, but everything already acknowledged stays.
	priorMark, err := w.AppendRun(func(emit func([]byte) error) error { return emit(groupCommitFrame(0xC0)) })
	if err != nil {
		return GroupCommitFailAllResult{}, fmt.Errorf("sim: fail-all prior append: %w", err)
	}
	if serr := w.SyncGroup(priorMark); serr != nil {
		return GroupCommitFailAllResult{}, fmt.Errorf("sim: fail-all prior sync: %w", serr)
	}

	// (2) Buffer every member's frames BEFORE any sync, so the leader's flush
	// snapshot covers all of them: on success this one fsync would acknowledge
	// the whole group, which is what makes it one group.
	marks := make([]int64, groupCommitFailAllMembers)
	for m := range marks {
		mark, aerr := w.AppendRun(func(emit func([]byte) error) error { return emit(groupCommitFrame(byte(0xA0 + m))) })
		if aerr != nil {
			return GroupCommitFailAllResult{}, fmt.Errorf("sim: fail-all append %d: %w", m, aerr)
		}
		marks[m] = mark
	}

	// Arm the fault FIRST (it resets the Sync counter) and the gate second, both
	// on the next Sync — the leader's. See [SimDisk.ArmSyncGateAt] on ordering.
	disk.ArmSyncFaultAt(1)
	gate := disk.ArmSyncGateAt(1)

	rb := newRecordingBackend()
	metrics.SetBackend(rb)
	defer metrics.SetBackend(nil)

	var acked, failed, durClass atomic.Int64
	commit := func(mark int64) {
		serr := w.SyncGroup(mark)
		if serr == nil {
			acked.Add(1)
			return
		}
		failed.Add(1)
		if errors.Is(serr, wal.ErrDurabilityFailed) {
			durClass.Add(1)
		}
	}

	var wg sync.WaitGroup
	// (3) The leader. It blocks inside its fsync until the gate is released.
	wg.Add(1)
	go func() {
		defer wg.Done()
		commit(marks[0])
	}()

	// (4) Wait for the leader to be parked INSIDE the fsync before the followers
	// arrive, so they find leaderActive set. This is a real rendezvous, not a
	// sleep: the gate closes its channel from inside the gated Sync.
	select {
	case <-gate.Reached():
	case <-ctx.Done():
		gate.Release()
		wg.Wait()
		_ = w.Close()
		return GroupCommitFailAllResult{}, ctx.Err()
	}

	var entered sync.WaitGroup
	entered.Add(groupCommitFailAllMembers - 1)
	for m := 1; m < groupCommitFailAllMembers; m++ {
		wg.Add(1)
		go func(m int) {
			defer wg.Done()
			entered.Done()
			commit(marks[m])
		}(m)
	}
	// Every follower goroutine has been scheduled and is about to call (or is
	// already inside) SyncGroup. Give them a bounded moment to reach the park
	// point; see [groupCommitSettle] on why the assertions do not depend on it.
	entered.Wait()
	time.Sleep(groupCommitSettle)

	// (5) Release the leader into its failing fsync.
	gate.Release()
	wg.Wait()

	res := GroupCommitFailAllResult{
		Members:         groupCommitFailAllMembers,
		Acked:           int(acked.Load()),
		Failed:          int(failed.Load()),
		DurabilityClass: int(durClass.Load()),
		Leaders:         rb.counter(metricSyncGroupLeader),
		PoisonedRounds:  uint64(w.Stats().SyncFailed),
		FsyncAttempts:   disk.SyncCount(),
		GateFired:       gate.Fired(),
	}

	// The writer is poisoned; Close is best-effort on its error path.
	_ = w.Close()

	prior, group, rerr := groupCommitRecoveredFrames(disk)
	if rerr != nil {
		return res, fmt.Errorf("sim: fail-all recovery: %w", rerr)
	}
	res.PriorDurable, res.GroupDurable = prior, group
	return res, nil
}

// groupCommitFrame builds one member's fixed-size frame payload, tagged so the
// recovery census can tell the prior commit's frame from the group's.
func groupCommitFrame(tag byte) []byte {
	f := make([]byte, 16)
	for i := range f {
		f[i] = tag
	}
	return f
}

// groupCommitRecoveredFrames reopens the WAL image and counts the frames that
// survived, split by tag: the prior commit's (0xC0) and the group's (0xA0+).
// It is the durable-image half of the fail-all contract — the poison must have
// discarded the whole failed suffix without touching what was acknowledged
// before it.
func groupCommitRecoveredFrames(disk *SimDisk) (prior, group int, err error) {
	rh, err := disk.OpenFile(groupCommitFailAllPath, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open for read: %w", err)
	}
	defer func() { _ = rh.Close() }()
	r := wal.NewReader(rh, rh)
	for f := range r.Frames() {
		if len(f.Payload) == 0 {
			continue
		}
		if f.Payload[0] == 0xC0 {
			prior++
			continue
		}
		group++
	}
	// A torn tail is not an error here: the poison TRUNCATED the failed suffix,
	// so a clean EOF is what a correct fail-all leaves behind, and anything else
	// is reported so it cannot be mistaken for a clean image.
	if terr := r.TailError(); terr != nil {
		return prior, group, fmt.Errorf("WAL tail at offset %d: %w", r.TailOffset(), terr)
	}
	return prior, group, nil
}

// checkGroupCommitFailAll adjudicates the fail-all contract over one group whose
// shared fsync failed. It is a PURE function of the observed result, which is
// what lets [TestGroupCommit_FailAllArmDetectsAWronglyAckedMember] falsify it by
// handing it a doctored result rather than by hoping a real run misbehaves.
//
// The clauses, in the order a failure is most usefully read:
//
//   - the arm actually built a group (the gate fired, every member ran);
//   - NO member was acknowledged — the property fail-all exists to provide;
//   - EVERY member failed, and with the durability CLASS, so a managed
//     transaction can tell this from a conflict of its own (rmp #2306);
//   - the group was ONE group: one fsync attempt, no completed leader round;
//   - recovery kept the previously acknowledged commit and discarded the whole
//     failed group.
func checkGroupCommitFailAll(r *GroupCommitFailAllResult) []Violation {
	var v []Violation
	add := func(kind ViolationKind, msg string) {
		v = append(v, Violation{Kind: kind, Op: "<group-commit fail-all>", Message: msg + " — " + r.String()})
	}

	if !r.GateFired {
		add(ViolationVacuousRun,
			"the sync gate never fired: the leader was not held inside its fsync, so no multi-member group was constructed and this run proves nothing about fail-all")
	}
	if r.Members < 2 {
		add(ViolationVacuousRun,
			fmt.Sprintf("a group of %d member(s) cannot demonstrate fail-all", r.Members))
	}
	if r.Acked != 0 {
		add(ViolationACIDDurability, fmt.Sprintf(
			"%d group member(s) were acknowledged although the group's shared fsync FAILED: the un-synced suffix was discarded, "+
				"so an acknowledged member's transaction is durable to nobody and would be lost after recovery", r.Acked))
	}
	if r.Failed != r.Members {
		add(ViolationACIDDurability, fmt.Sprintf(
			"%d of %d members failed; every member of a group whose fsync failed must fail", r.Failed, r.Members))
	}
	if r.DurabilityClass != r.Failed {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d of %d failures did not carry wal.ErrDurabilityFailed: a member that cannot identify a durability fail-stop "+
				"would retry against a poisoned writer (rmp #2306)", r.Failed-r.DurabilityClass, r.Failed))
	}
	if r.PoisonedRounds != 1 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d poisoned group round(s); expected exactly 1 — %d members did not share a single leader's fsync, "+
				"so this run tested the sticky poison rather than group fail-all", r.PoisonedRounds, r.Members))
	}
	if r.FsyncAttempts != groupCommitFailAllFsyncs {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d fsync attempt(s) on the disk; expected %d (the leader's failed group fsync plus the poison's post-truncate "+
				"metadata fsync) — a member appears to have led an fsync of its own", r.FsyncAttempts, groupCommitFailAllFsyncs))
	}
	if r.Leaders != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d leader round(s) completed although the only fsync failed", r.Leaders))
	}
	if r.PriorDurable != 1 {
		add(ViolationACIDDurability, fmt.Sprintf(
			"recovery found %d frame(s) of the commit acknowledged BEFORE the group; expected 1 — the poison discarded more than the failed suffix", r.PriorDurable))
	}
	if r.GroupDurable != 0 {
		add(ViolationACIDAtomicity, fmt.Sprintf(
			"recovery found %d frame(s) of the FAILED group; expected 0 — a transaction no client was told had committed survived", r.GroupDurable))
	}
	return v
}

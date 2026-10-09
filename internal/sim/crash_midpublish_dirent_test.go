package sim

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"
	"testing"
	"time"
)

// This file is the LOAD-BEARING form of the #1819 dirent-revocation guard
// (rmp #1827): a host crash that lands while the checkpointer is genuinely
// INSIDE the snapshot publish, driven through the simulator's own integrated
// crash trigger [Simulator.maybeCrash].
//
// The synthetic guards in crash_integrated_dirent_test.go plant a probe dirent
// because the in-loop ordering (maybeCheckpoint, then maybeCrash, with a
// synchronous checkpoint) can never crash mid-publish. This scenario reaches
// the window without an asynchronous checkpointer: the synchronous checkpoint
// runs on its own goroutine and is PARKED inside the fsync of a staged
// component by a [SyncGate] (the disk lock is released while it is parked), and
// the host crash is then taken through maybeCrash. [SimDisk.CrashHost] is
// designed for exactly this interleaving: the parked fsync's generation stamp
// stops it granting durability after the crash.
//
// What a crash at that instant must do, and what the scenario asserts:
//
//   - the half-published staging directory <dir>/snapshot.tmp — created by
//     MkdirAll and never made durable, because nothing fsyncs <dir> before the
//     publish rename — is revoked together with every component staged in it;
//   - recovery falls back to the prior durable snapshot (its manifest is
//     byte-identical before and after) plus the WAL suffix (WAL ops replayed > 0),
//     with no acknowledged op lost ([InvariantChecker.CheckDurability] inside
//     maybeCrash).
//
// Reach: the gate covers the staging phase only. The archive rename, publish
// rename and parent-directory fsync issue no file Sync, so no gate can park the
// publish there; that window is covered by the fault-then-crash scenario in
// checkpoint_crash_storm.go (rmp #2465).
//
// The synthetic guards are kept: TestSimulator_IntegratedCrashRevocationIsSelective
// pins a different property (durable names survive), and the wiring guard does
// not depend on how many Syncs the publish issues.

// midPublishSeedMix decorrelates the gate-ordinal draw from every other
// sub-stream of the run seed.
const midPublishSeedMix uint64 = 0x1827_D1E7_9B0C_4A11

// midPublishJoinBudget bounds the teardown join of the parked checkpoint. It is
// a watchdog: the join returns as soon as the aborted checkpoint does.
const midPublishJoinBudget = 30 * time.Second

// midPublishFingerprint is what one run MEASURED, compared across same-seed
// runs for reproducibility. The manifest bytes are deliberately absent: the
// manifest carries a wall-clock CreatedAt.
type midPublishFingerprint struct {
	ordinal    int64
	staged     []string
	walOps     int
	liveNodes  int
	crashCount int
}

// runMidPublishCrash drives one mid-publish crash for seed and returns what it
// measured. Every failure is fatal to t.
func runMidPublishCrash(t *testing.T, seed uint64) midPublishFingerprint {
	t.Helper()
	const (
		snapDir = defaultCheckpointDir + "/" + simSnapshotName
		tmpDir  = snapDir + ".tmp"
		bakDir  = snapDir + ".bak"
	)
	// Small workload: one in-loop checkpoint at tick 20 publishes the prior
	// durable snapshot, and ticks 20..30 leave a WAL suffix past it. Crashes are
	// off for the loop; the single crash below is constructed.
	s, err := New(Config{
		Seed:       seed,
		MaxTicks:   30,
		CheckEvery: 1,
		Workload:   WriteHeavyWorkload(NewSeed(seed)),
		Checkpoint: CheckpointConfig{Enabled: true, Every: 20},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if report, err := s.Run(context.Background()); err != nil || report != nil {
		t.Fatalf("pre-crash run: err=%v report=%v", err, report)
	}
	if s.CheckpointCount() == 0 {
		t.Fatal("no prior checkpoint was published: there is no durable snapshot to fall back to")
	}
	disk := s.Disk()

	priorManifest, err := disk.ReadFile(snapDir + "/manifest.json")
	if err != nil {
		t.Fatalf("read prior manifest: %v", err)
	}
	// K = the component files of the prior snapshot. The next publish fsyncs one
	// staged file per component (manifest last), so every ordinal in [1, K]
	// parks it inside the staging write; the pre-crash checks below verify it.
	liveEnts, err := disk.ReadDir(snapDir)
	if err != nil {
		t.Fatalf("read prior snapshot dir: %v", err)
	}
	k := 0
	for _, e := range liveEnts {
		if !e.IsDir() {
			k++
		}
	}
	if k == 0 {
		t.Fatal("prior snapshot has no component files")
	}
	ordinal := int64(1 + NewSeed(seed^midPublishSeedMix).IntN(k))

	gate := disk.ArmSyncGateAt(ordinal)
	done := make(chan error, 1)
	// Teardown, registered BEFORE the checkpoint starts so it runs on every exit
	// path. Order is load-bearing:
	//  1. disarm a gate that was never reached, so no later Sync — the store's
	//     Close flush — can park on it (the cause of the feasibility probe's
	//     hang); a reached gate is already consumed and this is a no-op;
	//  2. close the recovered store, so the parked publish never shares the disk
	//     with a live store;
	//  3. arm faults so the parked publish aborts at its next Sync or at the
	//     staging DirSync instead of completing on the discarded disk;
	//  4. release the gate (idempotent) and join with a bounded wait.
	started := false
	t.Cleanup(func() {
		disk.ArmSyncGateAt(0)
		_ = s.Close()
		disk.ArmSyncFaultAt(1)
		disk.ArmDirSyncFaultForPath(tmpDir)
		gate.Release()
		if !started {
			return
		}
		select {
		case <-done:
		case <-time.After(midPublishJoinBudget):
			t.Errorf("parked checkpoint did not return within %v after release", midPublishJoinBudget)
		}
	})
	started = true
	go func() { done <- s.store.Checkpoint() }()

	select {
	case <-gate.Reached():
	case err := <-done:
		t.Fatalf("checkpoint finished (err=%v) without reaching Sync ordinal %d: the crash would not land mid-publish", err, ordinal)
	case <-time.After(midPublishJoinBudget):
		t.Fatalf("checkpoint did not reach Sync ordinal %d within %v", ordinal, midPublishJoinBudget)
	}

	// Pre-crash: the publish is parked INSIDE the staging write — staged
	// components exist, the live snapshot was not archived, and it is unchanged.
	stagedEnts, err := disk.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("staging dir %s absent while the publish is parked (ordinal %d): %v", tmpDir, ordinal, err)
	}
	staged := make([]string, 0, len(stagedEnts))
	for _, e := range stagedEnts {
		staged = append(staged, e.Name())
	}
	if len(staged) == 0 {
		t.Fatalf("staging dir %s is empty at ordinal %d: the gate did not park the staging write", tmpDir, ordinal)
	}
	if disk.Exists(bakDir + "/manifest.json") {
		t.Fatalf("live snapshot already archived at ordinal %d: the gate parked past the staging phase", ordinal)
	}
	if cur, err := disk.ReadFile(snapDir + "/manifest.json"); err != nil || !bytes.Equal(cur, priorManifest) {
		t.Fatalf("live manifest changed before the crash (err=%v)", err)
	}

	// The crash, through the integrated trigger: a schedule that fires now.
	s.crash = NewCrashSchedule(NewSeed(seed^crashSeedMix), CrashConfig{Enabled: true, CrashProb: 1.0})
	// maybeCrash crashes AND reopens, and recovery unconditionally drops a stale
	// staging dir (store/recovery/recovery.go, "stale staging cleanup"), so the
	// dir's absence after the call proves nothing on its own. The load-bearing
	// observable is whether that cleanup FOUND anything to unlink: a removal
	// counts as a hit only when it unlinked an existing name, so a zero delta
	// means the crash itself had already revoked the staging dir.
	tmpHitsBefore := disk.RemoveHitCountForPath(tmpDir)
	report, err := s.maybeCrash(context.Background(), int64(s.cfg.MaxTicks)+1)
	if err != nil {
		t.Fatalf("maybeCrash: %v", err)
	}
	if report != nil {
		t.Fatalf("durability violated across the mid-publish crash:\n%s", report)
	}
	if s.CrashCount() != 1 {
		t.Fatalf("CrashCount=%d, want 1", s.CrashCount())
	}

	// Revocation: the host crash dropped the half-published staging directory,
	// so recovery's cleanup had nothing left to unlink…
	if hits := disk.RemoveHitCountForPath(tmpDir) - tmpHitsBefore; hits != 0 {
		t.Fatalf("staging dir %s survived the host crash: recovery's cleanup unlinked it %d time(s); "+
			"the crash must revoke a never-fsync'd subdirectory dirent (staged=%v)", tmpDir, hits, staged)
	}
	// …and nothing of it is left after recovery.
	if _, err := disk.ReadDir(tmpDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("half-published staging dir %s survived the host crash (ReadDir err=%v); staged=%v", tmpDir, err, staged)
	}
	for _, name := range staged {
		if disk.Exists(tmpDir + "/" + name) {
			t.Fatalf("staged component %s/%s survived the host crash", tmpDir, name)
		}
	}

	// Fallback: the prior durable snapshot is the live one, untouched, with no
	// backup left behind, and the WAL suffix past it was replayed on top.
	cur, err := disk.ReadFile(snapDir + "/manifest.json")
	if err != nil {
		t.Fatalf("live snapshot missing after recovery: %v", err)
	}
	if !bytes.Equal(cur, priorManifest) {
		t.Fatal("live manifest after recovery differs from the prior durable snapshot's")
	}
	if disk.Exists(bakDir + "/manifest.json") {
		t.Fatalf("a backup %s exists after recovery", bakDir)
	}
	walOps := s.store.WALOps()
	if walOps == 0 {
		t.Fatal("recovery replayed no WAL op: the suffix past the prior snapshot was not exercised")
	}

	return midPublishFingerprint{
		ordinal:    ordinal,
		staged:     staged,
		walOps:     walOps,
		liveNodes:  len(liveNodeIDs(s.store.Graph())),
		crashCount: s.CrashCount(),
	}
}

// TestSimulator_MidPublishCrashRevokesStagingAndFallsBack crashes the host while
// the checkpoint is parked inside the snapshot publish, over several seeds so
// several staging ordinals are covered.
func TestSimulator_MidPublishCrashRevokesStagingAndFallsBack(t *testing.T) {
	for _, seed := range []uint64{0x1827, 0x1828, 0x1829, 0x182A} {
		fp := runMidPublishCrash(t, seed)
		t.Logf("seed=%#x ordinal=%d staged=%v walOps=%d nodes=%d", seed, fp.ordinal, fp.staged, fp.walOps, fp.liveNodes)
	}
}

// TestSimulator_MidPublishCrashReproducible pins same-seed reproducibility of
// the mid-publish crash: the gate ordinal, the staged set at the crash, the
// replayed WAL ops and the recovered node count are identical across two runs.
func TestSimulator_MidPublishCrashReproducible(t *testing.T) {
	const seed = 0x1827
	a := runMidPublishCrash(t, seed)
	b := runMidPublishCrash(t, seed)
	if a.ordinal != b.ordinal || !slices.Equal(a.staged, b.staged) || a.walOps != b.walOps ||
		a.liveNodes != b.liveNodes || a.crashCount != b.crashCount {
		t.Fatalf("mid-publish crash not reproducible for seed %#x:\n run A %+v\n run B %+v", seed, a, b)
	}
}

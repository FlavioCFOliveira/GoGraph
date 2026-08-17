package sim

// mvcc_clock_recovery_test.go — validation of rmp #2469: the MVCC clock and the
// transaction sequence reconciled against the durable record across a SNAPSHOT
// boundary, plus the sensitivity arms that prove each oracle can fail.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// mvccClockCommit commits one single-node transaction through the store's own
// engine, so the commit takes the real MVCC path and leaves a real durable
// marker.
func mvccClockCommit(t *testing.T, st *SimStore, name string) {
	t.Helper()
	res, err := NewEngineAdapter(st.Engine()).RunWrite(context.Background(),
		tmplCreatePerson, map[string]any{"name": name, "age": int64(1)})
	if err != nil {
		t.Fatalf("commit %q: %v", name, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("commit %q close: %v", name, err)
	}
}

// mvccClockManifestPath is the published manifest of a full-stack sim store.
func mvccClockManifestPath(cfg simStoreConfig) string {
	return cfg.dir + "/" + simSnapshotName + "/manifest.json"
}

// rewriteManifestInstant replaces the published manifest with one whose
// recorded capture instant is instant, re-framed through the real writer so the
// integrity trailer stays valid (rmp #2520) — the point is a manifest that
// RECOVERY ACCEPTS and that says something different, not a corrupt one. It
// returns the instant the manifest carried before the rewrite.
func rewriteManifestInstant(t *testing.T, disk *SimDisk, cfg simStoreConfig, instant uint64) uint64 {
	t.Helper()
	path := mvccClockManifestPath(cfg)
	original, err := disk.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	man, err := snapshot.LoadManifest(bytes.NewReader(original))
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	was := man.CommitTS
	man.CommitTS = instant
	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, man); err != nil {
		t.Fatalf("re-frame manifest: %v", err)
	}
	if err := snapshotWriteFile(disk, path, buf.Bytes()); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return was
}

// TestMVCCClockRecovery_PureSnapshotFloorIsTheManifestInstant is the case rmp
// #2469 exists for: the WAL prefix that carried the early timestamps has been
// truncated to nothing, so the only durable record of them is the instant the
// manifest recorded, and the recovered clock floor must be reconciled against
// it. Every number is measured from the durable image.
func TestMVCCClockRecovery_PureSnapshotFloorIsTheManifestInstant(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	disk := NewSimDisk(NewSeed(2469), 0)
	cfg := fullStackStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := range 6 {
		mvccClockCommit(t, st, fmt.Sprintf("mvcc-clock-pre-%d", i))
	}

	st2, boundary, err := crossSnapshotBoundaryOn(disk, st, "clock floor crossing")
	if err != nil {
		t.Fatalf("cross the snapshot boundary: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if v := checkSnapshotSourcedRecovery(0, boundary); len(v) != 0 {
		t.Fatalf("the crossing was not snapshot-sourced: %v (%s)", v, boundary.summary())
	}

	ev, err := measureMVCCRecovery(disk, st2, "pure-snapshot floor")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if !ev.pureSnapshot() {
		t.Fatalf("the reopen was not a pure-snapshot recovery: %s", ev.summary())
	}
	// The DERIVED maximum is the sharp reading: with an empty WAL it can only
	// have come from the manifest, so it must be the recorded instant exactly.
	// The floor is then at least one past it — but only "at least", because
	// rehydrating the image mints instants of its own (measured at about three
	// per restored node), which is why the derived maximum is asserted and not
	// only the floor.
	if ev.recoveredMaxTS != ev.snapshotInstant {
		t.Fatalf("recovery derived maximum instant %d from an image recording %d: %s",
			ev.recoveredMaxTS, ev.snapshotInstant, ev.summary())
	}
	if ev.clockFloor < ev.snapshotInstant+1 {
		t.Fatalf("clock floor = %d, want at least %d (manifest instant %d + 1): %s",
			ev.clockFloor, ev.snapshotInstant+1, ev.snapshotInstant, ev.summary())
	}
	if err := ev.observePostRecoveryCommits(ctx, disk, st2, mvccPostRecoveryBeacons); err != nil {
		t.Fatalf("post-recovery commits: %v", err)
	}
	if v := checkMVCCRecovery(0, &ev); len(v) != 0 {
		t.Fatalf("violations after a pure-snapshot recovery: %v\n%s", v, ev.summary())
	}
	t.Logf("%s", boundary.summary())
	t.Logf("%s", ev.summary())
	for i, m := range ev.post {
		t.Logf("post-recovery commit %d: seq=%d instant=%d", i, m.seq, m.ts)
	}
}

// TestMVCCClockRecovery_FloorTracksTheRecordedInstant is the derivation probe:
// the same code path, one field of the durable record changed. A manifest
// bumped to an instant no graph in this run ever minted must raise the floor to
// match — which is only possible if the floor really is read from that field.
func TestMVCCClockRecovery_FloorTracksTheRecordedInstant(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	const bumped = uint64(1) << 40

	disk := NewSimDisk(NewSeed(24691), 0)
	cfg := fullStackStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := range 4 {
		mvccClockCommit(t, st, fmt.Sprintf("mvcc-bump-%d", i))
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	captured := rewriteManifestInstant(t, disk, cfg, bumped)
	if captured == 0 || captured >= bumped {
		t.Fatalf("the published manifest recorded instant %d: the bump to %d proves nothing", captured, bumped)
	}
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	ev, err := measureMVCCRecovery(disk, st2, "bumped manifest instant")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if !ev.pureSnapshot() {
		t.Fatalf("the reopen was not a pure-snapshot recovery: %s", ev.summary())
	}
	if ev.clockFloor < bumped {
		t.Fatalf("clock floor = %d for a manifest recording instant %d: the recorded instant is NOT what the "+
			"floor is derived from, so the pure-snapshot oracle would pass on a coincidence: %s",
			ev.clockFloor, bumped, ev.summary())
	}
	if err := ev.observePostRecoveryCommits(ctx, disk, st2, 2); err != nil {
		t.Fatalf("post-recovery commits: %v", err)
	}
	if v := checkMVCCRecovery(0, &ev); len(v) != 0 {
		t.Fatalf("violations over a bumped-but-honoured manifest: %v\n%s", v, ev.summary())
	}
	t.Logf("captured instant %d, manifest rewritten to %d, recovered floor %d, first post-recovery instant %d",
		captured, bumped, ev.clockFloor, ev.post[0].ts)
}

// TestMVCCClockRecovery_StaleInstantFiresTheFloorOracle is the sensitivity arm
// of the pure-snapshot clauses: a manifest that LOSES its recorded instant (the
// pre-rmp #2309 shape, and what rmp #2467 measured a `commit_ts` key flip
// producing silently) leaves the reopened graph re-minting instants the image
// already contains. Adjudicated against the instant the image was really
// captured at, the oracle must fire.
//
// The shape is deliberate: ONE node updated many times, so the graph's instant
// runs far ahead of its size. Rehydrating an image mints about three instants
// per restored node, so a WIDE graph lifts its own clock past its recorded
// instant whatever the manifest says — and this defect would be MASKED. That is
// the measurement that made the derived-maximum clause necessary.
func TestMVCCClockRecovery_StaleInstantFiresTheFloorOracle(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	disk := NewSimDisk(NewSeed(24692), 0)
	cfg := fullStackStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mvccClockCommit(t, st, "mvcc-stale-subject")
	adapter := NewEngineAdapter(st.Engine())
	for i := range 40 {
		res, err := adapter.RunWrite(ctx, "MATCH (n:Person {name:$name}) SET n.age = $v",
			map[string]any{"name": "mvcc-stale-subject", "v": int64(i)})
		if err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("update %d close: %v", i, err)
		}
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	captured := rewriteManifestInstant(t, disk, cfg, 0) // drop it, keeping the trailer valid
	if captured == 0 {
		t.Fatal("the published manifest recorded no instant: dropping it proves nothing")
	}
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	ev, err := measureMVCCRecovery(disk, st2, "stale snapshot instant")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if err := ev.observePostRecoveryCommits(ctx, disk, st2, mvccPostRecoveryBeacons); err != nil {
		t.Fatalf("post-recovery commits: %v", err)
	}
	// The manifest no longer says when the image was taken, so the oracle is fed
	// the instant the test knows it was taken at.
	ev.snapshotInstant, ev.hasSnapshot = captured, true

	v := checkMVCCRecovery(0, &ev)
	if len(v) == 0 {
		t.Fatalf("the floor oracle did not fire although the recorded instant was dropped: %s", ev.summary())
	}
	var floorClause, derivedClause, remintClause bool
	for _, x := range v {
		switch {
		case strings.Contains(x.Message, "clock floor after a PURE-SNAPSHOT recovery"):
			floorClause = true
		case strings.Contains(x.Message, "derived a durable maximum"):
			derivedClause = true
		case strings.Contains(x.Message, "minted again"):
			remintClause = true
		}
	}
	if !floorClause {
		t.Fatalf("no pure-snapshot floor finding (floor %d, instant %d): %v", ev.clockFloor, captured, v)
	}
	if !derivedClause {
		t.Fatalf("no derived-maximum finding (derived %d, instant %d): %v", ev.recoveredMaxTS, captured, v)
	}
	if !remintClause {
		t.Fatalf("no re-minted-instant finding although the floor collapsed to %d below instant %d: %v",
			ev.clockFloor, captured, v)
	}
	t.Logf("captured instant %d, manifest instant dropped, recovered floor %d, first post-recovery instant %d",
		captured, ev.clockFloor, ev.post[0].ts)
}

// TestMVCCClockRecovery_SequenceResumesAcrossTheBoundary is the transaction-
// sequence half, run as an A/B over the ONE seam that decides it: the same
// workload, the same crash, the same recovery — with and without the reopened
// store seeding [txn.Options.ResumeTxnSeq] from what recovery derived.
//
// Unseeded, the store restarts at 0 and one WAL image ends up holding two
// different transactions under one sequence number (rmp #2302). The oracle must
// separate the two arms; if it does not, it is measuring nothing.
func TestMVCCClockRecovery_SequenceResumesAcrossTheBoundary(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	run := func(t *testing.T, noResume bool) mvccRecoveryEvidence {
		t.Helper()
		disk := NewSimDisk(NewSeed(24693), 0)
		cfg := fullStackStoreConfig()
		cfg.noResumeTxnSeq = noResume

		st, err := OpenSimStore(disk, cfg)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		for i := range 3 {
			mvccClockCommit(t, st, fmt.Sprintf("mvcc-seq-pre-%d", i))
		}
		// The checkpoint folds those three and empties the WAL; the commits that
		// follow are the TAIL a restarted sequence would collide with.
		if err := st.Checkpoint(); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		for i := range 4 {
			mvccClockCommit(t, st, fmt.Sprintf("mvcc-seq-tail-%d", i))
		}
		st.Crash()

		st2, err := OpenSimStore(disk, cfg)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer func() { _ = st2.Close() }()
		ev, err := measureMVCCRecovery(disk, st2, fmt.Sprintf("sequence resume (noResume=%t)", noResume))
		if err != nil {
			t.Fatalf("measure: %v", err)
		}
		if !ev.snapshotPlusWALTail() {
			t.Fatalf("the reopen did not replay a WAL tail over a snapshot: %s", ev.summary())
		}
		if ev.imageMaxSeq == 0 {
			t.Fatalf("the surviving image carries no transaction sequence: %s", ev.summary())
		}
		if err := ev.observePostRecoveryCommits(ctx, disk, st2, mvccPostRecoveryBeacons); err != nil {
			t.Fatalf("post-recovery commits: %v", err)
		}
		return ev
	}

	t.Run("seeded", func(t *testing.T) {
		ev := run(t, false)
		if ev.resumedTxnSeq != ev.imageMaxSeq {
			t.Fatalf("resumed from sequence %d, want the image maximum %d: %s",
				ev.resumedTxnSeq, ev.imageMaxSeq, ev.summary())
		}
		if v := checkMVCCRecovery(0, &ev); len(v) != 0 {
			t.Fatalf("violations with the sequence resumed: %v\n%s", v, ev.summary())
		}
		t.Logf("%s; post-recovery sequences start at %d", ev.summary(), ev.post[0].seq)
	})

	t.Run("unseeded", func(t *testing.T) {
		ev := run(t, true)
		if ev.resumedTxnSeq != 0 {
			t.Fatalf("the sensitivity seam did not disarm the resume: resumed from %d", ev.resumedTxnSeq)
		}
		v := checkMVCCRecovery(0, &ev)
		if len(v) == 0 {
			t.Fatalf("the sequence oracle did not fire on a restarted sequence: %s", ev.summary())
		}
		var reused, restarted bool
		for _, x := range v {
			if x.Kind != ViolationACIDAtomicity {
				continue
			}
			if strings.Contains(x.Message, "re-minted transaction sequence") {
				reused = true
			}
			if strings.Contains(x.Message, "restarted instead of resuming") {
				restarted = true
			}
		}
		if !reused && !restarted {
			t.Fatalf("no transaction-sequence finding: %v", v)
		}
		t.Logf("%s; post-recovery sequences start at %d, findings: %v", ev.summary(), ev.post[0].seq, v)
	})
}

// TestMVCCRecoveryChecker_ClausesFire drives every clause of
// [checkMVCCRecovery] from fabricated evidence. A checker whose clauses cannot
// be reached individually is one whose green runs prove nothing.
func TestMVCCRecoveryChecker_ClausesFire(t *testing.T) {
	// healthy is one reopen that reconciled everything: an image holding
	// instants up to 40 and sequences up to 7, and post-recovery commits strictly
	// above both.
	healthy := func() mvccRecoveryEvidence {
		return mvccRecoveryEvidence{
			label:           "unit",
			snapshotInstant: 30,
			hasSnapshot:     true,
			imageMaxTS:      40,
			imageMaxSeq:     7,
			imageSeqs:       map[uint64]struct{}{6: {}, 7: {}},
			imageMarkers:    2,
			walLenAtReopen:  512,
			walOpsReplayed:  9,
			resumedTxnSeq:   7,
			recoveredMaxTS:  40,
			clockFloor:      41,
			post: []walCommitMarker{
				{off: 512, seq: 8, ts: 41, hasTS: true},
				{off: 700, seq: 9, ts: 42, hasTS: true},
			},
			measured: true,
			observed: true,
		}
	}
	h := healthy()
	if v := checkMVCCRecovery(1, &h); len(v) != 0 {
		t.Fatalf("the healthy evidence reports violations: %v", v)
	}

	tests := []struct {
		name    string
		mutate  func(*mvccRecoveryEvidence)
		wantSub string
		wantKnd ViolationKind
	}{
		{
			name:    "never measured",
			mutate:  func(e *mvccRecoveryEvidence) { e.measured = false },
			wantSub: "never measured",
			wantKnd: ViolationACIDDurability,
		},
		{
			name:    "never observed",
			mutate:  func(e *mvccRecoveryEvidence) { e.observed = false },
			wantSub: "no post-recovery commit was ever driven",
			wantKnd: ViolationACIDDurability,
		},
		{
			name:    "no post-recovery marker",
			mutate:  func(e *mvccRecoveryEvidence) { e.post = nil },
			wantSub: "left no durable commit marker",
			wantKnd: ViolationACIDDurability,
		},
		{
			name:    "commit marker without an instant",
			mutate:  func(e *mvccRecoveryEvidence) { e.post[0].hasTS = false },
			wantSub: "carries NO MVCC instant",
			wantKnd: ViolationACIDDurability,
		},
		{
			name:    "instant at the durable maximum",
			mutate:  func(e *mvccRecoveryEvidence) { e.post[0].ts = 40 },
			wantSub: "already contains has been minted again",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name:    "instant below the snapshot's recorded one",
			mutate:  func(e *mvccRecoveryEvidence) { e.imageMaxTS, e.post[0].ts = 0, 30 },
			wantSub: "already contains has been minted again",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name:    "repeated instant",
			mutate:  func(e *mvccRecoveryEvidence) { e.post[1].ts = e.post[0].ts },
			wantSub: "share instant",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name:    "sequence still in the image",
			mutate:  func(e *mvccRecoveryEvidence) { e.post[0].seq = 7 },
			wantSub: "re-minted transaction sequence",
			wantKnd: ViolationACIDAtomicity,
		},
		{
			name: "sequence restarted below the image maximum",
			mutate: func(e *mvccRecoveryEvidence) {
				e.post[0].seq, e.resumedTxnSeq = 1, 0
			},
			wantSub: "restarted instead of resuming",
			wantKnd: ViolationACIDAtomicity,
		},
		{
			name: "pure-snapshot floor below the recorded instant",
			mutate: func(e *mvccRecoveryEvidence) {
				e.imageMaxTS, e.imageMaxSeq, e.imageSeqs = 0, 0, nil
				e.imageMarkers, e.walLenAtReopen, e.walOpsReplayed = 0, 0, 0
				e.clockFloor, e.recoveredMaxTS = 3, 30
			},
			wantSub: "clock floor after a PURE-SNAPSHOT recovery",
			wantKnd: ViolationACIDDurability,
		},
		{
			// The floor is high enough — but only because rehydrating the image
			// minted instants of its own. The recorded instant was never folded.
			name: "pure-snapshot instant not folded into the derived maximum",
			mutate: func(e *mvccRecoveryEvidence) {
				e.imageMaxTS, e.imageMaxSeq, e.imageSeqs = 0, 0, nil
				e.imageMarkers, e.walLenAtReopen, e.walOpsReplayed = 0, 0, 0
				e.clockFloor, e.recoveredMaxTS = 90, 0
			},
			wantSub: "derived a durable maximum",
			wantKnd: ViolationACIDDurability,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := healthy()
			tc.mutate(&e)
			v := checkMVCCRecovery(1, &e)
			if len(v) == 0 {
				t.Fatalf("no violation for %q", tc.name)
			}
			var found bool
			for _, x := range v {
				if strings.Contains(x.Message, tc.wantSub) && x.Kind == tc.wantKnd {
					found = true
				}
			}
			if !found {
				t.Fatalf("no %s finding containing %q: %v", tc.wantKnd, tc.wantSub, v)
			}
		})
	}
}

// TestMVCCClockRecovery_WALOnlyReopenDerivesTheFloor pins the other reopen path
// the harness owns. [recovery.OpenFS] restores the clock itself, but the WAL-only
// core ([recovery.ReplayWAL]) leaves that to its caller — and this harness is a
// caller. Left undone, the reopened graph came up on whatever the replay happened
// to mint, which is a function of the WAL's OP COUNT and not of its instants: the
// property held by accident rather than by derivation, which is exactly what
// rmp #2309 refuses to rely on.
func TestMVCCClockRecovery_WALOnlyReopenDerivesTheFloor(t *testing.T) {
	defer goleak.VerifyNone(t)
	disk := NewSimDisk(NewSeed(24695), 0)
	cfg := durableStoreConfig() // WAL-only: no checkpoint dir, no snapshot
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := range 4 {
		mvccClockCommit(t, st, fmt.Sprintf("mvcc-walonly-%d", i))
	}
	markers, err := simWALCommitMarkers(disk, cfg.dir)
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	if len(markers) == 0 {
		t.Fatal("the WAL carries no commit marker: the reopen would have nothing to derive from")
	}
	durableMax := markers[len(markers)-1].ts
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if got := st2.RecoveredMaxCommitTS(); got != durableMax {
		t.Fatalf("the WAL-only reopen derived maximum instant %d, want the %d its own commit markers carry",
			got, durableMax)
	}
	if floor := st2.ClockNow(); floor < durableMax+1 {
		t.Fatalf("clock floor = %d after a WAL-only reopen, want at least %d", floor, durableMax+1)
	}
	ev, err := measureMVCCRecovery(disk, st2, "WAL-only reopen")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if err := ev.observePostRecoveryCommits(context.Background(), disk, st2, mvccPostRecoveryBeacons); err != nil {
		t.Fatalf("post-recovery commits: %v", err)
	}
	if v := checkMVCCRecovery(0, &ev); len(v) != 0 {
		t.Fatalf("violations after a WAL-only reopen: %v\n%s", v, ev.summary())
	}
	t.Logf("%s", ev.summary())
}

// TestMVCCRecovery_WALCommitMarkersReadTheDurableRecord pins the instrument
// itself: the markers come out of the WAL bytes in file order, carrying the
// sequence and the instant each transaction was made visible at. An instrument
// that silently returned nothing would make every oracle above vacuous.
func TestMVCCRecovery_WALCommitMarkersReadTheDurableRecord(t *testing.T) {
	defer goleak.VerifyNone(t)
	disk := NewSimDisk(NewSeed(24694), 0)
	cfg := fullStackStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	const commits = 5
	for i := range commits {
		mvccClockCommit(t, st, fmt.Sprintf("mvcc-marker-%d", i))
	}
	markers, err := simWALCommitMarkers(disk, cfg.dir)
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	if len(markers) != commits {
		t.Fatalf("read %d commit markers for %d commits", len(markers), commits)
	}
	for i, m := range markers {
		if !m.hasTS {
			t.Fatalf("marker %d carries no instant: the durable record does not say when it became visible", i)
		}
		if i > 0 {
			if m.ts <= markers[i-1].ts {
				t.Fatalf("marker %d instant %d does not exceed marker %d instant %d",
					i, m.ts, i-1, markers[i-1].ts)
			}
			if m.seq <= markers[i-1].seq {
				t.Fatalf("marker %d sequence %d does not exceed marker %d sequence %d",
					i, m.seq, i-1, markers[i-1].seq)
			}
			if m.off <= markers[i-1].off {
				t.Fatalf("marker %d offset %d is not past marker %d offset %d", i, m.off, i-1, markers[i-1].off)
			}
		}
	}
	// An emptied WAL yields no markers rather than an error: that is the state a
	// checkpoint that reclaimed everything leaves behind.
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	after, err := simWALCommitMarkers(disk, cfg.dir)
	if err != nil {
		t.Fatalf("read markers after the checkpoint: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("the checkpoint emptied the WAL but %d markers were still read", len(after))
	}
	instant, ok, err := simSnapshotInstant(disk, cfg.dir)
	if err != nil || !ok {
		t.Fatalf("read the snapshot instant: (%d, %t, %v)", instant, ok, err)
	}
	if instant != markers[commits-1].ts {
		t.Fatalf("the manifest recorded instant %d, want the last committed instant %d",
			instant, markers[commits-1].ts)
	}
}

package sim

import (
	"context"
	"testing"
)

// This file pins the wiring of the SimDisk power-loss (dirent-revocation) model
// into the INTEGRATED crash trigger [Simulator.maybeCrash] (#1811/#1819).
//
// [SimDisk.Crash] drops every directory entry (a create or rename) whose parent
// directory was never fsync'd, exactly as a real power-loss crash within the
// kernel writeback window loses it. The dedicated crash tests in
// disk_dirent_test.go exercise that model on a bare SimDisk; these tests assert
// that the simulator's own crash trigger — the one a crash-storm / full-stack
// run drives on the deterministic tick loop — invokes it too, so the harness
// exercises the genuine crash surface rather than a partial one. Without the
// s.disk.Crash() call in maybeCrash these tests fail (verified by removing it),
// so the wiring can never regress into a silent no-op.
//
// The tests call the unexported maybeCrash directly (white-box) with a schedule
// forced to fire, isolating the trigger from crash-schedule timing so the
// assertion is deterministic and targets exactly the wiring line.

// forcedCrashConfig builds a full-stack (WAL + snapshot) crash configuration
// whose crash schedule fires on every eligible tick (CrashProb 1.0), so a
// direct call to maybeCrash past the opening stability window is guaranteed to
// crash. MaxTicks is unused here because the tests drive maybeCrash by hand.
func forcedCrashConfig(seed uint64) Config {
	return Config{
		Seed:       seed,
		MaxTicks:   0,
		CheckEvery: 1,
		Workload:   WriteHeavyWorkload(NewSeed(seed)),
		Crash:      CrashConfig{Enabled: true, CrashProb: 1.0, StabilityWindow: 20},
		Checkpoint: CheckpointConfig{Enabled: true, Every: 25},
	}
}

// TestSimulator_IntegratedCrashRevokesUnsyncedDirent is the #1819 non-vacuity
// guard: the integrated crash trigger must drop a not-yet-fsync'd directory
// entry on the SimDisk, exactly as a real power-loss crash would. A subdirectory
// file whose parent was never DirSync'd is planted, one crash is forced through
// maybeCrash, and the planted name must be gone afterwards. This can only hold
// if maybeCrash calls s.disk.Crash(); removing that call leaves the name present
// and fails the test.
func TestSimulator_IntegratedCrashRevokesUnsyncedDirent(t *testing.T) {
	s, err := New(forcedCrashConfig(0xD1E7))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	disk := s.Disk()
	// Plant a durable-DATA-but-not-durable-DIRENT file in a subdirectory that is
	// NOT the store's own directory ("db") and NOT root-level: its parent is
	// never DirSync'd, so a real crash loses the name. writeFile Syncs the file
	// DATA (per-Sync durability) but never DirSyncs the parent, which is the
	// precondition SimDisk.Crash revokes.
	const probe = "crashmarker/probe"
	writeFile(t, disk, probe, []byte("unsynced-dirent"))
	if !disk.Exists(probe) {
		t.Fatalf("planted probe %q must exist before the crash", probe)
	}

	// Force exactly one crash on the integrated trigger. A tick past the opening
	// stability window with CrashProb 1.0 makes ShouldCrash fire deterministically.
	crashTick := int64(s.crash.stabilityWindow + 1)
	report, err := s.maybeCrash(context.Background(), crashTick)
	if err != nil {
		t.Fatalf("maybeCrash returned an error: %v", err)
	}
	if report != nil {
		t.Fatalf("maybeCrash reported a durability violation:\n%s", report)
	}
	if s.CrashCount() != 1 {
		t.Fatalf("expected exactly one crash to fire, got CrashCount=%d", s.CrashCount())
	}

	// The load-bearing assertion: the integrated crash revoked the not-yet-durable
	// dirent, so it invoked s.disk.Crash(). Without that call the name survives.
	if disk.Exists(probe) {
		t.Fatalf("integrated crash did NOT revoke the un-fsync'd dirent %q: "+
			"maybeCrash must invoke s.disk.Crash() so the harness exercises the "+
			"real power-loss crash surface (#1819)", probe)
	}
}

// TestSimulator_IntegratedCrashRevocationIsSelective is the fidelity arm: the
// integrated crash must revoke ONLY not-yet-durable dirents, never durable ones.
// It plants three names — a root-level file (durable on create), a subdirectory
// file made durable by DirSync, and a subdirectory file left un-fsync'd — forces
// one crash, and asserts the first two survive while only the third is dropped.
// A blanket wipe (or no revocation at all) fails this, so the test confirms the
// wiring reproduces the real POSIX dirent-durability model, not an artefact.
func TestSimulator_IntegratedCrashRevocationIsSelective(t *testing.T) {
	s, err := New(forcedCrashConfig(0xD1E8))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	disk := s.Disk()

	// Durable on create: a root-level name (the WAL layout relies on this).
	const rootLevel = "durable-root"
	writeFile(t, disk, rootLevel, []byte("root"))

	// Durable by DirSync: a subdirectory name whose parent was fsync'd.
	const syncedSub = "keep/file"
	writeFile(t, disk, syncedSub, []byte("kept"))
	if err := disk.DirSync("keep"); err != nil {
		t.Fatalf("DirSync keep: %v", err)
	}

	// Not durable: a subdirectory name whose parent was never fsync'd.
	const unsyncedSub = "drop/file"
	writeFile(t, disk, unsyncedSub, []byte("lost"))

	crashTick := int64(s.crash.stabilityWindow + 1)
	if _, err := s.maybeCrash(context.Background(), crashTick); err != nil {
		t.Fatalf("maybeCrash returned an error: %v", err)
	}
	if s.CrashCount() != 1 {
		t.Fatalf("expected exactly one crash to fire, got CrashCount=%d", s.CrashCount())
	}

	if !disk.Exists(rootLevel) {
		t.Fatalf("root-level durable name %q must survive the integrated crash", rootLevel)
	}
	if !disk.Exists(syncedSub) {
		t.Fatalf("DirSync'd durable name %q must survive the integrated crash", syncedSub)
	}
	if disk.Exists(unsyncedSub) {
		t.Fatalf("un-fsync'd name %q must be revoked by the integrated crash", unsyncedSub)
	}
}

// TestSimulator_IntegratedCrashRevocationReproducible pins the determinism
// invariant the #1819 wiring must preserve: two same-seed runs of the integrated
// crash trigger, each planting the same un-fsync'd dirent and forcing one crash,
// must reach the identical outcome. SimDisk.Crash is a pure map mutation that
// draws NOTHING from the fault RNG, so wiring it into maybeCrash cannot perturb
// the seeded draw order — this test guards that property against a future change
// that made the crash path consume randomness.
func TestSimulator_IntegratedCrashRevocationReproducible(t *testing.T) {
	run := func() (int, bool) {
		s, err := New(forcedCrashConfig(0xD1E9))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		disk := s.Disk()
		const probe = "crashmarker/probe"
		writeFile(t, disk, probe, []byte("unsynced-dirent"))
		crashTick := int64(s.crash.stabilityWindow + 1)
		if _, err := s.maybeCrash(context.Background(), crashTick); err != nil {
			t.Fatalf("maybeCrash: %v", err)
		}
		return s.crashCount, disk.Exists(probe)
	}
	countA, existsA := run()
	countB, existsB := run()
	if countA != countB {
		t.Fatalf("integrated crash count not reproducible: %d vs %d", countA, countB)
	}
	if existsA != existsB {
		t.Fatalf("integrated crash dirent-revocation not reproducible: %v vs %v", existsA, existsB)
	}
	if existsA {
		t.Fatal("integrated crash did not revoke the un-fsync'd dirent (both runs)")
	}
}

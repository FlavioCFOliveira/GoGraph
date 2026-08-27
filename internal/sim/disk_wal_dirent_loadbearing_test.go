package sim

// disk_wal_dirent_loadbearing_test.go — the WAL's directory fsync must be
// load-bearing in EVERY mode (rmp #2539, audit finding F5).
//
// # What was blind
//
// [isRootLevel] treats a name whose parent is "." as durably linked from
// creation. The WAL-only layout put the WAL at the bare key "wal", so it was
// fully exempt from the dirent model and wal.OpenFS's unconditional
// ParentDirSync was INERT: the call happened, changed nothing, and deleting it
// could not have failed a run. Full-stack mode put the WAL under a directory and
// the fsync was load-bearing there, which bounded the exposure — but the gap was
// invisible at the scenario level, so a WAL-only scenario appeared to cover the
// WAL dirent fsync and did not.
//
// The WAL-only layout now lives under [simWALDir], so the same fsync carries the
// same weight in both modes.
//
// # Why this test removes the fsync rather than asserting it happened
//
// Asserting that ParentDirSync was CALLED would have passed the whole time it
// was inert — that is precisely how the gap survived. The only assertion that
// distinguishes a load-bearing call from a decorative one is to withhold it and
// require the crash to punish the omission.

import (
	"os"
	"testing"
)

// walDirentFixture writes a WAL-shaped file under simWALDir and syncs its DATA,
// leaving the caller to decide whether its NAME is also made durable.
func walDirentFixture(t *testing.T, d *SimDisk, syncDir bool) {
	t.Helper()
	h, err := d.OpenFile(simWALPath, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", simWALPath, err)
	}
	if _, err := h.Write([]byte("frame-0")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The DATA is durable in both arms. Only the NAME differs, so a difference in
	// outcome is attributable to the dirent fsync and to nothing else.
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if syncDir {
		if err := d.DirSync(simWALDir); err != nil {
			t.Fatalf("DirSync(%s): %v", simWALDir, err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWALDirentFsyncIsLoadBearing is the AC: withhold the directory fsync and
// the crash must take the WAL with it; perform it and the WAL must survive.
func TestWALDirentFsyncIsLoadBearing(t *testing.T) {
	t.Run("without the dirent fsync the WAL does not survive", func(t *testing.T) {
		d := NewSimDisk(NewSeed(1), 0)
		walDirentFixture(t, d, false)
		d.CrashHost()

		if d.Exists(simWALPath) {
			t.Errorf("the WAL at %s survived a crash although nothing ever fsynced %s. "+
				"Its data was synced but its NAME was not, and a name that was never durable "+
				"cannot survive — this is the exemption #2539 removed, and if this passes the "+
				"WAL dirent fsync is inert again", simWALPath, simWALDir)
		}
	})

	t.Run("with the dirent fsync the WAL survives", func(t *testing.T) {
		d := NewSimDisk(NewSeed(1), 0)
		walDirentFixture(t, d, true)
		d.CrashHost()

		if !d.Exists(simWALPath) {
			t.Fatalf("the WAL at %s did not survive a crash although both its data and its "+
				"directory were fsynced; the dirent model is now rejecting a correctly "+
				"durable name", simWALPath)
		}
		got, err := d.ReadFile(simWALPath)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", simWALPath, err)
		}
		if string(got) != "frame-0" {
			t.Errorf("the surviving WAL holds %q, want %q", got, "frame-0")
		}
	})
}

// TestWALOnlyLayoutIsNotRootExempt pins the structural fact the two arms above
// depend on. If the WAL-only path ever moves back to the root, isRootLevel
// exempts it again, both arms above start passing for the wrong reason — the
// first because the file survives for a reason unrelated to the fsync — and the
// blindness returns silently.
func TestWALOnlyLayoutIsNotRootExempt(t *testing.T) {
	if isRootLevel(simWALPath) {
		t.Fatalf("the WAL-only layout is at %q, which isRootLevel exempts from the dirent "+
			"model; the WAL dirent fsync is inert again and every scenario that appears to "+
			"cover it does not (#2539)", simWALPath)
	}
	// And the exemption itself still exists for names that genuinely sit at the
	// root: #2539 removed the WAL's reliance on it, not the mechanism. Retiring
	// the mechanism outright was MEASURED and rejected — with isRootLevel always
	// false, 16 tests fail because a root-level file's name can never become
	// durable, there being no root directory for anything to fsync.
	if !isRootLevel("bare-name") {
		t.Errorf("isRootLevel no longer exempts a genuinely root-level name; a file at the " +
			"root has no parent directory whose fsync could make its name durable, so " +
			"removing the exemption makes such a file unrepresentable rather than better modelled")
	}
}

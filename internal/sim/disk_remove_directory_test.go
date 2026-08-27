package sim

// disk_remove_directory_test.go — Remove must not silently no-op on a directory
// (rmp #2545, audit finding F11).
//
// Remove always succeeded, on an absent path AND on a directory. The tolerant
// absent-path half is deliberate and documented — the snapshot writer's
// best-effort cleanup relies on it — and is preserved unchanged. The risky half
// was the directory: a caller that meant to delete one received success and no
// deletion, and the model would never have revealed the mistake.
//
// The target is os.Remove, which is what this surface models. It tries unlink
// and then rmdir (os/file_unix.go), so an EMPTY directory is removed and a
// NON-EMPTY one returns ENOTEMPTY. Directories only became distinguishable from
// absent paths with #2543.

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

func rmDirFixtureFile(t *testing.T, d *SimDisk, path string) {
	t.Helper()
	h, err := d.OpenFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	if _, err := h.Write([]byte("x")); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close(%s): %v", path, err)
	}
}

// TestRemove_AbsentPathStillSucceeds pins the half that must NOT change.
func TestRemove_AbsentPathStillSucceeds(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	if err := d.Remove("db/never-existed"); err != nil {
		t.Errorf("Remove on an absent path returned %v, want nil. The tolerance is deliberate "+
			"and the snapshot writer's best-effort cleanup relies on it", err)
	}
}

// TestRemove_NonEmptyDirectoryReturnsENOTEMPTY is the defect proper: the silent
// success is gone.
func TestRemove_NonEmptyDirectoryReturnsENOTEMPTY(t *testing.T) {
	d := NewSimDisk(NewSeed(2), 0)
	rmDirFixtureFile(t, d, "db/snapshot/manifest.json")

	err := d.Remove("db/snapshot")
	if err == nil {
		t.Fatalf("Remove on a NON-EMPTY directory returned nil. A caller that meant to delete a " +
			"directory would receive success and no deletion, and the model would never reveal " +
			"the mistake (#2545)")
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Errorf("Remove on a non-empty directory returned %v, want ENOTEMPTY — os.Remove falls "+
			"back to rmdir, which reports exactly that", err)
	}
	var perr *fs.PathError
	if !errors.As(err, &perr) || perr.Op != "remove" || perr.Path != "db/snapshot" {
		t.Errorf("the error is %#v, want a *fs.PathError with Op=remove and the offending path; "+
			"os.Remove wraps its failures that way and a caller may inspect them", err)
	}

	// And nothing was deleted, which is the other half of "no silent no-op": the
	// call must not half-succeed either.
	if !d.Exists("db/snapshot/manifest.json") {
		t.Errorf("the refused Remove deleted the directory's contents anyway")
	}
}

// TestRemove_EmptyDirectoryIsRemoved is the rmdir half. os.Remove SUCCEEDS on an
// empty directory, so refusing every directory outright would be just as wrong
// in the other direction.
func TestRemove_EmptyDirectoryIsRemoved(t *testing.T) {
	d := NewSimDisk(NewSeed(3), 0)
	if err := d.MkdirAll("db/empty", 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if !d.Exists("db/empty") {
		t.Fatalf("the fixture directory was not created")
	}

	if err := d.Remove("db/empty"); err != nil {
		t.Fatalf("Remove on an EMPTY directory returned %v, want nil: os.Remove tries unlink and "+
			"then rmdir, and rmdir succeeds on an empty directory", err)
	}
	if d.Exists("db/empty") {
		t.Errorf("the empty directory survived a successful Remove")
	}
}

// TestRemove_FileIsUnaffected is the control. Without it, every assertion above
// is satisfied by a Remove that stopped deleting anything at all.
func TestRemove_FileIsUnaffected(t *testing.T) {
	d := NewSimDisk(NewSeed(4), 0)
	rmDirFixtureFile(t, d, "db/snapshot/manifest.json")

	if err := d.Remove("db/snapshot/manifest.json"); err != nil {
		t.Fatalf("Remove on a FILE returned %v, want nil", err)
	}
	if d.Exists("db/snapshot/manifest.json") {
		t.Errorf("the file survived its own removal; the directory handling above would then be " +
			"asserting nothing")
	}
}

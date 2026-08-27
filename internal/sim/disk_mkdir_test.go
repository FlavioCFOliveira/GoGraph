package sim

// disk_mkdir_test.go — directories are first-class names (rmp #2543, audit
// finding F9).
//
// MkdirAll used to return nil and register nothing, so a created directory was
// IMPLICITLY DURABLE — needing no parent fsync, unlike every other name — and an
// EMPTY directory was UNREPRESENTABLE, because Stat inferred a directory purely
// from a key prefix and Exists never saw directories at all.
//
// Neither was a live defect: recovery keys on the manifest FILE rather than on
// the directory, so it is robust to the gap. The exposure was prospective — any
// future check of the form "does the snapshot directory exist" would have been
// silently inert, and "the snapshot directory exists but is incomplete" was not
// a state a scenario could reach. These tests make all three reachable.

import (
	"os"
	"testing"
)

func TestMkdirAll_CreatedDirectoryNeedsAParentFsync(t *testing.T) {
	t.Run("without the parent fsync the directory does not survive", func(t *testing.T) {
		d := NewSimDisk(NewSeed(1), 0)
		if err := d.MkdirAll("db/snap", 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if !d.Exists("db/snap") {
			t.Fatalf("MkdirAll did not create db/snap")
		}
		d.CrashHost()
		if d.Exists("db/snap") {
			t.Errorf("db/snap survived a crash although nothing fsynced its parent. A created " +
				"directory's name lives in its parent and is durable only once that parent is " +
				"fsynced, exactly like a file (#2543)")
		}
	})

	t.Run("with the parent fsync the directory survives", func(t *testing.T) {
		d := NewSimDisk(NewSeed(1), 0)
		if err := d.MkdirAll("db/snap", 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		// Fsync from the outside in: db's own name first, then db, which is what
		// durabilises db/snap.
		if err := d.DirSync("."); err != nil {
			t.Fatalf("DirSync(.): %v", err)
		}
		if err := d.DirSync("db"); err != nil {
			t.Fatalf("DirSync(db): %v", err)
		}
		d.CrashHost()
		if !d.Exists("db/snap") {
			t.Errorf("db/snap did not survive a crash although its parent chain was fsynced")
		}
	})
}

// TestMkdirAll_EmptyDirectoryIsRepresentable is the state the audit named
// unreachable: a directory that exists and holds nothing.
func TestMkdirAll_EmptyDirectoryIsRepresentable(t *testing.T) {
	d := NewSimDisk(NewSeed(2), 0)
	if err := d.MkdirAll("db/empty", 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if !d.Exists("db/empty") {
		t.Errorf("Exists reports no db/empty; an empty directory must be representable")
	}
	fi, err := d.Stat("db/empty")
	if err != nil {
		t.Fatalf("Stat(db/empty): %v; an empty directory must be stat-able", err)
	}
	if !fi.IsDir() {
		t.Errorf("Stat(db/empty).IsDir() = false, want true")
	}
	if fi.Name() != "empty" {
		t.Errorf("Stat(db/empty).Name() = %q, want %q", fi.Name(), "empty")
	}

	// A path that was never created still does not exist, so the assertions above
	// are not satisfied by Stat having become indiscriminate.
	if d.Exists("db/never") {
		t.Errorf("Exists reports db/never, which was never created")
	}
	if _, err := d.Stat("db/never"); err == nil {
		t.Errorf("Stat(db/never) succeeded for a path that was never created")
	}
}

// TestMkdirAll_ExistingDirectoryKeepsItsDurability is the safety bound. MkdirAll
// on an existing directory creates nothing and needs no fsync; registering it
// afresh as not-yet-durable would let a crash delete a subtree that was already
// durable, which is a WRONG model rather than a stricter one.
func TestMkdirAll_ExistingDirectoryKeepsItsDurability(t *testing.T) {
	d := NewSimDisk(NewSeed(3), 0)

	// A file under db/live, with the whole chain fsynced: durable.
	h, err := d.OpenFile("db/live/manifest", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("m")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = h.Close()
	for _, dir := range []string{".", "db", "db/live"} {
		if err := d.DirSync(dir); err != nil {
			t.Fatalf("DirSync(%s): %v", dir, err)
		}
	}

	// A later MkdirAll over the same path must change nothing.
	if err := d.MkdirAll("db/live", 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	d.CrashHost()

	if !d.Exists("db/live/manifest") {
		t.Errorf("a durable file vanished because MkdirAll re-registered its existing parent as " +
			"not-yet-durable; MkdirAll on an existing directory creates nothing and needs no fsync")
	}
}

// TestMkdirAll_PartialPublishStateIsReachable is acceptance criterion 3: the
// snapshot directory exists but is incomplete.
//
// The audit's point was that a future probe of the form "does the snapshot
// directory exist" would be silently inert under simulation. Today's recovery
// keys on the manifest FILE, so this state must be visible as "directory yes,
// manifest no" — and before #2543 the first half of that could not be observed
// at all.
func TestMkdirAll_PartialPublishStateIsReachable(t *testing.T) {
	d := NewSimDisk(NewSeed(4), 0)

	// A publish that got as far as creating its directory and nothing else.
	if err := d.MkdirAll("db/snapshot", 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if !d.Exists("db/snapshot") {
		t.Fatalf("the snapshot directory is not observable, so a directory-existence probe " +
			"would be inert under simulation — the exposure #2543 exists to remove")
	}
	if d.Exists("db/snapshot/manifest.json") {
		t.Fatalf("the manifest exists; this fixture is meant to be an INCOMPLETE publish")
	}

	// Now a publish that got one file in but not the manifest: still incomplete,
	// and still distinguishable from a complete one.
	h, err := d.OpenFile("db/snapshot/nodes", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = h.Close()

	if !d.Exists("db/snapshot") || !d.Exists("db/snapshot/nodes") {
		t.Errorf("the partially-published directory is not fully observable")
	}
	if d.Exists("db/snapshot/manifest.json") {
		t.Errorf("the manifest appeared without being written")
	}
	fi, err := d.Stat("db/snapshot")
	if err != nil || !fi.IsDir() {
		t.Errorf("Stat(db/snapshot) = (%v, %v), want a directory", fi, err)
	}
}

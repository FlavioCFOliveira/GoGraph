package sim

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// These tests pin the SimDisk dirent-durability model in isolation, independent
// of the snapshot/recovery integration: a created or renamed name survives a
// crash only after a DirSync of its parent directory.

func writeFile(t *testing.T, d *SimDisk, path string, data []byte) {
	t.Helper()
	h, err := d.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	if _, err := h.Write(data); err != nil {
		t.Fatalf("Write %s: %v", path, err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync %s: %v", path, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close %s: %v", path, err)
	}
}

// TestSimDisk_DirentDroppedWithoutDirSync: a freshly created file in a
// subdirectory whose parent is never DirSync'd is dropped by a crash.
func TestSimDisk_DirentDroppedWithoutDirSync(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "dir/file", []byte("hello"))
	if !d.Exists("dir/file") {
		t.Fatal("file should exist before crash")
	}
	d.Crash()
	if d.Exists("dir/file") {
		t.Fatal("not-yet-fsync'd dirent must be dropped by crash")
	}
}

// TestSimDisk_DirentSurvivesWithDirSync: the same file survives when its parent
// directory is DirSync'd before the crash.
func TestSimDisk_DirentSurvivesWithDirSync(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "dir/file", []byte("hello"))
	if err := d.DirSync("dir"); err != nil {
		t.Fatalf("DirSync: %v", err)
	}
	d.Crash()
	if !d.Exists("dir/file") {
		t.Fatal("dirent made durable by DirSync must survive crash")
	}
	b, err := d.ReadFile("dir/file")
	if err != nil || string(b) != "hello" {
		t.Fatalf("ReadFile = %q, %v; want \"hello\", nil", b, err)
	}
}

// TestSimDisk_RootLevelDurableOnCreate: a root-level file (e.g. the WAL) is
// treated as durably linked on creation so the WAL data-durability model is
// unaffected by the dirent model.
func TestSimDisk_RootLevelDurableOnCreate(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "wal", []byte("frame"))
	d.Crash()
	if !d.Exists("wal") {
		t.Fatal("root-level file (WAL) must survive crash without an explicit DirSync")
	}
}

// TestSimDisk_DirRenameLostWithoutParentFsync: renaming a directory and crashing
// before the parent fsync drops the whole moved subtree (the rename is lost).
func TestSimDisk_DirRenameLostWithoutParentFsync(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "db/stage/a", []byte("1"))
	writeFile(t, d, "db/stage/b", []byte("2"))
	if err := d.DirSync("db/stage"); err != nil { // children durable within stage
		t.Fatalf("DirSync stage: %v", err)
	}
	if err := d.Rename("db/stage", "db/live"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// Crash before ParentDirSync(db/live): the live directory's own dirent is
	// not durable, so the whole subtree is lost.
	d.Crash()
	if d.Exists("db/live/a") || d.Exists("db/live/b") {
		t.Fatal("directory rename not made durable must be fully dropped by crash")
	}
}

// TestSimDisk_DirRenameSurvivesWithParentFsync: the moved subtree survives when
// the parent of the new directory name is fsync'd after the rename.
func TestSimDisk_DirRenameSurvivesWithParentFsync(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "db/stage/a", []byte("1"))
	writeFile(t, d, "db/stage/b", []byte("2"))
	if err := d.DirSync("db/stage"); err != nil {
		t.Fatalf("DirSync stage: %v", err)
	}
	if err := d.Rename("db/stage", "db/live"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := d.ParentDirSync("db/live"); err != nil { // durabilise the live name
		t.Fatalf("ParentDirSync: %v", err)
	}
	d.Crash()
	if !d.Exists("db/live/a") || !d.Exists("db/live/b") {
		t.Fatal("durable directory rename must survive crash")
	}
}

// TestSimDisk_StatAndRemoveAll exercises the new probe + recursive-remove
// surface the snapshot/recovery seam relies on.
func TestSimDisk_StatAndRemoveAll(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "x/y/z", []byte("v"))
	if _, err := d.Stat("x/y/z"); err != nil {
		t.Fatalf("Stat existing: %v", err)
	}
	if fi, err := d.Stat("x"); err != nil || !fi.IsDir() {
		t.Fatalf("Stat dir: fi=%v err=%v; want a directory", fi, err)
	}
	if _, err := d.Stat("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat missing = %v; want ErrNotExist", err)
	}
	if err := d.RemoveAll("x"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if d.Exists("x/y/z") {
		t.Fatal("RemoveAll must remove the whole subtree")
	}
}

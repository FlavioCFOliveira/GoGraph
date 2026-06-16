package sim

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// TestSimDisk_WriteReadRoundTrip verifies that bytes written at offset zero are
// read back unchanged when no faults are configured.
func TestSimDisk_WriteReadRoundTrip(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	h, err := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	want := []byte("hello durable world")
	if n, err := h.Write(want); err != nil || n != len(want) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if _, err := h.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(h, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}

// TestSimDisk_OpenMissingNoCreate confirms a missing file without O_CREATE
// reports fs.ErrNotExist.
func TestSimDisk_OpenMissingNoCreate(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	if _, err := d.OpenFile("nope", os.O_RDONLY); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

// TestSimDisk_AppendAndTruncate verifies O_APPEND positions at end and Truncate
// resizes correctly.
func TestSimDisk_AppendAndTruncate(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	h, _ := d.OpenFile("f", os.O_CREATE|os.O_RDWR)
	_, _ = h.Write([]byte("0123456789"))

	h2, _ := d.OpenFile("f", os.O_RDWR|os.O_APPEND)
	if _, err := h2.Write([]byte("ABC")); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := h2.Truncate(5); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	snap := d.Snapshot()
	if string(snap["f"]) != "01234" {
		t.Fatalf("after truncate: got %q want %q", snap["f"], "01234")
	}
}

// TestSimDisk_RenameAtomic verifies Rename moves contents and removes the
// source key.
func TestSimDisk_RenameAtomic(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	h, _ := d.OpenFile("tmp", os.O_CREATE|os.O_RDWR)
	_, _ = h.Write([]byte("payload"))
	if err := d.Rename("tmp", "live"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if d.Exists("tmp") {
		t.Fatal("source still exists after rename")
	}
	if !d.Exists("live") {
		t.Fatal("destination missing after rename")
	}
	if string(d.Snapshot()["live"]) != "payload" {
		t.Fatal("contents lost on rename")
	}
}

// TestSimDisk_SyncFaultDeterministic verifies that the Sync fault sequence is a
// pure function of the seed: two disks with the same seed and fault rate
// produce the identical sequence of Sync results.
func TestSimDisk_SyncFaultDeterministic(t *testing.T) {
	const n = 200
	collect := func() []bool {
		d := NewSimDisk(NewSeed(99), 0.5)
		h, _ := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)
		out := make([]bool, n)
		for i := range out {
			out[i] = errors.Is(h.Sync(), ErrSimFault)
		}
		return out
	}
	a, b := collect(), collect()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("Sync fault sequence diverged at %d: %v vs %v", i, a[i], b[i])
		}
	}

	// Sanity: with rate 0.5 over 200 draws we expect a mix, not all-true/all-false.
	faults := 0
	for _, f := range a {
		if f {
			faults++
		}
	}
	if faults == 0 || faults == n {
		t.Fatalf("fault rate 0.5 produced degenerate sequence: %d/%d faults", faults, n)
	}
}

// TestSimDisk_SectorCorruption verifies that with faultRate 1.0 a write
// corrupts the data deterministically.
func TestSimDisk_SectorCorruption(t *testing.T) {
	d := NewSimDisk(NewSeed(7), 1.0)
	h, _ := d.OpenFile("f", os.O_CREATE|os.O_RDWR)
	orig := []byte{0x11, 0x22, 0x33}
	_, _ = h.Write(orig)
	snap := d.Snapshot()["f"]
	if snap[0] == orig[0] {
		t.Fatalf("expected first byte corrupted at faultRate 1.0, got %#x", snap[0])
	}
}

// TestSimDisk_SnapshotIsolation verifies the snapshot is a deep copy.
func TestSimDisk_SnapshotIsolation(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	h, _ := d.OpenFile("f", os.O_CREATE|os.O_RDWR)
	_, _ = h.Write([]byte("abc"))
	snap := d.Snapshot()
	snap["f"][0] = 'X'
	if d.Snapshot()["f"][0] != 'a' {
		t.Fatal("mutating snapshot affected live disk")
	}
}

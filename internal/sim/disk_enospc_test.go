package sim

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
)

// TestSimDisk_ENOSPC_EagerWrite verifies that, with a finite capacity and eager
// allocation, a Write that would grow the disk past its budget returns an
// ENOSPC os.PathError, grows nothing, and leaves the prior bytes intact.
func TestSimDisk_ENOSPC_EagerWrite(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	d.SetCapacity(16, false)
	h, err := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// First write fits exactly.
	if n, err := h.Write([]byte("0123456789ABCDEF")); err != nil || n != 16 {
		t.Fatalf("first Write: n=%d err=%v; want 16,nil", n, err)
	}
	// Next byte overflows the 16-byte budget.
	n, err := h.Write([]byte("!"))
	if n != 0 {
		t.Fatalf("overflow Write returned n=%d; want 0 (no partial write)", n)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("overflow Write err=%v; want ENOSPC", err)
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("overflow Write err is not *os.PathError: %T", err)
	}

	// The earlier bytes must survive unchanged (no partial growth/corruption).
	if _, err := h.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got := make([]byte, 16)
	if _, err := io.ReadFull(h, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != "0123456789ABCDEF" {
		t.Fatalf("data after ENOSPC = %q; want unchanged", got)
	}
}

// TestSimDisk_ENOSPC_OverwriteStillAllowed confirms that a non-growing overwrite
// is permitted even when the disk is full, because it consumes no new space.
func TestSimDisk_ENOSPC_OverwriteStillAllowed(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	d.SetCapacity(8, false)
	h, _ := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)
	if _, err := h.Write([]byte("AAAAAAAA")); err != nil {
		t.Fatalf("fill Write: %v", err)
	}
	// Rewind and overwrite in place: must succeed despite a full disk.
	if _, err := h.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if n, err := h.Write([]byte("BBBB")); err != nil || n != 4 {
		t.Fatalf("in-place overwrite: n=%d err=%v; want 4,nil", n, err)
	}
}

// TestSimDisk_ENOSPC_DelayedSync verifies the delayed-allocation model: writes
// past the budget are buffered and succeed, but Sync surfaces ENOSPC.
func TestSimDisk_ENOSPC_DelayedSync(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	d.SetCapacity(8, true) // delayed allocation
	h, _ := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)

	// Within budget: write + sync both succeed.
	if _, err := h.Write([]byte("01234567")); err != nil {
		t.Fatalf("write within budget: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("sync within budget: %v; want nil", err)
	}
	// Over budget: the buffered write succeeds, the fsync fails ENOSPC.
	if _, err := h.Write([]byte("89")); err != nil {
		t.Fatalf("buffered over-budget write should succeed, got %v", err)
	}
	if err := h.Sync(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("over-budget Sync err=%v; want ENOSPC", err)
	}
}

// TestSimDisk_ENOSPC_TruncateGrow checks both Truncate (handle) and TruncatePath
// honour the eager budget on grow.
func TestSimDisk_ENOSPC_TruncateGrow(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	d.SetCapacity(4, false)
	h, _ := d.OpenFile("snap", os.O_CREATE|os.O_RDWR)
	if err := h.Truncate(4); err != nil {
		t.Fatalf("Truncate within budget: %v", err)
	}
	if err := h.Truncate(5); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Truncate over budget err=%v; want ENOSPC", err)
	}
	if err := d.TruncatePath("snap", 9); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("TruncatePath over budget err=%v; want ENOSPC", err)
	}
	// Shrink is always allowed.
	if err := d.TruncatePath("snap", 2); err != nil {
		t.Fatalf("TruncatePath shrink: %v; want nil", err)
	}
}

// TestSimDisk_ENOSPC_ZeroCapacityUnbounded confirms the default (capacity 0) is
// unbounded — the pre-existing behaviour every other test relies on.
func TestSimDisk_ENOSPC_ZeroCapacityUnbounded(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0) // no SetCapacity => unbounded
	h, _ := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)
	big := make([]byte, 1<<20)
	if n, err := h.Write(big); err != nil || n != len(big) {
		t.Fatalf("unbounded Write: n=%d err=%v", n, err)
	}
}

// TestSimDisk_ENOSPC_Reproducible asserts the capacity check draws nothing from
// the seed: two SimDisks with identical capacity and seed reach byte-identical
// state and the same ENOSPC verdict.
func TestSimDisk_ENOSPC_Reproducible(t *testing.T) {
	run := func() (string, bool) {
		d := NewSimDisk(NewSeed(99), 0.25) // a non-zero fault rate to exercise the draw stream
		d.SetCapacity(32, false)
		h, _ := d.OpenFile("wal", os.O_CREATE|os.O_RDWR)
		var lastErr error
		for i := 0; i < 10; i++ {
			_, lastErr = h.Write([]byte("8bytes!!"))
		}
		img := d.Snapshot()["wal"]
		return string(img), errors.Is(lastErr, syscall.ENOSPC)
	}
	img1, enospc1 := run()
	img2, enospc2 := run()
	if img1 != img2 || enospc1 != enospc2 {
		t.Fatalf("capacity check is not reproducible: enospc %v/%v, image-equal=%v", enospc1, enospc2, img1 == img2)
	}
	if !enospc1 {
		t.Fatalf("expected the 10th 8-byte write to overflow a 32-byte disk")
	}
}

package sim

import (
	"errors"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// This file proves, over the in-memory [SimDisk], that a segmented WAL writer
// built via wal.OpenFS reclaims log space by unlinking whole segments: the
// control-file write, the segment rollover and the unlink all route through
// [simWALFS], so the reclaimed state is durable in the in-memory image and
// survives a crash (docs/design-wal-v2.md §2.4, rmp #2195).

// TestWAL_OpenFS_ReclaimSegmentsOverSimDisk appends three frames, durably syncs
// them, discards them with Writer.Truncate (rollover, control file with OR at
// the current position, unlink of the older segment), appends one more frame,
// and crashes. The log read back must hold exactly the post-truncation frame,
// starting at OR, with the prefix-truncated flag set and the old segment gone.
func TestWAL_OpenFS_ReclaimSegmentsOverSimDisk(t *testing.T) {
	disk := NewSimDisk(NewSeed(1752), 0) // no data faults: isolate the reclamation path
	const walPath = "db/wal"

	w, err := wal.OpenFS(simWALFS{disk: disk}, walPath)
	if err != nil {
		t.Fatalf("wal.OpenFS: %v", err)
	}
	mark, err := w.AppendRun(func(emit func([]byte) error) error {
		for _, p := range []string{"frame-zero", "frame-one", "frame-two"} {
			if err := emit([]byte(p)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := w.SyncGroup(mark); err != nil {
		t.Fatalf("SyncGroup: %v", err)
	}
	if !disk.Exists(wal.SegmentPath(walPath, 1)) {
		t.Fatal("segment 1 does not exist: the frames did not land in a segment")
	}
	reclaimed, err := w.Truncate()
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if reclaimed != mark {
		t.Fatalf("Truncate reclaimed %d log bytes, want %d", reclaimed, mark)
	}
	mark3, err := w.AppendRun(func(emit func([]byte) error) error { return emit([]byte("frame-three")) })
	if err != nil {
		t.Fatalf("post-truncate append: %v", err)
	}
	if err := w.SyncGroup(mark3); err != nil {
		t.Fatalf("post-truncate SyncGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Crash: revoke every not-yet-durable dirent. The control write and the
	// unlink fsynced their directories, so the reclaimed state must survive.
	disk.Crash()

	if disk.Exists(wal.SegmentPath(walPath, 1)) {
		t.Fatal("segment 1 survived the reclamation and the crash")
	}
	log, err := wal.OpenLogFS(simLogFS{disk: disk}, walPath)
	if err != nil {
		t.Fatalf("OpenLogFS: %v", err)
	}
	ctl, ok := log.Control()
	if !ok || ctl.Flags&wal.ControlPrefixTruncated == 0 || int64(ctl.OldestRetainedPos) != mark {
		t.Fatalf("control after reclamation = %+v (present %t), want PrefixTruncated with OR %d", ctl, ok, mark)
	}
	var got []string
	for f := range log.Frames() {
		got = append(got, string(f.Payload))
		if int64(f.Pos) != mark {
			t.Errorf("surviving frame at position %d, want %d", f.Pos, mark)
		}
	}
	if err := log.TailError(); err != nil {
		t.Fatalf("log tail error: %v", err)
	}
	if len(got) != 1 || got[0] != "frame-three" {
		t.Fatalf("surviving frames = %q, want [frame-three]", got)
	}
}

// TestWAL_OpenWith_ReclaimUnsupported is the contrast arm: the single-file
// wal.OpenWith test writer has no control file and no segments, so the
// checkpoint-side calls report wal.ErrSegmentsUnsupported.
func TestWAL_OpenWith_ReclaimUnsupported(t *testing.T) {
	disk := NewSimDisk(NewSeed(1752), 0)
	wh, err := disk.OpenFile("db/wal", os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w, err := wal.OpenWith(wh)
	if err != nil {
		t.Fatalf("wal.OpenWith: %v", err)
	}
	defer func() { _ = w.Close() }()
	if err := w.MarkCheckpoint(0); !errors.Is(err, wal.ErrSegmentsUnsupported) {
		t.Fatalf("OpenWith MarkCheckpoint err = %v, want ErrSegmentsUnsupported", err)
	}
	if _, err := w.ReclaimSegments(); !errors.Is(err, wal.ErrSegmentsUnsupported) {
		t.Fatalf("OpenWith ReclaimSegments err = %v, want ErrSegmentsUnsupported", err)
	}
}

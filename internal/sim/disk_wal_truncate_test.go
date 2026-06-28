package sim

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// This file proves rmp #1752 non-vacuously: a PATH-BACKED WAL writer built via
// wal.OpenFS over the in-memory [SimDisk] supports Writer.TruncatePrefix — the
// crash-safe prefix reclamation a Checkpointer drives in its phase 3. The
// path-less wal.OpenWith writer the simulator used before #1752 returned
// wal.ErrPrefixTruncateUnsupported here, which is exactly why a
// Checkpointer-backed checkpoint could not run on SimDisk.
//
// The truncation's temp-write -> rename -> parent-dir fsync -> reopen all route
// through [simWALFS] over the same SimDisk, so the surviving suffix is durable
// in the in-memory image and replays after a crash.

// frameBytes returns the on-disk encoding of a one-payload WAL frame, so the
// test can compute exact byte offsets for the prefix watermark.
func frameBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := wal.Encode(&buf, wal.Frame{Version: wal.CurrentVersion, Payload: payload}); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return buf.Bytes()
}

// TestWAL_OpenFS_TruncatePrefixOverSimDisk appends three frames through a
// path-backed wal.OpenFS writer over SimDisk, durably syncs them, then truncates
// the first frame away via the real Writer.TruncatePrefix. After a crash and a
// fresh reopen the WAL image must hold exactly the surviving two-frame suffix,
// byte for byte — proving the temp/rename/reopen dance ran correctly over the
// in-memory disk.
func TestWAL_OpenFS_TruncatePrefixOverSimDisk(t *testing.T) {
	disk := NewSimDisk(NewSeed(1752), 0) // no data faults: isolate the truncate path

	p0 := []byte("frame-zero")
	p1 := []byte("frame-one")
	p2 := []byte("frame-two")
	f0 := frameBytes(t, p0)
	f1 := frameBytes(t, p1)
	f2 := frameBytes(t, p2)

	w, err := wal.OpenFS(simWALFS{disk: disk}, "db/wal")
	if err != nil {
		t.Fatalf("wal.OpenFS: %v", err)
	}
	for _, p := range [][]byte{p0, p1, p2} {
		if err := w.Append(p); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Durably commit all three frames (group-commit fsync over the SimDisk).
	if err := w.SyncGroup(); err != nil {
		t.Fatalf("SyncGroup: %v", err)
	}

	// Reclaim the first frame: TruncatePrefix(upTo=len(f0)) writes the surviving
	// suffix (f1+f2) to db/wal.tmp, renames it over db/wal, fsyncs the parent
	// dir, and reopens — all through simWALFS. This is the call that returned
	// ErrPrefixTruncateUnsupported on the pre-#1752 path-less writer.
	reclaimed, err := w.TruncatePrefix(int64(len(f0)))
	if err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	if reclaimed != int64(len(f0)) {
		t.Fatalf("TruncatePrefix reclaimed %d bytes, want %d", reclaimed, len(f0))
	}

	// Append one more frame after the truncation to confirm the reopened handle
	// appends at the new (suffix) end, not behind a stale offset.
	p3 := []byte("frame-three")
	f3 := frameBytes(t, p3)
	if err := w.Append(p3); err != nil {
		t.Fatalf("post-truncate append: %v", err)
	}
	if err := w.SyncGroup(); err != nil {
		t.Fatalf("post-truncate SyncGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Crash: revoke any not-yet-durable dirents. The truncation fsynced the
	// rename's parent dir, so the suffix-only WAL must survive intact.
	disk.Crash()

	got, err := disk.ReadFile("db/wal")
	if err != nil {
		t.Fatalf("read WAL image: %v", err)
	}
	want := bytes.Join([][]byte{f1, f2, f3}, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("WAL image after TruncatePrefix+crash = %d bytes, want %d (suffix f1+f2+f3)", len(got), len(want))
	}

	// The temp file must have been renamed away, not left behind.
	if disk.Exists("db/wal.tmp") {
		t.Fatal("db/wal.tmp survived TruncatePrefix; the rename did not consume it")
	}
}

// TestWAL_OpenWith_TruncatePrefixUnsupported is the contrast arm: the path-less
// wal.OpenWith writer the simulator used before #1752 rejects TruncatePrefix,
// which is precisely the limitation #1752 lifts. It documents why OpenFS was
// needed rather than reusing OpenWith.
func TestWAL_OpenWith_TruncatePrefixUnsupported(t *testing.T) {
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
	if err := w.Append([]byte("x")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := w.SyncGroup(); err != nil {
		t.Fatalf("SyncGroup: %v", err)
	}
	if _, err := w.TruncatePrefix(1); !errors.Is(err, wal.ErrPrefixTruncateUnsupported) {
		t.Fatalf("OpenWith TruncatePrefix err = %v, want ErrPrefixTruncateUnsupported", err)
	}
}

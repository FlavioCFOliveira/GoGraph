package wal

// walv2_audit_fixes_test.go — regression tests for the storage-engine audit
// findings on WAL v2 step 2 (docs/design-wal-v2.md, "Step 2 — as
// implemented"), and dedicated tests for the control-file sentinels.
//
// Layer: short.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// tornFrameBytes returns a prefix of an encoded v2 frame long enough to pass
// the legacy header and short enough to be torn.
func tornFrameBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Encode(&buf, Frame{Version: CurrentVersion, Payload: []byte("torn"), Pos: 1, PrevLen: 0, StoreID: 1}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()[:20]
}

func appendToFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLog_TornFirstFrameOfEmptyActiveSegmentKeepsPrevLink (audit D1): after a
// truncation whose oldest retained position is the start of an empty active
// segment, a torn first frame in that segment stops the stream before any frame
// is read. The reader must still report the frame preceding OR as the last
// frame, so the frame the writer appends at OR links to it and the next open
// reads the log clean instead of refusing it with ErrPrevLink.
func TestLog_TornFirstFrameOfEmptyActiveSegmentKeepsPrevLink(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	appendFrames(t, w, 3, 100, 0xD1)
	if _, err := w.Truncate(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := log.Control()
	if ctl.OldestRetainedPos == 0 || ctl.PrevFramePosAtOR == NoFramePos {
		t.Fatalf("precondition: control %+v must record a truncated prefix and a predecessor", ctl)
	}
	segs := segmentPaths(t, walPath)
	for _, p := range segs {
		if fi, err := os.Stat(p); err != nil || fi.Size() != segHeaderSize {
			t.Fatalf("precondition: segment %s must be empty after the truncation (%v)", p, err)
		}
	}
	appendToFile(t, segs[0], tornFrameBytes(t))

	w2, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatalf("reopen over a torn first frame: %v", err)
	}
	appendFrames(t, w2, 1, 10, 0xD2)
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	log2, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range log2.Frames() {
		n++
	}
	if log2.TailError() != nil || n != 1 {
		t.Fatalf("log after the acknowledged append: %d frames, tail %v; want 1 frame, clean", n, log2.TailError())
	}
}

// TestListSegments_OutOfOrderUnlinkIsNotAGap (audit item 4): a checkpoint
// unlinks segments 1 and 2 oldest first under one directory fsync; a
// filesystem that persists only the second unlink leaves segment 1 beside
// segment 3, whose first frame is OR. That is a leftover below OR, not a
// missing segment: readers accept it and a writable open deletes it.
func TestListSegments_OutOfOrderUnlinkIsNotAGap(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	appendFrames(t, w, 40, 64<<10, 0xD4) // three segments
	end := w.DurableOffset()
	if err := w.MarkCheckpoint(end); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.list.segs) < 3 || log.list.segs[2].firstPos != log.OldestRetainedPos() {
		t.Fatalf("precondition: segment 3 must start at OR %d (%+v)", log.OldestRetainedPos(), log.list.segs)
	}
	if err := os.Remove(SegmentPath(walPath, 2)); err != nil {
		t.Fatal(err)
	}

	log2, err := OpenLog(walPath)
	if err != nil {
		t.Fatalf("a leftover segment below OR beside a gap was refused: %v", err)
	}
	for range log2.Frames() {
	}
	if log2.TailError() != nil || log2.TailOffset() != end {
		t.Fatalf("log: tail %v end %d, want clean end %d", log2.TailError(), log2.TailOffset(), end)
	}
	w2, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatalf("writable open: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SegmentPath(walPath, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover segment 1 survived a writable open: %v", err)
	}

	// Control: a gap above OR is still missing data.
	t.Run("gap_above_or_is_refused", func(t *testing.T) {
		t.Parallel()
		walPath := filepath.Join(t.TempDir(), "wal")
		w, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
		if err != nil {
			t.Fatal(err)
		}
		appendFrames(t, w, 40, 64<<10, 0xD5)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(SegmentPath(walPath, 2)); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenLog(walPath); !errors.Is(err, ErrSegmentGap) {
			t.Fatalf("OpenLog = %v, want ErrSegmentGap", err)
		}
	})
}

// TestOpen_SealsLegacyLogWrittenAfterInterruptedCreate (audit item 3): a crash
// after the control file of a fresh store is written and before its seal stub
// is leaves no legacy file; an older build then starts a v1 log there. The
// next open, finding segments that hold no frame, seals that log (discarding a
// torn tail) so the segment frames appended after it do not make recovery
// refuse with ErrLegacyNotSealed.
func TestOpen_SealsLegacyLogWrittenAfterInterruptedCreate(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(walPath); err != nil { // the stub was never written
		t.Fatal(err)
	}
	var v1 bytes.Buffer
	for _, p := range []string{"v1-a", "v1-b"} {
		if _, err := Encode(&v1, Frame{Payload: []byte(p)}); err != nil {
			t.Fatal(err)
		}
	}
	v1.Write(tornFrameBytes(t)[:12]) // the older build crashed mid-frame
	if err := os.WriteFile(walPath, v1.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	w2, err := Open(walPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	storeID := w2.StoreID()
	appendFrames(t, w2, 1, 10, 0xD3)
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(walPath) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(f, f)
	var payloads [][]byte
	for fr := range r.Frames() {
		payloads = append(payloads, fr.Payload)
	}
	_ = r.Close()
	if r.TailError() != nil || len(payloads) != 3 ||
		string(payloads[0]) != "v1-a" || string(payloads[1]) != "v1-b" {
		t.Fatalf("legacy log: %d frames, tail %v; want v1-a, v1-b and a seal, clean", len(payloads), r.TailError())
	}
	seal, ok := DecodeLegacySeal(payloads[2])
	if !ok || seal.StoreID != storeID {
		t.Fatalf("the legacy log does not end in this store's seal (ok %t, seal %+v, store %016x)", ok, seal, storeID)
	}
}

// TestOpenLog_MissingControl pins ErrMissingControl: a segment-named file, even
// an empty one, with no control file is refused by readers and by the writer.
func TestOpenLog_MissingControl(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	if err := os.MkdirAll(SegmentDir(walPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SegmentPath(walPath, 1), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLog(walPath); !errors.Is(err, ErrMissingControl) {
		t.Fatalf("OpenLog = %v, want ErrMissingControl", err)
	}
	if w, err := Open(walPath); !errors.Is(err, ErrMissingControl) {
		if w != nil {
			_ = w.Close()
		}
		t.Fatalf("Open = %v, want ErrMissingControl", err)
	}
}

// TestOpenLog_ControlCorrupt pins ErrControlCorrupt for every way the control
// file can be malformed: wrong length, bad magic, bad CRC, and a zero store id
// under a valid CRC.
func TestOpenLog_ControlCorrupt(t *testing.T) {
	t.Parallel()
	valid := encodeControl(Control{StoreID: 0x5EED, PrevFramePosAtOR: NoFramePos})
	recrc := func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b[56:60], crc32.Checksum(b[0:56], castagnoli))
		return b
	}
	for _, tc := range []struct {
		name string
		b    func() []byte
	}{
		{"short", func() []byte { return valid[:controlSize-1] }},
		{"long", func() []byte { return append(valid[:], 0) }},
		{"empty", func() []byte { return nil }},
		{"bad_magic", func() []byte { b := valid; b[0] ^= 0xFF; return recrc(b[:]) }},
		{"bad_crc", func() []byte { b := valid; b[56] ^= 0x01; return b[:] }},
		{"store_id_zero", func() []byte { b := encodeControl(Control{PrevFramePosAtOR: NoFramePos}); return b[:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			walPath := filepath.Join(t.TempDir(), "wal")
			if err := os.WriteFile(ControlPath(walPath), tc.b(), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenLog(walPath); !errors.Is(err, ErrControlCorrupt) {
				t.Fatalf("OpenLog = %v, want ErrControlCorrupt", err)
			}
			if w, err := Open(walPath); !errors.Is(err, ErrControlCorrupt) {
				if w != nil {
					_ = w.Close()
				}
				t.Fatalf("Open = %v, want ErrControlCorrupt", err)
			}
		})
	}
	// Control: the unmodified encoding opens.
	walPath := filepath.Join(t.TempDir(), "wal")
	if err := os.WriteFile(ControlPath(walPath), valid[:], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLog(walPath); err != nil {
		t.Fatalf("OpenLog over a valid control file: %v", err)
	}
}

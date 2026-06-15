package wal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
)

// collectPayloads reads every complete frame's payload from the WAL at path,
// in order, failing the test on a non-torn read error.
func collectPayloads(t *testing.T, path string) [][]byte {
	t.Helper()
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	var got [][]byte
	for f := range r.Frames() {
		cp := make([]byte, len(f.Payload))
		copy(cp, f.Payload)
		got = append(got, cp)
	}
	if e := r.TailError(); e != nil && !errors.Is(e, ErrTornFrame) {
		t.Fatalf("reader tail error: %v", e)
	}
	return got
}

// TestTruncatePrefix_PreservesSuffix is the core correctness proof: a
// non-blocking checkpoint folds the prefix [0,W) into a snapshot and calls
// TruncatePrefix(W); the surviving suffix [W,end) — the frames committed
// concurrently during the lock-free snapshot write — must remain byte-for-byte
// intact and replayable. Truncating to zero (the legacy primitive) would lose
// them; prefix truncation must not.
func TestTruncatePrefix_PreservesSuffix(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Prefix: three frames the "snapshot" will fold.
	prefix := [][]byte{[]byte("p0"), []byte("p1-longer"), []byte("p2")}
	for _, p := range prefix {
		if err := w.Append(p); err != nil {
			t.Fatalf("Append prefix: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync prefix: %v", err)
	}
	// Capture the watermark exactly here — every prefix frame is durable and on
	// a frame boundary.
	watermark := w.DurableOffset()

	// Suffix: two frames committed "after" the snapshot capture.
	suffix := [][]byte{[]byte("suffix-0"), []byte("suffix-1-longer")}
	for _, p := range suffix {
		if err := w.Append(p); err != nil {
			t.Fatalf("Append suffix: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync suffix: %v", err)
	}
	end := w.DurableOffset()
	if watermark <= 0 || watermark >= end {
		t.Fatalf("watermark %d not strictly inside (0, end=%d)", watermark, end)
	}

	reclaimed, err := w.TruncatePrefix(watermark)
	if err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	if reclaimed != watermark {
		t.Fatalf("reclaimed = %d, want watermark %d", reclaimed, watermark)
	}
	// In-memory bookkeeping is repointed to the suffix-only file.
	if got := w.DurableOffset(); got != end-watermark {
		t.Fatalf("DurableOffset after truncate = %d, want %d", got, end-watermark)
	}

	// The on-disk WAL now contains exactly the suffix frames, in order.
	got := collectPayloads(t, path)
	if len(got) != len(suffix) {
		t.Fatalf("after TruncatePrefix: %d frames, want %d (%q)", len(got), len(suffix), got)
	}
	for i := range suffix {
		if !bytes.Equal(got[i], suffix[i]) {
			t.Errorf("frame %d = %q, want %q", i, got[i], suffix[i])
		}
	}

	// The writer remains usable: a further append lands after the suffix.
	if err := w.Append([]byte("after-truncate")); err != nil {
		t.Fatalf("Append after TruncatePrefix: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync after TruncatePrefix: %v", err)
	}
	got = collectPayloads(t, path)
	if len(got) != len(suffix)+1 || string(got[len(got)-1]) != "after-truncate" {
		t.Fatalf("append after truncate not durable: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTruncatePrefix_WholeFileToZero confirms TruncatePrefix(durableSize)
// reclaims the entire WAL (suffix empty) — the full-fold case, the analogue of
// the legacy Truncate()-to-zero — leaving an empty, still-usable WAL.
func TestTruncatePrefix_WholeFileToZero(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := range 4 {
		if err := w.Append([]byte(fmt.Sprintf("frame-%d", i))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	all := w.DurableOffset()
	reclaimed, err := w.TruncatePrefix(all)
	if err != nil {
		t.Fatalf("TruncatePrefix(all): %v", err)
	}
	if reclaimed != all {
		t.Fatalf("reclaimed = %d, want %d", reclaimed, all)
	}
	if got := w.DurableOffset(); got != 0 {
		t.Fatalf("DurableOffset after full reclaim = %d, want 0", got)
	}
	if got := collectPayloads(t, path); len(got) != 0 {
		t.Fatalf("WAL not empty after full reclaim: %q", got)
	}
	if err := w.Append([]byte("fresh")); err != nil {
		t.Fatalf("Append after full reclaim: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync after full reclaim: %v", err)
	}
	if got := collectPayloads(t, path); len(got) != 1 || string(got[0]) != "fresh" {
		t.Fatalf("fresh append after full reclaim: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTruncatePrefix_ZeroIsNoOp confirms TruncatePrefix(0) reclaims nothing and
// leaves the WAL untouched.
func TestTruncatePrefix_ZeroIsNoOp(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Append([]byte("keep-me")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	before := w.DurableOffset()
	reclaimed, err := w.TruncatePrefix(0)
	if err != nil {
		t.Fatalf("TruncatePrefix(0): %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0", reclaimed)
	}
	if w.DurableOffset() != before {
		t.Fatalf("DurableOffset changed by no-op: %d -> %d", before, w.DurableOffset())
	}
	if got := collectPayloads(t, path); len(got) != 1 || string(got[0]) != "keep-me" {
		t.Fatalf("no-op altered WAL: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTruncatePrefix_RejectsOutOfRange confirms an upTo past the durable offset
// (or negative) is rejected without touching the file.
func TestTruncatePrefix_RejectsOutOfRange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Append([]byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	durable := w.DurableOffset()
	for _, bad := range []int64{-1, durable + 1, durable + 1000} {
		if _, err := w.TruncatePrefix(bad); err == nil {
			t.Errorf("TruncatePrefix(%d) = nil error, want out-of-range rejection", bad)
		}
	}
	// The WAL is untouched after the rejected calls.
	if got := collectPayloads(t, path); len(got) != 1 || string(got[0]) != "x" {
		t.Fatalf("rejected call altered WAL: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTruncatePrefix_PostRenamePoisons is the F1 fail-stop proof: an error
// AFTER the atomic suffix-rename has succeeded (here the parent-directory
// fsync) cannot be undone — the on-disk WAL has already advanced to the durable
// suffix-only file — so the Writer must POISON itself rather than return a
// plain retryable error and keep appending to the unlinked old inode (a silent
// Durability hole). The on-disk suffix-only WAL must remain intact and
// recoverable.
func TestTruncatePrefix_PostRenamePoisons(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	prefix := [][]byte{[]byte("fold-0"), []byte("fold-1")}
	for _, p := range prefix {
		if err := w.Append(p); err != nil {
			t.Fatalf("Append prefix: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync prefix: %v", err)
	}
	watermark := w.DurableOffset()
	if err := w.Append([]byte("survivor")); err != nil {
		t.Fatalf("Append suffix: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync suffix: %v", err)
	}

	// Inject a post-rename failure: the parent-dir fsync fails AFTER the rename
	// has already replaced the WAL with the suffix-only file.
	injErr := errors.New("injected dir fsync failure")
	w.dirFsync = func(string) error { return injErr }

	if _, err := w.TruncatePrefix(watermark); !errors.Is(err, injErr) {
		t.Fatalf("TruncatePrefix = %v, want injected error %v", err, injErr)
	}
	// The Writer must be poisoned: every subsequent Append/Sync fails with the
	// sticky error.
	if err := w.Append([]byte("must-fail")); !errors.Is(err, injErr) {
		t.Fatalf("Append after post-rename failure = %v, want poisoned with %v", err, injErr)
	}
	if err := w.Sync(); !errors.Is(err, injErr) {
		t.Fatalf("Sync after post-rename failure = %v, want poisoned with %v", err, injErr)
	}
	_ = w.Close()

	// The on-disk state has already advanced to the durable suffix-only WAL:
	// the rename happened, only the in-memory handle was abandoned. Recovery
	// (re-open) must read exactly the survivor frame.
	got := collectPayloads(t, path)
	if len(got) != 1 || string(got[0]) != "survivor" {
		t.Fatalf("on-disk WAL after post-rename poison = %q, want [survivor]", got)
	}
}

// pathlessFile is a minimal walFile with no real filesystem path, used to
// verify TruncatePrefix rejects an OpenWith-constructed (path-less) writer.
type pathlessFile struct{ pos int64 }

func (p *pathlessFile) Write(b []byte) (int, error) { p.pos += int64(len(b)); return len(b), nil }
func (p *pathlessFile) Read(_ []byte) (int, error)  { return 0, io.EOF }
func (p *pathlessFile) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return p.pos, nil
	}
	return off, nil
}
func (p *pathlessFile) Sync() error          { return nil }
func (p *pathlessFile) Truncate(int64) error { return nil }
func (p *pathlessFile) Close() error         { return nil }

// TestTruncatePrefix_PathlessUnsupported confirms a Writer built via OpenWith
// (no real path, as in fault-injection tests) rejects TruncatePrefix with
// ErrPrefixTruncateUnsupported rather than silently corrupting state.
func TestTruncatePrefix_PathlessUnsupported(t *testing.T) {
	t.Parallel()
	w, err := OpenWith(&pathlessFile{})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	// The path check precedes every other guard (offset range, no-op), so a
	// path-less writer is rejected for any upTo.
	for _, upTo := range []int64{0, 1, 99} {
		if _, err := w.TruncatePrefix(upTo); !errors.Is(err, ErrPrefixTruncateUnsupported) {
			t.Fatalf("TruncatePrefix(%d) on path-less writer = %v, want ErrPrefixTruncateUnsupported", upTo, err)
		}
	}
	_ = w.Close()
}

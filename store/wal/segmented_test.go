package wal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestFrameV2_RoundTripAndCRCCoversHeader pins the 36-byte frame: it round-trips
// its position, prev-link and store id, and its CRC covers every one of them,
// so no header field can be altered without detection.
func TestFrameV2_RoundTripAndCRCCoversHeader(t *testing.T) {
	t.Parallel()
	in := Frame{Version: CurrentVersion, Payload: []byte("payload"), Pos: 1 << 33, PrevLen: 77, StoreID: 0xABCDEF}
	var buf bytes.Buffer
	n, err := Encode(&buf, in)
	if err != nil || n != HeaderSizeV2+len(in.Payload) || FrameSize(in) != n {
		t.Fatalf("Encode = %d, %v; FrameSize %d", n, err, FrameSize(in))
	}
	out, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil || out.Pos != in.Pos || out.PrevLen != in.PrevLen || out.StoreID != in.StoreID ||
		out.Version != CurrentVersion || !bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("round trip = %+v, %v; want %+v", out, err, in)
	}
	for _, off := range []int{6, 8, 12, 19, 20, 24, 31, HeaderSizeV2} { // flags, length, pos, prevLen, store id, payload
		b := bytes.Clone(buf.Bytes())
		b[off] ^= 0x01
		if _, err := Decode(bytes.NewReader(b)); err == nil {
			t.Errorf("a flipped bit at header offset %d decoded cleanly", off)
		}
	}
}

// appendFrames appends n frames of size bytes, each as its own synced run, and
// returns the watermark of the last.
func appendFrames(t *testing.T, w *Writer, n, size int, tag byte) int64 {
	t.Helper()
	var mark int64
	for range n {
		var err error
		mark, err = w.AppendRun(func(emit func([]byte) error) error { return emit(bytes.Repeat([]byte{tag}, size)) })
		if err != nil {
			t.Fatalf("AppendRun: %v", err)
		}
		if err := w.SyncGroup(mark); err != nil {
			t.Fatalf("SyncGroup: %v", err)
		}
	}
	return mark
}

// TestWriter_RolloverKeepsRunsWholeAndPositionsContinuous: with 1 MiB segments
// a run of four 100 KiB frames lands whole in one segment, positions continue
// across segments and across a reopen, and the log reads every frame back.
func TestWriter_RolloverKeepsRunsWholeAndPositionsContinuous(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	const runs, perRun, size = 30, 4, 100 << 10
	for r := range runs {
		mark, err := w.AppendRun(func(emit func([]byte) error) error {
			for range perRun {
				if err := emit(bytes.Repeat([]byte{byte(r)}, size)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.SyncGroup(mark); err != nil {
			t.Fatal(err)
		}
	}
	end := w.DurableOffset()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	segs := segmentPaths(t, walPath)
	if len(segs) < 3 {
		t.Fatalf("%d segments, want rollovers to have produced at least 3", len(segs))
	}
	for _, p := range segs {
		b, err := os.ReadFile(p) //nolint:gosec // test path
		if err != nil {
			t.Fatal(err)
		}
		if len(b) <= segHeaderSize {
			continue
		}
		if (len(b)-segHeaderSize)%(perRun*(HeaderSizeV2+size)) != 0 {
			t.Fatalf("segment %s holds %d frame bytes, not a whole number of %d-frame runs: a run was split", p, len(b)-segHeaderSize, perRun)
		}
	}
	w2, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	if got := w2.DurableOffset(); got != end {
		t.Fatalf("reopened at position %d, the log ended at %d", got, end)
	}
	appendFrames(t, w2, 1, 10, 0xEE)
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	frames := 0
	for range log.Frames() {
		frames++
	}
	if log.TailError() != nil || frames != runs*perRun+1 {
		t.Fatalf("log read %d frames (tail %v), want %d", frames, log.TailError(), runs*perRun+1)
	}
}

// faultSyncFS is the OS filesystem with one switch: while failSync is set, the
// Sync of an open segment file fails.
type faultSyncFS struct {
	osWALFS
	failSync atomic.Bool
}

type faultSyncFile struct {
	*os.File
	fs *faultSyncFS
}

func (f *faultSyncFile) Sync() error {
	if f.fs.failSync.Load() {
		return errors.New("injected fsync failure")
	}
	return f.File.Sync()
}

func (fs *faultSyncFS) OpenFile(path string, flag int) (WALFile, error) {
	f, err := os.OpenFile(path, flag, 0o600) //nolint:gosec // test path
	if err != nil {
		return nil, err
	}
	return &faultSyncFile{File: f, fs: fs}, nil
}

// TestWriter_RolloverFsyncFailurePoisons is the §8 fault point
// wal.rollover.fsync-fails: the fdatasync that must make the old segment
// durable before the switch fails. The writer poisons before switching, the
// run that triggered the rollover is refused, and a reopen finds exactly the
// acknowledged frames.
func TestWriter_RolloverFsyncFailurePoisons(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	fs := &faultSyncFS{}
	w, err := OpenFSWithOptions(fs, walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	appendFrames(t, w, 16, 64<<10, 0xA0) // segment 1 at its target size
	acked := w.DurableOffset()
	fs.failSync.Store(true)
	_, err = w.AppendRun(func(emit func([]byte) error) error { return emit([]byte("triggers the rollover")) })
	if !errors.Is(err, ErrDurabilityFailed) {
		t.Fatalf("the run that triggered a failing rollover returned %v, want ErrDurabilityFailed", err)
	}
	if w.Poisoned() == nil {
		t.Fatal("the writer is not poisoned after the rollover fsync failed")
	}
	if _, statErr := os.Stat(SegmentPath(walPath, 2)); statErr == nil {
		b, _ := os.ReadFile(SegmentPath(walPath, 2))
		if len(b) > segHeaderSize {
			t.Fatal("a frame reached segment 2 although the switch must not happen after a failed fsync")
		}
	}
	fs.failSync.Store(false)
	_ = w.Close()
	w2, err := OpenFSWithOptions(fs, walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := w2.DurableOffset(); got != acked {
		t.Fatalf("reopened at %d, the acknowledged log ended at %d", got, acked)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpen_DeletesUnfinishedSpareAndLeftovers: an interrupted spare creation (a
// trailing segment with a short header) and segments wholly below the oldest
// retained position (an interrupted unlink) are ignored by readers and deleted
// by the next writable open.
func TestOpen_DeletesUnfinishedSpareAndLeftovers(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	appendFrames(t, w, 40, 64<<10, 0xB0) // three segments
	// The control write of a checkpoint whose unlink never ran.
	if err := w.MarkCheckpoint(w.DurableOffset()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	last := uint64(len(segmentPaths(t, walPath)))
	unfinished := SegmentPath(walPath, last+1)
	if err := os.WriteFile(unfinished, []byte("GGWS"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLog(walPath); err != nil {
		t.Fatalf("a reader refused leftovers and an unfinished spare: %v", err)
	}
	w2, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Close() }()
	for _, no := range []uint64{1, 2} {
		if _, err := os.Stat(SegmentPath(walPath, no)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("leftover segment %d below the oldest retained position survived the open: %v", no, err)
		}
	}
	if b, err := os.ReadFile(unfinished); err == nil && len(b) < segHeaderSize { //nolint:gosec // test path
		t.Error("the unfinished spare survived the open")
	}
}

// TestWriter_PreparerCreatesTheNextSegment: an [Open]ed writer prepares the next
// segment in the background, and Close waits for the preparer (the package's
// goleak TestMain fails the run if a preparer goroutine outlives its writer).
func TestWriter_PreparerCreatesTheNextSegment(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(SegmentPath(walPath, 2)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the preparer did not create segment 2")
		}
		time.Sleep(time.Millisecond)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// The spare is a valid empty tail segment: the log reads clean.
	log, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	for range log.Frames() {
	}
	if log.TailError() != nil {
		t.Fatalf("log over an empty spare: %v", log.TailError())
	}
}

// TestWriter_MarkCheckpointAndReclaimSegments: the control file records OR as
// the first position of the segment holding the redo position and the frame
// before it; ReclaimSegments unlinks exactly the segments wholly below OR and
// the log then starts at OR with a valid prev-link.
func TestWriter_MarkCheckpointAndReclaimSegments(t *testing.T) {
	t.Parallel()
	walPath := filepath.Join(t.TempDir(), "wal")
	w, err := OpenWithOptions(walPath, Options{SegmentSize: MinSegmentSize})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	appendFrames(t, w, 40, 64<<10, 0xC0)
	redo := w.DurableOffset()
	if err := w.MarkCheckpoint(redo); err != nil {
		t.Fatal(err)
	}
	log, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := log.Control()
	seg3 := log.list.segs[2]
	if ctl.Flags&ControlPrefixTruncated == 0 || int64(ctl.OldestRetainedPos) != seg3.firstPos ||
		ctl.CheckpointRedoPos != uint64(redo) || ctl.PrevFramePosAtOR != uint64(seg3.firstPos-int64(seg3.firstPrevLen)) {
		t.Fatalf("control %+v, segment 3 starts at %d", ctl, seg3.firstPos)
	}
	reclaimed, err := w.ReclaimSegments()
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != seg3.firstPos {
		t.Fatalf("reclaimed %d log bytes, want %d", reclaimed, seg3.firstPos)
	}
	for _, no := range []uint64{1, 2} {
		if _, err := os.Stat(SegmentPath(walPath, no)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("segment %d survived the reclaim: %v", no, err)
		}
	}
	log2, err := OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	for range log2.Frames() {
	}
	if log2.TailError() != nil || log2.TailOffset() != redo {
		t.Fatalf("log after the reclaim: tail %v end %d, want clean end %d", log2.TailError(), log2.TailOffset(), redo)
	}
}

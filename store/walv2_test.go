package store_test

// walv2_test.go — WAL v2 step 2 (docs/design-wal-v2.md §9): the segmented
// container's typed refusals (#3020), the snapshot-reaches-WAL check (#3014),
// compatibility with stores written before the segmented format, and a
// persisted multi-segment round trip.
//
// Every tamper below recomputes the CRCs it invalidates, so what recovery
// refuses is the container's own consistency (position, prev-link, store id,
// segment numbering), never a checksum.
//
// Layer: short.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// recoverDir runs recovery over dir with the store's codecs.
func recoverDir(dir string) (recovery.Result[string, float64], error) {
	return recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
}

// requireRefused asserts recovery and store.Open both refuse dir with want.
func requireRefused(t *testing.T, dir string, want error) {
	t.Helper()
	res, err := recoverDir(dir)
	if !errors.Is(err, want) {
		t.Fatalf("recovery.Open = %v, want %v", err, want)
	}
	if res.IsClean() {
		t.Fatalf("IsClean() = true on a refused recovery (%v)", err)
	}
	if _, err := store.Open[string, float64](dir, openOptions()); !errors.Is(err, store.ErrUncleanRecovery) || !errors.Is(err, want) {
		t.Fatalf("store.Open = %v, want ErrUncleanRecovery wrapping %v", err, want)
	}
}

// committedDir builds a store with n single-node transactions through
// store.Open and closes it.
func committedDir(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for i := range n {
		commitNodes(t, o.Store(), "k"+strconv.Itoa(i))
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir
}

// tamperFrame rewrites frame idx of the log of dir in place through mut, with
// its CRC recomputed. mut must keep the encoded size.
func tamperFrame(t *testing.T, dir string, idx int, mut func(*wal.Frame)) {
	t.Helper()
	locs, err := waltest.LocateFrames(filepath.Join(dir, "wal"))
	if err != nil || idx >= len(locs) {
		t.Fatalf("locate frame %d: %v (%d frames)", idx, err, len(locs))
	}
	l := locs[idx]
	f := l.Frame
	mut(&f)
	var buf bytes.Buffer
	if _, err := wal.Encode(&buf, f); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if int64(buf.Len()) != l.Size {
		t.Fatalf("tamper changed the frame size %d -> %d", l.Size, buf.Len())
	}
	writeAt(t, l.Path, l.Offset, buf.Bytes())
}

func writeAt(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: path under t.TempDir
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteAt(b, off); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWALv2_TamperIsRefused pins the #3020 sentinels: a frame moved to another
// position, a broken prev-link, a frame and a segment of another store, and a
// control file of another store are refused with their typed errors, and the
// refusal is open-fatal.
func TestWALv2_TamperIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		want   error
		tamper func(t *testing.T, dir string)
	}{
		{"frame_moved_to_another_position", wal.ErrFramePosition, func(t *testing.T, dir string) {
			tamperFrame(t, dir, 3, func(f *wal.Frame) { f.Pos++; f.PrevLen++ })
		}},
		{"broken_prev_link", wal.ErrPrevLink, func(t *testing.T, dir string) {
			tamperFrame(t, dir, 3, func(f *wal.Frame) { f.PrevLen++ })
		}},
		{"frame_of_another_store", wal.ErrForeignStore, func(t *testing.T, dir string) {
			tamperFrame(t, dir, 3, func(f *wal.Frame) { f.StoreID ^= 1 })
		}},
		{"segment_of_another_store", wal.ErrForeignStore, func(t *testing.T, dir string) {
			segs, err := waltest.SegmentPaths(filepath.Join(dir, "wal"))
			if err != nil || len(segs) == 0 {
				t.Fatalf("segments: %v", err)
			}
			hdr := readPrefix(t, segs[0], 32)
			binary.LittleEndian.PutUint64(hdr[8:16], binary.LittleEndian.Uint64(hdr[8:16])^1)
			binary.LittleEndian.PutUint32(hdr[28:32], crc32.Checksum(hdr[0:28], castagnoli))
			writeAt(t, segs[0], 0, hdr)
		}},
		{"control_of_another_store", wal.ErrForeignStore, func(t *testing.T, dir string) {
			p := wal.ControlPath(filepath.Join(dir, "wal"))
			b := readPrefix(t, p, 64)
			binary.LittleEndian.PutUint64(b[8:16], binary.LittleEndian.Uint64(b[8:16])^1)
			binary.LittleEndian.PutUint32(b[56:60], crc32.Checksum(b[0:56], castagnoli))
			writeAt(t, p, 0, b)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := committedDir(t, 4)
			tc.tamper(t, dir)
			requireRefused(t, dir, tc.want)
		})
	}
}

func readPrefix(t *testing.T, path string, n int) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path under t.TempDir
	if err != nil || len(b) < n {
		t.Fatalf("read %s: %v (%d bytes)", path, err, len(b))
	}
	return b[:n]
}

// segStore is a store over a WAL of 1 MiB segments, built the way store.Open
// builds one, so a test can drive rollovers and checkpoints that reclaim whole
// segments.
type segStore struct {
	dir string
	w   *wal.Writer
	st  *txn.Store[string, float64]
}

func openSegStore(t *testing.T, dir string) *segStore {
	t.Helper()
	res, err := recoverDir(dir)
	if err != nil || !res.IsClean() {
		t.Fatalf("recover %s: %v (clean %t)", dir, err, res.IsClean())
	}
	w, err := wal.OpenWithOptions(filepath.Join(dir, "wal"), wal.Options{SegmentSize: wal.MinSegmentSize})
	if err != nil {
		t.Fatalf("wal.OpenWithOptions: %v", err)
	}
	return &segStore{dir: dir, w: w, st: res.NewStore(w, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})}
}

// fill commits n transactions of one node with a 64 KiB property each.
func (s *segStore) fill(t *testing.T, prefix string, n int) {
	t.Helper()
	big := strings.Repeat("x", 64<<10)
	for i := range n {
		tx := s.st.Begin()
		k := prefix + strconv.Itoa(i)
		if err := tx.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := tx.SetNodeProperty(k, "fill", lpg.StringValue(big)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *segStore) checkpoint(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: s.dir}, s.st.Graph(), s.w, &mu,
		checkpoint.WithCommitSerialiser[string, float64](s.st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
}

func (s *segStore) close(t *testing.T) {
	t.Helper()
	if err := s.w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// TestWALv2_SegmentGapAndTornSegment pins the two structural refusals of a
// multi-segment log: a missing middle segment is ErrSegmentGap, and a torn
// frame in a segment that is not the tail is ErrTornSegment (every non-tail
// segment is fully durable before its successor receives a byte).
func TestWALv2_SegmentGapAndTornSegment(t *testing.T) {
	t.Parallel()
	build := func(t *testing.T) string {
		dir := t.TempDir()
		s := openSegStore(t, dir)
		s.fill(t, "g", 40) // about 2.6 MiB: three segments
		s.close(t)
		if _, err := os.Stat(wal.SegmentPath(filepath.Join(dir, "wal"), 3)); err != nil {
			t.Fatalf("the workload did not reach a third segment: %v", err)
		}
		return dir
	}
	t.Run("missing_middle_segment", func(t *testing.T) {
		t.Parallel()
		dir := build(t)
		if err := os.Remove(wal.SegmentPath(filepath.Join(dir, "wal"), 2)); err != nil {
			t.Fatal(err)
		}
		requireRefused(t, dir, wal.ErrSegmentGap)
	})
	t.Run("torn_non_tail_segment", func(t *testing.T) {
		t.Parallel()
		dir := build(t)
		p := wal.SegmentPath(filepath.Join(dir, "wal"), 1)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, fi.Size()-5); err != nil {
			t.Fatal(err)
		}
		requireRefused(t, dir, wal.ErrTornSegment)
	})
}

// TestWALv2_OlderGenerationFrameIsRefused: after a truncation the control file
// names the frame that precedes the oldest retained position. A first retained
// frame whose prev-link names another predecessor — a frame of an older
// generation of the log — is ErrPrevLink.
func TestWALv2_OlderGenerationFrameIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "a", "b")
	var mu sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := o.WAL().Truncate(); err != nil { // OR now names the next frame's predecessor
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "c")
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := recoverDir(dir); err != nil || !res.IsClean() {
		t.Fatalf("control: the untampered directory must recover clean: %v", err)
	}
	tamperFrame(t, dir, 0, func(f *wal.Frame) { f.PrevLen++ })
	requireRefused(t, dir, wal.ErrPrevLink)
}

// TestWALv2_SnapshotReachesWAL pins #3014: a snapshot older than the oldest
// retained position, a snapshot ahead of the end of the log, a missing
// snapshot over a truncated log, and a snapshot of another store are refused
// with typed errors.
func TestWALv2_SnapshotReachesWAL(t *testing.T) {
	t.Parallel()
	t.Run("snapshot_older_than_retained_log", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := openSegStore(t, dir)
		s.fill(t, "a", 2)
		s.checkpoint(t) // snapshot A, redo position in segment 1
		saved := filepath.Join(t.TempDir(), "snapshot-A")
		copyDirTree(t, filepath.Join(dir, "snapshot"), saved)
		s.fill(t, "b", 40) // reach segment 3
		s.checkpoint(t)    // snapshot B unlinks segments 1 and 2
		s.close(t)
		if _, err := os.Stat(wal.SegmentPath(filepath.Join(dir, "wal"), 1)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("checkpoint B did not unlink segment 1: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(dir, "snapshot")); err != nil {
			t.Fatal(err)
		}
		copyDirTree(t, saved, filepath.Join(dir, "snapshot"))
		requireRefused(t, dir, recovery.ErrSnapshotTooOld)
	})
	t.Run("snapshot_ahead_of_wal", func(t *testing.T) {
		t.Parallel()
		dir := committedDir(t, 3)
		old := filepath.Join(t.TempDir(), "old-wal")
		copyLog(t, dir, old)
		o, err := store.Open[string, float64](dir, openOptions())
		if err != nil {
			t.Fatal(err)
		}
		commitNodes(t, o.Store(), "later-1", "later-2")
		var mu sync.Mutex
		cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
			checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
			checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
			checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
		if err := cp.RunCheckpoint(); err != nil {
			t.Fatal(err)
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
		restoreLog(t, old, dir) // the log as it stood before the later commits
		requireRefused(t, dir, recovery.ErrSnapshotAheadOfWAL)
	})
	t.Run("missing_snapshot_over_reclaimed_segments", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := openSegStore(t, dir)
		s.fill(t, "a", 40)
		s.checkpoint(t)
		s.close(t)
		log, err := wal.OpenLog(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatal(err)
		}
		if ctl, _ := log.Control(); ctl.OldestRetainedPos == 0 {
			t.Fatal("the checkpoint reclaimed no segment: the retained log still starts at position 0")
		}
		if err := os.RemoveAll(filepath.Join(dir, "snapshot")); err != nil {
			t.Fatal(err)
		}
		requireRefused(t, dir, recovery.ErrMissingSnapshot)
	})
	t.Run("snapshot_of_another_store", func(t *testing.T) {
		t.Parallel()
		mk := func() string {
			dir := t.TempDir()
			o, err := store.Open[string, float64](dir, openOptions())
			if err != nil {
				t.Fatal(err)
			}
			commitNodes(t, o.Store(), "x")
			var mu sync.Mutex
			cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
				checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
				checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
				checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
			if err := cp.RunCheckpoint(); err != nil {
				t.Fatal(err)
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			return dir
		}
		mine, theirs := mk(), mk()
		if err := os.RemoveAll(filepath.Join(mine, "snapshot")); err != nil {
			t.Fatal(err)
		}
		copyDirTree(t, filepath.Join(theirs, "snapshot"), filepath.Join(mine, "snapshot"))
		requireRefused(t, mine, recovery.ErrForeignSnapshot)
	})
}

// copyLog copies the WAL files of dir (legacy file, control file, segments)
// into dst.
func copyLog(t *testing.T, dir, dst string) {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	copyFile(t, walPath, filepath.Join(dst, "wal"))
	copyFile(t, wal.ControlPath(walPath), filepath.Join(dst, "wal.control"))
	copyDirTree(t, wal.SegmentDir(walPath), filepath.Join(dst, "wal.d"))
}

// restoreLog replaces the WAL files of dir with the copy in src.
func restoreLog(t *testing.T, src, dir string) {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	if err := os.RemoveAll(wal.SegmentDir(walPath)); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(src, "wal"), walPath)
	copyFile(t, filepath.Join(src, "wal.control"), wal.ControlPath(walPath))
	copyDirTree(t, filepath.Join(src, "wal.d"), wal.SegmentDir(walPath))
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src) //nolint:gosec // test paths
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil { //nolint:gosec // G703: test paths under t.TempDir
		t.Fatalf("write %s: %v", dst, err)
	}
}

func copyDirTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		copyFile(t, p, filepath.Join(dst, rel))
		return nil
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

// fixtureIDs reads the node ids the pre-v2 build recorded for a fixture.
func fixtureIDs(t *testing.T, path string) map[string]uint64 {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), " ")
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("fixture id %q: %v", sc.Text(), err)
		}
		out[k] = n
	}
	return out
}

// requireIDs asserts every fixture key resolves to the id the pre-v2 build gave it.
func requireIDs(t *testing.T, o *store.Opened[string, float64], want map[string]uint64, stage string) {
	t.Helper()
	for k, id := range want {
		got, ok := o.Graph().AdjList().Mapper().Lookup(k)
		if !ok || uint64(got) != id {
			t.Errorf("%s: key %q has id %d (present %t), the pre-v2 build gave it %d", stage, k, got, ok, id)
		}
	}
}

// legacyReaderRefuses reports whether a reader of the pre-v2 build refuses the
// file at path. That reader accepted frame versions up to 1 and returned
// ErrUnsupportedVersion for anything newer (store/wal/format.go:216-220 at
// commit 2f64a880), so the file is refused exactly when one of its frames, read
// in order, carries a version above [wal.LegacyVersion].
func legacyReaderRefuses(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r := wal.NewReader(f, nil)
	for fr := range r.Frames() {
		if fr.Version > wal.LegacyVersion {
			return true
		}
	}
	return false
}

// TestWALv2_LegacyStoreMigratesKeepingIDs opens stores written by the build
// before the segmented format (frozen under testdata/walv1: a plain log, and a
// checkpointed one with a truncated prefix and its marker). The first writable
// open migrates each: the legacy history replays as before, every node keeps
// the id the old build gave it, the legacy file is sealed so the old build's
// reader refuses the directory, and new commits land in segments. A checkpoint
// then replaces the legacy file with its seal stub, and the ids still hold.
func TestWALv2_LegacyStoreMigratesKeepingIDs(t *testing.T) {
	t.Parallel()
	for _, fx := range []string{"plain", "checkpoint"} {
		t.Run(fx, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			copyDirTree(t, filepath.Join("testdata", "walv1", fx), dir)
			ids := fixtureIDs(t, filepath.Join(dir, "ids.txt"))
			walPath := filepath.Join(dir, "wal")
			if legacyReaderRefuses(t, walPath) {
				t.Fatal("control: the frozen fixture must be readable by the pre-v2 reader")
			}

			o, err := store.Open[string, float64](dir, openOptions())
			if err != nil {
				t.Fatalf("store.Open on the legacy store: %v", err)
			}
			requireIDs(t, o, ids, "after migration")
			commitNodes(t, o.Store(), "after-migration")
			log, err := wal.OpenLog(walPath)
			if err != nil {
				t.Fatal(err)
			}
			ctl, ok := log.Control()
			if !ok || ctl.Flags&wal.ControlLegacyV1Pending == 0 {
				t.Fatalf("the migration did not record the pending legacy log: %+v (present %t)", ctl, ok)
			}
			if !legacyReaderRefuses(t, walPath) {
				t.Fatal("the sealed legacy log is still accepted by the pre-v2 reader: an old build would ignore the segments")
			}

			var mu sync.Mutex
			cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
				checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
				checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
				checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
			if err := cp.RunCheckpoint(); err != nil {
				t.Fatalf("RunCheckpoint: %v", err)
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			stub, err := os.ReadFile(walPath) //nolint:gosec // test path
			if err != nil {
				t.Fatal(err)
			}
			frames := 0
			for range wal.NewReader(bytes.NewReader(stub), nil).Frames() {
				frames++
			}
			if frames != 1 || !legacyReaderRefuses(t, walPath) {
				t.Fatalf("after the checkpoint the legacy file holds %d frame(s) (want the 1-frame seal stub) and is refused=%t",
					frames, legacyReaderRefuses(t, walPath))
			}
			o2, err := store.Open[string, float64](dir, openOptions())
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = o2.Close() }()
			requireIDs(t, o2, ids, "after the stub")
			if !has(o2.Graph().AdjList().Mapper(), "after-migration") {
				t.Fatal("the commit made after the migration was lost")
			}
		})
	}
}

// TestWALv2_PersistedMultiSegmentRoundTrip commits across three segments,
// checkpoints, commits more, and reopens twice: every committed key survives
// with its id and its property, and the second reopen sees the same graph.
func TestWALv2_PersistedMultiSegmentRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openSegStore(t, dir)
	s.fill(t, "pre", 40)
	s.checkpoint(t)
	s.fill(t, "post", 20)
	ids := map[string]uint64{}
	for _, k := range []string{"pre0", "pre39", "post0", "post19"} {
		id, ok := s.st.Graph().AdjList().Mapper().Lookup(k)
		if !ok {
			t.Fatalf("live key %q missing", k)
		}
		ids[k] = uint64(id)
	}
	s.close(t)
	for round := range 2 {
		o, err := store.Open[string, float64](dir, openOptions())
		if err != nil {
			t.Fatalf("reopen %d: %v", round, err)
		}
		requireIDs(t, o, ids, "reopen "+strconv.Itoa(round))
		for _, k := range []string{"pre0", "pre39", "post0", "post19"} {
			v, ok := o.Graph().GetNodeProperty(k, "fill")
			if s, _ := v.String(); !ok || len(s) != 64<<10 {
				t.Fatalf("reopen %d: %q lost its property", round, k)
			}
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

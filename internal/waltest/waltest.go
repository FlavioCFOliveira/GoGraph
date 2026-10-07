// Package waltest locates the files of a segmented write-ahead log for tests
// and examples that inspect or damage WAL bytes directly. It reads the
// operating-system filesystem and has no side effects; every function is safe
// for concurrent use.
package waltest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// SegmentHeaderSize is the size of a segment file's header; its first frame
// starts at this offset.
const SegmentHeaderSize = 32

// SegmentPaths returns the segment files of the WAL at walPath, oldest first.
func SegmentPaths(walPath string) ([]string, error) {
	ents, err := os.ReadDir(wal.SegmentDir(walPath))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".wal") && len(n) == 16+len(".wal") {
			out = append(out, filepath.Join(wal.SegmentDir(walPath), n))
		}
	}
	slices.Sort(out) // fixed-width hex names sort numerically
	return out, nil
}

// TailSegmentPath returns the newest segment of the WAL at walPath that holds
// at least one frame byte: the segment a crash tears and the one the writer
// appends to. Empty spare segments after it are skipped.
func TailSegmentPath(walPath string) (string, error) {
	segs, err := SegmentPaths(walPath)
	if err != nil {
		return "", err
	}
	for i := len(segs) - 1; i >= 0; i-- {
		fi, err := os.Stat(segs[i])
		if err != nil {
			return "", err
		}
		if fi.Size() > SegmentHeaderSize {
			return segs[i], nil
		}
	}
	return "", fmt.Errorf("waltest: no segment of %s holds a frame", walPath)
}

// FrameBytes returns the frame bytes of every segment of the WAL at walPath,
// headers stripped, concatenated oldest first: a frame stream a [wal.Reader]
// decodes.
func FrameBytes(walPath string) ([]byte, error) {
	segs, err := SegmentPaths(walPath)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, p := range segs {
		b, err := os.ReadFile(p) //nolint:gosec // test helper over a caller-supplied WAL path
		if err != nil {
			return nil, err
		}
		if len(b) > SegmentHeaderSize {
			buf.Write(b[SegmentHeaderSize:])
		}
	}
	return buf.Bytes(), nil
}

// Frames decodes every frame of every segment of the WAL at walPath, oldest
// first, stopping at the first undecodable frame.
func Frames(walPath string) ([]wal.Frame, error) {
	b, err := FrameBytes(walPath)
	if err != nil {
		return nil, err
	}
	var out []wal.Frame
	r := wal.NewReader(bytes.NewReader(b), nil)
	for f := range r.Frames() {
		out = append(out, f)
	}
	return out, nil
}

// CheckpointRecorded reports whether the last checkpoint of the store in dir
// recorded its snapshot in the WAL control file: the control file has the
// prefix-truncated flag and its checkpoint redo position equals the redo
// position the published snapshot (dir/snapshot) records. That is the state in
// which recovery starts from the snapshot alone, whether or not a whole
// segment could be unlinked. redo is the snapshot's redo position.
func CheckpointRecorded(dir string) (redo uint64, ok bool, err error) {
	walPath := filepath.Join(dir, "wal")
	log, err := wal.OpenLog(walPath)
	if err != nil {
		return 0, false, err
	}
	ctl, segmented := log.Control()
	if !segmented {
		return 0, false, nil
	}
	m, err := snapshot.ReadManifestFile(filepath.Join(dir, "snapshot", "manifest.json"))
	if err != nil {
		return 0, false, err
	}
	if m.WALFormat != snapshot.WALFormatSegmented {
		return 0, false, nil
	}
	return m.WALRedoPos, ctl.Flags&wal.ControlPrefixTruncated != 0 && ctl.CheckpointRedoPos == m.WALRedoPos, nil
}

// FrameLoc is one frame of a segmented log and where it lies on disk.
type FrameLoc struct {
	Frame wal.Frame
	// Path is the segment file holding the frame.
	Path string
	// Offset is the file offset of the frame's first header byte.
	Offset int64
	// Size is the frame's encoded size.
	Size int64
}

// LocateFrames returns every decodable frame of every segment of the WAL at
// walPath, oldest first, with its file and offset, stopping in each segment at
// the first undecodable frame.
func LocateFrames(walPath string) ([]FrameLoc, error) {
	segs, err := SegmentPaths(walPath)
	if err != nil {
		return nil, err
	}
	var out []FrameLoc
	for _, p := range segs {
		b, err := os.ReadFile(p) //nolint:gosec // test helper over a caller-supplied WAL path
		if err != nil {
			return nil, err
		}
		if len(b) <= SegmentHeaderSize {
			continue
		}
		r := bytes.NewReader(b[SegmentHeaderSize:])
		off := int64(SegmentHeaderSize)
		for {
			f, derr := wal.Decode(r)
			if derr != nil {
				break
			}
			size := int64(wal.FrameSize(f))
			out = append(out, FrameLoc{Frame: f, Path: p, Offset: off, Size: size})
			off += size
		}
	}
	return out, nil
}

// RewriteFrames rewrites every segment of the WAL at walPath through fn, which
// may change a frame's payload in place (fn's frame is a copy it owns) and
// returns false to drop the frame. Every surviving frame is re-encoded with a
// recomputed position, prev-link and CRC, starting from the first frame's
// original position, so the result is a structurally valid segmented log whose
// content differs only as fn decided. The control file is left as it is, so a
// rewrite that moves the position of the oldest retained frame or of a
// snapshot's redo position is the caller's to reason about.
func RewriteFrames(walPath string, fn func(i int, f *wal.Frame) bool) error {
	locs, err := LocateFrames(walPath)
	if err != nil {
		return err
	}
	if len(locs) == 0 {
		return fmt.Errorf("waltest: no frame in %s", walPath)
	}
	pos, prev := locs[0].Frame.Pos, int64(-1)
	if locs[0].Frame.PrevLen != 0 {
		prev = int64(locs[0].Frame.Pos) - int64(locs[0].Frame.PrevLen) //nolint:gosec // G115: test positions are small
	}
	byPath := map[string]*bytes.Buffer{}
	for _, p := range uniquePaths(locs) {
		b, err := os.ReadFile(p) //nolint:gosec // test helper over a caller-supplied WAL path
		if err != nil {
			return err
		}
		byPath[p] = bytes.NewBuffer(append([]byte(nil), b[:SegmentHeaderSize]...))
	}
	for i, l := range locs {
		f := l.Frame
		f.Payload = append([]byte(nil), f.Payload...)
		if !fn(i, &f) {
			continue
		}
		f.Version, f.Pos, f.PrevLen = wal.CurrentVersion, pos, 0
		if prev >= 0 {
			f.PrevLen = uint32(int64(pos) - prev) //nolint:gosec // G115: one frame's distance
		}
		n, err := wal.Encode(byPath[l.Path], f)
		if err != nil {
			return err
		}
		prev = int64(pos) //nolint:gosec // G115: test positions are small
		pos += uint64(n)  //nolint:gosec // G115: n is non-negative
	}
	for p, buf := range byPath {
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func uniquePaths(locs []FrameLoc) []string {
	var out []string
	for _, l := range locs {
		if len(out) == 0 || out[len(out)-1] != l.Path {
			out = append(out, l.Path)
		}
	}
	return out
}

// LogImage returns the bytes of every file of the WAL at walPath — the legacy
// file, the control file and each segment — concatenated in that order, for a
// test that asserts nothing in the log was modified. Missing files contribute
// nothing.
func LogImage(walPath string) ([]byte, error) {
	var buf bytes.Buffer
	segs, err := SegmentPaths(walPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, p := range append([]string{walPath, wal.ControlPath(walPath)}, segs...) {
		b, err := os.ReadFile(p) //nolint:gosec // test helper over a caller-supplied WAL path
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		fmt.Fprintf(&buf, "%s:%d\n", filepath.Base(p), len(b))
		buf.Write(b)
	}
	return buf.Bytes(), nil
}

// AppendFrame appends one durable frame carrying payload to the tail segment of
// the WAL at walPath, after its last frame, with the position, prev-link and
// store id a writer would have given it. It models a frame written by a process
// that then died.
func AppendFrame(walPath string, payload []byte) error {
	locs, err := LocateFrames(walPath)
	if err != nil {
		return err
	}
	if len(locs) == 0 {
		return fmt.Errorf("waltest: no frame in %s to append after", walPath)
	}
	last := locs[len(locs)-1]
	var buf bytes.Buffer
	if _, err := wal.Encode(&buf, wal.Frame{
		Version: wal.CurrentVersion,
		Payload: payload,
		Pos:     last.Frame.Pos + uint64(last.Size), //nolint:gosec // G115: a frame size is positive
		PrevLen: uint32(last.Size),                  //nolint:gosec // G115: one frame's size
		StoreID: last.Frame.StoreID,
	}); err != nil {
		return err
	}
	f, err := os.OpenFile(last.Path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(buf.Bytes(), last.Offset+last.Size); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

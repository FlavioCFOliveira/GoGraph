package wal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"slices"
)

// LogFS is the read-only filesystem surface [OpenLogFS] reads a WAL through:
// open a file by path and list a directory's entry names. Both return an error
// satisfying errors.Is(err, os.ErrNotExist) when the path does not exist.
//
// Production code uses [OpenLog], which reads the operating-system filesystem;
// the deterministic-simulation harness supplies its in-memory disk.
//
// Concurrency: a [Log] calls its LogFS from one goroutine at a time; an
// implementation shared by several Logs must be safe for concurrent use.
type LogFS interface {
	Open(path string) (io.ReadCloser, error)
	ReadDir(dir string) ([]string, error)
}

// osLogFS reads the operating-system filesystem.
type osLogFS struct{}

func (osLogFS) Open(path string) (io.ReadCloser, error) {
	return os.OpenFile(path, os.O_RDONLY|walNoFollow, 0) //nolint:gosec // caller-supplied WAL path is by-design; walNoFollow rejects a symlinked final component (CWE-59)
}

func (osLogFS) ReadDir(dir string) ([]string, error) { return readDirNames(dir) }

func readDirNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

// walLogFS adapts the writer's [walFS] seam to [LogFS].
type walLogFS struct{ fsys walFS }

func (a walLogFS) Open(path string) (io.ReadCloser, error) { return a.fsys.OpenFile(path, os.O_RDONLY) }

func (a walLogFS) ReadDir(dir string) ([]string, error) { return a.fsys.ReadDir(dir) }

// FrameSource is what a replay consumes: an iterator over frames, and after it
// ends the reason it stopped and the end of the last consumed frame. [*Reader]
// (one file) and [*Log] (a store's segmented log) both implement it.
//
// Concurrency: implementations are NOT safe for concurrent use; one goroutine
// iterates a source and then reads its tail.
type FrameSource interface {
	Frames() iter.Seq[Frame]
	TailError() error
	TailOffset() int64
}

var (
	_ FrameSource = (*Reader)(nil)
	_ FrameSource = (*Log)(nil)
)

// segEntry is one enumerated segment.
type segEntry struct {
	path     string
	no       uint64
	firstPos int64 // position of its first frame; -1 when it holds none
	// firstPrevLen is the prevLen of its first frame.
	firstPrevLen uint32
	// partial reports bytes after the header that do not form a complete
	// first frame (a torn first frame).
	partial bool
}

// segListing is the validated segment set of a log.
type segListing struct {
	segs []segEntry
	// unfinished are trailing segments with a short or invalid header and no
	// frames: interrupted spare creations, ignored and deleted by the writer.
	unfinished []string
	// leftovers are segments wholly below OR: interrupted unlinks, ignored and
	// deleted by the writer.
	leftovers []string
	// start indexes segs at the segment that holds OR.
	start int
}

// listSegments enumerates and validates the segments of walPath against the
// control file c: headers, store id, names and consecutive numbering, and the
// position of each segment's first frame.
func listSegments(fsys LogFS, walPath string, c Control) (segListing, error) {
	var out segListing
	names, err := fsys.ReadDir(SegmentDir(walPath))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("wal: list segments: %w", err)
	}
	nos := make([]uint64, 0, len(names))
	for _, n := range names {
		if no, ok := parseSegmentName(n); ok {
			nos = append(nos, no)
		}
	}
	slices.Sort(nos)
	for _, no := range nos {
		path := SegmentPath(walPath, no)
		e, hdrOK, err := probeSegment(fsys, path, no, c.StoreID)
		if err != nil {
			return out, err
		}
		if !hdrOK {
			out.unfinished = append(out.unfinished, path)
			continue
		}
		if len(out.unfinished) > 0 {
			// A valid segment after an unfinished one: the unfinished one is
			// not trailing, so it is corruption, not an interrupted spare.
			return out, fmt.Errorf("%w: %s", ErrSegmentHeader, out.unfinished[0])
		}
		if n := len(out.segs); n > 0 && out.segs[n-1].no+1 != no {
			return out, fmt.Errorf("%w: segment %d follows segment %d", ErrSegmentGap, no, out.segs[n-1].no)
		}
		out.segs = append(out.segs, e)
	}
	// The start segment is the last one whose first frame lies at or below OR;
	// every segment before it lies wholly below OR.
	or := int64(c.OldestRetainedPos) //nolint:gosec // G115: positions are bounded by int64 file arithmetic
	start, framed := -1, false
	for i, s := range out.segs {
		if s.firstPos < 0 {
			continue
		}
		framed = true
		if s.firstPos <= or {
			start = i
		}
	}
	switch {
	case start >= 0:
	case framed:
		return out, fmt.Errorf("%w: no retained segment holds position %d", ErrSegmentGap, or)
	default:
		start = 0
	}
	out.start = start
	for i := 0; i < start; i++ {
		out.leftovers = append(out.leftovers, out.segs[i].path)
	}
	return out, nil
}

// probeSegment reads one segment's header and first frame. hdrOK is false for
// an unfinished spare (a short or invalid header with no byte after it).
func probeSegment(fsys LogFS, path string, no, storeID uint64) (e segEntry, hdrOK bool, err error) {
	rc, err := fsys.Open(path)
	if err != nil {
		return e, false, fmt.Errorf("wal: open segment %q: %w", path, err)
	}
	defer func() { _ = rc.Close() }()
	br := bufio.NewReaderSize(rc, 4096)
	var hdr [segHeaderSize]byte
	n, rerr := io.ReadFull(br, hdr[:])
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return e, false, fmt.Errorf("wal: read segment %q: %w", path, rerr)
	}
	hStore, hNo, herr := decodeSegmentHeader(hdr[:n])
	if herr != nil {
		if n < segHeaderSize {
			return e, false, nil
		}
		if _, perr := br.Peek(1); perr != nil {
			return e, false, nil
		}
		return e, false, fmt.Errorf("%w: %s", ErrSegmentHeader, path)
	}
	if hStore != storeID {
		return e, false, fmt.Errorf("%w: segment %s has store id %016x, control has %016x", ErrForeignStore, path, hStore, storeID)
	}
	if hNo != no {
		return e, false, fmt.Errorf("%w: %s holds segment number %d", ErrSegmentHeader, path, hNo)
	}
	e = segEntry{path: path, no: no, firstPos: -1}
	if _, perr := br.Peek(1); perr != nil {
		return e, true, nil
	}
	f, derr := Decode(br)
	switch {
	case derr == nil:
		if f.Version != CurrentVersion {
			return e, false, fmt.Errorf("%w: legacy frame in segment %s", ErrUnsupportedVersion, path)
		}
		if f.StoreID != storeID {
			return e, false, fmt.Errorf("%w: frame in segment %s has store id %016x", ErrForeignStore, path, f.StoreID)
		}
		e.firstPos = int64(f.Pos) //nolint:gosec // G115: positions are bounded by int64 file arithmetic
		e.firstPrevLen = f.PrevLen
	case errors.Is(derr, ErrTornFrame):
		e.partial = true
	default:
		// A corrupt first frame: recorded as a frame-bearing segment of unknown
		// position; the stream reports the decode error when it reaches it.
		e.partial = true
	}
	return e, true, nil
}

// Log reads a store's write-ahead log: the control file, the segments from the
// one holding the oldest retained position, and the legacy single-file log.
// It validates every frame's CRC, store id, position and prev-link.
//
// A Log is not safe for concurrent use; open one per goroutine.
type Log struct {
	fsys    LogFS
	walPath string
	ctl     Control
	hasCtl  bool
	list    segListing

	tailErr      error
	end          int64
	lastFramePos int64
	tailSegNo    uint64
	tailSegOff   int64
	frames       int
	iterated     bool
}

// OpenLog opens the write-ahead log at walPath on the operating-system
// filesystem; see [OpenLogFS].
func OpenLog(walPath string) (*Log, error) { return OpenLogFS(osLogFS{}, walPath) }

// OpenLogFS opens the write-ahead log at walPath through fsys. It reads the
// control file and enumerates and validates the segments; frames are read
// lazily by [Log.Frames].
//
// It returns [ErrMissingControl] when segments exist without a control file,
// [ErrControlCorrupt], [ErrSegmentHeader], [ErrForeignStore] and
// [ErrSegmentGap] for a damaged segment set. A directory with neither control
// file nor segments returns a Log whose [Log.Control] reports false: a store
// written only in the legacy single-file format.
func OpenLogFS(fsys LogFS, walPath string) (*Log, error) {
	l := &Log{fsys: fsys, walPath: walPath, lastFramePos: -1}
	c, ok, err := readControl(fsys, walPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		names, derr := fsys.ReadDir(SegmentDir(walPath))
		if derr != nil && !errors.Is(derr, os.ErrNotExist) {
			return nil, fmt.Errorf("wal: list segments: %w", derr)
		}
		for _, n := range names {
			if _, isSeg := parseSegmentName(n); isSeg {
				return nil, ErrMissingControl
			}
		}
		return l, nil
	}
	l.ctl, l.hasCtl = c, true
	l.list, err = listSegments(fsys, walPath, c)
	if err != nil {
		return nil, err
	}
	l.end = int64(c.OldestRetainedPos) //nolint:gosec // G115: positions are bounded by int64 file arithmetic
	return l, nil
}

// Control returns the control file, and false for a legacy-only store.
func (l *Log) Control() (Control, bool) { return l.ctl, l.hasCtl }

// OldestRetainedPos returns OR, the first retained position (0 for a
// legacy-only store).
func (l *Log) OldestRetainedPos() int64 {
	return int64(l.ctl.OldestRetainedPos) //nolint:gosec // G115: positions are bounded by int64 file arithmetic
}

// LegacyReader opens the legacy single-file log at walPath for reading, or
// returns nil when it does not exist. The caller closes it.
func (l *Log) LegacyReader() (*Reader, error) {
	rc, err := l.fsys.Open(l.walPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wal: open legacy log: %w", err)
	}
	return NewReader(rc, rc), nil
}

// Frames iterates the segment frames from the first frame of the segment that
// holds OR. Frames below OR in that segment are yielded too, so a caller can
// skip what a snapshot already covers. Iteration stops at the first error;
// [Log.TailError] then reports it: [ErrTornFrame] for a benign torn tail at
// the end of the last segment holding frames, and a corruption sentinel
// otherwise. A legacy-only Log yields nothing.
func (l *Log) Frames() iter.Seq[Frame] {
	return func(yield func(Frame) bool) {
		if l.iterated || !l.hasCtl {
			return
		}
		l.iterated = true
		segs := l.list.segs
		// lastFramed is the index of the last segment holding any frame byte.
		lastFramed := -1
		for i, s := range segs {
			if s.firstPos >= 0 || s.partial {
				lastFramed = i
			}
		}
		or := int64(l.ctl.OldestRetainedPos) //nolint:gosec // G115: positions are bounded by int64 file arithmetic
		expected := int64(-1)
		passedOR := false
		for i := l.list.start; i < len(segs); i++ {
			s := segs[i]
			if s.firstPos < 0 && !s.partial {
				if i < lastFramed {
					l.tailErr = fmt.Errorf("%w: empty segment %d precedes segment frames", ErrSegmentGap, s.no)
					return
				}
				continue
			}
			if !l.streamSegment(s, i == lastFramed, or, &expected, &passedOR, yield) {
				return
			}
		}
		if expected < 0 {
			l.end = or
			if l.lastFramePos < 0 && l.ctl.PrevFramePosAtOR != NoFramePos {
				l.lastFramePos = int64(l.ctl.PrevFramePosAtOR) //nolint:gosec // G115: positions are bounded
			}
			return
		}
		if !passedOR && expected != or && l.tailErr == nil {
			l.tailErr = fmt.Errorf("%w: frames end at %d below the oldest retained position %d", ErrFramePosition, expected, or)
		}
	}
}

// streamSegment yields one segment's frames, validating each. It returns false
// when iteration must stop (an error, a benign tail, or yield returning false).
func (l *Log) streamSegment(s segEntry, isLast bool, or int64, expected *int64, passedOR *bool, yield func(Frame) bool) bool {
	rc, err := l.fsys.Open(s.path)
	if err != nil {
		l.tailErr = fmt.Errorf("wal: open segment %q: %w", s.path, err)
		return false
	}
	defer func() { _ = rc.Close() }()
	br := bufio.NewReaderSize(rc, 64*1024)
	if _, err := br.Discard(segHeaderSize); err != nil {
		l.tailErr = fmt.Errorf("%w: %s", ErrSegmentHeader, s.path)
		return false
	}
	off := int64(segHeaderSize)
	for {
		if _, perr := br.Peek(1); perr != nil {
			if errors.Is(perr, io.EOF) {
				return true
			}
			l.tailErr = perr
			return false
		}
		f, derr := Decode(br)
		if derr != nil {
			if errors.Is(derr, ErrTornFrame) && !isLast {
				derr = fmt.Errorf("%w: segment %d", ErrTornSegment, s.no)
			}
			l.tailErr = derr
			return false
		}
		if f.Version != CurrentVersion {
			l.tailErr = fmt.Errorf("%w: legacy frame in segment %d", ErrUnsupportedVersion, s.no)
			return false
		}
		if f.StoreID != l.ctl.StoreID {
			l.tailErr = fmt.Errorf("%w: frame at segment %d offset %d has store id %016x", ErrForeignStore, s.no, off, f.StoreID)
			return false
		}
		pos := int64(f.Pos) //nolint:gosec // G115: positions are bounded by int64 file arithmetic
		if *expected >= 0 && pos != *expected {
			l.tailErr = fmt.Errorf("%w: frame at segment %d offset %d has position %d, expected %d", ErrFramePosition, s.no, off, pos, *expected)
			return false
		}
		// The prev-link: against the predecessor when one was read, against
		// the control file's record at OR when the first retained frame is.
		switch {
		case l.lastFramePos >= 0:
			if f.PrevLen == 0 || pos-int64(f.PrevLen) != l.lastFramePos {
				l.tailErr = fmt.Errorf("%w: frame at position %d links to %d, predecessor is at %d", ErrPrevLink, pos, pos-int64(f.PrevLen), l.lastFramePos)
				return false
			}
		case pos == or:
			if !prevLinkMatches(f, l.ctl.PrevFramePosAtOR) {
				l.tailErr = fmt.Errorf("%w: first retained frame at position %d does not link to %d", ErrPrevLink, pos, l.ctl.PrevFramePosAtOR)
				return false
			}
		}
		if !*passedOR && pos >= or {
			if pos > or {
				l.tailErr = fmt.Errorf("%w: no frame starts at the oldest retained position %d (next frame at %d)", ErrFramePosition, or, pos)
				return false
			}
			if l.lastFramePos >= 0 && !prevLinkMatches(f, l.ctl.PrevFramePosAtOR) {
				l.tailErr = fmt.Errorf("%w: frame at the oldest retained position %d does not link to %d", ErrPrevLink, pos, l.ctl.PrevFramePosAtOR)
				return false
			}
			*passedOR = true
		}
		size := int64(FrameSize(f))
		off += size
		*expected = pos + size
		l.end = *expected
		l.lastFramePos = pos
		l.tailSegNo, l.tailSegOff = s.no, off
		l.frames++
		if !yield(f) {
			return false
		}
	}
}

// prevLinkMatches reports whether f's prev-link names prev ([NoFramePos]
// meaning "no predecessor", which requires prevLen 0).
func prevLinkMatches(f Frame, prev uint64) bool {
	if prev == NoFramePos {
		return f.PrevLen == 0
	}
	return f.PrevLen != 0 && f.Pos-uint64(f.PrevLen) == prev
}

// TailError returns why [Log.Frames] stopped: nil at a clean end, [ErrTornFrame]
// for a benign torn tail, or a corruption sentinel.
func (l *Log) TailError() error { return l.tailErr }

// TailOffset returns E, the logical position just past the last valid frame
// (OR when no retained frame exists). It is final once [Log.Frames] has run.
func (l *Log) TailOffset() int64 { return l.end }

// LastFramePos returns the position of the last valid frame, or -1 when none
// is known.
func (l *Log) LastFramePos() int64 { return l.lastFramePos }

// TailSegment returns the segment number and the file offset just past the last
// valid frame; zero when no frame was read.
func (l *Log) TailSegment() (segNo uint64, offset int64) { return l.tailSegNo, l.tailSegOff }

// Close releases nothing today; it exists so a Log can be handled like a
// [Reader]. Segment files are opened and closed by [Log.Frames] itself.
func (l *Log) Close() error { return nil }

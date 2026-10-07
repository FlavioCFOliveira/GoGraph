package wal

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
)

// Layout of a store's write-ahead log (docs/design-wal-v2.md §2.1). Every path
// is derived from the WAL base path walPath (dir/wal in a store directory):
//
//	walPath + ".control"                the control file (64 B)
//	walPath + ".d/<016x segNo>.wal"     the segments
//	walPath                             the sealed legacy file, or a one-frame seal stub
//	walPath + ".lock"                   the writer's lock file
const (
	controlSuffix    = ".control"
	segmentDirSuffix = ".d"
	segmentExt       = ".wal"
	tmpSuffix        = ".tmp"
)

// ControlPath returns the path of the control file of the WAL at walPath. It is
// a pure function and safe for concurrent use.
func ControlPath(walPath string) string { return walPath + controlSuffix }

// SegmentDir returns the directory holding the segments of the WAL at walPath.
// It is a pure function and safe for concurrent use.
func SegmentDir(walPath string) string { return walPath + segmentDirSuffix }

// SegmentPath returns the path of segment segNo of the WAL at walPath. It is a
// pure function and safe for concurrent use.
func SegmentPath(walPath string, segNo uint64) string {
	return SegmentDir(walPath) + "/" + segmentName(segNo)
}

func segmentName(segNo uint64) string { return fmt.Sprintf("%016x%s", segNo, segmentExt) }

// parseSegmentName returns the segment number a directory entry names, and
// false for anything that is not a segment (a temp file, a foreign file).
func parseSegmentName(name string) (uint64, bool) {
	base, ok := strings.CutSuffix(name, segmentExt)
	if !ok || len(base) != 16 {
		return 0, false
	}
	n, err := strconv.ParseUint(base, 16, 64)
	return n, err == nil
}

// Errors of the WAL v2 container. Every one of them is corruption: recovery
// refuses to open a directory that reports one.
var (
	// ErrMissingControl indicates segments exist but the control file does
	// not, so neither the store id nor the oldest retained position is known.
	ErrMissingControl = errors.New("wal: segments exist but the control file is missing")
	// ErrControlCorrupt indicates the control file has a bad magic, version,
	// length or CRC.
	ErrControlCorrupt = errors.New("wal: control file is corrupt")
	// ErrSegmentHeader indicates a segment whose header has a bad magic,
	// version, length or CRC, or whose segment number differs from its name.
	ErrSegmentHeader = errors.New("wal: segment header is corrupt")
	// ErrForeignStore indicates a segment or a frame that carries a store id
	// other than the control file's: it belongs to another store.
	ErrForeignStore = errors.New("wal: segment or frame belongs to another store")
	// ErrSegmentGap indicates missing segments: segment numbers that are not
	// consecutive, an empty segment followed by one holding frames, or no
	// retained segment holding the oldest retained position.
	ErrSegmentGap = errors.New("wal: segments are missing")
	// ErrTornSegment indicates a torn frame anywhere but at the end of the
	// last segment holding frames. A non-tail segment is fully durable before
	// its successor receives a byte, so a tear there is corruption.
	ErrTornSegment = errors.New("wal: torn frame in a segment that is not the tail")
	// ErrFramePosition indicates a frame whose logical position is not the end
	// of its predecessor: a frame moved to another position or a hole.
	ErrFramePosition = errors.New("wal: frame position does not follow its predecessor")
	// ErrPrevLink indicates a frame whose prev-link does not name its
	// predecessor's position: a frame from an older generation of the log.
	ErrPrevLink = errors.New("wal: frame prev-link does not name its predecessor")
	// ErrLegacyNotSealed indicates a single-file legacy log that does not end
	// with its seal although segment frames exist, so the boundary between the
	// two histories is unknown.
	ErrLegacyNotSealed = errors.New("wal: legacy log is not sealed but segment frames exist")
)

// NoFramePos marks "no frame": the prev-frame position of the store's first
// frame, as recorded in [Control.PrevFramePosAtOR].
const NoFramePos = ^uint64(0)

// Control flags.
const (
	// ControlPrefixTruncated records that a checkpoint has discarded a prefix
	// of the log, so recovery requires the snapshot that folded it.
	ControlPrefixTruncated uint32 = 1 << 0
	// ControlLegacyV1Pending records that the legacy single-file log still
	// holds history and has not yet been replaced by its seal stub.
	ControlLegacyV1Pending uint32 = 1 << 1
)

const (
	controlSize    = 64
	controlVersion = 1
	segHeaderSize  = 32
	segVersion     = 2
)

var (
	controlMagic = [4]byte{'G', 'G', 'C', 'T'}
	segmentMagic = [4]byte{'G', 'G', 'W', 'S'}
)

// Control is the decoded control file of a WAL (docs/design-wal-v2.md §2.2),
// the analogue of PostgreSQL's pg_control. It is a plain value, safe to copy
// and to read concurrently.
type Control struct {
	// StoreID is the store's random 64-bit identity, created once.
	StoreID uint64
	// OldestRetainedPos (OR) is the first position of the oldest retained
	// segment, or the end of the log when the retained segments hold no frame.
	OldestRetainedPos uint64
	// PrevFramePosAtOR is the position of the frame preceding OR, or
	// [NoFramePos] when there is none.
	PrevFramePosAtOR uint64
	// CheckpointRedoPos is the redo position of the last truncating
	// checkpoint; diagnostic.
	CheckpointRedoPos uint64
	// CreatedUnixNano is when the store id was created; diagnostic.
	CreatedUnixNano uint64
	// Flags holds [ControlPrefixTruncated] and [ControlLegacyV1Pending].
	Flags uint32
}

func encodeControl(c Control) [controlSize]byte {
	var b [controlSize]byte
	copy(b[0:4], controlMagic[:])
	binary.LittleEndian.PutUint16(b[4:6], controlVersion)
	binary.LittleEndian.PutUint16(b[6:8], controlSize)
	binary.LittleEndian.PutUint64(b[8:16], c.StoreID)
	binary.LittleEndian.PutUint32(b[16:20], c.Flags)
	binary.LittleEndian.PutUint64(b[24:32], c.OldestRetainedPos)
	binary.LittleEndian.PutUint64(b[32:40], c.PrevFramePosAtOR)
	binary.LittleEndian.PutUint64(b[40:48], c.CheckpointRedoPos)
	binary.LittleEndian.PutUint64(b[48:56], c.CreatedUnixNano)
	binary.LittleEndian.PutUint32(b[56:60], crc32.Checksum(b[0:56], castagnoli))
	return b
}

func decodeControl(b []byte) (Control, error) {
	if len(b) != controlSize || [4]byte(b[0:4]) != controlMagic ||
		binary.LittleEndian.Uint16(b[4:6]) != controlVersion ||
		binary.LittleEndian.Uint16(b[6:8]) != controlSize ||
		binary.LittleEndian.Uint32(b[56:60]) != crc32.Checksum(b[0:56], castagnoli) {
		return Control{}, ErrControlCorrupt
	}
	c := Control{
		StoreID:           binary.LittleEndian.Uint64(b[8:16]),
		Flags:             binary.LittleEndian.Uint32(b[16:20]),
		OldestRetainedPos: binary.LittleEndian.Uint64(b[24:32]),
		PrevFramePosAtOR:  binary.LittleEndian.Uint64(b[32:40]),
		CheckpointRedoPos: binary.LittleEndian.Uint64(b[40:48]),
		CreatedUnixNano:   binary.LittleEndian.Uint64(b[48:56]),
	}
	if c.StoreID == 0 {
		return Control{}, ErrControlCorrupt
	}
	return c, nil
}

// newStoreID draws a non-zero random 64-bit store id.
func newStoreID() (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("wal: draw store id: %w", err)
		}
		if id := binary.LittleEndian.Uint64(b[:]); id != 0 {
			return id, nil
		}
	}
}

func encodeSegmentHeader(storeID, segNo uint64) [segHeaderSize]byte {
	var b [segHeaderSize]byte
	copy(b[0:4], segmentMagic[:])
	binary.LittleEndian.PutUint16(b[4:6], segVersion)
	binary.LittleEndian.PutUint16(b[6:8], segHeaderSize)
	binary.LittleEndian.PutUint64(b[8:16], storeID)
	binary.LittleEndian.PutUint64(b[16:24], segNo)
	binary.LittleEndian.PutUint32(b[28:32], crc32.Checksum(b[0:28], castagnoli))
	return b
}

// decodeSegmentHeader validates a segment header and returns its store id and
// segment number.
func decodeSegmentHeader(b []byte) (storeID, segNo uint64, err error) {
	if len(b) != segHeaderSize || [4]byte(b[0:4]) != segmentMagic ||
		binary.LittleEndian.Uint16(b[4:6]) != segVersion ||
		binary.LittleEndian.Uint16(b[6:8]) != segHeaderSize ||
		binary.LittleEndian.Uint32(b[28:32]) != crc32.Checksum(b[0:28], castagnoli) {
		return 0, 0, ErrSegmentHeader
	}
	return binary.LittleEndian.Uint64(b[8:16]), binary.LittleEndian.Uint64(b[16:24]), nil
}

// Control records ride in a frame payload whose leading byte is
// [ControlRecordTag] (docs/design-wal-v2.md §1.2). This build writes and reads
// only [CtlLegacySeal].
const (
	// ControlRecordTag is the leading payload byte of a control record.
	ControlRecordTag byte = 0xFC
	// CtlLegacySeal is the control-record kind that seals the legacy
	// single-file log: no frame of that file follows it.
	CtlLegacySeal  byte = 3
	legacySealSize      = 2 + 8 + 8
)

// LegacySeal is the decoded [CtlLegacySeal] record: the store id the legacy log
// was sealed for and the segment position at which its history continues. It is
// a plain value, safe to copy and to read concurrently.
type LegacySeal struct {
	StoreID    uint64
	V2StartPos uint64
}

// EncodeLegacySeal returns the payload of a [CtlLegacySeal] record.
func EncodeLegacySeal(s LegacySeal) []byte {
	b := make([]byte, legacySealSize)
	b[0], b[1] = ControlRecordTag, CtlLegacySeal
	binary.LittleEndian.PutUint64(b[2:10], s.StoreID)
	binary.LittleEndian.PutUint64(b[10:18], s.V2StartPos)
	return b
}

// DecodeLegacySeal parses a [CtlLegacySeal] payload; ok is false for any other
// payload.
func DecodeLegacySeal(payload []byte) (LegacySeal, bool) {
	if len(payload) != legacySealSize || payload[0] != ControlRecordTag || payload[1] != CtlLegacySeal {
		return LegacySeal{}, false
	}
	return LegacySeal{
		StoreID:    binary.LittleEndian.Uint64(payload[2:10]),
		V2StartPos: binary.LittleEndian.Uint64(payload[10:18]),
	}, true
}

// sealFrameBytes returns the encoded [CurrentVersion] frame carrying a seal.
// Its position is 0 and its prevLen 0: it lives in the legacy file, outside
// the segment position space.
func sealFrameBytes(s LegacySeal) []byte {
	var buf strings.Builder
	payload := EncodeLegacySeal(s)
	buf.Grow(HeaderSizeV2 + len(payload))
	// encodeV2 to a strings.Builder never fails.
	_, _ = encodeV2(&buf, payload, 0, 0, s.StoreID)
	return []byte(buf.String())
}

// writeFileDurably publishes body at path crash-atomically through fsys: temp
// file, fsync, rename, parent-directory fsync through dirFsync. A crash leaves
// either the previous file or the new one. preRename and preDirFsync name the
// crash-injection points around the rename; empty names are skipped.
func writeFileDurably(fsys walFS, dirFsync func(string) error, path string, body []byte, preRename, preDirFsync string) error {
	tmp := path + tmpSuffix
	f, err := fsys.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("wal: create %q: %w", tmp, err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = fsys.Remove(tmp)
		return fmt.Errorf("wal: write %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = fsys.Remove(tmp)
		return fmt.Errorf("wal: fsync %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("wal: close %q: %w", tmp, err)
	}
	if preRename != "" {
		crashpoint.Breakpoint(preRename)
	}
	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("wal: publish %q: %w", path, err)
	}
	if preDirFsync != "" {
		crashpoint.Breakpoint(preDirFsync)
	}
	if err := dirFsync(path); err != nil {
		return fmt.Errorf("wal: fsync parent dir of %q: %w", path, err)
	}
	return nil
}

// readAllFS reads the whole file at path; os.ErrNotExist passes through.
func readAllFS(fsys LogFS, path string) ([]byte, error) {
	rc, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// readControl reads and validates the control file of walPath. ok is false
// when the file does not exist.
func readControl(fsys LogFS, walPath string) (c Control, ok bool, err error) {
	b, err := readAllFS(fsys, ControlPath(walPath))
	if errors.Is(err, os.ErrNotExist) {
		return Control{}, false, nil
	}
	if err != nil {
		return Control{}, false, fmt.Errorf("wal: read control file: %w", err)
	}
	c, err = decodeControl(b)
	if err != nil {
		return Control{}, false, err
	}
	return c, true, nil
}

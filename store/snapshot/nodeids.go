package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// NodeIDsFile is the snapshot component that records the mapper's per-shard
// high-water marks (WAL v2 step 1, docs/design-wal-v2.md §3.3). Recovery hands
// them to [graph.Mapper.LoadFrom], so a key interned after the restore never
// receives an id at or below one the image could still name, and the holes the
// capture left out of mapper.bin stay holes.
//
// Layout, little-endian, 2 056 bytes:
//
//	magic   u32  "GNID"
//	version u16  1
//	shards  u16  256
//	next    256 × u64
//
// Integrity is the manifest [FileEntry] CRC32C, like every other component.
const NodeIDsFile = "nodeids.bin"

// nodeIDsMagic is "GNID" read as a little-endian u32.
const nodeIDsMagic uint32 = 'G' | 'N'<<8 | 'I'<<16 | 'D'<<24

// nodeIDsFormatVersion is the nodeids.bin layout version this build writes and
// reads. It is independent of [ManifestVersion].
const nodeIDsFormatVersion uint16 = 1

// nodeIDsSize is the exact byte length of a version-1 nodeids.bin.
const nodeIDsSize = 4 + 2 + 2 + 8*graph.MapperShards

// ErrNodeIDsCorrupted is returned when nodeids.bin does not parse: a wrong
// magic, an unknown version, a shard count other than 256, or a short file.
var ErrNodeIDsCorrupted = errors.New("snapshot: nodeids.bin corrupted")

// WriteNodeIDs serialises next to w and returns the byte count and the CRC32C
// of what it wrote.
func WriteNodeIDs(w io.Writer, next *[graph.MapperShards]uint64) (int64, uint32, error) {
	var buf [nodeIDsSize]byte
	binary.LittleEndian.PutUint32(buf[0:4], nodeIDsMagic)
	binary.LittleEndian.PutUint16(buf[4:6], nodeIDsFormatVersion)
	binary.LittleEndian.PutUint16(buf[6:8], graph.MapperShards)
	for i, v := range next {
		binary.LittleEndian.PutUint64(buf[8+8*i:], v)
	}
	n, err := w.Write(buf[:])
	if err != nil {
		return int64(n), 0, err
	}
	return int64(n), crc32.Checksum(buf[:], castagnoli), nil
}

// ReadNodeIDs parses a nodeids.bin from r.
func ReadNodeIDs(r io.Reader) (*[graph.MapperShards]uint64, error) {
	var buf [nodeIDsSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNodeIDsCorrupted, err)
	}
	if m := binary.LittleEndian.Uint32(buf[0:4]); m != nodeIDsMagic {
		return nil, fmt.Errorf("%w: magic %#x", ErrNodeIDsCorrupted, m)
	}
	if v := binary.LittleEndian.Uint16(buf[4:6]); v != nodeIDsFormatVersion {
		return nil, fmt.Errorf("%w: version %d", ErrNodeIDsCorrupted, v)
	}
	if s := binary.LittleEndian.Uint16(buf[6:8]); s != graph.MapperShards {
		return nil, fmt.Errorf("%w: %d shards, want %d", ErrNodeIDsCorrupted, s, graph.MapperShards)
	}
	next := new([graph.MapperShards]uint64)
	for i := range next {
		next[i] = binary.LittleEndian.Uint64(buf[8+8*i:])
	}
	return next, nil
}

// readVerifiedNodeIDs reads nodeids.bin at path, refusing trailing bytes and a
// CRC32C other than expected.
func readVerifiedNodeIDs(fsys fileSystem, path string, expected uint32) (*[graph.MapperShards]uint64, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return nil, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()
	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	next, err := ReadNodeIDs(tee)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	extra, err := io.Copy(io.Discard, tee)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if extra != 0 {
		return nil, fmt.Errorf("%w: %s has %d trailing bytes", ErrCorrupted, NodeIDsFile, extra)
	}
	if got := hasher.Sum32(); got != expected {
		return nil, fmt.Errorf("%w: %s crc32c=%d want=%d", ErrCorrupted, NodeIDsFile, got, expected)
	}
	return next, nil
}

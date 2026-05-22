package snapshot

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"gograph/graph"
	"gograph/internal/metrics"
)

// MapperFile is the conventional file name carrying the durable
// (NodeID -> natural key) interning table inside a v3 snapshot
// directory. It is a sibling of [CSRFile], [LabelsFile] and
// [PropertiesFile] and is referenced by an additional entry in
// [Manifest.Files] when the writer emitted it.
const MapperFile = "mapper.bin"

// mapperMagic is the four-byte magic ('G','M','A','P') that prefixes
// every mapper.bin file. Stored as a uint32 LE; the magic bytes appear
// on disk as 'GMAP'.
const mapperMagic uint32 = 0x50414D47

// mapperFormatVersion is the mapper.bin internal format version. It
// is independent of [ManifestVersion]: a future mapper.bin layout
// change bumps this value without forcing a manifest schema bump.
const mapperFormatVersion uint16 = 1

// ErrMapperCorrupted is returned by [ReadMapperString] when the
// mapper.bin file is structurally malformed (bad magic, unsupported
// format version, truncated record, or an implausible length prefix).
var ErrMapperCorrupted = errors.New("snapshot: mapper.bin corrupted")

// maxMapperKeyLen caps a single natural-key entry at 1 GiB. Larger
// values are rejected as corruption; without the cap a flipped byte
// in the length prefix could ask the reader to allocate an absurd
// buffer.
const maxMapperKeyLen = 1 << 30

// MapperPair is one (NodeID, natural key) record as parsed from the
// on-disk mapper.bin payload. The slice exposed by
// [MapperReadback.Pairs] is enumerated in shard-major / intra-index-
// major order — the same order [graph.Mapper.Walk] produces, which is
// the order the writer serialised.
type MapperPair struct {
	ID  graph.NodeID
	Key string
}

// MapperReadback is the structural parse of a mapper.bin file. The
// caller materialises it back into a live [graph.Mapper] via
// [graph.Mapper.LoadFrom] once a fresh mapper has been constructed.
type MapperReadback struct {
	Pairs []MapperPair
}

// WriteMapperString serialises every (NodeID -> string key) pair held
// by m into w in the mapper.bin format documented below. It returns
// the number of bytes written and the CRC32C of the serialised payload
// — both stored in the manifest's [FileEntry] for the mapper.bin
// component so [LoadSnapshotFull] can verify integrity at load time.
//
// On-disk layout (all little-endian):
//
//	uint32  magic           ('GMAP', 0x50414D47)
//	uint16  formatVersion   (1)
//	uint64  pairCount
//	for each pair:
//	    uint64  nodeID
//	    uint32  keyLen
//	    [keyLen]byte key
//
// Pairs are emitted in [graph.Mapper.Walk] order (shard-major,
// intra-index-major) so the read side can reconstruct the mapper
// deterministically.
//
// The CRC32C covers the entire on-disk file, including the magic
// header. The reader recomputes the CRC end-to-end at load time.
func WriteMapperString(w io.Writer, m *graph.Mapper[string]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteMapperString")()

	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, mapperMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, mapperFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
		return 0, 0, err
	}

	// Collect pairs in Walk order. The mapper takes per-shard RLocks
	// internally, so the snapshot writer sees a consistent view of the
	// interning table even under concurrent reads.
	pairs := collectMapperPairs(m)
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(pairs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
		return 0, 0, err
	}

	total := int64(4 + 2 + 8)
	for i := range pairs {
		if uint64(len(pairs[i].Key)) > uint64(^uint32(0)) {
			metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
			return 0, 0, fmt.Errorf("snapshot: mapper key too long: %d bytes", len(pairs[i].Key))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint64(pairs[i].ID)); err != nil {
			metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
			return 0, 0, err
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(pairs[i].Key))); err != nil {
			metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
			return 0, 0, err
		}
		if pairs[i].Key != "" {
			if _, err := tee.Write([]byte(pairs[i].Key)); err != nil {
				metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
				return 0, 0, err
			}
		}
		total += int64(8 + 4 + len(pairs[i].Key))
	}

	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
		return 0, 0, err
	}
	return total, hasher.Sum32(), nil
}

// collectMapperPairs walks m and returns every (NodeID, key) pair in
// Walk order. The function is split out so [WriteMapperString] stays
// allocation-cheap on the hot path: a single slice allocation, sized
// from m.Len(), then the per-pair append is amortised O(1).
func collectMapperPairs(m *graph.Mapper[string]) []MapperPair {
	out := make([]MapperPair, 0, m.Len())
	m.Walk(func(id graph.NodeID, k string) bool {
		out = append(out, MapperPair{ID: id, Key: k})
		return true
	})
	return out
}

// ReadMapperString parses a mapper.bin payload produced by
// [WriteMapperString]. It performs strict structural validation: a
// missing or wrong magic, an unsupported format version, a truncated
// record, or an implausible key length all surface as
// [ErrMapperCorrupted].
//
// The caller is responsible for verifying the surrounding manifest
// CRC matches the file bytes ([LoadSnapshotFull] does this); this
// function only enforces the structural contract.
func ReadMapperString(r io.Reader) (MapperReadback, error) {
	defer metrics.Time("store.snapshot.ReadMapperString")()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if magic != mapperMagic {
		metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: bad magic %#x", ErrMapperCorrupted, magic)
	}

	var version uint16
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if version != mapperFormatVersion {
		metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: unsupported mapper format version %d",
			ErrMapperCorrupted, version)
	}

	var pairCount uint64
	if err := binary.Read(br, binary.LittleEndian, &pairCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if pairCount > 1<<40 {
		metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: implausible pair count %d",
			ErrMapperCorrupted, pairCount)
	}
	pairs := make([]MapperPair, pairCount)
	for i := uint64(0); i < pairCount; i++ {
		var idRaw uint64
		if err := binary.Read(br, binary.LittleEndian, &idRaw); err != nil {
			metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
			return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
		}
		var keyLen uint32
		if err := binary.Read(br, binary.LittleEndian, &keyLen); err != nil {
			metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
			return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
		}
		if keyLen > maxMapperKeyLen {
			metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
			return MapperReadback{}, fmt.Errorf("%w: key len %d > %d",
				ErrMapperCorrupted, keyLen, maxMapperKeyLen)
		}
		buf := make([]byte, keyLen)
		if keyLen > 0 {
			if _, err := io.ReadFull(br, buf); err != nil {
				metrics.IncCounter("store.snapshot.ReadMapperString.errors", 1)
				return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
			}
		}
		pairs[i] = MapperPair{ID: graph.NodeID(idRaw), Key: string(buf)}
	}

	return MapperReadback{Pairs: pairs}, nil
}

// readVerifiedMapper opens path, runs the file bytes through CRC32C
// and the structural mapper reader simultaneously, and returns the
// parsed readback iff the CRC matches expected. Any disagreement
// surfaces as [ErrCorrupted]. Mirrors [readVerifiedCSR] /
// [readVerifiedLabels] / [readVerifiedProperties] in shape.
func readVerifiedMapper(path string, expected uint32) (MapperReadback, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path
	if err != nil {
		return MapperReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadMapperString(tee)
	if err != nil {
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return MapperReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, MapperFile, got, expected)
	}
	return parsed, nil
}

package snapshot

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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

// mapperFormatVersionString is the mapper.bin internal format version
// stamped for string-keyed graphs. It is independent of
// [ManifestVersion]: a future mapper.bin layout change bumps this value
// without forcing a manifest schema bump.
//
// The string layout is frozen at version 1 so that every mapper.bin
// produced for the production (string) key type is byte-identical to
// the pre-codec writer: the per-record key bytes are the raw UTF-8 of
// the key, with no codec framing. Bumping this would change the header
// bytes and break the cross-process byte-equality guarantee.
const mapperFormatVersionString uint16 = 1

// mapperFormatVersionCodec is the mapper.bin internal format version
// stamped for non-string-keyed graphs. A v2 record carries the
// codec-encoded key bytes (the output of [txn.Codec.Encode]) framed by
// the same uint32 length prefix the v1 layout uses. Recovery decodes
// each record's bytes back into the natural key N via the matching
// codec before re-seeding the interning table.
//
// String-keyed graphs never emit version 2: their natural bytes are the
// raw UTF-8 already written by the version-1 layout, so reusing version
// 1 keeps the on-disk image byte-identical (see
// [mapperFormatVersionString]).
const mapperFormatVersionCodec uint16 = 2

// ErrMapperCorrupted is returned by [ReadMapperString] when the
// mapper.bin file is structurally malformed (bad magic, unsupported
// format version, truncated record, or an implausible length prefix).
var ErrMapperCorrupted = errors.New("snapshot: mapper.bin corrupted")

// maxMapperKeyLen caps a single natural-key entry at 1 GiB. Larger
// values are rejected as corruption; without the cap a flipped byte
// in the length prefix could ask the reader to allocate an absurd
// buffer.
const maxMapperKeyLen = 1 << 30

// mapperCapHintMax caps an eager slice reservation in [ReadMapperString] /
// [ReadMapperBytes] so a hostile pairCount (up to the 1<<40 implausibility
// ceiling, a make() of pairCount*24 or *32 bytes) cannot drive a
// multi-gigabyte allocation before the per-pair reads fail on a truncated
// body. The reader validates pairCount against the ceiling first, then grows
// via append. Mirrors labels.go's labelsCapHintMax and tombstones.go's
// tombstonesCapHintMax.
const mapperCapHintMax = 1 << 20

// MapperPair is one (NodeID, natural key) record as parsed from the
// on-disk mapper.bin payload. The slice exposed by
// [MapperReadback.Pairs] is enumerated in shard-major / intra-index-
// major order — the same order [graph.Mapper.Walk] produces, which is
// the order the writer serialised.
type MapperPair struct {
	ID  graph.NodeID
	Key string
}

// MapperRawPair is one (NodeID, raw key bytes) record as parsed from a
// codec-encoded (version 2) mapper.bin payload. The bytes are the
// opaque output of [txn.Codec.Encode] for the natural key; recovery
// decodes them back into the concrete key type N via the matching
// codec. The slice exposed by [MapperReadback.RawPairs] is enumerated
// in the same Walk order the writer serialised.
type MapperRawPair struct {
	ID  graph.NodeID
	Key []byte
}

// MapperReadback is the structural parse of a mapper.bin file. The
// caller materialises it back into a live [graph.Mapper] via
// [graph.Mapper.LoadFrom] once a fresh mapper has been constructed.
//
// Exactly one of Pairs / RawPairs is populated, selected by the
// on-disk format version:
//
//   - Pairs holds string keys, parsed from a version-1 (string) file.
//     [ApplyMapperToGraph] consumes this directly for string-keyed
//     graphs.
//   - RawPairs holds codec-encoded key bytes, parsed from a version-2
//     file produced for a non-string key type.
//     [ApplyMapperToGraphWithCodec] decodes these via the supplied
//     codec.
//
// Both are empty when mapper.bin was absent (v1/v2 snapshots written by
// the no-codec writer for non-string keys, or any v1 CSR-only
// snapshot).
type MapperReadback struct {
	Pairs    []MapperPair
	RawPairs []MapperRawPair
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
	defer metrics.Time("store.snapshot.WriteMapperString").Stop()

	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, mapperMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, mapperFormatVersionString); err != nil {
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
	// scratch holds each record's fixed 12-byte prefix — ID(8) | keyLen(4) —
	// reused across records (allocated once, escapes the io.Writer chain once)
	// so packing with PutUintNN and one Write replaces the per-field
	// binary.Write reflection with zero per-record allocations, byte-identically.
	var scratch [12]byte
	for i := range pairs {
		if uint64(len(pairs[i].Key)) > uint64(^uint32(0)) {
			metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
			return 0, 0, fmt.Errorf("snapshot: mapper key too long: %d bytes", len(pairs[i].Key))
		}
		binary.LittleEndian.PutUint64(scratch[0:8], uint64(pairs[i].ID))
		binary.LittleEndian.PutUint32(scratch[8:12], uint32(len(pairs[i].Key)))
		if _, err := tee.Write(scratch[:12]); err != nil {
			metrics.IncCounter("store.snapshot.WriteMapperString.errors", 1)
			return 0, 0, err
		}
		if pairs[i].Key != "" {
			if _, err := io.WriteString(tee, pairs[i].Key); err != nil {
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

// keyEncoder is the minimal slice of [txn.Codec] that [WriteMapper]
// needs: the append-style key encoder. Declaring it locally keeps the
// snapshot package independent of store/txn (the snapshot layer sits
// below the transaction layer); any concrete txn.Codec[N] satisfies it
// structurally because the method set matches.
type keyEncoder[N comparable] interface {
	Encode(buf []byte, v N) ([]byte, error)
}

// keyDecoder is the minimal slice of [txn.Codec] that
// [ApplyMapperToGraphWithCodec] needs: the head-of-buffer key decoder.
// As with [keyEncoder] it is declared locally to avoid an upward
// dependency on store/txn; any txn.Codec[N] satisfies it structurally.
type keyDecoder[N comparable] interface {
	Decode(buf []byte) (value N, rest []byte, err error)
}

// WriteMapper serialises every (NodeID -> key) pair held by m into w,
// generalising [WriteMapperString] to any comparable key type N via the
// supplied codec. It returns the number of bytes written and the
// CRC32C of the serialised payload, both recorded in the manifest's
// [FileEntry] for the mapper.bin component so [LoadSnapshotFull] can
// verify integrity at load time.
//
// Back-compatibility: when N is the canonical string type the function
// delegates to [WriteMapperString], which emits the frozen version-1
// layout (raw UTF-8 key bytes, no codec framing). The on-disk image is
// therefore byte-identical to every string mapper.bin produced before
// the codec generalisation, regardless of which string codec is
// supplied. For any other N the function emits a version-2 layout whose
// per-record key bytes are the output of codec.Encode.
//
// On-disk layout (version 2, all little-endian):
//
//	uint32  magic           ('GMAP', 0x50414D47)
//	uint16  formatVersion   (2)
//	uint64  pairCount
//	for each pair:
//	    uint64  nodeID
//	    uint32  keyLen       (length of the codec-encoded key bytes)
//	    [keyLen]byte key     (codec.Encode output for the natural key)
//
// Pairs are emitted in [graph.Mapper.Walk] order (shard-major,
// intra-index-major) so the read side reconstructs the mapper
// deterministically. The CRC32C covers the entire on-disk file,
// including the magic header.
func WriteMapper[N comparable](w io.Writer, m *graph.Mapper[N], codec keyEncoder[N]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteMapper").Stop()

	// String keys reuse the frozen version-1 path so the bytes stay
	// identical to the pre-codec writer. The any() probe matches the
	// exact dynamic type, mirroring the writer-side dispatch in full.go.
	if sm, ok := any(m).(*graph.Mapper[string]); ok {
		return WriteMapperString(w, sm)
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, mapperMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, mapperFormatVersionCodec); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
		return 0, 0, err
	}

	// Collect (id, encoded-key) pairs in Walk order. Each key is encoded
	// once here; the per-shard RLocks the mapper takes internally give the
	// writer a consistent view under concurrent reads.
	ids, keys, encErr := collectEncodedMapperPairs(m, codec)
	if encErr != nil {
		metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
		return 0, 0, encErr
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(ids))); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
		return 0, 0, err
	}

	total := int64(4 + 2 + 8)
	// scratch holds each record's fixed 12-byte prefix — ID(8) | keyLen(4) —
	// reused across records so packing with PutUintNN and one Write replaces
	// the per-field binary.Write reflection with zero per-record allocations,
	// byte-identically.
	var scratch [12]byte
	for i := range ids {
		if uint64(len(keys[i])) > uint64(^uint32(0)) {
			metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
			return 0, 0, fmt.Errorf("snapshot: mapper key too long: %d bytes", len(keys[i]))
		}
		binary.LittleEndian.PutUint64(scratch[0:8], uint64(ids[i]))
		binary.LittleEndian.PutUint32(scratch[8:12], uint32(len(keys[i])))
		if _, err := tee.Write(scratch[:12]); err != nil {
			metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
			return 0, 0, err
		}
		if len(keys[i]) > 0 {
			if _, err := tee.Write(keys[i]); err != nil {
				metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
				return 0, 0, err
			}
		}
		total += int64(8 + 4 + len(keys[i]))
	}

	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteMapper.errors", 1)
		return 0, 0, err
	}
	return total, hasher.Sum32(), nil
}

// collectEncodedMapperPairs walks m in Walk order and returns parallel
// (NodeID, codec-encoded key) slices. Encoding happens inside Walk so a
// single consistent view of the interning table is serialised. A codec
// error aborts the collection and surfaces to the caller, which fails
// the whole snapshot write rather than persisting a partial mapper.
func collectEncodedMapperPairs[N comparable](m *graph.Mapper[N], codec keyEncoder[N]) (ids []graph.NodeID, keys [][]byte, err error) {
	n := m.Len()
	ids = make([]graph.NodeID, 0, n)
	keys = make([][]byte, 0, n)
	m.Walk(func(id graph.NodeID, k N) bool {
		enc, eerr := codec.Encode(nil, k)
		if eerr != nil {
			err = fmt.Errorf("snapshot: encode mapper key for node %d: %w", uint64(id), eerr)
			return false
		}
		ids = append(ids, id)
		keys = append(keys, enc)
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	return ids, keys, nil
}

// ReadMapperBytes parses a mapper.bin payload produced by [WriteMapper]
// for a non-string key type (version 2): the per-record key bytes are
// the opaque codec output and are returned verbatim in
// [MapperReadback.RawPairs] for the caller to decode with the matching
// codec. A version-1 (string) payload is accepted too — its UTF-8 key
// bytes are returned as RawPairs so a single reader path can serve both
// layouts when a codec is in hand.
//
// Structural validation matches [ReadMapperString]: bad magic, an
// unsupported version, a truncated record, or an implausible length
// prefix all surface as [ErrMapperCorrupted]. The caller verifies the
// surrounding manifest CRC ([readVerifiedMapperBytes] does this); this
// function enforces only the structural contract.
func ReadMapperBytes(r io.Reader) (MapperReadback, error) {
	defer metrics.Time("store.snapshot.ReadMapperBytes").Stop()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if magic != mapperMagic {
		metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: bad magic %#x", ErrMapperCorrupted, magic)
	}

	var version uint16
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if version != mapperFormatVersionCodec && version != mapperFormatVersionString {
		metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: unsupported mapper format version %d",
			ErrMapperCorrupted, version)
	}

	var pairCount uint64
	if err := binary.Read(br, binary.LittleEndian, &pairCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if pairCount > 1<<40 {
		metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
		return MapperReadback{}, fmt.Errorf("%w: implausible pair count %d",
			ErrMapperCorrupted, pairCount)
	}
	// Clamp the eager reservation: a hostile pairCount (up to 1<<40) is bounded
	// to mapperCapHintMax; the per-pair read loop grows via append and fails on
	// the first truncated read.
	raw := make([]MapperRawPair, 0, capHint(pairCount, mapperCapHintMax))
	for i := uint64(0); i < pairCount; i++ {
		var idRaw uint64
		if err := binary.Read(br, binary.LittleEndian, &idRaw); err != nil {
			metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
			return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
		}
		var keyLen uint32
		if err := binary.Read(br, binary.LittleEndian, &keyLen); err != nil {
			metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
			return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
		}
		if keyLen > maxMapperKeyLen {
			metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
			return MapperReadback{}, fmt.Errorf("%w: key len %d > %d",
				ErrMapperCorrupted, keyLen, maxMapperKeyLen)
		}
		buf := make([]byte, keyLen)
		if keyLen > 0 {
			if _, err := io.ReadFull(br, buf); err != nil {
				metrics.IncCounter("store.snapshot.ReadMapperBytes.errors", 1)
				return MapperReadback{}, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
			}
		}
		raw = append(raw, MapperRawPair{ID: graph.NodeID(idRaw), Key: buf})
	}

	return MapperReadback{RawPairs: raw}, nil
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
	defer metrics.Time("store.snapshot.ReadMapperString").Stop()
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
	if version != mapperFormatVersionString {
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
	// Clamp the eager reservation: a hostile pairCount (up to 1<<40) is bounded
	// to mapperCapHintMax; the per-pair read loop grows via append and fails on
	// the first truncated read.
	pairs := make([]MapperPair, 0, capHint(pairCount, mapperCapHintMax))
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
		pairs = append(pairs, MapperPair{ID: graph.NodeID(idRaw), Key: string(buf)})
	}

	return MapperReadback{Pairs: pairs}, nil
}

// readVerifiedMapper opens path, runs the file bytes through CRC32C
// and the structural mapper reader simultaneously, and returns the
// parsed readback iff the CRC matches expected. Any disagreement
// surfaces as [ErrCorrupted]. Mirrors [readVerifiedCSR] /
// [readVerifiedLabels] / [readVerifiedProperties] in shape. size bounds the
// body reader (see [readVerifiedLabels]).
func readVerifiedMapper(fsys fileSystem, path string, expected uint32, size int64) (MapperReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return MapperReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
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

// peekMapperVersion reads just the magic + format-version prefix of a
// mapper.bin file so the loader can choose the right reader without a
// full parse. A bad magic surfaces as [ErrMapperCorrupted]; an I/O or
// open error surfaces verbatim.
func peekMapperVersion(fsys fileSystem, path string) (uint16, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return 0, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()
	var hdr struct {
		Magic   uint32
		Version uint16
	}
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMapperCorrupted, err)
	}
	if hdr.Magic != mapperMagic {
		return 0, fmt.Errorf("%w: bad magic %#x", ErrMapperCorrupted, hdr.Magic)
	}
	return hdr.Version, nil
}

// readVerifiedMapperBytes is the version-aware dual of
// [readVerifiedMapper]: it opens path, runs the bytes through CRC32C
// and [ReadMapperBytes] simultaneously, and returns the raw-byte
// readback iff the CRC matches expected. It accepts both the
// version-1 (string) and version-2 (codec) layouts, returning the
// per-record key bytes in [MapperReadback.RawPairs] for the caller to
// decode through the matching codec. Any disagreement surfaces as
// [ErrCorrupted]. size bounds the body reader (see [readVerifiedLabels]).
func readVerifiedMapperBytes(fsys fileSystem, path string, expected uint32, size int64) (MapperReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return MapperReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
	parsed, err := ReadMapperBytes(tee)
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

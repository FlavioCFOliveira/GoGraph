package snapshot

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// IndexDefsFile is the conventional file name carrying the durable secondary
// index DEFINITION set inside a snapshot directory. It is a sibling of [CSRFile]
// and is referenced by an additional entry in the [Manifest.Files] slice.
//
// It is DISTINCT from [IndexesDir] ("indexes"), which holds the serialized
// per-index PAYLOADS (a best-effort recovery speed-up). The definition set
// (label, property, kind, name) is the load-bearing component: recovery rebuilds
// each index by backfilling it from the recovered graph, so the durable thing
// that must survive a WAL-truncating checkpoint is the definition, not the
// payload (#1755).
//
// The component is OPTIONAL: the writer emits it only when at least one index is
// declared, so a snapshot of a graph with no indexes is byte-identical to one
// produced before this component existed. A snapshot without the component loads
// as an empty index-definition set — the backward-compatibility contract.
//
// Forward compatibility is one-directional, matching constraints.bin: a reader
// that predates this component ignores the unknown file name and so would lose
// the index definitions (a downgrade hazard); upgrades (older snapshot, newer
// binary) are always safe.
const IndexDefsFile = "indexdefs.bin"

// indexDefsMagic is the four-byte magic ('S','I','D','X') that prefixes every
// indexdefs.bin file. Stored as a uint32 in little-endian; spelled out as
// 0x58444953 because the magic bytes appear on disk as 'SIDX'.
const indexDefsMagic uint32 = 0x58444953

// indexDefsFormatVersion is the indexdefs.bin internal format version. It is
// independent of [ManifestVersion]: a future indexdefs.bin layout change bumps
// this word without forcing a manifest schema bump (the same discipline
// constraints.bin / tombstones.bin follow).
const indexDefsFormatVersion uint32 = 1

// indexDefsMaxCount bounds the declared record count read from a hostile or
// corrupted file. It mirrors the implausibility ceiling the constraints reader
// applies; a real schema has at most a handful of indexes.
const indexDefsMaxCount uint32 = 1 << 20

// indexDefsMaxStringLen bounds a single name/label/property field read from a
// hostile file, before the per-field read allocates. 64 KiB is far above any
// legitimate identifier yet caps a crafted length prefix.
const indexDefsMaxStringLen = 1 << 16

// ErrIndexDefsCorrupted is returned by [ReadIndexDefs] when the indexdefs.bin
// file is structurally malformed (bad magic, unsupported format version,
// implausible count or string length, or a truncated record).
var ErrIndexDefsCorrupted = errors.New("snapshot: indexdefs.bin corrupted")

// IndexDefSpec is one durable index definition: its kind tag (0 = hash, 1 =
// btree, matching store/txn.IndexKind), the user-defined name, the indexed node
// label, and the indexed property key. The snapshot layer keeps its own index
// type so it does not import the txn or recovery packages.
type IndexDefSpec struct {
	// Kind is the index-kind tag: 0 = hash, 1 = btree.
	Kind uint8
	// Name is the user-defined index name.
	Name string
	// Label is the indexed node label.
	Label string
	// Property is the indexed property key.
	Property string
}

// IndexDefsReadback is the structural parse of an indexdefs.bin file: the
// recovered index definitions in the order they were written (the writer sorts
// them deterministically). The caller maps them back into its engine index
// registry.
type IndexDefsReadback struct {
	Specs []IndexDefSpec
}

// WriteIndexDefs serialises specs into w in the indexdefs.bin format. It returns
// the number of bytes written and the CRC32C of the serialised payload — both
// stored in the manifest's [FileEntry] for the component so [LoadSnapshotFull]
// can verify integrity at load time.
//
// The CRC32C covers the entire on-disk file, including the magic header,
// matching the constraints.bin / tombstones.bin discipline. The records are
// emitted in deterministic order (kind, name, label, property) so the component
// is byte-identical across writes of the same logical state.
func WriteIndexDefs(w io.Writer, specs []IndexDefSpec) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteIndexDefs").Stop()

	ordered := append([]IndexDefSpec(nil), specs...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Property < b.Property
	})

	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, indexDefsMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteIndexDefs.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, indexDefsFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteIndexDefs.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint32(len(ordered))); err != nil {
		metrics.IncCounter("store.snapshot.WriteIndexDefs.errors", 1)
		return 0, 0, err
	}

	total := int64(4 + 4 + 4) // magic + formatVersion + count
	for i := range ordered {
		n, werr := writeIndexDefRecord(tee, ordered[i])
		if werr != nil {
			metrics.IncCounter("store.snapshot.WriteIndexDefs.errors", 1)
			return 0, 0, werr
		}
		total += n
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteIndexDefs.errors", 1)
		return 0, 0, err
	}
	return total, hasher.Sum32(), nil
}

// writeIndexDefRecord writes one record: a uint8 kind tag followed by three
// uint32-length-prefixed strings (name, label, property). It returns the number
// of bytes written.
func writeIndexDefRecord(w io.Writer, d IndexDefSpec) (int64, error) {
	if _, err := w.Write([]byte{d.Kind}); err != nil {
		return 0, err
	}
	total := int64(1)
	for _, s := range [...]string{d.Name, d.Label, d.Property} {
		if err := binary.Write(w, binary.LittleEndian, uint32(len(s))); err != nil {
			return 0, err
		}
		if _, err := io.WriteString(w, s); err != nil {
			return 0, err
		}
		total += int64(4 + len(s))
	}
	return total, nil
}

// ReadIndexDefs parses an indexdefs.bin payload produced by [WriteIndexDefs]. It
// performs strict structural validation: a missing or wrong magic, a future
// format-version word, an implausible record count, an over-long string field,
// or a truncated record all surface as [ErrIndexDefsCorrupted].
//
// The caller is responsible for verifying the surrounding manifest CRC matches
// the file bytes ([LoadSnapshotFull] does this); this function only enforces the
// structural contract.
func ReadIndexDefs(r io.Reader) (IndexDefsReadback, error) {
	defer metrics.Time("store.snapshot.ReadIndexDefs").Stop()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
		return IndexDefsReadback{}, fmt.Errorf("%w: %w", ErrIndexDefsCorrupted, err)
	}
	if magic != indexDefsMagic {
		metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
		return IndexDefsReadback{}, fmt.Errorf("%w: bad magic %#x", ErrIndexDefsCorrupted, magic)
	}
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
		return IndexDefsReadback{}, fmt.Errorf("%w: %w", ErrIndexDefsCorrupted, err)
	}
	if version != indexDefsFormatVersion {
		metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
		return IndexDefsReadback{}, fmt.Errorf("%w: unsupported indexdefs format version %d",
			ErrIndexDefsCorrupted, version)
	}

	var count uint32
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
		return IndexDefsReadback{}, fmt.Errorf("%w: %w", ErrIndexDefsCorrupted, err)
	}
	if count > indexDefsMaxCount {
		metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
		return IndexDefsReadback{}, fmt.Errorf("%w: implausible index count %d",
			ErrIndexDefsCorrupted, count)
	}

	specs := make([]IndexDefSpec, 0, count)
	for i := uint32(0); i < count; i++ {
		d, err := readIndexDefRecord(br)
		if err != nil {
			metrics.IncCounter("store.snapshot.ReadIndexDefs.errors", 1)
			return IndexDefsReadback{}, err
		}
		specs = append(specs, d)
	}
	return IndexDefsReadback{Specs: specs}, nil
}

// readIndexDefRecord reads one record written by [writeIndexDefRecord].
func readIndexDefRecord(br io.Reader) (IndexDefSpec, error) {
	var kindByte [1]byte
	if _, err := io.ReadFull(br, kindByte[:]); err != nil {
		return IndexDefSpec{}, fmt.Errorf("%w: %w", ErrIndexDefsCorrupted, err)
	}
	name, err := readIndexDefString(br)
	if err != nil {
		return IndexDefSpec{}, err
	}
	label, err := readIndexDefString(br)
	if err != nil {
		return IndexDefSpec{}, err
	}
	prop, err := readIndexDefString(br)
	if err != nil {
		return IndexDefSpec{}, err
	}
	return IndexDefSpec{Kind: kindByte[0], Name: name, Label: label, Property: prop}, nil
}

// readIndexDefString reads a uint32-length-prefixed string, bounding the length
// against [indexDefsMaxStringLen] before allocating.
func readIndexDefString(br io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
		return "", fmt.Errorf("%w: %w", ErrIndexDefsCorrupted, err)
	}
	if n > indexDefsMaxStringLen {
		return "", fmt.Errorf("%w: implausible string length %d", ErrIndexDefsCorrupted, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", fmt.Errorf("%w: %w", ErrIndexDefsCorrupted, err)
	}
	return string(buf), nil
}

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

// ConstraintsFile is the conventional file name carrying the durable schema
// constraint set inside a snapshot directory. It is a sibling of [CSRFile] and
// is referenced by an additional entry in the [Manifest.Files] slice.
//
// The component is OPTIONAL: the writer emits it only when at least one
// constraint is declared, so a snapshot of a graph with no constraints is
// byte-identical to one produced before this component existed. A snapshot
// without the component loads as an empty constraint set — the
// backward-compatibility contract.
//
// Forward compatibility is one-directional, matching tombstones.bin: a reader
// that predates this component ignores the unknown file name and so would lose
// the constraints (a downgrade hazard); upgrades (older snapshot, newer
// binary) are always safe.
const ConstraintsFile = "constraints.bin"

// constraintsMagic is the four-byte magic ('S','C','N','S') that prefixes
// every constraints.bin file. Stored as a uint32 in little-endian; spelled out
// as 0x534E4353 because the magic bytes appear on disk as 'SCNS'.
const constraintsMagic uint32 = 0x534E4353

// constraintsFormatVersion is the constraints.bin internal format version. It
// is independent of [ManifestVersion]: a future constraints.bin layout change
// bumps this word without forcing a manifest schema bump (the same discipline
// tombstones.bin / labels.bin follow).
const constraintsFormatVersion uint32 = 1

// constraintsMaxCount bounds the declared record count read from a hostile or
// corrupted file. It mirrors the implausibility ceiling the tombstones reader
// applies; a real schema has at most a handful of constraints.
const constraintsMaxCount uint32 = 1 << 20

// constraintsMaxStringLen bounds a single label/property/name field read from
// a hostile file, before the per-field read allocates. 64 KiB is far above any
// legitimate identifier yet caps a crafted length prefix.
const constraintsMaxStringLen = 1 << 16

// ErrConstraintsCorrupted is returned by [ReadConstraints] when the
// constraints.bin file is structurally malformed (bad magic, unsupported
// format version, implausible count or string length, or a truncated record).
var ErrConstraintsCorrupted = errors.New("snapshot: constraints.bin corrupted")

// ConstraintSpec is one durable constraint definition: its kind tag (0 =
// UNIQUE, 1 = NOT NULL, matching store/txn.ConstraintKind), the constrained
// node label, the constrained property key, and the user-defined name. The
// snapshot layer keeps its own constraint type so it does not import the txn
// or recovery packages.
type ConstraintSpec struct {
	// Kind is the constraint-kind tag: 0 = UNIQUE, 1 = NOT NULL.
	Kind uint8
	// Label is the constrained node label.
	Label string
	// Property is the constrained property key.
	Property string
	// Name is the user-defined constraint name.
	Name string
}

// ConstraintsReadback is the structural parse of a constraints.bin file: the
// recovered constraint definitions in the order they were written (the writer
// sorts them deterministically). The caller maps them back into its engine
// constraint registry.
type ConstraintsReadback struct {
	Specs []ConstraintSpec
}

// WriteConstraints serialises specs into w in the constraints.bin format. It
// returns the number of bytes written and the CRC32C of the serialised
// payload — both stored in the manifest's [FileEntry] for the component so
// [LoadSnapshotFull] can verify integrity at load time.
//
// The CRC32C covers the entire on-disk file, including the magic header,
// matching the tombstones.bin / labels.bin discipline. The records are emitted
// in deterministic order (kind, label, property, name) so the component is
// byte-identical across writes of the same logical state.
func WriteConstraints(w io.Writer, specs []ConstraintSpec) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteConstraints")()

	ordered := append([]ConstraintSpec(nil), specs...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Property != b.Property {
			return a.Property < b.Property
		}
		return a.Name < b.Name
	})

	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, constraintsMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteConstraints.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, constraintsFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteConstraints.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint32(len(ordered))); err != nil {
		metrics.IncCounter("store.snapshot.WriteConstraints.errors", 1)
		return 0, 0, err
	}

	total := int64(4 + 4 + 4) // magic + formatVersion + count
	for i := range ordered {
		n, werr := writeConstraintRecord(tee, ordered[i])
		if werr != nil {
			metrics.IncCounter("store.snapshot.WriteConstraints.errors", 1)
			return 0, 0, werr
		}
		total += n
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteConstraints.errors", 1)
		return 0, 0, err
	}
	return total, hasher.Sum32(), nil
}

// writeConstraintRecord writes one record: a uint8 kind tag followed by three
// uint32-length-prefixed strings (label, property, name). It returns the
// number of bytes written.
func writeConstraintRecord(w io.Writer, c ConstraintSpec) (int64, error) {
	if _, err := w.Write([]byte{c.Kind}); err != nil {
		return 0, err
	}
	total := int64(1)
	for _, s := range [...]string{c.Label, c.Property, c.Name} {
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

// ReadConstraints parses a constraints.bin payload produced by
// [WriteConstraints]. It performs strict structural validation: a missing or
// wrong magic, a future format-version word, an implausible record count, an
// over-long string field, or a truncated record all surface as
// [ErrConstraintsCorrupted].
//
// The caller is responsible for verifying the surrounding manifest CRC matches
// the file bytes ([LoadSnapshotFull] does this); this function only enforces
// the structural contract.
func ReadConstraints(r io.Reader) (ConstraintsReadback, error) {
	defer metrics.Time("store.snapshot.ReadConstraints")()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrConstraintsCorrupted, err)
	}
	if magic != constraintsMagic {
		metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
		return ConstraintsReadback{}, fmt.Errorf("%w: bad magic %#x", ErrConstraintsCorrupted, magic)
	}
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrConstraintsCorrupted, err)
	}
	if version != constraintsFormatVersion {
		metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
		return ConstraintsReadback{}, fmt.Errorf("%w: unsupported constraints format version %d",
			ErrConstraintsCorrupted, version)
	}

	var count uint32
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrConstraintsCorrupted, err)
	}
	if count > constraintsMaxCount {
		metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
		return ConstraintsReadback{}, fmt.Errorf("%w: implausible constraint count %d",
			ErrConstraintsCorrupted, count)
	}

	specs := make([]ConstraintSpec, 0, count)
	for i := uint32(0); i < count; i++ {
		c, err := readConstraintRecord(br)
		if err != nil {
			metrics.IncCounter("store.snapshot.ReadConstraints.errors", 1)
			return ConstraintsReadback{}, err
		}
		specs = append(specs, c)
	}
	return ConstraintsReadback{Specs: specs}, nil
}

// readConstraintRecord reads one record written by [writeConstraintRecord].
func readConstraintRecord(br io.Reader) (ConstraintSpec, error) {
	var kindByte [1]byte
	if _, err := io.ReadFull(br, kindByte[:]); err != nil {
		return ConstraintSpec{}, fmt.Errorf("%w: %w", ErrConstraintsCorrupted, err)
	}
	label, err := readConstraintString(br)
	if err != nil {
		return ConstraintSpec{}, err
	}
	prop, err := readConstraintString(br)
	if err != nil {
		return ConstraintSpec{}, err
	}
	name, err := readConstraintString(br)
	if err != nil {
		return ConstraintSpec{}, err
	}
	return ConstraintSpec{Kind: kindByte[0], Label: label, Property: prop, Name: name}, nil
}

// readConstraintString reads a uint32-length-prefixed string, bounding the
// length against [constraintsMaxStringLen] before allocating.
func readConstraintString(br io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
		return "", fmt.Errorf("%w: %w", ErrConstraintsCorrupted, err)
	}
	if n > constraintsMaxStringLen {
		return "", fmt.Errorf("%w: implausible string length %d", ErrConstraintsCorrupted, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", fmt.Errorf("%w: %w", ErrConstraintsCorrupted, err)
	}
	return string(buf), nil
}

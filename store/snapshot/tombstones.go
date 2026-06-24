package snapshot

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// TombstonesFile is the conventional file name carrying the durable LPG
// node-tombstone set inside a snapshot directory. It is a sibling of
// [CSRFile] and is referenced by an additional entry in the
// [Manifest.Files] slice.
//
// The component is OPTIONAL: the writer emits it only when the graph has
// at least one tombstoned node, so a snapshot of a graph that never
// deleted a node is byte-identical to one produced before this component
// existed. A snapshot without the component loads as an empty tombstone
// set — the backward-compatibility contract.
//
// Forward compatibility is one-directional. A reader that predates this
// component ignores the unknown file name and so would resurrect deleted
// nodes; reopening a store written by a current binary with an older
// binary is therefore a downgrade hazard. Upgrades (older snapshot, newer
// binary) are always safe.
const TombstonesFile = "tombstones.bin"

// tombstonesMagic is the four-byte magic ('S','T','M','B') that prefixes
// every tombstones.bin file. Stored as a uint32 in little-endian; spelled
// out as 0x424D5453 because the magic bytes appear on disk as 'STMB'.
const tombstonesMagic uint32 = 0x424D5453

// tombstonesFormatVersion is the tombstones.bin internal format version.
// It is independent of [ManifestVersion]: a future tombstones.bin layout
// change bumps this word without forcing a manifest schema bump (the same
// discipline labels.bin and properties.bin follow).
const tombstonesFormatVersion uint32 = 1

// tombstonesMaxCount bounds the declared id count read from a hostile or
// corrupted file. It mirrors the implausibility ceiling the labels reader
// applies to its record counts.
const tombstonesMaxCount uint64 = 1 << 40

// tombstonesCapHintMax caps the eager slice reservation so a hostile count
// (up to tombstonesMaxCount) cannot drive a multi-gigabyte allocation
// before the per-id reads have a chance to fail on a truncated file.
const tombstonesCapHintMax = 1 << 20

// ErrTombstonesCorrupted is returned by [ReadTombstones] when the
// tombstones.bin file is structurally malformed (bad magic, unsupported
// format version, implausible count, or a truncated record).
var ErrTombstonesCorrupted = errors.New("snapshot: tombstones.bin corrupted")

// TombstonesReadback is the structural parse of a tombstones.bin file: the
// sorted set of removed NodeIDs. The caller materialises it back into a
// live [lpg.Graph] via [ApplyTombstonesToGraph].
type TombstonesReadback struct {
	IDs []graph.NodeID
}

// WriteTombstones serialises g's current tombstone set (the NodeIDs removed
// via [lpg.Graph.RemoveNode]) into w in the tombstones.bin format. It
// returns the number of bytes written and the CRC32C of the serialised
// payload — both stored in the manifest's [FileEntry] for the component so
// [LoadSnapshotFull] can verify integrity at load time.
//
// The CRC32C covers the entire on-disk file, including the magic header,
// matching the labels.bin / properties.bin discipline. The id list is
// emitted in ascending order ([lpg.Graph.TombstonedIDs] sorts it) so the
// component is deterministic across writes of the same logical state.
func WriteTombstones[N comparable, W any](w io.Writer, g *lpg.Graph[N, W]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteTombstones").Stop()

	ids := g.TombstonedIDs()

	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, tombstonesMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteTombstones.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, tombstonesFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteTombstones.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(ids))); err != nil {
		metrics.IncCounter("store.snapshot.WriteTombstones.errors", 1)
		return 0, 0, err
	}
	for _, id := range ids {
		if err := binary.Write(tee, binary.LittleEndian, uint64(id)); err != nil {
			metrics.IncCounter("store.snapshot.WriteTombstones.errors", 1)
			return 0, 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteTombstones.errors", 1)
		return 0, 0, err
	}

	// Total bytes: 4 (magic) + 4 (formatVersion) + 8 (count) + count*8.
	total := int64(4+4+8) + int64(len(ids))*8
	return total, hasher.Sum32(), nil
}

// ReadTombstones parses a tombstones.bin payload produced by
// [WriteTombstones]. It performs strict structural validation: a missing
// or wrong magic, a future format-version word, an implausible count, or a
// truncated record all surface as [ErrTombstonesCorrupted].
//
// The caller is responsible for verifying the surrounding manifest CRC
// matches the file bytes (the [LoadSnapshotFull] helper does this); this
// function only enforces the structural contract.
func ReadTombstones(r io.Reader) (TombstonesReadback, error) {
	defer metrics.Time("store.snapshot.ReadTombstones").Stop()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrTombstonesCorrupted, err)
	}
	if magic != tombstonesMagic {
		metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
		return TombstonesReadback{}, fmt.Errorf("%w: bad magic %#x", ErrTombstonesCorrupted, magic)
	}
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrTombstonesCorrupted, err)
	}
	if version != tombstonesFormatVersion {
		metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
		return TombstonesReadback{}, fmt.Errorf("%w: unsupported tombstones format version %d",
			ErrTombstonesCorrupted, version)
	}

	var count uint64
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrTombstonesCorrupted, err)
	}
	if count > tombstonesMaxCount {
		metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
		return TombstonesReadback{}, fmt.Errorf("%w: implausible tombstone count %d",
			ErrTombstonesCorrupted, count)
	}

	hint := count
	if hint > tombstonesCapHintMax {
		hint = tombstonesCapHintMax
	}
	ids := make([]graph.NodeID, 0, hint)
	for i := uint64(0); i < count; i++ {
		var id uint64
		if err := binary.Read(br, binary.LittleEndian, &id); err != nil {
			metrics.IncCounter("store.snapshot.ReadTombstones.errors", 1)
			return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrTombstonesCorrupted, err)
		}
		ids = append(ids, graph.NodeID(id))
	}
	return TombstonesReadback{IDs: ids}, nil
}

// ApplyTombstonesToGraph replays rb into a live g, re-tombstoning every
// NodeID the snapshot recorded as removed. It must run AFTER the snapshot
// nodes are loaded (mapper + CSR) so the ids it restores reference the same
// stable slots; it re-tombstones by id directly via
// [lpg.Graph.RestoreTombstones] and so does not require the natural keys to
// be resolvable.
//
// A later WAL re-create (OpAddNode) for any of these ids still revives it,
// preserving the chronology of a delete→recreate cycle that straddles the
// snapshot boundary.
func ApplyTombstonesToGraph[N comparable, W any](g *lpg.Graph[N, W], rb TombstonesReadback) {
	defer metrics.Time("store.snapshot.ApplyTombstonesToGraph").Stop()
	if len(rb.IDs) == 0 {
		return
	}
	g.RestoreTombstones(rb.IDs)
}

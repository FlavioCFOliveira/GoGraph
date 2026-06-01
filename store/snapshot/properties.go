package snapshot

// properties.bin format (little-endian throughout).
//
//   ['SPRP' magic = 0x53505250]  uint32
//   [format-version = 1]         uint32
//   [keyTableLen]                uint64
//   [keys: keyTableLen × (uint32 utf8Len, [utf8Len]byte)]
//   [nodePropEntries]            uint64
//   [node records:
//       uint64 NodeID
//       uint32 keyIdx       (index into the embedded key string table)
//       uint8  kind         (lpg.PropertyKind: PropString..PropList)
//       uint32 valueLen
//       [valueLen]byte value
//   ]
//   [edgePropEntries]            uint64
//   [edge records:
//       uint64 src
//       uint64 dst
//       uint32 keyIdx
//       uint8  kind
//       uint32 valueLen
//       [valueLen]byte value
//   ]
//
// Value bytes per kind (fixed-width — varint is intentionally avoided
// so the file is straightforward to dump and inspect with xxd):
//
//   PropString  (1) → raw utf-8 bytes (valueLen = byte length)
//   PropInt64   (2) → 8 bytes LE (binary.LittleEndian.Uint64)
//   PropFloat64 (3) → 8 bytes LE (math.Float64bits → LE uint64)
//   PropBool    (4) → 1 byte (0x00 false / 0x01 true)
//   PropTime    (5) → 16 bytes: uint64 seconds (Unix epoch) || uint64
//                     nanoseconds-within-second. Reconstituted via
//                     time.Unix(sec, nsec).UTC().
//   PropBytes   (6) → raw bytes (valueLen = byte length).
//   PropList    (7) → uint32 LE element-count followed by element-count
//                     sub-records, each encoded as:
//                         uint8  elem-kind
//                         uint32 elem-valueLen
//                         [elem-valueLen]byte elem-value
//                     Nesting is not permitted: list elements must not be
//                     PropList themselves.
//
// The whole file is covered by a single CRC32C (Castagnoli) recorded
// in the surrounding manifest's FileEntry, including the magic
// header. The reader recomputes the CRC end-to-end at load time.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// PropertiesFile is the conventional file name carrying the durable
// LPG typed-property state inside a v2 snapshot directory. It is a
// sibling of [CSRFile] and [LabelsFile] and is referenced by an
// additional entry in [Manifest.Files] when the writer emitted any
// property at all.
const PropertiesFile = "properties.bin"

// propertiesMagic is the four-byte magic ('S','P','R','P') that
// prefixes every properties.bin file. Stored as a uint32 LE; the
// magic bytes appear on disk as 'SPRP'.
const propertiesMagic uint32 = 0x50525053

// propertiesFormatVersion is the properties.bin internal format
// version. It is independent of [ManifestVersion]: a future
// properties.bin layout change bumps this byte without forcing a
// manifest schema bump.
const propertiesFormatVersion uint32 = 1

// ErrPropertiesCorrupted is returned by [ReadProperties] when the
// properties.bin file is structurally malformed (bad magic, truncated
// record, key index past the embedded key table, unknown kind, or a
// value length implausibly large).
var ErrPropertiesCorrupted = errors.New("snapshot: properties.bin corrupted")

// NodePropertyEntry pairs a NodeID with the key string-table index,
// the kind tag, and the encoded value bytes for one property attached
// to that node. A node carrying P properties yields P entries.
type NodePropertyEntry struct {
	NodeID     uint64
	KeyIdx     uint32
	Kind       lpg.PropertyKind
	ValueBytes []byte
}

// EdgePropertyEntry pairs an (src, dst) NodeID couple with the key
// string-table index, the kind tag, and the encoded value bytes for
// one property attached to that edge. An edge carrying P properties
// yields P entries; as with labels, parallel edges between the same
// endpoints fold into the same edgeKey on disk just as they do in
// [lpg.Graph]'s in-memory shards.
type EdgePropertyEntry struct {
	Src        uint64
	Dst        uint64
	KeyIdx     uint32
	Kind       lpg.PropertyKind
	ValueBytes []byte
}

// PropertiesReadback is the structural parse of a properties.bin
// file. The caller materialises it back into a live [lpg.Graph] via
// [ApplyPropertiesToGraph] once the underlying mapper is populated.
type PropertiesReadback struct {
	Keys           []string
	NodeProperties []NodePropertyEntry
	EdgeProperties []EdgePropertyEntry
}

// timeValueSize is the fixed on-disk size of an encoded PropTime
// value (uint64 sec + uint64 nsec).
const timeValueSize = 16

// boolValueSize is the fixed on-disk size of an encoded PropBool
// value (single byte: 0x00 / 0x01).
const boolValueSize = 1

// fixed64ValueSize is the fixed on-disk size of an encoded PropInt64
// or PropFloat64 value.
const fixed64ValueSize = 8

// maxValueLen caps a single property's encoded value at 1 GiB.
// Larger values are rejected by [ReadProperties] as corruption —
// without this cap, a flipped byte in the length prefix could ask the
// reader to allocate an absurd buffer.
const maxValueLen = 1 << 30

// WriteProperties serialises every node and edge property attached
// to g into w in the properties.bin format documented at the top of
// this file. It returns the number of bytes written and the CRC32C of
// the serialised payload — both stored in the manifest's [FileEntry]
// for the properties.bin component so [LoadSnapshotFull] can verify
// integrity at load time.
//
// The CRC32C covers the entire on-disk file, including the magic
// header. This lets the manifest's CRC field validate every byte of
// properties.bin end-to-end without a separate inner-payload
// checksum.
//
// The on-disk key string table is populated by walking g's
// [lpg.PropertyKeyRegistry] in interning order; the keyIdx written
// for each (node | edge) record indexes into that table.
//
// Concurrency contract: the walk relies on the same lock-free /
// RLock-only primitives the public LPG accessors expose. Properties
// added by a concurrent mutator race with the snapshot writer in the
// same way labels do — the writer either observes the new property
// (and the matching node/edge entry) or it does not, but never an
// inconsistent fragment.
//
//nolint:gocyclo // properties write: header + key table + node records + edge records, each guarded
func WriteProperties[N comparable, W any](w io.Writer, g *lpg.Graph[N, W]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteProperties")()

	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, propertiesMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, propertiesFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}

	// Snapshot the property-key table in interning order. Walking the
	// registry under its own RLock means a concurrent SetNodeProperty
	// / SetEdgeProperty that adds a brand-new key is serialised
	// against the snapshot writer — the writer either observes the
	// new key (and the matching record below) or it does not, but
	// never an inconsistent fragment.
	keys := snapshotPropertyKeys(g.PropertyKeys())
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(keys))); err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}
	for _, key := range keys {
		if uint64(len(key)) > uint64(^uint32(0)) {
			metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
			return 0, 0, fmt.Errorf("snapshot: property key too long: %d bytes", len(key))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(key))); err != nil {
			metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
			return 0, 0, err
		}
		if _, err := tee.Write([]byte(key)); err != nil {
			metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
			return 0, 0, err
		}
	}

	nodeRecs, err := collectNodePropertyRecords(g, keys)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(nodeRecs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}
	nodeBytes := int64(0)
	for i := range nodeRecs {
		nb, err := writeNodePropRecord(tee, &nodeRecs[i])
		if err != nil {
			metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
			return 0, 0, err
		}
		nodeBytes += nb
	}

	edgeRecs, err := collectEdgePropertyRecords(g, keys)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(edgeRecs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}
	edgeBytes := int64(0)
	for i := range edgeRecs {
		eb, err := writeEdgePropRecord(tee, &edgeRecs[i])
		if err != nil {
			metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
			return 0, 0, err
		}
		edgeBytes += eb
	}

	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteProperties.errors", 1)
		return 0, 0, err
	}

	// Total bytes: 4 (magic) + 4 (formatVersion) + 8 (keyCount)
	// + for each key: 4 (utf8Len) + utf8Len bytes
	// + 8 (nodeCount) + nodeBytes
	// + 8 (edgeCount) + edgeBytes.
	total := int64(4 + 4 + 8)
	for _, key := range keys {
		total += 4 + int64(len(key))
	}
	total += 8 + nodeBytes
	total += 8 + edgeBytes
	return total, hasher.Sum32(), nil
}

// writeNodePropRecord writes one node property record and returns the
// number of bytes emitted (always 8 + 4 + 1 + 4 + len(value)).
func writeNodePropRecord(w io.Writer, rec *NodePropertyEntry) (int64, error) {
	if err := binary.Write(w, binary.LittleEndian, rec.NodeID); err != nil {
		return 0, err
	}
	if err := binary.Write(w, binary.LittleEndian, rec.KeyIdx); err != nil {
		return 0, err
	}
	if _, err := w.Write([]byte{byte(rec.Kind)}); err != nil {
		return 0, err
	}
	if uint64(len(rec.ValueBytes)) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("snapshot: node property value too long: %d bytes", len(rec.ValueBytes))
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(rec.ValueBytes))); err != nil {
		return 0, err
	}
	if len(rec.ValueBytes) > 0 {
		if _, err := w.Write(rec.ValueBytes); err != nil {
			return 0, err
		}
	}
	return int64(8 + 4 + 1 + 4 + len(rec.ValueBytes)), nil
}

// writeEdgePropRecord writes one edge property record and returns the
// number of bytes emitted (8 + 8 + 4 + 1 + 4 + len(value)).
func writeEdgePropRecord(w io.Writer, rec *EdgePropertyEntry) (int64, error) {
	if err := binary.Write(w, binary.LittleEndian, rec.Src); err != nil {
		return 0, err
	}
	if err := binary.Write(w, binary.LittleEndian, rec.Dst); err != nil {
		return 0, err
	}
	if err := binary.Write(w, binary.LittleEndian, rec.KeyIdx); err != nil {
		return 0, err
	}
	if _, err := w.Write([]byte{byte(rec.Kind)}); err != nil {
		return 0, err
	}
	if uint64(len(rec.ValueBytes)) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("snapshot: edge property value too long: %d bytes", len(rec.ValueBytes))
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(rec.ValueBytes))); err != nil {
		return 0, err
	}
	if len(rec.ValueBytes) > 0 {
		if _, err := w.Write(rec.ValueBytes); err != nil {
			return 0, err
		}
	}
	return int64(8 + 8 + 4 + 1 + 4 + len(rec.ValueBytes)), nil
}

// snapshotPropertyKeys returns the key table in interning order. We
// rely on [lpg.PropertyKeyRegistry.Resolve] which honours the
// registry's own RWMutex; iterating by id from 0 upwards is
// well-defined because PropertyKeyID is dense and assigned
// monotonically by [lpg.PropertyKeyRegistry.Intern].
func snapshotPropertyKeys(reg *lpg.PropertyKeyRegistry) []string {
	out := make([]string, 0, 16)
	for i := uint32(0); ; i++ {
		name, ok := reg.Resolve(lpg.PropertyKeyID(i))
		if !ok {
			break
		}
		out = append(out, name)
	}
	return out
}

// collectNodePropertyRecords walks every interned node and emits one
// [NodePropertyEntry] per (node, key, value) triple. keys is the
// registry snapshot taken by [snapshotPropertyKeys]; we re-intern
// each key name to translate the LPG's runtime PropertyKeyID back
// into the snapshot's key-table index. The two indexes are equal in
// practice (both follow interning order), but the explicit lookup
// keeps the writer robust against a future divergence.
func collectNodePropertyRecords[N comparable, W any](
	g *lpg.Graph[N, W],
	keys []string,
) ([]NodePropertyEntry, error) {
	idx := buildKeyIndex(keys)
	out := make([]NodePropertyEntry, 0, 32)
	var walkErr error
	g.AdjList().Mapper().Walk(func(id graph.NodeID, n N) bool {
		props := g.NodeProperties(n)
		for key, val := range props {
			ki, ok := idx[key]
			if !ok {
				walkErr = fmt.Errorf("snapshot: node property key %q not in registry snapshot", key)
				return false
			}
			vb, encErr := encodePropertyValue(val)
			if encErr != nil {
				walkErr = encErr
				return false
			}
			out = append(out, NodePropertyEntry{
				NodeID:     uint64(id),
				KeyIdx:     ki,
				Kind:       val.Kind(),
				ValueBytes: vb,
			})
		}
		return true
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// collectEdgePropertyRecords walks every interned source node,
// snapshots its adjacency via the lock-free [adjlist.AdjList.LoadEntry],
// and emits one [EdgePropertyEntry] per (src, dst, key, value) tuple.
// Each (src, dst) pair is visited once even when the graph is a
// multigraph: edge properties in v1 are keyed by endpoints only,
// mirroring the LPG's in-memory shard semantics.
func collectEdgePropertyRecords[N comparable, W any](
	g *lpg.Graph[N, W],
	keys []string,
) ([]EdgePropertyEntry, error) {
	idx := buildKeyIndex(keys)
	out := make([]EdgePropertyEntry, 0, 32)
	var walkErr error
	adj := g.AdjList()
	adj.Mapper().Walk(func(srcID graph.NodeID, srcN N) bool {
		neighbours, _ := adj.LoadEntry(srcID)
		if len(neighbours) == 0 {
			return true
		}
		seen := make(map[graph.NodeID]struct{}, len(neighbours))
		for _, dstID := range neighbours {
			if _, dup := seen[dstID]; dup {
				continue
			}
			seen[dstID] = struct{}{}
			dstN, ok := adj.Mapper().Resolve(dstID)
			if !ok {
				continue
			}
			props := g.EdgeProperties(srcN, dstN)
			for key, val := range props {
				ki, ok := idx[key]
				if !ok {
					walkErr = fmt.Errorf("snapshot: edge property key %q not in registry snapshot", key)
					return false
				}
				vb, encErr := encodePropertyValue(val)
				if encErr != nil {
					walkErr = encErr
					return false
				}
				out = append(out, EdgePropertyEntry{
					Src:        uint64(srcID),
					Dst:        uint64(dstID),
					KeyIdx:     ki,
					Kind:       val.Kind(),
					ValueBytes: vb,
				})
			}
		}
		return true
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// buildKeyIndex returns name -> keyTableIndex.
func buildKeyIndex(names []string) map[string]uint32 {
	m := make(map[string]uint32, len(names))
	for i, n := range names {
		m[n] = uint32(i)
	}
	return m
}

// encodePropertyValue returns the on-disk byte payload for v. Layout
// per kind is fixed-width to keep the format human-debuggable.
func encodePropertyValue(v lpg.PropertyValue) ([]byte, error) {
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return []byte(s), nil
	case lpg.PropInt64:
		i, _ := v.Int64()
		out := make([]byte, fixed64ValueSize)
		binary.LittleEndian.PutUint64(out, uint64(i))
		return out, nil
	case lpg.PropFloat64:
		f, _ := v.Float64()
		out := make([]byte, fixed64ValueSize)
		binary.LittleEndian.PutUint64(out, math.Float64bits(f))
		return out, nil
	case lpg.PropBool:
		b, _ := v.Bool()
		if b {
			return []byte{0x01}, nil
		}
		return []byte{0x00}, nil
	case lpg.PropTime:
		t, _ := v.Time()
		out := make([]byte, timeValueSize)
		// We store seconds since the Unix epoch + nanosecond remainder
		// within the second. time.Unix(sec, nsec) reconstitutes the
		// instant exactly; we force .UTC() at decode time to drop the
		// caller's location (snapshots travel between machines).
		binary.LittleEndian.PutUint64(out[0:8], uint64(t.Unix()))
		binary.LittleEndian.PutUint64(out[8:16], uint64(t.Nanosecond()))
		return out, nil
	case lpg.PropBytes:
		b, _ := v.Bytes()
		// Copy so the on-disk payload does not alias the caller-held
		// slice (which the graph might mutate after the snapshot is
		// written).
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	case lpg.PropList:
		return encodeListPropertyValue(v)
	default:
		return nil, fmt.Errorf("snapshot: unknown property kind %d", v.Kind())
	}
}

// encodeListPropertyValue encodes a PropList value as:
//
//	uint32 LE element-count
//	element-count × ( uint8 elem-kind | uint32 elem-valueLen | [elem-valueLen]byte elem-value )
//
// Nested lists are rejected: list elements must not be PropList.
func encodeListPropertyValue(v lpg.PropertyValue) ([]byte, error) {
	elems, _ := v.List()
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(elems)))
	for _, elem := range elems {
		if elem.Kind() == lpg.PropList {
			return nil, fmt.Errorf("snapshot: nested PropList not supported")
		}
		payload, err := encodePropertyValue(elem)
		if err != nil {
			return nil, err
		}
		buf = append(buf, byte(elem.Kind()))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(payload)))
		buf = append(buf, payload...)
	}
	return buf, nil
}

// ReadProperties parses a properties.bin payload produced by
// [WriteProperties]. It performs strict structural validation: a
// missing or wrong magic, a future format-version byte, a truncated
// record, an unknown kind tag, or a key-table index that points
// beyond the embedded string table all surface as
// [ErrPropertiesCorrupted].
//
// The caller is responsible for verifying the surrounding manifest
// CRC matches the file bytes (the [LoadSnapshotFull] helper does
// this); this function only enforces the structural contract.
//
//nolint:gocyclo // properties read: header + key table + node records + edge records, each bounds-checked
func ReadProperties(r io.Reader) (PropertiesReadback, error) {
	defer metrics.Time("store.snapshot.ReadProperties")()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if magic != propertiesMagic {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: bad magic %#x", ErrPropertiesCorrupted, magic)
	}
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if version != propertiesFormatVersion {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: unsupported properties format version %d",
			ErrPropertiesCorrupted, version)
	}

	var keyCount uint64
	if err := binary.Read(br, binary.LittleEndian, &keyCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if keyCount > 1<<30 {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: implausible key count %d",
			ErrPropertiesCorrupted, keyCount)
	}
	keys := make([]string, keyCount)
	for i := uint64(0); i < keyCount; i++ {
		var n uint32
		if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
			metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
			return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
		}
		if n > 1<<20 {
			metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
			return PropertiesReadback{}, fmt.Errorf("%w: implausible key len %d",
				ErrPropertiesCorrupted, n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
			return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
		}
		keys[i] = string(buf)
	}

	var nodeCount uint64
	if err := binary.Read(br, binary.LittleEndian, &nodeCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if nodeCount > 1<<40 {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: implausible node-property count %d",
			ErrPropertiesCorrupted, nodeCount)
	}
	nodes := make([]NodePropertyEntry, nodeCount)
	for i := uint64(0); i < nodeCount; i++ {
		if err := readNodePropRecord(br, &nodes[i], keyCount); err != nil {
			metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
			return PropertiesReadback{}, err
		}
	}

	var edgeCount uint64
	if err := binary.Read(br, binary.LittleEndian, &edgeCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if edgeCount > 1<<40 {
		metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
		return PropertiesReadback{}, fmt.Errorf("%w: implausible edge-property count %d",
			ErrPropertiesCorrupted, edgeCount)
	}
	edges := make([]EdgePropertyEntry, edgeCount)
	for i := uint64(0); i < edgeCount; i++ {
		if err := readEdgePropRecord(br, &edges[i], keyCount); err != nil {
			metrics.IncCounter("store.snapshot.ReadProperties.errors", 1)
			return PropertiesReadback{}, err
		}
	}

	return PropertiesReadback{
		Keys:           keys,
		NodeProperties: nodes,
		EdgeProperties: edges,
	}, nil
}

// readNodePropRecord parses one node property record.
//
//nolint:gocyclo // record read: id + keyIdx + kind + length-prefixed value with bounds checks
func readNodePropRecord(br *bufio.Reader, out *NodePropertyEntry, keyCount uint64) error {
	if err := binary.Read(br, binary.LittleEndian, &out.NodeID); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if err := binary.Read(br, binary.LittleEndian, &out.KeyIdx); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if uint64(out.KeyIdx) >= keyCount {
		return fmt.Errorf("%w: node key idx %d >= %d",
			ErrPropertiesCorrupted, out.KeyIdx, keyCount)
	}
	var kind [1]byte
	if _, err := io.ReadFull(br, kind[:]); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	out.Kind = lpg.PropertyKind(kind[0])
	if !validKind(out.Kind) {
		return fmt.Errorf("%w: node unknown kind %d", ErrPropertiesCorrupted, out.Kind)
	}
	var valLen uint32
	if err := binary.Read(br, binary.LittleEndian, &valLen); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if valLen > maxValueLen {
		return fmt.Errorf("%w: node value len %d > %d", ErrPropertiesCorrupted, valLen, maxValueLen)
	}
	if err := validateFixedLen(out.Kind, valLen); err != nil {
		return err
	}
	if valLen > 0 {
		out.ValueBytes = make([]byte, valLen)
		if _, err := io.ReadFull(br, out.ValueBytes); err != nil {
			return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
		}
	}
	return nil
}

// readEdgePropRecord parses one edge property record.
//
//nolint:gocyclo // record read: src + dst + keyIdx + kind + length-prefixed value with bounds checks
func readEdgePropRecord(br *bufio.Reader, out *EdgePropertyEntry, keyCount uint64) error {
	if err := binary.Read(br, binary.LittleEndian, &out.Src); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if err := binary.Read(br, binary.LittleEndian, &out.Dst); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if err := binary.Read(br, binary.LittleEndian, &out.KeyIdx); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if uint64(out.KeyIdx) >= keyCount {
		return fmt.Errorf("%w: edge key idx %d >= %d",
			ErrPropertiesCorrupted, out.KeyIdx, keyCount)
	}
	var kind [1]byte
	if _, err := io.ReadFull(br, kind[:]); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	out.Kind = lpg.PropertyKind(kind[0])
	if !validKind(out.Kind) {
		return fmt.Errorf("%w: edge unknown kind %d", ErrPropertiesCorrupted, out.Kind)
	}
	var valLen uint32
	if err := binary.Read(br, binary.LittleEndian, &valLen); err != nil {
		return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
	}
	if valLen > maxValueLen {
		return fmt.Errorf("%w: edge value len %d > %d", ErrPropertiesCorrupted, valLen, maxValueLen)
	}
	if err := validateFixedLen(out.Kind, valLen); err != nil {
		return err
	}
	if valLen > 0 {
		out.ValueBytes = make([]byte, valLen)
		if _, err := io.ReadFull(br, out.ValueBytes); err != nil {
			return fmt.Errorf("%w: %w", ErrPropertiesCorrupted, err)
		}
	}
	return nil
}

// validKind reports whether k is one of the defined PropertyKind
// enum values. Anything else surfaces as corruption rather than
// silently degrading.
func validKind(k lpg.PropertyKind) bool {
	switch k {
	case lpg.PropString, lpg.PropInt64, lpg.PropFloat64,
		lpg.PropBool, lpg.PropTime, lpg.PropBytes, lpg.PropList:
		return true
	}
	return false
}

// validateFixedLen enforces that fixed-width kinds (Int64, Float64,
// Bool, Time) carry exactly the expected number of bytes. Variable-
// width kinds (String, Bytes) accept any length up to maxValueLen.
func validateFixedLen(k lpg.PropertyKind, n uint32) error {
	switch k {
	case lpg.PropInt64, lpg.PropFloat64:
		if n != fixed64ValueSize {
			return fmt.Errorf("%w: kind %d expects %d-byte value, got %d",
				ErrPropertiesCorrupted, k, fixed64ValueSize, n)
		}
	case lpg.PropBool:
		if n != boolValueSize {
			return fmt.Errorf("%w: kind %d expects %d-byte value, got %d",
				ErrPropertiesCorrupted, k, boolValueSize, n)
		}
	case lpg.PropTime:
		if n != timeValueSize {
			return fmt.Errorf("%w: kind %d expects %d-byte value, got %d",
				ErrPropertiesCorrupted, k, timeValueSize, n)
		}
	}
	return nil
}

// decodePropertyValue rebuilds a [lpg.PropertyValue] from a record's
// raw value bytes. Length and validity have already been checked by
// the reader, so this function only handles the type-specific
// transcoding.
func decodePropertyValue(kind lpg.PropertyKind, raw []byte) (lpg.PropertyValue, error) {
	switch kind {
	case lpg.PropString:
		return lpg.StringValue(string(raw)), nil
	case lpg.PropInt64:
		if len(raw) != fixed64ValueSize {
			return lpg.PropertyValue{}, fmt.Errorf("%w: int64 value size %d", ErrPropertiesCorrupted, len(raw))
		}
		return lpg.Int64Value(int64(binary.LittleEndian.Uint64(raw))), nil
	case lpg.PropFloat64:
		if len(raw) != fixed64ValueSize {
			return lpg.PropertyValue{}, fmt.Errorf("%w: float64 value size %d", ErrPropertiesCorrupted, len(raw))
		}
		return lpg.Float64Value(math.Float64frombits(binary.LittleEndian.Uint64(raw))), nil
	case lpg.PropBool:
		if len(raw) != boolValueSize {
			return lpg.PropertyValue{}, fmt.Errorf("%w: bool value size %d", ErrPropertiesCorrupted, len(raw))
		}
		return lpg.BoolValue(raw[0] != 0), nil
	case lpg.PropTime:
		if len(raw) != timeValueSize {
			return lpg.PropertyValue{}, fmt.Errorf("%w: time value size %d", ErrPropertiesCorrupted, len(raw))
		}
		sec := int64(binary.LittleEndian.Uint64(raw[0:8]))
		nsec := int64(binary.LittleEndian.Uint64(raw[8:16]))
		return lpg.TimeValue(time.Unix(sec, nsec).UTC()), nil
	case lpg.PropBytes:
		// Allocate a fresh slice so the rebuilt PropertyValue does not
		// alias the readback's storage, which might later be
		// re-used / overwritten by the caller.
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return lpg.BytesValue(cp), nil
	case lpg.PropList:
		return decodeListPropertyValue(raw)
	}
	return lpg.PropertyValue{}, fmt.Errorf("%w: decode unknown kind %d", ErrPropertiesCorrupted, kind)
}

// listElemMinBytes is the smallest number of bytes one PropList element
// can occupy on the wire: a 1-byte kind plus a 4-byte value-length
// prefix (the value itself may be zero bytes). It is the divisor used to
// bound a list capacity hint against the remaining input.
const listElemMinBytes = 5

// listCapHint returns a safe capacity hint for a PropList decode buffer.
// count is the untrusted element count from the wire; remaining is the
// number of bytes left to parse. Because each element consumes at least
// [listElemMinBytes] bytes, no more than remaining/listElemMinBytes
// elements can follow, so the hint is min(count, remaining/listElemMinBytes).
// This prevents a hostile count (up to ~4.3e9) from triggering a
// multi-gigabyte eager reservation while still pre-sizing accurately for
// legitimate lists.
func listCapHint(count uint32, remaining int) int {
	maxElems := remaining / listElemMinBytes
	if int64(count) < int64(maxElems) {
		return int(count)
	}
	return maxElems
}

// decodeListPropertyValue decodes the PropList wire format produced by
// [encodeListPropertyValue]:
//
//	uint32 LE element-count
//	element-count × ( uint8 elem-kind | uint32 elem-valueLen | [elem-valueLen]byte elem-value )
func decodeListPropertyValue(raw []byte) (lpg.PropertyValue, error) {
	if len(raw) < 4 {
		return lpg.PropertyValue{}, fmt.Errorf("%w: PropList: short element count", ErrPropertiesCorrupted)
	}
	count := binary.LittleEndian.Uint32(raw)
	raw = raw[4:]
	// count is an untrusted uint32 (up to ~4.3e9). Each element needs at
	// least listElemMinBytes on the wire, so at most len(raw)/listElemMinBytes
	// elements can actually follow; clamp the capacity hint to that ceiling
	// so a hostile count cannot drive a multi-GB eager reservation. The loop
	// below still validates and bounds every element, so a smaller-than-count
	// capacity only costs a few re-grows for a genuinely large legitimate list.
	elems := make([]lpg.PropertyValue, 0, listCapHint(count, len(raw)))
	for i := uint32(0); i < count; i++ {
		if len(raw) < 5 { // kind(1) + valueLen(4)
			return lpg.PropertyValue{}, fmt.Errorf("%w: PropList: truncated element header at index %d",
				ErrPropertiesCorrupted, i)
		}
		elemKind := lpg.PropertyKind(raw[0])
		elemLen := binary.LittleEndian.Uint32(raw[1:5])
		raw = raw[5:]
		if uint64(len(raw)) < uint64(elemLen) {
			return lpg.PropertyValue{}, fmt.Errorf("%w: PropList: truncated element body at index %d",
				ErrPropertiesCorrupted, i)
		}
		elem, err := decodePropertyValue(elemKind, raw[:elemLen])
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("%w: PropList: element %d: %w",
				ErrPropertiesCorrupted, i, err)
		}
		elems = append(elems, elem)
		raw = raw[elemLen:]
	}
	return lpg.ListValue(elems), nil
}

// ApplyPropertiesToGraph replays rb into a live g. The pre-condition
// is that g's underlying mapper has already been populated with every
// NodeID referenced by rb — typically by replaying the WAL prefix
// covered by the snapshot, or by re-issuing the original AddNode /
// AddEdge calls. Records whose NodeID cannot be resolved by the
// mapper are skipped and counted via the
// `store.snapshot.ApplyProperties.unresolved` metric counter; the
// function does not return an error for them so a partial mapper
// degrades cleanly rather than aborting recovery mid-way.
//
// Edge property records whose endpoints are resolvable but whose
// edge is absent from the adjacency list (e.g., the CSR was not yet
// applied) are likewise skipped and counted under
// `store.snapshot.ApplyProperties.edgeMissing`; this matches
// [lpg.Graph.SetEdgeProperty]'s own no-op-on-missing-edge contract.
//
//nolint:gocyclo // apply: bounds + mapper resolve + edge resolve + kind decode
func ApplyPropertiesToGraph[N comparable, W any](g *lpg.Graph[N, W], rb PropertiesReadback) error {
	defer metrics.Time("store.snapshot.ApplyPropertiesToGraph")()
	adj := g.AdjList()
	for i := range rb.NodeProperties {
		np := &rb.NodeProperties[i]
		if uint64(np.KeyIdx) >= uint64(len(rb.Keys)) {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		n, ok := adj.Mapper().Resolve(graph.NodeID(np.NodeID))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		v, err := decodePropertyValue(np.Kind, np.ValueBytes)
		if err != nil {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		if err := g.SetNodeProperty(n, rb.Keys[np.KeyIdx], v); err != nil {
			metrics.IncCounter("store.snapshot.ApplyProperties.setNodePropertyErrors", 1)
			return fmt.Errorf("snapshot.ApplyPropertiesToGraph: SetNodeProperty: %w", err)
		}
	}
	for i := range rb.EdgeProperties {
		ep := &rb.EdgeProperties[i]
		if uint64(ep.KeyIdx) >= uint64(len(rb.Keys)) {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		srcN, ok := adj.Mapper().Resolve(graph.NodeID(ep.Src))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		dstN, ok := adj.Mapper().Resolve(graph.NodeID(ep.Dst))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		if !adj.HasEdge(srcN, dstN) {
			metrics.IncCounter("store.snapshot.ApplyProperties.edgeMissing", 1)
			continue
		}
		v, err := decodePropertyValue(ep.Kind, ep.ValueBytes)
		if err != nil {
			metrics.IncCounter("store.snapshot.ApplyProperties.unresolved", 1)
			continue
		}
		_ = g.SetEdgeProperty(srcN, dstN, rb.Keys[ep.KeyIdx], v) //nolint:errcheck // no schema validator during snapshot restore
	}
	return nil
}

package snapshot

// edgehandles.bin format (little-endian throughout).
//
//   ['SEHD' magic = 0x44484553]  uint32
//   [format-version = 1]         uint32
//   [labelTableLen]              uint64
//   [labels: labelTableLen × (uint32 utf8Len, [utf8Len]byte)]
//   [keyTableLen]                uint64
//   [keys:   keyTableLen   × (uint32 utf8Len, [utf8Len]byte)]
//   [recordCount]                uint64
//   [records:
//       uint64 src
//       uint64 dst
//       uint64 handle
//       uint32 labelCount
//       [labelCount × uint32 labelIdx]            (index into label table)
//       uint32 propCount
//       [propCount × (uint32 keyIdx, uint8 kind, uint32 valueLen, [valueLen]byte value)]
//   ]
//
// The value bytes per kind are identical to properties.bin (see
// [encodePropertyValue] / [decodePropertyValue]), so the two components share
// one value codec.
//
// # Why this component exists
//
// The CSR component (csr.bin) carries the per-slot stable edge HANDLE column
// (Stage 2 trailing block), which re-stamps each recovered edge with its
// original identity. But the per-CREATE metadata keyed BY that handle — the
// relationship type and properties of one specific parallel edge — has no
// other on-disk home: labels.bin and properties.bin deliberately collapse
// parallel edges onto a single (src, dst) per-pair record. Without this
// component a self-sufficient snapshot (one whose WAL was truncated after the
// checkpoint) would recover the right number of parallel edges with the right
// handles, but every parallel edge between a pair would read back the per-pair
// UNION of types instead of its own type. edgehandles.bin closes that.
//
// # Optionality and backward compatibility
//
// The component is OPTIONAL: the writer emits it only when the graph carries
// at least one per-handle label or property record. A snapshot of a graph
// that never used the handle-keyed metadata stores (e.g. a pure simple graph,
// or one written before this component existed) omits the file entirely and
// loads as an empty readback — the same backward-compatibility contract
// tombstones.bin follows. A reader that predates this component ignores the
// unknown file name (and so would read parallel-edge types from the per-pair
// union); reopening with a current binary restores the per-handle types.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// EdgeHandlesFile is the conventional file name carrying the durable
// per-handle edge metadata (per-CREATE relationship type and properties keyed
// by the stable edge handle) inside a snapshot directory. It is a sibling of
// [CSRFile] and is referenced by an additional [FileEntry] in the manifest
// only when the writer emitted at least one record.
const EdgeHandlesFile = "edgehandles.bin"

// edgeHandlesMagic is the four-byte magic ('S','E','H','D') that prefixes
// every edgehandles.bin file. Stored as a uint32 LE; the bytes appear on disk
// as 'SEHD'.
const edgeHandlesMagic uint32 = 0x44484553

// edgeHandlesFormatVersion is the edgehandles.bin internal format version,
// independent of [ManifestVersion] (the same discipline the sibling
// components follow).
const edgeHandlesFormatVersion uint32 = 1

// edgeHandlesMaxCount bounds a declared count read from a hostile or corrupt
// file, mirroring the implausibility ceilings the sibling readers apply.
const edgeHandlesMaxCount uint64 = 1 << 40

// edgeHandlesCapHintMax caps an eager slice reservation so a hostile count
// cannot drive a multi-gigabyte allocation before the per-record reads have a
// chance to fail on a truncated file.
const edgeHandlesCapHintMax = 1 << 20

// ErrEdgeHandlesCorrupted is returned by [ReadEdgeHandles] when the
// edgehandles.bin file is structurally malformed (bad magic, unsupported
// version, implausible count, label/key index past its table, unknown
// property kind, or a truncated record).
var ErrEdgeHandlesCorrupted = errors.New("snapshot: edgehandles.bin corrupted")

// EdgeHandleRecord is one persisted per-handle edge metadata record: the
// endpoint NodeIDs, the stable handle, the per-CREATE label names, and the
// per-CREATE properties. NodeIDs are stored verbatim (the snapshot mapper is
// restored before this component is applied), matching csr.bin's NodeID
// references.
type EdgeHandleRecord struct {
	Src        uint64
	Dst        uint64
	Handle     uint64
	Labels     []string
	Properties map[string]lpg.PropertyValue
}

// EdgeHandlesReadback is the structural parse of an edgehandles.bin file. The
// caller materialises it back into a live [lpg.Graph] via
// [ApplyEdgeHandlesToGraph] once the underlying mapper is populated.
type EdgeHandlesReadback struct {
	Records []EdgeHandleRecord
}

// edgeHandleRaw is one record collected from the live graph before the string
// tables are built. Labels and property keys are kept as names so the two
// embedded tables can be assembled deterministically.
type edgeHandleRaw struct {
	src, dst, handle uint64
	labels           []string
	propKeys         []string
	propVals         []lpg.PropertyValue
}

// WriteEdgeHandles serialises every per-handle edge label and property
// attached to g into w in the edgehandles.bin format. It returns the number of
// bytes written and the CRC32C of the serialised payload — both stored in the
// manifest's [FileEntry] so [LoadSnapshotFull] can verify integrity at load
// time. It returns (0, 0, nil, false) when the graph carries no per-handle
// metadata, signalling the caller to omit the component entirely.
//
// Records are emitted in the deterministic order [lpg.Graph.WalkEdgeHandles]
// yields (the same source-node order csr.bin / labels.bin use), and within a
// record the label names and property keys are sorted, so the component is
// byte-stable across writes of the same logical state — the cross-process
// byte-equality contract the snapshot relies on.
//
//nolint:gocyclo // edgehandles write: collect + two string tables + per-record labels + per-record props, each guarded
func WriteEdgeHandles[N comparable, W any](w io.Writer, g *lpg.Graph[N, W]) (size int64, crc uint32, emitted bool, err error) {
	defer metrics.Time("store.snapshot.WriteEdgeHandles").Stop()

	raws, labelNames, keyNames := collectEdgeHandleRecords(g)
	if len(raws) == 0 {
		return 0, 0, false, nil
	}
	labelIdx := buildNameIndex(labelNames)
	keyIdx := buildKeyIndex(keyNames)

	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)
	total := int64(0)

	writeU32 := func(v uint32) error { return binary.Write(tee, binary.LittleEndian, v) }
	writeU64 := func(v uint64) error { return binary.Write(tee, binary.LittleEndian, v) }
	writeStrTable := func(names []string) error {
		if err := writeU64(uint64(len(names))); err != nil {
			return err
		}
		total += 8
		for _, n := range names {
			if uint64(len(n)) > uint64(^uint32(0)) {
				return fmt.Errorf("snapshot: edgehandles string too long: %d bytes", len(n))
			}
			if err := writeU32(uint32(len(n))); err != nil {
				return err
			}
			if _, err := tee.Write([]byte(n)); err != nil {
				return err
			}
			total += 4 + int64(len(n))
		}
		return nil
	}

	if err := writeU32(edgeHandlesMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
		return 0, 0, false, err
	}
	if err := writeU32(edgeHandlesFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
		return 0, 0, false, err
	}
	total += 8
	if err := writeStrTable(labelNames); err != nil {
		metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
		return 0, 0, false, err
	}
	if err := writeStrTable(keyNames); err != nil {
		metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
		return 0, 0, false, err
	}
	if err := writeU64(uint64(len(raws))); err != nil {
		metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
		return 0, 0, false, err
	}
	total += 8

	// scratch is the reusable per-record packing buffer (28 bytes covers the
	// largest fixed group), allocated once so records cost no per-field allocs.
	var scratch [28]byte
	for i := range raws {
		n, werr := writeEdgeHandleRecord(tee, scratch[:], &raws[i], labelIdx, keyIdx)
		if werr != nil {
			metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
			return 0, 0, false, werr
		}
		total += n
	}

	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteEdgeHandles.errors", 1)
		return 0, 0, false, err
	}
	return total, hasher.Sum32(), true, nil
}

// writeEdgeHandleRecord serialises one record and returns the byte count.
// scratch must be a caller-owned buffer of at least 28 bytes, reused across
// records so each fixed-width field group is packed with PutUintNN and emitted
// in one Write — byte-identical to the per-field binary.Write it replaces, but
// without that path's per-field reflection/boxing and per-record allocation.
func writeEdgeHandleRecord(w io.Writer, scratch []byte, r *edgeHandleRaw, labelIdx, keyIdx map[string]uint32) (int64, error) {
	total := int64(0)
	// Fixed prefix: src(8) | dst(8) | handle(8) | labelCount(4) = 28 bytes.
	binary.LittleEndian.PutUint64(scratch[0:8], r.src)
	binary.LittleEndian.PutUint64(scratch[8:16], r.dst)
	binary.LittleEndian.PutUint64(scratch[16:24], r.handle)
	binary.LittleEndian.PutUint32(scratch[24:28], uint32(len(r.labels)))
	if _, err := w.Write(scratch[:28]); err != nil {
		return 0, err
	}
	total += 28
	for _, name := range r.labels {
		si, ok := labelIdx[name]
		if !ok {
			return 0, fmt.Errorf("snapshot: edge handle label %q not in table", name)
		}
		binary.LittleEndian.PutUint32(scratch[0:4], si)
		if _, err := w.Write(scratch[:4]); err != nil {
			return 0, err
		}
		total += 4
	}
	binary.LittleEndian.PutUint32(scratch[0:4], uint32(len(r.propKeys)))
	if _, err := w.Write(scratch[:4]); err != nil {
		return 0, err
	}
	total += 4
	for j, key := range r.propKeys {
		ki, ok := keyIdx[key]
		if !ok {
			return 0, fmt.Errorf("snapshot: edge handle property key %q not in table", key)
		}
		valBytes, verr := encodePropertyValue(r.propVals[j])
		if verr != nil {
			return 0, verr
		}
		// Per-property header: keyIdx(4) | kind(1) | valLen(4) = 9 bytes.
		binary.LittleEndian.PutUint32(scratch[0:4], ki)
		scratch[4] = byte(r.propVals[j].Kind())
		binary.LittleEndian.PutUint32(scratch[5:9], uint32(len(valBytes)))
		if _, err := w.Write(scratch[:9]); err != nil {
			return 0, err
		}
		if _, err := w.Write(valBytes); err != nil {
			return 0, err
		}
		total += 4 + 1 + 4 + int64(len(valBytes))
	}
	return total, nil
}

// collectEdgeHandleRecords walks every live durable edge handle and gathers
// the per-handle labels and properties, returning the raw records plus the
// de-duplicated, sorted label-name and property-key tables. A handle with no
// label and no property is skipped (nothing to persist for it). Within each
// record the label names and property keys are sorted for byte-stability.
func collectEdgeHandleRecords[N comparable, W any](g *lpg.Graph[N, W]) (raws []edgeHandleRaw, labelNames, keyNames []string) {
	labelSet := make(map[string]struct{})
	keySet := make(map[string]struct{})
	g.WalkEdgeHandles(func(t lpg.EdgeHandleTriple) bool {
		labels := g.EdgeLabelsByHandleID(t.Src, t.Dst, t.Handle)
		props := g.EdgePropertiesByHandleID(t.Src, t.Dst, t.Handle)
		if len(labels) == 0 && len(props) == 0 {
			return true
		}
		sort.Strings(labels)
		propKeys := make([]string, 0, len(props))
		for k := range props {
			propKeys = append(propKeys, k)
		}
		sort.Strings(propKeys)
		propVals := make([]lpg.PropertyValue, len(propKeys))
		for i, k := range propKeys {
			propVals[i] = props[k]
		}
		for _, l := range labels {
			labelSet[l] = struct{}{}
		}
		for _, k := range propKeys {
			keySet[k] = struct{}{}
		}
		raws = append(raws, edgeHandleRaw{
			src:      uint64(t.Src),
			dst:      uint64(t.Dst),
			handle:   t.Handle,
			labels:   labels,
			propKeys: propKeys,
			propVals: propVals,
		})
		return true
	})
	labelNames = sortedSetKeys(labelSet)
	keyNames = sortedSetKeys(keySet)
	return raws, labelNames, keyNames
}

// sortedSetKeys returns the keys of set in ascending order.
func sortedSetKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadEdgeHandles parses an edgehandles.bin payload produced by
// [WriteEdgeHandles]. It performs strict structural validation: a missing or
// wrong magic, an unsupported version, an implausible count, a label/key index
// past its table, an unknown property kind, or a truncated record all surface
// as [ErrEdgeHandlesCorrupted].
//
//nolint:gocyclo // edgehandles read: header + two string tables + per-record labels + per-record props, each bounds-checked
func ReadEdgeHandles(r io.Reader) (EdgeHandlesReadback, error) {
	defer metrics.Time("store.snapshot.ReadEdgeHandles").Stop()
	br := bufio.NewReader(r)

	var magic, version uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		return failEdgeHandles(err)
	}
	if magic != edgeHandlesMagic {
		return failEdgeHandles(fmt.Errorf("bad magic %#x", magic))
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return failEdgeHandles(err)
	}
	if version != edgeHandlesFormatVersion {
		return failEdgeHandles(fmt.Errorf("unsupported version %d", version))
	}

	labels, err := readEdgeHandleStrTable(br)
	if err != nil {
		return failEdgeHandles(err)
	}
	keys, err := readEdgeHandleStrTable(br)
	if err != nil {
		return failEdgeHandles(err)
	}

	var count uint64
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		return failEdgeHandles(err)
	}
	if count > edgeHandlesMaxCount {
		return failEdgeHandles(fmt.Errorf("implausible record count %d", count))
	}
	hint := count
	if hint > edgeHandlesCapHintMax {
		hint = edgeHandlesCapHintMax
	}
	records := make([]EdgeHandleRecord, 0, hint)
	for i := uint64(0); i < count; i++ {
		rec, rerr := readEdgeHandleRecord(br, labels, keys)
		if rerr != nil {
			return failEdgeHandles(fmt.Errorf("record %d: %w", i, rerr))
		}
		records = append(records, rec)
	}
	return EdgeHandlesReadback{Records: records}, nil
}

// failEdgeHandles wraps err under [ErrEdgeHandlesCorrupted] and bumps the
// error metric, returning the zero readback.
func failEdgeHandles(err error) (EdgeHandlesReadback, error) {
	metrics.IncCounter("store.snapshot.ReadEdgeHandles.errors", 1)
	return EdgeHandlesReadback{}, fmt.Errorf("%w: %w", ErrEdgeHandlesCorrupted, err)
}

// readEdgeHandleStrTable reads a length-prefixed string table. A hostile
// length (up to the 1<<30 ceiling, a ~16 GiB string-header allocation) is
// bounded to edgeHandlesCapHintMax: the count is validated against the
// ceiling first, then the per-string read loop grows via append and fails on
// the first truncated read rather than after a giant make() — mirroring the
// record-loop clamp [ReadEdgeHandles] already applies.
func readEdgeHandleStrTable(br *bufio.Reader) ([]string, error) {
	var n uint64
	if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > 1<<30 {
		return nil, fmt.Errorf("implausible string-table length %d", n)
	}
	out := make([]string, 0, capHint(n, edgeHandlesCapHintMax))
	for i := uint64(0); i < n; i++ {
		var slen uint32
		if err := binary.Read(br, binary.LittleEndian, &slen); err != nil {
			return nil, err
		}
		if slen > 1<<20 {
			return nil, fmt.Errorf("implausible string length %d", slen)
		}
		buf := make([]byte, slen)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		out = append(out, string(buf))
	}
	return out, nil
}

// readEdgeHandleRecord reads one (src, dst, handle, labels, props) record.
func readEdgeHandleRecord(br *bufio.Reader, labels, keys []string) (EdgeHandleRecord, error) {
	var rec EdgeHandleRecord
	if err := binary.Read(br, binary.LittleEndian, &rec.Src); err != nil {
		return rec, err
	}
	if err := binary.Read(br, binary.LittleEndian, &rec.Dst); err != nil {
		return rec, err
	}
	if err := binary.Read(br, binary.LittleEndian, &rec.Handle); err != nil {
		return rec, err
	}
	var labelCount uint32
	if err := binary.Read(br, binary.LittleEndian, &labelCount); err != nil {
		return rec, err
	}
	if uint64(labelCount) > edgeHandlesMaxCount {
		return rec, fmt.Errorf("implausible label count %d", labelCount)
	}
	for j := uint32(0); j < labelCount; j++ {
		var li uint32
		if err := binary.Read(br, binary.LittleEndian, &li); err != nil {
			return rec, err
		}
		if uint64(li) >= uint64(len(labels)) {
			return rec, fmt.Errorf("label idx %d >= %d", li, len(labels))
		}
		rec.Labels = append(rec.Labels, labels[li])
	}
	var propCount uint32
	if err := binary.Read(br, binary.LittleEndian, &propCount); err != nil {
		return rec, err
	}
	if uint64(propCount) > edgeHandlesMaxCount {
		return rec, fmt.Errorf("implausible prop count %d", propCount)
	}
	if propCount > 0 {
		// Clamp the eager map reservation to edgeHandlesCapHintMax exactly as
		// every sibling reader (and this file's own string-table reader at
		// readEdgeHandleStrTable) does: propCount is a u32 read from a possibly
		// hostile/corrupt file and is validated here only against the loose
		// edgeHandlesMaxCount (1<<40) plausibility ceiling. make(map, hint)
		// eagerly pre-allocates hash buckets proportional to the hint, so an
		// unclamped propCount near uint32-max would attempt a multi-gigabyte
		// allocation and OOM the process at recovery before the per-property
		// loop below ever reaches EOF on a truncated body. The loop still grows
		// the map to the true count for a legitimate file (a few extra rehashes
		// beyond the clamp), and a truncated hostile body now fails fast in
		// readEdgeHandleProp (io.ReadFull EOF -> ErrEdgeHandlesCorrupted).
		rec.Properties = make(map[string]lpg.PropertyValue, capHint(uint64(propCount), edgeHandlesCapHintMax))
	}
	for j := uint32(0); j < propCount; j++ {
		key, val, perr := readEdgeHandleProp(br, keys)
		if perr != nil {
			return rec, perr
		}
		rec.Properties[key] = val
	}
	return rec, nil
}

// readEdgeHandleProp reads one (keyIdx, kind, valueLen, value) property tuple.
func readEdgeHandleProp(br *bufio.Reader, keys []string) (string, lpg.PropertyValue, error) {
	var ki uint32
	if err := binary.Read(br, binary.LittleEndian, &ki); err != nil {
		return "", lpg.PropertyValue{}, err
	}
	if uint64(ki) >= uint64(len(keys)) {
		return "", lpg.PropertyValue{}, fmt.Errorf("key idx %d >= %d", ki, len(keys))
	}
	kindByte, err := br.ReadByte()
	if err != nil {
		return "", lpg.PropertyValue{}, err
	}
	kind := lpg.PropertyKind(kindByte)
	if !validKind(kind) {
		return "", lpg.PropertyValue{}, fmt.Errorf("unknown property kind %d", kindByte)
	}
	var valueLen uint32
	if err := binary.Read(br, binary.LittleEndian, &valueLen); err != nil {
		return "", lpg.PropertyValue{}, err
	}
	if valueLen > 1<<30 {
		return "", lpg.PropertyValue{}, fmt.Errorf("implausible value length %d", valueLen)
	}
	raw := make([]byte, valueLen)
	if _, err := io.ReadFull(br, raw); err != nil {
		return "", lpg.PropertyValue{}, err
	}
	val, derr := decodePropertyValue(kind, raw)
	if derr != nil {
		return "", lpg.PropertyValue{}, derr
	}
	return keys[ki], val, nil
}

// ApplyEdgeHandlesToGraph replays rb into a live g, re-attaching every
// per-handle edge label and property keyed by its stable handle and the
// endpoint NodeID pair. It MUST run AFTER the mapper and CSR (with its handle
// column) are applied so the handle the record references is already live on
// the adjacency slot — though the per-handle metadata stores are keyed by
// (NodeID pair, handle) directly and do not require the adjacency edge to be
// present, so a record whose edge the CSR did not materialise is still
// re-attached harmlessly. The handle high-water counter is re-seeded for every
// record so a post-recovery edge creation never re-mints a live handle
// (invariant I5).
func ApplyEdgeHandlesToGraph[N comparable, W any](g *lpg.Graph[N, W], rb EdgeHandlesReadback) {
	defer metrics.Time("store.snapshot.ApplyEdgeHandlesToGraph").Stop()
	for i := range rb.Records {
		rec := &rb.Records[i]
		srcID := graph.NodeID(rec.Src)
		dstID := graph.NodeID(rec.Dst)
		for _, name := range rec.Labels {
			g.SetEdgeLabelByHandleID(srcID, dstID, rec.Handle, name)
		}
		for key, val := range rec.Properties {
			g.SetEdgePropertyByHandleID(srcID, dstID, rec.Handle, key, val)
		}
		g.SeedEdgeHandle(rec.Handle + 1)
	}
}

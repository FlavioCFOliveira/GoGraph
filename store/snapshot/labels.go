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

// LabelsFile is the conventional file name carrying the durable LPG
// label state inside a v2 snapshot directory. It is a sibling of
// [CSRFile] and is referenced by an additional entry in the
// [Manifest.Files] slice.
const LabelsFile = "labels.bin"

// labelsMagic is the four-byte magic ('S','L','B','L') that prefixes
// every labels.bin file. Stored as a uint32 in little-endian; spelled
// out as 0x4C424C53 because the magic bytes appear on disk as 'SLBL'.
const labelsMagic uint32 = 0x4C424C53

// labelsFormatVersion is the labels.bin internal format version. It
// is independent of [ManifestVersion]: a future labels.bin layout
// change bumps this byte without forcing a manifest schema bump.
const labelsFormatVersion uint32 = 1

// ErrLabelsCorrupted is returned by [ReadLabels] when the labels.bin
// file is structurally malformed (bad magic, truncated record, or a
// label-string index that points beyond the embedded string table).
var ErrLabelsCorrupted = errors.New("snapshot: labels.bin corrupted")

// labelsCapHintMax caps an eager slice reservation in [ReadLabels] so a
// hostile count (up to the implausibility ceilings: 1<<30 for the string
// table, 1<<40 for the record arrays) cannot drive a multi-gigabyte
// allocation before the per-record reads have a chance to fail on a
// truncated body. The reader still validates the count against the ceiling
// first and then grows via append, so a header declaring a vast count with
// a short body hits EOF on the first read rather than after a giant make().
// Mirrors tombstones.go's tombstonesCapHintMax and edgehandles.go's
// edgeHandlesCapHintMax.
const labelsCapHintMax = 1 << 20

// NodeLabelEntry pairs a NodeID with the string-table index of one
// label name attached to that node. A node carrying N labels yields
// N entries.
type NodeLabelEntry struct {
	NodeID    uint64
	StringIdx uint32
}

// EdgeLabelEntry pairs an (src, dst) NodeID couple with the
// string-table index of one label name attached to that edge. An
// edge carrying N labels yields N entries; parallel edges between
// the same endpoints fold into the same edgeKey on disk just as they
// do in [lpg.Graph]'s in-memory edgeBag.
type EdgeLabelEntry struct {
	Src       uint64
	Dst       uint64
	StringIdx uint32
}

// LabelsReadback is the structural parse of a labels.bin file. The
// caller materialises it back into a live [lpg.Graph] via
// [ApplyLabelsToGraph] once the underlying mapper is populated.
type LabelsReadback struct {
	Strings    []string
	NodeLabels []NodeLabelEntry
	EdgeLabels []EdgeLabelEntry
}

// WriteLabels serialises every node and edge label attached to g into
// w in the labels.bin format documented at the top of this file. It
// returns the number of bytes written and the CRC32C of the
// serialised payload — both stored in the manifest's [FileEntry] for
// the labels.bin component so [Open] / [LoadSnapshotFull] can verify
// integrity at load time.
//
// The CRC32C covers the entire on-disk file, including the magic
// header. This lets the manifest's CRC field validate every byte of
// labels.bin end-to-end without a separate inner-payload checksum.
//
// The on-disk string table is populated by walking g's
// [lpg.LabelRegistry] in interning order; the labelStringIdx written
// for each (node | edge) record indexes into that table. Because
// LabelID is itself assigned in interning order, this preserves the
// registry's identity across save and load: the reader interns each
// name back in the same order and observes the same LabelID values
// without an extra remap step.
//
// The walk holds the registry's RLock for the duration of the string
// table emission; node/edge enumeration uses the same lock-free /
// RLock-only primitives the public LPG accessors expose.
//
//nolint:gocyclo // labels write: header + string table + node records + edge records, each guarded
func WriteLabels[N comparable, W any](w io.Writer, g *lpg.Graph[N, W]) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteLabels")()

	bw := bufio.NewWriterSize(w, 1<<20)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, labelsMagic); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, labelsFormatVersion); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}

	// Snapshot the label name table in registry order. Walking the
	// registry under its own RLock means a concurrent SetNodeLabel /
	// SetEdgeLabel that adds a brand-new name is serialised against
	// the snapshot writer — the writer either observes the new name
	// (and the matching node/edge entry below) or it does not, but
	// never observes a name with no entry or an entry with no name.
	reg := g.Registry()
	names := snapshotRegistry(reg)
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(names))); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	for _, name := range names {
		if uint64(len(name)) > uint64(^uint32(0)) {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, fmt.Errorf("snapshot: label name too long: %d bytes", len(name))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(name))); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
		if _, err := tee.Write([]byte(name)); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
	}

	// Collect node-label records by walking the underlying mapper:
	// every interned (NodeID, N) pair contributes one record per
	// label attached to N. The mapper Walk holds each shard's RLock
	// only across its own slice — concurrent label mutations on
	// other shards run in parallel.
	nodeRecs, err := collectNodeLabelRecords(g, names)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(nodeRecs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	// scratch is the reusable per-record buffer (20 bytes covers the larger
	// edge record: Src(8) | Dst(8) | StringIdx(4)). Allocated once, it escapes
	// the io.Writer chain a single time rather than per record, so each record
	// is packed with PutUintNN and emitted in one Write with no per-field
	// reflection/boxing — byte-identical to the binary.Write it replaces.
	var scratch [20]byte
	for i := range nodeRecs {
		binary.LittleEndian.PutUint64(scratch[0:8], nodeRecs[i].NodeID)
		binary.LittleEndian.PutUint32(scratch[8:12], nodeRecs[i].StringIdx)
		if _, err := tee.Write(scratch[:12]); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
	}

	edgeRecs, err := collectEdgeLabelRecords(g, names)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(edgeRecs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	for i := range edgeRecs {
		binary.LittleEndian.PutUint64(scratch[0:8], edgeRecs[i].Src)
		binary.LittleEndian.PutUint64(scratch[8:16], edgeRecs[i].Dst)
		binary.LittleEndian.PutUint32(scratch[16:20], edgeRecs[i].StringIdx)
		if _, err := tee.Write(scratch[:20]); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
	}

	if err := bw.Flush(); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}

	// Total bytes: 4 (magic) + 4 (formatVersion) + 8 (stringCount) +
	// for each name: 4 (utf8Len) + utf8Len bytes;
	// + 8 (nodeCount) + nodeCount * (8 + 4);
	// + 8 (edgeCount) + edgeCount * (8 + 8 + 4).
	total := int64(4 + 4 + 8)
	for _, name := range names {
		total += 4 + int64(len(name))
	}
	total += 8 + int64(len(nodeRecs))*int64(8+4)
	total += 8 + int64(len(edgeRecs))*int64(8+8+4)
	return total, hasher.Sum32(), nil
}

// snapshotRegistry returns the label-name table in interning order.
// We rely on [lpg.LabelRegistry.Resolve] which honours the registry's
// own RWMutex; iterating by id from 0 upwards is well-defined
// because LabelID is dense and assigned monotonically by
// [lpg.LabelRegistry.Intern].
func snapshotRegistry(reg *lpg.LabelRegistry) []string {
	out := make([]string, 0, 16)
	for i := uint32(0); ; i++ {
		name, ok := reg.Resolve(lpg.LabelID(i))
		if !ok {
			break
		}
		out = append(out, name)
	}
	return out
}

// collectInternedNodeIDs returns every NodeID currently interned in g's Mapper,
// in Mapper.Walk order. It snapshots the IDs inside [graph.Mapper.Walk] —
// appending only, never re-entering the Mapper — so the bulk label/property
// collectors can resolve their per-node and per-edge state through the lock-free
// NodeID-keyed accessors AFTER Walk has released each shard's read lock.
//
// This is the remedy the Mapper contract itself prescribes (graph/mapper.go:
// 337-345): a callback that re-enters the Mapper (Lookup/Resolve) while holding
// a shard read lock deadlocks against a concurrent writer's queued internSlow
// write lock, because sync.RWMutex admits no new readers once a writer waits.
// The non-blocking checkpoint runs the collectors in its lock-free phase 2 with
// no commit lock and no Graph.View held, so a concurrent committer interning a
// fresh key is exactly such a writer (#1648).
func collectInternedNodeIDs[N comparable, W any](g *lpg.Graph[N, W]) []graph.NodeID {
	ids := make([]graph.NodeID, 0, 64)
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ N) bool {
		ids = append(ids, id)
		return true
	})
	return ids
}

// collectNodeLabelRecords emits one [NodeLabelEntry] per (node, label) pair.
// names is the registry snapshot taken by [snapshotRegistry]; we re-intern each
// label name to translate the LPG's runtime LabelID back into the snapshot's
// string-table index. The two indexes are equal in practice (both follow
// interning order), but the explicit lookup keeps the writer robust against a
// future divergence.
//
// The node IDs are snapshotted inside Mapper.Walk and labels resolved afterwards
// via the lock-free [lpg.Graph.NodeLabelsByID]; resolving inside the Walk
// callback would re-enter the Mapper and deadlock against a concurrent intern
// (#1648 — see [collectInternedNodeIDs]).
func collectNodeLabelRecords[N comparable, W any](
	g *lpg.Graph[N, W],
	names []string,
) ([]NodeLabelEntry, error) {
	idx := buildNameIndex(names)
	out := make([]NodeLabelEntry, 0, 32)
	// Stream each node's labels through ForEachNodeLabelByID instead of the
	// []string that NodeLabelsByID allocates per node; the visit closure is
	// defined once (capturing the stable idx/out plus a per-node curNodeID) so it
	// does not allocate per node either.
	var visitErr error
	var curNodeID uint64
	visit := func(name string) {
		if visitErr != nil {
			return
		}
		si, ok := idx[name]
		if !ok {
			visitErr = fmt.Errorf("snapshot: node label %q not in registry snapshot", name)
			return
		}
		out = append(out, NodeLabelEntry{NodeID: curNodeID, StringIdx: si})
	}
	for _, id := range collectInternedNodeIDs(g) {
		curNodeID = uint64(id)
		g.ForEachNodeLabelByID(id, visit)
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return out, nil
}

// collectEdgeLabelRecords emits one [EdgeLabelEntry] per (src, dst, label)
// triple. Each (src, dst) pair is visited once even when the graph is a
// multigraph: edge labels in v1 are keyed by endpoints only, mirroring the LPG's
// in-memory edgeBag semantics.
//
// Source IDs are snapshotted inside Mapper.Walk; the adjacency
// ([adjlist.AdjList.LoadEntry]) and edge labels ([lpg.Graph.EdgeLabelsByID]) are
// resolved afterwards. Both are lock-free with respect to the Mapper, so this
// never re-enters it from within the Walk callback (#1648 — see
// [collectInternedNodeIDs]).
func collectEdgeLabelRecords[N comparable, W any](
	g *lpg.Graph[N, W],
	names []string,
) ([]EdgeLabelEntry, error) {
	idx := buildNameIndex(names)
	out := make([]EdgeLabelEntry, 0, 32)
	adj := g.AdjList()
	// seen dedups parallel-edge destinations so each (src,dst) pair is emitted
	// once (v1 collapses parallel edges into one edgeBag); it is allocated ONCE
	// and cleared per source. The visit closure streams each pair's distinct
	// labels via ForEachEdgeLabelByID (no per-pair []string) and is defined once,
	// capturing the stable idx/out plus the per-pair curSrc/curDst.
	seen := make(map[graph.NodeID]struct{}, 16)
	var visitErr error
	var curSrc, curDst uint64
	visit := func(name string) {
		if visitErr != nil {
			return
		}
		si, ok := idx[name]
		if !ok {
			visitErr = fmt.Errorf("snapshot: edge label %q not in registry snapshot", name)
			return
		}
		out = append(out, EdgeLabelEntry{Src: curSrc, Dst: curDst, StringIdx: si})
	}
	for _, srcID := range collectInternedNodeIDs(g) {
		neighbours, _ := adj.LoadEntry(srcID)
		if len(neighbours) == 0 {
			continue
		}
		curSrc = uint64(srcID)
		clear(seen)
		for _, dstID := range neighbours {
			if _, dup := seen[dstID]; dup {
				continue
			}
			seen[dstID] = struct{}{}
			curDst = uint64(dstID)
			g.ForEachEdgeLabelByID(srcID, dstID, visit)
			if visitErr != nil {
				return nil, visitErr
			}
		}
	}
	return out, nil
}

// buildNameIndex returns name -> stringTableIndex.
func buildNameIndex(names []string) map[string]uint32 {
	m := make(map[string]uint32, len(names))
	for i, n := range names {
		m[n] = uint32(i)
	}
	return m
}

// ReadLabels parses a labels.bin payload produced by [WriteLabels]. It
// performs strict structural validation: a missing or wrong magic, a
// future format-version byte, a truncated record, or an out-of-range
// string-table index all surface as [ErrLabelsCorrupted].
//
// The caller is responsible for verifying the surrounding manifest
// CRC matches the file bytes (the [Open] / [LoadSnapshotFull]
// helpers do this); this function only enforces the structural
// contract.
//
//nolint:gocyclo // labels read: header + string table + node records + edge records, each bounds-checked
func ReadLabels(r io.Reader) (LabelsReadback, error) {
	defer metrics.Time("store.snapshot.ReadLabels")()
	br := bufio.NewReader(r)

	var magic uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
	}
	if magic != labelsMagic {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: bad magic %#x", ErrLabelsCorrupted, magic)
	}
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
	}
	if version != labelsFormatVersion {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: unsupported labels format version %d",
			ErrLabelsCorrupted, version)
	}

	var stringCount uint64
	if err := binary.Read(br, binary.LittleEndian, &stringCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
	}
	if stringCount > 1<<30 {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: implausible string count %d",
			ErrLabelsCorrupted, stringCount)
	}
	// Clamp the eager reservation: a hostile stringCount (up to 1<<30, a
	// ~16 GiB string-header allocation) is bounded to labelsCapHintMax here;
	// the per-string read loop grows via append, so a truncated body fails on
	// the first ReadFull rather than after a giant make().
	strings := make([]string, 0, capHint(stringCount, labelsCapHintMax))
	for i := uint64(0); i < stringCount; i++ {
		var n uint32
		if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		if n > 1<<20 {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: implausible string len %d",
				ErrLabelsCorrupted, n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		strings = append(strings, string(buf))
	}

	var nodeCount uint64
	if err := binary.Read(br, binary.LittleEndian, &nodeCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
	}
	if nodeCount > 1<<40 {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: implausible node-label count %d",
			ErrLabelsCorrupted, nodeCount)
	}
	// Clamp the eager reservation: a hostile nodeCount (up to 1<<40, a
	// ~16 TiB make()) is bounded to labelsCapHintMax; the per-record read loop
	// grows via append and fails on the first truncated read.
	nodes := make([]NodeLabelEntry, 0, capHint(nodeCount, labelsCapHintMax))
	for i := uint64(0); i < nodeCount; i++ {
		var rec NodeLabelEntry
		if err := binary.Read(br, binary.LittleEndian, &rec.NodeID); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		if err := binary.Read(br, binary.LittleEndian, &rec.StringIdx); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		if uint64(rec.StringIdx) >= stringCount {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: node string idx %d >= %d",
				ErrLabelsCorrupted, rec.StringIdx, stringCount)
		}
		nodes = append(nodes, rec)
	}

	var edgeCount uint64
	if err := binary.Read(br, binary.LittleEndian, &edgeCount); err != nil {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
	}
	if edgeCount > 1<<40 {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: implausible edge-label count %d",
			ErrLabelsCorrupted, edgeCount)
	}
	// Clamp the eager reservation: a hostile edgeCount (up to 1<<40) is bounded
	// to labelsCapHintMax; the per-record read loop grows via append.
	edges := make([]EdgeLabelEntry, 0, capHint(edgeCount, labelsCapHintMax))
	for i := uint64(0); i < edgeCount; i++ {
		var rec EdgeLabelEntry
		if err := binary.Read(br, binary.LittleEndian, &rec.Src); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		if err := binary.Read(br, binary.LittleEndian, &rec.Dst); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		if err := binary.Read(br, binary.LittleEndian, &rec.StringIdx); err != nil {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
		}
		if uint64(rec.StringIdx) >= stringCount {
			metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
			return LabelsReadback{}, fmt.Errorf("%w: edge string idx %d >= %d",
				ErrLabelsCorrupted, rec.StringIdx, stringCount)
		}
		edges = append(edges, rec)
	}

	return LabelsReadback{
		Strings:    strings,
		NodeLabels: nodes,
		EdgeLabels: edges,
	}, nil
}

// ApplyLabelsToGraph replays rb into a live g. The pre-condition is
// that g's underlying mapper has already been populated with every
// NodeID referenced by rb — typically by replaying the WAL prefix
// covered by the snapshot, or by re-issuing the original AddNode /
// AddEdge calls. Records whose NodeID cannot be resolved by the
// mapper are skipped and counted via the
// `store.snapshot.ApplyLabels.unresolved` metric counter; the
// function does not return an error for them so a partial mapper
// degrades cleanly rather than aborting recovery mid-way.
//
// Edge label records whose endpoints are resolvable but whose edge
// is absent from the adjacency list (e.g., the CSR was not yet
// applied) are likewise skipped and counted under
// `store.snapshot.ApplyLabels.edgeMissing`; this matches
// [lpg.Graph.SetEdgeLabel]'s own no-op-on-missing-edge contract.
func ApplyLabelsToGraph[N comparable, W any](g *lpg.Graph[N, W], rb LabelsReadback) error {
	defer metrics.Time("store.snapshot.ApplyLabelsToGraph")()
	adj := g.AdjList()
	for _, nl := range rb.NodeLabels {
		if uint64(nl.StringIdx) >= uint64(len(rb.Strings)) {
			metrics.IncCounter("store.snapshot.ApplyLabels.unresolved", 1)
			continue
		}
		n, ok := adj.Mapper().Resolve(graph.NodeID(nl.NodeID))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyLabels.unresolved", 1)
			continue
		}
		if err := g.SetNodeLabel(n, rb.Strings[nl.StringIdx]); err != nil {
			metrics.IncCounter("store.snapshot.ApplyLabels.setNodeLabelErrors", 1)
			return fmt.Errorf("snapshot.ApplyLabelsToGraph: SetNodeLabel: %w", err)
		}
	}
	for _, el := range rb.EdgeLabels {
		if uint64(el.StringIdx) >= uint64(len(rb.Strings)) {
			metrics.IncCounter("store.snapshot.ApplyLabels.unresolved", 1)
			continue
		}
		srcN, ok := adj.Mapper().Resolve(graph.NodeID(el.Src))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyLabels.unresolved", 1)
			continue
		}
		dstN, ok := adj.Mapper().Resolve(graph.NodeID(el.Dst))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyLabels.unresolved", 1)
			continue
		}
		if !adj.HasEdge(srcN, dstN) {
			metrics.IncCounter("store.snapshot.ApplyLabels.edgeMissing", 1)
			continue
		}
		g.SetEdgeLabel(srcN, dstN, rb.Strings[el.StringIdx])
	}
	return nil
}

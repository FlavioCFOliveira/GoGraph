package snapshot

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// LabelsFile is the conventional file name carrying the durable LPG
// label state inside a v2 snapshot directory. It is a sibling of
// [CSRFile] and is referenced by an additional entry in the
// [Manifest.Files] slice.
const LabelsFile = "labels.bin"

// MetricLabelsSkippedEmptyRegistry counts how many times [WriteLabels] proved,
// from an empty label registry, that no node and no edge carries a label and
// therefore skipped the O(V + E) collection walk (rmp #2271).
//
// It exists because the optimisation is otherwise unobservable: skipping a walk
// whose result was empty changes no output byte and no allocation, so a test
// cannot distinguish "the short-circuit fired" from "the short-circuit is not
// there and the walk happened to find nothing". Without this counter the
// regression test passes against the unfixed code — which is exactly what the
// first draft of that test did.
const MetricLabelsSkippedEmptyRegistry = "store.snapshot.WriteLabels.skippedEmptyRegistry"

// labelsMagic is the four-byte magic ('S','L','B','L') that prefixes
// every labels.bin file. Stored as a uint32 in little-endian; spelled
// out as 0x4C424C53 because the magic bytes appear on disk as 'SLBL'.
const labelsMagic uint32 = 0x4C424C53

// labelsFormatVersion is the labels.bin internal format version the writer
// emits. It is independent of [ManifestVersion]: a labels.bin layout change
// bumps this field without forcing a manifest schema bump.
//
// Version 2 (rmp #2262) widened the edge record with a SLOT ORDINAL, so the
// file can say WHICH parallel slot of a pair carries a relationship type.
// Version 1 keyed an edge-label record by (src, dst) alone; on a multigraph
// pair the parallel slots folded into one record and the per-slot truth was
// not representable. See [labelsFormatVersionPerSlot] for what a v1 file means
// on load.
const labelsFormatVersion uint32 = 2

// labelsFormatVersionPerSlot is the first labels.bin version whose edge records
// carry a slot ordinal. A file at or above it is applied PER SLOT; a file below
// it is applied with the v1 PER-PAIR semantics it was written with.
//
// # Reading an older file: accepted, with its original semantics
//
// A labels.bin already on disk cannot be rewritten retroactively, so rejecting
// v1 outright would turn a Durability fix into a Durability REGRESSION: a store
// that opened before the upgrade would fail to open after it, and committed data
// would become unreachable for no gain — the missing per-slot information is not
// recoverable from the file either way. v1 is therefore read exactly as v1 always
// meant: each record names the PAIR, and [ApplyLabelsToGraph] replays it through
// [lpg.Graph.SetEdgeLabel], which types every free column-typed slot of that
// pair. That reproduces the pre-upgrade load behaviour byte for byte, so
// upgrading changes nothing about an existing snapshot — including the fact that
// a v1 file recorded a multigraph pair lossily. The loss is FROZEN, not repaired:
// the information was never written. The first checkpoint after the upgrade
// emits v2 and the pair becomes durable from then on.
//
// This is the read-the-old-format / write-the-new upgrade path the mature
// engines use (PostgreSQL's catalog version, RocksDB's format_version, Lucene's
// read-the-previous-major policy). A FUTURE version is still rejected: a reader
// cannot invent semantics it does not have.
const labelsFormatVersionPerSlot uint32 = 2

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

// EdgeLabelSlotOverflow is the reserved [EdgeLabelEntry.Slot] value marking a
// record that belongs to the pair's OVERFLOW list rather than to one slot's
// inline label column. A pair's durable type state is exactly those two halves,
// so the file needs to distinguish them; every real ordinal is a slot index and
// so cannot collide with the maximum uint32.
const EdgeLabelSlotOverflow uint32 = ^uint32(0)

// EdgeLabelEntry names ONE relationship type of the directed pair (Src, Dst):
// Slot says WHERE on that pair the type lives, and StringIdx indexes the file's
// string table.
//
// Slot is what makes a multigraph pair representable. A relationship type
// belongs to the relationship INSTANCE, so two parallel edges between the same
// endpoints may carry different types — or one may carry a type and the other
// none. Version 1 had no such field and keyed a record by (Src, Dst) alone, so
// those slots folded together on disk and a checkpoint could silently lose a
// committed type or invent one that was never attached (rmp #2262).
//
// Slot is either:
//
//   - a canonical slot ordinal — the slot's position among the pair's slots after
//     a STABLE sort by stable-edge handle ascending, the order BOTH recovery
//     paths converge on. The record carries that slot's INLINE adjacency label
//     column entry. [lpg.Graph.ForEachPairSlotRelTypeByID] produces it on the
//     write side and [lpg.Graph.SetEdgeRelTypeAtSlotByID] resolves it on the
//     apply side; or
//   - [EdgeLabelSlotOverflow], meaning the type is in the pair's overflow list —
//     a type that could not be placed in any slot's column and that every
//     column-typed slot of the pair therefore carries.
//
// Slot is meaningless in a version-1 readback and is left zero there.
//
// A slot's inline entry is recorded whether or not a by-handle type record also
// covers it. The by-handle store has its own durable component
// (edgehandles.bin) and stays authoritative for what such a slot IS, but the
// column is what [lpg.Graph.EdgeLabels] and
// [lpg.Graph.RelationshipTypesInUse] read, so omitting it here would leave every
// Cypher-created relationship's type absent from those answers after a restart.
type EdgeLabelEntry struct {
	Src       uint64
	Dst       uint64
	Slot      uint32
	StringIdx uint32
}

// LabelsReadback is the structural parse of a labels.bin file. The
// caller materialises it back into a live [lpg.Graph] via
// [ApplyLabelsToGraph] once the underlying mapper is populated.
type LabelsReadback struct {
	Strings    []string
	NodeLabels []NodeLabelEntry
	EdgeLabels []EdgeLabelEntry
	// Version is the on-disk labels.bin format version this readback was parsed
	// from. It selects how [ApplyLabelsToGraph] replays EdgeLabels: at or above
	// [labelsFormatVersionPerSlot] each record names one SLOT, below it each
	// record names the PAIR. A hand-constructed readback leaves it zero and is
	// therefore replayed with the conservative per-pair semantics.
	Version uint32
}

// maxRegistryCaptureRetries bounds how many times [WriteLabels] /
// [WriteProperties] re-snapshot the label / property-key registry when a
// concurrent commit interns a brand-new name between the registry snapshot and
// the lock-free node/edge walk (#1880). Because the registries are monotonic
// and append-only, each re-snapshot is guaranteed to include any name the walk
// observed, so this self-heals — in the steady state (a bounded schema) the
// first attempt already succeeds. Only sustained, adversarial schema churn
// during a single checkpoint could exhaust the budget, in which case the caller
// falls back to the prior fail-stop (abort the checkpoint attempt, retain the
// WAL, retry on the next tick) — never a correctness or durability regression.
const maxRegistryCaptureRetries = 8

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
// [lpg.LabelRegistry] is a lock-free, copy-on-write structure (see its own
// doc): there is no RLock held across the string-table emission and the
// later node/edge enumeration, which read the registry and the live
// graph independently, at different times, via the same lock-free /
// per-shard-RLock-only primitives the public LPG accessors expose. A
// name interned strictly between those two reads and immediately
// attached to a node/edge is therefore visible to the enumeration but
// absent from the already-captured string table: collectNodeLabelRecords
// / collectEdgeLabelRecords detect this and return a "not in registry
// snapshot" error. Rather than abort the whole checkpoint attempt on that
// race, WriteLabels re-captures and retries as one consistent unit
// (self-healing, bounded by [maxRegistryCaptureRetries]); because the
// registry is monotonic and append-only, a re-capture is guaranteed to
// include the newly-interned name, so the retry converges — in the steady
// state on the first attempt (#1880). Only sustained, adversarial schema
// churn that races every attempt exhausts the budget, in which case
// WriteLabels falls back to the prior fail-stop (return the error with
// nothing written or truncated — the identical fail-safe posture
// [Checkpointer.truncatePrefixLocked] uses for a schema DDL racing phase 2,
// #1774), never a Consistency violation.
func WriteLabels[N comparable, W any](w io.Writer, g *lpg.Graph[N, W], at *lpg.Snapshot) (size int64, crc uint32, err error) {
	return writeLabels(w, g, at, snapshotRegistry)
}

// writeLabels is the seam-injected body of [WriteLabels]. snapReg captures the
// label-name table at the top of every self-heal attempt; production passes the
// real [snapshotRegistry] and the retry-loop wiring tests substitute a per-call
// closure that interns a fresh, node-attached label to force a re-capture
// deterministically (#1890). snapReg is an ordinary per-call parameter — there
// is no package-level mutable state, so writeLabels stays exactly as re-entrant
// and safe for concurrent independent calls as the code it replaced.
//
//nolint:gocyclo // labels write: header + string table + node records + edge records, each guarded
func writeLabels[N comparable, W any](w io.Writer, g *lpg.Graph[N, W], at *lpg.Snapshot, snapReg func(*lpg.LabelRegistry) []string) (size int64, crc uint32, err error) {
	defer metrics.Time("store.snapshot.WriteLabels").Stop()

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

	// Capture the label name table and collect the node/edge label records as
	// ONE consistent unit BEFORE writing any of them. The checkpoint's phase 2
	// is lock-free, so a concurrent commit may intern a brand-new label between
	// the registry snapshot and the node/edge walk, making that name visible to
	// the walk but absent from the snapshot; the collectors detect this and
	// return an error. Because the registry is monotonic and append-only, a
	// re-snapshot after such a race is guaranteed to include the new name, so a
	// bounded retry self-heals (typically on the first retry). Exhausting the
	// budget under sustained adversarial churn falls back to the prior
	// fail-stop (return the error → the checkpoint attempt aborts, the WAL is
	// retained, the next tick retries) — no correctness or durability
	// regression. This replaces the earlier behaviour where a single such race
	// aborted the whole checkpoint attempt (#1880).
	reg := g.Registry()
	var names []string
	var nodeRecs []NodeLabelEntry
	var edgeRecs []EdgeLabelEntry
	for attempt := 0; ; attempt++ {
		names = snapReg(reg)
		// EMPTY-REGISTRY SHORT-CIRCUIT (rmp #2271). Both collectors below are
		// O(V), and O(V + E) for the edge one, and this whole function runs
		// inside the checkpoint's exclusion window, where every millisecond
		// stalls every writer. When the label registry holds no names, that walk
		// is guaranteed to find nothing.
		//
		// The guarantee is one-directional and that is what makes it sound. A
		// label can only be attached through the registry, which is append-only
		// and never reassigns an id, so an EMPTY name table proves no node and
		// no edge carries a label. The converse does not hold — a name stays
		// interned after the last label using it is removed — so a non-empty
		// table still pays for the full walk, which is correct: it may find
		// records, and the cost then buys an answer.
		//
		// Measured on 200 000 nodes and 100 000 edges carrying no labels:
		// 11.886 ms before, and the walk is skipped entirely after.
		if len(names) == 0 {
			nodeRecs, edgeRecs = nil, nil
			// Engagement counter, not decoration. The skip is INVISIBLE in the
			// output — the bytes are identical either way, which is the point —
			// and it is invisible in allocations too, because the walk it
			// removes was already allocation-free. A first version of the
			// regression test asserted allocations and passed against the
			// unfixed code. This counter is what makes the skip observable, so
			// the gate can tell "fast because it skipped" from "unchanged
			// because it never fired".
			metrics.IncCounter(MetricLabelsSkippedEmptyRegistry, 1)
			break
		}
		var cerr error
		nodeRecs, cerr = collectNodeLabelRecords(g, at, names)
		if cerr == nil {
			edgeRecs, cerr = collectEdgeLabelRecords(g, at, names)
		}
		if cerr == nil {
			break
		}
		if attempt >= maxRegistryCaptureRetries {
			metrics.IncCounter("store.snapshot.WriteLabels.captureRetryExhausted", 1)
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, cerr
		}
		metrics.IncCounter("store.snapshot.WriteLabels.captureRetry", 1)
	}

	// Emit the now-consistent name table.
	if err := binary.Write(tee, binary.LittleEndian, uint64(len(names))); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	for _, name := range names {
		if uint64(len(name)) > uint64(^uint32(0)) {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, fmt.Errorf("snapshot: label name too long: %d bytes", len(name))
		}
		//nolint:gosec // G115: bounded by the len(name) > MaxUint32 fail-stop at labels.go:311, which aborts WriteLabels before the prefix is emitted
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(name))); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
		if _, err := tee.Write([]byte(name)); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
	}

	if err := binary.Write(tee, binary.LittleEndian, uint64(len(nodeRecs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	// scratch is the reusable per-record buffer (24 bytes covers the larger
	// edge record: Src(8) | Dst(8) | Slot(4) | StringIdx(4)). Allocated once, it
	// escapes the io.Writer chain a single time rather than per record, so each
	// record is packed with PutUintNN and emitted in one Write with no per-field
	// reflection/boxing — byte-identical to the binary.Write it replaces.
	var scratch [24]byte
	for i := range nodeRecs {
		binary.LittleEndian.PutUint64(scratch[0:8], nodeRecs[i].NodeID)
		binary.LittleEndian.PutUint32(scratch[8:12], nodeRecs[i].StringIdx)
		if _, err := tee.Write(scratch[:12]); err != nil {
			metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
			return 0, 0, err
		}
	}

	if err := binary.Write(tee, binary.LittleEndian, uint64(len(edgeRecs))); err != nil {
		metrics.IncCounter("store.snapshot.WriteLabels.errors", 1)
		return 0, 0, err
	}
	for i := range edgeRecs {
		binary.LittleEndian.PutUint64(scratch[0:8], edgeRecs[i].Src)
		binary.LittleEndian.PutUint64(scratch[8:16], edgeRecs[i].Dst)
		binary.LittleEndian.PutUint32(scratch[16:20], edgeRecs[i].Slot)
		binary.LittleEndian.PutUint32(scratch[20:24], edgeRecs[i].StringIdx)
		if _, err := tee.Write(scratch[:24]); err != nil {
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
	// + 8 (edgeCount) + edgeCount * (8 + 8 + 4 + 4).
	total := int64(4 + 4 + 8)
	for _, name := range names {
		total += 4 + int64(len(name))
	}
	total += 8 + int64(len(nodeRecs))*int64(8+4)
	total += 8 + int64(len(edgeRecs))*int64(8+8+4+4)
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

// distinctDestinationsSorted returns one source's DISTINCT out-neighbours in
// ascending NodeID order, reusing seen for the dedup and buf for the result so
// the per-source cost stays allocation-free after the first grow.
//
// The ascending order is what makes the emitted records CANONICAL — a pure
// function of the graph's content rather than of the order its edges were
// inserted. That matters for byte stability across a snapshot round-trip:
// [ApplyCSRToGraph] replays edges in csr.bin order, which since rmp #2141 is
// destination-ordered rather than insertion-ordered, so a recovered graph's
// adjacency insertion order legitimately differs from the original's. Walking
// the adjacency directly would then emit the same records in a permuted order
// and drift the component bytes, which
// TestRecovery_V3Snapshot_RoundTripByteStable asserts must not happen. Sorting
// here removes the dependence on mutation history entirely, so the property
// holds for any future change to insertion or replay order too.
//
// Parallel edges collapse to one entry per (src, dst). Edge PROPERTIES are keyed
// by endpoints only, so that is all they need. Edge LABELS are per-slot from
// labels.bin v2 on (rmp #2262), but the extra dimension lives INSIDE the pair:
// [collectEdgeLabelRecords] visits each pair once here and enumerates that pair's
// slots by canonical ordinal within the visit, so the destination order this
// function fixes still determines the record order.
func distinctDestinationsSorted(
	neighbours []graph.NodeID,
	seen map[graph.NodeID]struct{},
	buf []graph.NodeID,
) []graph.NodeID {
	clear(seen)
	buf = buf[:0]
	for _, dstID := range neighbours {
		if _, dup := seen[dstID]; dup {
			continue
		}
		seen[dstID] = struct{}{}
		buf = append(buf, dstID)
	}
	// Values are distinct after the dedup above, so an unstable sort is total.
	slices.Sort(buf)
	return buf
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
	at *lpg.Snapshot,
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
		g.ForEachNodeLabelByIDAsOf(id, at, visit)
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return out, nil
}

// collectEdgeLabelRecords emits the durable type state of every distinct
// (src, dst) pair: one [EdgeLabelEntry] per typed SLOT, carrying that slot's
// inline adjacency-label-column entry under its canonical ordinal, followed by
// one entry per type in the pair's OVERFLOW list, marked
// [EdgeLabelSlotOverflow].
//
// Those two halves are the whole of a pair's type state, so recording them
// verbatim is lossless in both directions. Emitting the pair's derived UNION
// instead — the v1 behaviour — cannot express a multigraph pair whose parallel
// slots disagree: replaying it re-typed every slot of the pair, so a checkpoint
// changed the answer to a typed-degree query (rmp #2262).
//
// A slot covered by a by-handle type record still contributes its column entry.
// edgehandles.bin is authoritative for what that slot IS, but the column is what
// the pair-level readers consult, and a Cypher CREATE writes both.
//
// The destination order stays [distinctDestinationsSorted]'s ascending NodeID
// order, the slot order is the pair's canonical ordinal order, and the overflow
// order is the list's own, so the emitted record sequence remains a pure
// function of the graph's content — the property
// TestRecovery_V3Snapshot_RoundTripByteStable pins.
//
// Source IDs are snapshotted inside Mapper.Walk; the adjacency
// ([adjlist.AdjList.LoadEntry]) and the type state
// ([lpg.Graph.ForEachPairSlotRelTypeByID] /
// [lpg.Graph.ForEachPairOverflowRelTypeByID]) are resolved afterwards. All are
// lock-free with respect to the Mapper, so this never re-enters it from within
// the Walk callback (#1648 — see [collectInternedNodeIDs]).
func collectEdgeLabelRecords[N comparable, W any](
	g *lpg.Graph[N, W],
	at *lpg.Snapshot,
	names []string,
) ([]EdgeLabelEntry, error) {
	idx := buildNameIndex(names)
	out := make([]EdgeLabelEntry, 0, 32)
	adj := g.AdjList()
	// seen dedups the destination list so each (src, dst) pair is visited once —
	// the pair's parallel slots are then enumerated within that single visit. It
	// is allocated ONCE and cleared per source. The visit closures are defined
	// once too, capturing the stable idx/out plus the per-pair curSrc/curDst.
	seen := make(map[graph.NodeID]struct{}, 16)
	var dsts []graph.NodeID
	var visitErr error
	var curSrc, curDst uint64
	emit := func(slot uint32, name string) {
		if visitErr != nil {
			return
		}
		si, ok := idx[name]
		if !ok {
			visitErr = fmt.Errorf("snapshot: edge label %q not in registry snapshot", name)
			return
		}
		out = append(out, EdgeLabelEntry{
			Src:       curSrc,
			Dst:       curDst,
			Slot:      slot,
			StringIdx: si,
		})
	}
	visitSlot := func(ordinal int, name string) {
		if visitErr != nil {
			return
		}
		if ordinal < 0 || uint64(ordinal) >= uint64(EdgeLabelSlotOverflow) {
			// An ordinal at or past the overflow sentinel cannot be encoded. A
			// pair would need 4 billion parallel edges to reach it, so this is a
			// fail-stop on the impossible rather than a real branch.
			visitErr = fmt.Errorf("snapshot: edge label slot ordinal %d out of range", ordinal)
			return
		}
		emit(uint32(ordinal), name)
	}
	visitOverflow := func(name string) { emit(EdgeLabelSlotOverflow, name) }
	for _, srcID := range collectInternedNodeIDs(g) {
		neighbours, _ := adj.LoadEntry(srcID)
		if len(neighbours) == 0 {
			continue
		}
		curSrc = uint64(srcID)
		dsts = distinctDestinationsSorted(neighbours, seen, dsts)
		for _, dstID := range dsts {
			curDst = uint64(dstID)
			g.ForEachPairSlotRelTypeByIDAsOf(srcID, dstID, at, visitSlot)
			if visitErr != nil {
				return nil, visitErr
			}
			g.ForEachPairOverflowRelTypeByIDAsOf(srcID, dstID, at, visitOverflow)
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
	defer metrics.Time("store.snapshot.ReadLabels").Stop()
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
	// Accept every version this build knows how to interpret, not just the one it
	// writes: a labels.bin already on disk cannot be rewritten retroactively, so
	// rejecting the previous version would make committed data unreachable after
	// an upgrade. A version 1 file is parsed with the narrower edge record and
	// applied with the PER-PAIR semantics it was written with; see
	// [labelsFormatVersionPerSlot]. A version this build does not know is still
	// rejected — a reader cannot invent semantics it does not have.
	if version == 0 || version > labelsFormatVersion {
		metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
		return LabelsReadback{}, fmt.Errorf("%w: unsupported labels format version %d",
			ErrLabelsCorrupted, version)
	}
	perSlot := version >= labelsFormatVersionPerSlot
	if !perSlot {
		metrics.IncCounter("store.snapshot.ReadLabels.legacyPerPair", 1)
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
		// The slot ordinal exists only from version 2 on; a version 1 record goes
		// straight from Dst to StringIdx and leaves Slot at its zero value, which
		// the per-pair apply path ignores.
		if perSlot {
			if err := binary.Read(br, binary.LittleEndian, &rec.Slot); err != nil {
				metrics.IncCounter("store.snapshot.ReadLabels.errors", 1)
				return LabelsReadback{}, fmt.Errorf("%w: %w", ErrLabelsCorrupted, err)
			}
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
		// Slot is deliberately NOT range-checked here: the file cannot know how
		// many slots the pair will have once the adjacency is replayed, and the
		// number legitimately differs when the snapshot is applied onto a graph a
		// WAL tail has since changed. ApplyLabelsToGraph resolves it against the
		// live adjacency and skips (with a metric) what it cannot place, so a
		// hostile ordinal costs one failed lookup and never an out-of-range index.
		edges = append(edges, rec)
	}

	return LabelsReadback{
		Strings:    strings,
		NodeLabels: nodes,
		EdgeLabels: edges,
		Version:    version,
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
//
// # Per-slot versus per-pair replay
//
// From labels.bin version 2 ([labelsFormatVersionPerSlot]) each edge record
// names ONE place on the pair. A record carrying a canonical ordinal is replayed
// through [lpg.Graph.SetEdgeRelTypeAtSlotByID], so a multigraph pair's parallel
// slots keep the types they actually carried; a record marked
// [EdgeLabelSlotOverflow] is replayed through
// [lpg.Graph.AddEdgeRelTypeOverflowByID], restoring the pair-wide half of its
// type state. A record whose ordinal the live adjacency cannot resolve — the
// pair now holds fewer slots than the snapshot recorded — is skipped and counted
// under `store.snapshot.ApplyLabels.slotMissing` rather than being retargeted at
// some other slot, because guessing would be the very failure this format
// removes.
//
// A version-1 readback (and any hand-constructed one, which leaves Version at
// zero) is replayed through [lpg.Graph.SetEdgeLabel], which names the PAIR. That
// is exactly what those records meant when they were written; see
// [labelsFormatVersionPerSlot] for why an older file is read rather than
// rejected.
//
//nolint:gocritic // hugeParam: rb is passed by value intentionally; ApplyLabelsToGraph is exported and the by-value readback is its stable contract. Adding the Version field (rmp #2262) pushed LabelsReadback from 72 to 80 bytes, exactly gocritic's threshold; the struct is three slice headers plus a word, it is copied twice per recovery, and switching the exported signature to a pointer would be an unrelated breaking API change.
func ApplyLabelsToGraph[N comparable, W any](g *lpg.Graph[N, W], rb LabelsReadback) error {
	defer metrics.Time("store.snapshot.ApplyLabelsToGraph").Stop()
	adj := g.AdjList()
	// Re-intern the WHOLE string table, in table order, before applying any
	// record. The table is the write-time [lpg.LabelRegistry] snapshot taken in
	// interning order, so replaying it in order restores the same LabelID for
	// every name — which is what [WriteLabels] documents and what makes a
	// snapshot's identity survive the round trip.
	//
	// Interning only the names the records happen to reference (the previous
	// behaviour) made the recovered registry's order depend on which records
	// exist and in what order they are applied, so a graph whose types are
	// recorded against handles rather than in the column came back with a
	// different registry order and re-checkpointed to different bytes. A name the
	// recovered graph does not attach to anything is inert: every "in use" answer
	// ([lpg.Graph.RelationshipTypesInUse], [lpg.Graph.NodeLabelsInUse]) is derived
	// from actual attachments, never from the registry.
	reg := g.Registry()
	for _, name := range rb.Strings {
		reg.Intern(name)
	}
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
	perSlot := rb.Version >= labelsFormatVersionPerSlot
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
		if !perSlot {
			g.SetEdgeLabel(srcN, dstN, rb.Strings[el.StringIdx])
			continue
		}
		if el.Slot == EdgeLabelSlotOverflow {
			g.AddEdgeRelTypeOverflowByID(
				graph.NodeID(el.Src), graph.NodeID(el.Dst), rb.Strings[el.StringIdx])
			continue
		}
		if !g.SetEdgeRelTypeAtSlotByID(
			graph.NodeID(el.Src), graph.NodeID(el.Dst),
			int(el.Slot), rb.Strings[el.StringIdx],
		) {
			metrics.IncCounter("store.snapshot.ApplyLabels.slotMissing", 1)
		}
	}
	return nil
}

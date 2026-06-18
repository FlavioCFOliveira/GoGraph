// Package label provides a Roaring-bitmap-backed inverted index from
// label identifiers to the NodeIDs that carry them.
//
// The index is the substrate for label-filtered queries such as
// "find every node with label Person and label Active": each label
// is represented by a 64-bit Roaring bitmap, and compound queries
// are answered via bitmap intersection / union, which Roaring
// implements with run-length and array-bitmap hybrids.
//
// Index is safe for concurrent use.
package label

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// Scope tags whether the index observes node-label or edge-label
// changes when registered with [index.Manager]. The two scopes share
// a common bitmap shape so the on-disk format is identical.
type Scope uint8

// Scope values for [NewNodeIndex] / [NewEdgeIndex].
const (
	// ScopeNode listens for [index.OpAddNodeLabel] / [index.OpRemoveNodeLabel]
	// when the index is registered with a [index.Manager]. It is the
	// default; callers building an unregistered index can ignore the
	// scope entirely.
	ScopeNode Scope = iota + 1
	// ScopeEdge listens for [index.OpAddEdgeLabel] / [index.OpRemoveEdgeLabel].
	// Edge bitmaps are keyed by the source NodeID, mirroring the LPG
	// convention exposed by [lpg.Graph.EdgeIndex].
	ScopeEdge
)

// Index maps label identifiers (uint32) to the set of NodeIDs that
// carry them. Different LabelID namespaces (vertices, edges) should
// use distinct Index instances.
//
// Each label's node set is held as an [index.NodeSet]: a sparse label
// carried by one or a handful of nodes stays in the inline small-set
// tier (no per-label roaring overhead), while a dense label — one built
// via [Index.AddRange] over a contiguous NodeID band, or grown past the
// small-set threshold — is a [roaring64.Bitmap] with its run-container
// optimality intact. Promotion to the bitmap tier is one-way, so a dense
// label can never be mis-tiered as a small set (sprint 206, #1585).
type Index struct {
	mu    sync.RWMutex
	bits  map[uint32]index.NodeSet
	scope Scope
}

// NewIndex returns an empty index in [ScopeNode] — equivalent to
// [NewNodeIndex]. Existing callers that pre-date the scope field
// keep this constructor as the default.
func NewIndex() *Index {
	return NewNodeIndex()
}

// NewNodeIndex returns an empty index that listens for node-label
// changes when registered with a [index.Manager].
func NewNodeIndex() *Index {
	return &Index{bits: make(map[uint32]index.NodeSet), scope: ScopeNode}
}

// NewEdgeIndex returns an empty index that listens for edge-label
// changes when registered with a [index.Manager].
func NewEdgeIndex() *Index {
	return &Index{bits: make(map[uint32]index.NodeSet), scope: ScopeEdge}
}

// Scope reports which label-event kind the index observes via
// [Index.Apply].
func (i *Index) Scope() Scope { return i.scope }

// Add records that node carries label.
func (i *Index) Add(label uint32, node graph.NodeID) {
	i.mu.Lock()
	// NodeSet is stored by value; read-modify-write so an inline-tier
	// mutation is recorded. A bitmap-tier Add mutates the shared bitmap in
	// place, so the store-back is a harmless no-op in that state.
	set := i.bits[label]
	set.Add(uint64(node))
	i.bits[label] = set
	i.mu.Unlock()
}

// Remove records that node no longer carries label. No-op if absent.
func (i *Index) Remove(label uint32, node graph.NodeID) {
	i.mu.Lock()
	if set, ok := i.bits[label]; ok {
		if set.Remove(uint64(node)) {
			delete(i.bits, label)
		} else {
			i.bits[label] = set
		}
	}
	i.mu.Unlock()
}

// AddRange records that all nodes in [fromNode, toNode] (inclusive) carry
// label. It uses [roaring64.Bitmap.AddRange] which represents dense ranges in
// O(1) space, making bulk ingestion of contiguous NodeID bands efficient.
func (i *Index) AddRange(label uint32, fromNode, toNode graph.NodeID) {
	i.mu.Lock()
	// AddRange promotes the label's NodeSet to (or keeps it on) the roaring
	// bitmap tier and uses run-container AddRange, so a contiguous band is
	// stored in O(1) space — the dense-label fast path. Promotion is
	// one-way, so a dense label stays optimal.
	set := i.bits[label]
	set.AddRange(uint64(fromNode), uint64(toNode))
	i.bits[label] = set
	i.mu.Unlock()
}

// RemoveRange records that all nodes in [fromNode, toNode] (inclusive) no
// longer carry label. Empty bitmaps are deleted so the map does not grow
// unboundedly after bulk-remove operations.
func (i *Index) RemoveRange(label uint32, fromNode, toNode graph.NodeID) {
	i.mu.Lock()
	if set, ok := i.bits[label]; ok {
		if set.RemoveRange(uint64(fromNode), uint64(toNode)) {
			delete(i.bits, label)
		} else {
			i.bits[label] = set
		}
	}
	i.mu.Unlock()
}

// Scan returns the sorted slice of NodeIDs that carry label.
// Returns nil when label has no entries.
func (i *Index) Scan(label uint32) []graph.NodeID {
	i.mu.RLock()
	set, ok := i.bits[label]
	if !ok {
		i.mu.RUnlock()
		return nil
	}
	raw := set.ToArray()
	i.mu.RUnlock()
	if len(raw) == 0 {
		return nil
	}
	out := make([]graph.NodeID, len(raw))
	for j, v := range raw {
		out[j] = graph.NodeID(v)
	}
	return out
}

// Count returns the number of NodeIDs that carry label.
func (i *Index) Count(label uint32) uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if set, ok := i.bits[label]; ok {
		return set.Cardinality()
	}
	return 0
}

// Has reports whether node carries label.
func (i *Index) Has(label uint32, node graph.NodeID) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	set, ok := i.bits[label]
	if !ok {
		return false
	}
	return set.Contains(uint64(node))
}

// Intersect returns a fresh Roaring bitmap containing the NodeIDs
// that carry every supplied label. Calling with no labels returns
// the empty bitmap.
func (i *Index) Intersect(labels ...uint32) *roaring64.Bitmap {
	if len(labels) == 0 {
		return roaring64.New()
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	first, ok := i.bits[labels[0]]
	if !ok {
		return roaring64.New()
	}
	// Materialise a fresh, caller-owned bitmap from the first label's set
	// (clone when it aliases the live bitmap) and intersect the rest into
	// it via OrInto-backed bitmaps.
	result, shared := first.Bitmap()
	if shared {
		result = result.Clone()
	}
	for _, l := range labels[1:] {
		set, ok := i.bits[l]
		if !ok {
			return roaring64.New()
		}
		other, _ := set.Bitmap()
		result.And(other)
		if result.IsEmpty() {
			return result
		}
	}
	return result
}

// Union returns a fresh Roaring bitmap containing the NodeIDs that
// carry any of the supplied labels.
func (i *Index) Union(labels ...uint32) *roaring64.Bitmap {
	result := roaring64.New()
	if len(labels) == 0 {
		return result
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, l := range labels {
		if set, ok := i.bits[l]; ok {
			set.OrInto(result)
		}
	}
	return result
}

// Kind returns "label" — satisfies [index.Subscriber].
func (*Index) Kind() string { return "label" }

// Apply dispatches the change to the underlying bitmaps when the
// change kind matches the index's [Scope]. Other ops are ignored
// (the manager fans every change to every subscriber; per-subscriber
// filtering is the subscriber's responsibility).
func (i *Index) Apply(c index.Change) {
	switch c.Op {
	case index.OpAddNodeLabel:
		if i.scope == ScopeNode {
			i.Add(c.Label, c.Node)
		}
	case index.OpRemoveNodeLabel:
		if i.scope == ScopeNode {
			i.Remove(c.Label, c.Node)
		}
	case index.OpAddEdgeLabel:
		if i.scope == ScopeEdge {
			i.Add(c.Label, c.Node)
		}
	case index.OpRemoveEdgeLabel:
		if i.scope == ScopeEdge {
			i.Remove(c.Label, c.Node)
		}
	}
}

// labelMagic is the four-byte magic at the head of a serialised
// label index ('SLBI' little-endian — 0x49424C53).
const labelMagic uint32 = 0x49424C53

// labelFormatVersion is the on-disk format version of a serialised
// label index.
const labelFormatVersion uint32 = 1

// labelCapHintMax caps the eager map size hint in Deserialize so a hostile
// label count cannot drive a large pre-allocation before any entry is read.
// It mirrors the safe sibling ceiling used by store/snapshot/tombstones.bin
// and constraints.bin (1<<20). A legitimately large index is unaffected: the
// map grows past the hint as entries are inserted.
const labelCapHintMax = 1 << 20

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Serialize writes the index's per-label bitmaps to w in the format
// documented in docs/persistence.md. The on-disk layout is:
//
//	uint32 magic ('SLBI')
//	uint32 formatVersion
//	uint32 labelCount
//	repeat labelCount times:
//	  uint32 labelID
//	  uint64 bitmapLen
//	  [bitmapLen]byte bitmap (Roaring native binary format)
//	uint32 crc32c (covers every byte above, little-endian)
//
// Serialize takes the index's RLock for the whole emission so a
// concurrent writer cannot observe a partially serialised state. The
// returned error wraps the underlying I/O failure verbatim; the
// caller treats short writes the same as any other I/O error.
func (i *Index) Serialize(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, labelMagic); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, labelFormatVersion); err != nil {
		return err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	if uint64(len(i.bits)) > uint64(^uint32(0)) {
		return fmt.Errorf("label: too many labels to serialize: %d", len(i.bits))
	}
	if err := binary.Write(tee, binary.LittleEndian, uint32(len(i.bits))); err != nil {
		return err
	}
	// Iterate in ascending labelID order so the on-disk form is
	// deterministic for a given in-memory state (helps fixture diffs
	// and reproducibility).
	keys := make([]uint32, 0, len(i.bits))
	for k := range i.bits {
		keys = append(keys, k)
	}
	sortUint32(keys)

	var scratch bytes.Buffer
	for _, k := range keys {
		set := i.bits[k]
		// Materialise a roaring bitmap from the set's logical contents and
		// write its native binary form. roaring64.WriteTo is a
		// content-deterministic pure function of the final set (it never
		// implicitly RunOptimizes), so a bitmap built here via AddMany of
		// the sorted ids is BYTE-IDENTICAL to one that held the same ids all
		// along — the inline small-set tier produces exactly the bytes the
		// pre-refactor per-label *roaring64.Bitmap produced, keeping the
		// on-disk format unchanged with zero migration (storage-engine-
		// auditor, #1585). A dense (AddRange) label is already a bitmap, so
		// Bitmap returns it directly with no materialisation cost.
		bm, _ := set.Bitmap()
		if err := binary.Write(tee, binary.LittleEndian, k); err != nil {
			return err
		}
		scratch.Reset()
		size := bm.GetSerializedSizeInBytes()
		scratch.Grow(int(size))
		n, err := bm.WriteTo(&scratch)
		if err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, uint64(n)); err != nil {
			return err
		}
		if _, err := tee.Write(scratch.Bytes()); err != nil {
			return err
		}
	}

	// CRC trailer is written to the underlying buffered writer only;
	// it must NOT feed back into the hasher.
	sum := hasher.Sum32()
	if err := binary.Write(bw, binary.LittleEndian, sum); err != nil {
		return err
	}
	return bw.Flush()
}

// Deserialize replaces the receiver's state with the contents of r.
// On any structural problem, truncated payload, or CRC mismatch the
// function returns a wrapped [index.ErrIndexCorrupted] and the
// receiver is restored to the pre-call state.
//
// The implementation reads the whole payload into a buffer, validates
// the trailing CRC32C against the prefix, then re-parses the prefix
// to populate the bitmaps. This costs one extra pass over the data
// but keeps the corruption-detection contract simple and lets the
// reader reject malformed inputs before any state mutation.
func (i *Index) Deserialize(r io.Reader) error {
	all, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%w: read: %w", index.ErrIndexCorrupted, err)
	}
	if len(all) < 4 {
		return fmt.Errorf("%w: short payload", index.ErrIndexCorrupted)
	}
	body := all[:len(all)-4]
	trailer := binary.LittleEndian.Uint32(all[len(all)-4:])
	if got := crc32.Checksum(body, castagnoli); got != trailer {
		return fmt.Errorf("%w: crc32c mismatch: got %d, want %d",
			index.ErrIndexCorrupted, got, trailer)
	}

	br := bufio.NewReader(bytes.NewReader(body))
	var magic, version uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		return fmt.Errorf("%w: magic: %w", index.ErrIndexCorrupted, err)
	}
	if magic != labelMagic {
		return fmt.Errorf("%w: bad magic %#x", index.ErrIndexCorrupted, magic)
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("%w: version: %w", index.ErrIndexCorrupted, err)
	}
	if version != labelFormatVersion {
		return fmt.Errorf("%w: unsupported format version %d",
			index.ErrIndexCorrupted, version)
	}
	var count uint32
	if err := binary.Read(br, binary.LittleEndian, &count); err != nil {
		return fmt.Errorf("%w: count: %w", index.ErrIndexCorrupted, err)
	}

	hint := int(count)
	if hint > labelCapHintMax {
		hint = labelCapHintMax
	}
	bits := make(map[uint32]index.NodeSet, hint)
	for k := uint32(0); k < count; k++ {
		var labelID uint32
		if err := binary.Read(br, binary.LittleEndian, &labelID); err != nil {
			return fmt.Errorf("%w: labelID: %w", index.ErrIndexCorrupted, err)
		}
		var bmLen uint64
		if err := binary.Read(br, binary.LittleEndian, &bmLen); err != nil {
			return fmt.Errorf("%w: bitmapLen: %w", index.ErrIndexCorrupted, err)
		}
		if bmLen > uint64(len(body)) {
			return fmt.Errorf("%w: implausible bitmap length %d",
				index.ErrIndexCorrupted, bmLen)
		}
		buf := make([]byte, bmLen)
		if _, err := io.ReadFull(br, buf); err != nil {
			return fmt.Errorf("%w: bitmap bytes: %w", index.ErrIndexCorrupted, err)
		}
		bm := roaring64.New()
		if _, err := bm.ReadFrom(bytes.NewReader(buf)); err != nil {
			return fmt.Errorf("%w: bitmap parse: %w", index.ErrIndexCorrupted, err)
		}
		// Down-convert a sparse label to the inline small-set tier so a
		// reload recovers the memory win (a snapshot taken before this
		// refactor carries roaring images for singleton labels). A dense
		// label stays on the bitmap tier. This is purely an in-memory
		// representation choice; the bytes already read were validated above
		// and are not affected.
		bits[labelID] = index.NodeSetFromBitmap(bm)
	}

	i.mu.Lock()
	i.bits = bits
	i.mu.Unlock()
	return nil
}

// sortUint32 sorts s in place in ascending order. Local to keep the
// import surface small (sort.Slice would force a closure allocation
// for what is a tiny in-place sort on a value type).
func sortUint32(s []uint32) {
	// Insertion sort is fine — labels per index are usually small
	// (dozens, at most thousands), well below the slice sort cutoff.
	for i := 1; i < len(s); i++ {
		x := s[i]
		j := i - 1
		for j >= 0 && s[j] > x {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = x
	}
}

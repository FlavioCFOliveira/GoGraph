package csrfile

// write_bytes.go — zero-copy little-endian byte views for the bulk writer
// (sprint 221, #1597). These mirror the read side's zero-copy reinterpretation
// (the mmap reader's bindSlices, which reslices the mapped region directly with
// unsafe.Slice; see also the standalone [Reinterpret] helper) and the snapshot
// codec's S210 streaming helpers:
// graph.NodeID and uint64 are 8-byte, 8-aligned, native-endian, and this
// package already relies on a little-endian host for its mmap reinterpretation,
// so the byte view is byte-identical to what binary.Write(LittleEndian, ...)
// would emit — with no transient widening/scratch allocation.

import (
	"io"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// csrWriteChunk bounds the writer's transient working set: sections are
// emitted in <= 64 KiB slices via [streamLE], so the serialiser never
// materialises a whole-section copy regardless of graph size.
const csrWriteChunk = 64 << 10

// nodeIDsAsBytes returns the raw little-endian byte view of a []graph.NodeID
// without copying. graph.NodeID IS uint64 (graph/graph.go); on a little-endian
// host the backing memory is exactly the on-disk layout binary.Write would
// produce. This removes the prior no-op widening copy
// (tmp := make([]uint64, len(edges))) — NodeID is already 8 bytes wide.
func nodeIDsAsBytes(s []graph.NodeID) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), 8*len(s)) //nolint:gosec // zero-copy LE reinterpretation; NodeID is an 8-byte uint64, host is little-endian
}

// uint64sAsBytes returns the raw little-endian byte view of a []uint64 without
// copying. See [nodeIDsAsBytes] for the soundness argument.
func uint64sAsBytes(s []uint64) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), 8*len(s)) //nolint:gosec // zero-copy LE reinterpretation; uint64 is 8 bytes, host is little-endian
}

// streamLE writes b to w in chunks of at most csrWriteChunk bytes. Chunking
// bounds the working set independently of len(b) and has NO effect on the bytes
// emitted (or the tee'd CRC32C), so the on-disk layout is identical to a single
// binary.Write call.
// weightsAsBytes returns the raw little-endian byte view of a fixed-size weight
// slice []W without copying. elemSize is the byte width of one W — the value
// [WeightKind.Size] returns for the kind [weightKindOf] resolved — and the
// caller guarantees it is non-zero, since weights are only serialised when the
// kind is not [WeightAbsent].
//
// It replaces binary.Write on the weights section (rmp #2529). binary.Write
// refuses any slice whose element is not fixed-size, which on this path meant
// int, uint and uintptr — types csrfile's own godoc advertised as supported and
// then failed on, with an opaque "some values are not fixed-sized" error rather
// than a typed one. bool was refused for the same reason. The raw view has no
// such restriction and, on a little-endian host, is byte-identical to what
// binary.Write emitted for every kind that already worked, so no existing file's
// layout changes.
//
// store/snapshot reached the identical conclusion first, for the identical
// reason; see its weightsAsBytes. Sharing the reasoning rather than the code is
// deliberate: the two packages own separate on-disk formats and must be free to
// diverge, so what is reconciled is the SET of kinds, stated once in
// [weightKindOf] and mirrored by snapshot's csrWeightSize.
func weightsAsBytes[W any](s []W, elemSize int) []byte {
	if len(s) == 0 || elemSize == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), elemSize*len(s)) //nolint:gosec // zero-copy reinterpretation of a fixed-size primitive weight slice; host is little-endian
}

func streamLE(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n := len(b)
		if n > csrWriteChunk {
			n = csrWriteChunk
		}
		if _, err := w.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

package csrfile

// write_bytes.go — zero-copy little-endian byte views for the bulk writer
// (sprint 221, #1597). These mirror the read side ([Reinterpret], used by the
// mmap reader's bindSlices) and the snapshot codec's S210 streaming helpers:
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

package snapshot

import (
	"io"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// csrWriteChunk is the fixed byte budget streamed to the destination writer
// per [streamLE] iteration. It bounds the writer's transient working set: the
// CSR sections (vertices, edges, weights, handles) are emitted in <= 64 KiB
// slices, so the serialiser never materialises a whole-section copy regardless
// of graph size. 64 KiB is large enough to amortise the per-Write call cost
// against the CRC32C hardware path yet small enough that the chunk stays in L1.
const csrWriteChunk = 64 << 10

// nodeIDsAsBytes returns the raw little-endian byte view of a []graph.NodeID
// without copying. graph.NodeID IS uint64 (graph/graph.go), 8 bytes wide and
// 8-byte aligned, so on a little-endian host the slice's backing memory is
// exactly the on-disk little-endian uint64 layout that binary.Write would
// produce — the byte view is byte-identical, no swap.
//
// This is the read-side seam: [readCSRLimited] reads the edge bytes straight
// into the destination []graph.NodeID via this view, eliminating the prior
// `raw []uint64` shadow column. The format is explicitly little-endian and the
// engine runs only on little-endian hosts (see store/csrfile, which relies on
// the same invariant for its mmap reinterpretation); a big-endian port would
// have to byte-swap here and in csrfile alike.
func nodeIDsAsBytes(s []graph.NodeID) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), 8*len(s)) //nolint:gosec // zero-copy LE reinterpretation; NodeID is an 8-byte uint64, host is little-endian
}

// uint64sAsBytes returns the raw little-endian byte view of a []uint64 without
// copying. See [nodeIDsAsBytes] for the soundness argument; the two differ only
// in element type (both are 8-byte, 8-aligned, native-endian).
func uint64sAsBytes(s []uint64) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), 8*len(s)) //nolint:gosec // zero-copy LE reinterpretation; uint64 is 8 bytes, host is little-endian
}

// weightsAsBytes returns the raw byte view of a fixed-size weight slice []W
// without copying. elemSize is the byte width of one W (the value
// csrWeightSize[W]() returns); the caller guarantees it is non-zero (weights
// are only serialised when wsize > 0, i.e. W is a fixed-size primitive). On a
// little-endian host the view is byte-identical to binary.Write's output for
// the same slice, so the on-disk weights section is unchanged.
func weightsAsBytes[W any](s []W, elemSize int) []byte {
	if len(s) == 0 || elemSize == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), elemSize*len(s)) //nolint:gosec // zero-copy reinterpretation of a fixed-size primitive weight slice; host is little-endian
}

// streamLE writes b to w in chunks of at most csrWriteChunk bytes. Splitting
// the write bounds the bufio writer's flush granularity and keeps the call's
// transient working set independent of len(b); it has NO effect on the bytes
// emitted (a chunked sequence of writes produces the same byte stream as one
// whole-slice write), so the on-disk layout and the tee'd CRC32C are identical
// to the prior single binary.Write call.
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

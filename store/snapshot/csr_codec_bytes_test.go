package snapshot

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestNodeIDsAsBytes_ByteIdentical asserts the zero-copy byte view of a
// []graph.NodeID is byte-identical to binary.Write's little-endian encoding of
// the same slice. This is the contract WriteCSR / readCSRLimited rely on: the
// view replaces binary.Write without changing a single on-disk byte. The empty
// slice yields a nil view (no out-of-bounds index of s[0]).
func TestNodeIDsAsBytes_ByteIdentical(t *testing.T) {
	t.Parallel()
	if got := nodeIDsAsBytes(nil); got != nil {
		t.Fatalf("nodeIDsAsBytes(nil) = %v, want nil", got)
	}
	if got := nodeIDsAsBytes([]graph.NodeID{}); got != nil {
		t.Fatalf("nodeIDsAsBytes(empty) = %v, want nil", got)
	}
	s := []graph.NodeID{0, 1, 0xFF, 0x0102030405060708, ^graph.NodeID(0)}
	got := nodeIDsAsBytes(s)
	var want bytes.Buffer
	if err := binary.Write(&want, binary.LittleEndian, []uint64{0, 1, 0xFF, 0x0102030405060708, ^uint64(0)}); err != nil {
		t.Fatalf("binary.Write: %v", err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("byte view mismatch:\n got=%x\nwant=%x", got, want.Bytes())
	}
}

// TestUint64sAsBytes_ByteIdentical is the []uint64 analogue of
// [TestNodeIDsAsBytes_ByteIdentical]; it also covers the empty-slice branch.
func TestUint64sAsBytes_ByteIdentical(t *testing.T) {
	t.Parallel()
	if got := uint64sAsBytes(nil); got != nil {
		t.Fatalf("uint64sAsBytes(nil) = %v, want nil", got)
	}
	if got := uint64sAsBytes([]uint64{}); got != nil {
		t.Fatalf("uint64sAsBytes(empty) = %v, want nil", got)
	}
	s := []uint64{0, 42, 0xDEADBEEFCAFEBABE, ^uint64(0)}
	got := uint64sAsBytes(s)
	var want bytes.Buffer
	if err := binary.Write(&want, binary.LittleEndian, s); err != nil {
		t.Fatalf("binary.Write: %v", err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("byte view mismatch:\n got=%x\nwant=%x", got, want.Bytes())
	}
}

// TestWeightsAsBytes_ByteIdentical asserts the weight byte view matches
// binary.Write across the fixed-size primitive weight kinds the codec supports
// (1/2/4/8-byte) and that a nil/zero-elemSize input yields a nil view.
func TestWeightsAsBytes_ByteIdentical(t *testing.T) {
	t.Parallel()
	if got := weightsAsBytes([]int64{1, 2}, 0); got != nil {
		t.Fatalf("weightsAsBytes(elemSize=0) = %v, want nil", got)
	}
	if got := weightsAsBytes[int64](nil, 8); got != nil {
		t.Fatalf("weightsAsBytes(nil) = %v, want nil", got)
	}

	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		s := []int64{-1, 0, 1, 0x0102030405060708}
		got := weightsAsBytes(s, 8)
		var want bytes.Buffer
		if err := binary.Write(&want, binary.LittleEndian, s); err != nil {
			t.Fatalf("binary.Write: %v", err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("int64 mismatch:\n got=%x\nwant=%x", got, want.Bytes())
		}
	})

	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		s := []float64{0, 1.5, -2.25, 1e308}
		got := weightsAsBytes(s, 8)
		var want bytes.Buffer
		if err := binary.Write(&want, binary.LittleEndian, s); err != nil {
			t.Fatalf("binary.Write: %v", err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("float64 mismatch:\n got=%x\nwant=%x", got, want.Bytes())
		}
	})

	t.Run("int32", func(t *testing.T) {
		t.Parallel()
		s := []int32{-1, 0, 1, 0x01020304}
		got := weightsAsBytes(s, 4)
		var want bytes.Buffer
		if err := binary.Write(&want, binary.LittleEndian, s); err != nil {
			t.Fatalf("binary.Write: %v", err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("int32 mismatch:\n got=%x\nwant=%x", got, want.Bytes())
		}
	})

	t.Run("int8", func(t *testing.T) {
		t.Parallel()
		s := []int8{-1, 0, 1, 127}
		got := weightsAsBytes(s, 1)
		var want bytes.Buffer
		if err := binary.Write(&want, binary.LittleEndian, s); err != nil {
			t.Fatalf("binary.Write: %v", err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("int8 mismatch:\n got=%x\nwant=%x", got, want.Bytes())
		}
	})
}

// TestStreamLE_SplitsWithoutChangingBytes asserts streamLE emits exactly the
// input bytes regardless of how the chunk boundary falls — proving the chunked
// write produces the same byte stream (and therefore the same CRC) as one
// whole-slice write. It probes sizes straddling csrWriteChunk.
func TestStreamLE_SplitsWithoutChangingBytes(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, csrWriteChunk - 1, csrWriteChunk, csrWriteChunk + 1, 3*csrWriteChunk + 7} {
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i*31 + 7)
		}
		var got bytes.Buffer
		if err := streamLE(&got, src); err != nil {
			t.Fatalf("n=%d streamLE: %v", n, err)
		}
		if !bytes.Equal(got.Bytes(), src) {
			t.Fatalf("n=%d: streamLE altered bytes", n)
		}
	}
}

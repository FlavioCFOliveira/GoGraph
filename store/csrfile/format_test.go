package csrfile

import (
	"errors"
	"testing"
)

func TestLayout_AlignedAndCovers(t *testing.T) {
	t.Parallel()
	h, total := Layout(1000, 4000, WeightUint64)
	if h.VerticesOffset%Alignment != 0 {
		t.Fatalf("vertices not aligned: %d", h.VerticesOffset)
	}
	if h.EdgesOffset%Alignment != 0 {
		t.Fatalf("edges not aligned: %d", h.EdgesOffset)
	}
	if h.WeightsOffset%Alignment != 0 {
		t.Fatalf("weights not aligned: %d", h.WeightsOffset)
	}
	if h.TailCRCOffset%Alignment != 0 {
		t.Fatalf("tail crc not aligned: %d", h.TailCRCOffset)
	}
	if total < h.TailCRCOffset+4 {
		t.Fatalf("total %d does not include CRC", total)
	}
}

func TestLayout_NoWeights(t *testing.T) {
	t.Parallel()
	h, _ := Layout(100, 200, WeightAbsent)
	if h.WeightsOffset != 0 {
		t.Fatalf("WeightsOffset should be 0 when absent, got %d", h.WeightsOffset)
	}
}

func TestHeaderRoundtrip(t *testing.T) {
	t.Parallel()
	h, _ := Layout(10, 20, WeightFloat64)
	buf := EncodeHeader(h)
	if len(buf) != HeaderSize {
		t.Fatalf("encoded length = %d, want %d", len(buf), HeaderSize)
	}
	back, err := DecodeHeader(buf)
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if back != h {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", back, h)
	}
}

func TestDecodeHeader_BadMagic(t *testing.T) {
	t.Parallel()
	buf := make([]byte, HeaderSize)
	if _, err := DecodeHeader(buf); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("expected ErrBadMagic, got %v", err)
	}
}

func TestDecodeHeader_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	h, _ := Layout(1, 1, WeightAbsent)
	h.Version = CurrentVersion + 9
	buf := EncodeHeader(h)
	if _, err := DecodeHeader(buf); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestDecodeHeader_UnsupportedByteOrder(t *testing.T) {
	t.Parallel()
	h, _ := Layout(1, 1, WeightAbsent)
	buf := EncodeHeader(h)
	buf[6] = 1 // big-endian flag
	if _, err := DecodeHeader(buf); !errors.Is(err, ErrUnsupportedByteOrder) {
		t.Fatalf("expected ErrUnsupportedByteOrder, got %v", err)
	}
}

func TestDecodeHeader_UnknownWeight(t *testing.T) {
	t.Parallel()
	h, _ := Layout(1, 1, WeightAbsent)
	buf := EncodeHeader(h)
	buf[24] = 99
	if _, err := DecodeHeader(buf); !errors.Is(err, ErrUnknownWeightKind) {
		t.Fatalf("expected ErrUnknownWeightKind, got %v", err)
	}
}

func TestAlignUp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, a, want uint64
	}{
		{0, 64, 0},
		{1, 64, 64},
		{63, 64, 64},
		{64, 64, 64},
		{65, 64, 128},
	}
	for _, c := range cases {
		if got := AlignUp(c.n, c.a); got != c.want {
			t.Fatalf("AlignUp(%d, %d) = %d, want %d", c.n, c.a, got, c.want)
		}
	}
}

func TestWeightKind_Size(t *testing.T) {
	t.Parallel()
	cases := map[WeightKind]int{
		WeightAbsent:  0,
		WeightUint32:  4,
		WeightFloat32: 4,
		WeightUint64:  8,
		WeightFloat64: 8,
	}
	for k, want := range cases {
		if got := k.Size(); got != want {
			t.Fatalf("%d.Size = %d, want %d", k, got, want)
		}
	}
}

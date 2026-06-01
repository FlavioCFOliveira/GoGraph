package csrfile

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"
	"unsafe"
)

func TestReinterpret_Uint64Roundtrip(t *testing.T) {
	t.Parallel()
	const n = 256
	r := rand.New(rand.NewPCG(13, 1)) //nolint:gosec // deterministic test RNG
	want := make([]uint64, n)
	for i := range want {
		want[i] = r.Uint64()
	}
	buf := make([]byte, 8*n)
	for i, v := range want {
		binary.LittleEndian.PutUint64(buf[i*8:], v)
	}
	got := Reinterpret[uint64](buf, n)
	if len(got) != n {
		t.Fatalf("len = %d, want %d", len(got), n)
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], v)
		}
	}
}

func TestReinterpret_Float64Roundtrip(t *testing.T) {
	t.Parallel()
	const n = 32
	want := make([]float64, n)
	for i := range want {
		want[i] = float64(i) * 0.5
	}
	buf := make([]byte, 8*n)
	for i, v := range want {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64ToBits(v))
	}
	got := Reinterpret[float64](buf, n)
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("got[%d] = %f, want %f", i, got[i], v)
		}
	}
}

func TestReinterpret_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	if got := Reinterpret[uint64]([]byte{}, 0); got != nil {
		t.Fatalf("Reinterpret(empty, 0) = %v, want nil", got)
	}
}

func TestReinterpret_ShortPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic")
		}
	}()
	_ = Reinterpret[uint64](make([]byte, 4), 2) // need 16 bytes
}

func TestReinterpret_MisalignedPanics(t *testing.T) {
	t.Parallel()
	// Build a 9-byte buffer and reinterpret starting at offset 1
	// to force misalignment.
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic")
		}
	}()
	buf := make([]byte, 17)
	// Adjust until misaligned for uint64 (need offset % 8 != 0).
	off := 1
	for (uintptr(unsafe.Pointer(&buf[off])) % 8) == 0 { //nolint:gosec // alignment probe
		off++
	}
	_ = Reinterpret[uint64](buf[off:], 1)
}

// TestReinterpret_OverflowPanics asserts that an (size, n) pair whose
// byte requirement size(T)*n overflows int is rejected via the guarded
// "data too short" panic path rather than wrapping to a small product
// and slicing out of bounds. With T=uint64 (size 8), n = MaxInt/8 + 1
// makes 8*n overflow int; no real buffer can satisfy it, so the call
// must panic deterministically instead of succeeding.
func TestReinterpret_OverflowPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on overflowing (size, n)")
		}
	}()
	// A tiny buffer: had the multiply wrapped to a small value, the
	// length check could have spuriously passed and produced an
	// out-of-bounds alias.
	n := math.MaxInt/8 + 1
	_ = Reinterpret[uint64](make([]byte, 16), n)
}

func BenchmarkReinterpret_Uint64(b *testing.B) {
	buf := make([]byte, 8*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Reinterpret[uint64](buf, 1024)
	}
}

func uint64ToBits(v float64) uint64 {
	return *(*uint64)(unsafe.Pointer(&v)) //nolint:gosec // bit pattern probe
}

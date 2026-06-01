package csrfile

import (
	"fmt"
	"math"
	"math/bits"
	"unsafe"
)

// Reinterpret returns a typed slice of length n that aliases the
// memory backing data. T must be a fixed-size primitive (or a named
// alias of one) — int8/int16/int32/int64/uint*/float32/float64 or a
// type whose layout is identical to one of those. The function
// panics when data is too short to hold n elements of T or when its
// alignment is incompatible with T.
//
// Precondition on n: the byte requirement size(T)*n must be
// representable; n is treated as untrusted. When size(T)*n overflows
// (or exceeds what a Go slice length can address), the requirement
// can never be satisfied by any real buffer, so Reinterpret takes the
// same "data too short" panic path rather than computing a wrapped
// product and slicing out of bounds. Callers that derive n from a
// wire- or file-encoded value therefore get a deterministic, guarded
// failure instead of an out-of-bounds read.
//
// Because the returned slice aliases data's memory, callers must
// preserve the data buffer's lifetime for the duration they hold
// the returned slice, and must not mutate it through the returned
// slice in ways that would surprise other readers. Typical use:
// re-typing the body of a memory-mapped region as a []uint64 view.
//
// This helper is unsafe in the Go-language sense (it uses
// [unsafe.Pointer] and [unsafe.Slice]); the project policy for
// unsafe usage is documented in CONTRIBUTING.md.
func Reinterpret[T any](data []byte, n int) []T {
	if n < 0 {
		panic("csrfile: Reinterpret called with negative n")
	}
	if n == 0 {
		return nil
	}
	var zero T
	size := int(unsafe.Sizeof(zero))
	if size == 0 {
		panic("csrfile: Reinterpret called with zero-sized element type")
	}
	// Compute size*n in 64-bit precision and reject overflow before
	// it can wrap. bits.Mul64 yields the full 128-bit product; a
	// non-zero high word, or a low word that exceeds the maximum int
	// (and therefore any addressable slice length), means no real
	// buffer can satisfy the requirement. Fall through to the same
	// "data too short" panic with the saturated requirement so the
	// failure is deterministic rather than an out-of-bounds slice.
	hi, lo := bits.Mul64(uint64(size), uint64(n))
	need := int(lo) //nolint:gosec // overflow guarded by hi/lo checks below
	if hi != 0 || lo > uint64(math.MaxInt) {
		need = math.MaxInt // unsatisfiable: forces the short-data path
	}
	if len(data) < need {
		panic(fmt.Sprintf("csrfile: Reinterpret: need %d bytes for %d x %T, got %d",
			need, n, zero, len(data)))
	}
	align := int(unsafe.Alignof(zero))
	if uintptr(unsafe.Pointer(&data[0]))%uintptr(align) != 0 { //nolint:gosec // alignment probe
		panic(fmt.Sprintf("csrfile: Reinterpret: base address not aligned to %d bytes for %T",
			align, zero))
	}
	return unsafe.Slice((*T)(unsafe.Pointer(&data[0])), n) //nolint:gosec // intentional zero-copy reinterpretation
}

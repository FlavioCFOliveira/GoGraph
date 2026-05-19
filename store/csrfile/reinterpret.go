package csrfile

import (
	"fmt"
	"unsafe"
)

// Reinterpret returns a typed slice of length n that aliases the
// memory backing data. T must be a fixed-size primitive (or a named
// alias of one) — int8/int16/int32/int64/uint*/float32/float64 or a
// type whose layout is identical to one of those. The function
// panics when data is too short to hold n elements of T or when its
// alignment is incompatible with T.
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
	need := size * n
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

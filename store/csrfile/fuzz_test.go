package csrfile

import (
	"testing"
)

// FuzzCSRFileReader feeds arbitrary bytes through the full in-memory reader
// path — DecodeHeader, header validation, the zero-copy slice reinterpretation
// (bindSlices) and the CSR semantic checks — via openBytes. The contract is
// fail-closed: the reader must return a typed error rather than panic on any
// input.
//
// The fuzz buffer is copied into an 8-byte-aligned allocation (allocAligned8),
// mirroring how production always presents the reader an aligned region (mmap
// page alignment, or allocAligned8 for the byte-backed path); bindSlices'
// unsafe.Slice reinterpretation requires that alignment.
func FuzzCSRFileReader(f *testing.F) {
	// Seed with the canonical magic + version header so the fuzzer has a
	// starting point that reaches past the magic check into validation and the
	// reinterpretation path. Magic is {'G','G','C','S'} (see format.go).
	seed := []byte{
		// magic bytes  G  G  C  S
		'G', 'G', 'C', 'S',
		// version, flags, weight kind, N nodes, N edges, offsets, padding...
	}
	for len(seed) < 96 {
		seed = append(seed, 0)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		// The contract: no panic on any input, only typed errors.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("csrfile reader panicked on input: %v", r)
			}
		}()
		// DecodeHeader alone (cheap; also covers inputs too short for openBytes).
		_, _ = DecodeHeader(data)
		// Full reinterpretation path on an aligned copy of the fuzz bytes.
		aligned := allocAligned8(len(data))
		copy(aligned, data)
		_, _ = openBytes(aligned)
	})
}

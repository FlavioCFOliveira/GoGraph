package csrfile

import (
	"testing"
)

// FuzzCSRFileReader feeds arbitrary bytes into DecodeHeader / the
// subsequent slice-reinterpretation path. The contract is fail-closed:
// the reader must return a typed error rather than panic on any input.
func FuzzCSRFileReader(f *testing.F) {
	// Seed with the canonical magic + version header so the fuzzer
	// has a starting point that exercises both the happy path and
	// targeted corruption.
	seed := []byte{
		// magic bytes  G  R  C  S  R  v  0  1
		0x47, 0x52, 0x43, 0x53, 0x52, 0x76, 0x30, 0x31,
		// version, flags, weight kind, N nodes, N edges, padding...
	}
	for len(seed) < 96 {
		seed = append(seed, 0)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		// Suppress panics from any deep parsing path.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("DecodeHeader panicked on input: %v", r)
			}
		}()
		_, _ = DecodeHeader(data)
	})
}

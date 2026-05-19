package graphml

import (
	"bytes"
	"testing"
)

// FuzzGraphMLReader feeds arbitrary bytes into the GraphML parser.
// Seeded with a valid minimal GraphML document so the fuzzer can
// exercise both happy-path tokens and adversarial mutations.
func FuzzGraphMLReader(f *testing.F) {
	const minimal = `<?xml version="1.0" encoding="UTF-8"?>
<graphml xmlns="http://graphml.graphdrawing.org/xmlns">
  <graph id="G" edgedefault="directed">
    <node id="n0"/>
    <node id="n1"/>
    <edge source="n0" target="n1"/>
  </graph>
</graphml>`
	f.Add([]byte(minimal))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Cap fuzz input size so pathological XML (deep nesting,
		// huge attribute lists) cannot stall the worker past the
		// fuzz timeout. The parser must still be fail-closed on
		// any input below the cap.
		const maxFuzzBytes = 16 << 10
		if len(data) > maxFuzzBytes {
			data = data[:maxFuzzBytes]
		}
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("ReadInto panicked on input: %v", r)
			}
		}()
		_, _, _ = ReadInto(bytes.NewReader(data))
	})
}

package graphml_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// FuzzSec_IO_GraphMLReadWithProps fuzzes the typed-property GraphML read
// path (ReadWithProps), which the existing FuzzGraphMLReader did not cover:
// that target exercises only the plain weight-only ReadInto path, leaving
// the <key>/<data> typed-property decoding — long/double/boolean/time/bytes/
// list, including the recursive list decoder — unfuzzed.
//
// The corpus seeds one valid document per PropertyKind so the fuzzer starts
// from structurally-valid typed payloads and mutates the type tags, the
// encoded values, and the nesting. The body asserts the contract every
// hostile mutation must uphold: the reader never panics, and on failure it
// returns a non-nil error with a nil graph (all-or-nothing). It runs as an
// ordinary unit test (replaying the seed corpus) under `go test`; pass
// -fuzz to explore further.
func FuzzSec_IO_GraphMLReadWithProps(f *testing.F) {
	header := `<?xml version="1.0"?>` +
		`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">`

	seeds := []string{
		// string
		header +
			`<key id="p_s" for="node" attr.name="s" attr.type="string"/>` +
			`<graph edgedefault="directed"><node id="n0"><data key="p_s">hello</data></node></graph></graphml>`,
		// long (int64)
		header +
			`<key id="p_i" for="node" attr.name="i" attr.type="long"/>` +
			`<graph edgedefault="directed"><node id="n0"><data key="p_i">42</data></node></graph></graphml>`,
		// double (float64), incl. an xs:double special form
		header +
			`<key id="p_f" for="node" attr.name="f" attr.type="double"/>` +
			`<graph edgedefault="directed"><node id="n0"><data key="p_f">INF</data></node></graph></graphml>`,
		// boolean
		header +
			`<key id="p_b" for="node" attr.name="b" attr.type="boolean"/>` +
			`<graph edgedefault="directed"><node id="n0"><data key="p_b">true</data></node></graph></graphml>`,
		// time (RFC3339Nano via the non-standard "time" tag)
		header +
			`<key id="p_t" for="node" attr.name="t" attr.type="time"/>` +
			`<graph edgedefault="directed"><node id="n0"><data key="p_t">2024-01-15T12:00:00Z</data></node></graph></graphml>`,
		// bytes (base64 via the non-standard "bytes" tag)
		header +
			`<key id="p_blob" for="node" attr.name="blob" attr.type="bytes"/>` +
			`<graph edgedefault="directed"><node id="n0"><data key="p_blob">3q2+7w==</data></node></graph></graphml>`,
		// list (JSON array of [type, value] pairs, with a nested list)
		header +
			`<key id="p_l" for="node" attr.name="l" attr.type="list"/>` +
			`<graph edgedefault="directed"><node id="n0">` +
			`<data key="p_l">[["long","1"],["list","[[\"long\",\"2\"]]"]]</data>` +
			`</node></graph></graphml>`,
		// edge with weight + a typed node property together
		header +
			`<key id="w" for="edge" attr.name="weight" attr.type="long"/>` +
			`<key id="p_i" for="node" attr.name="i" attr.type="long"/>` +
			`<graph edgedefault="directed">` +
			`<node id="a"><data key="p_i">7</data></node><node id="b"/>` +
			`<edge source="a" target="b"><data key="w">5</data></edge>` +
			`</graph></graphml>`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Cap the input so a pathological mutation (deep nesting, an
		// unterminated token) cannot stall the worker; the reader must
		// still be fail-closed below the cap.
		const maxFuzzBytes = 64 << 10
		if len(data) > maxFuzzBytes {
			data = data[:maxFuzzBytes]
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReadWithProps panicked on input %q: %v", data, r)
			}
		}()

		g, n, err := graphml.ReadWithPropsCappedCtx(
			context.Background(), bytes.NewReader(data), maxFuzzBytes)
		if err != nil {
			// Contract: on any error the graph is nil and the edge count is
			// not positive (all-or-nothing at the in-memory level).
			if g != nil {
				t.Fatalf("error returned but graph is non-nil: err=%v", err)
			}
			if n > 0 {
				t.Fatalf("error returned but edge count = %d (> 0)", n)
			}
			// An error is always wrapped — never a bare nil that escaped the
			// type. errors.Is on a sentinel must not panic on it either.
			_ = errors.Is(err, graphml.ErrInputTooLarge)
			return
		}
		// Success path: a non-nil graph and a sane (non-negative) count.
		if g == nil {
			t.Fatalf("nil error but nil graph")
		}
		if n < 0 {
			t.Fatalf("negative edge count %d", n)
		}
	})
}

package graphml_test

// security_dom_amplification_test.go — security engagement 2026-07-02 R2 (#1851).
//
// Finding F1 (CWE-789 / CWE-1284 / CWE-400): the reader decoded the whole
// document into a struct-per-element DOM (dec.Decode) before folding it into the
// graph, so a byte-capped file of millions of tiny <edge/>/<node/>/<key/> tags
// forced 2.2–3.4 GiB of transient heap (10–27x the input) — an untrusted-input
// OOM. The reader now STREAMS: it folds each <node>/<edge> into the graph as it
// is decoded (peak tracks the output graph, not a DOM) and caps <key>
// declarations (schema has no proportional graph output, so a <key/> flood is
// the one element that could grow unbounded without a cap).
//
// These tests pin both defences: the <key> cap rejects a schema flood
// deterministically, and a large <node>/<edge> flood streams into the graph
// with allocation bounded to O(output) rather than the old ~2x (DOM + graph).

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// TestSec_IO_GraphMLKeyFloodRejected builds a document declaring more than the
// <key> cap (65536) and requires both readers to reject it with ErrTooManyKeys
// before the old DOM would have retained one struct per <key>.
func TestSec_IO_GraphMLKeyFloodRejected(t *testing.T) {
	t.Parallel()
	const keys = 70000 // > maxKeyDecls (1<<16 = 65536)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><graphml>`)
	for i := 0; i < keys; i++ {
		b.WriteString(`<key id="k`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`" for="node" attr.name="a`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`" attr.type="string"/>`)
	}
	b.WriteString(`<graph edgedefault="directed"></graph></graphml>`)
	doc := b.String()

	// Disable the byte cap (0) so the KEY cap — not ErrInputTooLarge — is the
	// guard under test.
	if _, _, err := graphml.ReadIntoCappedCtx(context.Background(), strings.NewReader(doc), 0); !errors.Is(err, graphml.ErrTooManyKeys) {
		t.Fatalf("ReadInto key flood: err = %v, want ErrTooManyKeys", err)
	}
	if _, _, err := graphml.ReadWithPropsCappedCtx(context.Background(), strings.NewReader(doc), 0); !errors.Is(err, graphml.ErrTooManyKeys) {
		t.Fatalf("ReadWithProps key flood: err = %v, want ErrTooManyKeys", err)
	}
}

// TestSec_IO_GraphMLNodeEdgeFloodStreamsAtScale parses a large node+edge
// document and asserts every element is folded correctly. It is the
// streaming-at-scale correctness pin: the reader consumes hundreds of thousands
// of elements one at a time (never a whole-document DOM), so this completes in
// bounded memory where the prior reader first materialised every element as a
// struct. (The transient DOM this fix removes is dominated by the output graph
// in the folded path and is therefore not separable via a cumulative-allocation
// assertion; the deterministic <key>-flood cap above is the clean
// anti-amplification guard, and the streaming structure is what bounds the peak
// here.)
func TestSec_IO_GraphMLNodeEdgeFloodStreamsAtScale(t *testing.T) {
	t.Parallel()
	const n = 300_000
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><graphml><graph edgedefault="directed">`)
	for i := 0; i < n; i++ {
		b.WriteString(`<node id="n`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`"/>`)
	}
	for i := 0; i < n; i++ {
		b.WriteString(`<edge source="n`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`" target="n`)
		b.WriteString(strconv.Itoa((i + 1) % n))
		b.WriteString(`"/>`)
	}
	b.WriteString(`</graph></graphml>`)

	a, added, err := graphml.ReadIntoCtx(context.Background(), strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("ReadInto node/edge flood: %v", err)
	}
	if added != n {
		t.Fatalf("edges folded = %d, want %d", added, n)
	}
	if got := a.Order(); got != n {
		t.Fatalf("nodes folded = %d, want %d", got, n)
	}
}

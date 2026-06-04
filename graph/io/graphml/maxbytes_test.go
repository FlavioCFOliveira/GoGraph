package graphml_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// hugeDoc builds a GraphML document padded with an oversized comment so
// the whole document comfortably exceeds a low byte cap before any
// useful structure is decoded.
func hugeDoc(padBytes int) string {
	return `<?xml version="1.0"?><graphml><!-- ` +
		strings.Repeat("x", padBytes) +
		` --><graph edgedefault="directed"><node id="a"/></graph></graphml>`
}

// TestReadIntoCappedCtx_Exceeded feeds a document far larger than a low
// cap and asserts ErrInputTooLarge from the capped variant.
func TestReadIntoCappedCtx_Exceeded(t *testing.T) {
	t.Parallel()

	const capBytes = 1024
	a, n, err := graphml.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(hugeDoc(capBytes*8)), capBytes)
	if !errors.Is(err, graphml.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
	if n != 0 {
		t.Errorf("edges = %d, want 0 on cap error", n)
	}
}

// TestReadWithPropsCappedCtx_Exceeded is the property-graph analogue.
func TestReadWithPropsCappedCtx_Exceeded(t *testing.T) {
	t.Parallel()

	const capBytes = 1024
	g, _, err := graphml.ReadWithPropsCappedCtx(context.Background(),
		strings.NewReader(hugeDoc(capBytes*8)), capBytes)
	if !errors.Is(err, graphml.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on cap error", g)
	}
}

// TestReadInto_DefaultCapAllowsSmallInput is the control: the default
// entry point applies DefaultMaxBytes yet decodes a small document fine.
func TestReadInto_DefaultCapAllowsSmallInput(t *testing.T) {
	t.Parallel()

	const doc = `<?xml version="1.0"?><graphml>` +
		`<graph edgedefault="directed">` +
		`<node id="a"/><node id="b"/>` +
		`<edge source="a" target="b"/>` +
		`</graph></graphml>`
	a, n, err := graphml.ReadInto(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil || n != 1 {
		t.Fatalf("a=%v edges=%d, want non-nil, 1", a, n)
	}
}

// TestReadWithProps_DefaultCapAllowsSmallInput is the property-graph
// control through the default entry point.
func TestReadWithProps_DefaultCapAllowsSmallInput(t *testing.T) {
	t.Parallel()

	const doc = `<?xml version="1.0"?><graphml>` +
		`<graph edgedefault="directed">` +
		`<node id="a"/><node id="b"/>` +
		`<edge source="a" target="b"/>` +
		`</graph></graphml>`
	g, n, err := graphml.ReadWithProps(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if g == nil || n != 1 {
		t.Fatalf("g=%v edges=%d, want non-nil, 1", g, n)
	}
}

// TestReadIntoCappedCtx_Disabled confirms a non-positive cap opts out:
// a document that would trip a tiny cap decodes fine once disabled.
func TestReadIntoCappedCtx_Disabled(t *testing.T) {
	t.Parallel()

	doc := hugeDoc(4096)
	a, _, err := graphml.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(doc), 0)
	if err != nil {
		t.Fatalf("cap disabled but got error: %v", err)
	}
	if a == nil {
		t.Fatal("graph is nil")
	}
}

package graphml_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// validThenBadWeight is a structurally valid GraphML document whose nodes
// and first edge are well-formed but whose final edge carries an
// unparseable weight, so the reader fails partway through edge ingestion.
const validThenBadWeight = `<?xml version="1.0"?>` +
	`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` +
	`<key id="w" for="edge" attr.name="weight" attr.type="long"/>` +
	`<graph edgedefault="directed">` +
	`<node id="a"/><node id="b"/><node id="c"/>` +
	`<edge source="a" target="b"><data key="w">1</data></edge>` +
	`<edge source="b" target="c"><data key="w">not-a-number</data></edge>` +
	`</graph></graphml>`

// edgeDoc is a small well-formed document with one edge; combined with a
// pre-cancelled context it exercises the in-edge-loop ctx.Err() check.
const edgeDoc = `<?xml version="1.0"?>` +
	`<graphml><graph edgedefault="directed">` +
	`<node id="a"/><node id="b"/>` +
	`<edge source="a" target="b"/>` +
	`</graph></graphml>`

// TestReadInto_NilGraphOnParseError pins the uniform all-or-nothing
// contract for the adjacency-list reader: a document valid up to a point
// and then malformed must yield a nil graph plus the typed error.
func TestReadInto_NilGraphOnParseError(t *testing.T) {
	t.Parallel()

	a, _, err := graphml.ReadInto(strings.NewReader(validThenBadWeight))
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on parse error", a)
	}
}

// TestReadWithProps_NilGraphOnParseError is the property-graph analogue.
func TestReadWithProps_NilGraphOnParseError(t *testing.T) {
	t.Parallel()

	g, _, err := graphml.ReadWithProps(strings.NewReader(validThenBadWeight))
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on parse error", g)
	}
}

// TestReadInto_NilGraphOnCancel confirms cancellation discards the
// partial adjacency list and surfaces the context error.
func TestReadInto_NilGraphOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, _, err := graphml.ReadIntoCtx(ctx, strings.NewReader(edgeDoc))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cancellation", a)
	}
}

// TestReadWithProps_NilGraphOnCancel is the property-graph analogue.
func TestReadWithProps_NilGraphOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g, _, err := graphml.ReadWithPropsCtx(ctx, strings.NewReader(edgeDoc))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on cancellation", g)
	}
}

package jsonl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// goodPrefix is several well-formed JSONL records; appending a malformed
// record to it exercises the "valid for N, then bad" mid-stream path.
const goodPrefix = `{"type":"node","id":"alice"}
{"type":"node","id":"bob"}
{"type":"edge","src":"alice","dst":"bob","weight":1}
`

// TestReadInto_NilGraphOnParseError pins the uniform all-or-nothing
// contract for the adjacency-list reader: a stream valid for several
// records and then malformed must yield a nil graph plus the typed error.
func TestReadInto_NilGraphOnParseError(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true}

	cases := []struct {
		name  string
		input string
	}{
		{"bad_json", goodPrefix + "{not json}\n"},
		{"unknown_type", goodPrefix + `{"type":"spaceship"}` + "\n"},
		{"edge_missing_dst", goodPrefix + `{"type":"edge","src":"x"}` + "\n"},
		{"node_missing_id", goodPrefix + `{"type":"node"}` + "\n"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _, err := jsonl.ReadInto(strings.NewReader(tc.input), cfg)
			if err == nil {
				t.Fatalf("expected an error for %s, got nil", tc.name)
			}
			if a != nil {
				t.Errorf("graph = %v, want nil on error", a)
			}
		})
	}
}

// TestReadWithProps_NilGraphOnParseError is the property-graph analogue.
func TestReadWithProps_NilGraphOnParseError(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true}

	cases := []struct {
		name  string
		input string
	}{
		{"bad_json", goodPrefix + "{not json}\n"},
		{"unknown_type", goodPrefix + `{"type":"spaceship"}` + "\n"},
		{"property_bad_value", goodPrefix +
			`{"type":"property","id":"alice","key":"age","kind":"int64","value":"notanint"}` + "\n"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, _, err := jsonl.ReadWithProps(strings.NewReader(tc.input), cfg)
			if err == nil {
				t.Fatalf("expected an error for %s, got nil", tc.name)
			}
			if g != nil {
				t.Errorf("graph = %v, want nil on error", g)
			}
		})
	}
}

// TestReadInto_NilGraphOnCancel confirms cancellation discards the
// partial adjacency list and surfaces the context error.
func TestReadInto_NilGraphOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, _, err := jsonl.ReadIntoCtx(ctx, strings.NewReader(goodPrefix), adjlist.Config{Directed: true})
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

	g, _, err := jsonl.ReadWithPropsCtx(ctx, strings.NewReader(goodPrefix), adjlist.Config{Directed: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on cancellation", g)
	}
}

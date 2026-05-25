package jsonl_test

import (
	"errors"
	"strings"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/io/jsonl"
)

// TestJSONL_UnknownRecordType confirms the public error contract for
// malformed or unrecognised record types via the exported API.
//
// Note: cases already exercised by the internal test suite (jsonl_test.go)
// — `{"type":"alien"}`, node-missing-id, edge-missing-dst — are
// intentionally avoided here to prevent duplication. This suite covers
// the sentinel value ([jsonl.ErrUnknownType]) and edge cases that differ
// from those in the internal suite.
func TestJSONL_UnknownRecordType(t *testing.T) {
	t.Parallel()

	cfg := adjlist.Config{Directed: true}

	cases := []struct {
		name    string
		input   string
		wantErr bool
		// sentinelCheck is non-nil when the error must wrap ErrUnknownType.
		sentinelCheck bool
	}{
		{
			// A JSON object with no "type" key at all deserialises with
			// Type=="", which falls through to the default branch and
			// should return ErrUnknownType.
			name:          "missing_type_field",
			input:         `{"id":"foo"}` + "\n",
			wantErr:       true,
			sentinelCheck: true,
		},
		{
			// An explicit empty string is equally unrecognised.
			name:          "empty_type",
			input:         `{"type":""}` + "\n",
			wantErr:       true,
			sentinelCheck: true,
		},
		{
			// Confirm the sentinel wraps for a novel unknown value too.
			name:          "novel_unknown_type",
			input:         `{"type":"spaceship"}` + "\n",
			wantErr:       true,
			sentinelCheck: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := jsonl.ReadInto(strings.NewReader(tc.input), cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.sentinelCheck && !errors.Is(err, jsonl.ErrUnknownType) {
				t.Fatalf("error %v does not wrap jsonl.ErrUnknownType", err)
			}
		})
	}
}

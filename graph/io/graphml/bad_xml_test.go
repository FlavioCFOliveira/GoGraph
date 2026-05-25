package graphml_test

import (
	"strings"
	"testing"

	"gograph/graph/io/graphml"
)

// TestGraphMLRead_BadXML verifies that malformed and edge-case inputs
// produce the expected error behaviour and never panic.
func TestGraphMLRead_BadXML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			// Truncated mid-attribute: the XML decoder must surface a
			// syntax error rather than returning a partial result.
			name:    "truncated",
			input:   `<graphml><graph edgedefault="directed"><node id="`,
			wantErr: true,
		},
		{
			// Plain text is not XML; the decoder returns EOF.
			name:    "invalid_xml",
			input:   "not xml at all",
			wantErr: true,
		},
		{
			// Valid XML structure but the weight data element carries a
			// non-integer value. The reader must propagate the parse
			// error rather than silently defaulting to zero.
			name: "malformed_weight",
			input: `<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` +
				`<key id="w" for="edge" attr.name="weight" attr.type="long"/>` +
				`<graph edgedefault="directed">` +
				`<node id="a"/><node id="b"/>` +
				`<edge source="a" target="b"><data key="w">not-a-number</data></edge>` +
				`</graph></graphml>`,
			wantErr: true,
		},
		{
			// Empty input: the XML decoder returns EOF before reaching
			// the root element.
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			// Wrong root element: the struct decoder expects <graphml>
			// and returns an error when it finds <html>.
			name:    "wrong_root",
			input:   "<html><body></body></html>",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			_, _, err := graphml.ReadInto(strings.NewReader(tc.input))
			if tc.wantErr && err == nil {
				t.Fatalf("ReadInto(%q): expected error, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ReadInto(%q): unexpected error: %v", tc.name, err)
			}
		})
	}
}

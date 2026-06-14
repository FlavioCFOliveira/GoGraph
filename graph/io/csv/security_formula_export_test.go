package csv_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestSec_IO_CSVExportFormulaInjection covers the CSV-export formula
// (a.k.a. CSV / spreadsheet) injection finding reported as #1471 (CWE-1236).
//
// When a node id or edge endpoint begins with one of the spreadsheet
// formula-trigger characters '=', '+', '-', or '@', a spreadsheet program
// (Excel, LibreOffice Calc, Google Sheets) that opens the exported CSV will
// interpret that cell as a formula rather than text — enabling command
// execution or data exfiltration when the file is opened by a victim.
//
// The fix is the opt-in csv.Options.SanitizeFormulae flag. This test pins
// both halves of its contract:
//
//   - SanitizeFormulae=true neutralises each payload by prefixing a single
//     apostrophe, so the spreadsheet renders the cell as text.
//   - SanitizeFormulae off (the default) leaves the export byte-identical to
//     the verbatim form, preserving the lossless round-trip through
//     csv.ReadInto. encoding/csv still quotes a field only when it contains
//     the delimiter, a quote, or a newline.
func TestSec_IO_CSVExportFormulaInjection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		dst  string
		// wantVerbatim is the exact row the writer emits with the default
		// (sanitisation off) options.
		wantVerbatim string
		// wantSanitised is the exact row the writer emits with
		// SanitizeFormulae enabled.
		wantSanitised string
	}{
		{
			name:          "equals_command",
			src:           "=cmd|'/c calc'!A1",
			dst:           "safe",
			wantVerbatim:  "=cmd|'/c calc'!A1,safe,0",
			wantSanitised: "'=cmd|'/c calc'!A1,safe,0",
		},
		{
			name:          "plus_prefix",
			src:           "+SUM(1+1)",
			dst:           "safe",
			wantVerbatim:  "+SUM(1+1),safe,0",
			wantSanitised: "'+SUM(1+1),safe,0",
		},
		{
			name:          "minus_prefix",
			src:           "-2+3+cmd",
			dst:           "safe",
			wantVerbatim:  "-2+3+cmd,safe,0",
			wantSanitised: "'-2+3+cmd,safe,0",
		},
		{
			name:          "at_prefix",
			src:           "@SUM(A1)",
			dst:           "safe",
			wantVerbatim:  "@SUM(A1),safe,0",
			wantSanitised: "'@SUM(A1),safe,0",
		},
		{
			name:          "payload_in_destination",
			src:           "safe",
			dst:           "=HYPERLINK(\"http://evil\")", // contains a quote → encoding/csv WILL quote it
			wantVerbatim:  `safe,"=HYPERLINK(""http://evil"")",0`,
			wantSanitised: `safe,"'=HYPERLINK(""http://evil"")",0`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Default options: the export must stay byte-identical to the
			// verbatim form so the lossless round-trip is preserved.
			t.Run("default_verbatim", func(t *testing.T) {
				t.Parallel()
				got := writeOneEdge(t, tc.src, tc.dst, csv.DefaultOptions())
				if got != tc.wantVerbatim {
					t.Errorf("default export = %q, want %q (verbatim)", got, tc.wantVerbatim)
				}
			})

			// SanitizeFormulae enabled: every formula-trigger cell is
			// neutralised with a leading apostrophe so a spreadsheet treats
			// it as text.
			t.Run("sanitised", func(t *testing.T) {
				t.Parallel()
				opts := csv.DefaultOptions()
				opts.SanitizeFormulae = true
				got := writeOneEdge(t, tc.src, tc.dst, opts)
				if got != tc.wantSanitised {
					t.Errorf("sanitised export = %q, want %q", got, tc.wantSanitised)
				}
			})
		})
	}
}

// writeOneEdge builds a one-edge directed graph and returns the single CSV
// row the writer emits under opts, with the trailing line terminator trimmed.
func writeOneEdge(t *testing.T, src, dst string, opts csv.Options) string {
	t.Helper()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(src, dst, 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var buf bytes.Buffer
	n, err := csv.Write(&buf, a, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows written = %d, want 1", n)
	}
	return strings.TrimRight(buf.String(), "\r\n")
}

// TestSec_IO_CSVExportFormulaInNodeID is the property-graph analogue: a
// node id that is itself a formula payload. It pins that the default export
// emits the dangerous id verbatim (lossless round-trip) while the opt-in
// SanitizeFormulae flag neutralises the leading '=' with an apostrophe.
func TestSec_IO_CSVExportFormulaInNodeID(t *testing.T) {
	t.Parallel()

	const payload = "=2+5+cmd|'/c calc'!A0"

	// Default: the dangerous leading '=' survives into the exported cell
	// unescaped, and the writer does NOT prefix a neutralising character.
	t.Run("default_verbatim", func(t *testing.T) {
		t.Parallel()
		out := writeNodeIDExport(t, payload, csv.DefaultOptions())
		if !strings.Contains(out, payload) {
			t.Errorf("formula node id not present verbatim in default export:\n%s", out)
		}
		if strings.HasPrefix(out, "'") || strings.HasPrefix(out, "\t") {
			t.Errorf("default export unexpectedly neutralised the leading char:\n%s", out)
		}
	})

	// SanitizeFormulae enabled: the cell is neutralised with a leading
	// apostrophe. The payload contains a single quote but no delimiter,
	// double-quote, or newline, so encoding/csv emits the field unquoted;
	// the neutralised cell therefore begins with the literal apostrophe
	// immediately followed by the original '='.
	t.Run("sanitised", func(t *testing.T) {
		t.Parallel()
		opts := csv.DefaultOptions()
		opts.SanitizeFormulae = true
		out := writeNodeIDExport(t, payload, opts)
		if !strings.HasPrefix(out, "'="+payload[1:]) {
			t.Errorf("sanitised export did not neutralise the formula node id:\n%s", out)
		}
	})
}

// writeNodeIDExport builds a one-edge graph whose source id is the given
// payload and returns the full CSV export under opts.
func writeNodeIDExport(t *testing.T, payload string, opts csv.Options) string {
	t.Helper()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(payload, "target", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var buf bytes.Buffer
	if _, err := csv.Write(&buf, a, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.String()
}

// TestSec_IO_CSVSanitiseRoundtripUnaffected is the explicit guard for the
// hard constraint behind #1471: enabling SanitizeFormulae must not perturb
// the default path. A graph written with DefaultOptions re-imports
// byte-identically, which the sanitising option (off by default) leaves
// untouched.
func TestSec_IO_CSVSanitiseRoundtripUnaffected(t *testing.T) {
	t.Parallel()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range []struct {
		src, dst string
		w        int64
	}{
		{"=danger", "safe", 1},
		{"+danger", "safe", -2}, // negative weight leads with '-'
		{"plain", "@danger", 3},
	} {
		if err := a.AddEdge(e.src, e.dst, e.w); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}

	var verbatim bytes.Buffer
	if _, err := csv.Write(&verbatim, a, csv.DefaultOptions()); err != nil {
		t.Fatalf("default Write: %v", err)
	}

	// The default export round-trips: re-importing it yields the same edges.
	b, _, err := csv.ReadInto(bytes.NewReader(verbatim.Bytes()), csv.DefaultOptions())
	if err != nil {
		t.Fatalf("re-import of default export: %v", err)
	}
	if !b.HasEdge("=danger", "safe") || !b.HasEdge("+danger", "safe") || !b.HasEdge("plain", "@danger") {
		t.Fatalf("default export lost an edge on re-import")
	}

	// Sanitised export differs (it must, or it would not be neutralising),
	// confirming the option is a real, opt-in behaviour change.
	opts := csv.DefaultOptions()
	opts.SanitizeFormulae = true
	var sanitised bytes.Buffer
	if _, err := csv.Write(&sanitised, a, opts); err != nil {
		t.Fatalf("sanitised Write: %v", err)
	}
	if verbatim.String() == sanitised.String() {
		t.Fatalf("sanitised export is byte-identical to verbatim; option had no effect")
	}
}

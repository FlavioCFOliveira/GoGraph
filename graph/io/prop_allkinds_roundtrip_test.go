// Package io_test provides a cross-package gate test that verifies all
// seven PropertyKind values survive a WriteWithProps→ReadWithProps
// round-trip through both the JSONL and GraphML encoders.
//
// This test is designed so that it FAILS before the PropList fix is
// applied (encodePropertyValue and decodePropertyValue in the JSONL
// package, and graphMLAttrType / serialisePropertyValue /
// deserialisePropertyValue in the GraphML package).
package io_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// propCase describes one property that must round-trip correctly.
type propCase struct {
	key   string
	value lpg.PropertyValue
}

// allKindCases returns one propCase per PropertyKind; the list and its
// scalar elements exercise every code branch.
func allKindCases() []propCase {
	now := time.Date(2025, 3, 15, 10, 30, 0, 500, time.UTC)
	listElems := []lpg.PropertyValue{
		lpg.StringValue("elem0"),
		lpg.Int64Value(99),
		lpg.BoolValue(false),
	}
	return []propCase{
		{"str", lpg.StringValue("hello world")},
		{"count", lpg.Int64Value(-9876543210)},
		{"score", lpg.Float64Value(2.718281828)},
		{"flag", lpg.BoolValue(true)},
		{"stamp", lpg.TimeValue(now)},
		{"blob", lpg.BytesValue([]byte{0xCA, 0xFE, 0xBA, 0xBE})},
		{"tags", lpg.ListValue(listElems)},
	}
}

// buildGraph creates an lpg.Graph with node "n" carrying all kind
// variants as properties.
func buildGraph(t *testing.T, cases []propCase) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for _, c := range cases {
		if err := g.SetNodeProperty("n", c.key, c.value); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", c.key, err)
		}
	}
	return g
}

// checkPropValue asserts that got matches want by kind and typed value.
// For PropList it recurses element-wise.
func checkPropValue(t *testing.T, label string, want, got lpg.PropertyValue) {
	t.Helper()
	if got.Kind() != want.Kind() {
		t.Errorf("%s: kind = %v, want %v", label, got.Kind(), want.Kind())
		return
	}
	switch want.Kind() {
	case lpg.PropString:
		ws, _ := want.String()
		gs, _ := got.String()
		if ws != gs {
			t.Errorf("%s: string = %q, want %q", label, gs, ws)
		}
	case lpg.PropInt64:
		wi, _ := want.Int64()
		gi, _ := got.Int64()
		if wi != gi {
			t.Errorf("%s: int64 = %d, want %d", label, gi, wi)
		}
	case lpg.PropFloat64:
		wf, _ := want.Float64()
		gf, _ := got.Float64()
		if wf != gf {
			t.Errorf("%s: float64 = %v, want %v", label, gf, wf)
		}
	case lpg.PropBool:
		wb, _ := want.Bool()
		gb, _ := got.Bool()
		if wb != gb {
			t.Errorf("%s: bool = %v, want %v", label, gb, wb)
		}
	case lpg.PropTime:
		wt, _ := want.Time()
		gt, _ := got.Time()
		if !wt.Equal(gt) {
			t.Errorf("%s: time = %v, want %v", label, gt, wt)
		}
	case lpg.PropBytes:
		wb, _ := want.Bytes()
		gb, _ := got.Bytes()
		if len(wb) != len(gb) {
			t.Errorf("%s: bytes len = %d, want %d", label, len(gb), len(wb))
			return
		}
		for i := range wb {
			if wb[i] != gb[i] {
				t.Errorf("%s: bytes[%d] = 0x%02X, want 0x%02X", label, i, gb[i], wb[i])
			}
		}
	case lpg.PropList:
		wl, _ := want.List()
		gl, _ := got.List()
		if len(wl) != len(gl) {
			t.Errorf("%s: list len = %d, want %d", label, len(gl), len(wl))
			return
		}
		for i := range wl {
			checkPropValue(t, fmt.Sprintf("%s[%d]", label, i), wl[i], gl[i])
		}
	default:
		t.Errorf("%s: unexpected kind %v", label, want.Kind())
	}
}

// TestAllKindsRoundtrip_JSONL verifies that all seven PropertyKinds
// survive a JSONL WriteWithProps→ReadWithProps round-trip.
// Before the PropList fix this test will fail because encodePropertyValue
// emits kind="unknown" and decodePropertyValue rejects "unknown".
func TestAllKindsRoundtrip_JSONL(t *testing.T) {
	t.Parallel()
	cases := allKindCases()
	g := buildGraph(t, cases)

	var buf bytes.Buffer
	if _, err := jsonl.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}

	g2, _, err := jsonl.ReadWithProps(strings.NewReader(buf.String()), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}

	for _, c := range cases {
		got, ok := g2.GetNodeProperty("n", c.key)
		if !ok {
			t.Errorf("JSONL round-trip: property %q missing", c.key)
			continue
		}
		checkPropValue(t, "JSONL/"+c.key, c.value, got)
	}
}

// TestAllKindsRoundtrip_GraphML verifies that all seven PropertyKinds
// survive a GraphML WriteWithProps→ReadWithProps round-trip.
// Before the fix, PropTime and PropBytes are downgraded to PropString,
// and PropList falls to fmt.Sprintf garbage (or is missing).
func TestAllKindsRoundtrip_GraphML(t *testing.T) {
	t.Parallel()
	cases := allKindCases()
	g := buildGraph(t, cases)

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}

	g2, _, err := graphml.ReadWithProps(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}

	for _, c := range cases {
		got, ok := g2.GetNodeProperty("n", c.key)
		if !ok {
			t.Errorf("GraphML round-trip: property %q missing", c.key)
			continue
		}
		checkPropValue(t, "GraphML/"+c.key, c.value, got)
	}
}

// TestNonFiniteFloats_RoundTripAndLexical verifies that NaN, +Inf, and
// -Inf float properties round-trip through both encoders, and that GraphML
// emits the portable xs:double lexical forms (INF / -INF / NaN) rather
// than Go's non-conformant "+Inf"/"-Inf" text (#1440).
func TestNonFiniteFloats_RoundTripAndLexical(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key string
		val float64
	}{
		{"pinf", math.Inf(1)},
		{"ninf", math.Inf(-1)},
		{"nan", math.NaN()},
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for _, c := range cases {
		if err := g.SetNodeProperty("n", c.key, lpg.Float64Value(c.val)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", c.key, err)
		}
	}

	assertFloat := func(t *testing.T, label string, want float64, got lpg.PropertyValue) {
		t.Helper()
		if got.Kind() != lpg.PropFloat64 {
			t.Fatalf("%s: kind = %v, want PropFloat64", label, got.Kind())
		}
		gf, _ := got.Float64()
		switch {
		case math.IsNaN(want):
			if !math.IsNaN(gf) {
				t.Errorf("%s: got %v, want NaN", label, gf)
			}
		case gf != want:
			t.Errorf("%s: got %v, want %v", label, gf, want)
		}
	}

	// GraphML: lexical conformance + round-trip.
	var gbuf bytes.Buffer
	if err := graphml.WriteWithProps(&gbuf, g); err != nil {
		t.Fatalf("GraphML WriteWithProps: %v", err)
	}
	gout := gbuf.String()
	for _, form := range []string{"INF", "-INF", "NaN"} {
		if !strings.Contains(gout, form) {
			t.Errorf("GraphML output missing xs:double form %q:\n%s", form, gout)
		}
	}
	if strings.Contains(gout, "Inf") { // Go's "+Inf"/"-Inf" both contain "Inf"
		t.Errorf("GraphML output contains non-portable Go Inf text:\n%s", gout)
	}
	g2, _, err := graphml.ReadWithProps(bytes.NewReader(gbuf.Bytes()))
	if err != nil {
		t.Fatalf("GraphML ReadWithProps: %v", err)
	}
	for _, c := range cases {
		got, ok := g2.GetNodeProperty("n", c.key)
		if !ok {
			t.Errorf("GraphML round-trip: property %q missing", c.key)
			continue
		}
		assertFloat(t, "GraphML/"+c.key, c.val, got)
	}

	// JSONL: documented contract round-trip (Go strconv string form).
	var jbuf bytes.Buffer
	if _, err := jsonl.WriteWithProps(&jbuf, g); err != nil {
		t.Fatalf("JSONL WriteWithProps: %v", err)
	}
	g3, _, err := jsonl.ReadWithProps(strings.NewReader(jbuf.String()), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("JSONL ReadWithProps: %v", err)
	}
	for _, c := range cases {
		got, ok := g3.GetNodeProperty("n", c.key)
		if !ok {
			t.Errorf("JSONL round-trip: property %q missing", c.key)
			continue
		}
		assertFloat(t, "JSONL/"+c.key, c.val, got)
	}
}

// TestControlAndBinaryStrings_NoSilentCorruption verifies that a string
// property carrying bytes XML 1.0 cannot represent is never silently
// corrupted to U+FFFD. GraphML must fail fast with the typed
// graphml.ErrInvalidXMLChar (fail-stop, integrity preserved); JSONL must
// round-trip the exact bytes whenever they are valid UTF-8 (#1437).
func TestControlAndBinaryStrings_NoSilentCorruption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		raw           string
		jsonlLossless bool // valid UTF-8 → JSONL must round-trip exactly
	}{
		{"c0-controls", "ctrl\x01\x02\x1f", true},
		{"nul-byte", "a\x00b", true},
		{"invalid-utf8", "bin\xff\xfe", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := buildGraph(t, []propCase{{"payload", lpg.StringValue(tc.raw)}})

			// GraphML: typed error, and never a silent U+FFFD substitution.
			var gbuf bytes.Buffer
			err := graphml.WriteWithProps(&gbuf, g)
			if !errors.Is(err, graphml.ErrInvalidXMLChar) {
				t.Fatalf("GraphML WriteWithProps err = %v, want ErrInvalidXMLChar", err)
			}
			if strings.ContainsRune(gbuf.String(), '�') {
				t.Errorf("GraphML emitted a U+FFFD substitution: %q", gbuf.String())
			}

			// JSONL: lossless for valid-UTF-8 inputs.
			if tc.jsonlLossless {
				var jbuf bytes.Buffer
				if _, err := jsonl.WriteWithProps(&jbuf, g); err != nil {
					t.Fatalf("JSONL WriteWithProps: %v", err)
				}
				g2, _, err := jsonl.ReadWithProps(strings.NewReader(jbuf.String()), adjlist.Config{Directed: true})
				if err != nil {
					t.Fatalf("JSONL ReadWithProps: %v", err)
				}
				got, ok := g2.GetNodeProperty("n", "payload")
				if !ok {
					t.Fatal("JSONL round-trip: property payload missing")
				}
				checkPropValue(t, "JSONL/"+tc.name, lpg.StringValue(tc.raw), got)
			}
		})
	}
}

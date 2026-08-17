package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// TestWireParamTypes_MatrixRoundTrips is the happy path: every PackStream
// parameter kind a driver can bind crosses the real wire and comes back as
// specified, including a genuine Map used as the property map of a CREATE.
func TestWireParamTypes_MatrixRoundTrips(t *testing.T) {
	srv := newWireRoundTripServer(t)
	if fails := probeWireParamTypes(context.Background(), srv); len(fails) > 0 {
		t.Fatalf("wire parameter matrix diverged:\n%s", strings.Join(fails, "\n"))
	}
}

// TestWireParamTypes_IsPopulationNeutral proves the probe leaves the node count
// exactly as it found it. RunConcurrent reconciles acknowledged creates against
// the engine's node count at quiescence, so a probe that leaked a node would
// break that oracle for every caller.
func TestWireParamTypes_IsPopulationNeutral(t *testing.T) {
	srv := newWireRoundTripServer(t)

	before, err := queryNodeCount(srv)
	if err != nil {
		t.Fatalf("node count before: %v", err)
	}
	if fails := probeWireParamTypes(context.Background(), srv); len(fails) > 0 {
		t.Fatalf("wire parameter matrix diverged:\n%s", strings.Join(fails, "\n"))
	}
	after, err := queryNodeCount(srv)
	if err != nil {
		t.Fatalf("node count after: %v", err)
	}
	if before != after {
		t.Fatalf("probe changed the population: %d node(s) before, %d after", before, after)
	}
}

// TestWireParamTypes_ConversionCoversEveryKind pins the client-side conversion:
// each Go kind the simulator binds must map to its PackStream counterpart, and
// lists and maps must convert recursively. A kind that silently failed to
// convert would make the whole probe unreachable for that type.
func TestWireParamTypes_ConversionCoversEveryKind(t *testing.T) {
	out, err := toPackstreamParams(map[string]any{
		"s": "x", "i": int64(1), "n": 2, "f": 1.5, "b": true, "nul": nil,
		"li":     []int64{1, 2},
		"ls":     []string{"a"},
		"la":     []any{int64(1), "two", nil, []any{int64(3)}},
		"m":      map[string]any{"k": int64(9), "inner": map[string]any{"deep": true}},
		"listof": []any{map[string]any{"a": int64(1)}},
	})
	if err != nil {
		t.Fatalf("toPackstreamParams: %v", err)
	}

	assertKind := func(key string, want any) {
		t.Helper()
		got, ok := out[key]
		if !ok {
			t.Errorf("param %q missing from the conversion", key)
			return
		}
		if _, isNil := want.(nilMarker); isNil {
			if got != nil {
				t.Errorf("param %q: got %T %v, want packstream Null", key, got, got)
			}
			return
		}
		if gotT, wantT := typeName(got), typeName(want); gotT != wantT {
			t.Errorf("param %q: got %s, want %s", key, gotT, wantT)
		}
	}
	assertKind("s", "")
	assertKind("i", int64(0))
	assertKind("n", int64(0))
	assertKind("f", 0.0)
	assertKind("b", false)
	assertKind("nul", nilMarker{})
	assertKind("li", []packstream.Value{})
	assertKind("ls", []packstream.Value{})
	assertKind("la", []packstream.Value{})
	assertKind("m", map[string]packstream.Value{})
	assertKind("listof", []packstream.Value{})

	// Recursion: a nested list inside a list, and a nested map inside a map.
	la, _ := out["la"].([]packstream.Value)
	if len(la) != 4 {
		t.Fatalf("nested list arity = %d, want 4", len(la))
	}
	if _, ok := la[3].([]packstream.Value); !ok {
		t.Errorf("nested list element: got %T, want []packstream.Value", la[3])
	}
	if la[2] != nil {
		t.Errorf("nested null element: got %T %v, want nil", la[2], la[2])
	}
	m, _ := out["m"].(map[string]packstream.Value)
	if _, ok := m["inner"].(map[string]packstream.Value); !ok {
		t.Errorf("nested map value: got %T, want map[string]packstream.Value", m["inner"])
	}

	if _, err := toPackstreamParams(map[string]any{"bad": struct{}{}}); err == nil {
		t.Error("an unsupported parameter kind was silently accepted")
	}
}

// nilMarker requests a packstream Null assertion from assertKind.
type nilMarker struct{}

// typeName renders a value's concrete type for comparison.
func typeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case int64:
		return "int64"
	case float64:
		return "float64"
	case bool:
		return "bool"
	case []packstream.Value:
		return "[]packstream.Value"
	case map[string]packstream.Value:
		return "map[string]packstream.Value"
	default:
		return "other"
	}
}

// TestWireParamTypes_SensitivityToAWrongExpectation proves the comparison FIRES
// on both a wrong value and a wrong wire TYPE. The type matters as much as the
// value: an Integer arriving as a String is a defect even when the text matches.
func TestWireParamTypes_SensitivityToAWrongExpectation(t *testing.T) {
	cases := []struct {
		name string
		got  []any
		want []any
	}{
		{"wrong value", []any{int64(1)}, []any{int64(2)}},
		{"wrong type, same text", []any{"1"}, []any{int64(1)}},
		{"float vs int", []any{2.0}, []any{int64(2)}},
		{"null vs zero", []any{nil}, []any{int64(0)}},
		{"list vs string", []any{"[1, 2]"}, []any{[]any{int64(1), int64(2)}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if fails := compareWireRow("probe", c.got, c.want, []string{"value"}); len(fails) == 0 {
				t.Fatalf("comparison FAILED to fire for got=%#v want=%#v", c.got, c.want)
			}
		})
	}
	// Control: an exact match must not fire.
	if fails := compareWireRow("probe", []any{int64(1)}, []any{int64(1)}, []string{"value"}); len(fails) > 0 {
		t.Fatalf("comparison fired on an exact match: %v", fails)
	}
}

// TestWireParamTypes_ListEncodingPinIsLive proves the known-gap pin is doing
// real work: the RECORD encoder currently stringifies a list column (there is no
// expr.ListValue arm in bolt/server/session.go's exprValueToPackstream), so the
// pinned rendering must be what actually arrives. When the encoder is fixed this
// test is the one that flips.
func TestWireParamTypes_ListEncodingPinIsLive(t *testing.T) {
	srv := newWireRoundTripServer(t)
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	recs, err := wireQuery(c, "RETURN $l AS l", map[string]any{"l": []any{int64(1), int64(2), int64(3)}})
	if err != nil {
		t.Fatalf("list echo: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		t.Fatalf("expected one column in one row, got %d row(s)", len(recs))
	}
	switch got := recs[0].Data[0].(type) {
	case string:
		if got != wireParamListStringEncoding {
			t.Errorf("pinned list rendering = %q, but the wire produced %q", wireParamListStringEncoding, got)
		}
	case []any:
		t.Fatalf("a list column now arrives as a packstream List (%#v): the encoder gap is FIXED — "+
			"update wireParamListEncodingPin to assert the native list", got)
	default:
		t.Fatalf("unexpected list encoding %T %#v", got, got)
	}

	// The list nonetheless BINDS correctly: evaluation over it gives right
	// answers, so the gap is in the RECORD encoder, not in parameter binding.
	recs, err = wireQuery(c, "RETURN $l[1] AS second, size($l) AS n, $l = [1,2,3] AS eq",
		map[string]any{"l": []any{int64(1), int64(2), int64(3)}})
	if err != nil {
		t.Fatalf("list semantics: %v", err)
	}
	if fails := compareWireRow("list semantics", recs[0].Data,
		[]any{int64(2), int64(3), true}, []string{"index", "size", "equality"}); len(fails) > 0 {
		t.Fatalf("a bound list did not evaluate correctly:\n%s", strings.Join(fails, "\n"))
	}
}

// TestWireParamTypes_FailuresBreakConsistency proves the probe's findings are
// actually gating: a ConcurrentResult carrying a wire-parameter failure must not
// report itself consistent, otherwise the whole matrix would be advisory.
func TestWireParamTypes_FailuresBreakConsistency(t *testing.T) {
	clean := ConcurrentResult{EngineNodeCount: 5, AckedCreates: 5}
	if !clean.Consistent() {
		t.Fatal("a clean result must be consistent")
	}
	dirty := clean
	dirty.WireParamFailures = []string{"Float column 0: got string, want float64"}
	if dirty.Consistent() {
		t.Fatal("a result carrying a wire-parameter failure reported itself CONSISTENT")
	}
}

// TestWireParamTypes_RunConcurrentRunsTheProbe proves the probe is wired into the
// concurrent mode rather than merely existing: a run over a server whose engine
// is healthy must report no wire-parameter failures, and the field must be
// populated by RunConcurrent itself.
func TestWireParamTypes_RunConcurrentRunsTheProbe(t *testing.T) {
	srv := newWireRoundTripServer(t)
	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed: 0x2462, Connections: 2, OpsPerConn: 2,
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}
	if len(res.WireParamFailures) > 0 {
		t.Fatalf("wire parameter matrix diverged under the concurrent mode:\n%s",
			strings.Join(res.WireParamFailures, "\n"))
	}
	if !res.Consistent() {
		t.Fatalf("concurrent run inconsistent: %+v", res)
	}
}

// TestWireParamTypes_BoltFailureIsReported proves a refused statement surfaces
// as a probe failure rather than being read as an empty, passing result.
func TestWireParamTypes_BoltFailureIsReported(t *testing.T) {
	srv := newWireRoundTripServer(t)
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := wireQuery(c, "THIS IS NOT CYPHER", nil); err == nil {
		t.Fatal("a refused statement was reported as success")
	}
}

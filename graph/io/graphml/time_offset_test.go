package graphml

// time_offset_test.go — regression gate for the 2026-06-25 round-2 audit
// (#1769): a PropTime carrying a non-UTC zone offset must round-trip through
// WriteWithProps→ReadWithProps preserving BOTH the instant AND the offset, not
// silently normalised to UTC. (The pre-existing TypedPropsRoundtrip test only
// asserts .Equal(), i.e. the instant, so it could not catch the offset loss.)

import (
	"bytes"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestGraphML_TimeOffset_RoundTrips(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("IST", 5*3600+30*60) // +05:30
	want := time.Date(2025, 3, 15, 10, 30, 0, 123456789, loc)

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "stamp", lpg.TimeValue(want)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	var buf bytes.Buffer
	if err := WriteWithProps(&buf, g); err != nil {
		t.Fatalf("WriteWithProps: %v", err)
	}
	got, _, err := ReadWithProps(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWithProps: %v", err)
	}
	v, ok := got.NodeProperties("n")["stamp"]
	if !ok {
		t.Fatal("missing property 'stamp' after round-trip")
	}
	ts, ok := v.Time()
	if !ok {
		t.Fatalf("stamp kind = %v, want a time value", v.Kind())
	}
	if got, wantStr := ts.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano); got != wantStr {
		t.Errorf("offset not preserved: got %s, want %s", got, wantStr)
	}
}

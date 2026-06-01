package shapegen

import (
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestNilShape_BuildRoundTrip checks the canonical placeholder Shape
// against its own metadata: it has the documented name, no knobs,
// and Build returns a fresh empty graph on the requested adjlist
// configuration.
func TestNilShape_BuildRoundTrip(t *testing.T) {
	t.Parallel()

	var s Shape[string, int64] = NilShape{}

	if got, want := s.Name(), "nil"; got != want {
		t.Fatalf("NilShape.Name() = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("NilShape.Knobs() = %#v, want empty", got)
	}

	cfg := adjlist.Config{Directed: true}
	g, err := s.Build(cfg)
	if err != nil {
		t.Fatalf("NilShape.Build returned error: %v", err)
	}
	if g == nil {
		t.Fatal("NilShape.Build returned nil graph")
	}
	if got := g.AdjList().Order(); got != 0 {
		t.Fatalf("fresh NilShape graph Order() = %d, want 0", got)
	}
	if got := g.AdjList().Size(); got != 0 {
		t.Fatalf("fresh NilShape graph Size() = %d, want 0", got)
	}
	if !g.AdjList().Directed() {
		t.Fatal("NilShape.Build did not honour cfg.Directed")
	}
}

// TestRegistry_RegisterLookupUnregister exercises the registry
// surface end to end, including the duplicate-registration guard and
// the empty-name rejection.
func TestRegistry_RegisterLookupUnregister(t *testing.T) {
	t.Parallel()

	if err := Register[string, int64](NilShape{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { Unregister[string, int64]("nil") })

	got, ok := Lookup[string, int64]("nil")
	if !ok {
		t.Fatal("Lookup did not find newly registered NilShape")
	}
	if got.Name() != "nil" {
		t.Fatalf("Lookup returned Shape with Name=%q, want %q", got.Name(), "nil")
	}

	// Duplicate registration for the same specialisation must fail.
	if err := Register[string, int64](NilShape{}); err == nil {
		t.Fatal("duplicate Register returned nil error, want failure")
	}

	// A different specialisation may use the same name.
	var alt Shape[uint64, float64] = stubShape{name: "nil"}
	if err := Register[uint64, float64](alt); err != nil {
		t.Fatalf("Register on a distinct specialisation: %v", err)
	}
	t.Cleanup(func() { Unregister[uint64, float64]("nil") })

	if _, ok := Lookup[uint64, float64]("nil"); !ok {
		t.Fatal("Lookup did not find Shape registered under disjoint specialisation")
	}

	// Empty name and nil Shape are both rejected.
	if err := Register[uint64, float64](stubShape{name: ""}); err == nil {
		t.Fatal("Register with empty Name returned nil error")
	}
	var nilShape Shape[string, int64]
	if err := Register[string, int64](nilShape); err == nil {
		t.Fatal("Register with nil Shape returned nil error")
	}

	// Unregister followed by Lookup returns false.
	Unregister[string, int64]("nil")
	if _, ok := Lookup[string, int64]("nil"); ok {
		t.Fatal("Lookup found Shape after Unregister")
	}

	// Unregister on a missing name is a no-op (must not panic).
	Unregister[string, int64]("does-not-exist")
}

// TestMakeKnobValues_PropertyBased checks that MakeKnobValues honours
// the declared bounds of every knob for any draw rapid produces, and
// that the returned slice has the same length and order as the knob
// slice.
func TestMakeKnobValues_PropertyBased(t *testing.T) {
	t.Parallel()

	knobs := []Knob{
		{Name: "nodes", Min: 0, Max: 64, Default: 8},
		{Name: "edges", Min: 1, Max: 256, Default: 16},
		{Name: "fanout", Min: 2, Max: 8, Default: 4},
	}

	rapid.Check(t, func(r *rapid.T) {
		values := MakeKnobValues(r, knobs)
		if len(values) != len(knobs) {
			t.Fatalf("len(values)=%d, len(knobs)=%d", len(values), len(knobs))
		}
		for i, v := range values {
			k := knobs[i]
			if v < k.Min || v > k.Max {
				t.Fatalf("knob[%d]=%q drew %d outside [%d, %d]", i, k.Name, v, k.Min, k.Max)
			}
		}
	})
}

// TestMakeKnobValues_NilRapidPanics asserts that MakeKnobValues
// surfaces a misuse (nil *rapid.T) as a panic rather than a silent
// nil dereference deeper in rapid.
func TestMakeKnobValues_NilRapidPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MakeKnobValues(nil, ...) did not panic")
		}
	}()
	_ = MakeKnobValues(nil, []Knob{{Name: "x", Min: 0, Max: 1, Default: 0}})
}

// TestMakeKnobValues_BadBoundsPanics asserts that MakeKnobValues
// rejects a Knob with Max < Min as a programmer error.
func TestMakeKnobValues_BadBoundsPanics(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(r *rapid.T) {
		defer func() {
			if rec := recover(); rec == nil {
				t.Fatal("MakeKnobValues did not panic on inverted bounds")
			}
		}()
		_ = MakeKnobValues(r, []Knob{{Name: "bad", Min: 10, Max: 1, Default: 5}})
	})
}

// stubShape is a tiny [Shape] used by registry tests where the
// behaviour of Build is irrelevant — only the identity and Name
// matter. It is deliberately kept inside the _test.go file so it
// does not leak into the package's public surface.
type stubShape struct {
	name string
}

func (s stubShape) Name() string { return s.name }

func (s stubShape) Build(cfg adjlist.Config) (*lpg.Graph[uint64, float64], error) {
	if s.name == "" {
		return nil, errors.New("stubShape has no name")
	}
	return lpg.New[uint64, float64](cfg), nil
}

func (s stubShape) Knobs() []Knob { return nil }

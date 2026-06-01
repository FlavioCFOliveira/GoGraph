// Package shapegen defines a uniform contract for graph-shape
// generators used across property-based tests, golden corpora, and
// benchmarks in GoGraph.
//
// A Shape value exposes the canonical Go construction of a particular
// graph topology over the LPG and adjlist backends. It reports its
// parameter knobs so that property-based tests can sweep them with
// pgregory.net/rapid and so that goldens can pin specific
// configurations. The Build method is fully parameterised by an
// [adjlist.Config] and by knob values supplied to the Shape
// implementation through its own constructor; no Shape consults
// hidden process-level state.
//
// # Concurrency
//
// Shape implementations must be safe for concurrent Build calls. The
// Build method must be a pure function of its inputs and the supplied
// adjlist.Config: it must not consult, mutate, or close over any
// package-level state. The shapegen registry itself is safe for
// concurrent Register/Lookup and may be queried from any number of
// goroutines.
//
// # Determinism
//
// Build calls must be reproducible from their parameters and the seed
// implied by knob values. Shapes that internally need pseudo-random
// choices derive them from the knob vector or from caller-supplied
// material; they must never consult global random sources directly.
//
// # Registry
//
// shapegen maintains a typed, process-local registry keyed by Shape
// Name(). It is the only piece of mutable package-level state in the
// package, guarded by a sync.RWMutex. Tests that register transient
// shapes must clean up via t.Cleanup to keep the registry tidy.
package shapegen

import (
	"fmt"
	"sync"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Knob describes one integer-valued tunable parameter exposed by a
// Shape. Property-based tests sweep knobs by drawing values uniformly
// from [Min, Max]; goldens and benchmarks pin Default.
//
// Knobs are intentionally restricted to integers. Non-integer
// parameters (for example, an edge density expressed as a float)
// should be derived inside Shape.Build from an integer knob — for
// instance a percent-of-max knob with Min=0, Max=100.
type Knob struct {
	// Name uniquely identifies the knob within its Shape. It is used
	// as the label passed to rapid.Generator.Draw, so the same Shape
	// must never declare two knobs with the same Name.
	Name string

	// Min is the inclusive lower bound for the knob value.
	Min int

	// Max is the inclusive upper bound for the knob value. It must
	// satisfy Max >= Min.
	Max int

	// Default is the value used by goldens and benchmarks when no
	// property-based sweep is in effect. It must satisfy
	// Min <= Default <= Max.
	Default int
}

// Shape is the canonical contract every graph-shape generator
// implements. It is generic on the node type N and the edge weight
// type W so it can produce graphs compatible with any LPG
// specialisation in the module.
//
// Name returns a stable, package-unique identifier — used as the
// registry key, as the prefix in golden filenames, and as a label in
// benchmark reports.
//
// Build constructs a fresh *lpg.Graph[N, W] using cfg for the
// underlying adjacency-list backend. Build must be safe to call
// concurrently with itself and with calls to Build on any other Shape
// instance.
//
// Knobs returns the set of tunable parameters exposed by the Shape.
// The slice ordering must be stable across calls; tests rely on it
// to map drawn knob values back to their semantic role.
type Shape[N comparable, W any] interface {
	Name() string
	Build(cfg adjlist.Config) (*lpg.Graph[N, W], error)
	Knobs() []Knob
}

// MakeKnobValues draws one integer for each knob in knobs using the
// supplied rapid.T, in declaration order. Each draw uses the knob
// Name as its rapid label so that shrunk-and-reported counterexamples
// are self-describing.
//
// The returned slice has the same length and order as knobs.
// MakeKnobValues panics if any Knob has Min > Max; this indicates a
// programmer error in the Shape declaring the knob.
func MakeKnobValues(g *rapid.T, knobs []Knob) []int {
	if g == nil {
		panic("shapegen: MakeKnobValues called with nil *rapid.T")
	}
	out := make([]int, len(knobs))
	for i, k := range knobs {
		if k.Max < k.Min {
			panic(fmt.Sprintf("shapegen: knob %q has Max(%d) < Min(%d)", k.Name, k.Max, k.Min))
		}
		out[i] = rapid.IntRange(k.Min, k.Max).Draw(g, k.Name)
	}
	return out
}

// registry is the package-local typed registry shared by all Shape
// specialisations. Each Shape instance is stored as an any inside a
// keyType-bucketed map so that Lookup can recover the original
// generic type without sacrificing dispatch type safety.
//
// The registry is the only piece of mutable package-level state in
// shapegen. It is guarded by mu and exposes only the Register /
// Lookup / Unregister surface defined below.
var registry struct {
	mu sync.RWMutex
	// shapes maps a (typeKey, name) pair to the boxed Shape value.
	// typeKey is produced by typeKeyFor[N, W] so distinct generic
	// specialisations occupy disjoint key spaces.
	shapes map[registryKey]any
}

// registryKey pairs a generic-type discriminator with a Shape name.
type registryKey struct {
	typeKey string
	name    string
}

// typeKeyFor returns a stable string identifying the (N, W) pair. It
// is built from the fully qualified type name of a zero-valued
// *[N, W] sentinel constructed via fmt — sufficient to disambiguate
// the specialisations the module actually uses without pulling in
// reflect on the hot path of Build.
func typeKeyFor[N comparable, W any]() string {
	var n N
	var w W
	return fmt.Sprintf("%T|%T", n, w)
}

// Register installs s in the package-local registry under its
// Name() and the (N, W) specialisation it inhabits. It returns an
// error if a Shape with the same Name has already been registered
// for the same specialisation, or if s.Name() is empty. Registration
// of distinct specialisations under the same name is allowed and
// expected — for example, "nil" may exist for both
// (string, int64) and (uint64, float64).
func Register[N comparable, W any](s Shape[N, W]) error {
	if s == nil {
		return fmt.Errorf("shapegen: Register received nil Shape")
	}
	name := s.Name()
	if name == "" {
		return fmt.Errorf("shapegen: Shape with empty Name cannot be registered")
	}
	key := registryKey{typeKey: typeKeyFor[N, W](), name: name}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.shapes == nil {
		registry.shapes = make(map[registryKey]any)
	}
	if _, exists := registry.shapes[key]; exists {
		return fmt.Errorf("shapegen: Shape %q already registered for specialisation %s", name, key.typeKey)
	}
	registry.shapes[key] = s
	return nil
}

// Lookup retrieves a previously registered Shape by its name within
// the (N, W) specialisation inferred from the call site. The second
// return value reports whether a Shape was found.
func Lookup[N comparable, W any](name string) (Shape[N, W], bool) {
	key := registryKey{typeKey: typeKeyFor[N, W](), name: name}

	registry.mu.RLock()
	boxed, ok := registry.shapes[key]
	registry.mu.RUnlock()
	if !ok {
		return nil, false
	}
	s, ok := boxed.(Shape[N, W])
	if !ok {
		// Defensive: only reachable if a future code path inserts
		// the wrong dynamic type under a key. The package keeps the
		// invariant by construction in Register.
		return nil, false
	}
	return s, true
}

// Unregister removes the Shape registered under name for the
// (N, W) specialisation. It is a no-op when no such Shape exists.
// Tests that register transient Shapes should call Unregister from
// a t.Cleanup hook.
func Unregister[N comparable, W any](name string) {
	key := registryKey{typeKey: typeKeyFor[N, W](), name: name}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.shapes == nil {
		return
	}
	delete(registry.shapes, key)
}

// NilShape is the canonical placeholder Shape used to bootstrap the
// shapegen self-test. It declares no knobs and Build always returns
// a fresh empty *lpg.Graph[string, int64] configured by cfg. Real
// graph topologies (path, cycle, complete, etc.) land in later
// sprints and follow the same Shape contract.
type NilShape struct{}

// Name returns the canonical placeholder name.
func (NilShape) Name() string { return "nil" }

// Build returns an empty LPG specialised on (string, int64).
func (NilShape) Build(cfg adjlist.Config) (*lpg.Graph[string, int64], error) {
	return lpg.New[string, int64](cfg), nil
}

// Knobs reports that NilShape has no tunable parameters.
func (NilShape) Knobs() []Knob { return nil }

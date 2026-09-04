package csrfile

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// ErrInvalidFixtureSpec reports a [FixtureSpec] that [BuildFixture] cannot
// satisfy. Test for it with errors.Is; the wrapped message names the offending
// field and its value.
//
// It exists because the specs it now covers used to be answered with a runtime
// panic and with an interning loop that exhausts memory, under a godoc that
// told the caller the function could not fail (rmp #2744, found under #2708).
// BuildFixture is exported, so an embedder reaches both.
var ErrInvalidFixtureSpec = errors.New("csrfile: invalid fixture spec")

// FixtureSpec parameterises [BuildFixture]. The same seed produces
// the same graph every time, making the harness deterministic.
//
// FixtureSpec holds only scalars and is taken by value; [BuildFixture] seeds a
// fresh generator per call from Seed and shares no state between calls, so one
// spec may drive any number of concurrent builds and each independently
// produces the same graph.
type FixtureSpec struct {
	// Vertices is the number of pre-interned vertex IDs. Identifiers are
	// uint32, so Vertices must not exceed [math.MaxUint32]; a larger value is
	// refused with [ErrInvalidFixtureSpec] rather than silently truncated.
	Vertices uint64
	// Edges is the number of edges to add (uniformly random
	// (src, dst) over [0, Vertices)). Edges must be zero when Vertices is
	// zero — there is no endpoint to draw — and is otherwise unbounded.
	Edges uint64
	// Seed is the PCG seed (any uint64).
	Seed uint64
	// Multigraph allows parallel edges; without it duplicates are
	// silently collapsed.
	Multigraph bool
}

// validate reports whether [BuildFixture] can build a graph that meets s,
// without building anything.
//
// The line it draws is CORRECTNESS, not cost: a spec is refused only when no
// correct graph exists for it, never because producing one would be slow or
// large. Edges is therefore unbounded — every value yields a correct graph,
// merely slowly — and the cases that merely LOOK degenerate are accepted:
// Vertices == 0 with Edges == 0 is the empty graph; Edges may exceed the
// number of distinct (src, dst) pairs, the surplus collapsing exactly as
// [FixtureSpec.Multigraph] documents; self-loops are drawn like any other
// pair; and every Seed is valid, zero included.
//
// It is a separate, total predicate rather than an inline check because the
// second rule cannot be exercised end to end: a spec at the 2^32 boundary
// needs roughly 1.1 TB of heap to build, so the boundary is only assertable
// here (see TestFixtureSpec_Validate).
func (s FixtureSpec) validate() error {
	if s.Vertices == 0 && s.Edges > 0 {
		return fmt.Errorf("%w: Edges=%d with Vertices=0; an edge has no endpoint to draw from an empty vertex set",
			ErrInvalidFixtureSpec, s.Edges)
	}
	if s.Vertices > math.MaxUint32 {
		return fmt.Errorf("%w: Vertices=%d exceeds the uint32 vertex-identifier space (max %d)",
			ErrInvalidFixtureSpec, s.Vertices, uint64(math.MaxUint32))
	}
	return nil
}

// BuildFixture deterministically builds a CSR snapshot meeting spec.
// The graph is directed and uses uint32 vertex identifiers; weights
// are absent (struct{}). Suitable for Tier 2 benchmarks and the
// crash-recovery harness, where reproducibility matters more than
// realism.
//
// # Failure modes
//
// BuildFixture returns an error wrapping [ErrInvalidFixtureSpec], before doing
// any work, for the two specs it cannot satisfy:
//
//   - Edges > 0 while Vertices == 0. There is no vertex an edge can attach to.
//     This case PANICKED with "runtime error: integer divide by zero" until rmp
//     #2744, on the modulo that reduces the generator's output into the vertex
//     range; the godoc that stood here promised the only failure mode was
//     [adjlist.ErrShardFull] "which cannot be reached", so a caller had no
//     reason to write a recover.
//   - Vertices > [math.MaxUint32]. Vertex identifiers are uint32, so a larger
//     vertex set is not representable and the conversion would truncate: at
//     exactly 2^32 the range collapses to zero and divides by zero as above,
//     and just past it the edges would be drawn over a tiny prefix of a vertex
//     set the caller asked to be enormous. Refusing up front also replaces an
//     interning loop that ends in memory exhaustion: interning was measured at
//     ~270 bytes per node at V=4e6, so 2^32 vertices needs on the order of
//     1.1 TB of heap and cannot complete on any machine GoGraph targets. It
//     was also measured at 232.7 ns per node, but that timing was taken on a
//     host running other work and is indicative only; the memory figure is
//     what makes the case.
//
// Nothing else the type admits is refused. In particular Edges is NOT bounded:
// a large value is satisfied correctly, merely slowly.
//
// No error can reach the caller from the underlying [adjlist.AdjList] on the
// default uncapped configuration: [adjlist.AdjList.AddNode] never fails, and
// [adjlist.ErrShardFull] requires an [adjlist.Config.MaxShardCapacity] that
// BuildFixture does not set. Both call sites are checked and wrapped even so,
// so an adjlist that grows a new failure mode is surfaced rather than dropped.
func BuildFixture(spec FixtureSpec) (*csr.CSR[struct{}], error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true, Multigraph: spec.Multigraph})
	for i := uint64(0); i < spec.Vertices; i++ {
		//nolint:gosec // G115: spec.validate rejected Vertices > math.MaxUint32 above, and i < spec.Vertices, so this conversion is exact
		if err := a.AddNode(uint32(i)); err != nil {
			return nil, fmt.Errorf("csrfile.BuildFixture: AddNode(%d): %w", i, err)
		}
	}
	r := rand.New(rand.NewPCG(spec.Seed, 0x9E3779B97F4A7C15)) //nolint:gosec // deterministic fixture RNG
	//nolint:gosec // G115: spec.validate rejected Vertices > math.MaxUint32, so this conversion is exact; it also rejected Vertices==0 with Edges>0, so universe is non-zero whenever the loop below runs and the modulo cannot divide by zero
	universe := uint32(spec.Vertices)
	for i := uint64(0); i < spec.Edges; i++ {
		src := uint32(r.Uint32() % universe)
		dst := uint32(r.Uint32() % universe)
		if err := a.AddEdge(src, dst, struct{}{}); err != nil {
			return nil, fmt.Errorf("csrfile.BuildFixture: AddEdge[%d]: %w", i, err)
		}
	}
	return csr.BuildFromAdjList(a), nil
}

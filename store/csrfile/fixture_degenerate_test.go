package csrfile

import (
	"errors"
	"math"
	"testing"
)

// degenerateSpecCases is the enumeration of FixtureSpec's input space that
// rmp #2744 turned up, shared by the two tests below so the predicate and the
// end-to-end builder are held to ONE table rather than two that can drift.
//
// buildable records whether a row can be driven through BuildFixture on a real
// machine. The rows with Vertices above 2^32 cannot: interning was measured at
// ~270 bytes per node at V=4e6, so V=2^32 needs on the order of 1.1 TB of heap
// and exhausts memory long before it reaches the modulo that the arithmetic
// says would divide by zero. (A companion timing of 232.7 ns per node was
// taken on a host running other work, so it is indicative only; the memory
// figure is the one that decides it.) That is why those rows are asserted by
// the PREDICATE, which is cheap and total, and not driven end to end.
var degenerateSpecCases = []struct {
	name      string
	spec      FixtureSpec
	wantRefus bool
	buildable bool
	// wantOrder/wantSize are checked only for accepted, buildable rows.
	wantOrder uint64
	wantSize  uint64
}{
	// ---- REFUSED: no correct graph exists for these specs ----
	{
		// The input reported in rmp #2744, reproduced first-hand under #2708.
		// universe = uint32(0), so `r.Uint32() % universe` divided by zero.
		name: "reported/zero-vertices-one-edge", spec: FixtureSpec{Vertices: 0, Edges: 1},
		wantRefus: true, buildable: true,
	},
	{
		name: "refuse/zero-vertices-many-edges", spec: FixtureSpec{Vertices: 0, Edges: 4096, Seed: 9},
		wantRefus: true, buildable: true,
	},
	{
		name: "refuse/zero-vertices-multigraph", spec: FixtureSpec{Vertices: 0, Edges: 1, Multigraph: true},
		wantRefus: true, buildable: true,
	},
	{
		// uint32(1<<32) == 0, so this is the zero-universe divide-by-zero
		// reached by truncation rather than by asking for it directly.
		name: "refuse/vertices-exactly-2^32", spec: FixtureSpec{Vertices: 1 << 32, Edges: 1},
		wantRefus: true, buildable: false,
	},
	{
		// uint32(1<<32+5) == 5. No panic here: the edges would be drawn over
		// [0,5) while 2^32+5 nodes were interned, so the graph built would
		// silently NOT be the graph requested. Refused for that reason.
		name: "refuse/vertices-2^32-plus-5-truncates", spec: FixtureSpec{Vertices: 1<<32 + 5, Edges: 1},
		wantRefus: true, buildable: false,
	},
	{
		// Refused even with Edges == 0: the uint32 identifier space cannot
		// hold the vertex set asked for, whatever the edge count.
		name: "refuse/vertices-above-2^32-no-edges", spec: FixtureSpec{Vertices: 1 << 32, Edges: 0},
		wantRefus: true, buildable: false,
	},
	{
		name: "refuse/vertices-max-uint64", spec: FixtureSpec{Vertices: ^uint64(0), Edges: 1},
		wantRefus: true, buildable: false,
	},

	// ---- ACCEPTED: the controls. These are legal today and must stay legal,
	// so the fix cannot be a blanket narrowing of the input space.
	{
		// The empty graph. Degenerate-LOOKING and entirely valid: the edge
		// loop never runs, so the zero universe is never divided by.
		name: "accept/empty-graph", spec: FixtureSpec{Vertices: 0, Edges: 0},
		wantRefus: false, buildable: true, wantOrder: 0, wantSize: 0,
	},
	{
		name: "accept/one-vertex-no-edges", spec: FixtureSpec{Vertices: 1, Edges: 0},
		wantRefus: false, buildable: true, wantOrder: 1, wantSize: 0,
	},
	{
		// A self-loop: src == dst is the only pair a one-vertex graph admits,
		// and adjlist supports self-loops.
		name: "accept/one-vertex-one-self-loop", spec: FixtureSpec{Vertices: 1, Edges: 1},
		wantRefus: false, buildable: true, wantOrder: 1, wantSize: 1,
	},
	{
		// Edges far exceeding the distinct pairs available. FixtureSpec's
		// godoc states duplicates are silently collapsed without Multigraph,
		// so this is documented behaviour, not a degenerate spec.
		name: "accept/edges-exceed-pairs-collapse", spec: FixtureSpec{Vertices: 1, Edges: 1000},
		wantRefus: false, buildable: true, wantOrder: 1, wantSize: 1,
	},
	{
		name: "accept/edges-exceed-pairs-multigraph", spec: FixtureSpec{Vertices: 1, Edges: 1000, Multigraph: true},
		wantRefus: false, buildable: true, wantOrder: 1, wantSize: 1000,
	},
	{
		// E >> V^2 on a simple graph: all four directed pairs over {0,1},
		// self-loops included, and nothing more.
		name: "accept/two-vertices-saturated", spec: FixtureSpec{Vertices: 2, Edges: 100},
		wantRefus: false, buildable: true, wantOrder: 2, wantSize: 4,
	},
	{
		// The spec cmd/fmtfixture drives to regenerate testdata/v1/sample.csr.
		// If the fix moved this, the committed golden file would no longer be
		// reproducible.
		name: "accept/fmtfixture-production-spec", spec: FixtureSpec{Vertices: 32, Edges: 96, Seed: 0x1337},
		wantRefus: false, buildable: true, wantOrder: 32, wantSize: 94,
	},
	{
		// Seed 0 is a legal seed: BuildFixture pairs it with a non-zero second
		// PCG word, so there is no all-zero-state hazard to guard against.
		name: "accept/seed-zero", spec: FixtureSpec{Vertices: 8, Edges: 8, Seed: 0},
		wantRefus: false, buildable: true, wantOrder: 8, wantSize: 8,
	},
	{
		// The EXACT boundary of the identifier-space rule, on the accept side.
		// uint32(math.MaxUint32) is exact, so nothing is truncated and the
		// spec is satisfiable in principle. It is not buildable here (~1.1 TB),
		// which is precisely why the predicate has to be testable on its own.
		name: "accept/vertices-exactly-max-uint32", spec: FixtureSpec{Vertices: math.MaxUint32, Edges: 0},
		wantRefus: false, buildable: false,
	},
}

// TestFixtureSpec_Validate pins the refusal predicate over the whole
// enumerated input space, including the two rows at the exact 2^32 boundary
// that no machine can build.
//
// The line the predicate draws is CORRECTNESS, not cost: a spec is refused
// only when no correct graph can be produced for it. A large Edges count is
// therefore accepted — it is satisfied correctly, merely slowly — and is not
// represented here as a refusal.
func TestFixtureSpec_Validate(t *testing.T) {
	// Deliberately NOT parallel, and neither is the end-to-end test below: if
	// the predicate ever regresses, the buildable=false rows would attempt a
	// terabyte-scale interning loop, and a sequential run makes which row did
	// that unambiguous.
	refused, accepted := 0, 0
	for _, tc := range degenerateSpecCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.validate()
			switch {
			case tc.wantRefus && err == nil:
				t.Fatalf("validate(%+v) = nil; want an error wrapping ErrInvalidFixtureSpec", tc.spec)
			case tc.wantRefus && !errors.Is(err, ErrInvalidFixtureSpec):
				t.Fatalf("validate(%+v) = %v; want errors.Is(err, ErrInvalidFixtureSpec)", tc.spec, err)
			case !tc.wantRefus && err != nil:
				t.Fatalf("validate(%+v) = %v; want nil (this spec is legal and must stay legal)", tc.spec, err)
			}
			if tc.wantRefus {
				refused++
			} else {
				accepted++
			}
		})
	}
	// An oracle that cannot fail proves nothing: assert the table actually
	// exercised both verdicts rather than silently degenerating to one side
	// (or to none at all, had every row been skipped).
	if refused == 0 || accepted == 0 {
		t.Fatalf("table reached refused=%d accepted=%d assertions; want both non-zero", refused, accepted)
	}
	t.Logf("assertions reached: %d refused, %d accepted", refused, accepted)
}

// TestBuildFixture_DegenerateSpecs drives the same enumeration end to end
// through the exported entry point, for every row a real machine can build.
//
// # What this test did before the fix (rmp #2744)
//
// It did not FAIL — it PANICKED. The first row, FixtureSpec{Vertices: 0,
// Edges: 1}, reached `r.Uint32() % universe` with universe == 0 and brought
// the whole test binary down with "runtime error: integer divide by zero" and
// exit status 2, so no later row in the table ever ran. The godoc in force at
// the time told an embedder the only failure mode was adjlist.ErrShardFull,
// "which cannot be reached" — so there was no reason to write a recover, and
// the panic escaped into the caller.
func TestBuildFixture_DegenerateSpecs(t *testing.T) {
	refused, accepted := 0, 0
	for _, tc := range degenerateSpecCases {
		if !tc.buildable {
			// Not a t.Skip standing in for unfinished work: these rows ARE
			// asserted, by TestFixtureSpec_Validate above, which is the only
			// level at which a 2^32-vertex spec can be checked at all.
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			c, err := BuildFixture(tc.spec)
			if tc.wantRefus {
				if err == nil {
					t.Fatalf("BuildFixture(%+v) = (%v, nil); want an error wrapping ErrInvalidFixtureSpec", tc.spec, c)
				}
				if !errors.Is(err, ErrInvalidFixtureSpec) {
					t.Fatalf("BuildFixture(%+v) err = %v; want errors.Is(err, ErrInvalidFixtureSpec)", tc.spec, err)
				}
				if c != nil {
					t.Fatalf("BuildFixture(%+v) returned a non-nil CSR alongside its error", tc.spec)
				}
				refused++
				return
			}
			if err != nil {
				t.Fatalf("BuildFixture(%+v) = %v; want success (this spec is legal and must stay legal)", tc.spec, err)
			}
			if c == nil {
				t.Fatalf("BuildFixture(%+v) returned (nil, nil)", tc.spec)
			}
			if c.Order() != tc.wantOrder || c.Size() != tc.wantSize {
				t.Fatalf("BuildFixture(%+v) = order %d size %d; want order %d size %d",
					tc.spec, c.Order(), c.Size(), tc.wantOrder, tc.wantSize)
			}
			accepted++
		})
	}
	if refused == 0 || accepted == 0 {
		t.Fatalf("table reached refused=%d accepted=%d assertions; want both non-zero", refused, accepted)
	}
	t.Logf("assertions reached: %d refused, %d accepted", refused, accepted)
}

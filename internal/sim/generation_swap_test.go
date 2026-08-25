package sim

// generation_swap_test.go — rmp #2491.
//
// Layer: short. The scenario is sized so the whole file runs in a couple of
// seconds under -race (measured: 1.5 s for the whole -run 'TestGenerationSwap'
// set, darwin/arm64, 10 logical cores). The larger reader fleets live in
// generation_swap_wide_test.go, which is ALSO short layer since rmp #2491
// measured it at 0.46 s.
//
// # Why this file is mostly INJECTION tests
//
// A lifecycle oracle that cannot fail proves nothing, and every clause in
// generation_swap.go is invisible in a clean run by construction. So each
// clause that CAN be provoked from outside package graph/generation is
// provoked here, permanently, by forging an artefact or a shadow record that
// violates exactly one property. The table in
// TestGenerationSwap_ProbeCatchesAForgedGeneration names, per injection,
// which clause must fire — and asserts that a clean probe fires none, so the
// control is not doing the work.
//
// Three clauses are NOT injectable from this package and the reason is
// recorded rather than papered over:
//
//   - refcount-below-held-reference needs Release to drop an increment it
//     never took, i.e. a bug inside graph/generation.
//   - the READER-side generation-content-changed needs Generation.csr to be
//     reassigned, and it is an unexported field of an immutable struct. Its
//     publisher-side twin IS injected below, through verifyPublished.
//   - held-drain-did-not-time-out needs PublishWithDrain to stop honouring
//     its own wait condition. What is asserted instead is the DISCRIMINATOR
//     (TestGenerationSwap_DrainTimeoutIsDecidedByTheHeldReference): the same
//     tiny timeout returns ErrDrainTimeout with a reference held and nil
//     without one, so the arm's verdict is decided by the held reference and
//     not by the duration.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/generation"
)

// genSwapTestSeeds is a small fixed spread so the tests exercise several plan
// geometries (generation counts, node counts, op sequences and drain-timeout
// positions) without depending on wall-clock timing.
var genSwapTestSeeds = []uint64{generationSwapDefaultSeed, 0x1, 0x9E3779B9, 0xC0FFEE, 0xBADF00D}

// -----------------------------------------------------------------------------
// The scenario, end to end
// -----------------------------------------------------------------------------

// TestGenerationSwap_Scenario_Passes runs the registered scenario across
// seeds: no clause fires and every non-vacuity gate is satisfied.
func TestGenerationSwap_Scenario_Passes(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioGenerationSwap)
	if !ok {
		t.Fatalf("scenario %q is not registered", ScenarioGenerationSwap)
	}
	if sc.Mode.Reproducible() {
		t.Fatalf("the generation-swap scenario claims to be bit-reproducible; its interleaving is not")
	}
	for _, seed := range genSwapTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %#x: reported a violation:\n%s", seed, report)
		}
	}
}

// TestGenerationSwap_ReaderCounts runs the scenario at several goroutine
// counts. A concurrency-dependent failure that only appears at one width is
// invisible to a single-width run, so the reader fleet is varied
// deliberately across all four widths in the slice below: 2 (the minimum that
// can contend), the default 8, and 32 and 64 (above the 10 logical cores this
// was measured on, so goroutines are genuinely preempted mid-window). The
// wider 64/256/1024 fleets are in generation_swap_wide_test.go; 64 is the
// shared seam between the two files.
func TestGenerationSwap_ReaderCounts(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, readers := range []int{2, 8, 32, 64} {
		for _, seed := range genSwapTestSeeds {
			cfg := DefaultGenerationSwapConfig(seed)
			cfg.Readers = readers
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			ev, err := RunGenerationSwap(ctx, cfg)
			cancel()
			if err != nil {
				t.Fatalf("readers=%d seed=%#x: %v", readers, seed, err)
			}
			v := append(checkGenerationSwap(ev), checkGenerationSwapNonVacuity(ev)...)
			if len(v) != 0 {
				t.Fatalf("readers=%d seed=%#x reported:\n%s\nevidence: %s",
					readers, seed, genSwapViolationsText(v), ev)
			}
			t.Logf("readers=%d %s", readers, ev)
		}
	}
}

// -----------------------------------------------------------------------------
// Determinism: exactly what the seed pins
// -----------------------------------------------------------------------------

// TestGenerationSwap_PlanDigestIsSeedReproducible pins the determinism claim
// the scenario actually makes, and its boundary.
//
// The plan — every generation's shape, adjacency, fingerprint and publish op,
// and the drain-timeout index — is a pure function of the seed. It is
// therefore identical across runs AND across reader counts, because the
// reader sub-seeds are drawn AFTER the plan. That is what lets a concurrent
// scenario print a reproducible identifier in its report even though its
// interleaving is not reproducible at all.
func TestGenerationSwap_PlanDigestIsSeedReproducible(t *testing.T) {
	t.Parallel()

	for _, seed := range genSwapTestSeeds {
		a, err := buildGenerationSwapPlan(DefaultGenerationSwapConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		b, err := buildGenerationSwapPlan(DefaultGenerationSwapConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x (second build): %v", seed, err)
		}
		if a.digest != b.digest {
			t.Fatalf("seed %#x: two builds gave digests %#x and %#x", seed, a.digest, b.digest)
		}
		if a.generations() != b.generations() || a.nodes != b.nodes || a.drainTimeoutAt != b.drainTimeoutAt {
			t.Fatalf("seed %#x: two builds disagree on geometry", seed)
		}
		for i := range a.rows {
			if a.rows[i].fingerprint != b.rows[i].fingerprint || a.rows[i].op != b.rows[i].op {
				t.Fatalf("seed %#x row %d: two builds disagree", seed, i)
			}
		}

		// Varying the READER COUNT must not move the plan: the sub-seeds are
		// drawn last precisely so a config knob cannot perturb the model.
		wide := DefaultGenerationSwapConfig(seed)
		wide.Readers = 64
		c, err := buildGenerationSwapPlan(wide)
		if err != nil {
			t.Fatalf("seed %#x (64 readers): %v", seed, err)
		}
		if c.digest != a.digest {
			t.Fatalf("seed %#x: 8 readers gave digest %#x and 64 readers gave %#x; the plan must not "+
				"depend on the reader count", seed, a.digest, c.digest)
		}
		if len(c.readerSeeds) != 64 {
			t.Fatalf("seed %#x: expected 64 reader sub-seeds, got %d", seed, len(c.readerSeeds))
		}
		// The shared prefix must also match, so a narrower run is a genuine
		// prefix of a wider one.
		for i := range a.readerSeeds {
			if a.readerSeeds[i] != c.readerSeeds[i] {
				t.Fatalf("seed %#x reader %d: sub-seed differs between reader counts", seed, i)
			}
		}
	}

	// Different seeds must give different plans, or "reproducible from the
	// seed" would be true of a constant.
	seen := make(map[uint64]uint64, len(genSwapTestSeeds))
	for _, seed := range genSwapTestSeeds {
		p, err := buildGenerationSwapPlan(DefaultGenerationSwapConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		if other, dup := seen[p.digest]; dup {
			t.Fatalf("seeds %#x and %#x produced the same plan digest %#x", seed, other, p.digest)
		}
		seen[p.digest] = seed
	}
}

// TestGenerationSwap_FingerprintDiscriminatesEveryGeneration is the proof
// that the torn-swap oracle CAN observe a torn swap: the thing a reader
// compares must differ between every pair of generations, or a reader served
// the wrong one would compare equal and pass.
//
// It also records the measured BLINDNESS of the weaker oracles this replaces.
// Every generation the plan builds has the SAME node count, so an
// order-membership check — the shape examples/33_generation_swap uses — is
// blind to every swap here; and the package's own
// csr_rotation_consistency_test.go asserts a constant edge count, which
// cannot discriminate at all. The fingerprint is doing the work, and this
// test says so with numbers rather than by assertion.
func TestGenerationSwap_FingerprintDiscriminatesEveryGeneration(t *testing.T) {
	t.Parallel()

	for _, seed := range genSwapTestSeeds {
		plan, err := buildGenerationSwapPlan(DefaultGenerationSwapConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}

		// (a) The model's fingerprint and an independent traversal of the
		// artefact agree — the two sides of the comparison are calibrated.
		for i := range plan.rows {
			r := &plan.rows[i]
			if got := genSwapFingerprintCSR(r.snapshot); got != r.fingerprint {
				t.Fatalf("seed %#x row %d: traversal fingerprints to %#x, model to %#x",
					seed, i, got, r.fingerprint)
			}
			if decoded, ok := genSwapDecodeSeq(r.snapshot); !ok || decoded != r.seq {
				t.Fatalf("seed %#x row %d: content declares (%d, %t), want (%d, true)",
					seed, i, decoded, ok, r.seq)
			}
		}

		// (b) Pairwise distinct: the discriminating property.
		byFP := make(map[uint64]int, len(plan.rows))
		orders := make(map[uint64]struct{}, len(plan.rows))
		sizes := make(map[uint64]struct{}, len(plan.rows))
		for i := range plan.rows {
			r := &plan.rows[i]
			if other, dup := byFP[r.fingerprint]; dup {
				t.Fatalf("seed %#x: generations %d and %d fingerprint identically (%#x); a reader "+
					"served one while holding the other would pass", seed, other, r.seq, r.fingerprint)
			}
			byFP[r.fingerprint] = r.seq
			orders[r.order] = struct{}{}
			sizes[r.size] = struct{}{}
		}

		// (c) The measured blindness of the weaker oracles.
		if len(orders) != 1 {
			t.Fatalf("seed %#x: expected every generation to share one node count so the "+
				"order-only oracle is provably blind, got %d distinct orders", seed, len(orders))
		}
		t.Logf("seed %#x: %d generations, %d distinct fingerprints, %d distinct orders, %d distinct sizes "+
			"(an order-only oracle discriminates %d of %d generations)",
			seed, len(plan.rows), len(byFP), len(orders), len(sizes), len(orders), len(plan.rows))
	}
}

// -----------------------------------------------------------------------------
// Injection: proving each clause can fire
// -----------------------------------------------------------------------------

// genSwapForgeAdjacency clones a plan row's adjacency so a test can corrupt
// exactly one property of it.
func genSwapForgeAdjacency(plan *genSwapPlan, seq int) map[graph.NodeID][]graph.NodeID {
	src := plan.row(seq).adjacency
	out := make(map[graph.NodeID][]graph.NodeID, len(src))
	for k, v := range src {
		out[k] = slices.Clone(v)
	}
	return out
}

// genSwapPerturbOneArc rewrites a single destination of a single source,
// preserving both the node count and the edge count. It is the TARGETED
// content injection: only the adjacency changes, so only the fingerprint
// clause can fire, and a shape-only oracle stays silent.
func genSwapPerturbOneArc(plan *genSwapPlan, seq int) (map[graph.NodeID][]graph.NodeID, bool) {
	out := genSwapForgeAdjacency(plan, seq)
	for s := 1; s < plan.nodes; s++ {
		dsts := out[graph.NodeID(s)]
		if len(dsts) == 0 {
			continue
		}
		for cand := 1; cand < plan.nodes; cand++ {
			c := graph.NodeID(cand)
			if slices.Contains(dsts, c) {
				continue
			}
			dsts[0] = c
			slices.Sort(dsts)
			out[graph.NodeID(s)] = dsts
			return out, true
		}
	}
	return nil, false
}

// genSwapEncode is the test-side encoder for a forged adjacency.
func genSwapEncode(t *testing.T, nodes int, adj map[graph.NodeID][]graph.NodeID) *csr.CSR[struct{}] {
	t.Helper()
	c, _, _, err := encodeGenSwapCSR(nodes, adj)
	if err != nil {
		t.Fatalf("encoding a forged adjacency: %v", err)
	}
	return c
}

// genSwapClauses lists the clause names a reader recorded.
func genSwapClauses(found []genSwapFinding) []string {
	out := make([]string, 0, len(found))
	for i := range found {
		out = append(out, found[i].Clause)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestGenerationSwap_ProbeCatchesAForgedGeneration is the mutation table for
// the reader's identity and content clauses. Each case publishes an artefact
// that violates exactly one property and asserts that the corresponding
// clause — and, where the properties genuinely nest, only the expected set —
// fires. The first case is the CONTROL: a clean generation must fire nothing,
// so a passing row below is attributable to the injection.
func TestGenerationSwap_ProbeCatchesAForgedGeneration(t *testing.T) {
	t.Parallel()

	cfg := DefaultGenerationSwapConfig(generationSwapDefaultSeed)
	cfg.Readers = 1
	plan, err := buildGenerationSwapPlan(cfg)
	if err != nil {
		t.Fatalf("buildGenerationSwapPlan: %v", err)
	}
	nodes := plan.nodes
	beyond := plan.generations() + 3
	if beyond+1 >= nodes {
		t.Fatalf("the fixture cannot encode an out-of-range sequence marker (%d needs node %d of %d)",
			beyond, beyond+1, nodes)
	}
	perturbed, ok := genSwapPerturbOneArc(plan, 1)
	if !ok {
		t.Fatal("the fixture has no arc to perturb, so the targeted content injection cannot be built")
	}

	// A shape forgery: the same declared sequence, one FEWER node, so order
	// and size differ. shape-mismatch is the precise message; content-mismatch
	// necessarily fires with it, because order and size are folded into the
	// fingerprint. That nesting is real, not oracle coupling: both properties
	// are genuinely violated.
	//
	// The highest node id has to go from the DESTINATIONS as well as from the
	// sources, or the forgery is not encodable at all: csr.Validate rejects a
	// destination that is not strictly below the snapshot's node count, so an
	// arc pointing at the removed node makes genSwapEncode fail as a harness
	// error instead of producing the injection. Measured while auditing rmp
	// #2491: dropping only the source is refused for 120 of 200 seeds, and this
	// fixture passed solely because generationSwapDefaultSeed is one of the
	// other 80. Dropping the destinations too makes it seed-independent and
	// changes neither expected clause — order and size still differ from the
	// model's.
	shapeAdj := genSwapForgeAdjacency(plan, 1)
	delete(shapeAdj, graph.NodeID(nodes-1))
	for src, dsts := range shapeAdj {
		shapeAdj[src] = slices.DeleteFunc(dsts, func(d graph.NodeID) bool {
			return d >= graph.NodeID(nodes-1)
		})
	}

	noMarker := genSwapForgeAdjacency(plan, 1)
	delete(noMarker, genSwapMarkerNode)

	pluralMarker := genSwapForgeAdjacency(plan, 1)
	pluralMarker[genSwapMarkerNode] = []graph.NodeID{2, 3}

	unknownSeq := genSwapForgeAdjacency(plan, 1)
	unknownSeq[genSwapMarkerNode] = []graph.NodeID{graph.NodeID(beyond + 1)}

	cases := []struct {
		name    string
		adj     map[graph.NodeID][]graph.NodeID
		nodes   int
		want    []string
		wantSeq int
	}{
		{
			name: "control-clean-generation", adj: genSwapForgeAdjacency(plan, 1), nodes: nodes,
			want: nil, wantSeq: 1,
		},
		{
			name: "one-arc-rewritten", adj: perturbed, nodes: nodes,
			want: []string{genSwapClauseContent}, wantSeq: 1,
		},
		{
			name: "one-node-removed", adj: shapeAdj, nodes: nodes - 1,
			want: []string{genSwapClauseContent, genSwapClauseShape}, wantSeq: 1,
		},
		{
			name: "marker-absent", adj: noMarker, nodes: nodes,
			want: []string{genSwapClauseMarker}, wantSeq: -1,
		},
		{
			name: "marker-plural", adj: pluralMarker, nodes: nodes,
			want: []string{genSwapClauseMarker}, wantSeq: -1,
		},
		{
			name: "marker-names-an-unpublished-sequence", adj: unknownSeq, nodes: nodes,
			want: []string{genSwapClauseUnknownSeq}, wantSeq: beyond,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := generation.New(genSwapEncode(t, tc.nodes, tc.adj))
			defer pub.Close()
			rd := newGenSwapReader(0, 0, pub, plan, &genSwapRegistry{}, 2)
			seq, acquired := rd.probe(true)
			if !acquired {
				t.Fatal("probe did not acquire from an open publisher")
			}
			if seq != tc.wantSeq {
				t.Fatalf("probe decoded sequence %d, want %d", seq, tc.wantSeq)
			}
			got := genSwapClauses(rd.found)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("clauses fired = %v, want %v\nmessages:\n%s",
					got, want, genSwapFindingsText(rd.found))
			}
		})
	}
}

// genSwapViolationsText renders violations WITH their clause names. The
// shared violationsText helper prints only messages, and a red gate on a
// concurrency scenario is read by whoever has the log and nothing else: the
// clause name is what makes a failure attributable to ONE property rather
// than to "something in generation-swap". Measured while building the
// mutation table for rmp #2491: two library mutations produced clean,
// correctly-attributed failures whose log named no clause at all.
func genSwapViolationsText(vs []Violation) string {
	var b strings.Builder
	for i := range vs {
		b.WriteString("  ")
		b.WriteString(vs[i].Op)
		b.WriteString(" ")
		b.WriteString(vs[i].Message)
		b.WriteByte('\n')
	}
	return b.String()
}

// genSwapFindingsText renders findings for a failure message.
func genSwapFindingsText(found []genSwapFinding) string {
	var b strings.Builder
	for i := range found {
		b.WriteString("  - [")
		b.WriteString(found[i].Clause)
		b.WriteString("] ")
		b.WriteString(found[i].Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestGenerationSwap_ProbeCatchesNonMonotonicAndRecycled injects the two
// lifecycle clauses that need a sequence of probes rather than a single
// forged artefact.
func TestGenerationSwap_ProbeCatchesNonMonotonicAndRecycled(t *testing.T) {
	t.Parallel()

	cfg := DefaultGenerationSwapConfig(generationSwapDefaultSeed)
	cfg.Readers = 1
	plan, err := buildGenerationSwapPlan(cfg)
	if err != nil {
		t.Fatalf("buildGenerationSwapPlan: %v", err)
	}

	t.Run("acquire-went-backwards", func(t *testing.T) {
		pub := generation.New(plan.row(3).snapshot)
		defer pub.Close()
		rd := newGenSwapReader(0, 0, pub, plan, &genSwapRegistry{}, 2)
		if seq, _ := rd.probe(true); seq != 3 {
			t.Fatalf("first probe decoded %d, want 3", seq)
		}
		if len(rd.found) != 0 {
			t.Fatalf("the clean first probe fired:\n%s", genSwapFindingsText(rd.found))
		}
		// Regress the current pointer, which a single publisher can never do.
		if _, err := pub.Publish(plan.row(1).snapshot); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if seq, _ := rd.probe(false); seq != 1 {
			t.Fatalf("second probe decoded %d, want 1", seq)
		}
		if got := genSwapClauses(rd.found); !slices.Equal(got, []string{genSwapClauseNonMonotonic}) {
			t.Fatalf("clauses fired = %v, want [%s]\n%s",
				got, genSwapClauseNonMonotonic, genSwapFindingsText(rd.found))
		}
	})

	t.Run("use-after-recycle", func(t *testing.T) {
		reg := &genSwapRegistry{}
		pub := generation.New(plan.row(2).snapshot)
		defer pub.Close()
		rd := newGenSwapReader(0, 0, pub, plan, reg, 2)
		// Mark the CURRENT generation reclaimed, which a correct publisher only
		// ever does to a superseded one.
		reg.slot(pub.Current()).freed.Store(true)
		if _, acquired := rd.probe(false); !acquired {
			t.Fatal("probe did not acquire")
		}
		if got := genSwapClauses(rd.found); !slices.Equal(got, []string{genSwapClauseRecycled}) {
			t.Fatalf("clauses fired = %v, want [%s]\n%s",
				got, genSwapClauseRecycled, genSwapFindingsText(rd.found))
		}
	})

	t.Run("refcount-above-holder-bound", func(t *testing.T) {
		pub := generation.New(plan.row(2).snapshot)
		defer pub.Close()
		// maxHolders 0 is unreachable in a real run (it is readers+1 >= 2); it
		// is set here so the ceiling clause's arithmetic is exercised, proving
		// the branch is live rather than dead code.
		rd := newGenSwapReader(0, 0, pub, plan, &genSwapRegistry{}, 0)
		if _, acquired := rd.probe(false); !acquired {
			t.Fatal("probe did not acquire")
		}
		if got := genSwapClauses(rd.found); !slices.Equal(got, []string{genSwapClauseRefCeiling}) {
			t.Fatalf("clauses fired = %v, want [%s]\n%s",
				got, genSwapClauseRefCeiling, genSwapFindingsText(rd.found))
		}
	})
}

// TestGenerationSwap_PublisherCatchesAMisidentifiedPublish injects the
// publisher-side identity clause: publishing the model's snapshot for one
// sequence while booking it as another must be caught, which is the
// reachable twin of the reader's generation-content-changed clause.
func TestGenerationSwap_PublisherCatchesAMisidentifiedPublish(t *testing.T) {
	t.Parallel()

	cfg := DefaultGenerationSwapConfig(generationSwapDefaultSeed)
	cfg.Readers = 1
	plan, err := buildGenerationSwapPlan(cfg)
	if err != nil {
		t.Fatalf("buildGenerationSwapPlan: %v", err)
	}
	pub := generation.New(plan.row(0).snapshot)
	defer pub.Close()
	pw := &genSwapPublisher{pub: pub, plan: plan, reg: &genSwapRegistry{}, clock: NewVirtualClock(time.Millisecond)}

	// CONTROL: the right artefact under the right sequence fires nothing.
	pw.verifyPublished(pub.Current(), 0)
	if len(pw.found) != 0 {
		t.Fatalf("verifying a correctly published generation fired:\n%s", genSwapFindingsText(pw.found))
	}

	// INJECTION: sequence 4's artefact booked as sequence 5.
	g, err := pub.Publish(plan.row(4).snapshot)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	pw.verifyPublished(g, 5)
	if got := genSwapClauses(pw.found); !slices.Equal(got, []string{genSwapClauseMutated}) {
		t.Fatalf("clauses fired = %v, want [%s]\n%s",
			got, genSwapClauseMutated, genSwapFindingsText(pw.found))
	}
}

// TestGenerationSwap_TerminalAuditsFire injects the two quiescent audits.
// Both are exact assertions rather than samples, so both must be provably
// able to fail.
func TestGenerationSwap_TerminalAuditsFire(t *testing.T) {
	t.Parallel()

	cfg := DefaultGenerationSwapConfig(generationSwapDefaultSeed)
	cfg.Readers = 1
	plan, err := buildGenerationSwapPlan(cfg)
	if err != nil {
		t.Fatalf("buildGenerationSwapPlan: %v", err)
	}

	t.Run("refcount-nonzero-at-rest", func(t *testing.T) {
		pub := generation.New(plan.row(0).snapshot)
		defer pub.Close()
		g := pub.Current()

		// CONTROL: nothing held, so the audit is silent and reads a zero.
		rcs, found := auditGenSwapRefcountsAtRest([]*generation.Generation[struct{}]{g})
		if len(found) != 0 || len(rcs) != 1 || rcs[0] != 0 {
			t.Fatalf("the audit of an untouched generation reported rcs=%v findings:\n%s",
				rcs, genSwapFindingsText(found))
		}

		// INJECTION: a leaked Acquire — exactly the defect the audit exists for.
		leaked := pub.Acquire()
		rcs, found = auditGenSwapRefcountsAtRest([]*generation.Generation[struct{}]{g})
		if len(rcs) != 1 || rcs[0] != 1 {
			t.Fatalf("the audit read rcs=%v, want [1]", rcs)
		}
		if got := genSwapClauses(found); !slices.Equal(got, []string{genSwapClauseRefAtRest}) {
			t.Fatalf("clauses fired = %v, want [%s]\n%s",
				got, genSwapClauseRefAtRest, genSwapFindingsText(found))
		}
		pub.Release(leaked)
	})

	t.Run("pointer-sequence-disagreement", func(t *testing.T) {
		reg := &genSwapRegistry{}
		pub := generation.New(plan.row(6).snapshot)
		defer pub.Close()
		rd := newGenSwapReader(0, 0, pub, plan, reg, 2)
		if seq, _ := rd.probe(false); seq != 6 {
			t.Fatalf("probe decoded %d, want 6", seq)
		}

		// CONTROL: the publisher records what the content declared, so the
		// cross-check is silent.
		reg.slot(pub.Current()).seq.Store(6)
		if found := auditGenSwapPointerSequences([]*genSwapReader{rd}, reg); len(found) != 0 {
			t.Fatalf("the cross-check fired on agreeing records:\n%s", genSwapFindingsText(found))
		}

		// INJECTION: the publisher's book and the artefact's content disagree.
		reg.slot(pub.Current()).seq.Store(7)
		found := auditGenSwapPointerSequences([]*genSwapReader{rd}, reg)
		if got := genSwapClauses(found); !slices.Equal(got, []string{genSwapClausePointerSeq}) {
			t.Fatalf("clauses fired = %v, want [%s]\n%s",
				got, genSwapClausePointerSeq, genSwapFindingsText(found))
		}
	})
}

// TestGenerationSwap_DrainTimeoutIsDecidedByTheHeldReference is the
// discriminator for the drain-timeout arm, and the answer to "is this arm
// timing-thresholded?".
//
// The SAME timeout is used for both directions on the SAME publisher. With a
// reference held for the whole call, PublishWithDrain's wait condition is
// permanently true and ErrDrainTimeout is the only reachable exit. With no
// reference held, the predecessor's refcount is already zero, the wait loop
// is never entered, and the call returns nil without consulting the deadline
// at all. So the verdict is decided by the held reference, and the duration
// changes only how long the timing-out arm takes.
//
// The 1-nanosecond case is included on purpose: if the arm were a race
// between the timer and the drain, one nanosecond would be where it broke.
func TestGenerationSwap_DrainTimeoutIsDecidedByTheHeldReference(t *testing.T) {
	t.Parallel()

	cfg := DefaultGenerationSwapConfig(generationSwapDefaultSeed)
	cfg.Readers = 1
	plan, err := buildGenerationSwapPlan(cfg)
	if err != nil {
		t.Fatalf("buildGenerationSwapPlan: %v", err)
	}

	for _, timeout := range []time.Duration{time.Nanosecond, time.Microsecond, time.Millisecond, 20 * time.Millisecond} {
		pub := generation.New(plan.row(0).snapshot)

		// HELD: must time out, and Current must still be the new generation
		// with its content intact.
		prev := pub.Current()
		hostage := pub.Acquire()
		if hostage != prev {
			t.Fatalf("timeout=%s: the hostage is not the generation about to be superseded", timeout)
		}
		next, err := pub.PublishWithDrain(plan.row(1).snapshot, timeout)
		if !errors.Is(err, generation.ErrDrainTimeout) {
			t.Fatalf("timeout=%s: PublishWithDrain with a reference held returned %v, want ErrDrainTimeout",
				timeout, err)
		}
		if next == nil {
			t.Fatalf("timeout=%s: ErrDrainTimeout came back with a nil generation", timeout)
		}
		if cur := pub.Current(); cur != next {
			t.Fatalf("timeout=%s: Current() is not the generation the timed-out publish installed", timeout)
		}
		if got := genSwapFingerprintCSR(next.CSR()); got != plan.row(1).fingerprint {
			t.Fatalf("timeout=%s: the generation installed by the timed-out publish fingerprints to "+
				"%#x, want %#x: the timeout corrupted Current", timeout, got, plan.row(1).fingerprint)
		}
		pub.Release(hostage)

		// FREE: the identical timeout now completes, so the arm above was not
		// simply "PublishWithDrain always times out".
		if _, err := pub.PublishWithDrain(plan.row(2).snapshot, timeout); err != nil {
			t.Fatalf("timeout=%s: PublishWithDrain with nothing held returned %v, want nil", timeout, err)
		}
		pub.Close()
	}
}

// TestGenerationSwap_FingerprintIsAllocationFree turns the file header's
// zero-allocation claim into a gate instead of an assertion. The reader hot
// path runs the CSR fingerprint on a quarter of its acquisitions, and
// csr.CSR.NeighboursByID is allocation-free ONLY in the direct-range form its
// godoc documents — storing the iterator in a variable heap-escapes the
// closure. This pins both: the digest allocates nothing, and that call form
// still inlines.
func TestGenerationSwap_FingerprintIsAllocationFree(t *testing.T) {
	// t.Parallel() is intentionally absent: testing.AllocsPerRun panics when
	// called while any parallel test is in flight, so this test must stay in
	// the sequential pass. Same reason as store/wal's TestWALOps_RoundTrip.

	plan, err := buildGenerationSwapPlan(DefaultGenerationSwapConfig(generationSwapDefaultSeed))
	if err != nil {
		t.Fatalf("buildGenerationSwapPlan: %v", err)
	}
	snap := plan.row(1).snapshot
	got := testing.AllocsPerRun(100, func() { _ = genSwapFingerprintCSR(snap) })
	if got != 0 {
		t.Errorf("genSwapFingerprintCSR allocates %.1f time(s) per call, want 0: either the digest "+
			"gained an allocation or NeighboursByID stopped inlining at its direct-range call site",
			got)
	}
	// And the decode, which every acquisition runs.
	if got := testing.AllocsPerRun(100, func() { _, _ = genSwapDecodeSeq(snap) }); got != 0 {
		t.Errorf("genSwapDecodeSeq allocates %.1f time(s) per call, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// The gates themselves
// -----------------------------------------------------------------------------

// TestGenerationSwapNonVacuity_FiresOnARunThatDidNothing proves the
// non-vacuity gate can fail. A gate that stays silent for a run in which
// nothing happened would let a scenario pass by doing nothing at all — the
// exact failure mode the gate exists to prevent.
func TestGenerationSwapNonVacuity_FiresOnARunThatDidNothing(t *testing.T) {
	t.Parallel()

	// A run whose goroutines never finished: the gate must say so and stop,
	// rather than evaluating clauses on unfinished state.
	if v := checkGenerationSwapNonVacuity(&GenerationSwapEvidence{}); len(v) == 0 {
		t.Fatal("the non-vacuity gate passed a run in which the publisher never finished")
	}

	// A run that finished and joined but observed nothing: every gate must
	// fire, so an empty run cannot be mistaken for a clean one.
	empty := &GenerationSwapEvidence{
		PublisherFinished: true,
		ReadersJoined:     true,
		Readers:           4,
		Generations:       10,
		Arm2Ran:           true,
		Arm2ReadersJoined: true,
		Arm2Readers:       4,
	}
	v := checkGenerationSwapNonVacuity(empty)
	fired := make(map[string]struct{}, len(v))
	for _, x := range v {
		fired[x.Op] = struct{}{}
	}
	for _, clause := range []string{
		genSwapClauseNVAcquires, genSwapClauseNVFull, genSwapClauseNVDistinct,
		genSwapClauseNVTimeout, genSwapClauseNVDrain, genSwapClauseNVRecycle,
		genSwapClauseNVGenCount, genSwapClauseNVCloseLoad,
	} {
		if _, ok := fired[genSwapOp(clause)]; !ok {
			t.Errorf("the non-vacuity gate did not fire %q on a run that observed nothing", clause)
		}
	}
}

// TestGenerationSwap_PostCloseClausesFire proves the four post-close contract
// clauses can fail. They are booleans recorded by the run, so without this
// they would be unfalsifiable by inspection.
func TestGenerationSwap_PostCloseClausesFire(t *testing.T) {
	t.Parallel()

	clean := &GenerationSwapEvidence{
		PublisherFinished:          true,
		ReadersJoined:              true,
		CloseReturned:              true,
		AcquireAfterCloseNil:       true,
		CurrentAfterCloseNil:       true,
		PublishAfterCloseErrClosed: true,
		DrainAfterCloseErrClosed:   true,
	}
	if v := checkGenerationSwap(clean); len(v) != 0 {
		t.Fatalf("the contract check fired on a run that satisfied every post-close clause:\n%s",
			genSwapViolationsText(v))
	}

	cases := []struct {
		name   string
		mutate func(*GenerationSwapEvidence)
		clause string
	}{
		{"acquire-returned-a-generation", func(e *GenerationSwapEvidence) { e.AcquireAfterCloseNil = false },
			genSwapClauseAcqAfterClose},
		{"current-not-nil", func(e *GenerationSwapEvidence) { e.CurrentAfterCloseNil = false },
			genSwapClauseCurAfterClose},
		{"publish-not-errclosed", func(e *GenerationSwapEvidence) { e.PublishAfterCloseErrClosed = false },
			genSwapClausePubAfterClose},
		{"drain-not-errclosed", func(e *GenerationSwapEvidence) { e.DrainAfterCloseErrClosed = false },
			genSwapClauseDrainAfterClose},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := *clean
			tc.mutate(&e)
			v := checkGenerationSwap(&e)
			if len(v) != 1 || v[0].Op != genSwapOp(tc.clause) {
				t.Fatalf("expected exactly the %q clause, got:\n%s", tc.clause, genSwapViolationsText(v))
			}
		})
	}
}

// TestGenerationSwapReport_PanicsWithoutViolation pins the reporting
// discipline: a non-nil report that names nothing is a defect, so building
// one is refused rather than rendered.
func TestGenerationSwapReport_PanicsWithoutViolation(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("generationSwapReport accepted an empty violation slice")
		}
	}()
	_ = generationSwapReport(1, &GenerationSwapEvidence{}, nil)
}

// TestGenerationSwap_PlanGeometryIsAlwaysEncodable pins the harness
// precondition the identity oracle rests on: the sequence marker encodes
// generation k as node k+1, so the smallest permitted node count must leave
// room for the largest permitted generation count. If a future edit to the
// geometry constants broke this, buildGenerationSwapPlan would start
// refusing seeds at random rather than silently publishing generations whose
// identity cannot be decoded — this test makes the constraint explicit.
func TestGenerationSwap_PlanGeometryIsAlwaysEncodable(t *testing.T) {
	t.Parallel()
	if genSwapMaxGenerations+1 >= genSwapMinNodes {
		t.Fatalf("the plan constants allow up to %d generations, needing node id %d, against a "+
			"minimum of %d nodes: the sequence marker cannot always be encoded",
			genSwapMaxGenerations, genSwapMaxGenerations+1, genSwapMinNodes)
	}
	// The refusal branch in buildGenerationSwapPlan is therefore
	// UNREACHABLE-BY-CONSTRUCTION, and the assertion above — not a test of that
	// branch — is what protects it. It cannot be driven from outside: the
	// geometry is drawn from the seed inside the builder, not passed in. What is
	// asserted here is only the other half: the default geometry is ACCEPTED, so
	// a constants edit that made every seed unreachable would fail loudly.
	if _, err := buildGenerationSwapPlan(GenerationSwapConfig{}); err != nil {
		t.Fatalf("the default geometry was refused: %v", err)
	}
}

package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// fluentQueryFixtureNodes / fluentQueryFixtureDeletes size the tiny in-memory
// fixture the falsifiability table drives. It is deliberately NOT the scenario:
// what these cases need is a graph with tombstones, indexes and a KNOWS path,
// reached in milliseconds, so each perturbation can be shown to fire.
const (
	fluentQueryFixtureNodes   = 12
	fluentQueryFixtureDeletes = 3
)

// newFluentQueryFixture builds a small in-memory (no crash, no store) simulator
// holding a Person path graph, the scenario's three indexes, and a few
// DETACH-DELETEd Persons so the :Person label bitmap really carries corpses —
// which is the precondition every prune clause needs in order to be able to
// fail.
//
// It drives the SAME modelled templates the scenario does, so the oracle stays
// the arbiter here exactly as it is there.
func newFluentQueryFixture(t *testing.T, seed uint64) *Simulator {
	return newFluentQueryFixtureOpt(t, seed, true)
}

// newFluentQueryFixtureOpt is [newFluentQueryFixture] with the index DDL made
// optional, so a test can drive the battery against a graph whose indexes do
// NOT satisfy query/index_seek.go's guard and watch the seek-eligibility
// precondition fire.
func newFluentQueryFixtureOpt(t *testing.T, seed uint64, withIndexes bool) *Simulator {
	t.Helper()
	sm, err := New(Config{Seed: seed, MaxTicks: 0})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	ctx := context.Background()
	names := make([]string, 0, fluentQueryFixtureNodes)
	for i := 0; i < fluentQueryFixtureNodes; i++ {
		name := fluentQueryFixtureName(i)
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": name, "age": int64(20 + i)}}
		if committed := sm.execute(ctx, op); !committed {
			t.Fatalf("fixture: CREATE %q was not committed", name)
		} else {
			sm.applyToOracle(op, committed)
		}
		names = append(names, name)
	}
	for i := 0; i+1 < len(names); i++ {
		op := Op{Kind: OpCreate, Cypher: tmplCreateKnows,
			Params: map[string]any{"a": names[i], "b": names[i+1]}}
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		if !committed {
			t.Fatalf("fixture: KNOWS %q->%q was not committed", names[i], names[i+1])
		}
	}
	if withIndexes {
		for _, ddl := range fluentQueryDDL {
			if err := sm.engineRunDDL(ctx, ddl); err != nil {
				t.Fatalf("fixture DDL %q: %v", ddl, err)
			}
		}
	}
	// Delete INTERIOR nodes, so the survivors still form KNOWS arcs and the
	// one-hop probes stay non-empty while the label bitmap gains corpses.
	for i := 0; i < fluentQueryFixtureDeletes; i++ {
		victim := names[2+2*i]
		op := Op{Kind: OpDelete, Cypher: tmplDetachDelete, Params: map[string]any{"name": victim}}
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		if !committed {
			t.Fatalf("fixture: DETACH DELETE %q was not committed", victim)
		}
	}
	return sm
}

// fluentQueryFixtureName is the fixture's node-name namespace, disjoint from
// every name a workload template can emit.
func fluentQueryFixtureName(i int) string {
	return fmt.Sprintf("fq-fixture-%02d", i)
}

// TestFluentQuery_FixtureBatteryIsClean is the control for the whole
// falsifiability table below: on an unperturbed fixture every clause must be
// silent, AND the fixture must have reached the state that lets the clauses
// fail — a positive tombstoned-in-label-index count and a constructed ghost
// arc. A clean run with a zero precondition would prove nothing.
func TestFluentQuery_FixtureBatteryIsClean(t *testing.T) {
	t.Parallel()
	sm := newFluentQueryFixture(t, 0x2492_0001)
	probes := NewFluentQueryProbes(NewSeed(0x2492_0001 ^ fluentQueryProbeSeedMix))
	v, err := probes.Check(context.Background(), 1, sm.graph(), sm.engine, sm.oracle, fqPerturbNone)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("the unperturbed battery reported %d violation(s):\n%s", len(v), renderViolations(v))
	}
	ev := probes.Evidence()
	if ev.MaxTombstonedSlots == 0 || ev.MaxTombstoneCount == 0 {
		t.Fatalf("the fixture left NO tombstoned Mapper slot (slots=%d count=%d), so the seed-prune "+
			"clauses could not have failed and the clean result above proves nothing",
			ev.MaxTombstonedSlots, ev.MaxTombstoneCount)
	}
	if ev.GhostArcsSeen == 0 {
		t.Fatal("the ghost fixture constructed no ghost arc, so Out()'s prune branch was not reached")
	}
	if ev.HashSeekEligible != 1 || ev.BTreeSeekEligible != 1 {
		t.Fatalf("seek eligibility was not established: hash=%d btree=%d (want 1 and 1)",
			ev.HashSeekEligible, ev.BTreeSeekEligible)
	}
	if ev.MaxOutTargets == 0 {
		t.Fatal("the one-hop probe matched nothing, so no CSR was actually expanded")
	}
	t.Logf("fixture evidence: %s", ev.String())
}

// TestFluentQuery_ClausesAreFalsifiable is the assert-something-was-seen gate
// for every clause family: each perturbation reproduces the OUTPUT of a
// specific defect and the named clause must go red.
//
// The two prune perturbations do not fabricate a mismatch. They recompute the
// answer with the prune omitted — the raw label bitmap for the seed path, and
// the raw out-neighbourhood for the hop — so a case passing here proves the
// clause catches the real defect, not merely that two unequal sets differ.
func TestFluentQuery_ClausesAreFalsifiable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// wantClause is a substring of the Op label of the clause that must fire.
		wantClause string
		// wantProbe is a substring of the message, naming the probe.
		wantProbe string
		perturb   fqPerturb
	}{
		{
			name: "a seed-path prune that stopped removing tombstoned ids",
			// The corpses are not live named nodes in the substrate view, so the
			// identity clause fires — which is the point: the detector is identity,
			// not a count that might coincidentally match.
			perturb:    fqPerturbSeedPruneDisabled,
			wantClause: "unknown-id",
			wantProbe:  "all-live(seed-prune-disabled)",
		},
		{
			// The DETECTOR here is identity, not the name set: a corpse is unnamed
			// in the substrate view by construction, so it contributes no name and
			// the "ghost-fixture:out-prune" name comparison stays SILENT. MEASURED
			// by running this very case. That asymmetry is why both clauses exist.
			name:       "an Out() prune that stopped removing ghost targets",
			perturb:    fqPerturbOutPruneDisabled,
			wantClause: "unknown-id",
			wantProbe:  "ghost-fixture:out(out-prune-disabled)",
		},
		{
			name:       "the fluent seek arm losing a name",
			perturb:    fqPerturbFluentDropName,
			wantClause: "fluent-vs-oracle",
			wantProbe:  "label",
		},
		{
			name:       "the fluent seek arm losing a name, seen against Cypher",
			perturb:    fqPerturbFluentDropName,
			wantClause: "fluent-vs-cypher",
			wantProbe:  "label",
		},
		{
			name:       "the raw-CSR arm diverging from the live-filtered one",
			perturb:    fqPerturbRawArmDrop,
			wantClause: "csr-generation-invariance",
			wantProbe:  "out-all",
		},
		{
			name:       "the raw-CSR arm diverging inside the ghost fixture",
			perturb:    fqPerturbRawArmDrop,
			wantClause: "ghost-fixture:csr-invariance",
			wantProbe:  "out-all",
		},
		{
			name:       "the Cypher arm losing a row",
			perturb:    fqPerturbCypherDropRow,
			wantClause: "cypher-vs-oracle",
			wantProbe:  "label",
		},
		{
			name:       "the scan arm losing a name the seek arm kept",
			perturb:    fqPerturbScanArmDrop,
			wantClause: "seek-vs-scan",
			wantProbe:  "eq-present",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sm := newFluentQueryFixture(t, 0x2492_0002)
			probes := NewFluentQueryProbes(NewSeed(0x2492_0002 ^ fluentQueryProbeSeedMix))
			v, err := probes.Check(context.Background(), 1, sm.graph(), sm.engine, sm.oracle, tc.perturb)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(v) == 0 {
				t.Fatalf("the perturbed battery reported NOTHING; the %q clause cannot fire, so its "+
					"silence on a real run proves nothing", tc.wantClause)
			}
			found := false
			for _, x := range v {
				if strings.Contains(x.Op, tc.wantClause) && strings.Contains(x.Message, tc.wantProbe) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no violation names clause %q on probe %q; got:\n%s",
					tc.wantClause, tc.wantProbe, renderViolations(v))
			}
		})
	}
}

// fluentQueryGhostSeedSweep is how many derived seeds
// TestFluentQuery_GhostFixtureIsConstructibleForEverySeed drives.
//
// It exists because rmp #2491 shipped a falsifiability fixture that was
// UNBUILDABLE for 120 of 200 seeds and passed only because the catalogue seed
// happened to be lucky. A fixture whose precondition holds only sometimes is a
// clause that silently stops testing anything.
const fluentQueryGhostSeedSweep = 400

// TestFluentQuery_GhostFixtureIsConstructibleForEverySeed sweeps the ghost
// fixture across many derived seeds and requires it to be clean AND to have
// constructed at least one ghost arc EVERY time.
//
// The construction is designed to make that a theorem rather than a hope: the
// fixture is a path graph and the removed nodes are drawn from its INTERIOR, and
// an interior node of a path always has an incoming arc, so tombstoning it
// always leaves an arc pointing into a tombstoned target. The sweep is what
// proves the design claim rather than asserting it.
func TestFluentQuery_GhostFixtureIsConstructibleForEverySeed(t *testing.T) {
	t.Parallel()
	worstArcs := -1
	for i := 0; i < fluentQueryGhostSeedSweep; i++ {
		seed := NewSeed(uint64(i)*0x9E37_79B9_7F4A_7C15 + 1)
		v, arcs := fluentQueryGhostFixture(int64(i), seed, fqPerturbNone)
		if len(v) != 0 {
			t.Fatalf("seed index %d: the ghost fixture reported %d violation(s):\n%s",
				i, len(v), renderViolations(v))
		}
		if arcs <= 0 {
			t.Fatalf("seed index %d: the ghost fixture constructed %d ghost arcs; its precondition "+
				"must hold for EVERY seed, not only the catalogue one", i, arcs)
		}
		if worstArcs < 0 || arcs < worstArcs {
			worstArcs = arcs
		}
	}
	t.Logf("%d seeds: the fixture always constructed at least %d ghost arc(s)",
		fluentQueryGhostSeedSweep, worstArcs)
}

// TestFluentQuery_GhostFixturePerturbationFiresForEverySeed is the falsifiability
// sweep's twin: the out-prune perturbation must fire for every seed too. A
// perturbation that only bites for some seeds would make the falsifiability
// proof above conditional on the seed the table happens to use.
func TestFluentQuery_GhostFixturePerturbationFiresForEverySeed(t *testing.T) {
	t.Parallel()
	for i := 0; i < fluentQueryGhostSeedSweep; i++ {
		seed := NewSeed(uint64(i)*0x9E37_79B9_7F4A_7C15 + 1)
		v, _ := fluentQueryGhostFixture(int64(i), seed, fqPerturbOutPruneDisabled)
		fired := false
		for _, x := range v {
			// The identity clause, not the name comparison — see
			// TestFluentQuery_ClausesAreFalsifiable for why.
			if strings.Contains(x.Op, "unknown-id") {
				fired = true
				break
			}
		}
		if !fired {
			t.Fatalf("seed index %d: the out-prune perturbation did NOT fire; got:\n%s",
				i, renderViolations(v))
		}
	}
}

// TestFluentQuery_FinishGatesAllFire proves every terminal non-vacuity gate can
// fail, by handing [FluentQueryProbes.Finish] an evidence record that reached
// none of its preconditions. Without this, a green Finish on a real run could
// mean "the gates hold" or "the gates are unreachable", and those are different
// facts.
func TestFluentQuery_FinishGatesAllFire(t *testing.T) {
	t.Parallel()
	// Batteries == 0 short-circuits with a single, distinct message.
	p := NewFluentQueryProbes(NewSeed(1))
	v := p.Finish(7)
	if len(v) != 1 || !strings.Contains(v[0].Op, "vacuity:batteries") {
		t.Fatalf("a run with zero batteries must fail with exactly the batteries gate; got:\n%s",
			renderViolations(v))
	}

	// One battery and nothing else: every other gate must name itself.
	p = NewFluentQueryProbes(NewSeed(1))
	p.Evidence().Batteries = 1
	v = p.Finish(7)
	want := []string{
		"vacuity:post-recovery", "vacuity:churn", "vacuity:live-names", "vacuity:out-targets",
		"vacuity:tombstone-load-bearing", "vacuity:string-range", "vacuity:int-range",
		"vacuity:hash-seek", "vacuity:btree-seek", "vacuity:ghost-arcs",
	}
	for _, clause := range want {
		found := false
		for _, x := range v {
			if strings.Contains(x.Op, clause) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("gate %q did not fire on an empty run; got:\n%s", clause, renderViolations(v))
		}
	}
	if len(v) != len(want) {
		t.Errorf("Finish reported %d gate(s), want %d: a gate was added or removed without updating "+
			"this test, so the list of things the scenario proves it exercised is no longer pinned\n%s",
			len(v), len(want), renderViolations(v))
	}
}

// TestFluentQuery_ModelShapePreconditionFires proves the precondition that the
// Out()-vs-`-[:KNOWS]->` equivalence rests on is a real clause and not a
// comment.
//
// The equivalence holds only because every modelled node is :Person and every
// modelled edge is :KNOWS — [query.Pattern.Out] expands a CSR that carries no
// relationship type at all, so a second edge type would silently break the
// comparison. These cases plant each violation directly in the model, which is
// the only way to reach a shape the workload templates cannot produce.
func TestFluentQuery_ModelShapePreconditionFires(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(o *GraphOracle)
		wantMsg string
	}{
		{
			name: "a node carrying a label other than Person",
			mutate: func(o *GraphOracle) {
				o.nodes[o.nextNodeID] = &NodeState{
					ID: o.nextNodeID, Labels: []string{"Person", "Admin"},
					Properties: map[string]any{"name": "shape-a"},
				}
				o.nextNodeID++
			},
			wantMsg: "not exactly [Person]",
		},
		{
			name: "an edge carrying a type other than KNOWS",
			mutate: func(o *GraphOracle) {
				a, b := o.nextNodeID, o.nextNodeID+1
				o.nodes[a] = &NodeState{ID: a, Labels: []string{"Person"},
					Properties: map[string]any{"name": "shape-b1"}}
				o.nodes[b] = &NodeState{ID: b, Labels: []string{"Person"},
					Properties: map[string]any{"name": "shape-b2"}}
				o.nextNodeID += 2
				o.edges[edgeKey{src: a, dst: b, label: "LIKES"}] =
					&EdgeState{SrcID: a, DstID: b, Label: "LIKES", Properties: map[string]any{}}
			},
			wantMsg: `type "LIKES"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := NewGraphOracle()
			tc.mutate(o)
			m := newFluentQueryModel(o)
			if len(m.shapeFindings) == 0 {
				t.Fatal("newFluentQueryModel accepted a shape the Out()-vs-KNOWS equivalence forbids")
			}
			joined := strings.Join(m.shapeFindings, "\n")
			if !strings.Contains(joined, tc.wantMsg) {
				t.Fatalf("no finding mentions %q; got:\n%s", tc.wantMsg, joined)
			}
		})
	}
}

// fluentQueryShortTicks is the SHORT-layer tick budget for the scenario loop.
// The catalogue budget itself is small enough for the short layer, but the
// end-to-end test runs the loop TWICE (for the determinism claim) under -race,
// so it uses a trimmed budget that still contains a checkpoint, several crashes,
// several churn cycles and several batteries.
const fluentQueryShortTicks = 160

// TestFluentQuery_RunsCleanAndIsDeterministic drives the whole scenario loop
// twice with the same seed and asserts both that it is clean and that it is
// bit-reproducible.
//
// The determinism claim is deliberately made on [FluentQueryEvidence.Digest],
// which folds only model and count quantities. It cannot be made on node keys
// or NodeIDs: the Cypher engine mints keys from a PROCESS-GLOBAL counter
// (cypher/exec/create_node.go), so those differ between the first and the second
// run in the SAME process even at an identical seed — which is exactly why the
// digest excludes them.
func TestFluentQuery_RunsCleanAndIsDeterministic(t *testing.T) {
	t.Parallel()
	cfg := DefaultFluentQueryConfig(fluentQueryDefaultSeed)
	cfg.MaxTicks = fluentQueryShortTicks

	first, report, err := RunFluentQuery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if report != nil {
		t.Fatalf("first run reported a violation:\n%s", report)
	}
	second, report, err := RunFluentQuery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if report != nil {
		t.Fatalf("second run reported a violation:\n%s", report)
	}
	if first.Digest != second.Digest {
		t.Fatalf("same seed produced different digests: %#016x vs %#016x\nfirst:  %s\nsecond: %s",
			first.Digest, second.Digest, first.String(), second.String())
	}
	// The comparison is on ReproducibleSummary, not on the whole record. Exactly
	// one measured field is deliberately excluded —
	// MaxTombstonedInLabelIndexObserved — because lpg defers a deleted node's
	// label-bitmap removal and applies it from its BACKGROUND VACUUM, so the
	// count of label-advertised corpses at a given tick is a function of when
	// that goroutine woke. MEASURED: two runs of this very seed in this very
	// process observed 3 and 2. Asserting equality on it would be asserting a
	// scheduler outcome, which is what #2587/#2596 removed from other scenarios.
	if first.ReproducibleSummary() != second.ReproducibleSummary() {
		t.Fatalf("same seed produced different reproducible evidence:\nfirst:  %s\nsecond: %s",
			first.ReproducibleSummary(), second.ReproducibleSummary())
	}

	// The run must have been non-vacuous. Finish already gated this inside the
	// run; asserting the numbers here is what makes a future budget change that
	// quietly stops reaching a phase visible as a test failure and not as a
	// silently weaker scenario.
	if first.Batteries < 3 {
		t.Errorf("only %d batteries ran at %d ticks", first.Batteries, cfg.MaxTicks)
	}
	if first.BatteriesAfterRecovery == 0 {
		t.Error("no battery ran after a crash recovery")
	}
	if first.ChurnCycles == 0 {
		t.Error("the churn phase never completed a delete-then-recreate pair")
	}
	// The docs claim the raw and live-filtered CSR builds never differ ON THE
	// LIVE GRAPH — which is why the ghost-arc prune needs a constructed fixture.
	// That is a claim about every battery, so it is checked over the whole run
	// rather than read off the last battery's pair. It is reported, not failed:
	// a non-zero value would mean the live graph had started producing ghost
	// arcs, which is a documentation problem rather than an engine defect.
	if first.CSRGenerationsDiffered != 0 {
		t.Errorf("the two CSR generations differed at %d of %d batteries; docs/dst-feature-coverage.md "+
			"claims the live graph produces no ghost arc (DETACH DELETE strips them), and that claim "+
			"now needs revisiting", first.CSRGenerationsDiffered, first.Batteries)
	}
	if first.MaxTombstonedSlots == 0 || first.MaxTombstoneCount == 0 {
		t.Errorf("no battery saw a tombstoned Mapper slot: slots=%d count=%d",
			first.MaxTombstonedSlots, first.MaxTombstoneCount)
	}
	t.Logf("evidence: %s", first.String())
}

// TestFluentQuery_ScenarioPasses runs the registered catalogue scenario at its
// own budget, through the registry, so the catalogue wiring — not just the
// exported Run function — is exercised. goleak guards the durable store's
// goroutines.
func TestFluentQuery_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioFluentQuery)
	if !ok {
		t.Fatal("the fluent-query scenario is not registered in the catalogue")
	}
	if !sc.Mode.Reproducible() {
		t.Fatalf("the fluent-query scenario must be bit-reproducible; mode is %s", sc.Mode)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("fluent-query run: %v", err)
	}
	if report != nil {
		t.Fatalf("fluent-query reported a violation:\n%s", report)
	}
}

// TestFluentQuery_MixedKindTelemetryIsRecordedNotAsserted pins the one thing the
// scenario deliberately does NOT assert.
//
// A float64-bounded [query.WithRange] over an int64-valued property is served by
// the internal numeric companion btree and returns the numeric matches, while
// the scan arm's query.valueInRange requires identical kinds and returns
// nothing. The two paths therefore disagree, which contradicts index_seek.go's
// own statement that they cannot. Which side is wrong is a semantics decision
// for the project owner (openCypher orders integers and floats in one numeric
// order, which argues the SCAN is the wrong side), so the scenario records the
// divergence and pins neither side.
//
// This test asserts only that the telemetry is COLLECTED — that the probe ran
// and its four cardinalities were recorded — and deliberately does not assert
// what the two arms returned. If a future change makes the two arms agree, the
// divergence counter drops to zero and nothing here goes red; only the recorded
// numbers move, which is the correct behaviour for a finding under review.
func TestFluentQuery_MixedKindTelemetryIsRecordedNotAsserted(t *testing.T) {
	t.Parallel()
	sm := newFluentQueryFixture(t, 0x2492_0003)
	probes := NewFluentQueryProbes(NewSeed(0x2492_0003 ^ fluentQueryProbeSeedMix))
	v, err := probes.Check(context.Background(), 1, sm.graph(), sm.engine, sm.oracle, fqPerturbNone)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("the battery reported %d violation(s):\n%s", len(v), renderViolations(v))
	}
	ev := probes.Evidence()
	if ev.MixedKindProbes == 0 {
		t.Fatal("the mixed-kind telemetry probe did not run, so the open finding is not being " +
			"observed at all")
	}
	if ev.MixedKindLastOracle == 0 {
		t.Fatal("the mixed-kind probe's model answer was empty, so the two arms had nothing to " +
			"disagree about and the telemetry is vacuous")
	}
	t.Logf("mixed-kind telemetry (asserted: nothing): probes=%d divergences=%d "+
		"seek=%d scan=%d cypher=%d oracle=%d",
		ev.MixedKindProbes, ev.MixedKindSeekScanDivergences,
		ev.MixedKindLastSeek, ev.MixedKindLastScan, ev.MixedKindLastCypher, ev.MixedKindLastOracle)
}

// TestFluentQuery_SeekEligibilityPreconditionFires proves the precondition that
// underwrites every "the seek arm was index-served" claim.
//
// Served-ness cannot be OBSERVED from outside graph/query, so the scenario
// asserts every condition of query.trySeekProperty / query.trySeekRange plus
// query.indexCovers instead. A precondition asserted but never shown to fail is
// indistinguishable from a comment, so this drives the same battery against a
// graph with no indexes at all and requires the clause to name the miss.
func TestFluentQuery_SeekEligibilityPreconditionFires(t *testing.T) {
	t.Parallel()
	sm := newFluentQueryFixtureOpt(t, 0x2492_0004, false)
	probes := NewFluentQueryProbes(NewSeed(0x2492_0004 ^ fluentQueryProbeSeedMix))
	v, err := probes.Check(context.Background(), 1, sm.graph(), sm.engine, sm.oracle, fqPerturbNone)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	found := false
	for _, x := range v {
		if strings.Contains(x.Op, "precondition:seek-eligibility") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no violation names the seek-eligibility precondition on an index-free graph; got:\n%s",
			renderViolations(v))
	}
	if ev := probes.Evidence(); ev.HashSeekIneligible == 0 || ev.BTreeSeekIneligible == 0 {
		t.Fatalf("the evidence did not record the ineligibility: hash=%d btree=%d",
			ev.HashSeekIneligible, ev.BTreeSeekIneligible)
	}
}

// TestFluentQuery_SubstratePreconditionFires proves the two preconditions that
// make a probe failure ATTRIBUTABLE rather than merely visible.
//
// The parity case removes a node from the MODEL only, so the substrate and the
// model disagree before any probe runs and the battery must say so instead of
// blaming an engine. The uniqueness case builds two nodes carrying the same
// `name`, which would silently collapse two nodes into one expected answer.
func TestFluentQuery_SubstratePreconditionFires(t *testing.T) {
	t.Parallel()

	t.Run("the model diverging from the substrate", func(t *testing.T) {
		t.Parallel()
		sm := newFluentQueryFixture(t, 0x2492_0005)
		// Remove one Person from the MODEL alone. The engine still holds it, so the
		// substrate view and the model disagree.
		names := sm.oracle.NodeNames()
		if len(names) == 0 {
			t.Fatal("the fixture modelled no Person")
		}
		victim := names[0]
		id := sm.oracle.byName[victim]
		delete(sm.oracle.nodes, id)
		delete(sm.oracle.byName, victim)

		probes := NewFluentQueryProbes(NewSeed(0x2492_0005 ^ fluentQueryProbeSeedMix))
		v, err := probes.Check(context.Background(), 1, sm.graph(), sm.engine, sm.oracle, fqPerturbNone)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		found := false
		for _, x := range v {
			if strings.Contains(x.Op, "precondition:substrate-parity") ||
				strings.Contains(x.Op, "precondition:model-shape") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("a model that lost a node the engine still holds was not reported as a "+
				"precondition failure; got:\n%s", renderViolations(v))
		}
	})

	t.Run("two live nodes carrying the same name", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		for _, key := range []string{"k1", "k2"} {
			if err := g.AddNode(key); err != nil {
				t.Fatalf("AddNode(%q): %v", key, err)
			}
			if err := g.SetNodeLabel(key, fluentQueryLabel); err != nil {
				t.Fatalf("SetNodeLabel(%q): %v", key, err)
			}
			if err := g.SetNodeProperty(key, "name", lpg.StringValue("same")); err != nil {
				t.Fatalf("SetNodeProperty(%q): %v", key, err)
			}
		}
		// A third node with no name at all: the other half of the precondition.
		if err := g.AddNode("k3"); err != nil {
			t.Fatalf("AddNode(k3): %v", err)
		}
		if err := g.SetNodeLabel("k3", fluentQueryLabel); err != nil {
			t.Fatalf("SetNodeLabel(k3): %v", err)
		}
		sub := newFluentQuerySubstrate(g)
		if len(sub.duplicateNames) == 0 {
			t.Error("the substrate view accepted two live nodes sharing a name")
		}
		if sub.unnamedLive == 0 {
			t.Error("the substrate view accepted a live node with no name")
		}
	})
}

// TestFluentQuery_SelfConsistencyClauseFires proves the clause that holds the
// three public accessors to each other can fail.
//
// It is a unit case rather than a perturbation because the three accessors read
// the same bitmap and no reachable engine state makes them disagree — the
// clause exists for the day one of them stops doing so (a Collect that drops an
// id whose key no longer resolves through the Mapper is the concrete shape).
func TestFluentQuery_SelfConsistencyClauseFires(t *testing.T) {
	t.Parallel()
	// Cardinality says 3, Collect returned 2, NodeIDs yielded 3: the shape a
	// working-set id whose Mapper key no longer resolves would produce.
	obs := fqObservation{names: map[string]struct{}{}, cardinality: 3, collected: 2, iterated: 3}
	v := fqSelfConsistency(9, "unit", obs)
	if len(v) == 0 || !strings.Contains(v[0].Op, "self-consistency") {
		t.Fatalf("the self-consistency clause did not fire on disagreeing accessors; got:\n%s",
			renderViolations(v))
	}
	// And it must be SILENT when they agree, or it would fire on every probe.
	obs = fqObservation{names: map[string]struct{}{}, cardinality: 3, collected: 3, iterated: 3}
	if v := fqSelfConsistency(9, "unit", obs); len(v) != 0 {
		t.Fatalf("the self-consistency clause fired on agreeing accessors:\n%s", renderViolations(v))
	}
}

// TestFluentQuery_ForcedCrashArmSuppliesPostRecoveryCoverage proves the
// CONSTRUCTED crash arm works and is reached.
//
// The post-recovery battery is a coverage claim, and gating it on a crash the
// seeded schedule happened to draw would fail runs whose seed simply did not
// crash inside the budget — a non-vacuity gate failing a run whose precondition
// was never constructed, which is the shape #2596 removed from another scenario.
// So the loop forces one when none fired. This case disables the schedule
// outright, which is the only way to make the forced arm the one that runs
// deterministically: with the schedule on, whether it fires in a small budget is
// seed-dependent and an arm only some seeds reach is an arm no test can pin.
func TestFluentQuery_ForcedCrashArmSuppliesPostRecoveryCoverage(t *testing.T) {
	t.Parallel()
	cfg := DefaultFluentQueryConfig(fluentQueryDefaultSeed)
	cfg.MaxTicks = 60
	cfg.Crash = CrashConfig{} // no scheduled crashes at all

	ev, report, err := RunFluentQuery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("a run whose only crash is the forced one reported a violation:\n%s", report)
	}
	if ev.BatteriesAfterRecovery != 1 {
		t.Fatalf("the forced crash arm did not supply exactly one post-recovery battery: got %d\n%s",
			ev.BatteriesAfterRecovery, ev.String())
	}
	// And the run must still be non-vacuous on the axes the schedule was not
	// carrying: the prologue's constructed churn is what guarantees the tombstone
	// axis without the workload having to draw a DELETE.
	if ev.ChurnCycles == 0 || ev.MaxTombstoneCount == 0 {
		t.Fatalf("the constructed prologue churn did not run: churn=%d tombstones=%d\n%s",
			ev.ChurnCycles, ev.MaxTombstoneCount, ev.String())
	}
	t.Logf("forced-crash run: %s", ev.String())
}

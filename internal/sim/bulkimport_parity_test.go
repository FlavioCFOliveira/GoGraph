package sim

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestBulkImportParity_Scenario_Passes runs the registered scenario in its own
// configuration: a seed-derived labelled property multigraph published through
// bulkimport and reopened through real recovery must equal the harness model
// exactly, and the package's lifecycle contract must still hold.
func TestBulkImportParity_Scenario_Passes(t *testing.T) {
	t.Parallel()
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBulkImportParity)
	if !ok {
		t.Fatalf("bulkimport-parity scenario not registered")
	}
	if !sc.Mode.Reproducible() {
		t.Fatalf("bulkimport-parity mode is %s, want a bit-reproducible mode", sc.Mode)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("bulkimport-parity run: %v", err)
	}
	if report != nil {
		t.Fatalf("bulkimport-parity reported a violation:\n%s", report)
	}
}

// TestBulkImportParity_NonVacuous is the measured-evidence gate. It asserts the
// run really published a non-trivial graph and really compared it, with numbers
// read off the durable image and the parity pass rather than inferred. A
// scenario that silently degenerated — an empty fixture, a checker that skipped
// its work, a publish that wrote nothing — passes the happy test above and fails
// here.
func TestBulkImportParity_NonVacuous(t *testing.T) {
	t.Parallel()
	sc := bulkImportParityScenario()
	ev, report, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed, defaultBulkImportOptions())
	if err != nil {
		t.Fatalf("runBulkImportParityWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}

	t.Logf("publish: stats=%+v storeEntries=%v snapshotFiles=%d snapshotBytes=%d",
		ev.stats, ev.storeEntries, ev.snapshotFiles, ev.snapshotBytes)
	t.Logf("reopen 1: snapshotHit=%t walOps=%d schemaVersion=%d liveOrder=%d",
		ev.snapshotHit, ev.walOps, ev.schemaVersion, ev.liveOrder)
	t.Logf("reopen 2: snapshotHit=%t walOps=%d liveOrder=%d",
		ev.reopenSnapshotHit, ev.reopenWALOps, ev.reopenLiveOrder)
	t.Logf("parity: nodes=%d labels=%d props=%d pairs=%d handles=%d typed=%d parallelPairs=%d",
		ev.nodesChecked, ev.labelsChecked, ev.propsChecked,
		ev.pairsChecked, ev.handlesChecked, ev.handlesTyped, ev.parallelPairsSeen)
	t.Logf("crashed import: assemblyBytesPre=%d snapshotHit=%t liveOrder=%d assemblyRemoved=%t",
		ev.crashedAssemblyBytesPre, ev.crashedSnapshotHit, ev.crashedLiveOrder, ev.crashedAssemblyRemoved)

	// The publish actually wrote a store.
	if want := []string{"snapshot"}; !slices.Equal(ev.storeEntries, want) {
		t.Errorf("store directory holds %v, want exactly %v", ev.storeEntries, want)
	}
	if ev.snapshotDirBase != "snapshot" {
		t.Errorf("PublishResult.SnapshotDir basename = %q, want %q — recovery reads that name and no other",
			ev.snapshotDirBase, "snapshot")
	}
	if ev.snapshotFiles == 0 {
		t.Error("the published snapshot holds no files: the publish wrote nothing")
	}
	if ev.snapshotBytes < bulkImportMinSnapshotBytes {
		t.Errorf("the published snapshot is %d bytes, want at least %d — the fixture degenerated",
			ev.snapshotBytes, bulkImportMinSnapshotBytes)
	}
	// The determinism digest must cover real components. A digest taken over an
	// empty file set is a constant, and would make TestBulkImportParity_Deterministic
	// pass without comparing anything.
	if len(ev.dataComponents) < bulkImportMinDataComponents {
		t.Errorf("determinism digest covers %d components (%v), want at least %d — "+
			"a digest over nothing compares equal to itself and proves nothing",
			len(ev.dataComponents), ev.dataComponents, bulkImportMinDataComponents)
	}
	if slices.Contains(ev.dataComponents, bulkImportManifestName) {
		t.Errorf("determinism digest includes %s, which carries a wall-clock created_at "+
			"and would make the digest flake", bulkImportManifestName)
	}
	if ev.dataDigest == "" {
		t.Error("determinism digest is empty")
	}

	// The graph published is non-trivial, on both axes.
	if ev.stats.Nodes < bulkImportMinNodes {
		t.Errorf("Stats.Nodes = %d, want at least %d", ev.stats.Nodes, bulkImportMinNodes)
	}
	if ev.stats.Edges < bulkImportMinEdges {
		t.Errorf("Stats.Edges = %d, want at least %d", ev.stats.Edges, bulkImportMinEdges)
	}
	if ev.stats.NodeRecords <= ev.stats.Nodes {
		t.Errorf("Stats.NodeRecords = %d, Stats.Nodes = %d — the repeated-key merge path was never driven",
			ev.stats.NodeRecords, ev.stats.Nodes)
	}
	if got, want := ev.liveOrder, uint64(ev.stats.Nodes); got != want {
		t.Errorf("recovered live order %d, publish reported %d nodes", got, want)
	}

	// The parity pass really compared what it claims to have compared.
	if ev.nodesChecked != ev.stats.Nodes {
		t.Errorf("parity checked %d nodes, the publish reported %d", ev.nodesChecked, ev.stats.Nodes)
	}
	if ev.propsChecked < bulkImportMinPropsChecked {
		t.Errorf("parity compared %d node properties, want at least %d", ev.propsChecked, bulkImportMinPropsChecked)
	}
	if ev.labelsChecked == 0 {
		t.Error("parity compared no labels")
	}
	if ev.handlesChecked != ev.stats.Edges {
		t.Errorf("parity walked %d edge handles, the publish reported %d edges",
			ev.handlesChecked, ev.stats.Edges)
	}
	if ev.handlesTyped < bulkImportMinHandlesTyped {
		t.Errorf("only %d handles carried a relationship type back, want at least %d",
			ev.handlesTyped, bulkImportMinHandlesTyped)
	}
	if ev.parallelPairsSeen == 0 {
		t.Error("no pair came back with two or more instances: per-handle carriage was never proven")
	}

	// The snapshot is self-sufficient on BOTH opens.
	if !ev.snapshotHit || !ev.reopenSnapshotHit {
		t.Errorf("snapshotHit = %t / %t on the two reopens, want both true", ev.snapshotHit, ev.reopenSnapshotHit)
	}
	if ev.walOps != 0 || ev.reopenWALOps != 0 {
		t.Errorf("WAL ops = %d / %d on the two reopens, want 0 — the snapshot must carry every byte",
			ev.walOps, ev.reopenWALOps)
	}
	if ev.reopenLiveOrder != ev.liveOrder {
		t.Errorf("live order changed between reopens: %d then %d", ev.liveOrder, ev.reopenLiveOrder)
	}

	// The crashed-import arm staged real debris before measuring its removal.
	if ev.crashedAssemblyBytesPre < bulkImportMinSnapshotBytes {
		t.Errorf("staged assembly was %d bytes, want at least %d — the arm proved nothing",
			ev.crashedAssemblyBytesPre, bulkImportMinSnapshotBytes)
	}
	if !ev.crashedAssemblyRemoved {
		t.Error("recovery left the assembly directory behind")
	}
}

// TestBulkImportParity_LifecycleContract pins the bulkimport lifecycle contract
// as MEASURED. Every field here was observed by a probe in the run, not read off
// the documentation, so the assertions below are what the package does today.
func TestBulkImportParity_LifecycleContract(t *testing.T) {
	t.Parallel()
	sc := bulkImportParityScenario()
	ev, report, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed, defaultBulkImportOptions())
	if err != nil {
		t.Fatalf("runBulkImportParityWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}

	lc := ev.lifecycle
	for _, tc := range []struct {
		name string
		got  bool
	}{
		{"Builder.Graph() is nil before Finish", lc.graphNilBeforeFinish},
		{"Publish(unfinished) is ErrNotFinished", lc.publishRefusesUnfinished},
		{"AddNode after Finish is ErrFinished", lc.addNodeAfterFinish},
		{"AddEdge after Finish is ErrFinished", lc.addEdgeAfterFinish},
		{"second Finish is ErrFinished", lc.secondFinishIsErrFinished},
		{"second Finish keeps the accumulated stats", lc.secondFinishKeepsStats},
		{"Publish into a non-empty dir is ErrStoreNotEmpty", lc.publishRefusesNonEmpty},
		{"ImportInto a non-empty dir is ErrStoreNotEmpty", lc.importIntoRefusesNonEmpty},
		{"Publish creates an absent dir", lc.publishAcceptsAbsentDir},
		{"Publish(nil builder) is neither sentinel", lc.nilBuilderIsNotSentinel},
		{"unfinished-builder check precedes the context check", lc.unfinishedBeatsCancelledCtx},
		{"context check precedes the directory check", lc.cancelledCtxBeatsNonEmptyDir},
		{"ImportInto inspects the dir before building", lc.importIntoDirCheckBeatsBuild},
		{"PublishResult.Stats equals the model's counts", lc.statsMatchModel},
		{"PublishResult.SnapshotDir is <storeDir>/snapshot", lc.snapshotDirIsTheRecoveredName},
	} {
		if !tc.got {
			t.Errorf("lifecycle contract: %s — NOT observed", tc.name)
		}
	}
}

// TestBulkImportParity_Deterministic asserts the run is a pure function of the
// seed: two runs produce the same publish counts, the same parity counters, and
// a BYTE-IDENTICAL set of snapshot data components, even though each ran in a
// different temporary directory.
//
// Two things are deliberately NOT asserted here, both because they are measured
// to be untrue rather than because they are hard:
//
//   - The snapshot's TOTAL byte count, because manifest.json carries a
//     `created_at` wall clock whose rendering drops a trailing zero about one run
//     in ten (a measured 654-vs-655-byte swing).
//   - Byte-identity of the data components, because the importer writes each
//     item's properties in Go map iteration order and
//     [bulkimport.Node.Properties] is a map. The combined SIZE is invariant —
//     the same keys are written either way — but the bytes are not.
//     TestBulkImportParity_ByteBoundary pins exactly where that begins.
//
// Asserting either would be asserting that a clock, or a map walk, is constant:
// a flake, not a property.
func TestBulkImportParity_Deterministic(t *testing.T) {
	t.Parallel()
	sc := bulkImportParityScenario()
	a, ra, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed, defaultBulkImportOptions())
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	b, rb, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed, defaultBulkImportOptions())
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	if ra != nil || rb != nil {
		t.Fatalf("a clean seed reported a violation: %v / %v", ra, rb)
	}
	if a.stats != b.stats {
		t.Errorf("stats differ across runs of the same seed: %+v vs %+v", a.stats, b.stats)
	}
	if a.snapshotFiles != b.snapshotFiles {
		t.Errorf("published file count differs across runs of the same seed: %d vs %d",
			a.snapshotFiles, b.snapshotFiles)
	}
	if !slices.Equal(a.dataComponents, b.dataComponents) {
		t.Errorf("published data components differ across runs of the same seed: %v vs %v",
			a.dataComponents, b.dataComponents)
	}
	if a.dataBytes != b.dataBytes {
		t.Errorf("snapshot data components differ in SIZE across runs of the same seed: %d vs %d bytes — "+
			"property map order changes the byte layout but must not change how much is written",
			a.dataBytes, b.dataBytes)
	}
	t.Logf("data components %v: %d bytes both runs; digests %s / %s (manifest.json excluded — wall-clock created_at)",
		a.dataComponents, a.dataBytes, a.dataDigest[:16], b.dataDigest[:16])
	if a.handlesChecked != b.handlesChecked || a.pairsChecked != b.pairsChecked ||
		a.propsChecked != b.propsChecked || a.parallelPairsSeen != b.parallelPairsSeen {
		t.Errorf("parity counters differ across runs of the same seed: %d/%d/%d/%d vs %d/%d/%d/%d",
			a.handlesChecked, a.pairsChecked, a.propsChecked, a.parallelPairsSeen,
			b.handlesChecked, b.pairsChecked, b.propsChecked, b.parallelPairsSeen)
	}
}

// TestBulkImportParity_ByteBoundary records where byte-reproducibility of a
// bulk-import publish begins and ends, AS MEASURED under rmp #2466.
//
// The finding: publishing the identical record slices twice produces data
// components with the same names and the same sizes but DIFFERENT bytes, and the
// whole of that divergence is Go map iteration over the `Properties` maps.
// Stripping properties entirely, or reducing each item to exactly one property,
// makes the publish byte-identical. Publishing the same slices twice in one
// process is already enough to diverge, which rules out the fixture's own
// construction, a timestamp, or an address.
//
// This is NOT a correctness defect: `bulkimport.Node` documents that properties
// are set in unspecified map order and that "each key is written once, so no
// ordering can change the result", and the parity pass re-proves that logical
// claim on every run. What is not promised, and is not true, is byte-identity of
// the physical image — so two imports of identical data cannot be compared by
// checksum, and bulk-import snapshots will not deduplicate in content-addressed
// storage.
//
// The test exists so that if any of the three regimes flips, it is noticed. A
// flip to "multi-property is stable" would be an improvement (property keys
// presumably ordered) and requires this documentation to be updated, not
// preserved.
func TestBulkImportParity_ByteBoundary(t *testing.T) {
	t.Parallel()
	sc := bulkImportParityScenario()
	ev, report, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed, defaultBulkImportOptions())
	if err != nil {
		t.Fatalf("runBulkImportParityWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}

	b := ev.byteBoundary
	t.Logf("byte-reproducibility of an identical republish: multi-property=%t (after %d attempt(s)), "+
		"no-properties=%t, single-property=%t",
		b.multiStable, b.attempts, b.noneStable, b.singleStable)

	if b.attempts == 0 {
		t.Fatal("the byte-boundary arm never ran")
	}
	if b.multiStable {
		t.Error("multi-property republish is byte-identical: the documented boundary has moved " +
			"(likely an improvement) and this scenario's documentation is now stale")
	}
	if !b.noneStable {
		t.Error("a property-free republish is not byte-identical: a second source of " +
			"non-determinism has appeared in the publish path")
	}
	if !b.singleStable {
		t.Error("a single-property republish is not byte-identical: the divergence is no longer " +
			"explained by property map order alone")
	}
}

// TestBulkImportParity_Sensitivity proves the parity check can FAIL. Each case
// perturbs the MODEL — never the durable image — so the published graph is
// exactly what the passing run publishes and the only variable is whether the
// checker can see the difference. A guard that cannot fail proves nothing; this
// is the test that establishes it can.
//
// The KindSwap case is the one that kills a textual tautology: it rebinds a
// property to the same DIGITS under a different type, so a comparison that
// rendered values without their kind would still match.
func TestBulkImportParity_Sensitivity(t *testing.T) {
	t.Parallel()
	sc := bulkImportParityScenario()

	for _, tc := range []struct {
		perturb func(t *testing.T, m *bulkImportModel)
		name    string
		wantIn  string
	}{
		{
			name:   "MissingNode",
			wantIn: "live order",
			perturb: func(t *testing.T, m *bulkImportModel) {
				key := bulkImportTestNodeKey(t, m, false)
				delete(m.nodes, key)
			},
		},
		{
			name:   "ExtraNode",
			wantIn: "is absent after recovery",
			perturb: func(_ *testing.T, m *bulkImportModel) {
				m.nodes["phantom"] = &bulkImportModelNode{
					labels: map[string]struct{}{},
					props:  map[string]lpg.PropertyValue{},
				}
			},
		},
		{
			name:   "WrongLabel",
			wantIn: "labels",
			perturb: func(t *testing.T, m *bulkImportModel) {
				key := bulkImportTestNodeKey(t, m, true)
				m.nodes[key].labels = map[string]struct{}{"NotThisLabel": {}}
			},
		},
		{
			name:   "WrongPropertyValue",
			wantIn: "properties",
			perturb: func(t *testing.T, m *bulkImportModel) {
				key := bulkImportTestNodeKey(t, m, true)
				m.nodes[key].props["id"] = lpg.Int64Value(-999)
			},
		},
		{
			name:   "KindSwapSameDigits",
			wantIn: "properties",
			perturb: func(t *testing.T, m *bulkImportModel) {
				key := bulkImportTestNodeKey(t, m, true)
				pv, ok := m.nodes[key].props["id"]
				if !ok {
					t.Fatalf("fixture node %q has no id property to swap", key)
				}
				n, ok := pv.Int64()
				if !ok {
					t.Fatalf("fixture node %q id is not an int64", key)
				}
				// Same digits, different type. A kind-blind comparison matches.
				m.nodes[key].props["id"] = lpg.StringValue(strconv.FormatInt(n, 10))
			},
		},
		{
			name:   "MissingProperty",
			wantIn: "properties",
			perturb: func(t *testing.T, m *bulkImportModel) {
				key := bulkImportTestNodeKey(t, m, true)
				delete(m.nodes[key].props, "id")
			},
		},
		{
			name:   "MissingEdge",
			wantIn: "edge multiset",
			perturb: func(t *testing.T, m *bulkImportModel) {
				p := bulkImportTestPair(t, m, 1)
				m.edges[p] = m.edges[p][1:]
				if len(m.edges[p]) == 0 {
					delete(m.edges, p)
				}
			},
		},
		{
			name:   "PhantomEdge",
			wantIn: "edge multiset",
			perturb: func(t *testing.T, m *bulkImportModel) {
				p := bulkImportTestPair(t, m, 1)
				m.edges[p] = append(m.edges[p], bulkImportModelEdge{
					typ: "NEVER_IMPORTED", weight: 7,
					props: map[string]lpg.PropertyValue{"x": lpg.Int64Value(1)},
				})
			},
		},
		{
			name:   "CollapsedParallelEdges",
			wantIn: "edge multiset",
			perturb: func(t *testing.T, m *bulkImportModel) {
				p := bulkImportTestPair(t, m, 2)
				m.edges[p] = m.edges[p][:1] // model one instance where the graph has two
			},
		},
		{
			name:   "WrongEdgeType",
			wantIn: "edge multiset",
			perturb: func(t *testing.T, m *bulkImportModel) {
				p := bulkImportTestPair(t, m, 1)
				m.edges[p][0].typ = "NOT_THIS_TYPE"
			},
		},
		{
			name:   "WrongEdgeWeight",
			wantIn: "edge multiset",
			perturb: func(t *testing.T, m *bulkImportModel) {
				p := bulkImportTestPair(t, m, 1)
				m.edges[p][0].weight += 12345
			},
		},
		{
			name:   "WrongEdgeProperty",
			wantIn: "edge multiset",
			perturb: func(t *testing.T, m *bulkImportModel) {
				p := bulkImportTestPair(t, m, 1)
				m.edges[p][0].props = map[string]lpg.PropertyValue{"bogus": lpg.StringValue("nope")}
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, report, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed,
				bulkImportOptions{perturb: func(m *bulkImportModel) { tc.perturb(t, m) }})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if report == nil {
				t.Fatalf("perturbation %q produced NO violation: the parity check cannot see it, "+
					"so a passing run proves nothing about this dimension", tc.name)
			}
			joined := violationMessages(report)
			if !strings.Contains(joined, tc.wantIn) {
				t.Fatalf("perturbation %q fired, but on the wrong dimension.\nwant a message containing %q\ngot:\n%s",
					tc.name, tc.wantIn, joined)
			}
			t.Logf("%s -> %d violation(s); first: %s", tc.name, len(report.Violations), report.Violations[0].Message)
		})
	}
}

// bulkImportTestNodeKey returns a deterministic model node key. When labelled is
// true it returns one that carries at least one label and an "id" property, so a
// perturbation of either has something to perturb.
func bulkImportTestNodeKey(t *testing.T, m *bulkImportModel, labelled bool) string {
	t.Helper()
	keys := make([]string, 0, len(m.nodes))
	for k := range m.nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := m.nodes[k]
		if !labelled {
			return k
		}
		if _, ok := n.props["id"]; ok && len(n.labels) > 0 {
			return k
		}
	}
	t.Fatalf("no suitable node in the model (labelled=%t)", labelled)
	return ""
}

// bulkImportTestPair returns the first pair, in sorted order, holding at least
// minInstances edge instances.
func bulkImportTestPair(t *testing.T, m *bulkImportModel, minInstances int) bulkImportPair {
	t.Helper()
	pairs := make([]bulkImportPair, 0, len(m.edges))
	for p := range m.edges {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].src != pairs[j].src {
			return pairs[i].src < pairs[j].src
		}
		return pairs[i].dst < pairs[j].dst
	})
	for _, p := range pairs {
		if len(m.edges[p]) >= minInstances {
			return p
		}
	}
	t.Fatalf("no pair in the model holds at least %d instances", minInstances)
	return bulkImportPair{}
}

// violationMessages joins a report's violation messages for substring assertion.
func violationMessages(r *SimReport) string {
	msgs := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		msgs = append(msgs, string(v.Kind)+": "+v.Message)
	}
	return strings.Join(msgs, "\n")
}

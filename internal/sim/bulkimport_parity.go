package sim

// bulkimport_parity.go — offline bulk-import publication, round-tripped through
// real recovery (rmp #2466).
//
// [bulkimport.Publish] is the one write path in the module that builds a store
// from nothing: it assembles a labelled property graph in memory, writes it as
// the store's snapshot, and hands the directory to recovery. Nothing in the DST
// touched it. The `bulk-vs-online` scenario drives store/bulk, which is a
// different package with a different record — adjacency only, (src, dst, weight),
// no labels and no properties — so every label, every property, every
// relationship type and every parallel-edge handle that bulkimport carries was
// unexercised by simulation.
//
// This scenario occupies that gap. It builds a seed-derived graph through
// [bulkimport.Builder], publishes it to a real temporary directory, reopens the
// directory through [recovery.Open], and requires the recovered graph to equal a
// harness-side model EXACTLY — the node set, the labels on every node, the
// properties on every node (kind AND value), and the per-handle multiset of
// (type, properties, weight) on every pair, including the parallel twins a
// pair-addressed carriage would collapse.
//
// # The fault regimes this scenario CANNOT reach, and why
//
// Read this before assuming bulk-import publication is fault-covered. It is not.
//
// Every other durability scenario in this package injects its faults through
// [SimDisk], which reaches the persistence packages via their filesystem seams
// (`wal.OpenFS`, `recovery.OpenFS`, `snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS`
// and siblings). [bulkimport.Publish] has NO such seam: it calls `os.MkdirAll`
// and `os.ReadDir` directly and writes through the NON-seamed
// [snapshot.WriteSnapshotFullCtx], and [bulkimport.ImportInto] takes a
// `storeDir string` plus an `Options` that carries no filesystem. There is
// therefore no way to put a SimDisk underneath a publish without changing the
// production API, which is out of scope here and filed for a user decision as
// rmp #2518.
//
// Concretely, the following are UNREACHABLE by this scenario, and no test below
// should be read as covering them:
//
//   - ENOSPC part-way through writing the snapshot components.
//   - A failing fsync on a component file, on the staging directory, or on the
//     store's parent directory.
//   - A failing or crash-interrupted rename of `snapshot.tmp` to `snapshot` —
//     the exact instant [bulkimport.Publish]'s atomicity claim rests on.
//   - A crash landing INSIDE the publish window, with the crash-window
//     non-determinism (`ArmRenameWritebackForPath`) that
//     `checkpoint-crash-storm` uses to select which dirent survived.
//
// What IS reachable against a real directory, and is exercised below, is the
// publish's OUTCOME state rather than its interruption: the parity of the
// published image, the reopen being repeatable, and the durable state a crashed
// import leaves behind — RECONSTRUCTED by moving a completed snapshot to the
// assembly name, which is byte-for-byte what a crash between assembly and rename
// leaves, but is not an interrupted publish. The distinction matters: this
// scenario proves recovery's treatment of that state, not the writer's behaviour
// while reaching it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ScenarioBulkImportParity is the catalogue key of the bulk-import publication
// parity scenario.
const ScenarioBulkImportParity = "bulkimport-parity"

// The fixture's shape. It is sized so the published snapshot is a genuinely
// non-trivial multigraph with every property kind represented, while the whole
// scenario stays a small fraction of the short-layer budget.
const (
	// bulkImportNodes is the number of DISTINCT node keys.
	bulkImportNodes = 400
	// bulkImportEdgeRecords is the number of ordinary (non-twin) edge records.
	bulkImportEdgeRecords = 1200
	// bulkImportParallelPairs is the number of pairs that additionally carry TWO
	// typed twins each, so per-handle carriage is proven to survive the round
	// trip rather than being collapsed to one edge per pair.
	bulkImportParallelPairs = 40
	// bulkImportDupRecords is the number of REPEATED node records, each adding a
	// label and a property to a key an earlier record already created. It makes
	// Stats.NodeRecords exceed Stats.Nodes, which is the merge semantics the
	// package documents for a CSV split across files.
	bulkImportDupRecords = 60
	// bulkImportBareEvery makes every Nth node bare — no label, no property — so
	// the "a node with neither is still created" clause is exercised.
	bulkImportBareEvery = 37
)

// Non-vacuity floors. They are deliberately BELOW the fixture's exact counts:
// the exact counts are asserted against the model elsewhere, and these exist so
// a fixture that silently degenerated to a handful of records cannot pass as
// coverage.
const (
	bulkImportMinNodes         = 300
	bulkImportMinEdges         = 1100
	bulkImportMinSnapshotBytes = 4096
	bulkImportMinPropsChecked  = 500
	bulkImportMinHandlesTyped  = 900
	// bulkImportMinDataComponents is the floor on the number of snapshot data
	// components the determinism digest must cover. The v3 layout publishes five
	// (csr, labels, properties, mapper, edgehandles); the floor guards against a
	// digest taken over an empty file set, which would compare equal to itself.
	bulkImportMinDataComponents = 5
)

// bulkImportLabels and bulkImportTypes are the label and relationship-type
// vocabularies. Both are small so every value recurs many times.
var (
	bulkImportLabels = []string{"Person", "Robot", "Ghost"}
	bulkImportTypes  = []string{"KNOWS", "FOLLOWS", "LIKES"}
)

// bulkImportModelNode is one node in the harness model: the labels and
// properties the published graph must carry for that key.
type bulkImportModelNode struct {
	props  map[string]lpg.PropertyValue
	labels map[string]struct{}
}

// bulkImportModelEdge is one edge instance in the harness model. It is held per
// (src, dst) pair as a MULTISET, because a multigraph pair may carry several
// instances and the importer attaches type and properties to the edge HANDLE,
// so only the multiset — never the pair — is a well-defined comparison.
type bulkImportModelEdge struct {
	props  map[string]lpg.PropertyValue
	typ    string
	weight int64
}

// bulkImportPair keys the edge multiset.
type bulkImportPair struct {
	src string
	dst string
}

// bulkImportModel is the shadow model the recovered graph is adjudicated
// against. It is built by the same pass that emits the records, from the same
// seed draws, but it is an independent structure: nothing in it is read back
// from the builder, the graph, or the snapshot.
type bulkImportModel struct {
	nodes map[string]*bulkImportModelNode
	edges map[bulkImportPair][]bulkImportModelEdge
	// nodeRecords is the number of node records emitted, which exceeds
	// len(nodes) by exactly the number of repeats.
	nodeRecords int
	// edgeRecords is the number of edge records emitted.
	edgeRecords int
}

// totalEdges returns the number of edge instances across every pair.
func (m *bulkImportModel) totalEdges() int {
	n := 0
	for _, es := range m.edges {
		n += len(es)
	}
	return n
}

// bulkImportKey renders the natural key of node i.
func bulkImportKey(i int) string { return fmt.Sprintf("bi%06d", i) }

// buildBulkImportFixture draws the seed-derived shape once, emitting the
// importer's records and the model together so the two cannot drift.
//
// The draw order is fixed and every value comes from s, so the fixture is a pure
// function of the seed.
func buildBulkImportFixture(s *Seed) (*bulkImportModel, []bulkimport.Node, []bulkimport.Edge[int64]) {
	m := &bulkImportModel{
		nodes: make(map[string]*bulkImportModelNode, bulkImportNodes),
		edges: make(map[bulkImportPair][]bulkImportModelEdge, bulkImportNodes),
	}
	nodes := make([]bulkimport.Node, 0, bulkImportNodes+bulkImportDupRecords)
	edges := make([]bulkimport.Edge[int64], 0, bulkImportEdgeRecords+2*bulkImportParallelPairs)

	// --- Nodes ---
	for i := 0; i < bulkImportNodes; i++ {
		key := bulkImportKey(i)
		mn := &bulkImportModelNode{
			labels: make(map[string]struct{}, 2),
			props:  make(map[string]lpg.PropertyValue, 4),
		}
		rec := bulkimport.Node{Key: key}

		if i%bulkImportBareEvery != 0 {
			// Labels: one always, a second on a seed-chosen fraction.
			l0 := bulkImportLabels[s.IntN(len(bulkImportLabels))]
			mn.labels[l0] = struct{}{}
			rec.Labels = append(rec.Labels, l0)
			if s.Bool(0.35) {
				l1 := bulkImportLabels[s.IntN(len(bulkImportLabels))]
				if _, dup := mn.labels[l1]; !dup {
					mn.labels[l1] = struct{}{}
					rec.Labels = append(rec.Labels, l1)
				}
			}
			// Properties: two always, plus a float and a bool on a fraction each,
			// so all four round-tripping scalar kinds are represented.
			rec.Properties = make(map[string]lpg.PropertyValue, 4)
			bulkImportSetProp(mn, rec.Properties, "id", lpg.Int64Value(int64(i)))
			bulkImportSetProp(mn, rec.Properties, "name", lpg.StringValue(fmt.Sprintf("n%06d", i)))
			if s.Bool(0.5) {
				bulkImportSetProp(mn, rec.Properties, "score", lpg.Float64Value(float64(s.IntN(10_000))/100.0))
			}
			if s.Bool(0.5) {
				bulkImportSetProp(mn, rec.Properties, "active", lpg.BoolValue(s.Bool(0.5)))
			}
		}
		m.nodes[key] = mn
		m.nodeRecords++
		nodes = append(nodes, rec)
	}

	// --- Repeated node records: merge, do not replace. ---
	for r := 0; r < bulkImportDupRecords; r++ {
		i := s.IntN(bulkImportNodes)
		key := bulkImportKey(i)
		mn := m.nodes[key]
		lbl := "Repeat"
		mn.labels[lbl] = struct{}{}
		props := map[string]lpg.PropertyValue{
			"repeat": lpg.Int64Value(int64(r)),
		}
		bulkImportSetProp(mn, props, "repeat", lpg.Int64Value(int64(r)))
		m.nodeRecords++
		nodes = append(nodes, bulkimport.Node{Key: key, Labels: []string{lbl}, Properties: props})
	}

	// --- Ordinary edges ---
	for e := 0; e < bulkImportEdgeRecords; e++ {
		src := bulkImportKey(s.IntN(bulkImportNodes))
		dst := bulkImportKey(s.IntN(bulkImportNodes))
		typ := bulkImportTypes[s.IntN(len(bulkImportTypes))]
		w := int64(s.IntN(1000))
		props := map[string]lpg.PropertyValue{"since": lpg.Int64Value(int64(e))}
		if s.Bool(0.4) {
			props["strength"] = lpg.Float64Value(float64(s.IntN(1000)) / 10.0)
		}
		m.addEdge(src, dst, typ, w, props)
		edges = append(edges, bulkimport.Edge[int64]{
			Src: src, Dst: dst, Type: typ, Weight: w,
			Properties: bulkImportCopyProps(props),
		})
	}

	// --- Parallel twins: same pair, DISTINCT types and properties. ---
	for p := 0; p < bulkImportParallelPairs; p++ {
		i := s.IntN(bulkImportNodes)
		src := bulkImportKey(i)
		dst := bulkImportKey((i + 1 + s.IntN(7)) % bulkImportNodes)
		for _, t := range []string{"PAR_A", "PAR_B"} {
			w := int64(s.IntN(1000))
			props := map[string]lpg.PropertyValue{
				"twin": lpg.StringValue(t),
				"pair": lpg.Int64Value(int64(p)),
			}
			m.addEdge(src, dst, t, w, props)
			edges = append(edges, bulkimport.Edge[int64]{
				Src: src, Dst: dst, Type: t, Weight: w,
				Properties: bulkImportCopyProps(props),
			})
		}
	}

	return m, nodes, edges
}

// bulkImportSetProp records a property on both the model node and the record
// map, so a fixture edit cannot update one and forget the other.
func bulkImportSetProp(mn *bulkImportModelNode, rec map[string]lpg.PropertyValue, k string, v lpg.PropertyValue) {
	mn.props[k] = v
	rec[k] = v
}

// bulkImportCopyProps returns an independent copy, so a later model mutation (a
// sensitivity perturbation) cannot reach the records that were published.
func bulkImportCopyProps(in map[string]lpg.PropertyValue) map[string]lpg.PropertyValue {
	out := make(map[string]lpg.PropertyValue, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// addEdge appends one instance to the pair's multiset.
func (m *bulkImportModel) addEdge(src, dst, typ string, w int64, props map[string]lpg.PropertyValue) {
	p := bulkImportPair{src: src, dst: dst}
	m.edges[p] = append(m.edges[p], bulkImportModelEdge{
		typ: typ, weight: w, props: bulkImportCopyProps(props),
	})
	m.edgeRecords++
}

// bulkImportPropKind renders a property value as kind AND value.
//
// The kind is part of the rendering ON PURPOSE. Comparing only the textual value
// would make an integer 7 and the string "7" compare equal, so a round trip that
// lost the type would pass — the class of tautology that makes a guard exist and
// prove nothing. An unrecognised kind renders as a distinct sentinel rather than
// silently matching anything.
func bulkImportPropKind(pv lpg.PropertyValue) string {
	switch pv.Kind() {
	case lpg.PropString:
		v, _ := pv.String()
		return "string:" + v
	case lpg.PropInt64:
		v, _ := pv.Int64()
		return fmt.Sprintf("int64:%d", v)
	case lpg.PropFloat64:
		v, _ := pv.Float64()
		return fmt.Sprintf("float64:%v", v)
	case lpg.PropBool:
		v, _ := pv.Bool()
		return fmt.Sprintf("bool:%t", v)
	default:
		return fmt.Sprintf("kind(%v):<unrenderable>", pv.Kind())
	}
}

// bulkImportPropsCanon renders a property map canonically: keys sorted, each
// value rendered with its kind. Two maps render identically exactly when they
// hold the same keys bound to the same kind and value.
func bulkImportPropsCanon(props map[string]lpg.PropertyValue) string {
	if len(props) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(bulkImportPropKind(props[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// bulkImportEdgeCanon renders one edge instance canonically for multiset
// matching: its type, its weight, and its properties with kinds.
func bulkImportEdgeCanon(typ string, weight int64, props map[string]lpg.PropertyValue) string {
	return typ + "|" + fmt.Sprint(weight) + "|" + bulkImportPropsCanon(props)
}

// bulkImportLabelsCanon renders a label set canonically.
func bulkImportLabelsCanon(labels []string) string {
	if len(labels) == 0 {
		return "[]"
	}
	cp := append([]string(nil), labels...)
	sort.Strings(cp)
	return "[" + strings.Join(cp, ",") + "]"
}

// bulkImportEvidence is what one run MEASURED. Every field is read off the
// published image, the recovered graph, or the package's own return values —
// none is inferred. The non-vacuity test reads it to prove the run did the work
// the scenario claims.
type bulkImportEvidence struct {
	// lifecycle is the measured sentinel behaviour (see bulkImportLifecycle).
	lifecycle bulkImportLifecycle
	// stats is what Publish reported.
	stats bulkimport.Stats
	// snapshotDirBase is the basename of PublishResult.SnapshotDir. Only the
	// basename is kept: the absolute path holds a per-run temporary directory,
	// so recording it would make the evidence — and any report built from it —
	// differ between two runs of the same seed.
	snapshotDirBase string
	// storeEntries are the entry names the store directory held after the
	// publish, sorted.
	storeEntries []string
	// snapshotFiles / snapshotBytes are the file count and total byte size of
	// the published snapshot directory tree. They are the floor proving the
	// publish actually wrote something.
	snapshotFiles int
	snapshotBytes int64
	// dataComponents are the names of the published snapshot's DATA components,
	// sorted; dataBytes is their combined size; dataDigest is a SHA-256 over
	// their names and contents.
	//
	// manifest.json is excluded from all three because it carries a `created_at`
	// wall clock, whose rendering drops a trailing zero about one run in ten — a
	// measured 654-vs-655-byte swing that has nothing to do with the graph.
	//
	// # dataBytes is seed-stable; dataDigest is NOT, and that is the package's
	// behaviour rather than a defect
	//
	// A publish of the same records twice produces data components of the same
	// names and the same sizes, but NOT the same bytes. The cause was isolated by
	// measurement, not inferred: publishing the identical record slices twice in
	// one process already diverges, while the same fixture stripped to NO
	// properties, and the same fixture reduced to exactly ONE property per node
	// and per edge, are both byte-identical.
	//
	// So the divergence is Go map iteration order over
	// [bulkimport.Node.Properties] / [bulkimport.Edge.Properties] whenever an
	// item carries two or more properties. Those fields are maps, so no caller
	// can avoid it. [bulkimport.Node] documents that properties are set "in
	// map-iteration order, which is unspecified. That is safe because each key is
	// written once, so no ordering can change the result" — and that claim holds
	// exactly as written: the LOGICAL result is identical every run, which the
	// parity pass re-proves on every execution of this scenario. What is not
	// promised, and is not true, is byte-identity of the physical image.
	//
	// The practical consequence, worth knowing before relying on it: two imports
	// of identical data cannot be compared by checksum, and bulk-import snapshots
	// will not deduplicate in content-addressed storage.
	// [bulkImportCheckByteBoundary] pins this boundary so a future change to it
	// is noticed rather than assumed.
	dataComponents []string
	dataDigest     string
	dataBytes      int64
	// snapshotHit, walOps, schemaVersion, liveOrder describe the FIRST reopen.
	snapshotHit   bool
	walOps        int
	schemaVersion int
	liveOrder     uint64
	// reopenSnapshotHit, reopenWALOps, reopenLiveOrder describe the SECOND
	// reopen of the same directory.
	reopenSnapshotHit bool
	reopenWALOps      int
	reopenLiveOrder   uint64
	// nodesChecked / propsChecked / labelsChecked count what the parity pass
	// actually compared, so a checker that silently skipped its work cannot
	// pass for one that ran.
	nodesChecked  int
	propsChecked  int
	labelsChecked int
	// pairsChecked / handlesChecked count the edge side. handlesTyped counts the
	// handles that carried at least one relationship type back.
	pairsChecked   int
	handlesChecked int
	handlesTyped   int
	// parallelPairsSeen counts pairs that came back with two or more instances,
	// which is what proves per-handle carriage was not collapsed.
	parallelPairsSeen int
	// crashedSnapshotHit / crashedLiveOrder / crashedAssemblyRemoved describe
	// the reconstructed crashed-import directory (see the file header: this is
	// the crash's OUTCOME state, not an interrupted publish).
	crashedSnapshotHit      bool
	crashedLiveOrder        uint64
	crashedAssemblyRemoved  bool
	crashedAssemblyBytesPre int64
	// byteBoundary is the measured byte-reproducibility of the publish across
	// the three property regimes. See [bulkImportCheckByteBoundary].
	byteBoundary bulkImportByteBoundary
}

// bulkImportLifecycle records the package's lifecycle contract AS MEASURED,
// one field per probe. Every field is true only when the probe observed the
// stated behaviour; the run turns any false into a violation, so a contract that
// changes underneath this scenario fails it rather than being silently absorbed.
type bulkImportLifecycle struct {
	// graphNilBeforeFinish: Builder.Graph() is nil while the commit window is open.
	graphNilBeforeFinish bool
	// publishRefusesUnfinished: Publish(unfinished) is ErrNotFinished.
	publishRefusesUnfinished bool
	// addNodeAfterFinish / addEdgeAfterFinish: both are ErrFinished.
	addNodeAfterFinish bool
	addEdgeAfterFinish bool
	// secondFinishIsErrFinished: a second Finish returns ErrFinished...
	secondFinishIsErrFinished bool
	// ...and still returns the accumulated stats rather than a zero value.
	secondFinishKeepsStats bool
	// publishRefusesNonEmpty / importIntoRefusesNonEmpty: ErrStoreNotEmpty.
	publishRefusesNonEmpty    bool
	importIntoRefusesNonEmpty bool
	// publishAcceptsAbsentDir: an absent directory is created, not refused.
	publishAcceptsAbsentDir bool
	// nilBuilderIsNotSentinel: Publish(nil) fails with a plain error that
	// matches NEITHER ErrNotFinished nor ErrStoreNotEmpty. Measured because the
	// package documents the sentinels and this case is not one of them; a caller
	// switching on sentinels alone would mis-handle it.
	nilBuilderIsNotSentinel bool
	// unfinishedBeatsCancelledCtx: with BOTH an unfinished builder and a
	// cancelled context, Publish reports ErrNotFinished — the builder check runs
	// before the context check.
	unfinishedBeatsCancelledCtx bool
	// cancelledCtxBeatsNonEmptyDir: with a FINISHED builder, a cancelled context
	// and a non-empty directory, Publish reports the context error — the context
	// check runs before the directory check.
	cancelledCtxBeatsNonEmptyDir bool
	// importIntoDirCheckBeatsBuild: ImportInto given a non-empty directory AND a
	// record set that cannot build (an edge whose endpoint was never added)
	// reports ErrStoreNotEmpty — the directory is inspected before any work.
	importIntoDirCheckBeatsBuild bool
	// statsMatchModel: PublishResult.Stats equals the model's own counts.
	statsMatchModel bool
	// snapshotDirIsTheRecoveredName: PublishResult.SnapshotDir is
	// <storeDir>/snapshot, the one name recovery reads.
	snapshotDirIsTheRecoveredName bool
}

// bulkImportOptions parameterises one run. The zero value is NOT the scenario's
// configuration; use [defaultBulkImportOptions].
type bulkImportOptions struct {
	// perturb, when non-nil, is invoked on the MODEL after the graph has been
	// published and reopened, and before the parity pass. It is the SENSITIVITY
	// seam: a test uses it to introduce a discrepancy the parity check must
	// detect. Perturbing the model rather than the graph is deliberate — it
	// leaves the durable image exactly as the scenario publishes it, so what is
	// being measured is the checker's power to see a difference, not the
	// engine's reaction to damage.
	//
	// Nil — the scenario's own configuration — leaves the model untouched.
	perturb func(*bulkImportModel)
}

// defaultBulkImportOptions is the scenario's own configuration: no perturbation.
func defaultBulkImportOptions() bulkImportOptions { return bulkImportOptions{} }

// bulkImportParityScenario registers the bulk-import publication parity
// scenario. It is bit-reproducible: the fixture is drawn from the seed, the
// build and the publish are single-goroutine, and nothing in the run consults a
// clock or a map iteration order that reaches the result.
func bulkImportParityScenario() Scenario {
	return Scenario{
		Name: ScenarioBulkImportParity,
		Description: "offline bulk-import publication round-tripped through real recovery: exact node/label/property/" +
			"per-handle-edge parity against a harness model, plus the measured lifecycle contract " +
			"(fault injection is out of reach — see rmp #2518)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xB01C1770,
		run:         runBulkImportParity,
	}
}

// runBulkImportParity performs one scenario run in its own configuration.
func runBulkImportParity(ctx context.Context, seed uint64) (*SimReport, error) {
	_, report, err := runBulkImportParityWith(ctx, seed, defaultBulkImportOptions())
	return report, err
}

// bulkImportReport builds the scenario's failure report. It carries no path, so
// two runs of the same seed produce the same report text even though each ran in
// a different temporary directory.
func bulkImportReport(seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Seed:       seed,
		Scenario:   ScenarioBulkImportParity,
		Mode:       ModeDeterministic,
		FailedOp:   Op{Kind: OpCreate, Cypher: "<bulk import publish>"},
		Violations: v,
	}
}

// runBulkImportParityWith performs one run under opts and returns what it
// measured alongside the report (nil == passed).
//
// The three arms, in order:
//
//  1. BUILD AND PUBLISH. A seed-derived graph is built through the Builder and
//     published to a real directory, with the lifecycle contract measured on
//     throwaway builders and directories beside it.
//  2. REOPEN AND ADJUDICATE. The directory is reopened through real recovery and
//     the recovered graph is required to equal the model exactly. It is then
//     reopened a SECOND time and adjudicated again, so a reopen that mutated the
//     directory — or a first reopen that happened to be lucky — is caught.
//  3. CRASHED-IMPORT OUTCOME. A completed snapshot is moved to the assembly name
//     in a fresh directory, reproducing the on-disk state a crash between
//     assembly and rename leaves, and recovery must find nothing and remove the
//     debris. See the file header: this is the crash's outcome state, NOT an
//     interrupted publish, which no seam in this package can produce.
//  4. BYTE-REPRODUCIBILITY BOUNDARY. The same records are published twice at
//     three property regimes, pinning where the published image is byte-stable
//     and where it is not (see [bulkImportCheckByteBoundary]).
func runBulkImportParityWith(
	ctx context.Context, seed uint64, opts bulkImportOptions,
) (*bulkImportEvidence, *SimReport, error) {
	root, err := os.MkdirTemp("", "sim-bulkimport-*")
	if err != nil {
		return nil, nil, fmt.Errorf("sim: bulkimport-parity tempdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	ev := &bulkImportEvidence{}
	model, nodes, edges := buildBulkImportFixture(NewSeed(seed))

	// --- Arm 1: build and publish. ---
	storeDir := filepath.Join(root, "store")
	b := bulkimport.New[int64](bulkimport.Options{
		Directed: true, Multigraph: true, ExpectNodes: bulkImportNodes,
	})
	ev.lifecycle.graphNilBeforeFinish = b.Graph() == nil
	ev.lifecycle.publishRefusesUnfinished = bulkImportPublishErrIs(ctx, filepath.Join(root, "never"), b, bulkimport.ErrNotFinished)

	if err := b.AddNodes(nodes); err != nil {
		return ev, nil, fmt.Errorf("sim: bulkimport-parity add nodes: %w", err)
	}
	if err := b.AddEdges(edges); err != nil {
		return ev, nil, fmt.Errorf("sim: bulkimport-parity add edges: %w", err)
	}
	if _, err := b.Finish(); err != nil {
		return ev, nil, fmt.Errorf("sim: bulkimport-parity finish: %w", err)
	}

	res, err := bulkimport.Publish[int64](ctx, storeDir, b)
	if err != nil {
		return ev, nil, fmt.Errorf("sim: bulkimport-parity publish: %w", err)
	}
	ev.stats = res.Stats
	ev.snapshotDirBase = filepath.Base(res.SnapshotDir)
	ev.lifecycle.snapshotDirIsTheRecoveredName = res.SnapshotDir == filepath.Join(storeDir, "snapshot")
	ev.lifecycle.publishAcceptsAbsentDir = true // storeDir did not exist above.
	// Compare Stats against the model's INDEPENDENTLY derived counts: the edge
	// total is folded over the per-pair multisets the parity pass will compare,
	// not read off the running counter, so a model whose two views of its own
	// edge set disagree fails here rather than passing on the cheaper one.
	ev.lifecycle.statsMatchModel = res.Stats.Nodes == len(model.nodes) &&
		res.Stats.Edges == model.totalEdges() &&
		model.totalEdges() == model.edgeRecords &&
		res.Stats.NodeRecords == model.nodeRecords

	if err := bulkImportMeasureImage(storeDir, res.SnapshotDir, ev); err != nil {
		return ev, nil, err
	}
	if err := bulkImportMeasureLifecycle(ctx, root, b, nodes, edges, &ev.lifecycle); err != nil {
		return ev, nil, err
	}

	// --- Arm 2: reopen, adjudicate, reopen again, adjudicate again. ---
	if opts.perturb != nil {
		opts.perturb(model)
	}

	first, err := bulkImportOpen(storeDir)
	if err != nil {
		return ev, nil, err
	}
	ev.snapshotHit, ev.walOps = first.SnapshotHit, first.WALOps
	ev.schemaVersion, ev.liveOrder = first.SnapshotSchemaVersion, first.Graph.LiveOrder()

	v := bulkImportCheckParity(model, first.Graph, ev)
	v = append(v, bulkImportCheckDurableShape(first, "first reopen")...)

	second, err := bulkImportOpen(storeDir)
	if err != nil {
		return ev, nil, err
	}
	ev.reopenSnapshotHit, ev.reopenWALOps = second.SnapshotHit, second.WALOps
	ev.reopenLiveOrder = second.Graph.LiveOrder()

	// Adjudicate the second reopen with a throwaway evidence record, so the
	// counters the non-vacuity test reads describe ONE pass rather than two.
	var again bulkImportEvidence
	v = append(v, bulkImportCheckParity(model, second.Graph, &again)...)
	v = append(v, bulkImportCheckDurableShape(second, "second reopen")...)

	// --- Arm 3: the crashed-import outcome state. ---
	crashViolations, err := bulkImportCheckCrashedImport(ctx, root, nodes, edges, ev)
	if err != nil {
		return ev, nil, err
	}
	v = append(v, crashViolations...)

	// --- Arm 4: the byte-reproducibility boundary. ---
	boundary, boundaryViolations, err := bulkImportCheckByteBoundary(ctx, nodes, edges)
	if err != nil {
		return ev, nil, err
	}
	ev.byteBoundary = boundary
	v = append(v, boundaryViolations...)

	v = append(v, bulkImportCheckLifecycle(&ev.lifecycle)...)

	if len(v) > 0 {
		return ev, bulkImportReport(seed, v), nil
	}
	return ev, nil, nil
}

// bulkImportOpen reopens a published store through the real recovery path.
func bulkImportOpen(dir string) (recovery.Result[string, int64], error) {
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		return res, fmt.Errorf("sim: bulkimport-parity recovery.Open: %w", err)
	}
	if res.Graph == nil {
		return res, fmt.Errorf("sim: bulkimport-parity recovery.Open returned a nil graph")
	}
	return res, nil
}

// bulkImportMeasureImage records what the publish actually wrote: the store
// directory's entries, and the file count and byte size of the snapshot tree.
func bulkImportMeasureImage(storeDir, snapDir string, ev *bulkImportEvidence) error {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return fmt.Errorf("sim: bulkimport-parity read store dir: %w", err)
	}
	for _, e := range entries {
		ev.storeEntries = append(ev.storeEntries, e.Name())
	}
	sort.Strings(ev.storeEntries)

	files, bytes, err := bulkImportTreeSize(snapDir)
	if err != nil {
		return err
	}
	ev.snapshotFiles, ev.snapshotBytes = files, bytes

	img, err := bulkImportMeasureComponents(snapDir)
	if err != nil {
		return err
	}
	ev.dataComponents, ev.dataBytes, ev.dataDigest = img.names, img.bytes, img.digest
	return nil
}

// bulkImportManifestName is the snapshot component excluded from the component
// measurement, because it carries a wall-clock `created_at`.
const bulkImportManifestName = "manifest.json"

// bulkImportImage is a measurement of a published snapshot's DATA components:
// their names, their combined size, and a digest over their names and contents.
type bulkImportImage struct {
	digest string
	names  []string
	bytes  int64
}

// bulkImportMeasureComponents measures the published snapshot's data components
// — every regular file except the manifest — in sorted name order, so the
// directory walk order cannot reach the result.
func bulkImportMeasureComponents(snapDir string) (bulkImportImage, error) {
	var img bulkImportImage
	entries, err := os.ReadDir(snapDir)
	if err != nil {
		return img, fmt.Errorf("sim: bulkimport-parity read snapshot dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || e.Name() == bulkImportManifestName {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		//nolint:gosec // G304: snapDir is the harness's own os.MkdirTemp root and
		// name came from os.ReadDir of that same directory a few lines above —
		// neither is user input, and both are removed before the run returns.
		body, rerr := os.ReadFile(filepath.Join(snapDir, name))
		if rerr != nil {
			return img, fmt.Errorf("sim: bulkimport-parity read component %q: %w", name, rerr)
		}
		_, _ = h.Write([]byte(name)) // hash.Hash never reports an error
		_, _ = h.Write(body)
		img.bytes += int64(len(body))
	}
	img.names = names
	img.digest = hex.EncodeToString(h.Sum(nil))
	return img, nil
}

// bulkImportPublishAndMeasure publishes one record set into a fresh temporary
// directory, measures the resulting data components, and removes the directory.
// It is the vehicle for the byte-reproducibility boundary test.
func bulkImportPublishAndMeasure(
	ctx context.Context, nodes []bulkimport.Node, edges []bulkimport.Edge[int64],
) (bulkImportImage, error) {
	var img bulkImportImage
	root, err := os.MkdirTemp("", "sim-bulkimport-img-*")
	if err != nil {
		return img, fmt.Errorf("sim: bulkimport-parity image tempdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	res, err := bulkimport.ImportInto[int64](ctx, filepath.Join(root, "store"),
		bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges)
	if err != nil {
		return img, fmt.Errorf("sim: bulkimport-parity image publish: %w", err)
	}
	return bulkImportMeasureComponents(res.SnapshotDir)
}

// bulkImportTreeSize walks dir and returns the number of regular files and their
// total size in bytes.
func bulkImportTreeSize(dir string) (files int, bytes int64, err error) {
	walkErr := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files++
		bytes += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("sim: bulkimport-parity walk snapshot: %w", walkErr)
	}
	return files, bytes, nil
}

// bulkImportPublishErrIs reports whether publishing b into dir fails with the
// given sentinel.
func bulkImportPublishErrIs(ctx context.Context, dir string, b *bulkimport.Builder[int64], want error) bool {
	_, err := bulkimport.Publish[int64](ctx, dir, b)
	return errors.Is(err, want)
}

// bulkImportMeasureLifecycle probes the package's lifecycle contract on
// throwaway builders and directories, recording what each probe OBSERVED rather
// than what the documentation claims. `finished` is the run's own finished
// builder, reused for the probes that need one.
func bulkImportMeasureLifecycle(
	ctx context.Context, root string, finished *bulkimport.Builder[int64],
	nodes []bulkimport.Node, edges []bulkimport.Edge[int64], lc *bulkImportLifecycle,
) error {
	// ErrFinished on every ingest route, and on a second Finish.
	lc.addNodeAfterFinish = errors.Is(finished.AddNode(bulkimport.Node{Key: "after"}), bulkimport.ErrFinished)
	lc.addEdgeAfterFinish = errors.Is(
		finished.AddEdge(bulkimport.Edge[int64]{Src: bulkImportKey(0), Dst: bulkImportKey(1)}),
		bulkimport.ErrFinished)
	stats, err := finished.Finish()
	lc.secondFinishIsErrFinished = errors.Is(err, bulkimport.ErrFinished)
	lc.secondFinishKeepsStats = stats.Nodes == bulkImportNodes && stats.Edges > 0

	// ErrStoreNotEmpty, on both entry points. The directory holds a WAL-shaped
	// file, which is the case the refusal exists to prevent: this path writes no
	// WAL, so recovery would replay an existing one on top of the new snapshot.
	occupied := filepath.Join(root, "occupied")
	if err := os.MkdirAll(occupied, 0o750); err != nil {
		return fmt.Errorf("sim: bulkimport-parity occupied dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "wal.log"), []byte("stale"), 0o600); err != nil {
		return fmt.Errorf("sim: bulkimport-parity occupied file: %w", err)
	}
	lc.publishRefusesNonEmpty = bulkImportPublishErrIs(ctx, occupied, finished, bulkimport.ErrStoreNotEmpty)
	_, ierr := bulkimport.ImportInto[int64](ctx, occupied,
		bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges)
	lc.importIntoRefusesNonEmpty = errors.Is(ierr, bulkimport.ErrStoreNotEmpty)

	// A nil builder is refused, but NOT with either sentinel.
	_, nerr := bulkimport.Publish[int64](ctx, filepath.Join(root, "nilbuilder"), nil)
	lc.nilBuilderIsNotSentinel = nerr != nil &&
		!errors.Is(nerr, bulkimport.ErrNotFinished) &&
		!errors.Is(nerr, bulkimport.ErrStoreNotEmpty)

	// Precedence: which check runs first when two would fire.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	open := bulkimport.New[int64](bulkimport.Options{Directed: true})
	_, uerr := bulkimport.Publish[int64](cancelled, filepath.Join(root, "unfinished"), open)
	lc.unfinishedBeatsCancelledCtx = errors.Is(uerr, bulkimport.ErrNotFinished)
	if _, err := open.Finish(); err != nil {
		return fmt.Errorf("sim: bulkimport-parity finish probe builder: %w", err)
	}
	_, cerr := bulkimport.Publish[int64](cancelled, occupied, open)
	lc.cancelledCtxBeatsNonEmptyDir = errors.Is(cerr, context.Canceled)

	// ImportInto inspects the directory before doing any work: given BOTH a
	// non-empty directory and a record set that cannot build, it reports the
	// directory, not the build failure.
	badEdges := []bulkimport.Edge[int64]{{Src: "absent-src", Dst: "absent-dst"}}
	_, berr := bulkimport.ImportInto[int64](ctx, occupied,
		bulkimport.Options{Directed: true}, nil, badEdges)
	lc.importIntoDirCheckBeatsBuild = errors.Is(berr, bulkimport.ErrStoreNotEmpty)

	return nil
}

// bulkImportCheckLifecycle turns every lifecycle probe that did NOT observe the
// contract into a violation. Each message names the probe, so a failure says
// which clause of the contract moved.
func bulkImportCheckLifecycle(lc *bulkImportLifecycle) []Violation {
	probes := []struct {
		name string
		ok   bool
	}{
		{"Builder.Graph() is nil before Finish", lc.graphNilBeforeFinish},
		{"Publish(unfinished builder) is ErrNotFinished", lc.publishRefusesUnfinished},
		{"AddNode after Finish is ErrFinished", lc.addNodeAfterFinish},
		{"AddEdge after Finish is ErrFinished", lc.addEdgeAfterFinish},
		{"a second Finish is ErrFinished", lc.secondFinishIsErrFinished},
		{"a second Finish still returns the accumulated stats", lc.secondFinishKeepsStats},
		{"Publish into a non-empty directory is ErrStoreNotEmpty", lc.publishRefusesNonEmpty},
		{"ImportInto a non-empty directory is ErrStoreNotEmpty", lc.importIntoRefusesNonEmpty},
		{"Publish creates an absent directory", lc.publishAcceptsAbsentDir},
		{"Publish(nil builder) is neither sentinel", lc.nilBuilderIsNotSentinel},
		{"the unfinished-builder check precedes the context check", lc.unfinishedBeatsCancelledCtx},
		{"the context check precedes the directory check", lc.cancelledCtxBeatsNonEmptyDir},
		{"ImportInto inspects the directory before building", lc.importIntoDirCheckBeatsBuild},
		{"PublishResult.Stats equals the model's counts", lc.statsMatchModel},
		{"PublishResult.SnapshotDir is <storeDir>/snapshot", lc.snapshotDirIsTheRecoveredName},
	}
	var v []Violation
	for _, p := range probes {
		if !p.ok {
			v = append(v, Violation{
				Kind:    ViolationOracleDeviation,
				Op:      "<bulkimport lifecycle>",
				Message: "bulkimport lifecycle contract changed: " + p.name + " no longer holds",
			})
		}
	}
	return v
}

// bulkImportCheckDurableShape asserts the recovered image is the one the publish
// promised: the snapshot was found, it was self-sufficient (no WAL op
// contributed a byte), and it is a v2-or-later manifest.
func bulkImportCheckDurableShape(res recovery.Result[string, int64], which string) []Violation {
	var v []Violation
	if !res.SnapshotHit {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bulk import publish>",
			Message: which + ": recovery did not find the published snapshot",
		})
	}
	if res.WALOps != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bulk import publish>",
			Message: fmt.Sprintf("%s: replayed %d WAL ops; the published snapshot must be self-sufficient",
				which, res.WALOps),
		})
	}
	if res.SnapshotSchemaVersion < 2 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<bulk import publish>",
			Message: fmt.Sprintf("%s: snapshot manifest version %d; labels and properties need v2 or later",
				which, res.SnapshotSchemaVersion),
		})
	}
	return v
}

// bulkImportCheckParity is the adjudicator: the recovered graph must equal the
// model EXACTLY.
//
// Exactness is two-sided in both dimensions. For nodes, every modelled key must
// be present and live, AND the graph's live order must equal the model's
// cardinality, so an extra node is caught as surely as a missing one. For edges,
// every modelled pair's multiset of (type, weight, properties) must match
// instance for instance, AND the walk covers every out-edge of every node — not
// only the modelled pairs — so an edge to an unmodelled pair is caught too.
//
// The counters it writes into ev are how the non-vacuity test proves the pass
// ran: a checker that compared nothing would report no violation and no work.
func bulkImportCheckParity(m *bulkImportModel, g *lpg.Graph[string, int64], ev *bulkImportEvidence) []Violation {
	var v []Violation

	if got, want := g.LiveOrder(), uint64(len(m.nodes)); got != want {
		v = append(v, Violation{
			Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
			Message: fmt.Sprintf("recovered live order %d, model holds %d nodes", got, want),
		})
	}

	// Node keys must be visited in a fixed order: map iteration is unspecified,
	// and a report whose first violation depended on it would not be reproducible.
	keys := make([]string, 0, len(m.nodes))
	for k := range m.nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	mapper := g.AdjList().Mapper()
	ids := make(map[string]graph.NodeID, len(keys))

	for _, key := range keys {
		mn := m.nodes[key]
		id, ok := mapper.Lookup(key)
		if !ok {
			v = append(v, Violation{
				Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
				Message: fmt.Sprintf("node %q is absent after recovery", key),
			})
			continue
		}
		if g.IsTombstoned(id) {
			v = append(v, Violation{
				Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
				Message: fmt.Sprintf("node %q came back tombstoned", key),
			})
			continue
		}
		ids[key] = id
		ev.nodesChecked++

		wantLabels := make([]string, 0, len(mn.labels))
		for l := range mn.labels {
			wantLabels = append(wantLabels, l)
		}
		if got, want := bulkImportLabelsCanon(g.NodeLabels(key)), bulkImportLabelsCanon(wantLabels); got != want {
			v = append(v, Violation{
				Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
				Message: fmt.Sprintf("node %q labels %s, model says %s", key, got, want),
			})
		}
		ev.labelsChecked += len(mn.labels)

		if got, want := bulkImportPropsCanon(g.NodeProperties(key)), bulkImportPropsCanon(mn.props); got != want {
			v = append(v, Violation{
				Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
				Message: fmt.Sprintf("node %q properties %s, model says %s", key, got, want),
			})
		}
		ev.propsChecked += len(mn.props)
	}

	v = append(v, bulkImportCheckEdges(m, g, ids, keys, ev)...)
	return v
}

// bulkImportCheckEdges adjudicates the edge side. It walks every node's
// adjacency, groups the out-edges by target, and compares each pair's multiset
// of canonical (type, weight, properties) renderings against the model's.
func bulkImportCheckEdges(
	m *bulkImportModel, g *lpg.Graph[string, int64],
	ids map[string]graph.NodeID, keys []string, ev *bulkImportEvidence,
) []Violation {
	var v []Violation

	// Reverse map so a neighbour id can be named. Every live node is in ids.
	names := make(map[graph.NodeID]string, len(ids))
	for k, id := range ids {
		names[id] = k
	}

	seen := make(map[bulkImportPair]bool, len(m.edges))
	adj := g.AdjList()

	for _, src := range keys {
		srcID, ok := ids[src]
		if !ok {
			continue // already reported as absent
		}
		nbrs, weights, handles := adj.LoadEntryH(srcID)
		byTarget := make(map[string][]string, len(nbrs))
		for i, n := range nbrs {
			dst, named := names[n]
			if !named {
				v = append(v, Violation{
					Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
					Message: fmt.Sprintf("node %q has an edge to unknown node id %d", src, n),
				})
				continue
			}
			if i >= len(handles) || handles[i] == 0 {
				v = append(v, Violation{
					Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
					Message: fmt.Sprintf("edge %q->%q came back without a stable handle; "+
						"type and properties are addressed by handle and would be unreadable", src, dst),
				})
				continue
			}
			h := handles[i]
			labels := g.EdgeLabelsByHandle(src, dst, h)
			if len(labels) > 0 {
				ev.handlesTyped++
			}
			typ := ""
			if len(labels) == 1 {
				typ = labels[0]
			} else if len(labels) > 1 {
				typ = bulkImportLabelsCanon(labels) // forces a mismatch, and shows what came back
			}
			var w int64
			if i < len(weights) {
				w = weights[i]
			}
			byTarget[dst] = append(byTarget[dst],
				bulkImportEdgeCanon(typ, w, g.EdgePropertiesByHandle(src, dst, h)))
			ev.handlesChecked++
		}

		for dst, got := range byTarget {
			pair := bulkImportPair{src: src, dst: dst}
			seen[pair] = true
			want := make([]string, 0, len(m.edges[pair]))
			for _, e := range m.edges[pair] {
				want = append(want, bulkImportEdgeCanon(e.typ, e.weight, e.props))
			}
			sort.Strings(got)
			sort.Strings(want)
			ev.pairsChecked++
			if len(got) > 1 {
				ev.parallelPairsSeen++
			}
			if strings.Join(got, ";") != strings.Join(want, ";") {
				v = append(v, Violation{
					Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
					Message: fmt.Sprintf("edge multiset %q->%q recovered as [%s], model says [%s]",
						src, dst, strings.Join(got, " "), strings.Join(want, " ")),
				})
			}
		}
	}

	// Two-sided: a modelled pair the walk never reached is a lost edge.
	missing := make([]string, 0, 4)
	for pair := range m.edges {
		if !seen[pair] {
			missing = append(missing, pair.src+"->"+pair.dst)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		v = append(v, Violation{
			Kind: ViolationGraphIntegrity, Op: "<bulk import parity>",
			Message: fmt.Sprintf("modelled pair %s has no edge after recovery", p),
		})
	}
	return v
}

// bulkImportByteBoundary reports the measured byte-reproducibility of the
// bulk-import publish at each of three property regimes.
type bulkImportByteBoundary struct {
	// multiStable is whether the full fixture — most items carrying two or more
	// properties — published byte-identically across repeated publishes.
	multiStable bool
	// noneStable and singleStable are the same measurement for the fixture
	// stripped to no properties at all, and reduced to exactly one property per
	// node and per edge.
	noneStable   bool
	singleStable bool
	// attempts is how many publish pairs the multi-property regime was given
	// before being declared unstable.
	attempts int
}

// bulkImportCheckByteBoundary measures where byte-reproducibility of a
// bulk-import publish begins and ends, and returns violations when the boundary
// has moved from what was measured under rmp #2466.
//
// The three regimes are the experiment that isolated the cause. Publishing the
// SAME record slices twice diverges once items carry two or more properties, and
// stops diverging when they carry one or none — which identifies Go map
// iteration over the Properties maps as the whole of it, and rules out a
// timestamp, a pointer address, or the fixture's own construction.
//
// A regime that flips is not necessarily a regression: making the writer sort
// property keys would turn multiStable true, which is an improvement. It is
// reported so the change is noticed and this scenario's documentation updated,
// rather than the old claim quietly becoming false.
func bulkImportCheckByteBoundary(
	ctx context.Context, nodes []bulkimport.Node, edges []bulkimport.Edge[int64],
) (bulkImportByteBoundary, []Violation, error) {
	var b bulkImportByteBoundary

	// The multi-property regime is given a few attempts before being called
	// unstable: "differs" needs only one witness, and demanding it from a single
	// pair would be a flake if two map walks ever coincided.
	const attempts = 3
	for i := 0; i < attempts; i++ {
		b.attempts++
		same, err := bulkImportPublishesIdentically(ctx, nodes, edges)
		if err != nil {
			return b, nil, err
		}
		b.multiStable = same
		if !same {
			break
		}
	}

	bare, bareEdges := bulkImportStripProperties(nodes, edges)
	noneSame, err := bulkImportPublishesIdentically(ctx, bare, bareEdges)
	if err != nil {
		return b, nil, err
	}
	b.noneStable = noneSame

	one, oneEdges := bulkImportSingleProperty(nodes, edges)
	singleSame, err := bulkImportPublishesIdentically(ctx, one, oneEdges)
	if err != nil {
		return b, nil, err
	}
	b.singleStable = singleSame

	var v []Violation
	if b.multiStable {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk import publish>",
			Message: "bulk-import publication is now byte-reproducible with multi-property items, " +
				"which it was not when this scenario was written (rmp #2466). That is likely an " +
				"improvement — property keys are presumably ordered now — but the documented boundary " +
				"is stale and must be updated",
		})
	}
	if !b.noneStable {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk import publish>",
			Message: "a property-free bulk import no longer publishes byte-identically; the byte " +
				"divergence is no longer explained by property map order alone, so a second source " +
				"of non-determinism has appeared in the publish path",
		})
	}
	if !b.singleStable {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk import publish>",
			Message: "a single-property-per-item bulk import no longer publishes byte-identically; " +
				"the byte divergence is no longer explained by property map order alone",
		})
	}
	return b, v, nil
}

// bulkImportPublishesIdentically publishes the same records twice and reports
// whether the two data images are byte-identical.
func bulkImportPublishesIdentically(
	ctx context.Context, nodes []bulkimport.Node, edges []bulkimport.Edge[int64],
) (bool, error) {
	first, err := bulkImportPublishAndMeasure(ctx, nodes, edges)
	if err != nil {
		return false, err
	}
	second, err := bulkImportPublishAndMeasure(ctx, nodes, edges)
	if err != nil {
		return false, err
	}
	return first.digest == second.digest, nil
}

// bulkImportStripProperties returns the same records with every property map
// dropped. Labels and relationship types are kept, so only the property maps
// change.
func bulkImportStripProperties(
	nodes []bulkimport.Node, edges []bulkimport.Edge[int64],
) ([]bulkimport.Node, []bulkimport.Edge[int64]) {
	ns := make([]bulkimport.Node, len(nodes))
	for i, n := range nodes {
		ns[i] = bulkimport.Node{Key: n.Key, Labels: n.Labels}
	}
	es := make([]bulkimport.Edge[int64], len(edges))
	for i, e := range edges {
		es[i] = bulkimport.Edge[int64]{Src: e.Src, Dst: e.Dst, Type: e.Type, Weight: e.Weight}
	}
	return ns, es
}

// bulkImportSingleProperty returns the same records reduced to at most ONE
// property each. Which key survives is drawn from map iteration and so is
// unspecified, but that does not matter: a one-entry map has exactly one
// iteration order, which is the property under test.
func bulkImportSingleProperty(
	nodes []bulkimport.Node, edges []bulkimport.Edge[int64],
) ([]bulkimport.Node, []bulkimport.Edge[int64]) {
	ns := make([]bulkimport.Node, len(nodes))
	for i, n := range nodes {
		ns[i] = bulkimport.Node{Key: n.Key, Labels: n.Labels}
		for k, val := range n.Properties {
			ns[i].Properties = map[string]lpg.PropertyValue{k: val}
			break
		}
	}
	es := make([]bulkimport.Edge[int64], len(edges))
	for i, e := range edges {
		es[i] = bulkimport.Edge[int64]{Src: e.Src, Dst: e.Dst, Type: e.Type, Weight: e.Weight}
		for k, val := range e.Properties {
			es[i].Properties = map[string]lpg.PropertyValue{k: val}
			break
		}
	}
	return ns, es
}

// bulkImportCheckCrashedImport reconstructs the durable state a crash between
// the snapshot writer's assembly and its rename leaves behind, and requires
// recovery to treat it as no import at all.
//
// It is a RECONSTRUCTION, not an interruption: a completed snapshot is published
// to a scratch directory and then moved to `snapshot.tmp` in a fresh one. That
// is byte-for-byte the directory shape the crash leaves, so what recovery does
// with it is genuinely measured — but the writer was never actually interrupted,
// and no seam in this package can interrupt it (see the file header, rmp #2518).
func bulkImportCheckCrashedImport(
	ctx context.Context, root string,
	nodes []bulkimport.Node, edges []bulkimport.Edge[int64], ev *bulkImportEvidence,
) ([]Violation, error) {
	scratch := filepath.Join(root, "scratch")
	res, err := bulkimport.ImportInto[int64](ctx, scratch,
		bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges)
	if err != nil {
		return nil, fmt.Errorf("sim: bulkimport-parity crashed-import publish: %w", err)
	}

	crashed := filepath.Join(root, "crashed")
	if err := os.MkdirAll(crashed, 0o750); err != nil {
		return nil, fmt.Errorf("sim: bulkimport-parity crashed dir: %w", err)
	}
	assembly := filepath.Join(crashed, "snapshot.tmp")
	if err := os.Rename(res.SnapshotDir, assembly); err != nil {
		return nil, fmt.Errorf("sim: bulkimport-parity stage assembly: %w", err)
	}
	// Measure the debris BEFORE the reopen, so "recovery removed it" is a
	// measured delta rather than an assumption that it was ever there.
	_, bytes, err := bulkImportTreeSize(assembly)
	if err != nil {
		return nil, err
	}
	ev.crashedAssemblyBytesPre = bytes

	out, err := bulkImportOpen(crashed)
	if err != nil {
		return nil, err
	}
	ev.crashedSnapshotHit = out.SnapshotHit
	ev.crashedLiveOrder = out.Graph.LiveOrder()
	_, statErr := os.Stat(assembly)
	ev.crashedAssemblyRemoved = errors.Is(statErr, fs.ErrNotExist)

	var v []Violation
	if ev.crashedSnapshotHit {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<bulk import publish>",
			Message: "recovery opened the assembly directory; a crashed import must be invisible",
		})
	}
	if ev.crashedLiveOrder != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<bulk import publish>",
			Message: fmt.Sprintf("a crashed import left %d nodes visible, want 0", ev.crashedLiveOrder),
		})
	}
	if !ev.crashedAssemblyRemoved {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<bulk import publish>",
			Message: "the assembly directory survived recovery; a crashed import must leave no debris",
		})
	}
	if ev.crashedAssemblyBytesPre <= 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<bulk import publish>",
			Message: "the staged assembly directory was empty; the crashed-import arm proved nothing",
		})
	}
	return v, nil
}

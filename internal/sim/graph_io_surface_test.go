package sim

// graph_io_surface_test.go — the wiring and the falsifiability proofs for the
// graph/io completeness surface (rmp #2480).
//
// Each arm follows the standing structure rmp #2470/#2472 fixed:
//
//   - the SEPARATE shape-only non-vacuity gate runs FIRST. It asks only whether
//     the run (or the declaration) could ever have failed;
//   - the VERDICT is unconditional;
//   - the WITNESS — what the run actually produced — is logged with t.Logf and
//     never asserted, so a formatting or size detail cannot fail the run.
//
// Every gate is then shown to be FALSIFIABLE by feeding it a synthetic result
// that must trip it. A gate that has never been seen to fail is a gate that
// proves nothing.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// graphIOTestSeed is the fixed seed the surface arms run at. ST8 already drives
// RunGraphIOSurface across storageFaultTestSeeds; this arm exists to adjudicate
// and WITNESS one run in detail.
const graphIOTestSeed uint64 = 0x2480_C0DE

// -----------------------------------------------------------------------------
// The cross-format surface
// -----------------------------------------------------------------------------

// TestGraphIOSurface_CrossFormatAgreement drives the DOT / CSV / JSONL
// cross-format agreement, the JSONL property-graph round-trip, the csv.Options
// matrix and the mutated-export sweep at one seed, and adjudicates all four.
func TestGraphIOSurface_CrossFormatAgreement(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("RunGraphIOSurface: %v", err)
	}

	// Non-vacuity FIRST: a run that drove no quoting, no weight label, no
	// isolated vertex, or no effective mutation makes the verdict meaningless.
	if v := CheckGraphIOSurfaceShape(&r); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("NON-VACUITY %s: %s", viol.Op, viol.Message)
		}
		t.Fatalf("the run proves nothing; the verdict below would be meaningless")
	}

	// The verdict, unconditional.
	for _, viol := range CheckGraphIOSurface(&r) {
		t.Errorf("VERDICT %s %s: %s", viol.Kind, viol.Op, viol.Message)
	}

	// Witness only.
	t.Logf("witness: model %d vertices / %d distinct edges; DOT %d B (quoted=%d, labelled=%d, bare=%d), CSV %d B, JSONL %d B",
		len(r.ModelNodes), len(r.ModelTriples), r.DOTBytes, r.DOT.QuotedIDs, r.DOT.Labelled, r.DOT.BareNodes,
		r.CSVBytes, r.JSONLBytes)
	t.Logf("witness: JSONL props %d B, %d rows, %d property records, kinds=%v",
		r.Props.Bytes, r.Props.Rows, r.Props.PropertyRecords, r.Props.KindsOnWire)
	for _, arm := range r.CSVArms {
		t.Logf("witness: csv arm %-32s delim=%q comment=%q header=%t sanitize=%t bytes=%d rows=%d roundTrips=%t (declared %t)",
			arm.Name, arm.Delimiter, arm.Comment, arm.HasHeader, arm.Sanitize, arm.Bytes, arm.Rows,
			arm.RoundTrips, arm.ExpectRoundTrip)
	}
	effective, typed := 0, 0
	for i := range r.Mutations {
		m := &r.Mutations[i]
		if m.Effective() {
			effective++
		}
		if m.Typed {
			typed++
		}
	}
	// This path measures NO allocation (see RunGraphIOSurface): the figure it
	// would report comes from a process-global counter, so the bounded-allocation
	// bound is adjudicated by TestGraphIOSurface_MutationAllocBoundHoldsSerially
	// instead.
	t.Logf("witness: %d mutations, %d effective, %d typed rejections, over %d B of input",
		len(r.Mutations), effective, typed, r.MutationInputBytes)
	for _, name := range sortedCopy(keysOf(r.ExportStability)) {
		t.Logf("witness: byte-reproducibility %-38s differed in %d of %d repeat exports",
			name, r.ExportStability[name], graphIOStabilityRuns-1)
	}
}

// keysOf returns the keys of m.
func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestGraphIOSurface_CrossFormatVerdictIsFalsifiable proves the agreement
// oracle can fail. It drops one edge statement from the real DOT export, re-
// parses, and requires the parser to see the loss — so a writer that silently
// dropped an edge would be caught rather than agreed with.
func TestGraphIOSurface_CrossFormatVerdictIsFalsifiable(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("RunGraphIOSurface: %v", err)
	}
	if len(CheckGraphIOSurface(&r)) != 0 {
		t.Fatalf("the healthy run must adjudicate clean before it can be broken")
	}

	// One edge removed from the DOT description: the verdict must notice.
	broken := r
	broken.DOT.Triples = make(map[string]int, len(r.DOT.Triples))
	dropped := false
	for k, n := range r.DOT.Triples {
		if !dropped {
			dropped = true
			continue
		}
		broken.DOT.Triples[k] = n
	}
	v := CheckGraphIOSurface(&broken)
	if len(v) == 0 {
		t.Fatal("dropping a DOT edge produced no violation — the cross-format oracle cannot fail")
	}
	t.Logf("witness: dropping one DOT edge produced %d violation(s), first: %s", len(v), v[0].Message)

	// A property lost on the JSONL wire: the property arm must notice too.
	brokenProps := r
	brokenProps.Props.Equal = false
	brokenProps.Props.Mismatch = "synthetic"
	if len(CheckGraphIOSurface(&brokenProps)) == 0 {
		t.Fatal("a failed property round-trip produced no violation")
	}

	// A panic recorded on a mutation must fail the run rather than be logged.
	brokenPanic := r
	brokenPanic.Mutations = append([]GraphIOMutation(nil), r.Mutations...)
	brokenPanic.Mutations[0].Panicked = "synthetic panic"
	if len(CheckGraphIOSurface(&brokenPanic)) == 0 {
		t.Fatal("a recorded importer panic produced no violation")
	}

	// The byte-reproducibility oracle must be able to fire — and must NOT fire
	// for the one encoder measured unstable, so a future fix there cannot break
	// this run.
	brokenStable := r
	brokenStable.ExportStability = map[string]int{}
	for k, n := range r.ExportStability {
		brokenStable.ExportStability[k] = n
	}
	brokenStable.ExportStability["dot.Write"] = 3
	if len(CheckGraphIOSurface(&brokenStable)) == 0 {
		t.Fatal("a non-reproducible DOT export produced no violation")
	}
	exempt := r
	exempt.ExportStability = map[string]int{}
	for k, n := range r.ExportStability {
		exempt.ExportStability[k] = n
	}
	exempt.ExportStability[graphIOUnstableEncoder] = graphIOStabilityRuns - 1
	if len(CheckGraphIOSurface(&exempt)) != 0 {
		t.Fatal("the exempted encoder's instability failed the verdict; the exemption is not in force")
	}
}

// TestGraphIOSurface_ShapeGateIsFalsifiable proves the non-vacuity gate can
// fail: each synthetic result below removes exactly one piece of evidence the
// verdict depends on, and the gate must object to every one.
func TestGraphIOSurface_ShapeGateIsFalsifiable(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	healthy, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("RunGraphIOSurface: %v", err)
	}
	if v := CheckGraphIOSurfaceShape(&healthy); len(v) > 0 {
		t.Fatalf("the healthy run must pass the shape gate first: %v", v)
	}

	cases := []struct {
		corrupt func(*GraphIOSurfaceResult)
		name    string
	}{
		{name: "no quoted identifier", corrupt: func(r *GraphIOSurfaceResult) { r.DOT.QuotedIDs = 0 }},
		{name: "no weight label", corrupt: func(r *GraphIOSurfaceResult) { r.DOT.Labelled = 0 }},
		{name: "no bare node statement", corrupt: func(r *GraphIOSurfaceResult) { r.DOT.BareNodes = 0 }},
		{name: "DOT arm did not run", corrupt: func(r *GraphIOSurfaceResult) { r.DOTBytes = 0 }},
		{name: "CSV carries every vertex", corrupt: func(r *GraphIOSurfaceResult) { r.CSVNodes = r.ModelNodes }},
		{name: "no property record", corrupt: func(r *GraphIOSurfaceResult) { r.Props.PropertyRecords = 0 }},
		{name: "a property kind never written", corrupt: func(r *GraphIOSurfaceResult) {
			r.Props.KindsOnWire = []string{"string"}
		}},
		{name: "every csv arm used the default delimiter", corrupt: func(r *GraphIOSurfaceResult) {
			arms := append([]GraphIOCSVArm(nil), r.CSVArms...)
			for i := range arms {
				arms[i].Delimiter = ','
			}
			r.CSVArms = arms
		}},
		{name: "no csv arm declares a failed round-trip", corrupt: func(r *GraphIOSurfaceResult) {
			arms := append([]GraphIOCSVArm(nil), r.CSVArms...)
			for i := range arms {
				arms[i].ExpectRoundTrip = true
			}
			r.CSVArms = arms
		}},
		{name: "every mutation was inert", corrupt: func(r *GraphIOSurfaceResult) {
			muts := append([]GraphIOMutation(nil), r.Mutations...)
			for i := range muts {
				muts[i].Err, muts[i].Equal = "", true
			}
			r.Mutations = muts
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := healthy
			tc.corrupt(&broken)
			v := CheckGraphIOSurfaceShape(&broken)
			if len(v) == 0 {
				t.Fatalf("the shape gate accepted a run with %q — it cannot fail", tc.name)
			}
			t.Logf("witness: %s", v[0].Message)
		})
	}
}

// TestGraphIOSurface_Deterministic pins bit-reproducibility of the surface: the
// same seed yields the same export sizes, the same DOT census and the same
// mutation offsets, so a failure always replays. The export SIZE is asserted
// equal across runs because every mutation offset is drawn against it — a size
// that drifted would silently move every offset.
func TestGraphIOSurface_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	b, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if a.DOTBytes != b.DOTBytes || a.CSVBytes != b.CSVBytes || a.JSONLBytes != b.JSONLBytes {
		t.Fatalf("export sizes differ across runs: DOT %d/%d, CSV %d/%d, JSONL %d/%d",
			a.DOTBytes, b.DOTBytes, a.CSVBytes, b.CSVBytes, a.JSONLBytes, b.JSONLBytes)
	}
	if len(a.Mutations) != len(b.Mutations) {
		t.Fatalf("mutation count differs: %d vs %d", len(a.Mutations), len(b.Mutations))
	}
	for i := range a.Mutations {
		x, y := a.Mutations[i], b.Mutations[i]
		if x.Format != y.Format || x.Kind != y.Kind || x.Offset != y.Offset || x.Source != y.Source {
			t.Fatalf("mutation %d differs: %+v vs %+v", i, x, y)
		}
		if x.Err != y.Err || x.Equal != y.Equal {
			t.Fatalf("mutation %d outcome differs: %+v vs %+v", i, x, y)
		}
	}
	t.Logf("witness: %d mutations reproduced byte-for-byte over exports of %d/%d/%d B",
		len(a.Mutations), a.DOTBytes, a.CSVBytes, a.JSONLBytes)
}

// -----------------------------------------------------------------------------
// The DOT reader this package had to supply
// -----------------------------------------------------------------------------

// TestGraphIODOTParser_RefusesWhatItCannotRead pins that the parser is an
// oracle rather than a guesser: it must reject anything outside the subset
// dot.Write emits, because a parser that shrugged at malformed input would
// agree with a broken writer.
func TestGraphIODOTParser_RefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{name: "no header", src: "{ a -> b; }"},
		{name: "wrong edge operator for a digraph", src: "digraph G {\n  a -- b;\n}\n"},
		{name: "unterminated statement", src: "digraph G {\n  a -> b\n}\n"},
		{name: "unterminated quoted id", src: "digraph G {\n  \"a -> b;\n}\n"},
		{name: "unterminated body", src: "digraph G {\n  a -> b;\n"},
		{name: "non-numeric label", src: "digraph G {\n  a -> b [label=\"x\"];\n}\n"},
		{name: "unknown attribute", src: "digraph G {\n  a -> b [colour=\"red\"];\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDOT(tc.src); err == nil {
				t.Fatalf("parseDOT accepted %q", tc.src)
			}
		})
	}

	// And it must READ what the writer emits, including the hostile identifiers
	// a line-oriented parser would mis-split.
	doc, err := parseDOT("digraph G {\n  \"x->y\" -> \"a;b\" [label=\"7\"];\n  \"lone one\";\n}\n")
	if err != nil {
		t.Fatalf("parseDOT rejected a well-formed document: %v", err)
	}
	if doc.BareNodes != 1 || doc.Labelled != 1 || doc.QuotedIDs != 3 {
		t.Fatalf("census = bare %d, labelled %d, quoted %d; want 1, 1, 3", doc.BareNodes, doc.Labelled, doc.QuotedIDs)
	}
	if n := doc.Triples["x->y\x00a;b\x007"]; n != 1 {
		t.Fatalf("the edge across quoted identifiers did not survive: %v", doc.Triples)
	}
}

// -----------------------------------------------------------------------------
// The guard battery: caps and mid-parse cancellation
// -----------------------------------------------------------------------------

// TestGraphIOGuards_EveryDeclaredCapIsProvoked runs the whole crafted battery.
// It is the arm that makes "each defensive cap must be provoked at least once"
// enforceable: the verdict fails when a cap declared reachable was not reached,
// so deleting a probe cannot quietly reduce the coverage.
func TestGraphIOGuards_EveryDeclaredCapIsProvoked(t *testing.T) {
	defer goleak.VerifyNone(t)

	// The SEPARATE shape-only gate first, about the DECLARATIONS rather than
	// the run: a declaration set that could not fail makes the verdict moot.
	if v := CheckGraphIOGuardDeclShape(GraphIOGuardDecls()); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("NON-VACUITY %s: %s", viol.Op, viol.Message)
		}
		t.Fatal("the cap declarations prove nothing; the verdict below would be meaningless")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	r, err := RunGraphIOGuards(ctx)
	if err != nil {
		t.Fatalf("RunGraphIOGuards: %v", err)
	}

	for _, viol := range CheckGraphIOGuards(&r) {
		t.Errorf("VERDICT %s %s: %s", viol.Kind, viol.Op, viol.Message)
	}

	// Witness only.
	for _, c := range r.Caps {
		t.Logf("witness: cap %-34s matched=%t alloc=%.1f MiB input=%d B overran=%t err=%v",
			c.Name, c.Matched, float64(c.AllocBytes)/(1<<20), c.InputBytes, c.Overran, c.Err)
	}
	if n := len(r.ListDepthBytes); n >= 2 {
		t.Logf("witness: nested-list wire grows %d B at depth 1 to %d B at depth %d (%.2fx per level); deepest round-trip %d",
			r.ListDepthBytes[0], r.ListDepthBytes[n-1], n,
			float64(r.ListDepthBytes[n-1])/float64(r.ListDepthBytes[n-2]), r.DeepestRoundTrip)
	}
	for _, c := range r.Cancels {
		t.Logf("witness: cancel %-26s canceled=%t nilGraph=%t units=%d (control %d, equal=%t) err=%v",
			c.Name, c.Canceled, c.GraphNil, c.Rows, c.ControlRows, c.ControlEqual, c.Err)
	}
}

// TestGraphIOGuards_VerdictCatchesAnUnprovokedCap proves the cap verdict can
// fail. A cap silently going unprovoked is exactly the failure mode this task
// exists to prevent, so the oracle for it is exercised rather than trusted.
func TestGraphIOGuards_VerdictCatchesAnUnprovokedCap(t *testing.T) {
	decls := GraphIOGuardDecls()
	healthy := GraphIOGuardResult{
		ListDepthBytes:   []int{16, 34, 70, 142, 286, 574, 1150, 2302, 4606, 9214, 18430, 36862, 73726},
		DeepestRoundTrip: 16,
	}
	for _, d := range decls {
		if d.Unreachable != "" {
			continue
		}
		healthy.Caps = append(healthy.Caps, GraphIOCapObservation{
			Name: d.Name, Ran: true, Matched: true, Err: d.Sentinel, AllocBytes: 1024,
		})
	}
	for _, name := range []string{"csv.ReadIntoCtx", "jsonl.ReadIntoCtx", "graphml.ReadIntoCtx"} {
		healthy.Cancels = append(healthy.Cancels, GraphIOCancelObservation{
			Name: name, Canceled: true, GraphNil: true, Rows: 8192, ControlRows: 20000, ControlEqual: true,
		})
	}
	if v := CheckGraphIOGuards(&healthy); len(v) > 0 {
		t.Fatalf("the synthetic healthy result must adjudicate clean first: %v", v)
	}

	cases := []struct {
		corrupt func(*GraphIOGuardResult)
		name    string
	}{
		{name: "a cap was never probed", corrupt: func(r *GraphIOGuardResult) { r.Caps = r.Caps[1:] }},
		{name: "a cap returned the wrong error", corrupt: func(r *GraphIOGuardResult) {
			caps := append([]GraphIOCapObservation(nil), r.Caps...)
			caps[0].Matched, caps[0].Err = false, errors.New("some other failure")
			r.Caps = caps
		}},
		{name: "a probe panicked", corrupt: func(r *GraphIOGuardResult) {
			caps := append([]GraphIOCapObservation(nil), r.Caps...)
			caps[0].Panicked = "index out of range"
			r.Caps = caps
		}},
		{name: "the endless input overran its ceiling", corrupt: func(r *GraphIOGuardResult) {
			caps := append([]GraphIOCapObservation(nil), r.Caps...)
			caps[0].Overran = true
			r.Caps = caps
		}},
		{name: "a probe blew its heap bound", corrupt: func(r *GraphIOGuardResult) {
			caps := append([]GraphIOCapObservation(nil), r.Caps...)
			caps[0].AllocBytes = 1 << 40
			r.Caps = caps
		}},
		{name: "the list wire stopped re-escaping", corrupt: func(r *GraphIOGuardResult) {
			flat := make([]int, len(r.ListDepthBytes))
			for i := range flat {
				flat[i] = 16 + 4*i
			}
			r.ListDepthBytes = flat
		}},
		{name: "the depth guard fires early", corrupt: func(r *GraphIOGuardResult) { r.DeepestRoundTrip = 3 }},
		{name: "cancellation was not typed", corrupt: func(r *GraphIOGuardResult) {
			cs := append([]GraphIOCancelObservation(nil), r.Cancels...)
			cs[0].Canceled = false
			r.Cancels = cs
		}},
		{name: "a partial graph escaped", corrupt: func(r *GraphIOGuardResult) {
			cs := append([]GraphIOCancelObservation(nil), r.Cancels...)
			cs[0].GraphNil = false
			r.Cancels = cs
		}},
		{name: "cancellation landed before parsing began", corrupt: func(r *GraphIOGuardResult) {
			cs := append([]GraphIOCancelObservation(nil), r.Cancels...)
			cs[0].Rows = 0
			r.Cancels = cs
		}},
		{name: "the uncancelled control did not reproduce the model", corrupt: func(r *GraphIOGuardResult) {
			cs := append([]GraphIOCancelObservation(nil), r.Cancels...)
			cs[0].ControlEqual = false
			r.Cancels = cs
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := healthy
			tc.corrupt(&broken)
			v := CheckGraphIOGuards(&broken)
			if len(v) == 0 {
				t.Fatalf("the cap verdict accepted a result with %q — it cannot fail", tc.name)
			}
			t.Logf("witness: %s", v[0].Message)
		})
	}
}

// TestGraphIOGuards_DeclShapeGateIsFalsifiable proves the shape-only gate can
// fail, and pins the declared cap set by name so a sentinel added to graph/io
// without a declaration here is caught by a failing assertion rather than by
// nobody noticing.
func TestGraphIOGuards_DeclShapeGateIsFalsifiable(t *testing.T) {
	want := []string{
		"csv.ErrInputTooLarge", "csv.ErrTooManyFields",
		"jsonl.ErrInputTooLarge", "jsonl.ErrLineTooLong", "jsonl.ErrUnknownType", "jsonl.ErrListTooDeep",
		"graphml.ErrInputTooLarge", "graphml.ErrTooManyKeys", "graphml.ErrTooManyData",
		"jsonl.ErrPropertyValueTooLarge", "jsonl.ErrPropertyNestingTooDeep",
		"graphml.ErrPropertyValueTooLarge", "graphml.ErrPropertyNestingTooDeep",
		"graphml.ErrInvalidXMLChar",
	}
	decls := GraphIOGuardDecls()
	got := make([]string, 0, len(decls))
	for _, d := range decls {
		got = append(got, d.Name)
	}
	if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(want), ",") {
		t.Fatalf("the declared cap set changed:\n got %v\nwant %v", sortedCopy(got), sortedCopy(want))
	}

	cases := []struct {
		corrupt func([]GraphIOGuardDecl) []GraphIOGuardDecl
		name    string
	}{
		{name: "empty", corrupt: func([]GraphIOGuardDecl) []GraphIOGuardDecl { return nil }},
		{name: "duplicate name", corrupt: func(d []GraphIOGuardDecl) []GraphIOGuardDecl {
			return append(append([]GraphIOGuardDecl(nil), d...), d[0])
		}},
		{name: "no sentinel", corrupt: func(d []GraphIOGuardDecl) []GraphIOGuardDecl {
			out := append([]GraphIOGuardDecl(nil), d...)
			out[0].Sentinel = nil
			return out
		}},
		{name: "no heap bound", corrupt: func(d []GraphIOGuardDecl) []GraphIOGuardDecl {
			out := append([]GraphIOGuardDecl(nil), d...)
			out[0].AllocBoundBytes = 0
			return out
		}},
		{name: "unknown side", corrupt: func(d []GraphIOGuardDecl) []GraphIOGuardDecl {
			out := append([]GraphIOGuardDecl(nil), d...)
			out[0].Side = "middle"
			return out
		}},
		{name: "no writer-side cap", corrupt: func(d []GraphIOGuardDecl) []GraphIOGuardDecl {
			var out []GraphIOGuardDecl
			for _, x := range d {
				if x.Side == "reader" {
					out = append(out, x)
				}
			}
			return out
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckGraphIOGuardDeclShape(tc.corrupt(GraphIOGuardDecls()))
			if len(v) == 0 {
				t.Fatalf("the declaration shape gate accepted %q", tc.name)
			}
			t.Logf("witness: %s", v[0].Message)
		})
	}
}

// TestGraphIOGuards_CapsAreCraftedNotSeeded records, as an assertion, why the
// caps are driven deterministically rather than from the seeded mutation sweep:
// the crafted documents that provoke them are far outside anything a byte flip
// or truncation of an ordinary export can produce. Feeding the sweep's own
// corruptions to the importers must NOT raise the structural caps, and the
// gate above must therefore not be satisfiable by the sweep alone.
func TestGraphIOGuards_CapsAreCraftedNotSeeded(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("RunGraphIOSurface: %v", err)
	}
	structural := []error{jsonl.ErrLineTooLong, jsonl.ErrListTooDeep, jsonl.ErrUnknownType}
	for _, m := range r.Mutations {
		if m.Err == "" {
			continue
		}
		for _, s := range structural {
			if strings.Contains(m.Err, s.Error()) && m.Format != "jsonl" && m.Format != "jsonl-props" {
				t.Errorf("mutation %s/%s raised %v, which belongs to another codec", m.Format, m.Kind, s)
			}
		}
	}
	// The delimiter-run mutation is the ONE corruption shaped like a structural
	// attack, so it is the one that may legitimately reach a cap. Recording
	// which mutations reached one keeps the crafted/seeded split honest.
	reached := 0
	for _, m := range r.Mutations {
		if m.Typed {
			reached++
			t.Logf("witness: seeded mutation %s/%s reached a typed cap: %s", m.Format, m.Kind, m.Err)
		}
	}
	t.Logf("witness: %d of %d seeded mutations reached a typed cap; the remaining caps are unreachable from the sweep and are crafted in RunGraphIOGuards",
		reached, len(r.Mutations))
	if reached == len(r.Mutations) {
		t.Error("every seeded mutation reached a typed cap — the crafted battery would then be redundant, which contradicts its declared purpose")
	}
	// csv.ErrInputTooLarge must not be reachable from the sweep: the sweep caps
	// at four times the source, and no mutation grows the source that far.
	for _, m := range r.Mutations {
		if m.Format == "csv" && strings.Contains(m.Err, csv.ErrInputTooLarge.Error()) {
			t.Errorf("a seeded mutation tripped the csv byte cap at %s/%s — the sweep's cap is too tight to be a control",
				m.Format, m.Kind)
		}
	}
}

// sortedCopy returns a sorted copy of names.
func sortedCopy(names []string) []string {
	out := append([]string(nil), names...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// The SERIALISED allocation arm (rmp #2553)
// -----------------------------------------------------------------------------
//
// Every test below reads runtime.MemStats.TotalAlloc, a PROCESS-GLOBAL
// cumulative counter, so none of them calls t.Parallel: a window opened while
// anything else in this process allocates bills the measured code for its
// neighbours. None of them is skipped under -race either. Measured at
// graphIOTestSeed, the race detector inflates the figure by under 2%
// (46.7-46.8x without it, 47.6x with it) against a bound of 64x, so the
// headroom is ample and the property stays inside the race-enabled gate
// rather than being skipped out of it.

// TestGraphIOSurface_MutationAllocBoundHoldsSerially is the arm that adjudicates
// the mutation sweep's bounded allocation. It is the ONLY place the bound is
// decided: the swarm-reachable path does not measure at all, because it runs
// concurrently with other scenarios.
func TestGraphIOSurface_MutationAllocBoundHoldsSerially(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seeds := append([]uint64{graphIOTestSeed}, storageFaultTestSeeds...)
	for _, seed := range seeds {
		r, err := runGraphIOSurface(ctx, seed, graphIOSurfaceOpts{measureAlloc: true})
		if err != nil {
			t.Fatalf("runGraphIOSurface seed %#x: %v", seed, err)
		}
		if !r.AllocMeasured {
			t.Fatalf("seed %#x: the measured arm produced a result carrying no measurement", seed)
		}
		for _, viol := range checkGraphIOMutationAlloc(&r) {
			t.Errorf("VERDICT seed %#x %s %s: %s", seed, viol.Kind, viol.Op, viol.Message)
		}
		t.Logf("witness: seed %#x allocated %d B over %d B of input (%.1fx, bound %dx)",
			seed, r.MutationAllocBytes, r.MutationInputBytes,
			float64(r.MutationAllocBytes)/float64(r.MutationInputBytes), graphIOMutationAllocRatio)
	}
}

// TestGraphIOSurface_MutationAllocOracleCatchesOverAllocation proves the bound
// above can FAIL. It re-runs the same seed with a deliberate over-allocation
// injected inside the sweep and requires the oracle to condemn it — a bound
// never seen to fail is a bound that proves nothing.
//
// The injection is 1 MiB per mutation. The sweep replays 16 (format, mutation)
// pairs, so it adds 16 MiB on top of a control measured at 14 550 032 B over
// 311 091 B of input: the ratio moves from 46.8x to 100.6x against the 64x
// bound (47.7x to 101.5x under -race), so the margin is large in both
// directions and neither arm sits near the ceiling.
func TestGraphIOSurface_MutationAllocOracleCatchesOverAllocation(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The CONTROL first: a control that does not adjudicate clean would make
	// the injected run's failure meaningless, since it would fail either way.
	control, cerr := runGraphIOSurface(ctx, graphIOTestSeed, graphIOSurfaceOpts{measureAlloc: true})
	if cerr != nil {
		t.Fatalf("control run: %v", cerr)
	}
	if v := checkGraphIOMutationAlloc(&control); len(v) != 0 {
		for _, viol := range v {
			t.Errorf("CONTROL %s: %s", viol.Op, viol.Message)
		}
		t.Fatalf("the unmodified control does not adjudicate clean, so the sensitivity proof below would prove nothing")
	}

	const injectPerMutation = 1 << 20
	var held [][]byte
	inflated, ierr := runGraphIOSurface(ctx, graphIOTestSeed, graphIOSurfaceOpts{
		measureAlloc: true,
		inflate: func() {
			b := make([]byte, injectPerMutation)
			b[0] = 1 // written, so the allocation cannot be optimised away
			held = append(held, b)
		},
	})
	if ierr != nil {
		t.Fatalf("inflated run: %v", ierr)
	}
	runtime.KeepAlive(held)

	v := checkGraphIOMutationAlloc(&inflated)
	if len(v) == 0 {
		t.Fatalf("the oracle passed a run inflated by %d B per mutation (%d B over %d B of input, %.1fx) — it cannot detect over-allocation",
			injectPerMutation, inflated.MutationAllocBytes, inflated.MutationInputBytes,
			float64(inflated.MutationAllocBytes)/float64(inflated.MutationInputBytes))
	}
	for _, viol := range v {
		if viol.Op != "<io-mutation-alloc>" {
			t.Errorf("the violation carried Op %q, want %q", viol.Op, "<io-mutation-alloc>")
		}
	}
	// The window must enclose the whole REPLAY the arm measures: if it did not,
	// the injected bytes would not appear in the difference between the two runs.
	//
	// The comparison carries a tolerance because both sides are MEASUREMENTS of
	// a process-global counter, not terms of an arithmetic identity: two runs of
	// the same workload differ by map growth, size-class rounding and GC timing.
	// The zero-tolerance form of this clause failed a full gate run by 1120 B
	// out of 31.5 MB (0.0035%) while the oracle under test worked, condemning
	// the inflated run at 101.5x against its 64x bound.
	//
	// The tolerance is derived from the measured distribution rather than chosen
	// to pass. Over 40 consecutive control/inflated pairs the inflated run's
	// NON-injected portion — the measurement less the injected bytes — sat
	// between 31728 B BELOW the control and 8336 B above it, a spread of
	// 40064 B; 39 of the 40 landed above the control, and the single shortfall
	// of 31728 B is 0.19% of the injected amount. The smallest miss this clause
	// guards is a window that stopped enclosing one mutation's injection, 1 MiB.
	//
	// 1% of the INJECTED amount — 167772 B at 16 MiB injected — sits 5.3x above
	// that worst measured shortfall and 6.25x below the 1 MiB defect floor,
	// within 8% of the geometric mean of the two, so it separates the noise from
	// the defect instead of straddling either. It is expressed against the
	// injected amount and never against the total, so it does not widen as
	// unrelated allocation grows. Verified in both directions: losing exactly
	// one mutation's injection failed this clause by 907972 B (rmp #2591).
	const allocTolerancePercent = 1
	injected := uint64(injectPerMutation) * uint64(len(inflated.Mutations))
	tolerance := injected * allocTolerancePercent / 100
	if want := control.MutationAllocBytes + injected - tolerance; inflated.MutationAllocBytes < want {
		t.Errorf("the inflated run measured %d B, want at least %d B (control %d B + %d B injected across %d mutations, less a %d B tolerance) — the window does not enclose the replay",
			inflated.MutationAllocBytes, want, control.MutationAllocBytes, injected, len(inflated.Mutations), tolerance)
	}
	t.Logf("witness: control %d B over %d B (%.1fx); inflated %d B over %d B (%.1fx) after injecting %d B across %d mutations",
		control.MutationAllocBytes, control.MutationInputBytes,
		float64(control.MutationAllocBytes)/float64(control.MutationInputBytes),
		inflated.MutationAllocBytes, inflated.MutationInputBytes,
		float64(inflated.MutationAllocBytes)/float64(inflated.MutationInputBytes),
		injected, len(inflated.Mutations))
	t.Logf("witness: the oracle condemned it: %s", v[0].Message)
}

// TestGraphIOSurface_ConcurrentPathCarriesNoAllocClause is the regression gate
// for rmp #2553 itself. Re-adding the process-global allocation clause to the
// verdict the swarm runs fails here.
//
// Billing a scenario for its neighbours' allocations made io-roundtrip-fault
// fail 116 of 120 seeds at -workers=3 that pass at -workers=1, so the clause
// must be absent from the concurrently reachable path — and the separate
// checker must REFUSE an unmeasured result rather than read its zero as a pass.
func TestGraphIOSurface_ConcurrentPathCarriesNoAllocClause(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := RunGraphIOSurface(ctx, graphIOTestSeed)
	if err != nil {
		t.Fatalf("RunGraphIOSurface: %v", err)
	}
	if r.AllocMeasured {
		t.Errorf("the swarm-reachable path reported AllocMeasured=true; it must open no process-global window")
	}
	if r.MutationAllocBytes != 0 {
		t.Errorf("the swarm-reachable path reported %d B allocated; it must leave the figure zero", r.MutationAllocBytes)
	}

	// An absurd figure the path never measured must raise nothing in the
	// verdict the swarm runs.
	absurd := r
	absurd.MutationAllocBytes = 1 << 40
	if v := CheckGraphIOSurface(&absurd); len(v) != 0 {
		for _, viol := range v {
			t.Errorf("the swarm's verdict raised %s: %s", viol.Op, viol.Message)
		}
		t.Errorf("CheckGraphIOSurface adjudicated an unmeasured allocation figure of %d B — the process-global clause is back on the concurrently scheduled path",
			absurd.MutationAllocBytes)
	}

	// ... and the separate checker must refuse the unmeasured result.
	v := checkGraphIOMutationAlloc(&r)
	if len(v) == 0 {
		t.Errorf("checkGraphIOMutationAlloc passed a result carrying no measurement — a zero would then read as a pass")
	} else {
		t.Logf("witness: the unmeasured result is refused: %s", v[0].Message)
	}
}

// TestGraphIOMeasureProcessAlloc_IsLive proves the shared instrument both
// measuring sites use is live and sensitive. It is the sensitivity proof for the
// guard battery's per-cap heap bounds too, since [graphIOMeasure] measures
// through this same primitive.
func TestGraphIOMeasureProcessAlloc_IsLive(t *testing.T) {
	defer goleak.VerifyNone(t)

	// The noise floor: an empty window. It is witnessed rather than bounded
	// tightly — the counter is process-global, so its floor is not ours to fix.
	quiet := measureProcessAlloc(func() {})

	const (
		chunk  = 1 << 20
		chunks = 8
	)
	var held [][]byte
	loaded := measureProcessAlloc(func() {
		for i := 0; i < chunks; i++ {
			b := make([]byte, chunk)
			b[0] = byte(i + 1) // written, so the allocation cannot be optimised away
			held = append(held, b)
		}
	})
	runtime.KeepAlive(held)

	if want := uint64(chunk) * chunks; loaded < want {
		t.Errorf("a window that allocated %d B reported only %d B — the instrument is not live", want, loaded)
	}
	if quiet >= loaded {
		t.Errorf("the empty window reported %d B, not far below the loaded window's %d B — the figure is noise rather than measurement",
			quiet, loaded)
	}
	t.Logf("witness: empty window %d B (the noise floor); loaded window %d B for %d B deliberately allocated",
		quiet, loaded, uint64(chunk)*chunks)
}

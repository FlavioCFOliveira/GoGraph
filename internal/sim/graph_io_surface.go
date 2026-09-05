package sim

// graph_io_surface.go — the graph/io completeness surface (rmp #2480).
//
// # What was unreached, and why the gap was invisible
//
// Before this file the simulator touched graph/io through ST8 only
// (storage_fault_scenarios.go): a CSV and a JSONL edge-list round-trip over a
// SimDisk, plus a GraphML property-graph round-trip, each clean and then under
// an ENOSPC export fault. That left four holes, and every one of them was
// invisible to a green suite because nothing referenced the missing surface at
// all:
//
//   - graph/io/dot was imported nowhere in the simulator. The DOT writer is
//     the only exporter with no matching reader in the module, so a round-trip
//     cannot adjudicate it; this file adjudicates it by CROSS-FORMAT
//     AGREEMENT instead — the same model written as DOT, as CSV and as JSONL
//     must describe the same graph.
//   - the JSONL property-graph path (WriteWithProps / ReadWithProps) never
//     ran. ST8 covered the JSONL EDGE-LIST path and the GraphML property path,
//     so the one encoder that carries typed property kinds over JSON Lines was
//     unexercised.
//   - no byte-cap variant was ever driven, so none of the defensive caps in
//     graph/io raised its sentinel in a simulated run.
//   - no *Ctx reader was ever cancelled mid-parse, so the readers' cancellation
//     contract — a typed ctx error and a NIL graph, never a partial one —
//     rested on unit tests alone.
//
// # The structure
//
// Each arm follows the standing shape rmp #2470/#2472 fixed: the SEPARATE
// shape-only non-vacuity gate first, then the unconditional verdict, then the
// witness by t.Logf only. The seed-driven half ([RunGraphIOSurface]) is folded
// into ST8 so the swarm drives it across seeds; the crafted half
// ([RunGraphIOGuards]) is seed-independent by construction — no random mutation
// of an export will ever produce 65537 <key> declarations — and is driven once
// from its own test rather than on every seed.
//
// # What the caps actually are, as verified in source
//
// The audit list this task carried named eight sentinels and implied all eight
// are reader-side and provoked by feeding a mutated export back through an
// importer. Three of them are WRITER-side and cannot be reached that way at
// all: jsonl.ErrPropertyValueTooLarge, jsonl.ErrPropertyNestingTooDeep and
// their GraphML twins live in the encoders (graph/io/jsonl/writer.go,
// graph/io/graphml/writer_props.go), as does graphml.ErrInvalidXMLChar. They
// are provoked here by handing the writer a hostile GRAPH. See
// [GraphIOGuardDecls] for the verified side of every cap, and for the one cap
// that is structurally unreachable.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// -----------------------------------------------------------------------------
// The model
// -----------------------------------------------------------------------------

// graphIOHostileNames are the node identifiers the cross-format model is built
// from. They are chosen so that a single export drives every branch of the DOT
// writer's quoting rule ([dot.Write] quotes an id that is not [A-Za-z_][A-Za-z0-9_]*
// or whose lowercase form is a DOT reserved keyword) and the CSV writer's
// quoting and formula-sanitisation rules at once:
//
//	"graph"      — a DOT reserved keyword, quoted for that reason alone
//	"node with space", "héllo" — outside the bare-id alphabet
//	`a"b`, `a\b` — the two characters dot.quote escapes
//	"x->y"       — contains the directed edge operator
//	"p,q"        — forces CSV field quoting
//	"-danger"    — a leading '-' is a live spreadsheet formula, so it is the
//	               cell csv.Options.SanitizeFormulae rewrites
//	""           — the empty identifier the engine accepts (rmp #2043)
//	"#hash"      — begins with the CSV reader's comment character, and is the
//	               one id the CSV writer emits as a FORCE-QUOTED record
//	"plain0"...  — ordinary ids, so the bare (unquoted) DOT path also runs
//
// The '#'-leading id used to be deliberately ABSENT, on the recorded ground that
// "the CSV reader treats a leading comment character as a comment line, so such
// an id does not survive a CSV round-trip". Re-validating that claim under rmp
// #2533 REFUTED it: rmp #2042 had already made the CSV writer force-quote any
// cell whose first rune is the active comment rune, so the id round-trips
// intact, and the exclusion was preserving a claim that had stopped being true.
// It is now in the set, where it asserts the force-quoting path end to end
// instead of documenting a defect that no longer exists.
var graphIOHostileNames = []string{
	"graph", "node with space", "héllo", `a"b`, `a\b`, "x->y", "p,q", "-danger", "",
	"#hash", "plain0", "plain1", "plain2", "plain3",
}

// graphIOIsolatedName is the vertex that carries no incident edge. It exists to
// drive the DOT writer's bare-node-statement path and to make the one place the
// three formats legitimately DISAGREE observable: an edge-list CSV cannot carry
// an isolated vertex, while DOT and JSONL can.
const graphIOIsolatedName = "lonely_island"

// graphIOModel builds the deterministic directed simple graph the cross-format
// arm exports. Weights mix zero and non-zero so both the labelled and the
// unlabelled DOT edge branches run.
func graphIOModel(s *Seed) (*adjlist.AdjList[string, int64], error) {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	names := graphIOHostileNames
	for i := range names {
		// Two out-edges per vertex, one deliberately weightless.
		dst := names[(i+1+s.IntN(len(names)-1))%len(names)]
		if err := a.AddEdge(names[i], dst, int64(s.Uint64N(1000))); err != nil {
			return nil, fmt.Errorf("AddEdge %q->%q: %w", names[i], dst, err)
		}
		zdst := names[(i+2)%len(names)]
		if zdst == dst {
			continue
		}
		if err := a.AddEdge(names[i], zdst, 0); err != nil {
			return nil, fmt.Errorf("AddEdge %q->%q (weightless): %w", names[i], zdst, err)
		}
	}
	if err := a.AddNode(graphIOIsolatedName); err != nil {
		return nil, fmt.Errorf("AddNode %q: %w", graphIOIsolatedName, err)
	}
	return a, nil
}

// graphIOPropModel builds the labelled property graph the JSONL property arm
// round-trips. It carries one property of EVERY kind the JSONL encoder
// distinguishes — string, int64, float64, bool, time, bytes, list — because the
// wire format tags each with its own "kind" literal and a kind that is never
// written is a kind whose decode is never exercised.
func graphIOPropModel() (*lpg.Graph[string, int64], []string) {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	keys := []string{"p0", "p1", "p2", "p3"}
	// A fixed instant with a NON-UTC offset: the encoder formats in the value's
	// own location (rmp #1769), so a UTC-only fixture would not detect a
	// regression that normalised the offset away.
	inst := time.Date(2026, time.August, 18, 9, 30, 15, 123456789, time.FixedZone("CET", 2*3600))
	for i, k := range keys {
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "IONode")
		_ = g.SetNodeLabel(k, "Second")
		_ = g.SetNodeProperty(k, "s", lpg.StringValue("v"+strconv.Itoa(i)))
		_ = g.SetNodeProperty(k, "i", lpg.Int64Value(int64(i)*7-3))
		_ = g.SetNodeProperty(k, "f", lpg.Float64Value(float64(i)+0.5))
		_ = g.SetNodeProperty(k, "b", lpg.BoolValue(i%2 == 0))
		_ = g.SetNodeProperty(k, "t", lpg.TimeValue(inst.Add(time.Duration(i)*time.Second)))
		_ = g.SetNodeProperty(k, "y", lpg.BytesValue([]byte{0x00, byte(i), 0xFF}))
		_ = g.SetNodeProperty(k, "l", lpg.ListValue([]lpg.PropertyValue{
			lpg.StringValue("e" + strconv.Itoa(i)),
			lpg.Int64Value(int64(i)),
			lpg.ListValue([]lpg.PropertyValue{lpg.BoolValue(true)}),
		}))
	}
	_ = g.AddEdge("p0", "p1", 11)
	_ = g.AddEdge("p1", "p2", 0)
	_ = g.AddEdge("p0", "p3", -5)
	return g, keys
}

// graphIOPropKinds names every property key graphIOPropModel writes, so the
// comparison below can be exhaustive rather than sampling.
var graphIOPropKinds = []string{"s", "i", "f", "b", "t", "y", "l"}

// adjNodeNames returns every live vertex name of a, sorted, so two exports can
// be compared as sets without depending on internal id order.
func adjNodeNames(a *adjlist.AdjList[string, int64]) []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, a.Order())
	a.Mapper().Walk(func(_ graph.NodeID, v string) bool {
		out = append(out, v)
		return true
	})
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// A DOT reader for the writer that has none
// -----------------------------------------------------------------------------

// dotDocument is the parsed form of a DOT export: the graph kind, the vertex
// set, the (src, dst, weight) multiset in the same canonical encoding
// [edgeTriples] uses, and the counters that make the arm's non-vacuity
// checkable.
type dotDocument struct {
	Triples map[string]int
	Nodes   []string
	// BareNodes counts statements of the form `id;` — the only way an isolated
	// vertex survives a DOT export.
	BareNodes int
	// QuotedIDs counts identifiers the writer had to quote, and Labelled counts
	// edges that carried a [label="w"] attribute. Both are non-vacuity
	// evidence: a model that drove neither branch would adjudicate nothing
	// about the writer's quoting or weight encoding.
	QuotedIDs int
	Labelled  int
	Directed  bool
}

// parseDOT parses the subset of the DOT language [dot.Write] emits: a
// `digraph G {` / `graph G {` header, edge statements `a -> b;` with an
// optional ` [label="w"]`, bare node statements `a;`, and a closing `}`.
//
// It is a genuine character scanner rather than a line split because the writer
// quotes an identifier containing the edge operator or a statement terminator,
// and a line-oriented parser would mis-split exactly the hostile identifiers
// this arm exists to drive. Anything outside that subset is an error: the
// parser is the oracle here, so it must refuse to guess.
//
// one flat scanner: header + per-statement id/op/attr/terminator
func parseDOT(src string) (dotDocument, error) {
	d := dotDocument{Triples: make(map[string]int)}
	p := &dotParser{s: src}
	p.skipSpace()
	switch {
	case p.consumeWord("digraph"):
		d.Directed = true
	case p.consumeWord("graph"):
		d.Directed = false
	default:
		return d, fmt.Errorf("dot: expected a graph header at offset %d", p.i)
	}
	p.skipSpace()
	if !p.consumeWord("G") {
		return d, fmt.Errorf("dot: expected the graph name at offset %d", p.i)
	}
	p.skipSpace()
	if !p.consume('{') {
		return d, fmt.Errorf("dot: expected '{' at offset %d", p.i)
	}
	wantOp := "->"
	if !d.Directed {
		wantOp = "--"
	}
	seen := make(map[string]struct{})
	note := func(name string) {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			d.Nodes = append(d.Nodes, name)
		}
	}
	for {
		p.skipSpace()
		if p.consume('}') {
			break
		}
		if p.eof() {
			return d, errors.New("dot: unterminated graph body")
		}
		src, quoted, err := p.readID()
		if err != nil {
			return d, err
		}
		if quoted {
			d.QuotedIDs++
		}
		note(src)
		p.skipSpace()
		if p.consume(';') {
			d.BareNodes++
			continue
		}
		if !p.consumeWord(wantOp) {
			return d, fmt.Errorf("dot: expected %q at offset %d", wantOp, p.i)
		}
		p.skipSpace()
		dst, dquoted, err := p.readID()
		if err != nil {
			return d, err
		}
		if dquoted {
			d.QuotedIDs++
		}
		note(dst)
		p.skipSpace()
		var w int64
		if p.consume('[') {
			p.skipSpace()
			if !p.consumeWord("label=") {
				return d, fmt.Errorf("dot: expected label= at offset %d", p.i)
			}
			lit, _, lerr := p.readID()
			if lerr != nil {
				return d, lerr
			}
			w, err = strconv.ParseInt(lit, 10, 64)
			if err != nil {
				return d, fmt.Errorf("dot: edge label %q: %w", lit, err)
			}
			p.skipSpace()
			if !p.consume(']') {
				return d, fmt.Errorf("dot: expected ']' at offset %d", p.i)
			}
			d.Labelled++
			p.skipSpace()
		}
		if !p.consume(';') {
			return d, fmt.Errorf("dot: expected ';' at offset %d", p.i)
		}
		d.Triples[src+"\x00"+dst+"\x00"+strconv.FormatInt(w, 10)]++
	}
	sort.Strings(d.Nodes)
	return d, nil
}

// dotParser is the cursor parseDOT walks the document with.
type dotParser struct {
	s string
	i int
}

func (p *dotParser) eof() bool { return p.i >= len(p.s) }

func (p *dotParser) skipSpace() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\r', '\n':
			p.i++
		default:
			return
		}
	}
}

func (p *dotParser) consume(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *dotParser) consumeWord(w string) bool {
	if strings.HasPrefix(p.s[p.i:], w) {
		p.i += len(w)
		return true
	}
	return false
}

// readID reads one DOT identifier, reversing the writer's quoting, and reports
// whether it was quoted.
func (p *dotParser) readID() (string, bool, error) {
	if p.eof() {
		return "", false, fmt.Errorf("dot: expected an identifier at offset %d", p.i)
	}
	if p.s[p.i] == '"' {
		p.i++
		var b strings.Builder
		for p.i < len(p.s) {
			c := p.s[p.i]
			switch c {
			case '\\':
				if p.i+1 >= len(p.s) {
					return "", true, errors.New("dot: trailing escape inside a quoted identifier")
				}
				b.WriteByte(p.s[p.i+1])
				p.i += 2
			case '"':
				p.i++
				return b.String(), true, nil
			default:
				b.WriteByte(c)
				p.i++
			}
		}
		return "", true, errors.New("dot: unterminated quoted identifier")
	}
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.i++
			continue
		}
		break
	}
	if p.i == start {
		return "", false, fmt.Errorf("dot: expected an identifier at offset %d", p.i)
	}
	return p.s[start:p.i], false, nil
}

// -----------------------------------------------------------------------------
// The cross-format surface (seed-driven; folded into ST8)
// -----------------------------------------------------------------------------

// GraphIOPropsObservation records the JSONL property-graph round-trip: the
// exported size, how many "property" records the encoder emitted, which kind
// tags appeared on the wire, and whether the re-imported graph reproduced the
// model.
type GraphIOPropsObservation struct {
	Mismatch        string
	KindsOnWire     []string
	Bytes           int
	Rows            int
	PropertyRecords int
	LabelRecords    int
	Equal           bool
}

// GraphIOCSVArm is one point of the csv.Options space: a delimiter, a comment
// rune, a header flag and the formula-sanitisation flag, driven either by
// exporting the model (Literal == "") or by importing a hand-built document
// (Literal != "") that no exporter produces — the two-column weightless layout,
// a comment line, and a header row.
type GraphIOCSVArm struct {
	Name      string
	Literal   string
	ImportErr string
	Want      map[string]int
	Delimiter rune
	Comment   rune
	Bytes     int
	Rows      int
	HasHeader bool
	Sanitize  bool
	// ExpectRoundTrip is the arm's DECLARED outcome. It is false for exactly one
	// arm — the sanitised one — because csv.Options.SanitizeFormulae documents
	// that an apostrophe-prefixed cell no longer re-imports byte-identically.
	// Declaring it makes that documented asymmetry an assertion instead of an
	// unexplained failure.
	ExpectRoundTrip bool
	RoundTrips      bool
}

// GraphIOMutation is one seed-derived corruption of an export, fed back through
// the matching importer.
type GraphIOMutation struct {
	Format   string
	Kind     string
	Err      string
	Panicked string
	Offset   int
	Source   int
	Changed  bool
	Typed    bool
	Equal    bool
}

// Effective reports whether the mutation actually altered what the importer
// produced. A mutation that leaves the re-imported graph equal to the model and
// raises no error proves nothing, so the non-vacuity gate counts these.
func (m *GraphIOMutation) Effective() bool { return m.Err != "" || !m.Equal }

// GraphIOSurfaceResult is one seed's observation of the cross-format surface.
type GraphIOSurfaceResult struct {
	ModelTriples map[string]int
	CSVTriples   map[string]int
	JSONLTriples map[string]int
	ModelNodes   []string
	CSVNodes     []string
	JSONLNodes   []string
	CSVArms      []GraphIOCSVArm
	Mutations    []GraphIOMutation
	DOT          dotDocument
	Props        GraphIOPropsObservation
	Seed         uint64
	DOTBytes     int
	CSVBytes     int
	JSONLBytes   int
	// MutationAllocBytes is the heap allocated by the mutation sweep's REPLAY
	// LOOP — its own exports sit outside the window — and MutationInputBytes
	// the total input fed to that loop. The ratio is the bounded-allocation
	// evidence for the readers, but ONLY when
	// AllocMeasured is true: the figure comes from a process-global counter, so
	// [RunGraphIOSurface] — which the swarm runs concurrently — does not measure
	// it at all and leaves it zero (rmp #2553).
	MutationAllocBytes uint64
	MutationInputBytes int
	// AllocMeasured reports whether MutationAllocBytes was actually measured,
	// under the exclusivity measureProcessAlloc requires. A verdict over the
	// ratio must refuse a result where this is false rather than read a zero as
	// a pass.
	AllocMeasured bool
	// ExportStability maps an encoder to the number of repeat exports of the
	// SAME graph that differed byte for byte from the first, out of
	// graphIOStabilityRuns-1 repeats. Every encoder here is expected to be
	// byte-reproducible EXCEPT jsonl.WriteWithProps, which emits one record per
	// entry of the node's property map and therefore in Go's randomised map
	// order. That one is witnessed rather than asserted, so a future fix does
	// not fail the run; the canonical form the mutation sweep uses is asserted
	// instead.
	ExportStability map[string]int
}

// graphIOStabilityRuns is how many times each encoder is re-run over the same
// graph when measuring byte-reproducibility. Eight is ample: a writer emitting
// n > 1 property records in map order reproduces its first ordering with
// probability well under a half per repeat.
const graphIOStabilityRuns = 8

// graphIOUnstableEncoder is the one encoder whose byte output is NOT asserted
// stable, because it is measurably not. It is named once here so the exemption
// is explicit rather than an omission from a list.
const graphIOUnstableEncoder = "jsonl.WriteWithProps"

// graphIOMeasureExportStability re-exports the same two models through every
// encoder and counts how many repeats differ from the first.
func graphIOMeasureExportStability(ctx context.Context, model *adjlist.AdjList[string, int64]) (map[string]int, error) {
	propModel, _ := graphIOPropModel()
	encoders := map[string]func() ([]byte, error){
		"dot.Write": func() ([]byte, error) {
			var b bytes.Buffer
			if err := dot.WriteCtx(ctx, &b, model); err != nil {
				return nil, err
			}
			return b.Bytes(), nil
		},
		"csv.Write": func() ([]byte, error) {
			var b bytes.Buffer
			o := csv.DefaultOptions()
			o.Directed = true
			_, err := csv.WriteCtx(ctx, &b, model, o)
			return b.Bytes(), err
		},
		"jsonl.Write": func() ([]byte, error) {
			var b bytes.Buffer
			_, err := jsonl.WriteCtx(ctx, &b, model)
			return b.Bytes(), err
		},
		"graphml.Write": func() ([]byte, error) {
			var b bytes.Buffer
			if err := graphml.WriteCtx(ctx, &b, model); err != nil {
				return nil, err
			}
			return b.Bytes(), nil
		},
		"graphml.WriteWithProps": func() ([]byte, error) {
			var b bytes.Buffer
			if err := graphml.WriteWithPropsCtx(ctx, &b, propModel); err != nil {
				return nil, err
			}
			return b.Bytes(), nil
		},
		graphIOUnstableEncoder: func() ([]byte, error) {
			var b bytes.Buffer
			_, err := jsonl.WriteWithPropsCtx(ctx, &b, propModel)
			return b.Bytes(), err
		},
		graphIOUnstableEncoder + "/canonical": func() ([]byte, error) {
			var b bytes.Buffer
			_, err := jsonl.WriteWithPropsCtx(ctx, &b, propModel)
			return graphIOCanonicalJSONLProps(b.Bytes()), err
		},
	}
	out := make(map[string]int, len(encoders))
	for name, enc := range encoders {
		first, err := enc()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		differed := 0
		for i := 1; i < graphIOStabilityRuns; i++ {
			again, aerr := enc()
			if aerr != nil {
				return nil, fmt.Errorf("%s repeat %d: %w", name, i, aerr)
			}
			if !bytes.Equal(first, again) {
				differed++
			}
		}
		out[name] = differed
	}
	return out, nil
}

// RunGraphIOSurface drives the whole seed-dependent half of the graph/io
// completeness surface for one seed: the DOT / CSV / JSONL cross-format
// agreement, the JSONL property-graph round-trip, the csv.Options matrix, and
// the mutated-export sweep. It reports a harness error only when the harness
// itself could not proceed; every adjudication is left to
// [CheckGraphIOSurface] and [CheckGraphIOSurfaceShape].
//
// It performs NO allocation measurement, because it is reachable from the
// concurrently scheduled io-roundtrip-fault scenario; the bounded-allocation
// property is adjudicated by the serialised arm through
// [checkGraphIOMutationAlloc].
func RunGraphIOSurface(ctx context.Context, seed uint64) (GraphIOSurfaceResult, error) {
	return runGraphIOSurface(ctx, seed, graphIOSurfaceOpts{})
}

// graphIOSurfaceOpts configures one [runGraphIOSurface] pass. Both fields are
// zero on every production path; they exist for the SERIALISED allocation arm.
type graphIOSurfaceOpts struct {
	// measureAlloc opens a process-global allocation window around the REPLAY
	// LOOP inside the mutation sweep — the sweep's own exports and its offset
	// draw stay outside it. See measureProcessAlloc for the contract it imposes
	// on the caller: it is only ever set by a serialised arm.
	measureAlloc bool
	// inflate is handed to graphIOMutationSweep; see that function.
	inflate func()
}

// runGraphIOSurface is the body of [RunGraphIOSurface], with the two options
// only the serialised allocation arm ever sets.
func runGraphIOSurface(ctx context.Context, seed uint64, opts graphIOSurfaceOpts) (GraphIOSurfaceResult, error) {
	r := GraphIOSurfaceResult{Seed: seed}
	s := NewSeed(seed)
	model, err := graphIOModel(s)
	if err != nil {
		return r, fmt.Errorf("sim: graph-io model: %w", err)
	}
	r.ModelNodes = adjNodeNames(model)
	r.ModelTriples = edgeTriples(model)

	// --- DOT: written by the module, read back by parseDOT. ---
	var dotBuf bytes.Buffer
	if werr := dot.WriteCtx(ctx, &dotBuf, model); werr != nil {
		return r, fmt.Errorf("sim: graph-io dot export: %w", werr)
	}
	r.DOTBytes = dotBuf.Len()
	doc, perr := parseDOT(dotBuf.String())
	if perr != nil {
		return r, fmt.Errorf("sim: graph-io dot parse: %w", perr)
	}
	r.DOT = doc

	// --- CSV and JSONL: written and read by the module. ---
	csvOpts := csv.DefaultOptions()
	csvOpts.Directed = true
	var csvBuf bytes.Buffer
	if _, werr := csv.WriteCtx(ctx, &csvBuf, model, csvOpts); werr != nil {
		return r, fmt.Errorf("sim: graph-io csv export: %w", werr)
	}
	r.CSVBytes = csvBuf.Len()
	csvGot, _, cerr := csv.ReadIntoCtx(ctx, bytes.NewReader(csvBuf.Bytes()), csvOpts)
	if cerr != nil {
		return r, fmt.Errorf("sim: graph-io csv import: %w", cerr)
	}
	r.CSVNodes, r.CSVTriples = adjNodeNames(csvGot), edgeTriples(csvGot)

	cfg := adjlist.Config{Directed: true, Multigraph: false}
	var jsonlBuf bytes.Buffer
	if _, werr := jsonl.WriteCtx(ctx, &jsonlBuf, model); werr != nil {
		return r, fmt.Errorf("sim: graph-io jsonl export: %w", werr)
	}
	r.JSONLBytes = jsonlBuf.Len()
	jGot, _, jerr := jsonl.ReadIntoCtx(ctx, bytes.NewReader(jsonlBuf.Bytes()), cfg)
	if jerr != nil {
		return r, fmt.Errorf("sim: graph-io jsonl import: %w", jerr)
	}
	r.JSONLNodes, r.JSONLTriples = adjNodeNames(jGot), edgeTriples(jGot)

	// --- the JSONL property-graph path. ---
	props, perr2 := graphIOPropsRoundTrip(ctx, cfg)
	if perr2 != nil {
		return r, fmt.Errorf("sim: graph-io jsonl props: %w", perr2)
	}
	r.Props = props

	// --- the csv.Options matrix. ---
	arms, aerr := graphIOCSVArms(ctx, model, r.ModelTriples)
	if aerr != nil {
		return r, fmt.Errorf("sim: graph-io csv options: %w", aerr)
	}
	r.CSVArms = arms

	// --- the mutated-export sweep. ---
	muts, allocBytes, inBytes, merr := graphIOMutationSweep(ctx, s, model, r.ModelTriples,
		csvBuf.Bytes(), jsonlBuf.Bytes(), csvOpts, cfg, opts.measureAlloc, opts.inflate)
	if merr != nil {
		return r, fmt.Errorf("sim: graph-io mutation sweep: %w", merr)
	}
	r.Mutations, r.MutationInputBytes = muts, inBytes
	if opts.measureAlloc {
		r.MutationAllocBytes, r.AllocMeasured = allocBytes, true
	}

	// --- byte-reproducibility of every encoder. ---
	stability, serr := graphIOMeasureExportStability(ctx, model)
	if serr != nil {
		return r, fmt.Errorf("sim: graph-io export stability: %w", serr)
	}
	r.ExportStability = stability
	return r, nil
}

// graphIOPropsRoundTrip exports the property model through jsonl.WriteWithPropsCtx
// and re-imports it through jsonl.ReadWithPropsCtx, censusing the wire so the
// arm's non-vacuity can be judged from what was actually written.
func graphIOPropsRoundTrip(ctx context.Context, cfg adjlist.Config) (GraphIOPropsObservation, error) {
	var obs GraphIOPropsObservation
	model, keys := graphIOPropModel()
	var buf bytes.Buffer
	rows, werr := jsonl.WriteWithPropsCtx(ctx, &buf, model)
	if werr != nil {
		return obs, fmt.Errorf("export: %w", werr)
	}
	obs.Bytes, obs.Rows = buf.Len(), rows
	kinds := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		switch {
		case strings.Contains(line, `"type":"property"`):
			obs.PropertyRecords++
			if i := strings.Index(line, `"kind":"`); i >= 0 {
				rest := line[i+len(`"kind":"`):]
				if j := strings.IndexByte(rest, '"'); j > 0 {
					kinds[rest[:j]] = struct{}{}
				}
			}
		case strings.Contains(line, `"labels":[`):
			obs.LabelRecords++
		}
	}
	obs.KindsOnWire = make([]string, 0, len(kinds))
	for k := range kinds {
		obs.KindsOnWire = append(obs.KindsOnWire, k)
	}
	sort.Strings(obs.KindsOnWire)

	got, _, rerr := jsonl.ReadWithPropsCtx(ctx, bytes.NewReader(buf.Bytes()), cfg)
	if rerr != nil {
		return obs, fmt.Errorf("import: %w", rerr)
	}
	obs.Equal, obs.Mismatch = graphIOPropsEqual(got, model, keys)
	return obs, nil
}

// graphIOPropsEqual reports whether got reproduces want over the modelled keys —
// order, size, label set and every property of every kind — and, when it does
// not, the first difference found.
func graphIOPropsEqual(got, want *lpg.Graph[string, int64], keys []string) (bool, string) {
	if got == nil {
		return false, "the import returned a nil graph"
	}
	if got.AdjList().Order() != want.AdjList().Order() {
		return false, fmt.Sprintf("order %d, want %d", got.AdjList().Order(), want.AdjList().Order())
	}
	if got.AdjList().Size() != want.AdjList().Size() {
		return false, fmt.Sprintf("size %d, want %d", got.AdjList().Size(), want.AdjList().Size())
	}
	if !triplesEqual(edgeTriples(want.AdjList()), edgeTriples(got.AdjList())) {
		return false, "the edge multiset differs"
	}
	for _, k := range keys {
		gl, wl := append([]string(nil), got.NodeLabels(k)...), append([]string(nil), want.NodeLabels(k)...)
		sort.Strings(gl)
		sort.Strings(wl)
		if strings.Join(gl, ",") != strings.Join(wl, ",") {
			return false, fmt.Sprintf("node %q labels %v, want %v", k, gl, wl)
		}
		gp, wp := got.NodeProperties(k), want.NodeProperties(k)
		for _, pk := range graphIOPropKinds {
			gv, gok := gp[pk]
			wv, wok := wp[pk]
			if gok != wok {
				return false, fmt.Sprintf("node %q property %q presence %t, want %t", k, pk, gok, wok)
			}
			if !graphIOPropValueEqual(gv, wv) {
				return false, fmt.Sprintf("node %q property %q = %v, want %v", k, pk, gv, wv)
			}
		}
	}
	return true, ""
}

// graphIOPropValueEqual compares two property values structurally. Lists are
// compared elementwise (a PropertyValue holding a slice is not comparable with
// ==), times by instant AND zone offset — the offset is what rmp #1769 made the
// encoder preserve — and bytes by content.
func graphIOPropValueEqual(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropList:
		ae, _ := a.List()
		be, _ := b.List()
		if len(ae) != len(be) {
			return false
		}
		for i := range ae {
			if !graphIOPropValueEqual(ae[i], be[i]) {
				return false
			}
		}
		return true
	case lpg.PropBytes:
		ab, _ := a.Bytes()
		bb, _ := b.Bytes()
		return bytes.Equal(ab, bb)
	case lpg.PropTime:
		at, _ := a.Time()
		bt, _ := b.Time()
		_, ao := at.Zone()
		_, bo := bt.Zone()
		return at.Equal(bt) && ao == bo
	default:
		return a == b
	}
}

// -----------------------------------------------------------------------------
// The csv.Options matrix
// -----------------------------------------------------------------------------

// graphIOCSVArms drives the delimiter / comment / header / sanitisation space of
// [csv.Options], which every prior caller reached only through
// [csv.DefaultOptions]. Arms with an empty Literal export the model and
// re-import it; arms with a Literal import a hand-built document no exporter
// produces — the two-column weightless layout, a comment line, and a header row
// — each paired with the SAME document under the opposite flag, so the flag's
// effect is measured rather than assumed.
func graphIOCSVArms(ctx context.Context, model *adjlist.AdjList[string, int64], want map[string]int) ([]GraphIOCSVArm, error) {
	// Two documents, each read twice under opposite flags. The pairing is the
	// point: "the header was skipped" is only meaningful beside a run in which
	// it was not.
	const headerDoc = "s0,d0\na,b,5\n"
	const commentDoc = "%x,y,3\na,b,5\n"
	tri := func(pairs ...[3]string) map[string]int {
		m := make(map[string]int, len(pairs))
		for _, p := range pairs {
			m[p[0]+"\x00"+p[1]+"\x00"+p[2]]++
		}
		return m
	}
	arms := []GraphIOCSVArm{
		{Name: "export/default", Delimiter: ',', Comment: '#', Want: want, ExpectRoundTrip: true},
		{Name: "export/tab+header", Delimiter: '\t', Comment: '#', HasHeader: true, Want: want, ExpectRoundTrip: true},
		{Name: "export/semicolon+pct-comment", Delimiter: ';', Comment: '%', Want: want, ExpectRoundTrip: true},
		{Name: "export/pipe", Delimiter: '|', Comment: '#', Want: want, ExpectRoundTrip: true},
		// SanitizeFormulae rewrites the "-danger" cell to "'-danger", which the
		// package documents as breaking the byte-identical round-trip. The arm
		// asserts that documented loss rather than tolerating it.
		{Name: "export/sanitised", Delimiter: ',', Comment: '#', Sanitize: true, Want: want, ExpectRoundTrip: false},
		{
			Name: "literal/two-column-weightless", Literal: "a,b\nb,c\n", Delimiter: ',', Comment: '#',
			Want: tri([3]string{"a", "b", "0"}, [3]string{"b", "c", "0"}), ExpectRoundTrip: true,
		},
		{
			Name: "literal/header-skipped", Literal: headerDoc, Delimiter: ',', Comment: '#', HasHeader: true,
			Want: tri([3]string{"a", "b", "5"}), ExpectRoundTrip: true,
		},
		{
			Name: "literal/header-not-skipped", Literal: headerDoc, Delimiter: ',', Comment: '#',
			Want: tri([3]string{"s0", "d0", "0"}, [3]string{"a", "b", "5"}), ExpectRoundTrip: true,
		},
		{
			Name: "literal/comment-honoured", Literal: commentDoc, Delimiter: ',', Comment: '%',
			Want: tri([3]string{"a", "b", "5"}), ExpectRoundTrip: true,
		},
		{
			Name: "literal/comment-is-data", Literal: commentDoc, Delimiter: ',', Comment: '#',
			Want: tri([3]string{"%x", "y", "3"}, [3]string{"a", "b", "5"}), ExpectRoundTrip: true,
		},
		{
			Name: "literal/pipe-delimited", Literal: "a|b|9\n", Delimiter: '|', Comment: '#',
			Want: tri([3]string{"a", "b", "9"}), ExpectRoundTrip: true,
		},
	}
	for i := range arms {
		arm := &arms[i]
		opts := csv.DefaultOptions()
		opts.Directed = true
		opts.Delimiter = arm.Delimiter
		opts.Comment = arm.Comment
		opts.HasHeader = arm.HasHeader
		opts.SanitizeFormulae = arm.Sanitize

		src := arm.Literal
		if src == "" {
			var buf bytes.Buffer
			n, werr := csv.WriteCtx(ctx, &buf, model, opts)
			if werr != nil {
				return nil, fmt.Errorf("%s export: %w", arm.Name, werr)
			}
			arm.Rows = n
			src = buf.String()
		}
		arm.Bytes = len(src)
		got, rows, rerr := csv.ReadIntoCtx(ctx, strings.NewReader(src), opts)
		if rerr != nil {
			arm.ImportErr = rerr.Error()
			continue
		}
		if arm.Literal != "" {
			arm.Rows = rows
		}
		arm.RoundTrips = got != nil && triplesEqual(arm.Want, edgeTriples(got))
	}
	return arms, nil
}

// -----------------------------------------------------------------------------
// The shared allocation instrument
// -----------------------------------------------------------------------------

// graphIOAllocMu serialises every allocation window this file opens, so two
// measured arms in the same process cannot bill each other.
var graphIOAllocMu sync.Mutex

// measureProcessAlloc runs fn and reports the bytes the PROCESS allocated while
// it ran.
//
// CONCURRENCY CONTRACT — the caller must hold the rest of the process quiet.
// runtime.MemStats.TotalAlloc is a process-global cumulative counter, so the
// delta below is what EVERY goroutine allocated during the window, not what fn
// allocated. Go offers no per-goroutine attribution to do better with: as of
// go1.26.6 the runtime exports only ReadMemStats (global) and MemProfile
// (sampled, per call stack, up to two GC cycles stale), runtime/metrics carries
// no per-goroutine allocation metric, and the heap profile does not record
// pprof labels. The figure is therefore evidence ONLY in a serialised arm, and
// is not evidence at all inside a scenario the swarm may schedule alongside
// others (rmp #2553).
//
// runtime.ReadMemStats also stops the world, so a window opened on a
// concurrently scheduled path stalls every other worker for its duration.
func measureProcessAlloc(fn func()) uint64 {
	graphIOAllocMu.Lock()
	defer graphIOAllocMu.Unlock()
	graphIOAllocWindowOpen.Store(true)
	defer graphIOAllocWindowOpen.Store(false)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	fn()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

// graphIOAllocWindowOpen is true while [measureProcessAlloc] is between its two
// ReadMemStats calls.
//
// It exists so the falsifiability proof can assert INTERVAL CONTAINMENT — that
// each injected allocation happened while the window was open — instead of
// differencing two separately-taken readings of a process-global counter
// (rmp #2555).
//
// The difference matters because the counter is per-PROCESS and monotonic, so the
// only thing that can perturb the difference is this process's own allocation
// varying between the control and the inflated run: map growth, size-class
// rounding, GC timing. Under CPU contention that variance grew enough to flip the
// comparison, measured at 1 failure in 20 runs against six concurrent
// allocation-load processes even with a tolerance of 1% of the injected amount.
// A boolean read at the moment of injection has no such variance.
//
// Written only under graphIOAllocMu, so windows cannot nest or overlap; read
// without it by the injection hook, which is why it is atomic.
var graphIOAllocWindowOpen atomic.Bool

// -----------------------------------------------------------------------------
// The mutated-export sweep
// -----------------------------------------------------------------------------

// graphIOMutationKinds are the corruptions applied to each export. A byte flip
// and a truncation model a torn or bit-rotted artefact; a spliced prefix models
// a duplicated write; a delimiter run models the structural amplification the
// per-record field cap exists to refuse.
var graphIOMutationKinds = []string{"flip", "truncate", "splice", "delimiter-run"}

// graphIOMutate applies one corruption at off and returns the mutated bytes.
// Every kind is constructed so the result CANNOT equal the input: a mutation
// that left the bytes unchanged would make the arm vacuous, so the sweep
// records Changed and the gate rejects a run in which any mutation was inert.
func graphIOMutate(kind string, src []byte, off int) []byte {
	out := make([]byte, len(src), len(src)+70000)
	copy(out, src)
	switch kind {
	case "flip":
		out[off] ^= 0xFF
		return out
	case "truncate":
		return out[:off]
	case "splice":
		return append(out, out[:off+1]...)
	case "delimiter-run":
		// 70000 > the 65536-field per-record ceiling graph/io/csv enforces, so a
		// run landing outside a quoted field provokes csv.ErrTooManyFields.
		run := bytes.Repeat([]byte{','}, 70000)
		return append(append(append([]byte{}, out[:off]...), run...), out[off:]...)
	}
	return out
}

// graphIOMutationSweep replays every (format, mutation) pair for one seed and
// returns the observations, the heap the REPLAY LOOP allocated and the input
// consumed.
//
// Every importer is driven through its CAPPED entry point at four times the
// source size. That is the second half of the allocation bound: a mutation that
// tricked a reader into consuming without limit trips the cap instead of
// growing the heap, and the measured ratio then confirms the cap held.
//
// measure opens the allocation window, and encloses the REPLAY LOOP ALONE. The
// sweep's own exports and the offset draw sit outside it deliberately: the
// bound is over what the READERS allocate against hostile input, and
// inputBytes — its denominator — counts the mutated inputs only, so billing an
// exporter into the numerator would let an unrelated encoder flap the reader
// bound. It is set only by the SERIALISED arm
// (TestGraphIOSurface_MutationAllocBoundHoldsSerially, which adjudicates the
// figure through [checkGraphIOMutationAlloc]), because
// runtime.MemStats.TotalAlloc is process-global: measured on a path the swarm
// schedules concurrently it bills this sweep for its neighbours (rmp #2553).
// It is false everywhere else, and the figure is then left zero.
//
// inflate, when non-nil, is called once per mutation INSIDE the replay loop. It
// is nil on every production path; it exists so the allocation oracle can be
// shown to fail on genuine over-allocation (see the sensitivity test), and it
// sits inside the loop deliberately, so a measurement window that stopped
// enclosing the replay would be caught.
func graphIOMutationSweep(
	ctx context.Context,
	s *Seed,
	model *adjlist.AdjList[string, int64],
	want map[string]int,
	csvBytes, jsonlBytes []byte,
	csvOpts csv.Options,
	cfg adjlist.Config,
	measure bool,
	inflate func(),
) ([]GraphIOMutation, uint64, int, error) {
	var graphmlBuf bytes.Buffer
	if err := graphml.WriteCtx(ctx, &graphmlBuf, model); err != nil {
		return nil, 0, 0, fmt.Errorf("graphml export: %w", err)
	}
	propModel, propKeys := graphIOPropModel()
	var propBuf bytes.Buffer
	if _, err := jsonl.WriteWithPropsCtx(ctx, &propBuf, propModel); err != nil {
		return nil, 0, 0, fmt.Errorf("jsonl props export: %w", err)
	}
	// jsonl.WriteWithProps emits one "property" record per entry of the node's
	// property MAP, in Go map-iteration order, so two exports of the same graph
	// differ byte for byte (measured: 4 of 7 repeat exports differed; the
	// GraphML property writer, which emits in a fixed key order, differed in 0
	// of 7). A seed-derived byte offset into an unstable artefact is not
	// reproducible, so the sweep mutates the CANONICAL form. See
	// [graphIOCanonicalJSONLProps].
	propSrc := graphIOCanonicalJSONLProps(propBuf.Bytes())

	type target struct {
		name string
		src  []byte
		// imp re-imports the mutated bytes and reports whether the result still
		// equals the model, under the byte cap the sweep imposes.
		imp func(r io.Reader, capBytes int64) (equal bool, err error)
	}
	targets := []target{
		{name: "csv", src: csvBytes, imp: func(r io.Reader, capBytes int64) (bool, error) {
			o := csvOpts
			o.MaxBytes = capBytes
			got, _, err := csv.ReadIntoCtx(ctx, r, o)
			return got != nil && triplesEqual(want, edgeTriples(got)), err
		}},
		{name: "jsonl", src: jsonlBytes, imp: func(r io.Reader, capBytes int64) (bool, error) {
			got, _, err := jsonl.ReadIntoCappedCtx(ctx, r, cfg, capBytes)
			return got != nil && triplesEqual(want, edgeTriples(got)), err
		}},
		{name: "graphml", src: graphmlBuf.Bytes(), imp: func(r io.Reader, capBytes int64) (bool, error) {
			got, _, err := graphml.ReadIntoCappedCtx(ctx, r, capBytes)
			return got != nil && triplesEqual(want, edgeTriples(got)), err
		}},
		{name: "jsonl-props", src: propSrc, imp: func(r io.Reader, capBytes int64) (bool, error) {
			got, _, err := jsonl.ReadWithPropsCappedCtx(ctx, r, cfg, capBytes)
			eq, _ := graphIOPropsEqual(got, propModel, propKeys)
			return eq, err
		}},
	}

	// Draw every offset BEFORE the measured region so the PRNG draw is not part
	// of the allocation being attributed to the readers.
	type job struct {
		t    *target
		kind string
		off  int
	}
	jobs := make([]job, 0, len(targets)*len(graphIOMutationKinds))
	for i := range targets {
		t := &targets[i]
		if len(t.src) < 4 {
			return nil, 0, 0, fmt.Errorf("%s export is %d bytes, too small to mutate", t.name, len(t.src))
		}
		for _, kind := range graphIOMutationKinds {
			// The offset is drawn against the ACTUAL export length, and the
			// length is recorded on the observation: a seed-drawn offset is
			// only reproducible while the artefact's size is, so the size is
			// carried in the witness rather than assumed stable.
			off := 1 + int(s.Uint64N(uint64(len(t.src)-2)))
			jobs = append(jobs, job{t: t, kind: kind, off: off})
		}
	}

	inputBytes := 0
	out := make([]GraphIOMutation, 0, len(jobs))
	replay := func() {
		for _, j := range jobs {
			obs := GraphIOMutation{Format: j.t.name, Kind: j.kind, Offset: j.off, Source: len(j.t.src)}
			mutated := graphIOMutate(j.kind, j.t.src, j.off)
			obs.Changed = !bytes.Equal(mutated, j.t.src)
			inputBytes += len(mutated)
			// The cap scales with the MUTATED size, not the source: a fixed cap
			// sized from the source would be tripped by the delimiter-run mutation
			// itself, and every arm would then report the byte cap instead of the
			// structural guard the corruption was aimed at.
			equal, err := graphIOImportGuarded(j.t.imp, mutated, int64(4*len(mutated)+4096), &obs)
			obs.Equal = equal
			if err != nil {
				obs.Err = err.Error()
				obs.Typed = graphIOIsKnownSentinel(err)
			}
			out = append(out, obs)
			if inflate != nil {
				inflate()
			}
		}
	}
	var alloc uint64
	if measure {
		alloc = measureProcessAlloc(replay)
	} else {
		replay()
	}
	return out, alloc, inputBytes, nil
}

// graphIOCanonicalJSONLProps returns src with its "property" records sorted,
// leaving every other record in place.
//
// It exists because jsonl.WriteWithProps walks the node's property MAP, so the
// order of its property records is Go's randomised map order and two exports of
// the same graph are not byte-identical. The GraphML property writer emits in a
// fixed key order and is byte-stable, so this canonicalisation is needed for the
// JSONL artefact alone. The writer already emits every property record after
// every node record, so sorting that suffix preserves the ordering the reader
// requires (a property record must follow the node record it names).
func graphIOCanonicalJSONLProps(src []byte) []byte {
	lines := strings.Split(strings.TrimRight(string(src), "\n"), "\n")
	head := make([]string, 0, len(lines))
	props := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, `"type":"property"`) {
			props = append(props, l)
			continue
		}
		head = append(head, l)
	}
	sort.Strings(props)
	return []byte(strings.Join(append(head, props...), "\n") + "\n")
}

// graphIOImportGuarded runs one import and converts a panic into a recorded
// observation. The library must never panic on hostile input; recovering here
// is not hiding the defect but capturing it, because the recovered value is
// re-reported as a violation by [CheckGraphIOSurface] and fails the run with the
// panic text attached.
func graphIOImportGuarded(
	imp func(io.Reader, int64) (bool, error),
	src []byte,
	capBytes int64,
	obs *GraphIOMutation,
) (equal bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			obs.Panicked = fmt.Sprintf("%v", rec)
			equal, err = false, nil
		}
	}()
	return imp(bytes.NewReader(src), capBytes)
}

// graphIOKnownSentinels is every exported error value the graph/io importers and
// exporters raise, as verified in source. It is the set the sweep matches
// against when reporting whether a rejection was TYPED rather than a bare parse
// error.
var graphIOKnownSentinels = []error{
	csv.ErrInputTooLarge, csv.ErrTooManyFields,
	jsonl.ErrInputTooLarge, jsonl.ErrLineTooLong, jsonl.ErrListTooDeep,
	jsonl.ErrUnknownType, jsonl.ErrPropertyNestingTooDeep, jsonl.ErrPropertyValueTooLarge,
	graphml.ErrInputTooLarge, graphml.ErrTooManyKeys, graphml.ErrTooManyData,
	graphml.ErrInvalidXMLChar, graphml.ErrPropertyNestingTooDeep, graphml.ErrPropertyValueTooLarge,
}

// graphIOIsKnownSentinel reports whether err wraps one of the graph/io sentinels.
func graphIOIsKnownSentinel(err error) bool {
	for _, s := range graphIOKnownSentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// The surface verdict, and the SEPARATE shape-only non-vacuity gate
// -----------------------------------------------------------------------------

// graphIOMutationAllocRatio bounds the heap the mutation sweep may allocate, as
// a multiple of the bytes it feeds the importers. The readers are streaming and
// capped, so the ratio is small and roughly constant; a reader that buffered a
// hostile document whole, or followed a corrupted length prefix, would exceed
// it long before it exhausted memory. It is a CEILING chosen with headroom over
// the measured value, not a tight fit, so ordinary allocator variation cannot
// make it flap.
//
// Measured at graphIOTestSeed (0x2480C0DE), process quiet, with the window
// enclosing the replay loop alone: 46.7-46.8x without -race and 47.6x with
// it, against this ceiling of 64. Across the seeds the serialised arm runs,
// the widest measures 47.9-48.0x and the narrowest 18.2x, so the ceiling has
// to clear a real spread rather than a single figure.
const graphIOMutationAllocRatio = 64

// checkGraphIOMutationAlloc is the verdict over the mutation sweep's bounded
// allocation. It is SEPARATE from [CheckGraphIOSurface] because the figure it
// reads is measured with a process-global counter: it is evidence only in a
// serialised arm, and billing a scenario for its neighbours' allocations made
// the swarm fail 116 of 120 seeds at -workers=3 that pass at -workers=1
// (rmp #2553).
//
// It refuses an unmeasured result rather than reading a zero as a pass, so the
// clause cannot be satisfied by the path that does not measure.
func checkGraphIOMutationAlloc(r *GraphIOSurfaceResult) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<io-mutation-alloc>", Message: msg})
	}
	if !r.AllocMeasured {
		add("the result carries no allocation measurement, so the bound cannot be adjudicated over it")
		return v
	}
	if r.MutationInputBytes <= 0 {
		add("the sweep consumed no input, so the allocation ratio is undefined")
		return v
	}
	if r.MutationAllocBytes == 0 {
		add("the measured window reported zero bytes allocated — the instrument is not live")
		return v
	}
	if bound := uint64(graphIOMutationAllocRatio) * uint64(r.MutationInputBytes); r.MutationAllocBytes > bound {
		add(fmt.Sprintf("the mutation sweep allocated %d bytes over %d bytes of input (ratio %.1f), above the bound of %d×",
			r.MutationAllocBytes, r.MutationInputBytes,
			float64(r.MutationAllocBytes)/float64(r.MutationInputBytes), graphIOMutationAllocRatio))
	}
	return v
}

// CheckGraphIOSurface is the unconditional verdict over one
// [RunGraphIOSurface] result. It runs whatever the run produced and never
// consults the non-vacuity gate.
//
// one flat adjudication per arm; splitting it would hide the list
func CheckGraphIOSurface(r *GraphIOSurfaceResult) []Violation {
	var v []Violation
	add := func(op, msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: op, Message: msg})
	}

	// --- cross-format agreement on the edge multiset. ---
	if !r.DOT.Directed {
		add("<io-dot>", "the DOT export of a directed model declared an undirected graph")
	}
	if !triplesEqual(r.ModelTriples, r.DOT.Triples) {
		add("<io-dot>", fmt.Sprintf("DOT describes %d distinct edges, the model %d — the formats disagree",
			len(r.DOT.Triples), len(r.ModelTriples)))
	}
	if !triplesEqual(r.ModelTriples, r.CSVTriples) {
		add("<io-csv>", "the CSV round-trip did not reproduce the modelled edge multiset")
	}
	if !triplesEqual(r.ModelTriples, r.JSONLTriples) {
		add("<io-jsonl>", "the JSONL round-trip did not reproduce the modelled edge multiset")
	}
	if !triplesEqual(r.DOT.Triples, r.JSONLTriples) {
		add("<io-crossformat>", "DOT and JSONL describe different edge multisets for the same model")
	}
	if !triplesEqual(r.DOT.Triples, r.CSVTriples) {
		add("<io-crossformat>", "DOT and CSV describe different edge multisets for the same model")
	}

	// --- cross-format agreement on the vertex set, including the one
	// legitimate disagreement: an edge-list CSV cannot carry an isolated
	// vertex, while DOT and JSONL both can. Asserting the SHAPE of that loss
	// is what stops it degenerating into "the formats differ, never mind". ---
	if !stringsEqual(r.DOT.Nodes, r.ModelNodes) {
		add("<io-dot>", fmt.Sprintf("DOT carries %d vertices, the model %d — an isolated vertex was lost",
			len(r.DOT.Nodes), len(r.ModelNodes)))
	}
	if !stringsEqual(r.JSONLNodes, r.ModelNodes) {
		add("<io-jsonl>", fmt.Sprintf("JSONL carries %d vertices, the model %d",
			len(r.JSONLNodes), len(r.ModelNodes)))
	}
	if !stringsEqual(withName(r.CSVNodes, graphIOIsolatedName), r.ModelNodes) {
		add("<io-csv>", "the CSV vertex set is not exactly the model minus the isolated vertex")
	}
	if containsName(r.CSVNodes, graphIOIsolatedName) {
		add("<io-csv>", "an edge-list CSV re-imported the isolated vertex, which it cannot encode")
	}

	// --- the JSONL property-graph path. ---
	if !r.Props.Equal {
		add("<io-jsonl-props>", "the JSONL property-graph round-trip did not reproduce the model: "+r.Props.Mismatch)
	}

	// --- the csv.Options matrix. ---
	for _, arm := range r.CSVArms {
		if arm.ImportErr != "" {
			add("<io-csv-options>", fmt.Sprintf("%s: import failed: %s", arm.Name, arm.ImportErr))
			continue
		}
		if arm.RoundTrips != arm.ExpectRoundTrip {
			add("<io-csv-options>", fmt.Sprintf("%s: round-trip = %t, declared %t",
				arm.Name, arm.RoundTrips, arm.ExpectRoundTrip))
		}
	}

	// --- the mutated-export sweep: never a panic, never an inert mutation. ---
	for _, m := range r.Mutations {
		if m.Panicked != "" {
			v = append(v, Violation{
				Kind: ViolationACIDConsistency, Op: "<io-mutation>",
				Message: fmt.Sprintf("%s/%s at offset %d of %d PANICKED: %s",
					m.Format, m.Kind, m.Offset, m.Source, m.Panicked),
			})
		}
		if !m.Changed {
			add("<io-mutation>", fmt.Sprintf("%s/%s at offset %d of %d left the bytes unchanged — the mutation proves nothing",
				m.Format, m.Kind, m.Offset, m.Source))
		}
	}
	// --- byte-reproducibility. Every encoder but the one measured unstable must
	// produce identical bytes for identical input; the canonical form of the
	// unstable one must be stable, because the mutation sweep's seed-derived
	// offsets are drawn against it. ---
	for name, differed := range r.ExportStability {
		if name == graphIOUnstableEncoder {
			continue
		}
		if differed != 0 {
			add("<io-export-stability>", fmt.Sprintf(
				"%s produced different bytes for the same graph in %d of %d repeat exports — the export is not reproducible",
				name, differed, graphIOStabilityRuns-1))
		}
	}

	// The bounded-allocation clause is deliberately absent here: it cannot be
	// measured on a path the swarm schedules concurrently, because the counter
	// it reads is process-global. It lives in [checkGraphIOMutationAlloc], which
	// a serialised arm runs (rmp #2553).
	return v
}

// CheckGraphIOSurfaceShape is the SEPARATE non-vacuity gate. It asks only
// whether the run could ever have failed — whether each format arm actually
// ran, whether the model drove the branches the verdict claims to adjudicate,
// and whether the mutations were semantically effective. It never decides
// correctness, so a violation here means the run proved too little, not that
// the module is wrong.
//
// one flat gate per arm; splitting it would hide the list
func CheckGraphIOSurfaceShape(r *GraphIOSurfaceResult) []Violation {
	var v []Violation
	add := func(op, msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: op, Message: msg})
	}

	// The DOT arm adjudicates quoting, weight labels and bare node statements;
	// a model that drove none of them would pass the verdict vacuously.
	if r.DOTBytes == 0 {
		add("<io-dot-shape>", "the DOT arm produced no bytes — it did not run")
	}
	if len(r.DOT.Triples) == 0 {
		add("<io-dot-shape>", "the DOT export carried no edge")
	}
	if r.DOT.QuotedIDs == 0 {
		add("<io-dot-shape>", "no identifier in the DOT export was quoted — the quoting branch never ran")
	}
	if r.DOT.Labelled == 0 {
		add("<io-dot-shape>", "no DOT edge carried a weight label — the weight branch never ran")
	}
	if r.DOT.BareNodes == 0 {
		add("<io-dot-shape>", "the DOT export carried no bare node statement — no isolated vertex was modelled")
	}
	if len(r.ModelNodes) == len(r.CSVNodes) {
		add("<io-crossformat-shape>", "CSV carries as many vertices as the model — the format disagreement the arm asserts cannot arise")
	}
	if r.CSVBytes == 0 || r.JSONLBytes == 0 {
		add("<io-crossformat-shape>", "a cross-format arm produced no bytes — it did not run")
	}

	// The property arm adjudicates every property KIND; a wire carrying only
	// some of them leaves the rest unexercised.
	if r.Props.PropertyRecords == 0 {
		add("<io-jsonl-props-shape>", "the JSONL property export emitted no property record")
	}
	if r.Props.LabelRecords == 0 {
		add("<io-jsonl-props-shape>", "the JSONL property export emitted no labelled node record")
	}
	for _, want := range []string{"string", "int64", "float64", "bool", "time", "bytes", "list"} {
		if !containsName(r.Props.KindsOnWire, want) {
			add("<io-jsonl-props-shape>", "property kind "+want+" never appeared on the JSONL wire")
		}
	}

	// The csv.Options matrix must actually leave DefaultOptions behind, and must
	// contain the one arm whose declared outcome is a FAILED round-trip —
	// without it every assertion in the arm is "it worked", which a no-op
	// option would also satisfy.
	var sawNonDefaultDelim, sawHeader, sawNegative, sawLiteral bool
	for _, arm := range r.CSVArms {
		if arm.Delimiter != ',' {
			sawNonDefaultDelim = true
		}
		if arm.HasHeader {
			sawHeader = true
		}
		if !arm.ExpectRoundTrip {
			sawNegative = true
		}
		if arm.Literal != "" {
			sawLiteral = true
		}
	}
	if !sawNonDefaultDelim {
		add("<io-csv-options-shape>", "every csv arm used the default delimiter")
	}
	if !sawHeader {
		add("<io-csv-options-shape>", "no csv arm set HasHeader")
	}
	if !sawNegative {
		add("<io-csv-options-shape>", "no csv arm declares a FAILED round-trip — the matrix asserts only success")
	}
	if !sawLiteral {
		add("<io-csv-options-shape>", "no csv arm imported a hand-built document — only exporter output was read")
	}

	// The mutation sweep must have been semantically effective per format, and
	// must have provoked at least one rejection overall.
	byFormat := make(map[string]int)
	rejections := 0
	for i := range r.Mutations {
		m := &r.Mutations[i]
		if m.Effective() {
			byFormat[m.Format]++
		}
		if m.Err != "" {
			rejections++
		}
	}
	if len(r.Mutations) == 0 {
		add("<io-mutation-shape>", "the mutation sweep ran no mutation")
	}
	for _, f := range []string{"csv", "jsonl", "graphml", "jsonl-props"} {
		if byFormat[f] == 0 {
			add("<io-mutation-shape>", "every mutation of the "+f+" export re-imported as the model — the sweep proved nothing for that format")
		}
	}
	if rejections == 0 {
		add("<io-mutation-shape>", "no mutation was rejected by any importer — the error path never ran")
	}

	// The stability oracle must have run over every encoder, including the DOT
	// writer this file exists to reach.
	for _, name := range []string{
		"dot.Write", "csv.Write", "jsonl.Write", "graphml.Write",
		"graphml.WriteWithProps", graphIOUnstableEncoder, graphIOUnstableEncoder + "/canonical",
	} {
		if _, ok := r.ExportStability[name]; !ok {
			add("<io-export-stability-shape>", name+" was not measured for byte-reproducibility")
		}
	}
	return v
}

// stringsEqual compares two sorted name slices.
func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsName reports whether names holds want.
func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// withName returns names plus one, re-sorted, without mutating the input.
func withName(names []string, extra string) []string {
	out := append(append([]string(nil), names...), extra)
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// The guard battery: every defensive cap, and mid-parse cancellation
// -----------------------------------------------------------------------------

// GraphIOGuardDecl declares one defensive cap in graph/io: the sentinel it
// raises, WHICH SIDE of the codec it lives on, whether the simulator can
// provoke it at all, and the heap ceiling the provocation must respect.
//
// The side matters and was mis-stated in the audit this file closes. Three of
// the caps — ErrPropertyValueTooLarge, ErrPropertyNestingTooDeep and
// ErrInvalidXMLChar — are raised by the ENCODERS, so no mutated export can ever
// reach them; they are provoked by handing the writer a hostile graph instead.
type GraphIOGuardDecl struct {
	// Sentinel is the exported error the probe must match with errors.Is. A
	// non-nil error of the wrong identity is a failure, not a pass.
	Sentinel error
	Name     string
	// Side is "reader" or "writer".
	Side string
	// Unreachable, when non-empty, is why no probe can provoke this cap. A
	// declaration that carries it is not exempt from adjudication: the reason
	// itself is asserted, so a change that made the cap reachable — or that
	// made the stated reason false — fails the run.
	Unreachable string
	// AllocBoundNote states what the bound below actually proves, because it
	// differs by probe class and a single number would otherwise imply more
	// than it establishes.
	AllocBoundNote string
	// AllocBoundBytes is the ceiling on heap allocated while provoking the cap.
	AllocBoundBytes uint64
}

// GraphIOCapObservation is one cap probe's outcome.
type GraphIOCapObservation struct {
	Err        error
	Name       string
	Panicked   string
	AllocBytes uint64
	InputBytes int64
	Matched    bool
	Ran        bool
	// Overran records that the probe's endless input hit its safety ceiling —
	// which means the cap under test did NOT stop the reader.
	Overran bool
}

// GraphIOCancelObservation is one reader's mid-parse cancellation outcome,
// paired with the uncancelled control run over the same bytes.
type GraphIOCancelObservation struct {
	Err          error
	Name         string
	Panicked     string
	Rows         int
	ControlRows  int
	Canceled     bool
	GraphNil     bool
	ControlEqual bool
}

// GraphIOGuardResult is one run of the whole guard battery.
type GraphIOGuardResult struct {
	Caps    []GraphIOCapObservation
	Cancels []GraphIOCancelObservation
	// ListDepthBytes[i] is the encoded size of a list property nested i+1 deep.
	// It is the evidence for the one cap declared unreachable: the wire re-
	// escapes every level, so the series must roughly double, and a guard that
	// only fires at depth 64 therefore needs an input no machine can hold.
	ListDepthBytes []int
	// DeepestRoundTrip is the deepest nesting that still round-trips through the
	// reader, proving the depth guard is not firing early.
	DeepestRoundTrip int
}

// GraphIOGuardDecls returns the declared cap surface of graph/io, as verified in
// source rather than from the audit list. Adding a sentinel to graph/io without
// adding it here is caught by the pinned-name assertion in the tests.
func GraphIOGuardDecls() []GraphIOGuardDecl {
	const (
		endlessNote = "the probe feeds an UNBOUNDED stream; without the cap the reader would not stop, so the bound is decisive"
		craftedNote = "the probe feeds a finite crafted document; the bound shows the guard fires before the per-element amplification it exists to refuse"
		encodeNote  = "the cap is checked after the value is serialised, so the bound is a ceiling on the encoder's blow-up, not a claim of zero copies"
	)
	return []GraphIOGuardDecl{
		{Name: "csv.ErrInputTooLarge", Sentinel: csv.ErrInputTooLarge, Side: "reader", AllocBoundBytes: 128 << 20, AllocBoundNote: endlessNote},
		{Name: "csv.ErrTooManyFields", Sentinel: csv.ErrTooManyFields, Side: "reader", AllocBoundBytes: 128 << 20, AllocBoundNote: craftedNote},
		{Name: "jsonl.ErrInputTooLarge", Sentinel: jsonl.ErrInputTooLarge, Side: "reader", AllocBoundBytes: 128 << 20, AllocBoundNote: endlessNote},
		{Name: "jsonl.ErrLineTooLong", Sentinel: jsonl.ErrLineTooLong, Side: "reader", AllocBoundBytes: 256 << 20, AllocBoundNote: endlessNote},
		{Name: "jsonl.ErrUnknownType", Sentinel: jsonl.ErrUnknownType, Side: "reader", AllocBoundBytes: 8 << 20, AllocBoundNote: craftedNote},
		{
			Name: "jsonl.ErrListTooDeep", Sentinel: jsonl.ErrListTooDeep, Side: "reader",
			AllocBoundBytes: 8 << 20, AllocBoundNote: craftedNote,
			Unreachable: "the list wire format embeds each nesting level as a re-escaped JSON string, so the encoded size roughly DOUBLES per level; " +
				"the guard fires only at depth 64, which needs an input of order 2^64 bytes. Measured: depth 20 is already ~4 MiB. " +
				"The declaration is still adjudicated — the measured growth and the deepest surviving round-trip are asserted, " +
				"so a change to the encoding or to the depth ceiling that made the guard reachable fails this run",
		},
		{Name: "graphml.ErrInputTooLarge", Sentinel: graphml.ErrInputTooLarge, Side: "reader", AllocBoundBytes: 128 << 20, AllocBoundNote: endlessNote},
		{Name: "graphml.ErrTooManyKeys", Sentinel: graphml.ErrTooManyKeys, Side: "reader", AllocBoundBytes: 512 << 20, AllocBoundNote: craftedNote},
		{Name: "graphml.ErrTooManyData", Sentinel: graphml.ErrTooManyData, Side: "reader", AllocBoundBytes: 512 << 20, AllocBoundNote: craftedNote},
		{Name: "jsonl.ErrPropertyValueTooLarge", Sentinel: jsonl.ErrPropertyValueTooLarge, Side: "writer", AllocBoundBytes: 640 << 20, AllocBoundNote: encodeNote},
		{Name: "jsonl.ErrPropertyNestingTooDeep", Sentinel: jsonl.ErrPropertyNestingTooDeep, Side: "writer", AllocBoundBytes: 8 << 20, AllocBoundNote: encodeNote},
		{Name: "graphml.ErrPropertyValueTooLarge", Sentinel: graphml.ErrPropertyValueTooLarge, Side: "writer", AllocBoundBytes: 640 << 20, AllocBoundNote: encodeNote},
		{Name: "graphml.ErrPropertyNestingTooDeep", Sentinel: graphml.ErrPropertyNestingTooDeep, Side: "writer", AllocBoundBytes: 8 << 20, AllocBoundNote: encodeNote},
		{Name: "graphml.ErrInvalidXMLChar", Sentinel: graphml.ErrInvalidXMLChar, Side: "writer", AllocBoundBytes: 8 << 20, AllocBoundNote: encodeNote},
	}
}

// graphIOEndlessReader emits an unbounded stream of records so a byte cap has
// something genuinely unbounded to stop. It refuses to run away for ever:
// beyond Ceiling it reports EOF and latches Overran, which the verdict reads as
// "the cap did not bite" rather than hanging the suite.
type graphIOEndlessReader struct {
	line      func(n int) string
	pending   string
	n         int
	Delivered int64
	Ceiling   int64
	Overran   bool
}

func (e *graphIOEndlessReader) Read(p []byte) (int, error) {
	if e.Delivered >= e.Ceiling {
		e.Overran = true
		return 0, io.EOF
	}
	if e.pending == "" {
		e.pending = e.line(e.n)
		e.n++
	}
	n := copy(p, e.pending)
	e.pending = e.pending[n:]
	e.Delivered += int64(n)
	return n, nil
}

// graphIOCancelReader delivers src and cancels the context once Trigger bytes
// have crossed the boundary, so cancellation lands MID-parse at a byte offset
// derived from the artefact itself rather than from a race with the scheduler.
type graphIOCancelReader struct {
	cancel  context.CancelFunc
	src     []byte
	off     int
	Trigger int
	fired   bool
}

func (c *graphIOCancelReader) Read(p []byte) (int, error) {
	if c.off >= len(c.src) {
		return 0, io.EOF
	}
	n := copy(p, c.src[c.off:])
	c.off += n
	if !c.fired && c.off >= c.Trigger {
		c.fired = true
		c.cancel()
	}
	return n, nil
}

// graphIOOffsetPastMarker returns the byte offset just past the k-th occurrence
// of marker in src, or -1 when there are fewer than k.
func graphIOOffsetPastMarker(src []byte, marker string, k int) int {
	off := 0
	for i := 0; i < k; i++ {
		j := bytes.Index(src[off:], []byte(marker))
		if j < 0 {
			return -1
		}
		off += j + len(marker)
	}
	return off
}

// graphIOCapProbe is one crafted provocation of a single defensive cap.
type graphIOCapProbe struct {
	// run performs the provocation and reports the bytes fed to it (zero for a
	// writer probe, which is driven by an in-memory value), whether an endless
	// input hit its safety ceiling, and the error raised.
	run  func(ctx context.Context) (in int64, overran bool, err error)
	name string
}

// graphIOCapCeiling bounds every endless probe. It is far above any cap under
// test, so reaching it means the cap failed rather than that the probe was
// impatient.
const graphIOCapCeiling int64 = 64 << 20

// graphIOEndlessCap is the byte ceiling handed to the capped readers in the
// ErrInputTooLarge probes: small enough that the graph built before the cap
// bites stays cheap, large enough that the reader must have folded in thousands
// of real records first. Most of the heap each of these probes allocates is the
// GRAPH accumulated up to the cap, not the reader, so this value — rather than
// any reader behaviour — is what sets the measured allocation.
const graphIOEndlessCap int64 = 256 << 10

// graphIOCapProbes builds every provocation. Each is crafted, not seeded: no
// random mutation of an export will ever produce 65537 <key> declarations, so
// these caps are unreachable from the seeded sweep by construction and are
// driven deterministically instead.
func graphIOCapProbes() []graphIOCapProbe {
	cfg := adjlist.Config{Directed: true}
	hugeList := func() lpg.PropertyValue {
		// One element, larger than the encoders' 64 MiB serialised-value cap.
		return lpg.ListValue([]lpg.PropertyValue{lpg.StringValue(strings.Repeat("x", (64<<20)+16))})
	}
	deepList := func() lpg.PropertyValue {
		// 130 nested EMPTY lists: the depth guard is checked on the way DOWN, so
		// it fires before a single level is serialised. That is what separates
		// this probe from the size-cap probe above — a nested list carrying data
		// trips the SIZE cap at depth ~24 and never reaches the depth ceiling.
		v := lpg.ListValue(nil)
		for i := 0; i < 130; i++ {
			v = lpg.ListValue([]lpg.PropertyValue{v})
		}
		return v
	}
	oneProp := func(pv lpg.PropertyValue) *lpg.Graph[string, int64] {
		g := lpg.New[string, int64](cfg)
		_ = g.AddNode("a")
		_ = g.SetNodeProperty("a", "p", pv)
		return g
	}
	return []graphIOCapProbe{
		{name: "csv.ErrInputTooLarge", run: func(ctx context.Context) (int64, bool, error) {
			src := &graphIOEndlessReader{Ceiling: graphIOCapCeiling, line: func(n int) string {
				return fmt.Sprintf("n%d,n%d,%d\n", n, n+1, n)
			}}
			o := csv.DefaultOptions()
			o.Directed, o.MaxBytes = true, graphIOEndlessCap
			_, _, err := csv.ReadIntoCtx(ctx, src, o)
			return src.Delivered, src.Overran, err
		}},
		{name: "csv.ErrTooManyFields", run: func(ctx context.Context) (int64, bool, error) {
			// 70000 > the 65536-field per-record ceiling.
			doc := "a,b," + strings.Repeat(",", 70000) + "\n"
			o := csv.DefaultOptions()
			o.Directed = true
			_, _, err := csv.ReadIntoCtx(ctx, strings.NewReader(doc), o)
			return int64(len(doc)), false, err
		}},
		{name: "jsonl.ErrInputTooLarge", run: func(ctx context.Context) (int64, bool, error) {
			src := &graphIOEndlessReader{Ceiling: graphIOCapCeiling, line: func(n int) string {
				return fmt.Sprintf("{\"type\":\"node\",\"id\":\"n%d\"}\n", n)
			}}
			_, _, err := jsonl.ReadIntoCappedCtx(ctx, src, cfg, graphIOEndlessCap)
			return src.Delivered, src.Overran, err
		}},
		{name: "jsonl.ErrLineTooLong", run: func(ctx context.Context) (int64, bool, error) {
			// No newline, ever, and the aggregate cap DISABLED: the only thing
			// that can stop this is the 16 MiB per-line scanner ceiling.
			src := &graphIOEndlessReader{Ceiling: graphIOCapCeiling, line: func(int) string {
				return strings.Repeat("a", 64<<10)
			}}
			_, _, err := jsonl.ReadIntoCappedCtx(ctx, src, cfg, 0)
			return src.Delivered, src.Overran, err
		}},
		{name: "jsonl.ErrUnknownType", run: func(ctx context.Context) (int64, bool, error) {
			doc := "{\"type\":\"node\",\"id\":\"a\"}\n{\"type\":\"widget\",\"id\":\"a\"}\n"
			_, _, err := jsonl.ReadIntoCappedCtx(ctx, strings.NewReader(doc), cfg, graphIOEndlessCap)
			return int64(len(doc)), false, err
		}},
		{name: "graphml.ErrInputTooLarge", run: func(ctx context.Context) (int64, bool, error) {
			src := &graphIOEndlessReader{Ceiling: graphIOCapCeiling, line: func(n int) string {
				if n == 0 {
					return `<graphml><graph edgedefault="directed">`
				}
				return fmt.Sprintf(`<edge source="n%d" target="n%d"/>`, n, n+1)
			}}
			_, _, err := graphml.ReadIntoCappedCtx(ctx, src, graphIOEndlessCap)
			return src.Delivered, src.Overran, err
		}},
		{name: "graphml.ErrTooManyKeys", run: func(ctx context.Context) (int64, bool, error) {
			var b strings.Builder
			b.WriteString(`<graphml>`)
			for i := 0; i <= 1<<16; i++ {
				fmt.Fprintf(&b, `<key id="k%d" for="node" attr.name="p%d" attr.type="string"/>`, i, i)
			}
			b.WriteString(`<graph edgedefault="directed"><node id="a"/></graph></graphml>`)
			doc := b.String()
			_, _, err := graphml.ReadIntoCappedCtx(ctx, strings.NewReader(doc), 0)
			return int64(len(doc)), false, err
		}},
		{name: "graphml.ErrTooManyData", run: func(ctx context.Context) (int64, bool, error) {
			var b strings.Builder
			b.WriteString(`<graphml><key id="d0" for="node" attr.name="w" attr.type="int"/>` +
				`<graph edgedefault="directed"><node id="a">`)
			for i := 0; i <= 1<<16; i++ {
				b.WriteString(`<data key="d0">1</data>`)
			}
			b.WriteString(`</node></graph></graphml>`)
			doc := b.String()
			_, _, err := graphml.ReadIntoCappedCtx(ctx, strings.NewReader(doc), 0)
			return int64(len(doc)), false, err
		}},
		{name: "jsonl.ErrPropertyValueTooLarge", run: func(ctx context.Context) (int64, bool, error) {
			_, err := jsonl.WriteWithPropsCtx(ctx, io.Discard, oneProp(hugeList()))
			return 0, false, err
		}},
		{name: "jsonl.ErrPropertyNestingTooDeep", run: func(ctx context.Context) (int64, bool, error) {
			_, err := jsonl.WriteWithPropsCtx(ctx, io.Discard, oneProp(deepList()))
			return 0, false, err
		}},
		{name: "graphml.ErrPropertyValueTooLarge", run: func(ctx context.Context) (int64, bool, error) {
			return 0, false, graphml.WriteWithPropsCtx(ctx, io.Discard, oneProp(hugeList()))
		}},
		{name: "graphml.ErrPropertyNestingTooDeep", run: func(ctx context.Context) (int64, bool, error) {
			return 0, false, graphml.WriteWithPropsCtx(ctx, io.Discard, oneProp(deepList()))
		}},
		{name: "graphml.ErrInvalidXMLChar", run: func(ctx context.Context) (int64, bool, error) {
			// U+0000 is not representable in XML 1.0 at all — not even escaped.
			return 0, false, graphml.WriteWithPropsCtx(ctx, io.Discard, oneProp(lpg.StringValue("bad\x00char")))
		}},
	}
}

// graphIOListDepthCensus writes a list property nested 1..maxDepth levels deep
// and reads each one back, returning the encoded size at every level and the
// deepest level that still survived the round-trip.
//
// The two together are the evidence behind the one cap declared unreachable:
// the size series shows the wire still re-escapes (so the input needed to reach
// the depth-64 guard is astronomical), and the deepest surviving round-trip
// shows the guard is not firing early. One pass produces both, because writing
// these documents twice would double the most expensive part of the battery to
// learn nothing extra.
func graphIOListDepthCensus(maxDepth int) (sizes []int, deepest int, err error) {
	cfg := adjlist.Config{Directed: true}
	sizes = make([]int, 0, maxDepth)
	v := lpg.StringValue("leaf")
	for d := 1; d <= maxDepth; d++ {
		v = lpg.ListValue([]lpg.PropertyValue{v})
		g := lpg.New[string, int64](cfg)
		_ = g.AddNode("a")
		_ = g.SetNodeProperty("a", "p", v)
		var buf bytes.Buffer
		if _, werr := jsonl.WriteWithProps(&buf, g); werr != nil {
			return sizes, deepest, fmt.Errorf("write depth %d: %w", d, werr)
		}
		sizes = append(sizes, buf.Len())
		got, _, rerr := jsonl.ReadWithProps(bytes.NewReader(buf.Bytes()), cfg)
		switch {
		case errors.Is(rerr, jsonl.ErrListTooDeep):
			return sizes, deepest, nil
		case rerr != nil:
			return sizes, deepest, fmt.Errorf("read depth %d: %w", d, rerr)
		case got == nil:
			return sizes, deepest, fmt.Errorf("read depth %d returned a nil graph", d)
		}
		deepest = d
	}
	return sizes, deepest, nil
}

// graphIOCancelEdges is the chain length the cancellation arms import. Every
// *Ctx reader in graph/io checks ctx.Err() once every 4096 units — rows for the
// JSON Lines and CSV readers, EDGES for the GraphML readers — so a shorter
// document would run to completion and the arm would silently prove nothing.
// The chain is long enough that the check at unit 8192 is still well inside it.
const graphIOCancelEdges = 12000

// graphIOCancelTriggerUnit is the unit whose byte offset fires the cancellation.
// It sits between the check at 4096 and the one at 8192, so the cancellation is
// observed at 8192 with thousands of units already folded in — MID-parse, not
// before it.
const graphIOCancelTriggerUnit = 5000

// graphIOCancelModels builds the chain graph in both representations.
func graphIOCancelModels() (*adjlist.AdjList[string, int64], *lpg.Graph[string, int64], error) {
	cfg := adjlist.Config{Directed: true, Multigraph: false}
	a := adjlist.New[string, int64](cfg)
	g := lpg.New[string, int64](cfg)
	for i := 0; i < graphIOCancelEdges; i++ {
		src, dst := "c"+strconv.Itoa(i), "c"+strconv.Itoa(i+1)
		if err := a.AddEdge(src, dst, int64(i)); err != nil {
			return nil, nil, fmt.Errorf("AddEdge %d: %w", i, err)
		}
		if err := g.AddEdge(src, dst, int64(i)); err != nil {
			return nil, nil, fmt.Errorf("lpg AddEdge %d: %w", i, err)
		}
	}
	// One labelled, propertied vertex so the property readers decode their own
	// record shapes rather than only the edge records the plain readers see.
	_ = g.SetNodeLabel("c0", "Chain")
	_ = g.SetNodeProperty("c0", "k", lpg.StringValue("v"))
	return a, g, nil
}

// graphIOCancelArm is one *Ctx reader driven to cancellation, paired with the
// uncancelled control over the same bytes.
type graphIOCancelArm struct {
	read   func(ctx context.Context, r io.Reader) (int, map[string]int, error)
	name   string
	marker string
	src    []byte
}

// graphIOCancelArms builds every arm. The marker is the unit the reader's own
// ctx tick counts, so the trigger offset is derived from what the reader
// actually paces itself by.
func graphIOCancelArms(ctx context.Context) ([]graphIOCancelArm, map[string]int, error) {
	adj, g, err := graphIOCancelModels()
	if err != nil {
		return nil, nil, err
	}
	want := edgeTriples(adj)
	cfg := adjlist.Config{Directed: true, Multigraph: false}

	csvOpts := csv.DefaultOptions()
	csvOpts.Directed = true
	var csvBuf, jsonlBuf, jsonlPropBuf, graphmlBuf, graphmlPropBuf bytes.Buffer
	if _, werr := csv.WriteCtx(ctx, &csvBuf, adj, csvOpts); werr != nil {
		return nil, nil, fmt.Errorf("csv export: %w", werr)
	}
	if _, werr := jsonl.WriteCtx(ctx, &jsonlBuf, adj); werr != nil {
		return nil, nil, fmt.Errorf("jsonl export: %w", werr)
	}
	if _, werr := jsonl.WriteWithPropsCtx(ctx, &jsonlPropBuf, g); werr != nil {
		return nil, nil, fmt.Errorf("jsonl props export: %w", werr)
	}
	if werr := graphml.WriteCtx(ctx, &graphmlBuf, adj); werr != nil {
		return nil, nil, fmt.Errorf("graphml export: %w", werr)
	}
	if werr := graphml.WriteWithPropsCtx(ctx, &graphmlPropBuf, g); werr != nil {
		return nil, nil, fmt.Errorf("graphml props export: %w", werr)
	}

	return []graphIOCancelArm{
		{
			name: "csv.ReadIntoCtx", src: csvBuf.Bytes(), marker: "\n",
			read: func(c context.Context, r io.Reader) (int, map[string]int, error) {
				got, rows, rerr := csv.ReadIntoCtx(c, r, csvOpts)
				return rows, adjTriplesOrNil(got), rerr
			},
		},
		{
			name: "jsonl.ReadIntoCtx", src: jsonlBuf.Bytes(), marker: "\n",
			read: func(c context.Context, r io.Reader) (int, map[string]int, error) {
				got, rows, rerr := jsonl.ReadIntoCtx(c, r, cfg)
				return rows, adjTriplesOrNil(got), rerr
			},
		},
		{
			name: "jsonl.ReadWithPropsCtx", src: jsonlPropBuf.Bytes(), marker: "\n",
			read: func(c context.Context, r io.Reader) (int, map[string]int, error) {
				got, rows, rerr := jsonl.ReadWithPropsCtx(c, r, cfg)
				if got == nil {
					return rows, nil, rerr
				}
				return rows, edgeTriples(got.AdjList()), rerr
			},
		},
		{
			name: "graphml.ReadIntoCtx", src: graphmlBuf.Bytes(), marker: "<edge",
			read: func(c context.Context, r io.Reader) (int, map[string]int, error) {
				got, rows, rerr := graphml.ReadIntoCtx(c, r)
				return rows, adjTriplesOrNil(got), rerr
			},
		},
		{
			name: "graphml.ReadWithPropsCtx", src: graphmlPropBuf.Bytes(), marker: "<edge",
			read: func(c context.Context, r io.Reader) (int, map[string]int, error) {
				got, rows, rerr := graphml.ReadWithPropsCtx(c, r)
				if got == nil {
					return rows, nil, rerr
				}
				return rows, edgeTriples(got.AdjList()), rerr
			},
		},
	}, want, nil
}

// adjTriplesOrNil is edgeTriples, preserving the nil-graph distinction the
// cancellation contract turns on.
func adjTriplesOrNil(a *adjlist.AdjList[string, int64]) map[string]int {
	if a == nil {
		return nil
	}
	return edgeTriples(a)
}

// RunGraphIOGuards drives the crafted half of the surface: every defensive cap
// in graph/io, the reachability evidence for the one that cannot be provoked,
// and every *Ctx reader cancelled mid-parse against an uncancelled control.
//
// CONCURRENCY CONTRACT — it measures heap through a process-global counter
// (see measureProcessAlloc), so it must be driven from a serialised arm and
// never from a scenario the swarm can schedule alongside others. It is called
// from its own test only.
func RunGraphIOGuards(ctx context.Context) (GraphIOGuardResult, error) {
	var res GraphIOGuardResult

	for _, p := range graphIOCapProbes() {
		obs := GraphIOCapObservation{Name: p.name, Ran: true}
		in, overran, alloc, panicked, err := graphIOMeasure(func() (int64, bool, error) {
			return p.run(ctx)
		})
		obs.Err, obs.InputBytes, obs.Overran, obs.AllocBytes, obs.Panicked = err, in, overran, alloc, panicked
		for _, d := range GraphIOGuardDecls() {
			if d.Name == p.name {
				obs.Matched = errors.Is(err, d.Sentinel)
				break
			}
		}
		res.Caps = append(res.Caps, obs)
	}

	// Evidence for the unreachable cap. 18 levels already weighs ~2 MiB and
	// establishes the doubling beyond doubt; each further level doubles the cost
	// to learn nothing new.
	sizes, deepest, err := graphIOListDepthCensus(18)
	if err != nil {
		return res, fmt.Errorf("sim: graph-io list-depth census: %w", err)
	}
	res.ListDepthBytes, res.DeepestRoundTrip = sizes, deepest

	arms, want, err := graphIOCancelArms(ctx)
	if err != nil {
		return res, fmt.Errorf("sim: graph-io cancel arms: %w", err)
	}
	for i := range arms {
		obs, aerr := graphIOCancelProbe(&arms[i], want)
		if aerr != nil {
			return res, fmt.Errorf("sim: graph-io cancel %s: %w", arms[i].name, aerr)
		}
		res.Cancels = append(res.Cancels, obs)
	}
	return res, nil
}

// graphIOCancelProbe runs one arm twice over the SAME bytes: once cancelled
// mid-parse, once not. The control is what stops "the reader returned nil"
// being satisfiable by a reader that always returns nil.
func graphIOCancelProbe(arm *graphIOCancelArm, want map[string]int) (GraphIOCancelObservation, error) {
	obs := GraphIOCancelObservation{Name: arm.name}
	trigger := graphIOOffsetPastMarker(arm.src, arm.marker, graphIOCancelTriggerUnit)
	if trigger < 0 {
		return obs, fmt.Errorf("the export carries fewer than %d %q markers", graphIOCancelTriggerUnit, arm.marker)
	}

	// The control first, so a broken export fails here rather than being
	// mistaken for a cancellation.
	ctrlRows, ctrlTriples, ctrlErr := arm.read(context.Background(), bytes.NewReader(arm.src))
	if ctrlErr != nil {
		return obs, fmt.Errorf("uncancelled control failed: %w", ctrlErr)
	}
	obs.ControlRows = ctrlRows
	obs.ControlEqual = triplesEqual(want, ctrlTriples)

	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &graphIOCancelReader{src: arm.src, Trigger: trigger, cancel: cancel}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				obs.Panicked = fmt.Sprintf("%v", rec)
			}
		}()
		rows, triples, rerr := arm.read(cctx, src)
		obs.Rows, obs.Err = rows, rerr
		obs.GraphNil = triples == nil
		obs.Canceled = errors.Is(rerr, context.Canceled)
	}()
	return obs, nil
}

// graphIOMeasure runs one probe under a heap measurement, converting a panic
// into a recorded string rather than letting it escape. The recovered value is
// re-reported as a violation, so the panic still fails the run — with its text
// attached to the probe that caused it.
//
// The window is process-global (see measureProcessAlloc), so [RunGraphIOGuards]
// is a serialised arm. The runtime.GC() this function used to run before the
// window was removed: it is process-wide and disruptive to anything else
// running, and it cannot affect the figure at all, because TotalAlloc counts
// cumulative allocation and is unchanged by collection (rmp #2553).
func graphIOMeasure(fn func() (int64, bool, error)) (in int64, overran bool, alloc uint64, panicked string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = fmt.Sprintf("%v", rec)
		}
	}()
	alloc = measureProcessAlloc(func() { in, overran, err = fn() })
	return in, overran, alloc, panicked, err
}

// graphIOListGuardDepth is the nesting depth at which graph/io/jsonl raises
// [jsonl.ErrListTooDeep]. It is not importable (the constant is unexported), so
// it is pinned here and the extrapolation below asserts what an input reaching
// it would have to weigh.
const graphIOListGuardDepth = 64

// graphIOMinListGrowth is the per-level size growth the list wire format must
// still exhibit for the unreachability claim to hold. Measured at ~2.0; a value
// below this would mean the re-escaping was removed and the guard had become
// reachable, which must fail rather than pass silently.
const graphIOMinListGrowth = 1.9

// CheckGraphIOGuards is the unconditional verdict over one [RunGraphIOGuards]
// result: every cap declared reachable was reached with its own sentinel and
// within its heap bound, the one declared unreachable still is, and every *Ctx
// reader stopped mid-parse with a typed cancellation and no partial graph.
//
// one flat adjudication per declaration and per arm
func CheckGraphIOGuards(r *GraphIOGuardResult) []Violation {
	var v []Violation
	add := func(op, msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: op, Message: msg})
	}
	byName := make(map[string]GraphIOCapObservation, len(r.Caps))
	for _, c := range r.Caps {
		byName[c.Name] = c
	}

	for _, d := range GraphIOGuardDecls() {
		if d.Unreachable != "" {
			if obs, ok := byName[d.Name]; ok && obs.Matched {
				add("<io-cap>", d.Name+" is declared unreachable but a probe provoked it — the declaration is stale")
			}
			continue
		}
		obs, ok := byName[d.Name]
		if !ok || !obs.Ran {
			add("<io-cap>", d.Name+" was declared reachable but no probe ran for it")
			continue
		}
		if obs.Panicked != "" {
			v = append(v, Violation{
				Kind: ViolationACIDConsistency, Op: "<io-cap>",
				Message: d.Name + " PANICKED instead of returning a typed error: " + obs.Panicked,
			})
			continue
		}
		if !obs.Matched {
			add("<io-cap>", fmt.Sprintf("%s was not provoked: the probe returned %v, want an error wrapping the declared sentinel", d.Name, obs.Err))
		}
		if obs.Overran {
			add("<io-cap>", fmt.Sprintf("%s: the probe's endless input reached its %d-byte safety ceiling — the cap did not stop the reader",
				d.Name, graphIOCapCeiling))
		}
		if obs.AllocBytes > d.AllocBoundBytes {
			add("<io-cap>", fmt.Sprintf("%s allocated %d bytes, above the declared bound of %d (%s)",
				d.Name, obs.AllocBytes, d.AllocBoundBytes, d.AllocBoundNote))
		}
	}

	// The unreachability of jsonl.ErrListTooDeep is asserted, not assumed: the
	// measured per-level growth must still hold, the extrapolated input at the
	// guard's depth must still be absurd, and the guard must not be firing at
	// any depth the encoder can actually produce.
	if n := len(r.ListDepthBytes); n < 12 {
		add("<io-cap-listdepth>", fmt.Sprintf("only %d nesting levels were measured; the unreachability claim rests on the growth series", n))
	} else {
		ratio := float64(r.ListDepthBytes[n-1]) / float64(r.ListDepthBytes[n-2])
		if ratio < graphIOMinListGrowth {
			add("<io-cap-listdepth>", fmt.Sprintf(
				"the nested-list wire grows %.2fx per level, below the %.2fx the unreachability of jsonl.ErrListTooDeep depends on — the guard may now be reachable",
				ratio, graphIOMinListGrowth))
		}
		log2AtGuard := math.Log2(float64(r.ListDepthBytes[n-1])) + float64(graphIOListGuardDepth-n)*math.Log2(ratio)
		if log2AtGuard < 62 {
			add("<io-cap-listdepth>", fmt.Sprintf(
				"an input reaching the depth-%d guard would weigh 2^%.1f bytes, no longer beyond reach — jsonl.ErrListTooDeep must now be probed directly",
				graphIOListGuardDepth, log2AtGuard))
		}
	}
	if r.DeepestRoundTrip < 12 {
		add("<io-cap-listdepth>", fmt.Sprintf(
			"the deepest list that round-tripped was %d levels; the depth guard appears to be firing far below its declared ceiling",
			r.DeepestRoundTrip))
	}

	// Mid-parse cancellation.
	for _, c := range r.Cancels {
		if c.Panicked != "" {
			v = append(v, Violation{
				Kind: ViolationACIDConsistency, Op: "<io-cancel>",
				Message: c.Name + " PANICKED on cancellation: " + c.Panicked,
			})
			continue
		}
		if !c.ControlEqual {
			add("<io-cancel>", c.Name+": the UNCANCELLED control did not reproduce the model — a nil result under cancellation would prove nothing")
		}
		if !c.Canceled {
			add("<io-cancel>", fmt.Sprintf("%s returned %v on a cancelled context, want an error wrapping context.Canceled", c.Name, c.Err))
		}
		if !c.GraphNil {
			add("<io-cancel>", c.Name+": a partial graph escaped a cancelled read — the import is not all-or-nothing")
		}
		if c.Rows == 0 {
			add("<io-cancel>", c.Name+": cancellation was observed with zero units consumed — the read stopped BEFORE parsing, so nothing mid-parse was tested")
		}
		if c.Rows >= c.ControlRows {
			add("<io-cancel>", fmt.Sprintf("%s consumed %d units under cancellation and %d without it — the cancellation did not shorten the read",
				c.Name, c.Rows, c.ControlRows))
		}
	}
	return v
}

// CheckGraphIOGuardDeclShape is the SEPARATE shape-only non-vacuity gate for the
// cap declarations. It reads no run: it asks only whether the declarations could
// ever have failed. A declaration missing its sentinel, or carrying neither a
// heap bound nor a reason for being unreachable, makes the verdict above
// meaningless however the run went.
func CheckGraphIOGuardDeclShape(decls []GraphIOGuardDecl) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: "<io-cap-shape>", Message: msg})
	}
	if len(decls) == 0 {
		add("the cap declaration list is empty")
		return v
	}
	seen := make(map[string]struct{}, len(decls))
	readers, writers, unreachable := 0, 0, 0
	for _, d := range decls {
		if _, dup := seen[d.Name]; dup {
			add(d.Name + " is declared twice")
		}
		seen[d.Name] = struct{}{}
		if d.Sentinel == nil {
			add(d.Name + " declares no sentinel, so no probe could ever match it")
		}
		switch d.Side {
		case "reader":
			readers++
		case "writer":
			writers++
		default:
			add(d.Name + " declares side " + strconv.Quote(d.Side) + ", want \"reader\" or \"writer\"")
		}
		if d.Unreachable != "" {
			unreachable++
			continue
		}
		if d.AllocBoundBytes == 0 {
			add(d.Name + " declares no heap bound, so its bounded-allocation assertion cannot fail")
		}
		if d.AllocBoundNote == "" {
			add(d.Name + " declares a heap bound with no statement of what the bound proves")
		}
	}
	// The split between reader-side and writer-side caps is the correction this
	// file records; a declaration set that lost it would quietly restore the
	// wrong premise that every cap is reachable from a mutated export.
	if readers == 0 {
		add("no cap is declared reader-side")
	}
	if writers == 0 {
		add("no cap is declared writer-side — the encoders' caps are not represented")
	}
	if unreachable == len(decls) {
		add("every cap is declared unreachable — the battery provokes nothing")
	}
	return v
}

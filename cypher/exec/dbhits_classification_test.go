package exec

// dbhits_classification_test.go — the drift gate on the db-hits classification
// (rmp #2760).
//
// Every operator's db-hits cell is one of four things, and which one is decided
// entirely by the marker interfaces it implements:
//
//	storageAccessCounter  MEASURED — the operator counted its own accesses
//	StorageRecordScan     DERIVED  — one record read per row emitted
//	noStorageAccess       ZERO     — it opens no access path at all
//	(none of the three)   UNKNOWN  — nobody counted it; the cell renders "?"
//
// The classification is the whole of rmp #2760's design decision, so it must not
// be possible to change it — or to add an operator that silently inherits the
// UNKNOWN default — without somebody saying so in writing. This test derives the
// operator set from the package SOURCE, computes each operator's class from the
// methods it actually has (own or promoted through embedding), and compares that
// against the census below.
//
// It is deliberately a hand-maintained expectation, unlike its sibling
// TestPlanChildren_EveryOperatorWithInputsImplementsIt, which derives its
// obligation entirely. The difference is that PlanChildren has a mechanically
// checkable trigger — "this struct holds an input" — whereas "this operator
// provably reads no storage" is a JUDGEMENT about what the operator's closures
// can reach, and a judgement has to be written down to be auditable. The list
// below IS the audit, and the test is what stops the code and the audit drifting.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// dbHitsClass names one of the four cells an operator can report.
type dbHitsClass string

const (
	classMeasured dbHitsClass = "MEASURED (storageAccessCounter)"
	classDerived  dbHitsClass = "DERIVED (StorageRecordScan)"
	classZero     dbHitsClass = "ZERO (noStorageAccess)"
	classUnknown  dbHitsClass = "UNKNOWN (no marker)"
)

// dbHitsCensus is the classification of every operator in this package, with the
// reason each entry is what it is.
//
// The reasons for the UNKNOWN entries matter most, because UNKNOWN is the
// default and a default with no reason attached is indistinguishable from an
// oversight. Two distinct causes appear:
//
//   - READS STORAGE, COUNTS NOTHING — the operator opens an access path and has
//     no counter to report. Tasks #2762 (parallel leaves) and #2763 (shortest
//     path) exist to convert some of these to MEASURED; #2761 already did it for
//     Expand, OptionalExpand and columnarExpand, which is why the DERIVED group
//     now holds only access-path leaves.
//   - HOLDS AN EXPRESSION CLOSURE — the operator evaluates a caller-supplied
//     expression, and a GoGraph expression can WALK THE GRAPH: cypher's evalRow
//     bridge passes expr.PatternEvaluator, whose EvalPattern /
//     EvalPatternComp traverse adjacency. Measured on a 100-way fan,
//     `WHERE (r)-[:LIKES]->()` reports one db-hit for the whole plan while the
//     equivalent Expand reports 100, and `RETURN size([(r)-[:LIKES]->(x) | 1])`
//     is evaluated inside Project and reports the same 1. So these operators
//     cannot claim a zero, even though most instances of them read nothing.
//
// A future refinement could decide the second group PER INSTANCE — a Sort whose
// every SortKey.Eval is nil reads nothing, and the planner knows that when it
// builds the operator. That is a larger change than rmp #2760's state model and
// is deliberately not made here.
var dbHitsCensus = map[string]struct {
	class  dbHitsClass
	reason string
}{
	// ── MEASURED ───────────────────────────────────────────────────────────────
	"VarLengthExpand": {classMeasured, "reports totalEdgesVisited, a counter its traversal budget already maintains"},
	"Expand":          {classMeasured, "reports the adjacency slots its cursors consumed, walked or rejected; recovered from the cursor positions in O(1) per input row, never per slot (rmp #2761)"},
	"OptionalExpand":  {classMeasured, "forwards the inner Expand's slot count; the inner operator is private to it and is never a node of the rendered plan (rmp #2761)"},
	"columnarExpand":  {classMeasured, "embeds *Expand and inherits its counter"},

	// ── DERIVED ────────────────────────────────────────────────────────────────
	"AllNodesScan":         {classDerived, "one node reference per emitted row"},
	"NodeByLabelScan":      {classDerived, "one node reference per emitted row"},
	"NodeByIndexSeek":      {classDerived, "one posting per emitted row"},
	"NodeByIndexSeekSet":   {classDerived, "one posting per emitted row"},
	"NodeByIndexRangeScan": {classDerived, "one posting per emitted row"},

	// ── ZERO ───────────────────────────────────────────────────────────────────
	"Argument":               {classZero, "re-emits the outer row its Apply driver set"},
	"SingleRow":              {classZero, "emits one empty row"},
	"singleRow":              {classZero, "emits one caller-supplied row"},
	"StaticRows":             {classZero, "emits rows built before execution"},
	"Limit":                  {classZero, "forwards rows up to a build-time count"},
	"ColumnarLimit":          {classZero, "embeds Limit and inherits its marker"},
	"Skip":                   {classZero, "forwards rows after a build-time count"},
	"Eager":                  {classZero, "buffers and re-emits its child's rows"},
	"Distinct":               {classZero, "hashes values already bound in the row"},
	"CountRows":              {classZero, "counts its child's rows"},
	"UnionAll":               {classZero, "concatenates two inputs"},
	"Union":                  {classZero, "a Distinct over a UnionAll"},
	"EagerAggregation":       {classZero, "groups on column INDICES and feeds aggregates from columns; evaluates no expression"},
	"GlobalAggregateAdapter": {classZero, "forwards rows, or emits aggregator neutral values"},
	"Apply":                  {classZero, "drives an inner plan that is measured in its own right"},
	"CorrelatedApply":        {classZero, "drives an inner plan that is measured in its own right"},
	"OptionalApply":          {classZero, "drives an inner plan that is measured in its own right"},
	"SemiApply":              {classZero, "drives an inner plan that is measured in its own right"},
	"AntiSemiApply":          {classZero, "drives an inner plan that is measured in its own right"},
	"Foreach":                {classZero, "drives an inner plan that is measured in its own right"},

	// ── UNKNOWN: reads storage, counts nothing ─────────────────────────────────
	"ShortestPath":          {classUnknown, "bidirectional BFS reads relationship records; totalEdgesTraversed covers only the exhaustive search (rmp #2763)"},
	"AllShortestPaths":      {classUnknown, "as ShortestPath (rmp #2763)"},
	"ExpandIntersect":       {classUnknown, "walks two CSR ranges and intersects them"},
	"IndexNestedLoopJoin":   {classUnknown, "seeks the index once per outer row"},
	"ParallelScanProject":   {classUnknown, "workers scan the label; the tier is one node by construction (rmp #2762)"},
	"ParallelAggregateScan": {classUnknown, "as ParallelScanProject (rmp #2762)"},
	"ParallelCountScan":     {classUnknown, "as ParallelScanProject (rmp #2762)"},
	"LabelCountScan":        {classUnknown, "answers from a maintained counter when it can, and otherwise MATERIALISES the label bitmap; it counts neither path"},
	"AllNodesCountScan":     {classUnknown, "answers from a maintained counter when it can, and otherwise WALKS every node id; it counts neither path"},

	// ── UNKNOWN: holds a caller-supplied expression closure ────────────────────
	"Filter":           {classUnknown, "predFn may be a pattern predicate, which walks adjacency"},
	"ColumnarFilter":   {classUnknown, "embeds Filter and inherits its predicate"},
	"Project":          {classUnknown, "a projection item may be a pattern comprehension, evaluated in place"},
	"ColumnarProject":  {classUnknown, "embeds *Project and inherits its items"},
	"Sort":             {classUnknown, "SortKey.Eval is an ORDER BY expression compiled through evalRowPooled"},
	"Top":              {classUnknown, "as Sort"},
	"Unwind":           {classUnknown, "listFn is a list expression compiled through evalRow"},
	"HashJoin":         {classUnknown, "buildFn/probeFn are join-key expressions"},
	"ColumnarHashJoin": {classUnknown, "as HashJoin"},
	"RollUpApply":      {classUnknown, "listEval projects each inner row through an expression"},
	"ProcedureCallOp":  {classUnknown, "argExprs are expressions, and a procedure may itself read the graph"},

	// ── UNKNOWN: write operators ───────────────────────────────────────────────
	//
	// None appears in a PROFILE today — both Engine.Profile and the PROFILE
	// statement prefix refuse a writing statement — but the classification is
	// stated rather than left blank, so it is right if that ever changes. Each
	// reads storage (a lookup, an adjacency walk, an index probe) and counts none
	// of it.
	"CreateNode":         {classUnknown, "writes, and probes indexes and constraints"},
	"CreateRelationship": {classUnknown, "writes, and reads the endpoints"},
	"DeleteNode":         {classUnknown, "reads the node it deletes"},
	"DeleteRelationship": {classUnknown, "reads the relationship it deletes"},
	"DetachDelete":       {classUnknown, "walks the node's adjacency to detach it"},
	"SetProperty":        {classUnknown, "reads the entity and maintains indexes"},
	"SetAllProperties":   {classUnknown, "reads the entity and maintains indexes"},
	"SetLabels":          {classUnknown, "reads the node and maintains label sets"},
	"RemoveLabels":       {classUnknown, "reads the node and maintains label sets"},
	"RemoveProperty":     {classUnknown, "reads the entity and maintains indexes"},
	"Merge":              {classUnknown, "searches for a match before creating"},
	"MergePattern":       {classUnknown, "searches for a matching pattern before creating"},
	"MergeRelationship":  {classUnknown, "searches for a matching relationship before creating"},
}

// TestDbHitsClassification_EveryOperatorIsClassified fails when the operator set
// and the census above disagree — a new operator, a renamed one, or a marker
// added or removed.
func TestDbHitsClassification_EveryOperatorIsClassified(t *testing.T) {
	t.Parallel()

	operators, methods := parseOperatorMethodSets(t)

	classOf := func(name string) dbHitsClass {
		switch {
		case methods[name]["storageAccesses"]:
			return classMeasured
		case methods[name]["storageRecordPerRow"]:
			return classDerived
		case methods[name]["readsNoStorage"]:
			return classZero
		default:
			return classUnknown
		}
	}

	var problems []string
	for _, name := range operators {
		want, listed := dbHitsCensus[name]
		got := classOf(name)
		switch {
		case !listed:
			problems = append(problems, name+": NOT IN THE CENSUS (source says "+string(got)+")")
		case want.class != got:
			problems = append(problems, name+": census says "+string(want.class)+
				", source says "+string(got))
		}
	}
	present := map[string]bool{}
	for _, n := range operators {
		present[n] = true
	}
	for name := range dbHitsCensus {
		if !present[name] {
			problems = append(problems, name+": in the census but is no longer an operator in this package")
		}
	}
	sort.Strings(problems)

	if len(problems) > 0 {
		t.Fatalf("the db-hits classification and the source disagree in %d place(s):\n  %s\n\n"+
			"Every operator's db-hits cell is decided by which of storageAccessCounter, "+
			"StorageRecordScan and noStorageAccess it implements, and the default is "+
			"UNKNOWN — a cell that renders %q. Adding an operator without deciding is "+
			"how a plan comes to report a figure nobody stands behind, which is what "+
			"rmp #2760 removed. Decide, add the marker if the operator earns one, and "+
			"record the decision and its REASON in dbHitsCensus "+
			"(cypher/exec/dbhits_classification_test.go).",
			len(problems), strings.Join(problems, "\n  "), DbHitsUnknown)
	}
}

// TestDbHitsClassification_CensusReasonsArePresent keeps the census from
// degrading into a bare list. The reason is the audit; without it the entry
// records only that somebody typed a name.
func TestDbHitsClassification_CensusReasonsArePresent(t *testing.T) {
	t.Parallel()
	for name, e := range dbHitsCensus {
		if strings.TrimSpace(e.reason) == "" {
			t.Errorf("%s is classified %s with no reason recorded", name, e.class)
		}
	}
}

// parseOperatorMethodSets parses this package's non-test sources and returns the
// sorted names of every operator struct, plus each struct's method set including
// methods promoted from embedded structs.
//
// An operator is a struct declaring Next(out *Row) (bool, error) or
// FillChunk(dst *Chunk, maxRows int) (int, error), own or promoted. The exact
// signature is required rather than the bare method name, because this package
// also holds ResultSet.Next() bool, which is the driver's cursor and not a plan
// operator. The profiling wrappers are excluded by name: they are the
// instrumentation, never a node in a rendered plan.
func parseOperatorMethodSets(t *testing.T) ([]string, map[string]map[string]bool) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	structs := map[string]*ast.StructType{}
	own := map[string]map[string]bool{}
	// sig records, per receiver and method name, the rendered parameter and result
	// list, so an operator is recognised by its Next SIGNATURE and not by the name
	// alone.
	sig := map[string]map[string]string{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, isType := spec.(*ast.TypeSpec)
					if !isType {
						continue
					}
					if st, isStruct := ts.Type.(*ast.StructType); isStruct {
						structs[ts.Name.Name] = st
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					continue
				}
				recv := baseTypeName(d.Recv.List[0].Type)
				if recv == "" {
					continue
				}
				if own[recv] == nil {
					own[recv] = map[string]bool{}
					sig[recv] = map[string]string{}
				}
				own[recv][d.Name.Name] = true
				sig[recv][d.Name.Name] = renderFuncType(d.Type)
			}
		}
	}
	if len(structs) == 0 {
		t.Fatal("no struct declarations found in the exec package")
	}

	effective := resolveEmbeddedMethods(structs, own)
	// Promote signatures the same way the method names were promoted, so an
	// embedding operator is recognised by the signature it inherits.
	effSig := promoteSignatures(structs, sig)

	const (
		nextSig  = "(out *Row) (bool, error)"
		chunkSig = "(dst *Chunk, maxRows int) (int, error)"
	)
	var operators []string
	for name := range structs {
		if strings.HasPrefix(name, "profiled") {
			continue // the instrumentation wrappers, never plan nodes
		}
		if !effective[name]["Next"] && !effective[name]["FillChunk"] {
			continue
		}
		if effSig[name]["Next"] != nextSig && effSig[name]["FillChunk"] != chunkSig {
			continue
		}
		operators = append(operators, name)
	}
	sort.Strings(operators)
	if len(operators) < 40 {
		t.Fatalf("found only %d operator structs; the detection heuristic has broken "+
			"and this gate would pass vacuously", len(operators))
	}
	return operators, effective
}

// promoteSignatures resolves each struct's method SIGNATURES through embedding,
// to a fixed point, mirroring resolveEmbeddedMethods.
func promoteSignatures(structs map[string]*ast.StructType, own map[string]map[string]string) map[string]map[string]string {
	embeds := map[string][]string{}
	for name, st := range structs {
		for _, f := range st.Fields.List {
			if len(f.Names) != 0 {
				continue
			}
			if base := baseTypeName(f.Type); base != "" && structs[base] != nil {
				embeds[name] = append(embeds[name], base)
			}
		}
	}
	out := map[string]map[string]string{}
	for name := range structs {
		out[name] = map[string]string{}
		for m, s := range own[name] {
			out[name][m] = s
		}
	}
	for changed := true; changed; {
		changed = false
		for name, parents := range embeds {
			for _, p := range parents {
				for m, s := range out[p] {
					if _, have := out[name][m]; !have {
						out[name][m] = s
						changed = true
					}
				}
			}
		}
	}
	return out
}

// renderFuncType renders a function's parameter and result lists in the canonical
// spelling this file compares against. It handles only the shapes the operator
// signatures use; anything else renders to something that simply will not match.
func renderFuncType(ft *ast.FuncType) string {
	var b strings.Builder
	b.WriteString("(")
	writeFields(&b, ft.Params, true)
	b.WriteString(")")
	if ft.Results != nil && len(ft.Results.List) > 0 {
		b.WriteString(" (")
		writeFields(&b, ft.Results, false)
		b.WriteString(")")
	}
	return b.String()
}

// writeFields renders one field list; withNames keeps the parameter names, which
// the signatures above include.
func writeFields(b *strings.Builder, fl *ast.FieldList, withNames bool) {
	if fl == nil {
		return
	}
	first := true
	for _, f := range fl.List {
		typ := renderExpr(f.Type)
		names := []string{""}
		if withNames && len(f.Names) > 0 {
			names = nil
			for _, n := range f.Names {
				names = append(names, n.Name)
			}
		}
		for _, n := range names {
			if !first {
				b.WriteString(", ")
			}
			first = false
			if n != "" {
				b.WriteString(n)
				b.WriteString(" ")
			}
			b.WriteString(typ)
		}
	}
}

// renderExpr renders the small set of type expressions the operator signatures
// use: identifiers and pointers to them.
func renderExpr(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + renderExpr(t.X)
	default:
		return "?"
	}
}

package sim

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// EngineVariant names and builds one side of a differential comparison: a fresh
// engine over a fresh directed simple graph, configured by Options. Two variants
// that the engine guarantees are result-equivalent (e.g. the default planner vs
// the same planner with a physical optimisation disabled) MUST produce identical
// observable output on the same trace; any divergence is a regression.
//
// The build is a factory so each differential run gets an isolated engine — the
// two variants never share graph state.
type EngineVariant struct {
	// Name is a short label for the variant, used in divergence reports.
	Name string
	// Options configures the engine. The zero value selects the engine's
	// defaults (NewEngineWithOptions fills them in).
	Options cypher.EngineOptions
}

// buildEngine constructs the variant's engine adapter over a fresh graph. The
// graph shape matches the deterministic scripted executor (directed simple
// graph) so the two variants and a recorded trace are directly comparable.
func (v EngineVariant) buildEngine() *EngineAdapter {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return NewEngineAdapter(cypher.NewEngineWithOptions(g, v.Options))
}

// DefaultVariantPair returns the PRIMARY differential pair: the engine's default
// configuration versus the same engine with the disconnected-equi-join hash-join
// optimisation turned OFF. The engine documents DisableHashJoin as existing
// "for the differential test that proves both plans return an identical result
// multiset" — so the two MUST agree on every observable output. This is a real,
// in-process, equivalent-result toggle, not a contrived comparison.
func DefaultVariantPair() (EngineVariant, EngineVariant) {
	return EngineVariant{Name: "default"},
		EngineVariant{Name: "no-hash-join", Options: cypher.EngineOptions{DisableHashJoin: true}}
}

// RangeSeekVariantPair returns a second PRIMARY pair: the default configuration
// versus the same engine with the range-predicate B+tree index seek turned OFF.
// Like DisableHashJoin, DisableRangeIndexSeek exists for the differential proof
// that both plans return an identical result multiset.
func RangeSeekVariantPair() (EngineVariant, EngineVariant) {
	return EngineVariant{Name: "default"},
		EngineVariant{Name: "no-range-seek", Options: cypher.EngineOptions{DisableRangeIndexSeek: true}}
}

// DiffResult is the outcome of a differential run: whether the two variants
// agreed, and on a divergence the first op index, the op, and the two
// (canonicalised) observable signatures that differed.
type DiffResult struct {
	// Agreed reports whether the two variants produced identical observable
	// output for every op and an identical end-state.
	Agreed bool
	// DivergedAt is the 0-based op index of the first divergence (-1 when the
	// variants agreed).
	DivergedAt int
	// DivergedOp is the op at the first divergence (zero value when agreed).
	DivergedOp Op
	// SignatureA / SignatureB are the diverging observable signatures of variant
	// A and B at DivergedAt (empty when agreed).
	SignatureA string
	SignatureB string
	// VariantA / VariantB are the variant names, for the report.
	VariantA string
	VariantB string
	// Reason is a human-readable description of the divergence (empty when
	// agreed): an end-state mismatch or a per-op result mismatch.
	Reason string
}

// String renders a differential result. On a divergence it names the variants,
// the first diverging op, and the two signatures so the regression is actionable.
func (r DiffResult) String() string {
	if r.Agreed {
		return fmt.Sprintf("differential: variants %q and %q AGREED on all ops + end-state", r.VariantA, r.VariantB)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DIFFERENTIAL DIVERGENCE between %q and %q\n", r.VariantA, r.VariantB)
	fmt.Fprintf(&b, "  first diverging op: index=%d kind=%s cypher=%q params=%v\n",
		r.DivergedAt, r.DivergedOp.Kind, r.DivergedOp.Cypher, r.DivergedOp.Params)
	fmt.Fprintf(&b, "  reason: %s\n", r.Reason)
	fmt.Fprintf(&b, "  %s => %s\n", r.VariantA, r.SignatureA)
	fmt.Fprintf(&b, "  %s => %s", r.VariantB, r.SignatureB)
	return b.String()
}

// diffEngine pairs a variant's engine with the oracle-applying machinery so a
// trace can be replayed against it while capturing each op's observable
// signature. It mirrors the scripted executor but exposes a per-op result
// signature for comparison.
type diffEngine struct {
	name   string
	engine *EngineAdapter
}

// DifferentialTrace replays the SAME recorded [Trace] against two engine
// variants and compares their observable outputs op-by-op, reporting the FIRST
// divergence. The observable output of an op is its canonicalised result-row
// multiset (for reads) plus the running engine node/edge counts (for writes),
// and the comparison also asserts the two variants reach an identical end-state.
//
// Because the engine guarantees the default and toggled plans are
// result-equivalent (DisableHashJoin / DisableRangeIndexSeek exist precisely for
// this proof), a clean trace must replay to identical output on both.
//
// It spawns no goroutines and is a pure function of trace + the two variants.
func DifferentialTrace(ctx context.Context, trace Trace, a, b EngineVariant) (DiffResult, error) {
	return differentialTrace(ctx, trace, a, b, -1)
}

// DifferentialTraceInjectB replays the trace against both variants but injects a
// deterministic lost-write fault into variant B at op index injectAt (a write
// op). It exists for the test that proves the differential CATCHES a behavioural
// divergence: variant B drops one write, so its end-state diverges from variant
// A and the first comparison after the drop fails. injectAt < 0 injects nothing
// (equivalent to [DifferentialTrace]).
func DifferentialTraceInjectB(ctx context.Context, trace Trace, a, b EngineVariant, injectAt int) (DiffResult, error) {
	return differentialTrace(ctx, trace, a, b, injectAt)
}

// differentialTrace is the shared core: replay against both variants, optionally
// dropping the write at injectBAt on variant B only.
func differentialTrace(ctx context.Context, trace Trace, a, b EngineVariant, injectBAt int) (DiffResult, error) {
	ea := diffEngine{name: a.Name, engine: a.buildEngine()}
	eb := diffEngine{name: b.Name, engine: b.buildEngine()}

	for i, top := range trace.Ops {
		if err := ctx.Err(); err != nil {
			return DiffResult{}, err
		}
		topB := top
		if i == injectBAt {
			topB.Fault = FaultDropEngineWrite
		}
		sigA, err := ea.stepSignature(ctx, top)
		if err != nil {
			return DiffResult{}, fmt.Errorf("sim: differential: variant %q op %d: %w", a.Name, i, err)
		}
		sigB, err := eb.stepSignature(ctx, topB)
		if err != nil {
			return DiffResult{}, fmt.Errorf("sim: differential: variant %q op %d: %w", b.Name, i, err)
		}
		if sigA != sigB {
			return DiffResult{
				Agreed:     false,
				DivergedAt: i,
				DivergedOp: top.Op,
				SignatureA: sigA,
				SignatureB: sigB,
				VariantA:   a.Name,
				VariantB:   b.Name,
				Reason:     "per-op observable result signature differs",
			}, nil
		}
	}

	// End-state cross-check: both variants must hold identical node/edge counts.
	naN, _ := ea.engine.NodeCount()
	naE, _ := ea.engine.EdgeCount()
	nbN, _ := eb.engine.NodeCount()
	nbE, _ := eb.engine.EdgeCount()
	if naN != nbN || naE != nbE {
		return DiffResult{
			Agreed:     false,
			DivergedAt: len(trace.Ops),
			VariantA:   a.Name,
			VariantB:   b.Name,
			SignatureA: fmt.Sprintf("nodes=%d edges=%d", naN, naE),
			SignatureB: fmt.Sprintf("nodes=%d edges=%d", nbN, nbE),
			Reason:     "end-state node/edge counts differ",
		}, nil
	}

	return DiffResult{Agreed: true, DivergedAt: -1, VariantA: a.Name, VariantB: b.Name}, nil
}

// stepSignature runs one traced op against the variant's engine and returns a
// canonical, order-independent signature of its observable output: the sorted
// multiset of result rows plus the post-op engine node/edge counts. An injected
// [FaultDropEngineWrite] suppresses the engine write on THIS variant only, so a
// caller can force a deterministic behavioural divergence to prove the
// differential catches it.
func (d *diffEngine) stepSignature(ctx context.Context, top TracedOp) (string, error) {
	op := top.Op

	if top.Fault == FaultDropEngineWrite && op.Kind.IsWrite() {
		// Suppress the write on this variant: its end-state will diverge from the
		// unfaulted variant, which the comparison catches.
		n, _ := d.engine.NodeCount()
		e, _ := d.engine.EdgeCount()
		return fmt.Sprintf("rows=[DROPPED] nodes=%d edges=%d", n, e), nil
	}

	var (
		res Result
		err error
	)
	if op.Kind.IsWrite() {
		res, err = d.engine.RunWrite(ctx, op.Cypher, op.Params)
	} else {
		res, err = d.engine.Run(ctx, op.Cypher, op.Params)
	}
	if err != nil {
		// An engine error is part of the observable behaviour; encode it so two
		// variants that differ on error-vs-success are caught. The error TEXT is
		// not compared (it may carry plan-specific detail); the fact of an error
		// at this op is.
		n, _ := d.engine.NodeCount()
		e, _ := d.engine.EdgeCount()
		return fmt.Sprintf("error nodes=%d edges=%d", n, e), nil
	}

	rows := canonicalRows(res, plannerNondeterministicRows(op.Cypher))
	_ = res.Close()

	n, _ := d.engine.NodeCount()
	e, _ := d.engine.EdgeCount()
	return fmt.Sprintf("rows=%s nodes=%d edges=%d", rows, n, e), nil
}

// plannerNondeterministicRows reports whether a query's ROW SET is legitimately
// plan-dependent and therefore must NOT be compared row-for-row across two
// variants. A LIMIT without an ORDER BY returns an unspecified subset under
// openCypher: two equivalent plans may each return a different valid subset, so
// comparing the exact rows would be a false positive. For such queries the
// differential compares only the row COUNT (which the plans must agree on) plus
// the engine end-state, not the row contents. Every other query has a
// deterministic, fully-compared multiset.
func plannerNondeterministicRows(query string) bool {
	up := strings.ToUpper(query)
	return strings.Contains(up, "LIMIT") && !strings.Contains(up, "ORDER BY")
}

// canonicalRows drains a result and returns an order-independent canonical
// signature of its rows. When countOnly is false the full row multiset is
// rendered (each row to a stable string, then sorted, so two plans that emit the
// same multiset in different orders compare equal). When countOnly is true (an
// unordered-LIMIT query whose row SELECTION is legitimately plan-dependent) only
// the row count is signed, so a spec-legal subset difference is not a false
// divergence while the count the plans must still agree on is compared.
func canonicalRows(res Result, countOnly bool) string {
	ra, ok := res.(*resultAdapter)
	if !ok {
		// The differential always drives the engine adapter; a foreign Result is a
		// programmer error, encoded loudly rather than silently mis-compared.
		return "<non-adapter-result>"
	}
	var rows []string
	n := 0
	for ra.Next() {
		n++
		if !countOnly {
			rows = append(rows, renderRow(ra.res))
		}
	}
	errMark := ""
	if err := ra.Err(); err != nil {
		errMark = "<err>"
	}
	if countOnly {
		return fmt.Sprintf("count=%d%s", n, errMark)
	}
	sort.Strings(rows)
	return "[" + strings.Join(rows, "|") + "]" + errMark
}

// renderRow renders all columns of the current result row to a stable string.
// It reads through the engine's column accessor so every projected value
// contributes to the signature.
func renderRow(res *cypher.Result) string {
	cols := res.Columns()
	parts := make([]string, 0, len(cols))
	for i := range cols {
		parts = append(parts, fmt.Sprintf("%v", res.ValueAt(i)))
	}
	return strings.Join(parts, ",")
}

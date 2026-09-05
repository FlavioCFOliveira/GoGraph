package cypher

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Recognising a key SET and serving it with one seek (task #2183).
//
// correlated_seek_plan.go turns an UNWIND-bound key into a disjunction of
// equalities on one property, pushed into the inner arm of an Apply:
//
//	Selection(a.name = k)                                  <- retained
//	└─ CartesianProduct
//	   ├─ Unwind [k := ['name-7', 'name-9']]
//	   └─ Selection(a.name = 'name-7' OR a.name = 'name-9') <- pushed
//	      └─ NodeByLabelScan [a:P]
//
// This file recognises that pushed disjunction and replaces the scan with a
// single [exec.NodeByIndexSeekSet] — one probe per distinct key, merged into one
// ascending NodeID run.
//
// # The cost gate is mandatory, and why
//
// The #2181 spike measured that the plan being replaced costs Θ(N + rows), not the
// Θ(rows·N) the audit had recorded: the Apply materialises the label scan ONCE and
// joins the keys against it. A 300× increase in key count cost 12 % at N=5 000 and
// nothing measurable at N=20 000.
//
// The consequence is that the gain is N/rows, not N per row: about 3 000× for a
// single key, ~10× for 300 keys, and NOTHING once the key count approaches the
// label population. An ungated rewrite would therefore REGRESS wide-key queries —
// it would pay for one probe per key and arrive at a posting list the size of the
// scan it replaced. The gate is what makes this rewrite safe, not what makes it
// tidy.
//
// The gate reuses the range seek's policy rather than inventing a second one
// (rangeSeekMinLabelPopulation, rangeSeekMaxSelectivity), and the quantity it
// tests is EXACT — the cardinality of the merged posting run, not an estimate — so
// it needs no margin for estimation error. That exactness is the same property
// that made the range seek's gate provably non-regressing.
//
// Two things are gated, both at plan time:
//
//  1. The label population must reach the floor. Below it a scan is a few
//     microseconds and an index descent cannot win.
//  2. The merged posting count must stay within the selectivity ceiling. This does
//     NOT require probing: the distinct keys are on one property, so a node matches
//     at most one of them, which makes the per-key cardinalities DISJOINT and their
//     sum the exact count. So the whole decision is made before a single posting
//     list is built. [exec.NodeByIndexSeekSet] enforces the same budget itself and
//     reports [exec.ErrSeekSetOverBudget], which is defence in depth for a Go-API
//     caller that builds the operator directly, not the planner's mechanism.
//
// # An unclaimed hint must be dropped, not evaluated
//
// The pushed disjunction is a hint. When the gate declines it, leaving the hint in
// place would cost Θ(k·N) predicate evaluations to re-establish what the retained
// Selection already establishes. Measured at N=20 000 with 2 001 keys: 2 952 ms
// with the hint evaluated against 19.4 ms with it dropped. The build drops it (see
// planCacheEntry.pushedSeekHints) and EXPLAIN renders the dropped shape, so the
// declined plan is structurally identical to the plan that existed before this
// rewrite — which is what makes the gate non-regressing in fact and not only in
// principle.
//
// # Result-identity
//
// The retained outer Selection is never removed, so this can only narrow what
// that filter examines — the same argument correlated_seek_plan.go relies on. The
// seek must therefore not UNDER-return, which is what the skip rules in
// [exec.NodeByIndexSeekSet.Init] establish: a NULL key matches nothing under
// openCypher, and a key whose type the index cannot hold matches nothing because
// equality across type groups is FALSE.

// tryBuildIndexSeekSetFromSelection inspects a Selection whose predicate is a
// disjunction of equalities on one property of a scanned node, and returns a
// key-set seek when a covering hash index exists and the cost gate allows it.
//
// ok is false whenever the rewrite does not apply, which the caller treats as
// "build the child scan instead". An over-budget key set is one such case, not an
// error: the scan is the correct and cheaper answer.
func tryBuildIndexSeekSetFromSelection(
	sel *ir.Selection,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.ReadView[string, float64],
) (exec.Operator, bool) {
	if sel.PredicateExpr == nil || idxMgr == nil || g == nil {
		return nil, false
	}
	nodeVar, label, ok := scanLeafNodeVar(sel.Child)
	if !ok || label == "" {
		return nil, false
	}
	budget, ok := seekSetBudget(g, label)
	if !ok {
		return nil, false
	}
	// Size-gate BEFORE extracting the keys. Extraction boxes one expr.Value per
	// disjunct and builds a deduplication map over them, and this runs on EVERY
	// build — that is, once per query execution. Paying O(k) allocation only to
	// reject the set is a measurable regression on the declined path: at N=20 000
	// with 2 001 keys it cost 20.2 ms against 15.7 ms for the same query before this
	// rewrite existed, and 6 021 extra allocations, almost exactly 3 per key.
	//
	// This is §3.3's third condition in docs/design-correlated-seek.md — "decline
	// when the key set exceeds a bound, which is the same condition seen from the
	// input side" — and it makes the plan-time cost of a rejected set O(1).
	//
	// It is a genuine approximation, unlike the posting-count gate: k distinct keys
	// can exceed the budget while matching few nodes, so a set of mostly-absent keys
	// that WOULD have passed the exact gate is declined here. That set is answered
	// by the scan, which is correct and, for a set that large, close to what the
	// seek would have cost anyway.
	n, ok := countOrDisjuncts(sel.PredicateExpr)
	// A single key is the ordinary seek's business; routing it here would add the
	// set operator's merge for no gain.
	if !ok || n < 2 || uint64(n) > budget {
		return nil, false
	}
	propKey, keys, ok := extractKeySetFromAST(sel.PredicateExpr, nodeVar, params)
	if !ok {
		return nil, false
	}
	// The label the subsumed scan leaf carried must still qualify every candidate
	// (rmp #2423); a set seek that cannot verify it declines, exactly as the
	// single-key seek does.
	admit, canVerify := labelAdmitFn(labelSrcFromView(g), label)
	if !canVerify {
		return nil, false
	}
	return buildSeekSetOperator(idxMgr, label, propKey, keys, budget, nodeVar, schema, admit)
}

// countOrDisjuncts counts the operands of a chain of OR without allocating.
//
// ok is false once the count passes maxSeekSetDisjuncts, so a pathologically wide
// disjunction cannot make the counter itself the cost. The bound is far above any
// budget the selectivity ceiling can produce for a graph that fits in memory, so it
// never rejects a set the gate would have accepted.
func countOrDisjuncts(e ast.Expression) (int, bool) {
	binOp, isOr := e.(*ast.BinaryOp)
	if !isOr || (binOp.Operator != "OR" && binOp.Operator != "or") {
		return 1, true
	}
	l, ok := countOrDisjuncts(binOp.Left)
	if !ok {
		return 0, false
	}
	r, ok := countOrDisjuncts(binOp.Right)
	if !ok || l+r > maxSeekSetDisjuncts {
		return 0, false
	}
	return l + r, true
}

// maxSeekSetDisjuncts caps the disjunction width countOrDisjuncts will count.
const maxSeekSetDisjuncts = 1 << 20

// seekSetBudget returns the maximum merged posting count a key-set seek may
// produce for label, and false when the label is too small for any seek to win.
//
// This is the plan-time half of the gate; [exec.NodeByIndexSeekSet] enforces the
// budget itself once the exact count is known.
func seekSetBudget(g *lpg.ReadView[string, float64], label string) (uint64, bool) {
	nLabel := g.NodeIndex().Count(uint32(g.Registry().Intern(label)))
	if nLabel < rangeSeekMinLabelPopulation {
		return 0, false
	}
	budget := uint64(float64(nLabel) * rangeSeekMaxSelectivity)
	if budget == 0 {
		return 0, false
	}
	return budget, true
}

// buildSeekSetOperator finds a hash index covering (label, propKey) and builds the
// key-set seek, declining when the exact merged posting count exceeds budget.
//
// The count is obtained from the index's per-key cardinality, not by probing: the
// distinct keys are on ONE property, so a node matches at most one of them and the
// per-key counts are DISJOINT, which makes their sum the exact cardinality of the
// merged result. That is what lets the whole gate run at plan time, with no
// posting list built for a set that is about to be rejected, and with no estimate
// anywhere in the decision.
func buildSeekSetOperator(
	idxMgr *index.Manager,
	label, propKey string,
	keys []expr.Value,
	budget uint64,
	nodeVar string,
	schema map[string]int,
	admit func(uint64) bool,
) (exec.Operator, bool) {
	for _, name := range idxMgr.ListIndexes() {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" || !indexCoversNode(sub, label, propKey) {
			continue
		}
		sl, isStr := sub.(hashStringLookup)
		if !isStr {
			continue
		}
		card, hasCard := sub.(hashStringCardinality)
		if !hasCard {
			continue
		}
		total, servable := mergedPostingCount(card, keys, budget)
		// Over budget, or no key this index can serve. An empty result is correct
		// but pointless to seek: the scan reaches the same zero rows without an
		// index descent, which is the range seek's rule too.
		if !servable || total == 0 {
			return nil, false
		}
		op := exec.NewNodeByIndexSeekSet(exec.NewStringHashIndex(sl), keys, budget).Admitting(admit)
		schema[nodeVar] = schemaWidth(schema)
		return op, true
	}
	return nil, false
}

// mergedPostingCount sums the exact posting counts of the distinct string keys,
// stopping as soon as the running total exceeds budget.
//
// Keys that this index cannot hold contribute nothing and are skipped, mirroring
// [exec.NodeByIndexSeekSet.Init] — a non-string or NULL key matches nothing on a
// string-keyed index, so it neither adds postings nor invalidates the seek.
// servable is false only when the budget is exceeded.
func mergedPostingCount(card hashStringCardinality, keys []expr.Value, budget uint64) (total uint64, servable bool) {
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k == nil || k.Kind() != expr.KindString {
			continue
		}
		//nolint:forcetypeassert // k reaches here only from the StringValue arm of the kind switch above, which is what selects this seek-set encoding
		s := string(k.(expr.StringValue))
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		total += card.Cardinality(s)
		if total > budget {
			return total, false
		}
	}
	return total, true
}

// extractKeySetFromAST returns the property and the key values of a predicate
// that is a disjunction of equalities on ONE property of nodeVar.
//
// Every disjunct must be such an equality on the SAME property. A disjunction
// mixing properties, or carrying any other predicate, describes a union of
// different access paths and is declined: serving part of it with a seek would
// leave the rest to a scan, which is two access paths for one predicate.
//
// A key that resolves to NULL is retained in the returned set rather than
// dropped, because [exec.NodeByIndexSeekSet] skips it deliberately and the count
// of keys is what distinguishes a set from a single seek.
func extractKeySetFromAST(
	predExpr ast.Expression,
	nodeVar string,
	params map[string]expr.Value,
) (propKey string, keys []expr.Value, ok bool) {
	disjuncts := orDisjuncts(predExpr)
	if len(disjuncts) < 2 {
		return "", nil, false
	}
	keys = make([]expr.Value, 0, len(disjuncts))
	for _, d := range disjuncts {
		pk, v, got := extractEqFromAST(d, nodeVar, params)
		if !got {
			// A null-literal key is not a "literal" to astLiteralToValue, which
			// serves several other callers and is left alone. Recognised here
			// instead: within a key set a NULL contributes no postings, and the
			// seek operator skips it, so the set stays servable.
			pk, got = nullKeyEquality(d, nodeVar)
			if !got {
				return "", nil, false
			}
			v = expr.Null
		}
		if propKey == "" {
			propKey = pk
		} else if pk != propKey {
			return "", nil, false
		}
		keys = append(keys, v)
	}
	return propKey, keys, true
}

// nullKeyEquality matches `nodeVar.prop = null` in either operand order and
// returns the property key.
func nullKeyEquality(e ast.Expression, nodeVar string) (propKey string, ok bool) {
	binOp, isBin := e.(*ast.BinaryOp)
	if !isBin || binOp.Operator != "=" {
		return "", false
	}
	for _, cand := range [2]struct{ propSide, other ast.Expression }{
		{binOp.Left, binOp.Right},
		{binOp.Right, binOp.Left},
	} {
		if _, isNull := cand.other.(*ast.NullLiteral); !isNull {
			continue
		}
		prop, isProp := cand.propSide.(*ast.Property)
		if !isProp {
			continue
		}
		if recv, isVar := prop.Receiver.(*ast.Variable); isVar && recv.Name == nodeVar {
			return prop.Key, true
		}
	}
	return "", false
}

// orDisjuncts flattens a chain of OR into its operands. A predicate that is not
// an OR yields itself, so callers need no special case.
func orDisjuncts(e ast.Expression) []ast.Expression {
	binOp, ok := e.(*ast.BinaryOp)
	if !ok || (binOp.Operator != "OR" && binOp.Operator != "or") {
		return []ast.Expression{e}
	}
	return append(orDisjuncts(binOp.Left), orDisjuncts(binOp.Right)...)
}

// seekClaimsHint reports whether some index access path would claim sel, the
// pushed seek hint.
//
// It is the EXPLAIN-side counterpart of the build's decision, and it asks the same
// two questions in the same order the build asks them, so the rendered tree cannot
// disagree with the built one: the single-key seek first, then the key set.
func seekClaimsHint(
	sel *ir.Selection,
	params map[string]expr.Value,
	idxMgr *index.Manager,
	g *lpg.ReadView[string, float64],
) bool {
	if op, fired, err := tryBuildIndexSeekFromSelection(sel, params, make(map[string]int), idxMgr, labelSrcFromView(g)); err == nil && fired && op != nil {
		return true
	}
	_, fired := tryBuildIndexSeekSetFromSelection(sel, params, make(map[string]int), idxMgr, g)
	return fired
}

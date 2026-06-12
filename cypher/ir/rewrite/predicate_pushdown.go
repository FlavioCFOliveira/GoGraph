package rewrite

import (
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// PredicatePushdown pushes Selection predicates down the plan tree toward the
// scan operators. A Selection can be pushed past any operator that is both
// transparent (does not introduce new variable bindings consumed by the
// predicate) and non-eager (does not act as a pipeline-breaking barrier).
//
// Pushdown rules:
//
//   - Selection above Projection: push when all predicate variables are in the
//     projected output.
//   - Selection above Sort: always push (sort does not change the set of
//     available variables and filter-then-sort produces the same rows as
//     sort-then-filter).
//   - Selection above Limit / Skip: NEVER push (openCypher applies LIMIT/SKIP
//     before the WHERE predicate in a WITH clause; pushing the filter below
//     the limit/skip changes the result cardinality).
//   - Selection above Eager: NEVER push (Eager is a hard barrier).
//   - Selection above another Selection: push the outer predicate past the inner
//     selection so predicates accumulate close to the scan.
//
// Concurrency: PredicatePushdown is stateless and goroutine-safe.
type PredicatePushdown struct{}

// Name implements Rule.
func (PredicatePushdown) Name() string { return "PredicatePushdown" }

// Apply implements Rule. It matches a Selection node and attempts to commute it
// downward past its child.
func (PredicatePushdown) Apply(plan ir.LogicalPlan) (ir.LogicalPlan, bool) {
	sel, ok := plan.(*ir.Selection)
	if !ok {
		return plan, false
	}

	switch child := sel.Child.(type) {
	// ── Eager barrier: never push ────────────────────────────────────────────
	case *ir.Eager:
		return plan, false

	// ── Projection: push only when all predicate vars are in scope ───────────
	case *ir.Projection:
		if !predicateVarsInScope(sel.Predicate, child.Vars()) {
			return plan, false
		}
		// Move Selection below Projection.
		newSel := ir.NewSelection(sel.Predicate, child.Child)
		newProj := ir.NewProjection(child.Items, newSel)
		return newProj, true

	// ── Limit: do NOT push — filter-after-limit ≠ filter-before-limit ───────
	case *ir.Limit:
		return plan, false

	// ── Skip: do NOT push — filter-after-skip ≠ filter-before-skip ──────────
	case *ir.Skip:
		return plan, false

	// ── Sort: always push ────────────────────────────────────────────────────
	case *ir.Sort:
		newSel := ir.NewSelection(sel.Predicate, child.Child)
		newSort := ir.NewSort(child.SortItems, newSel)
		return newSort, true

	// ── Selection: swap so outer predicate sinks below inner ─────────────────
	case *ir.Selection:
		// Turn Selection(pred1, Selection(pred2, X))
		// into Selection(pred2, Selection(pred1, X))
		// This moves pred1 one level deeper. Combined with repeated application
		// it achieves maximum pushdown depth.
		inner := ir.NewSelection(sel.Predicate, child.Child)
		outer := ir.NewSelection(child.Predicate, inner)
		return outer, true

	default:
		return plan, false
	}
}

// predicateVarsInScope reports whether every variable referenced in predicate
// appears in the scope slice. Variable names are extracted heuristically from
// the opaque predicate string by splitting on common delimiters and matching
// identifiers against the scope.
//
// Because the expression IR uses opaque strings at this stage, we adopt a
// conservative approach: extract alpha-numeric tokens and check that the
// dot-prefix variable names (e.g. "n" from "n.name") are in scope. A predicate
// with no extractable variable tokens is considered safe to push.
func predicateVarsInScope(predicate string, scope []string) bool {
	vars := extractVarNames(predicate)
	if len(vars) == 0 {
		return true
	}
	scopeSet := make(map[string]struct{}, len(scope))
	for _, v := range scope {
		scopeSet[v] = struct{}{}
	}
	for _, v := range vars {
		if _, ok := scopeSet[v]; !ok {
			return false
		}
	}
	return true
}

// extractVarNames parses an opaque predicate string and returns the set of
// variable names referenced. It recognises identifiers of the form `name` and
// `name.property`; in both cases `name` is the variable.
func extractVarNames(predicate string) []string {
	// Tokenise: split on everything that is not a letter, digit, dot, or underscore.
	tokens := strings.FieldsFunc(predicate, func(r rune) bool {
		return !isIdentRune(r) && r != '.'
	})

	seen := make(map[string]struct{}, len(tokens))
	var vars []string
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		// Take the part before the first dot as the variable name.
		base := strings.SplitN(tok, ".", 2)[0]
		if base == "" || !isIdentStart(rune(base[0])) {
			continue
		}
		if _, dup := seen[base]; !dup {
			seen[base] = struct{}{}
			vars = append(vars, base)
		}
	}
	return vars
}

func isIdentStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
}

func isIdentRune(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}

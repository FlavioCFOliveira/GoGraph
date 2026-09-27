package expr

// callsite.go — per-statement memo of function call sites (rmp #2891).
//
// The evaluator walks the AST once per row, so without a memo every row of a
// statement re-derives the same facts about one call site: it lower-cases and
// joins the function name, asks the registry chain to resolve it (the
// statement's now-aware wrapper builds a fresh closure for each temporal
// constructor on every Resolve), and — for a call such as date($ref), whose
// arguments cannot change within the statement — recomputes the same value.
// A CPU profile of examples/26_social_scale_bench at 127c012b attributed 0.87 s
// to the resolution chain and 0.72 s to fnDate on date($ref) alone.
//
// # Design
//
// The memo lives on the statement, not on the AST: the AST is shared by every
// execution of a cached plan and by concurrent statements, while a resolved
// function (it may capture the statement's frozen "now") and a folded value (it
// depends on the statement's parameters) belong to exactly one statement. The
// per-statement registry the engine builds for every statement already has that
// lifetime, so it hosts the table through the optional [callSiteHost]
// capability. A registry that does not implement it — every caller of the bare
// [Eval] API, a user registry — keeps the unmemoised path unchanged.
//
// # What is folded
//
// A call's VALUE is memoised only when all three hold:
//
//  1. the host reports that the registry resolves the openCypher built-ins
//     ([callSiteHost.FoldsStatementConstants]) — a user registry may bind the
//     same name to a function of its own, whose determinism is unknown;
//  2. the function is one [isStatementConstantFn] accepts: deterministic in its
//     arguments, or — for the zero-argument temporal forms and timestamp() —
//     frozen at the statement's instant by the engine's per-statement registry
//     (the TCK requires two such calls in one statement to agree:
//     Temporal10.feature [12], duration.inSeconds(date(), date()) = PT0S).
//     rand() and randomUUID() are NOT in the set;
//  3. every argument is a literal, a parameter, or itself such a call (checked
//     on the AST once per site), AND every argument VALUE of the first
//     evaluation is a scalar or temporal value. The second test excludes the
//     map forms (date({timezone: 'Europe/Lisbon'})), which read the current
//     instant through a path other than the statement's frozen one.
//
// Folding is lazy — the value is computed by the first evaluation that
// reaches the call — so a call behind a CASE branch or on a zero-row input is
// still never evaluated, and an error is raised exactly where the unfolded
// evaluation would raise it. Because the call is deterministic, memoising its
// error is equivalent to recomputing it.

import (
	"strings"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// callSiteHost is the optional [FunctionRegistry] capability that hosts the
// per-statement call-site table. Implementations must be safe for concurrent
// use: the parallel scan tier evaluates one statement on several goroutines.
//
// The interface is unexported on purpose: it is an internal contract between
// this package and the engine's per-statement registry, not an extension point.
type callSiteHost interface {
	// LoadCallSite returns the value stored for key by StoreCallSite.
	LoadCallSite(key *ast.FunctionInvocation) (site any, ok bool)
	// StoreCallSite stores site for key unless one is already stored, and
	// returns the stored value. It returns nil, and stores nothing, when the
	// table has reached its bound; the caller then evaluates unmemoised.
	StoreCallSite(key *ast.FunctionInvocation, site any) (stored any)
	// FoldsStatementConstants reports whether the registry resolves the
	// openCypher built-ins, which is the precondition for folding a call's value.
	FoldsStatementConstants() bool
}

// callSite is the memo for one [ast.FunctionInvocation] within one statement.
// Every field but value and noFold is written before the site is published through
// [callSiteHost.StoreCallSite] and read-only afterwards.
type callSite struct {
	name  string    // the lower-cased, namespace-joined name
	fn    BuiltinFn // the resolved function; nil when found is false
	found bool      // whether the registry resolves name
	fold  bool      // the AST and registry preconditions of folding hold
	value atomic.Pointer[foldedCall]
	// noFold is set when an evaluation saw a non-scalar argument value, which
	// disqualifies the site from folding for the rest of the statement.
	noFold atomic.Bool
}

// foldedCall is a memoised call result. It is immutable once stored.
type foldedCall struct {
	v   Value
	err error
}

// isStatementConstantFn reports whether name is a built-in whose value is
// fixed for a statement once its arguments are (condition 2 in the file
// comment). name is lower-cased and namespace-joined, as [callName] produces it.
func isStatementConstantFn(name string) bool {
	switch name {
	case "date", "datetime", "localdatetime", "localtime", "time", "duration",
		"duration.between", "duration.inmonths", "duration.indays", "duration.inseconds",
		"timestamp":
		return true
	}
	return false
}

// callName returns n's lower-cased, namespace-joined function name.
func callName(n *ast.FunctionInvocation) string {
	name := strings.ToLower(n.Name)
	if len(n.Namespace) == 0 {
		return name
	}
	parts := make([]string, 0, len(n.Namespace)+1)
	for _, ns := range n.Namespace {
		parts = append(parts, strings.ToLower(ns))
	}
	parts = append(parts, name)
	return strings.Join(parts, ".")
}

// resolveCallSite returns the memo for n in host, creating and publishing it on
// the first evaluation of n in the statement. It returns nil when the host's
// table is full; the caller then takes the unmemoised path.
func resolveCallSite(host callSiteHost, n *ast.FunctionInvocation, reg FunctionRegistry) *callSite {
	if s, ok := host.LoadCallSite(n); ok {
		site, _ := s.(*callSite)
		return site
	}
	site := &callSite{name: callName(n)}
	site.fn, site.found = reg.Resolve(site.name)
	site.fold = site.found && host.FoldsStatementConstants() && isStatementConstantCall(n)
	stored, _ := host.StoreCallSite(n, site).(*callSite)
	return stored
}

// isStatementConstantCall reports whether n names an [isStatementConstantFn]
// function and every argument is a literal scalar, a parameter, or itself such
// a call — the AST half of condition 3.
func isStatementConstantCall(n *ast.FunctionInvocation) bool {
	if n.Distinct || n.CountStar {
		return false
	}
	if !isStatementConstantFn(callName(n)) {
		return false
	}
	for _, a := range n.Args {
		switch v := a.(type) {
		case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral, *ast.BoolLiteral,
			*ast.NullLiteral, *ast.Parameter:
		case *ast.FunctionInvocation:
			if !isStatementConstantCall(v) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// foldableArgs reports whether every evaluated argument is a scalar or temporal
// value — the value half of condition 3.
func foldableArgs(args []Value) bool {
	for _, a := range args {
		if a == nil {
			return false
		}
		switch a.Kind() {
		case KindList, KindMap, KindNode, KindRelationship, KindPath:
			return false
		}
	}
	return true
}

// foldedValue returns site's memoised result when fold holds and a result has
// been stored, and nil otherwise.
func foldedValue(site *callSite, fold bool) *foldedCall {
	if !fold {
		return nil
	}
	return site.value.Load()
}

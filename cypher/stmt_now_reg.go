package cypher

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// nowAwareRegistry wraps an [expr.FunctionRegistry] and overrides the
// zero-argument forms of the five temporal "now" constructors so that each
// query observes its own frozen statement timestamp rather than the
// process-global statementNow in [funcs].
//
// The five functions affected are date(), time(), localtime(), datetime(), and
// localdatetime(). For zero-argument calls the wrapper returns a value derived
// from r.now directly, bypassing the process-global entirely. Non-zero-argument
// calls are delegated unchanged to the underlying registry.
//
// A nowAwareRegistry is constructed once per Engine.Run / Engine.RunInTx call
// via [newNowAwareRegistry] and is never shared across queries, so r.now is
// effectively immutable after construction.
//
// Because it has exactly one statement's lifetime, it also hosts that
// statement's call-site table (rmp #2891): the evaluator memoises, per
// [ast.FunctionInvocation], the resolved function and — for a statement-constant
// call such as date($ref) — its value (see cypher/expr/callsite.go). The table
// is bounded by [maxStatementCallSites].
//
// # Concurrency
//
// Safe for concurrent use by the goroutines evaluating one statement (the
// parallel scan tier shares it): delegate, now and foldsBuiltins are written
// only by the constructor or [nowAwareRegistry.bind], before the statement's
// plan is built, and the call-site table is a [sync.Map] with an atomic bound.
type nowAwareRegistry struct {
	delegate expr.FunctionRegistry
	now      time.Time
	// foldsBuiltins reports that delegate resolves the openCypher built-ins
	// ([funcs.DefaultRegistry], optionally behind the engine's graph-aware
	// overlay), the precondition for folding a call's value.
	foldsBuiltins bool
	sites         sync.Map // *ast.FunctionInvocation -> evaluator-owned memo
	nSites        atomic.Int32
}

// maxStatementCallSites bounds one statement's call-site table. A statement's
// call sites are the FunctionInvocation nodes of its plan, so the bound is
// reached only by a pathological query; past it the remaining sites are
// evaluated unmemoised, exactly as before the table existed.
const maxStatementCallSites = 1024

// LoadCallSite implements the evaluator's call-site host capability.
func (r *nowAwareRegistry) LoadCallSite(key *ast.FunctionInvocation) (any, bool) {
	return r.sites.Load(key)
}

// StoreCallSite implements the evaluator's call-site host capability: it keeps
// the first site stored for key and returns it, or returns nil once the table
// holds [maxStatementCallSites] entries.
func (r *nowAwareRegistry) StoreCallSite(key *ast.FunctionInvocation, site any) any {
	if r.nSites.Add(1) > maxStatementCallSites {
		r.nSites.Add(-1)
		return nil
	}
	actual, loaded := r.sites.LoadOrStore(key, site)
	if loaded {
		r.nSites.Add(-1)
	}
	return actual
}

// FoldsStatementConstants implements the evaluator's call-site host capability.
func (r *nowAwareRegistry) FoldsStatementConstants() bool { return r.foldsBuiltins }

// resolvesBuiltins reports whether reg is [funcs.DefaultRegistry], directly or
// behind the engine's [graphAwareRegistry] overlay, whose overrides (startNode,
// endNode) are not statement-constant functions.
func resolvesBuiltins(reg expr.FunctionRegistry) bool {
	if ga, ok := reg.(*graphAwareRegistry); ok {
		reg = ga.delegate
	}
	fr, ok := reg.(*funcs.Registry)
	return ok && fr == funcs.DefaultRegistry
}

// newNowAwareRegistry wraps delegate, pinning t as the statement-frozen "now"
// for the five temporal constructors. t should be time.Now() captured at the
// start of the query.
func newNowAwareRegistry(delegate expr.FunctionRegistry, t time.Time) expr.FunctionRegistry {
	return &nowAwareRegistry{delegate: delegate, now: t.UTC(), foldsBuiltins: resolvesBuiltins(delegate)}
}

// bind is [newNowAwareRegistry] for a wrapper that already has storage — the one
// carried inline by each write mutator adapter (rmp #2339), so an autocommit
// write freezes its "now" without a malloc for the two words that hold it.
//
// The returned registry aliases r, so a caller must not bind the same wrapper
// twice while a statement built on it is still running. The write path binds
// once per statement, before the plan is built. bind empties the call-site
// table, so a wrapper re-bound for a later statement never serves the previous
// statement's memo.
func (r *nowAwareRegistry) bind(delegate expr.FunctionRegistry, t time.Time) expr.FunctionRegistry {
	r.delegate, r.now = delegate, t.UTC()
	r.foldsBuiltins = resolvesBuiltins(delegate)
	// Guarded so a first bind — the common case, on a freshly allocated adapter —
	// does not initialise the empty map: Clear would allocate its root node.
	if r.nSites.Load() != 0 {
		r.sites.Clear()
		r.nSites.Store(0)
	}
	return r
}

// Resolve implements [expr.FunctionRegistry]. For the five temporal-now
// constructors the returned function handles the zero-argument case using
// r.now; all other call shapes (and all other function names) are delegated
// to the underlying registry.
func (r *nowAwareRegistry) Resolve(name string) (expr.BuiltinFn, bool) {
	fn, ok := r.delegate.Resolve(name)
	if !ok {
		return nil, false
	}
	switch name {
	case "date":
		now := r.now
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) == 0 {
				return expr.DateFromTime(now), nil
			}
			return fn(args)
		}, true
	case "localdatetime":
		now := r.now
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) == 0 {
				return expr.LocalDateTimeValue{T: now}, nil
			}
			return fn(args)
		}, true
	case "datetime":
		now := r.now
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) == 0 {
				return expr.DateTimeValue{T: now}, nil
			}
			return fn(args)
		}, true
	case "localtime":
		now := r.now
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) == 0 {
				return expr.NewLocalTime(now.Hour(), now.Minute(), now.Second(), now.Nanosecond()), nil
			}
			return fn(args)
		}, true
	case "time":
		now := r.now
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) == 0 {
				return expr.NewTime(now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), 0), nil
			}
			return fn(args)
		}, true
	case "timestamp":
		// timestamp() returns milliseconds since the Unix epoch at the statement's
		// frozen instant, so every call within a query — and across rows — yields
		// the same value. Derive it from r.now (the per-query frozen instant),
		// bypassing the process-global funcs.StatementNow exactly as the five
		// temporal constructors above do. timestamp() takes no arguments.
		now := r.now
		return func(args []expr.Value) (expr.Value, error) {
			if len(args) == 0 {
				return expr.IntegerValue(now.UnixMilli()), nil
			}
			return fn(args)
		}, true
	}
	return fn, true
}

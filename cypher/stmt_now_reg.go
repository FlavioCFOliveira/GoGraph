package cypher

import (
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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
type nowAwareRegistry struct {
	delegate expr.FunctionRegistry
	now      time.Time
}

// newNowAwareRegistry wraps delegate, pinning t as the statement-frozen "now"
// for the five temporal constructors. t should be time.Now() captured at the
// start of the query.
func newNowAwareRegistry(delegate expr.FunctionRegistry, t time.Time) expr.FunctionRegistry {
	return &nowAwareRegistry{delegate: delegate, now: t.UTC()}
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

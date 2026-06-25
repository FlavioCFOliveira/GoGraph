// Package funcs implements the built-in Cypher function registry.
//
// # Registry
//
// [DefaultRegistry] is the pre-populated registry containing all essential
// built-ins. It implements [expr.FunctionRegistry] and is safe for concurrent
// use after construction.
//
// # Implementing built-ins
//
// Each function follows the [expr.BuiltinFn] signature:
//
//	func(args []expr.Value) (expr.Value, error)
//
// Type errors are returned as typed [TypeError] values rather than propagated
// as Go errors, unless the error is truly fatal (impossible with well-formed
// arguments). NULL arguments propagate according to each function's documented
// NULL-handling behaviour.
//
// # Concurrency
//
// The registry is immutable after [NewRegistry] returns; all exported symbols
// are safe for concurrent use.
package funcs

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Error types
// ─────────────────────────────────────────────────────────────────────────────

// TypeError is returned by built-in functions when an argument has the wrong
// type for the operation requested.
type TypeError struct {
	// Function is the name of the function that encountered the type error.
	Function string
	// ArgIndex is the 0-based index of the offending argument.
	ArgIndex int
	// Got is the kind of the offending argument.
	Got expr.Kind
	// Want is a human-readable description of the expected type.
	Want string
}

// Error implements the error interface.
func (e *TypeError) Error() string {
	return fmt.Sprintf("funcs: %s() argument %d: got %s, want %s", e.Function, e.ArgIndex, e.Got, e.Want)
}

// ArityError is returned when a built-in receives an unexpected number of
// arguments.
type ArityError struct {
	// Function is the name of the function.
	Function string
	// Got is the number of arguments received.
	Got int
	// Want is a human-readable description of the expected argument count.
	Want string
}

// Error implements the error interface.
func (e *ArityError) Error() string {
	return fmt.Sprintf("funcs: %s() takes %s argument(s), got %d", e.Function, e.Want, e.Got)
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry
// ─────────────────────────────────────────────────────────────────────────────

// Registry maps lower-cased function names to their implementations.
// It is immutable after [NewRegistry] returns; safe for concurrent reads.
type Registry struct {
	fns map[string]expr.BuiltinFn
}

// NewRegistry creates an empty registry. Use [DefaultRegistry] to obtain
// the pre-populated built-in registry.
func NewRegistry() *Registry {
	return &Registry{fns: make(map[string]expr.BuiltinFn)}
}

// Register adds fn under the given name (stored lower-cased). Panics on
// duplicate; this is a programming error, not a runtime condition.
func (r *Registry) Register(name string, fn expr.BuiltinFn) {
	name = strings.ToLower(name)
	if _, exists := r.fns[name]; exists {
		panic(fmt.Sprintf("funcs: duplicate registration for %q", name))
	}
	r.fns[name] = fn
}

// Resolve implements [expr.FunctionRegistry].
func (r *Registry) Resolve(name string) (expr.BuiltinFn, bool) {
	fn, ok := r.fns[strings.ToLower(name)]
	return fn, ok
}

// DefaultRegistry is the pre-populated registry containing all essential
// built-ins. It is safe for concurrent use.
//
//nolint:gochecknoglobals // package-level singleton; immutable after init
var DefaultRegistry = buildDefaultRegistry()

func buildDefaultRegistry() *Registry {
	r := NewRegistry()

	// ── Aggregates (stub — real aggregation lives in exec) ─────────────────────
	r.Register("count", fnCount)

	// ── Graph accessors ────────────────────────────────────────────────────────
	r.Register("id", fnID)
	r.Register("labels", fnLabels)
	r.Register("type", fnType)
	r.Register("startnode", fnStartNode)
	r.Register("endnode", fnEndNode)
	r.Register("nodes", fnNodes)
	r.Register("relationships", fnRelationships)

	// ── Map / collection accessors ─────────────────────────────────────────────
	r.Register("keys", fnKeys)
	r.Register("properties", fnProperties)
	r.Register("size", fnSize)
	r.Register("length", fnLength)
	r.Register("head", fnHead)
	r.Register("tail", fnTail)
	r.Register("last", fnLast)
	r.Register("range", fnRange)

	// ── Type conversion ────────────────────────────────────────────────────────
	r.Register("tostring", fnToString)
	r.Register("tointeger", fnToInteger)
	r.Register("tofloat", fnToFloat)
	r.Register("toboolean", fnToBoolean)

	// ── NULL handling ──────────────────────────────────────────────────────────
	r.Register("coalesce", fnCoalesce)

	// ── Math ───────────────────────────────────────────────────────────────────
	r.Register("abs", fnAbs)
	r.Register("ceil", fnCeil)
	r.Register("floor", fnFloor)
	r.Register("round", fnRound)
	r.Register("sqrt", fnSqrt)
	r.Register("sign", fnSign)

	// ── String ─────────────────────────────────────────────────────────────────
	r.Register("trim", fnTrim)
	r.Register("ltrim", fnLTrim)
	r.Register("rtrim", fnRTrim)
	r.Register("toupper", fnToUpper)
	r.Register("tolower", fnToLower)
	r.Register("substring", fnSubstring)
	r.Register("replace", fnReplace)
	r.Register("split", fnSplit)
	r.Register("left", fnLeft)
	r.Register("right", fnRight)
	r.Register("reverse", fnReverse)

	// ── Extended math (exp, log, trig, pi, e, rand, degrees, radians) ──────────
	registerMathFuncs(r)

	// ── Extended list (sort, extract stub, filter stub) ────────────────────────
	registerListFuncs(r)

	// ── Temporal constructors (date, datetime, duration, ...) ──────────────────
	registerTemporal(r)

	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// requireArity returns an ArityError when len(args) != want.
func requireArity(fn string, args []expr.Value, want int) error {
	if len(args) != want {
		return &ArityError{Function: fn, Got: len(args), Want: fmt.Sprintf("exactly %d", want)}
	}
	return nil
}

// requireArityRange returns an ArityError when len(args) < min or > max.
func requireArityRange(fn string, args []expr.Value, min, max int) error {
	if len(args) < min || len(args) > max {
		return &ArityError{Function: fn, Got: len(args), Want: fmt.Sprintf("%d..%d", min, max)}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Aggregate stub
// ─────────────────────────────────────────────────────────────────────────────

// fnCount is a scalar stub that returns 1 for non-NULL and 0 for NULL.
// Real aggregation is handled by the EagerAggregation operator.
func fnCount(args []expr.Value) (expr.Value, error) {
	if err := requireArity("count", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.IntegerValue(0), nil
	}
	return expr.IntegerValue(1), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph accessors
// ─────────────────────────────────────────────────────────────────────────────

// fnID implements the Cypher id() scalar: the integer identity of a node or
// relationship (NULL for NULL input).
//
// Stability contract: a node's id() is its interned NodeID and is STABLE across
// a store reopen (persisted via the mapper). A relationship's id() is its
// positional index in the current CSR adjacency — unique and forward/reverse
// consistent WITHIN a query (it doubles as the relationship-isomorphism key) —
// but NOT guaranteed stable across a reopen or CSR rebuild. The durable
// per-edge handle is intentionally not surfaced here, and elementId() is not
// implemented. openCypher treats the concrete id() value as
// implementation-defined; the TCK does not constrain it.
func fnID(args []expr.Value) (expr.Value, error) {
	if err := requireArity("id", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.NodeValue:
		return expr.IntegerValue(int64(v.ID)), nil
	case expr.RelationshipValue:
		return expr.IntegerValue(int64(v.ID)), nil
	default:
		return nil, &TypeError{Function: "id", ArgIndex: 0, Got: args[0].Kind(), Want: "Node or Relationship"}
	}
}

func fnLabels(args []expr.Value) (expr.Value, error) {
	if err := requireArity("labels", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	nv, ok := args[0].(expr.NodeValue)
	if !ok {
		return nil, &TypeError{Function: "labels", ArgIndex: 0, Got: args[0].Kind(), Want: "Node"}
	}
	if nv.Deleted {
		// openCypher 9 §3.5.8: accessing the label set of a node deleted
		// in the same statement is EntityNotFound: DeletedEntityAccess.
		return nil, &expr.EvalError{Msg: "EntityNotFound: DeletedEntityAccess: cannot read labels of deleted node"}
	}
	result := make(expr.ListValue, len(nv.Labels))
	for i, l := range nv.Labels {
		result[i] = expr.StringValue(l)
	}
	return result, nil
}

func fnType(args []expr.Value) (expr.Value, error) {
	if err := requireArity("type", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	rv, ok := args[0].(expr.RelationshipValue)
	if !ok {
		return nil, &TypeError{Function: "type", ArgIndex: 0, Got: args[0].Kind(), Want: "Relationship"}
	}
	return expr.StringValue(rv.Type), nil
}

func fnStartNode(args []expr.Value) (expr.Value, error) {
	if err := requireArity("startNode", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	rv, ok := args[0].(expr.RelationshipValue)
	if !ok {
		return nil, &TypeError{Function: "startNode", ArgIndex: 0, Got: args[0].Kind(), Want: "Relationship"}
	}
	return expr.NodeValue{ID: rv.StartID}, nil
}

func fnEndNode(args []expr.Value) (expr.Value, error) {
	if err := requireArity("endNode", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	rv, ok := args[0].(expr.RelationshipValue)
	if !ok {
		return nil, &TypeError{Function: "endNode", ArgIndex: 0, Got: args[0].Kind(), Want: "Relationship"}
	}
	return expr.NodeValue{ID: rv.EndID}, nil
}

func fnNodes(args []expr.Value) (expr.Value, error) {
	if err := requireArity("nodes", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.PathValue:
		result := make(expr.ListValue, len(v.Nodes))
		for i, n := range v.Nodes {
			result[i] = n
		}
		return result, nil
	case expr.ListValue:
		// VarLengthExpand encodes named paths as a flat alternating
		// list [node, rel, node, rel, ..., node]. Extract every even
		// index. A purely-edge or purely-node list still works:
		// elements that are not NodeValue are silently skipped.
		result := make(expr.ListValue, 0, (len(v)+1)/2)
		for i, e := range v {
			if i%2 == 0 {
				if _, ok := e.(expr.NodeValue); ok {
					result = append(result, e)
				}
			}
		}
		return result, nil
	}
	return nil, &TypeError{Function: "nodes", ArgIndex: 0, Got: args[0].Kind(), Want: "Path"}
}

func fnRelationships(args []expr.Value) (expr.Value, error) {
	if err := requireArity("relationships", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.PathValue:
		result := make(expr.ListValue, len(v.Relationships))
		for i, r := range v.Relationships {
			result[i] = r
		}
		return result, nil
	case expr.ListValue:
		// VarLengthExpand encodes named paths as a flat alternating
		// list [node, rel, node, rel, ..., node]. Extract every odd
		// index that is a relationship.
		result := make(expr.ListValue, 0, len(v)/2)
		for i, e := range v {
			if i%2 == 1 {
				if _, ok := e.(expr.RelationshipValue); ok {
					result = append(result, e)
				}
			}
		}
		return result, nil
	}
	return nil, &TypeError{Function: "relationships", ArgIndex: 0, Got: args[0].Kind(), Want: "Path"}
}

// ─────────────────────────────────────────────────────────────────────────────
// Map / collection accessors
// ─────────────────────────────────────────────────────────────────────────────

func fnKeys(args []expr.Value) (expr.Value, error) {
	if err := requireArity("keys", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.MapValue:
		result := make(expr.ListValue, 0, len(v))
		for k := range v {
			result = append(result, expr.StringValue(k))
		}
		return result, nil
	case expr.NodeValue:
		if v.Properties == nil {
			return expr.ListValue{}, nil
		}
		result := make(expr.ListValue, 0, len(v.Properties))
		for k := range v.Properties {
			result = append(result, expr.StringValue(k))
		}
		return result, nil
	case expr.RelationshipValue:
		if v.Properties == nil {
			return expr.ListValue{}, nil
		}
		result := make(expr.ListValue, 0, len(v.Properties))
		for k := range v.Properties {
			result = append(result, expr.StringValue(k))
		}
		return result, nil
	default:
		return nil, &TypeError{Function: "keys", ArgIndex: 0, Got: args[0].Kind(), Want: "Map, Node, or Relationship"}
	}
}

func fnProperties(args []expr.Value) (expr.Value, error) {
	if err := requireArity("properties", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.MapValue:
		return v, nil
	case expr.NodeValue:
		if v.Properties == nil {
			return expr.MapValue{}, nil
		}
		return v.Properties, nil
	case expr.RelationshipValue:
		if v.Properties == nil {
			return expr.MapValue{}, nil
		}
		return v.Properties, nil
	default:
		return nil, &TypeError{Function: "properties", ArgIndex: 0, Got: args[0].Kind(), Want: "Map, Node, or Relationship"}
	}
}

// fnSize returns the number of elements in a list, or the number of characters
// in a string, or the number of entries in a map.
func fnSize(args []expr.Value) (expr.Value, error) {
	if err := requireArity("size", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.ListValue:
		return expr.IntegerValue(int64(len(v))), nil
	case expr.StringValue:
		// size() on a string returns the number of Unicode characters.
		return expr.IntegerValue(int64(len([]rune(string(v))))), nil
	case expr.MapValue:
		return expr.IntegerValue(int64(len(v))), nil
	default:
		return nil, &TypeError{Function: "size", ArgIndex: 0, Got: args[0].Kind(), Want: "List, String, or Map"}
	}
}

// fnLength returns the number of relationships in a path.
func fnLength(args []expr.Value) (expr.Value, error) {
	if err := requireArity("length", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.PathValue:
		return expr.IntegerValue(int64(len(v.Relationships))), nil
	case expr.ListValue:
		// Fallback: length on a list returns its size.
		return expr.IntegerValue(int64(len(v))), nil
	case expr.StringValue:
		return expr.IntegerValue(int64(len([]rune(string(v))))), nil
	default:
		return nil, &TypeError{Function: "length", ArgIndex: 0, Got: args[0].Kind(), Want: "Path or String"}
	}
}

func fnHead(args []expr.Value) (expr.Value, error) {
	if err := requireArity("head", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	lv, ok := args[0].(expr.ListValue)
	if !ok {
		return nil, &TypeError{Function: "head", ArgIndex: 0, Got: args[0].Kind(), Want: "List"}
	}
	if len(lv) == 0 {
		return expr.Null, nil
	}
	return lv[0], nil
}

func fnTail(args []expr.Value) (expr.Value, error) {
	if err := requireArity("tail", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	lv, ok := args[0].(expr.ListValue)
	if !ok {
		return nil, &TypeError{Function: "tail", ArgIndex: 0, Got: args[0].Kind(), Want: "List"}
	}
	if len(lv) == 0 {
		return expr.ListValue{}, nil
	}
	result := make(expr.ListValue, len(lv)-1)
	copy(result, lv[1:])
	return result, nil
}

func fnLast(args []expr.Value) (expr.Value, error) {
	if err := requireArity("last", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	lv, ok := args[0].(expr.ListValue)
	if !ok {
		return nil, &TypeError{Function: "last", ArgIndex: 0, Got: args[0].Kind(), Want: "List"}
	}
	if len(lv) == 0 {
		return expr.Null, nil
	}
	return lv[len(lv)-1], nil
}

// maxRangeElements bounds the number of integers a single range() call may
// materialise. It guards against untrusted queries such as
// range(1, 9223372036854775807), whose unbounded element count previously
// caused a makeslice panic or an out-of-memory kill. The limit (1e8) is far
// above any value used by an openCypher TCK scenario (the largest TCK bound is
// on the order of a few thousand) while keeping the worst-case allocation
// bounded. A request that exceeds it returns a typed [expr.EvalError] rather
// than allocating.
const maxRangeElements = 100_000_000

// errRangeTooLarge builds the typed error returned when a range() call would
// materialise more than maxRangeElements integers. quotient is the floor of
// span/|step|; the true element count is quotient+1, but quotient is already
// at or above the cap and quotient+1 may overflow uint64, so the message is
// expressed in terms of the lower bound to stay overflow-safe.
func errRangeTooLarge(quotient uint64) error {
	return &expr.EvalError{Msg: fmt.Sprintf(
		"ArgumentError: NumberOutOfRange: range() would produce more than %d elements (at least %d), exceeding the maximum of %d",
		maxRangeElements, quotient, maxRangeElements)}
}

// maxReplaceOutputBytes bounds the byte size a single replace() call may
// materialise. It is kept equal to expr.DefaultMaxStringEvalBytes (1 GiB) so
// replace() obeys the same per-evaluation string ceiling as the "+" / reduce()
// paths, which charge that budget directly. replace() runs via the generic
// function dispatch (cypher/expr/eval.go), which never charges the byte budget,
// so the bound is enforced here instead. It guards the empty-search quadratic
// amplification — replace($a, ”, $b) inserts $b between every rune of $a, so
// the output grows to (runeCount($a)+1)*len($b) — which O(1) query text with
// large string PARAMETERS (parameters bypass the 1 MiB query-text guard) could
// drive to a multi-terabyte allocation and OOM-kill the process (#1494).
const maxReplaceOutputBytes = expr.DefaultMaxStringEvalBytes

// errReplaceTooLarge builds the typed error returned when a replace() call would
// materialise more than maxReplaceOutputBytes output bytes. The message shape
// mirrors errRangeTooLarge / expr.errStringTooLarge so callers map it to a query
// error, never a panic or an out-of-memory crash.
func errReplaceTooLarge() error {
	return &expr.EvalError{Msg: fmt.Sprintf(
		"ArgumentError: NumberOutOfRange: replace() would produce more than %d string bytes, exceeding the maximum of %d",
		maxReplaceOutputBytes, maxReplaceOutputBytes)}
}

// fnRange returns a list of integers: range(start, end) or range(start, end, step).
// Start and end are inclusive. A negative step traverses downward.
func fnRange(args []expr.Value) (expr.Value, error) {
	if err := requireArityRange("range", args, 2, 3); err != nil {
		return nil, err
	}
	for i, a := range args {
		if expr.IsNull(a) {
			return expr.Null, nil
		}
		if _, ok := a.(expr.IntegerValue); !ok {
			return nil, &TypeError{Function: "range", ArgIndex: i, Got: a.Kind(), Want: "Integer"}
		}
	}
	start := int64(args[0].(expr.IntegerValue)) //nolint:forcetypeassert // type-checked above
	end := int64(args[1].(expr.IntegerValue))   //nolint:forcetypeassert // type-checked above
	step := int64(1)
	if len(args) == 3 {
		step = int64(args[2].(expr.IntegerValue)) //nolint:forcetypeassert // type-checked above
	}
	if step == 0 {
		return nil, &ArityError{Function: "range", Got: 0, Want: "non-zero step"}
	}

	// Compute the element count overflow-safely and reject ranges that would
	// materialise more than maxRangeElements. The naive count
	// (end-start)/step + 1 can overflow int64 (e.g. start=MinInt64,
	// end=MaxInt64) and the int cap could be negative or astronomically large,
	// which previously caused a `makeslice: cap out of range` panic or an
	// out-of-memory kill for an untrusted query such as
	// `range(1, 9223372036854775807)`.
	//
	// We never form a wrapped value nor hand a giant capacity to make. The span
	// is computed in uint64 (two's complement yields the correct magnitude when
	// the bounds are ordered, even when the signed difference would overflow).
	// The element count is span/|step| + 1, but adding 1 can itself overflow
	// uint64 when the quotient is 2^64-1 (e.g. range(MinInt64, MaxInt64)). Since
	// maxRangeElements is far below uint64's range, we test the quotient first:
	// count = quotient + 1 > maxRangeElements is equivalent to
	// quotient >= maxRangeElements for any non-negative integer cap, so we never
	// compute the wrapping +1 on the over-cap path.
	var count uint64
	switch {
	case step > 0 && end >= start:
		span := uint64(end) - uint64(start)
		quotient := span / uint64(step)
		if quotient >= maxRangeElements {
			return nil, errRangeTooLarge(quotient)
		}
		count = quotient + 1
	case step < 0 && end <= start:
		span := uint64(start) - uint64(end)
		quotient := span / uint64(-step)
		if quotient >= maxRangeElements {
			return nil, errRangeTooLarge(quotient)
		}
		count = quotient + 1
	default:
		// Direction of the range is inconsistent with the step: empty list.
		count = 0
	}

	// Materialise exactly count elements rather than looping on i <= end /
	// i >= end. A comparison loop with `i += step` overflows int64 at the
	// boundary (e.g. range(MaxInt64-2, MaxInt64): after the final element
	// MaxInt64, i += 1 wraps to MinInt64 and the i <= end test stays true),
	// which then marches across the entire int64 space — an unbounded append
	// that hangs and OOMs the process. count is computed overflow-safely
	// above, so iterating it exactly bounds the work. The increment on the
	// final iteration may itself overflow, but its value is never read (the
	// loop has already produced count elements) and int64 overflow is defined
	// (two's-complement wrap) in Go, so it is harmless.
	result := make(expr.ListValue, 0, count)
	v := start
	for n := uint64(0); n < count; n++ {
		result = append(result, expr.IntegerValue(v))
		v += step
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Type conversion
// ─────────────────────────────────────────────────────────────────────────────

// formatFloatToString renders a FLOAT for toString() so its FLOAT-ness survives:
// an integer-valued finite float keeps an explicit ".0" suffix ("1.0", not "1"),
// matching openCypher/Neo4j (toString(1.0)='1.0') and the engine's own TCK
// comparison formatter. Non-integer floats and NaN/±Inf are unchanged from the
// prior shortest-fixed-point form (#1764). The 1e21 ceiling avoids emitting an
// astronomically long fixed-point digit string for huge magnitudes.
func formatFloatToString(f float64) string {
	if !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func fnToString(args []expr.Value) (expr.Value, error) {
	if err := requireArity("toString", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.StringValue:
		return v, nil
	case expr.IntegerValue:
		return expr.StringValue(strconv.FormatInt(int64(v), 10)), nil
	case expr.FloatValue:
		return expr.StringValue(formatFloatToString(float64(v))), nil
	case expr.BoolValue:
		if bool(v) {
			return expr.StringValue("true"), nil
		}
		return expr.StringValue("false"), nil
	// Temporal types — return their canonical ISO-8601 string representation.
	case expr.DateValue:
		return expr.StringValue(v.String()), nil
	case expr.LocalDateTimeValue:
		return expr.StringValue(v.String()), nil
	case expr.DateTimeValue:
		return expr.StringValue(v.String()), nil
	case expr.LocalTimeValue:
		return expr.StringValue(v.String()), nil
	case expr.TimeValue:
		return expr.StringValue(v.String()), nil
	case expr.DurationValue:
		return expr.StringValue(v.String()), nil
	default:
		return nil, &TypeError{Function: "toString", ArgIndex: 0, Got: args[0].Kind(), Want: "String, Integer, Float, Boolean, or temporal type"}
	}
}

func fnToInteger(args []expr.Value) (expr.Value, error) {
	if err := requireArity("toInteger", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.IntegerValue:
		return v, nil
	case expr.FloatValue:
		f := math.Trunc(float64(v))
		// float64(math.MaxInt64) rounds UP to 2^63 = 9223372036854775808.0, so
		// the naive `f > float64(math.MaxInt64)` misses exactly 2^63 itself.
		// Use a named constant for the rounded value and guard with >=.
		// -maxInt64Float == float64(math.MinInt64) exactly, so `f < -maxInt64Float`
		// rejects only values strictly below MinInt64 (MinInt64 itself is valid).
		const maxInt64Float = 9223372036854775808.0 // 2^63; float64(math.MaxInt64) rounds to this
		if math.IsNaN(f) || math.IsInf(f, 0) || f >= maxInt64Float || f < -maxInt64Float {
			return nil, &expr.EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: float value %v is out of range for integer conversion", float64(v))}
		}
		return expr.IntegerValue(int64(f)), nil
	case expr.StringValue:
		s := strings.TrimSpace(string(v))
		// Direct integer parse first — the common case for "42", "-7", etc.
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return expr.IntegerValue(n), nil
		}
		// Fall back to float parse so "2.9" → 2 (truncate toward zero), per
		// openCypher: toInteger(<floatString>) drops the fractional part.
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			ft := math.Trunc(f)
			// Same boundary fix as the FloatValue path: use >= maxInt64Float.
			const maxInt64Float = 9223372036854775808.0
			if math.IsNaN(ft) || math.IsInf(ft, 0) || ft >= maxInt64Float || ft < -maxInt64Float {
				return nil, &expr.EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: float value %v is out of range for integer conversion", f)}
			}
			return expr.IntegerValue(int64(ft)), nil
		}
		return expr.Null, nil // non-parseable → NULL
	case expr.BoolValue:
		if bool(v) {
			return expr.IntegerValue(1), nil
		}
		return expr.IntegerValue(0), nil
	default:
		return nil, &TypeError{Function: "toInteger", ArgIndex: 0, Got: args[0].Kind(), Want: "Integer, Float, String, or Boolean"}
	}
}

func fnToFloat(args []expr.Value) (expr.Value, error) {
	if err := requireArity("toFloat", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.FloatValue:
		return v, nil
	case expr.IntegerValue:
		return expr.FloatValue(float64(int64(v))), nil
	case expr.StringValue:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		if err != nil {
			return expr.Null, nil // non-parseable → NULL
		}
		return expr.FloatValue(f), nil
	default:
		return nil, &TypeError{Function: "toFloat", ArgIndex: 0, Got: args[0].Kind(), Want: "Float, Integer, or String"}
	}
}

func fnToBoolean(args []expr.Value) (expr.Value, error) {
	if err := requireArity("toBoolean", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.BoolValue:
		return v, nil
	case expr.StringValue:
		s := strings.TrimSpace(strings.ToLower(string(v)))
		switch s {
		case "true":
			return expr.BoolValue(true), nil
		case "false":
			return expr.BoolValue(false), nil
		default:
			return expr.Null, nil
		}
	default:
		return nil, &TypeError{Function: "toBoolean", ArgIndex: 0, Got: args[0].Kind(), Want: "Boolean or String"}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NULL handling
// ─────────────────────────────────────────────────────────────────────────────

// fnCoalesce returns the first non-NULL argument, or NULL if all are NULL.
// This respects 3VL: NULL inputs are simply skipped.
func fnCoalesce(args []expr.Value) (expr.Value, error) {
	if len(args) == 0 {
		return expr.Null, nil
	}
	for _, a := range args {
		if !expr.IsNull(a) {
			return a, nil
		}
	}
	return expr.Null, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Math
// ─────────────────────────────────────────────────────────────────────────────

func fnAbs(args []expr.Value) (expr.Value, error) {
	if err := requireArity("abs", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.IntegerValue:
		n := int64(v)
		if n == math.MinInt64 {
			// -MinInt64 is not representable as int64 — overflow.
			return nil, &expr.EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: integer overflow in abs(%d)", n)}
		}
		if n < 0 {
			return expr.IntegerValue(-n), nil
		}
		return v, nil
	case expr.FloatValue:
		return expr.FloatValue(math.Abs(float64(v))), nil
	default:
		return nil, &TypeError{Function: "abs", ArgIndex: 0, Got: args[0].Kind(), Want: "Integer or Float"}
	}
}

func fnCeil(args []expr.Value) (expr.Value, error) {
	if err := requireArity("ceil", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.FloatValue:
		return expr.FloatValue(math.Ceil(float64(v))), nil
	case expr.IntegerValue:
		return v, nil
	default:
		return nil, &TypeError{Function: "ceil", ArgIndex: 0, Got: args[0].Kind(), Want: "Float or Integer"}
	}
}

func fnFloor(args []expr.Value) (expr.Value, error) {
	if err := requireArity("floor", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.FloatValue:
		return expr.FloatValue(math.Floor(float64(v))), nil
	case expr.IntegerValue:
		return v, nil
	default:
		return nil, &TypeError{Function: "floor", ArgIndex: 0, Got: args[0].Kind(), Want: "Float or Integer"}
	}
}

func fnRound(args []expr.Value) (expr.Value, error) {
	if err := requireArity("round", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.FloatValue:
		return expr.FloatValue(math.Round(float64(v))), nil
	case expr.IntegerValue:
		return v, nil
	default:
		return nil, &TypeError{Function: "round", ArgIndex: 0, Got: args[0].Kind(), Want: "Float or Integer"}
	}
}

func fnSqrt(args []expr.Value) (expr.Value, error) {
	if err := requireArity("sqrt", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	var f float64
	switch v := args[0].(type) {
	case expr.FloatValue:
		f = float64(v)
	case expr.IntegerValue:
		f = float64(int64(v))
	default:
		return nil, &TypeError{Function: "sqrt", ArgIndex: 0, Got: args[0].Kind(), Want: "Float or Integer"}
	}
	return expr.FloatValue(math.Sqrt(f)), nil
}

func fnSign(args []expr.Value) (expr.Value, error) {
	if err := requireArity("sign", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.IntegerValue:
		n := int64(v)
		if n < 0 {
			return expr.IntegerValue(-1), nil
		}
		if n > 0 {
			return expr.IntegerValue(1), nil
		}
		return expr.IntegerValue(0), nil
	case expr.FloatValue:
		return expr.FloatValue(math.Copysign(1, float64(v))), nil
	default:
		return nil, &TypeError{Function: "sign", ArgIndex: 0, Got: args[0].Kind(), Want: "Integer or Float"}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// String
// ─────────────────────────────────────────────────────────────────────────────

func fnTrim(args []expr.Value) (expr.Value, error) {
	if err := requireArity("trim", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "trim", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	return expr.StringValue(strings.TrimSpace(string(sv))), nil
}

func fnLTrim(args []expr.Value) (expr.Value, error) {
	if err := requireArity("ltrim", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "ltrim", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	return expr.StringValue(strings.TrimLeft(string(sv), " \t\n\r")), nil
}

func fnRTrim(args []expr.Value) (expr.Value, error) {
	if err := requireArity("rtrim", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "rtrim", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	return expr.StringValue(strings.TrimRight(string(sv), " \t\n\r")), nil
}

func fnToUpper(args []expr.Value) (expr.Value, error) {
	if err := requireArity("toUpper", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "toUpper", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	return expr.StringValue(strings.ToUpper(string(sv))), nil
}

func fnToLower(args []expr.Value) (expr.Value, error) {
	if err := requireArity("toLower", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "toLower", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	return expr.StringValue(strings.ToLower(string(sv))), nil
}

// fnSubstring extracts a substring: substring(string, start) or substring(string, start, length).
func fnSubstring(args []expr.Value) (expr.Value, error) {
	if err := requireArityRange("substring", args, 2, 3); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "substring", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	startV, ok := args[1].(expr.IntegerValue)
	if !ok {
		return nil, &TypeError{Function: "substring", ArgIndex: 1, Got: args[1].Kind(), Want: "Integer"}
	}
	runes := []rune(string(sv))
	start := int(int64(startV))
	if start < 0 {
		// Neo4j raises ArgumentError for a negative start index rather than
		// clamping (#1768). A start beyond the end still returns "" (a value
		// query, below) — only a negative start is an error.
		return nil, &expr.EvalError{Msg: fmt.Sprintf("ArgumentError: substring: negative start index %d", start)}
	}
	if start > len(runes) {
		return expr.StringValue(""), nil
	}
	if len(args) == 3 {
		if expr.IsNull(args[2]) {
			return expr.Null, nil
		}
		lenV, ok := args[2].(expr.IntegerValue)
		if !ok {
			return nil, &TypeError{Function: "substring", ArgIndex: 2, Got: args[2].Kind(), Want: "Integer"}
		}
		length := int(int64(lenV))
		if length < 0 {
			return nil, &expr.EvalError{Msg: fmt.Sprintf("ArgumentError: substring: negative length %d", length)}
		}
		// Compute the end bound overflow-safely. start ∈ [0, len(runes)] and
		// length ≥ 0 at this point, so len(runes)-start ≥ 0 cannot underflow.
		// A naive `start + length` overflows int to a negative value for a huge
		// length (e.g. substring('hello', 2, MaxInt64)); the negative end would
		// slip past the `end > len(runes)` clamp and panic in the slice. Clamping
		// length to the available tail instead matches Neo4j/openCypher, which
		// return the truncated tail for an over-large length (a value query, not
		// an out-of-range error).
		var end int
		if length > len(runes)-start {
			end = len(runes)
		} else {
			end = start + length
		}
		return expr.StringValue(string(runes[start:end])), nil
	}
	return expr.StringValue(string(runes[start:])), nil
}

func fnReplace(args []expr.Value) (expr.Value, error) {
	if err := requireArity("replace", args, 3); err != nil {
		return nil, err
	}
	for i, a := range args {
		if expr.IsNull(a) {
			return expr.Null, nil
		}
		if _, ok := a.(expr.StringValue); !ok {
			return nil, &TypeError{Function: "replace", ArgIndex: i, Got: a.Kind(), Want: "String"}
		}
	}
	original := string(args[0].(expr.StringValue)) //nolint:forcetypeassert // type-checked above
	search := string(args[1].(expr.StringValue))   //nolint:forcetypeassert // type-checked above
	replace := string(args[2].(expr.StringValue))  //nolint:forcetypeassert // type-checked above
	if err := checkReplaceBudget(original, search, replace); err != nil {
		return nil, err
	}
	return expr.StringValue(strings.ReplaceAll(original, search, replace)), nil
}

// checkReplaceBudget bounds the worst-case output of strings.ReplaceAll BEFORE
// it allocates, returning a typed [expr.EvalError] (NumberOutOfRange) when the
// output would exceed maxReplaceOutputBytes. All arithmetic is overflow-safe
// (int64, bounded against the cap at each step), so a crafted input can never
// wrap to a small positive size and slip past the check.
//
//   - Empty search: ReplaceAll inserts replace before each rune and once at the
//     end, so output = len(original) + (runeCount(original)+1)*len(replace).
//   - Non-empty search: at most floor(len(original)/len(search)) replacements,
//     so output ≤ len(original) + occurrences*len(replace) (occurrences is an
//     upper bound: each match consumes ≥ len(search) bytes of the original).
//
// The legitimate path (small inputs / no amplification) returns nil and the
// behaviour of replace() is unchanged.
func checkReplaceBudget(original, search, replace string) error {
	const cap64 = int64(maxReplaceOutputBytes)
	var occurrences int64
	if search == "" {
		occurrences = int64(utf8.RuneCountInString(original)) + 1
	} else {
		occurrences = int64(len(original) / len(search))
	}
	// out = len(original) + occurrences*len(replace), computed so neither term
	// nor the sum can overflow undetected: bail as soon as a partial result
	// exceeds the cap.
	out := int64(len(original))
	if out > cap64 {
		return errReplaceTooLarge()
	}
	if lr := int64(len(replace)); lr > 0 && occurrences > 0 {
		// occurrences*lr must not overflow: compare against the remaining budget
		// using division rather than multiplication.
		if occurrences > (cap64-out)/lr {
			return errReplaceTooLarge()
		}
		out += occurrences * lr
	}
	if out > cap64 {
		return errReplaceTooLarge()
	}
	return nil
}

func fnSplit(args []expr.Value) (expr.Value, error) {
	if err := requireArity("split", args, 2); err != nil {
		return nil, err
	}
	for i, a := range args {
		if expr.IsNull(a) {
			return expr.Null, nil
		}
		if _, ok := a.(expr.StringValue); !ok {
			return nil, &TypeError{Function: "split", ArgIndex: i, Got: a.Kind(), Want: "String"}
		}
	}
	original := string(args[0].(expr.StringValue)) //nolint:forcetypeassert // type-checked above
	delim := string(args[1].(expr.StringValue))    //nolint:forcetypeassert // type-checked above
	parts := strings.Split(original, delim)
	result := make(expr.ListValue, len(parts))
	for i, p := range parts {
		result[i] = expr.StringValue(p)
	}
	return result, nil
}

func fnLeft(args []expr.Value) (expr.Value, error) {
	if err := requireArity("left", args, 2); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "left", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	nv, ok := args[1].(expr.IntegerValue)
	if !ok {
		return nil, &TypeError{Function: "left", ArgIndex: 1, Got: args[1].Kind(), Want: "Integer"}
	}
	runes := []rune(string(sv))
	n := int(int64(nv))
	if n < 0 {
		return nil, &expr.EvalError{Msg: fmt.Sprintf("ArgumentError: left: negative length %d", n)}
	}
	if n > len(runes) {
		n = len(runes)
	}
	return expr.StringValue(string(runes[:n])), nil
}

func fnRight(args []expr.Value) (expr.Value, error) {
	if err := requireArity("right", args, 2); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return expr.Null, nil
	}
	sv, ok := args[0].(expr.StringValue)
	if !ok {
		return nil, &TypeError{Function: "right", ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	nv, ok := args[1].(expr.IntegerValue)
	if !ok {
		return nil, &TypeError{Function: "right", ArgIndex: 1, Got: args[1].Kind(), Want: "Integer"}
	}
	runes := []rune(string(sv))
	n := int(int64(nv))
	if n < 0 {
		return nil, &expr.EvalError{Msg: fmt.Sprintf("ArgumentError: right: negative length %d", n)}
	}
	if n > len(runes) {
		n = len(runes)
	}
	return expr.StringValue(string(runes[len(runes)-n:])), nil
}

func fnReverse(args []expr.Value) (expr.Value, error) {
	if err := requireArity("reverse", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.StringValue:
		runes := []rune(string(v))
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return expr.StringValue(string(runes)), nil
	case expr.ListValue:
		result := make(expr.ListValue, len(v))
		for i, elem := range v {
			result[len(v)-1-i] = elem
		}
		return result, nil
	default:
		return nil, &TypeError{Function: "reverse", ArgIndex: 0, Got: args[0].Kind(), Want: "String or List"}
	}
}

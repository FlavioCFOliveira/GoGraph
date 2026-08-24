package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
)

// EngineAdapter wraps the real [github.com/FlavioCFOliveira/GoGraph/cypher.Engine]
// so it satisfies the simulator's minimal [Engine] interface. It converts the
// simulator's string-keyed parameter maps into the engine's
// map[string]expr.Value and projects the engine's rich *cypher.Result onto the
// checker's narrow [Result] view.
//
// # Concurrency contract
//
// EngineAdapter is NOT safe for concurrent use; the simulator drives it from a
// single goroutine.
type EngineAdapter struct {
	eng *cypher.Engine
}

// NewEngineAdapter wraps eng. eng must be non-nil.
func NewEngineAdapter(eng *cypher.Engine) *EngineAdapter {
	return &EngineAdapter{eng: eng}
}

// Run converts params and executes a read-only query, returning a [Result]
// over the engine's result. The returned Result must be closed by the caller.
// It routes through the engine's read path ([cypher.Engine.Run]); use
// [EngineAdapter.RunWrite] for statements that mutate the graph.
func (a *EngineAdapter) Run(ctx context.Context, query string, params map[string]any) (Result, error) {
	ev, err := toExprParams(params)
	if err != nil {
		return nil, err
	}
	res, err := a.eng.Run(ctx, query, ev)
	if err != nil {
		return nil, err
	}
	return &resultAdapter{res: res}, nil
}

// RunWrite converts params and executes a mutating query through the engine's
// autocommit write path ([cypher.Engine.RunInTx]), which the engine requires
// for CREATE / MERGE / SET / DELETE statements. The returned Result must be
// closed by the caller.
func (a *EngineAdapter) RunWrite(ctx context.Context, query string, params map[string]any) (Result, error) {
	ev, err := toExprParams(params)
	if err != nil {
		return nil, err
	}
	res, err := a.eng.RunInTx(ctx, query, ev)
	if err != nil {
		return nil, err
	}
	return &resultAdapter{res: res}, nil
}

// Explain converts params and returns the engine's physical-plan rendering for
// query without executing it ([cypher.Engine.Explain]). The access-path parity
// checker reads the chosen access path (seek vs scan) from this rendering.
func (a *EngineAdapter) Explain(query string, params map[string]any) (string, error) {
	ev, err := toExprParams(params)
	if err != nil {
		return "", err
	}
	return a.eng.Explain(query, ev)
}

// Profile converts params, executes the read-only query, and returns the
// physical plan annotated with per-operator rows, db-hits, and time
// ([cypher.Engine.Profile]). The access-path parity checker uses it to assert
// that a data-touching probe reports non-zero db-hits.
func (a *EngineAdapter) Profile(ctx context.Context, query string, params map[string]any) (string, error) {
	ev, err := toExprParams(params)
	if err != nil {
		return "", err
	}
	return a.eng.Profile(ctx, query, ev)
}

// StatsTrackedPairs reports how many distinct (label, property) pairs the
// wrapped engine currently holds planner statistics for
// ([cypher.Engine.StatsTrackedPairs]): 0 until the first completed
// db.stats.refresh() of the engine's lifetime, and 0 again on a recovered
// engine until its next refresh. The statistics-regime checker (rmp #2456)
// reads it as the something-was-seen observable that a rebuild really
// published statistics.
func (a *EngineAdapter) StatsTrackedPairs() int {
	return a.eng.StatsTrackedPairs()
}

// CountSnapshot returns a point-in-time copy of the wrapped engine's
// relationship count-store cells and dirty markings
// ([cypher.Engine.CountSnapshot]). The count-store oracle (rmp #2494) reads it as
// the observed side of its cell-by-cell parity check; the keys are the interned
// label/relationship-type ids of the graph THIS engine holds, so a caller must
// resolve them through that same graph's registry — a recovered engine re-interns
// from scratch and its ids are unrelated to the crashed one's.
func (a *EngineAdapter) CountSnapshot() count.Snapshot {
	return a.eng.CountSnapshot()
}

// CountStoreCells reports how many distinct live count-store cells the wrapped
// engine holds ([cypher.Engine.CountStoreCells]). The count-store oracle reads it
// as the footprint the boundedness clause bounds by observed schema cardinality
// rather than by |V| or |E|.
func (a *EngineAdapter) CountStoreCells() int {
	return a.eng.CountStoreCells()
}

// NodeCount returns the live node count by running a whole-graph count query
// through the real engine, so it exercises the same execution path the
// workload uses.
func (a *EngineAdapter) NodeCount() (int64, error) {
	return a.scalarCount("MATCH (n) RETURN count(n)")
}

// EdgeCount returns the live edge count by running a whole-graph relationship
// count query through the real engine.
func (a *EngineAdapter) EdgeCount() (int64, error) {
	return a.scalarCount("MATCH ()-[r]->() RETURN count(r)")
}

// scalarCount runs query and returns the integer in its first column.
func (a *EngineAdapter) scalarCount(query string) (int64, error) {
	res, err := a.eng.Run(context.Background(), query, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var n int64
	if res.Next() {
		if v, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			n = int64(v)
		}
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// projectRowStrings runs a single-row read query and returns the canonical
// String() rendering of each of the first ncols projected columns. It returns
// (nil, nil) when the query yields no row, so the type-coverage checker can
// distinguish a missing node from a value mismatch. It routes through the real
// engine read path so the values are exactly what a workload query would see.
func (a *EngineAdapter) projectRowStrings(ctx context.Context, query string, ncols int) ([]string, error) {
	res, err := a.eng.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		return nil, res.Err()
	}
	out := make([]string, ncols)
	for i := 0; i < ncols; i++ {
		out[i] = res.ValueAt(i).String()
	}
	return out, res.Err()
}

// projectRowValues is the TYPE-PRESERVING sibling of
// [EngineAdapter.projectRowStrings]: it runs a single-row read query and
// returns the first ncols projected columns as the engine's own expr.Value
// instances rather than their String() renderings, so a caller can assert the
// value's KIND and not merely its text. It returns (nil, nil) when the query
// yields no row.
//
// The type-coverage checker (rmp #2457) needs this because a temporal that has
// degraded to a plain string is a DIFFERENT KIND carrying the SAME text: only
// the [expr.Value.Kind] of the read-back value distinguishes a working
// temporal round-trip from a broken one.
func (a *EngineAdapter) projectRowValues(ctx context.Context, query string, ncols int) ([]expr.Value, error) {
	res, err := a.eng.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		return nil, res.Err()
	}
	out := make([]expr.Value, ncols)
	for i := 0; i < ncols; i++ {
		out[i] = res.ValueAt(i)
	}
	return out, res.Err()
}

// queryRowStrings runs a read query and returns the canonical String()
// rendering of the first ncols projected columns of EVERY row, in result
// order. It is the multi-row sibling of [EngineAdapter.projectRowStrings],
// used by the schema-introspection oracle to materialise SHOW INDEXES /
// SHOW CONSTRAINTS / db.* procedure row sets for comparison against the
// harness's own DDL model. A query yielding no rows returns an empty
// (non-nil) slice so callers can distinguish "ran, empty" from an error.
func (a *EngineAdapter) queryRowStrings(ctx context.Context, query string, ncols int) ([][]string, error) {
	res, err := a.eng.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	rows := make([][]string, 0, 8)
	for res.Next() {
		row := make([]string, ncols)
		for i := 0; i < ncols; i++ {
			row[i] = res.ValueAt(i).String()
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// resultAdapter projects a *cypher.Result onto the checker's [Result].
type resultAdapter struct {
	res      *cypher.Result
	rowCount int
}

// Next advances the underlying result, tracking the row count.
func (r *resultAdapter) Next() bool {
	if r.res.Next() {
		r.rowCount++
		return true
	}
	return false
}

// ScalarInt reads the first column of the current row as an int64.
func (r *resultAdapter) ScalarInt() (int64, bool) {
	if v, ok := r.res.ValueAt(0).(expr.IntegerValue); ok {
		return int64(v), true
	}
	return 0, false
}

// IntAt reads column i of the current row as an int64.
func (r *resultAdapter) IntAt(i int) (int64, bool) {
	if v, ok := r.res.ValueAt(i).(expr.IntegerValue); ok {
		return int64(v), true
	}
	return 0, false
}

// StringAt reads column i of the current row as a string.
func (r *resultAdapter) StringAt(i int) (string, bool) {
	if v, ok := r.res.ValueAt(i).(expr.StringValue); ok {
		return string(v), true
	}
	return "", false
}

// RowCount reports how many rows have been produced so far.
func (r *resultAdapter) RowCount() int { return r.rowCount }

// Counters returns the per-statement write-effect counters of the underlying
// engine result ([cypher.Result.Counters]): nil for a read-only statement, the
// applied effect set for a write. It implements the checker's counterReporter
// facet so the per-op counters oracle (#2448) can read the effect report from
// the same execution the tick drained. The returned pointer is owned by the
// underlying result and must be treated as read-only.
func (r *resultAdapter) Counters() *exec.QueryCounters { return r.res.Counters() }

// Notifications returns the out-of-band plan-time advisories the engine attached
// to this result ([cypher.Result.Notifications]) — for example the
// Cartesian-product warning raised for a query whose reading clauses build a
// cross product between disconnected patterns (#1483). It implements the
// checker's [notificationReporter] facet so the DST can adjudicate an advisory
// that is invisible to the result rows. Notifications never affect iteration, so
// reading them cannot perturb a run.
func (r *resultAdapter) Notifications() []cypher.Notification { return r.res.Notifications() }

// Err returns the underlying result error.
func (r *resultAdapter) Err() error { return r.res.Err() }

// Close releases the underlying result.
func (r *resultAdapter) Close() error { return r.res.Close() }

// toExprParams converts a string-keyed parameter map into the engine's
// expr.Value map. The supported value kinds are exactly those the Phase-1
// workload binds: string, int64, int, float64, and bool. An unsupported kind is
// an error rather than a silent coercion, so a workload bug surfaces loudly.
func toExprParams(params map[string]any) (map[string]expr.Value, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]expr.Value, len(params))
	for k, v := range params {
		ev, err := toExprValue(v)
		if err != nil {
			return nil, fmt.Errorf("sim: param %q: %w", k, err)
		}
		out[k] = ev
	}
	return out, nil
}

// toExprValue maps a single Go value to its expr.Value. It supports the scalar
// kinds the workload binds (string, int, float, bool), a nil (→ the NULL
// singleton), a homogeneous-or-mixed list ([]any of supported kinds → an
// expr.ListValue), and a string-keyed map (map[string]any → an expr.MapValue),
// so scenarios can bind list-, null-, and map-valued parameters. A map parameter
// is what `SET n = $map`, `SET n += $map`, and `CREATE (n $map)` consume to set
// a SET of scalar properties — openCypher does not permit a map as a single
// property value, so the map's elements are themselves restricted to the scalar
// and list kinds above.
//
// A map parameter as a whole MERGE node pattern (`MERGE (n $map)`) is a
// different matter: the engine REJECTS it at compile time, as the openCypher
// TCK requires (cypher/tck/features/clauses/merge/Merge1.feature scenario
// [16]), and the simulator drives that rejection deliberately as an
// [OpMalformed] op — see [tmplMergeParamMap] and [checkMergeRejection].
//
// # Temporal binding
//
// A temporal is bound as a genuine temporal parameter, never as an ISO-8601
// string. The engine's parameter write path
// (cypher/api.go:exprValueToLPGProp) encodes each temporal expr.Value as an
// [github.com/FlavioCFOliveira/GoGraph/graph/lpg.PropString] carrying the
// kind tag byte that cypher/exec/temporal_literal.go defines (0x01 date,
// 0x02 localdatetime, 0x03 datetime, 0x04 localtime, 0x05 time, 0x06
// duration), and the read path decodes that tag back into the matching
// temporal expr.Value. Binding the ISO-8601 STRING instead stores an
// untagged PropString that reads back as an [expr.StringValue], which is
// exactly the degradation the type-coverage checker exists to catch
// (rmp #2457) — so the mapping here is load-bearing, not a convenience.
//
// Two Go-native kinds are accepted and mapped explicitly, because a Go value
// alone cannot say which of the six Cypher temporal types was meant:
//
//   - time.Time     → [expr.DateTimeValue] (instant + zone, the faithful
//     counterpart of a time.Time; the other five types would each discard a
//     component a time.Time carries)
//   - time.Duration → [expr.DurationValue] with only the seconds and
//     nanoseconds components set, because a time.Duration has no month or
//     day stride to carry
//
// A caller needing one of the other four types (or a duration with a month or
// day stride) constructs the expr temporal value itself and binds it directly:
// the six temporal expr.Value types are passed through verbatim. Every other
// expr.Value kind is still rejected, so a workload bug surfaces loudly rather
// than being silently coerced.
func toExprValue(v any) (expr.Value, error) {
	switch t := v.(type) {
	case nil:
		return expr.Null, nil
	case string:
		return expr.StringValue(t), nil
	case int64:
		return expr.IntegerValue(t), nil
	case int:
		return expr.IntegerValue(int64(t)), nil
	case float64:
		return expr.FloatValue(t), nil
	case bool:
		return expr.BoolValue(t), nil
	case expr.DateValue, expr.LocalDateTimeValue, expr.DateTimeValue,
		expr.LocalTimeValue, expr.TimeValue, expr.DurationValue:
		// An already-typed temporal binds verbatim; the engine tags it on write.
		ev, ok := t.(expr.Value)
		if !ok { // unreachable: every case type implements expr.Value
			return nil, fmt.Errorf("temporal %T does not implement expr.Value", t)
		}
		return ev, nil
	case time.Time:
		return expr.DateTimeValue{T: t}, nil
	case time.Duration:
		return expr.NewDuration(0, 0, int64(t/time.Second), int32(t%time.Second)), nil
	case []any:
		items := make(expr.ListValue, 0, len(t))
		for i, e := range t {
			ev, err := toExprValue(e)
			if err != nil {
				return nil, fmt.Errorf("list element %d: %w", i, err)
			}
			items = append(items, ev)
		}
		return items, nil
	case map[string]any:
		m := make(expr.MapValue, len(t))
		for k, e := range t {
			ev, err := toExprValue(e)
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", k, err)
			}
			m[k] = ev
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unsupported param type %T", v)
	}
}

// canonicalValueString renders a Go value (as bound by the workload) to the same
// canonical string the engine's expr.Value yields, so the type-coverage checker
// can compare a read-back property against the oracle's modelled value across all
// supported kinds without per-type equality logic. A value that cannot be mapped
// renders as a distinctive marker so a mismatch surfaces loudly rather than
// comparing equal by accident.
func canonicalValueString(v any) string {
	ev, err := toExprValue(v)
	if err != nil {
		return fmt.Sprintf("<unmappable:%T>", v)
	}
	return ev.String()
}

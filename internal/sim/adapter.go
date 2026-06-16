package sim

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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

// toExprValue maps a single Go value to its expr.Value.
func toExprValue(v any) (expr.Value, error) {
	switch t := v.(type) {
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
	default:
		return nil, fmt.Errorf("unsupported param type %T", v)
	}
}

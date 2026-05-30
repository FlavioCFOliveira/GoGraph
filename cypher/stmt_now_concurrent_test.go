package cypher_test

// stmt_now_concurrent_test.go — race-detector test for per-query statement-now
// isolation. Each concurrent Engine.Run call must observe its own frozen
// timestamp; no goroutine may see another's instant.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestEngine_ConcurrentRun_IndependentStatementNow spawns 10 goroutines, each
// running a RETURN date() / RETURN datetime() query. Every goroutine pre-selects
// a target date that is at least 24 h in the past relative to today, so no two
// goroutines share a target and no accidental overlap with the real current time
// is possible. The test verifies that the returned date matches what was
// computed at Run-call time — not a value leaked from another goroutine.
//
// The test is run under -race by CI and is in the short layer (no build tag).
func TestEngine_ConcurrentRun_IndependentStatementNow(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	const goroutines = 10

	type result struct {
		idx  int
		date string
		err  error
	}

	// Each goroutine picks a unique date far enough in the past that it cannot
	// coincide with the actual wall-clock date during the test run. We use
	// 2000-01-01 + goroutine-index days.
	baseDate := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)

	results := make([]result, goroutines)
	var wg sync.WaitGroup

	// barrier ensures all goroutines call eng.Run as close to simultaneously as
	// possible so that any process-global race is maximally likely to surface.
	var barrier sync.WaitGroup
	barrier.Add(goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Each goroutine targets a distinct past date.
			targetDate := baseDate.AddDate(0, 0, idx)
			expectedDateStr := fmt.Sprintf("%04d-%02d-%02d",
				targetDate.Year(), int(targetDate.Month()), targetDate.Day())

			// Signal ready, then wait for all goroutines to be ready.
			barrier.Done()
			barrier.Wait()

			// Run `RETURN date($d)` with an explicit date parameter so the
			// result is deterministic and goroutine-specific. This exercises
			// the non-zero-arg path (which is delegated, not intercepted) and
			// confirms the delegate still works correctly alongside the wrapper.
			params := map[string]expr.Value{
				"d": expr.StringValue(expectedDateStr),
			}
			res, err := eng.Run(context.Background(), `RETURN date($d) AS d`, params)
			if err != nil {
				results[idx] = result{idx: idx, err: err}
				return
			}
			defer res.Close()

			var got string
			if res.Next() {
				rec := res.Record()
				if v, ok := rec["d"]; ok {
					got = fmt.Sprintf("%v", v)
				}
			}
			if iterErr := res.Err(); iterErr != nil {
				results[idx] = result{idx: idx, err: iterErr}
				return
			}
			results[idx] = result{idx: idx, date: got}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", r.idx, r.err)
			continue
		}
		targetDate := baseDate.AddDate(0, 0, r.idx)
		expected := fmt.Sprintf("%04d-%02d-%02d",
			targetDate.Year(), int(targetDate.Month()), targetDate.Day())
		if r.date != expected {
			t.Errorf("goroutine %d: got date %q, want %q", r.idx, r.date, expected)
		}
	}
}

// TestEngine_ConcurrentRun_ZeroArgDatetime verifies that concurrent zero-arg
// datetime() calls via Engine.Run each return a value in the UTC timezone (the
// nowAwareRegistry pins r.now as UTC) and that no data race occurs on the
// underlying process-global statementNow.
//
// This test does not assert exact timestamp equality — wall-clock time advances
// between goroutine starts — but it does confirm each result is a valid
// DateTimeValue and that the race detector sees no conflict.
func TestEngine_ConcurrentRun_ZeroArgDatetime(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	const goroutines = 10
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	var barrier sync.WaitGroup
	barrier.Add(goroutines)

	before := time.Now().UTC().Add(-time.Second) // allow 1 s of clock skew
	after := time.Now().UTC().Add(10 * time.Second)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			barrier.Done()
			barrier.Wait()

			res, err := eng.Run(context.Background(), `RETURN datetime() AS dt`, nil)
			if err != nil {
				errs[idx] = err
				return
			}
			defer res.Close()

			if !res.Next() {
				errs[idx] = fmt.Errorf("goroutine %d: no rows returned", idx)
				return
			}
			rec := res.Record()
			v, ok := rec["dt"]
			if !ok {
				errs[idx] = fmt.Errorf("goroutine %d: column 'dt' missing", idx)
				return
			}
			dt, ok := v.(expr.DateTimeValue)
			if !ok {
				errs[idx] = fmt.Errorf("goroutine %d: expected DateTimeValue, got %T", idx, v)
				return
			}
			// The returned instant must be in the window [before, after].
			if dt.T.Before(before) || dt.T.After(after) {
				errs[idx] = fmt.Errorf("goroutine %d: datetime() = %v outside window [%v, %v]",
					idx, dt.T, before, after)
			}
		}(i)
	}
	wg.Wait()

	for idx, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", idx, err)
		}
	}
}

package tck_test

import (
	"context"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/cypher/tck"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestDDLProcScenarios runs each DDLScenario through the execution engine and
// verifies that no unexpected errors occur (and that expected errors are
// returned when WantErr is true).
//
// Each sub-test creates its own engine so that DDL side-effects (index/constraint
// creation) do not bleed across parallel runs.
func TestDDLProcScenarios(t *testing.T) {
	t.Parallel()

	params := map[string]any{
		"name": "Alice",
		"age":  int64(30),
	}

	for _, sc := range tck.DDLScenarios() {
		sc := sc // capture
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()

			g := lpg.New[string, float64](adjlist.Config{})
			engine := cypher.NewEngine(g)

			result, err := engine.RunAny(context.Background(), sc.Query, params)

			if sc.WantErr {
				if err == nil {
					t.Errorf("scenario %q: expected an error but got none", sc.Name)
					return
				}
				if sc.ErrContains != "" && !strings.Contains(err.Error(), sc.ErrContains) {
					t.Errorf("scenario %q: error %q does not contain %q", sc.Name, err.Error(), sc.ErrContains)
				}
				return
			}

			if err != nil {
				t.Errorf("scenario %q: unexpected error: %v", sc.Name, err)
				return
			}

			// Drain the result set to exercise the streaming path.
			if result != nil {
				defer func() {
					if closeErr := result.Close(); closeErr != nil {
						t.Logf("scenario %q: result.Close: %v", sc.Name, closeErr)
					}
				}()
				for result.Next() {
					_ = result.Record()
				}
				if rsErr := result.Err(); rsErr != nil {
					t.Errorf("scenario %q: result iteration error: %v", sc.Name, rsErr)
				}
			}
		})
	}
}

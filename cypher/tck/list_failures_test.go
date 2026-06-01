package tck_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"

	"github.com/FlavioCFOliveira/GoGraph/cypher/tck"
)

// TestListTCKFailures enumerates failing scenarios with their feature URI and
// the error returned by the AfterScenario hook. Activate with
// GOGRAPH_TCK_LIST_FAILURES=1. Writes a TSV-style sorted list to stdout (and
// to the path in GOGRAPH_TCK_FAILURES_OUT if set). Used to identify the
// highest-impact failure clusters when moving the TCK baseline.
func TestListTCKFailures(t *testing.T) {
	if os.Getenv("GOGRAPH_TCK_LIST_FAILURES") == "" {
		t.Skip("set GOGRAPH_TCK_LIST_FAILURES=1 to enumerate failing scenarios")
	}

	type failure struct {
		uri, name, err string
	}
	var (
		mu            sync.Mutex
		fails, undefs []failure
	)

	init := func(sc *godog.ScenarioContext) {
		initScenario(sc)
		sc.After(func(ctx context.Context, s *godog.Scenario, err error) (context.Context, error) {
			if err == nil {
				return ctx, nil
			}
			f := failure{uri: s.Uri, name: s.Name, err: err.Error()}
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(err.Error(), "undefined") || errors.Is(err, godog.ErrUndefined) {
				undefs = append(undefs, f)
			} else {
				fails = append(fails, f)
			}
			return ctx, nil
		})
	}

	opts := &godog.Options{
		Format:        "progress",
		Paths:         []string{"features"},
		FS:            tck.FeatureFiles(),
		Strict:        false,
		StopOnFailure: false,
		Output:        io.Discard,
		NoColors:      true,
	}
	_ = godog.TestSuite{Name: "TCK failures", ScenarioInitializer: init, Options: opts}.Run()

	sortFn := func(s []failure) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].uri != s[j].uri {
				return s[i].uri < s[j].uri
			}
			return s[i].name < s[j].name
		})
	}
	sortFn(fails)
	sortFn(undefs)

	var b strings.Builder
	fmt.Fprintf(&b, "# TCK failing scenarios (%d failed, %d undefined)\n", len(fails), len(undefs))
	emit := func(kind string, list []failure) {
		for _, f := range list {
			first := f.err
			if i := strings.IndexByte(first, '\n'); i >= 0 {
				first = first[:i]
			}
			if len(first) > 220 {
				first = first[:220] + "…"
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", kind, f.uri, f.name, first)
		}
	}
	emit("FAIL", fails)
	emit("UNDEF", undefs)

	if path := os.Getenv("GOGRAPH_TCK_FAILURES_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write failures file: %v", err)
		}
		t.Logf("wrote %d failures + %d undefined to %s", len(fails), len(undefs), path)
	} else {
		fmt.Print(b.String())
	}
}

// TestListTCKFailuresDetail runs a single feature file with rich per-scenario
// detail (full step list incl. doc strings / data tables) so the offending
// sub-row of a Scenario Outline can be identified. Activate with
// GOGRAPH_TCK_LIST_FAILURES_DETAIL=<feature-path>.
func TestListTCKFailuresDetail(t *testing.T) {
	target := os.Getenv("GOGRAPH_TCK_LIST_FAILURES_DETAIL")
	if target == "" {
		t.Skip("set GOGRAPH_TCK_LIST_FAILURES_DETAIL to a feature path")
	}

	type failure struct {
		uri, name, err string
		steps          []string
	}
	var (
		mu    sync.Mutex
		fails []failure
	)

	init := func(sc *godog.ScenarioContext) {
		initScenario(sc)
		sc.After(func(ctx context.Context, s *godog.Scenario, err error) (context.Context, error) {
			if err == nil {
				return ctx, nil
			}
			steps := make([]string, 0, len(s.Steps))
			for _, st := range s.Steps {
				txt := st.Text
				if st.Argument != nil {
					if st.Argument.DocString != nil {
						txt += "\n      " + strings.ReplaceAll(st.Argument.DocString.Content, "\n", "\n      ")
					}
					if st.Argument.DataTable != nil {
						for _, row := range st.Argument.DataTable.Rows {
							cells := make([]string, 0, len(row.Cells))
							for _, c := range row.Cells {
								cells = append(cells, c.Value)
							}
							txt += "\n      | " + strings.Join(cells, " | ") + " |"
						}
					}
				}
				steps = append(steps, txt)
			}
			f := failure{uri: s.Uri, name: s.Name, err: err.Error(), steps: steps}
			mu.Lock()
			defer mu.Unlock()
			fails = append(fails, f)
			return ctx, nil
		})
	}

	opts := &godog.Options{
		Format:        "progress",
		Paths:         []string{target},
		FS:            tck.FeatureFiles(),
		Strict:        false,
		StopOnFailure: false,
		Output:        io.Discard,
		NoColors:      true,
	}
	_ = godog.TestSuite{Name: "TCK detail", ScenarioInitializer: init, Options: opts}.Run()

	sort.Slice(fails, func(i, j int) bool {
		if fails[i].name != fails[j].name {
			return fails[i].name < fails[j].name
		}
		return fails[i].err < fails[j].err
	})

	var b strings.Builder
	fmt.Fprintf(&b, "# %d failures in %s\n\n", len(fails), target)
	for i, f := range fails {
		fmt.Fprintf(&b, "## %d — %s\n", i+1, f.name)
		fmt.Fprintf(&b, "ERR: %s\n", f.err)
		fmt.Fprintf(&b, "STEPS:\n")
		for _, s := range f.steps {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
		fmt.Fprintln(&b)
	}
	if path := os.Getenv("GOGRAPH_TCK_FAILURES_DETAIL_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("wrote %d failures to %s", len(fails), path)
	} else {
		fmt.Print(b.String())
	}
}

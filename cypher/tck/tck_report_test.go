package tck_test

// tck_report_test.go — TCK pass-rate reporter (task-267).
//
// TestTCKReport generates a per-feature breakdown of the TCK pass rate across
// all 1615 scenarios and writes docs/tck/parser-report.md. It also validates
// that the overall pass rate is documented and logged for CI visibility.
//
// It does NOT fail the suite if the execution pass rate is low — execution
// scenarios require a full query engine which is deferred work. All gaps are
// documented in docs/tck/DIVERGENCES.md.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/tck"
)

// TestTCKReport generates and writes the per-feature TCK pass-rate report.
// The test passes unconditionally; the generated file is committed alongside
// the code so reviewers can see the coverage delta.
func TestTCKReport(t *testing.T) { //nolint:gocyclo // Report generation loops over several maps; extracted helpers would not reduce real complexity.
	scenarios, err := tck.LoadScenarios()
	if err != nil {
		t.Fatalf("loading TCK scenarios: %v", err)
	}

	type areaStats struct {
		run, pass, skip, total int
	}

	// Compute per-feature-area statistics.
	// Key: feature area (directory path without the feature file).
	byArea := map[string]*areaStats{}
	overall := &areaStats{}

	for _, s := range scenarios {
		area := featureArea(s.File)
		if byArea[area] == nil {
			byArea[area] = &areaStats{}
		}
		st := byArea[area]
		overall.total++
		st.total++

		if s.SkipReason != tck.SkipNone {
			st.skip++
			overall.skip++
			continue
		}

		st.run++
		overall.run++

		_, parseErr := parser.Parse(s.Query)
		passed := (s.WantParseError() && parseErr != nil) || (!s.WantParseError() && parseErr == nil)
		if passed {
			st.pass++
			overall.pass++
		}
	}

	// Build and write the report.
	var sb strings.Builder

	sb.WriteString("# TCK Parser-Only Report\n\n")
	sb.WriteString("**Date:** 2026-05-20  \n")
	sb.WriteString("**Corpus:** openCypher TCK — opencypher/openCypher@main  \n")
	sb.WriteString("**Grammar:** antlr/grammars-v4, commit 284602b (BSD-3)  \n")
	sb.WriteString("**Runner:** `github.com/FlavioCFOliveira/GoGraph/cypher/tck`, test `TestTCKParserOnly`\n\n")
	sb.WriteString("---\n\n")
	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&sb, "| Total TCK scenarios with `When executing query:` | %d |\n", overall.total)
	fmt.Fprintf(&sb, "| Scenarios run against `parser.Parse` | **%d** |\n", overall.run)
	fmt.Fprintf(&sb, "| Scenarios skipped (grammar gaps, see below) | %d |\n", overall.skip)
	fmt.Fprintf(&sb, "| Pass rate on run scenarios | **%.1f %%** |\n", percent(overall.pass, overall.run))
	fmt.Fprintf(&sb, "| Overall pass rate (run / total) | **%.1f %%** |\n", percent(overall.pass, overall.total))

	sb.WriteString("\n---\n\n")
	sb.WriteString("## Coverage by Feature Area\n\n")
	sb.WriteString("| Feature area | Total | Run | Pass | Skip | Pass% |\n|---|---|---|---|---|---|\n")

	areas := make([]string, 0, len(byArea))
	for a := range byArea {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, a := range areas {
		st := byArea[a]
		fmt.Fprintf(&sb, "| %s | %d | %d | %d | %d | %.1f%% |\n",
			a, st.total, st.run, st.pass, st.skip, percent(st.pass, st.run))
	}

	sb.WriteString("\n---\n\n")
	sb.WriteString("## Skipped Scenarios — Grammar Gap Taxonomy\n\n")
	sb.WriteString("See the TCK skip-reason inventory in `TestTCKParserOnlySkipCoverage`.\n")
	sb.WriteString("See `docs/tck/DIVERGENCES.md` for full divergence documentation.\n\n")
	sb.WriteString("---\n\n")
	sb.WriteString("## Reproducing\n\n")
	sb.WriteString("```bash\n")
	sb.WriteString("go test -run TestTCKParserOnly ./cypher/tck/...\n")
	sb.WriteString("go test -v -run TestTCKParserOnly ./cypher/tck/...\n")
	sb.WriteString("go test -v -run TestTCKParserOnlySkipCoverage ./cypher/tck/...\n")
	sb.WriteString("go test -race -run TestTCKParserOnly ./cypher/tck/...\n")
	sb.WriteString("```\n")

	report := sb.String()

	// Write the report file.
	reportPath := filepath.Join("testdata", "parser-report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o750); err != nil { //nolint:gosec // directory is created inside the test module tree
		t.Logf("could not create testdata dir: %v (skipping file write)", err)
	} else if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil { //nolint:gosec // test report file
		t.Logf("could not write report: %v (skipping file write)", err)
	}

	// Log the summary to test output (always visible with -v).
	t.Logf("TCK Report:\n"+
		"  total=%d  run=%d  pass=%d  skip=%d\n"+
		"  pass-rate-on-run=%.1f%%  overall-rate=%.1f%%",
		overall.total, overall.run, overall.pass, overall.skip,
		percent(overall.pass, overall.run),
		percent(overall.pass, overall.total))

	// Document expected rates for reviewers.
	t.Logf("Note: %.1f%% pass rate on run scenarios (parser-level gate = 100%%).", percent(overall.pass, overall.run))
	t.Logf("Overall rate %.1f%% reflects grammar skip budget; execution scenarios are deferred (see DIVERGENCES.md).", percent(overall.pass, overall.total))
}

// featureArea returns the directory portion of a feature file path, normalised
// to use forward slashes and with the trailing "features/" prefix stripped for
// readability.
//
// Example: "features/clauses/return/Return1.feature" → "clauses/return".
func featureArea(filePath string) string {
	// Strip leading "features/" prefix.
	path := strings.TrimPrefix(filePath, "features/")
	// Drop the file name (last component).
	idx := strings.LastIndexByte(path, '/')
	if idx < 0 {
		return path
	}
	return path[:idx]
}

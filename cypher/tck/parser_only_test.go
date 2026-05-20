package tck_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"gograph/cypher/parser"
	"gograph/cypher/tck"
)

// TestTCKParserOnly is the CI gate for grammar coverage.
//
// It runs every openCypher TCK scenario that is not excluded by a documented
// grammar-gap skip condition through [parser.Parse] and asserts:
//
//   - Scenarios with no expected error parse without error.
//   - Scenarios with a parse-time SyntaxError type return a non-nil error.
//
// The test fails if the pass rate across all non-skipped scenarios drops below
// 100 %, blocking CI on any regression.
func TestTCKParserOnly(t *testing.T) { //nolint:gocyclo // Test accumulates stats over three loops (skip/run/summary); extracting loops would reduce readability without reducing real complexity.
	scenarios, err := tck.LoadScenarios()
	if err != nil {
		t.Fatalf("loading TCK scenarios: %v", err)
	}

	type featureStat struct {
		run, pass, skip int
	}
	stats := map[string]*featureStat{}

	var totalRun, totalPass, totalSkip int

	for _, s := range scenarios {
		if stats[s.File] == nil {
			stats[s.File] = &featureStat{}
		}
		st := stats[s.File]

		if s.SkipReason != tck.SkipNone {
			st.skip++
			totalSkip++
			continue
		}

		st.run++
		totalRun++

		s := s // capture
		t.Run(fmt.Sprintf("%s/%s", s.File, s.Name), func(t *testing.T) {
			t.Parallel()
			_, parseErr := parser.Parse(s.Query)
			if s.WantParseError() {
				if parseErr == nil {
					t.Errorf("expected parse error (%s) for query %q but got none",
						s.SyntaxErrorType, s.Query)
				}
			} else {
				if parseErr != nil {
					t.Errorf("unexpected parse error for query %q: %v", s.Query, parseErr)
				}
			}
		})
	}

	// Accumulate pass/fail counts from a second, non-parallel pass so that we
	// can emit a human-readable summary even when t.Run sub-tests are run in
	// parallel.  This adds ~1 ms of overhead for 1 092 scenarios.
	for _, s := range scenarios {
		if s.SkipReason != tck.SkipNone {
			continue
		}
		st := stats[s.File]
		_, parseErr := parser.Parse(s.Query)
		if (s.WantParseError() && parseErr != nil) || (!s.WantParseError() && parseErr == nil) {
			st.pass++
			totalPass++
		}
	}

	// Print per-file summary — visible with go test -v.
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		st := stats[k]
		if st.run > 0 {
			t.Logf("%-70s  run=%4d  pass=%4d  skip=%4d", k, st.run, st.pass, st.skip)
		}
	}

	t.Logf("TOTAL  run=%d  pass=%d  skip=%d  (%.1f%% pass rate on run scenarios)",
		totalRun, totalPass, totalSkip, percent(totalPass, totalRun))

	// Enforce 100 % pass rate on run scenarios.
	if totalPass < totalRun {
		t.Errorf("TCK parser pass rate below 100%%: %d/%d passed (%.1f%%)",
			totalPass, totalRun, percent(totalPass, totalRun))
	}
}

// TestTCKParserOnlySkipCoverage documents the skip-reason taxonomy and counts.
// It does not enforce any threshold; its purpose is to make the current skip
// inventory visible so regressions in skip counts are noticed during code review.
func TestTCKParserOnlySkipCoverage(t *testing.T) {
	scenarios, err := tck.LoadScenarios()
	if err != nil {
		t.Fatalf("loading TCK scenarios: %v", err)
	}

	byReason := map[string]int{}
	total := 0

	for _, s := range scenarios {
		total++
		reason := string(s.SkipReason)
		if reason == "" {
			reason = "(run)"
		}
		byReason[reason]++
	}

	reasons := make([]string, 0, len(byReason))
	for r := range byReason {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i] == "(run)" {
			return true
		}
		if reasons[j] == "(run)" {
			return false
		}
		return reasons[i] < reasons[j]
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "\nTCK skip-reason inventory (%d total scenarios):\n", total)
	for _, r := range reasons {
		fmt.Fprintf(&sb, "  %-30s  %d\n", r, byReason[r])
	}
	t.Log(sb.String())
}

func percent(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) * 100 / float64(d)
}

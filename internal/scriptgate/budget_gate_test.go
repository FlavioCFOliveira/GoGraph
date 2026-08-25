package scriptgate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// hardBudgetRe matches the Makefile's global hard-ceiling assignment. It is
// anchored at the start of a line so it cannot be satisfied by
// PKG_HARD_BUDGET_OVERRIDES, which also contains the string "HARD_BUDGET".
var hardBudgetRe = regexp.MustCompile(`(?m)^HARD_BUDGET\s*\??=\s*([0-9.]+)`)

// overridesRe captures the value of PKG_HARD_BUDGET_OVERRIDES.
var overridesRe = regexp.MustCompile(`(?m)^PKG_HARD_BUDGET_OVERRIDES\s*\??=\s*(.*)$`)

// TestShortLayerBudgetIsEnforcedOnTheRoutineGate guards rmp #2577 and #2599:
// the per-package short-layer cost budget must be read by the gate every change
// actually takes, not by a target nobody runs.
//
// Both assertions fail on the tree this fixed, which is what makes them a
// regression test rather than a description:
//
//   - `test-short` did not pipe through scripts/pkg_time_budget.sh at all. The
//     script was reachable only from `test-short-timings`, which `make ci` does
//     not invoke, so the documented ceiling was enforced by nothing while the
//     package drifted — internal/sim grew from 818 tests / 460.9 s on
//     2026-08-20 to 974 tests / 570.1 s on 2026-08-25, unremarked.
//   - `HARD_BUDGET` defaulted to 0, which scripts/pkg_time_budget.sh documents
//     as DISABLED. A ceiling of zero is not a strict ceiling, it is no ceiling,
//     and it reported success in exactly the same way as a passing one.
func TestShortLayerBudgetIsEnforcedOnTheRoutineGate(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")

	// 1. The budget script sits on the routine path, i.e. inside `test-short`'s
	//    own recipe — the target `ci` depends on. Checked against THIS recipe,
	//    not the whole file, so moving the script back to some other target
	//    fails here instead of passing on a file-wide mention.
	recipe, ok := makeRecipeFor(makefile, "test-short")
	if !ok {
		t.Fatalf("the Makefile no longer defines a `test-short` target")
	}
	if !strings.Contains(recipe, "scripts/pkg_time_budget.sh") {
		t.Errorf("the `test-short` recipe no longer pipes through "+
			"scripts/pkg_time_budget.sh, so nothing on the `make ci` path reads the "+
			"per-package cost budget and the ceiling is decoration again "+
			"(rmp #2577, #2599). Recipe is:\n%s", recipe)
	}

	// 2. The ceiling is actually armed. `HARD_BUDGET=0` means disabled, so a
	//    zero here would leave the pipe in place and gate nothing — the failure
	//    mode this test exists to make impossible.
	m := hardBudgetRe.FindStringSubmatch(makefile)
	if m == nil {
		t.Fatalf("the Makefile no longer assigns HARD_BUDGET; the budget script " +
			"would fall back to its own default of 0, which is DISABLED")
	}
	secs, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("HARD_BUDGET is not a number: %q", m[1])
	}
	if secs <= 0 {
		t.Errorf("HARD_BUDGET = %v, which scripts/pkg_time_budget.sh documents as "+
			"DISABLED: the budget check would run and gate nothing", secs)
	}
}

// TestBudgetOverridesNameRealPackages keeps the per-package accommodations
// honest. Each override is a measured exception for ONE package; if that package
// is renamed or removed, the entry must go with it.
//
// A stale key is not inert. Keys match as a SUFFIX of the import path, so an
// entry left behind after a rename can start covering whatever package later
// happens to end with it — silently granting a ceiling nobody measured. This
// asserts every key still names a directory that exists.
func TestBudgetOverridesNameRealPackages(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	m := overridesRe.FindStringSubmatch(makefile)
	if m == nil {
		t.Skip("PKG_HARD_BUDGET_OVERRIDES is not set; there is nothing to keep honest")
	}
	value := m[1]
	if i := strings.Index(value, "##"); i >= 0 {
		value = value[:i]
	}
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	})
	if len(fields) == 0 {
		t.Fatalf("PKG_HARD_BUDGET_OVERRIDES is assigned but empty: %q", m[1])
	}
	for _, f := range fields {
		key, val, found := strings.Cut(f, "=")
		if !found || key == "" {
			t.Errorf("override %q is not a `path-suffix=seconds` pair", f)
			continue
		}
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			t.Errorf("override %q has a non-numeric ceiling %q", f, val)
		}
		dir := filepath.Join(repoRoot(t), filepath.FromSlash(strings.TrimPrefix(key, "/")))
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("override key %q does not name an existing package directory "+
				"(%s): a stale suffix key silently grants its ceiling to whatever "+
				"package later ends with it", key, dir)
		}
	}
}

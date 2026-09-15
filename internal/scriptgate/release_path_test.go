package scriptgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readRepoFile reads a repo-relative file or fails the test.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel)) //nolint:gosec // G304: rel is a repo-relative path chosen by the caller; repoRoot(t) is located by walking up to go.mod.
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// makeRecipeFor returns the recipe body of the named Makefile target — the run of
// tab-indented lines immediately following the target line — and whether the
// target was found.
//
// Scoping the assertions to one target's recipe is the point: several recipes
// mention $(RACE_FLAGS) and $(PACKAGES), so a file-wide substring check would go
// on passing after the specific gate under test stopped using them.
func makeRecipeFor(makefile, target string) (string, bool) {
	lines := strings.Split(makefile, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, target+":") {
			continue
		}
		var body []string
		for _, r := range lines[i+1:] {
			// A recipe is the consecutive tab-prefixed lines; the first line that is
			// not tab-prefixed ends it.
			if !strings.HasPrefix(r, "\t") {
				break
			}
			body = append(body, r)
		}
		return strings.Join(body, "\n"), true
	}
	return "", false
}

// TestReleasePathsConverge guards #1444: neither release path may publish
// while bypassing the release gate.
//
// GitHub Actions runs ONLY the release workflow
// (.github/workflows/release.yml) — the per-push ci.yml/tck.yml/crash.yml
// workflows were removed. Correctness, TCK, and crash gating are no longer
// enforced by GitHub; they run LOCALLY via `make release-preflight` (and
// `make ci` before every push), which a developer MUST run before tagging.
// The two release paths are therefore:
//
//   - Tag-push path (.github/workflows/release.yml): runs only the Phase-A
//     release-accuracy gate (release-doc consistency) and then goreleaser. It
//     re-runs none of the heavy correctness gates; it relies on the developer
//     having run `make release-preflight` locally before pushing the tag.
//   - Local path (`make release`): depends on release-preflight, which folds
//     in release-accuracy and — as the correctness gate — `make ci`. The
//     gate's membership and its ORDER are the Makefile's CI_STAGES:
//     `shell-guard tidy fmt vet build ci-kg-verify vulncheck lint
//     test-uninstrumented test-timing test-short`, where `test-short` runs
//     the race detector over every package (`go test -race ./...`) and the
//     openCypher TCK execution baseline (TestTCKExecution) runs inside that
//     pass.
//
// The correctness gate therefore lives in `make ci`, not in
// scripts/pre-release.sh: since the release-gate de-duplication (commit
// af8eefc) release-preflight invokes `make ci`, and scripts/pre-release.sh is
// a standalone convenience gate that is NOT on the release path.
//
// WHAT CHANGED ON 2026-09-15, and why this test changed with it. By the user's
// decision the release path runs CORRECTNESS GATES ONLY: benchmarks left it in
// v0.14.2 and `cover-gate` left it here. This test previously required
// `cover-gate` to be a member, which encoded the older mandate; re-stating it
// is part of that decision, not a relaxation of this guard. Coverage is a
// quality metric, not a correctness gate — scripts/cover_gate.sh and its
// thresholds are untouched and are run deliberately via `make cover-gate`.
//
// The guard did not merely shrink. It gained the two properties the reorder
// exists to create, neither of which was asserted before: the module suite
// must appear EXACTLY ONCE (it ran twice — test-short under -race and
// cover-gate under coverage), and every seconds-scale check must PRECEDE every
// suite phase (`lint` ran tenth, so one revive:context-as-argument violation
// failed the gate after the ~11-minute race suite; measured at HEAD, the same
// violation now fails it at 18.0 s).
//
// The assertions are static (file content), not a live release run, so the
// gate is cheap and deterministic. Because no per-push CI stands behind the
// tag-push path any more, the local `make release-preflight` gate is the sole
// line of defence — so this test asserts its completeness. If a future change
// drops release-accuracy from the tag-push path, or removes release-preflight
// or the `make ci` correctness gate from the local path, this test fails,
// flagging the reintroduced bypass (#1444).
func TestReleasePathsConverge(t *testing.T) {
	// 1. Tag-push path runs the Phase-A release-accuracy gate.
	releaseYML := readRepoFile(t, ".github/workflows/release.yml")
	if !strings.Contains(releaseYML, "make release-accuracy") {
		t.Errorf("release.yml no longer runs `make release-accuracy`; the tag-push " +
			"release path would publish without the release-accuracy gate (#1444)")
	}

	// 2. Local path (`make release`) is the sole correctness gate now that no
	//    per-push CI exists. It must depend on release-preflight, which in turn
	//    must invoke `make ci` (the correctness + coverage gate).
	makefile := readRepoFile(t, "Makefile")
	if !strings.Contains(makefile, "release: release-preflight") {
		t.Errorf("Makefile `release` target no longer depends on `release-preflight`; " +
			"the local release path would bypass the canonical gate")
	}
	if !strings.Contains(makefile, "$(MAKE) ci") {
		t.Errorf("release-preflight no longer invokes `make ci`; the local release path " +
			"would publish without the correctness + coverage gate (#1444)")
	}

	// 3. `make ci` must still run every mandated gate — it is the last line of
	//    defence now that no per-push CI exists.
	//
	//    This checks that every mandated prerequisite is PRESENT, rather than
	//    matching the prerequisite list as one exact string. The exact-string form
	//    failed on an ADDITION as readily as on a removal, which is the opposite of
	//    what this gate is for: it exists to catch a mandated gate being dropped
	//    (#1444), and adding a phase strengthens the pipeline rather than weakening
	//    it. It duly went red when `test-timing` was inserted (rmp #2517) on a tree
	//    where every mandated gate was still present and running.
	//
	//    The subset check keeps all of the original power — remove any one of these
	//    prerequisites and this fails, naming it — while allowing the pipeline to
	//    grow.
	mandated := []string{
		"shell-guard",         // the -e -u -o pipefail regime the whole gate rests on (rmp #2672)
		"tidy",                // module hygiene
		"fmt",                 // formatting
		"vet",                 // static analysis
		"build",               // compiles
		"lint",                // golangci-lint
		"vulncheck",           // govulncheck, asserting analysis happened (rmp #2722)
		"ci-kg-verify",        // knowledge-graph fidelity (rmp #2677, #2796)
		"test-uninstrumented", // neither -race nor coverage: the ONLY phase that compiles
		//                        the //go:build !race files, six of whose tests run in no
		//                        other stage — two of them bounding the allocation a forged
		//                        length prefix can provoke (rmp #2709)
		"test-timing", // the serial phase in which the wall-clock gates assert (rmp #2517)
		"test-short",  // the race pass, in which the openCypher TCK baseline runs
	}
	// `cover-gate` is DELIBERATELY ABSENT — see this function's docstring.
	ciLine := ciStages(makefile)
	if ciLine == "" {
		t.Errorf("could not find a CI_STAGES assignment in the Makefile; `make ci` has no " +
			"readable stage list, so the release path has no canonical gate (#1444)")
	}
	for _, gate := range mandated {
		if !prereqPresent(ciLine, gate) {
			t.Errorf("CI_STAGES no longer runs %q; a mandated correctness gate would be "+
				"skipped on the release path (#1444). Current stages: %q", gate, ciLine)
		}
	}

	// 3a. The whole-module suite must appear EXACTLY ONCE.
	//
	// This is the user's requirement stated as an assertion: when no correction
	// is needed, the tests run once. Until 2026-09-15 they ran TWICE on every
	// green gate — `test-short` under `-race` and `cover-gate` under
	// `-coverpkg=./... -covermode=atomic` — the same corpus, differently
	// instrumented. A requirement that lives only in a decision decays the
	// moment someone adds a second whole-suite stage for a good local reason;
	// asserted here, it cannot.
	suiteStages := 0
	for _, s := range strings.Fields(ciLine) {
		if wholeModuleSuiteStage(s) {
			suiteStages++
		}
	}
	if suiteStages != 1 {
		t.Errorf("CI_STAGES runs the whole-module suite %d times, want exactly 1; the gate "+
			"must execute the corpus ONCE when no correction is needed. Stages: %q",
			suiteStages, ciLine)
	}

	// 3b. FAIL CHEAP: every seconds-scale check must precede every test phase.
	//
	// This is the property the 2026-09-15 reordering exists to create, and it is
	// the one that decays silently — a stage appended to the end of the list
	// looks harmless and is not. Measured on the reference host: the cheap block
	// costs ~20 s in total, `test-short` alone 682.6 s. Until the reorder `lint`
	// ran TENTH, so a single `revive: context-as-argument` violation in one file
	// failed the gate AFTER the race suite and two further stages never ran at
	// all; that is what publishing v0.14.2 paid for. The same violation, injected
	// at HEAD, now fails the gate at 18.0 s.
	stages := strings.Fields(ciLine)
	firstTest := -1
	for i, s := range stages {
		if testPhaseStage(s) {
			firstTest = i
			break
		}
	}
	if firstTest < 0 {
		t.Errorf("CI_STAGES contains no test phase at all; the gate would publish without "+
			"running a single test (#1444). Stages: %q", ciLine)
	} else {
		for i, s := range stages {
			if i > firstTest && !testPhaseStage(s) {
				t.Errorf("CI_STAGES runs the seconds-scale check %q at position %d, AFTER the "+
					"first test phase %q at position %d. The gate must fail cheap: every check "+
					"that concludes in seconds precedes every suite phase, so a lint or vet "+
					"failure costs seconds and not the whole suite. Stages: %q",
					s, i, stages[firstTest], firstTest, ciLine)
			}
		}
	}

	// 4. `test-short` (the gate `make ci` runs) must exercise the race detector
	//    over every package — this is also the pass in which the openCypher TCK
	//    execution baseline runs.
	if !strings.Contains(makefile, ":= -race") {
		t.Errorf("RACE_FLAGS is no longer `-race`; the release gate would run the test " +
			"suite without the race detector (#1444)")
	}
	// Matched TOKEN BY TOKEN against the `test-short` recipe, not against one
	// literal command string. The recipe legitimately grows flags over time — it
	// gained `-timeout=$(SHORT_TIMEOUT)` for rmp #2584 — and a whole-line literal
	// turns every such addition into a false failure here, which is how this guard
	// broke. Each token is still required INDIVIDUALLY and is looked for inside
	// THIS target's recipe only, so dropping `$(RACE_FLAGS)`, `-count=1` or
	// `$(PACKAGES)` still fires. This is deliberately not a loose search of the
	// whole file: `$(RACE_FLAGS)` appears in other recipes too, so a file-wide
	// substring check would keep passing after test-short stopped using it.
	recipe, ok := makeRecipeFor(makefile, "test-short")
	if !ok {
		t.Errorf("the Makefile no longer defines a `test-short` target; the release gate " +
			"would skip the race/TCK test pass (#1444)")
	} else {
		for _, want := range []string{"$(GO) test", "$(RACE_FLAGS)", "-count=1", "$(PACKAGES)"} {
			if !strings.Contains(recipe, want) {
				t.Errorf("the `test-short` recipe no longer contains %q, so it no longer runs "+
					"`go test -race ... ./...`; the release gate would skip the race/TCK test "+
					"pass (#1444). Recipe is:\n%s", want, recipe)
			}
		}
	}

	// 5. The openCypher TCK execution baseline must run in the short layer, so
	//    `test-short`'s `./...` pass includes it. Assert the test exists and
	//    carries no build constraint that would exclude it from the default
	//    build (a constraint would sit above the package clause).
	tckRunner := readRepoFile(t, "cypher/tck/runner_test.go")
	if !strings.Contains(tckRunner, "func TestTCKExecution(") {
		t.Errorf("cypher/tck/runner_test.go no longer defines TestTCKExecution; the " +
			"release gate would publish without the TCK execution baseline (#1444)")
	}
	header := tckRunner
	if idx := strings.Index(tckRunner, "\npackage "); idx >= 0 {
		header = tckRunner[:idx]
	}
	if strings.Contains(header, "//go:build") {
		t.Errorf("cypher/tck/runner_test.go carries a build tag; TestTCKExecution would be " +
			"excluded from the default `test-short ./...` pass and the release gate would " +
			"skip the TCK baseline (#1444)")
	}
}

// ciStages returns the ordered `make ci` stage list: the value of the Makefile's
// CI_STAGES assignment, which is where the gate's membership AND its order are
// written down. The `ci:` target itself is a recipe that hands that list to
// scripts/ci_stages.sh, so it carries no prerequisites to read.
//
// The comment strip matters: CI_STAGES is assigned with `?=` and the line may
// carry a trailing `# …`, which would otherwise be parsed as stage names.
func ciStages(makefile string) string {
	for _, line := range strings.Split(makefile, "\n") {
		rest, ok := strings.CutPrefix(line, "CI_STAGES")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " \t")
		for _, op := range []string{"?=", ":=", "="} {
			after, found := strings.CutPrefix(rest, op)
			if !found {
				continue
			}
			if i := strings.Index(after, "#"); i >= 0 {
				after = after[:i]
			}
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// wholeModuleSuiteStage reports whether a stage runs `go test` over the WHOLE
// module (`./...`), as opposed to a named subset.
//
// Named explicitly rather than pattern-matched on "test-": `test-timing` and
// `test-uninstrumented` are subset runs that exist for measurement-validity
// reasons (a quiet machine; neither instrumentation), and counting them as
// whole-suite passes would make the exactly-once assertion fire on a correct
// gate. Any future whole-suite stage must be added here, which is the point —
// adding one is exactly the change this assertion exists to notice.
func wholeModuleSuiteStage(stage string) bool {
	switch stage {
	case "test-short", "test-soak", "test-nightly", "test-nightly-ci", "cover-gate", "race", "test":
		return true
	}
	return false
}

// testPhaseStage reports whether a stage runs tests at all, as opposed to being
// a static check that concludes in seconds. Used by the fail-cheap ordering
// assertion.
func testPhaseStage(stage string) bool {
	return wholeModuleSuiteStage(stage) || strings.HasPrefix(stage, "test-")
}

// makefileTargetPrereqs was removed on 2026-09-15. It read the prerequisite
// list of a named Make target, and its only caller read `ci:`. `ci` is now a
// recipe target that hands CI_STAGES to scripts/ci_stages.sh and carries no
// prerequisites, so the helper had no remaining caller and `unused` — enabled
// in .golangci.yml — would have failed the gate on it. ciStages above replaces
// it for the one job that survives.

// prereqPresent reports whether gate appears as a WHOLE entry in the
// space-separated list, so `test-short` is not satisfied by `test-shortcut` and
// `ci` is not satisfied by `ci-soak`. Used against CI_STAGES.
func prereqPresent(prereqs, gate string) bool {
	for _, f := range strings.Fields(prereqs) {
		if f == gate {
			return true
		}
	}
	return false
}

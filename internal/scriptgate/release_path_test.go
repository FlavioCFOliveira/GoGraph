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
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
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
// workflows were removed. Correctness, coverage, TCK, and crash gating are no
// longer enforced by GitHub; they run LOCALLY via `make release-preflight`
// (and `make ci` before every push), which a developer MUST run before
// tagging. The two release paths are therefore:
//
//   - Tag-push path (.github/workflows/release.yml): runs only the Phase-A
//     release-accuracy gate (release-doc consistency) and then goreleaser. It
//     re-runs none of the heavy correctness gates; it relies on the developer
//     having run `make release-preflight` locally before pushing the tag.
//   - Local path (`make release`): depends on release-preflight, which folds
//     in release-accuracy, the headline bench, and — as the correctness AND
//     coverage gate — `make ci`. `make ci` is
//     `tidy fmt vet build test-short lint cover-gate`, where `test-short`
//     runs the race detector over every package (`go test -race ./...`) and
//     the openCypher TCK execution baseline (TestTCKExecution) runs inside
//     that pass.
//
// The correctness gate therefore lives in `make ci`, not in
// scripts/pre-release.sh: since the release-gate de-duplication (commit
// af8eefc) release-preflight invokes `make ci`, and scripts/pre-release.sh is
// a standalone, no-coverage convenience gate that is NOT on the release path.
//
// The assertions are static (file content), not a live release run, so the
// gate is cheap and deterministic. Because no per-push CI stands behind the
// tag-push path any more, the local `make release-preflight` gate is the sole
// line of defence — so this test asserts its completeness. If a future change
// drops release-accuracy from the tag-push path, or removes release-preflight
// or the `make ci` correctness/coverage gate from the local path, this test
// fails, flagging the reintroduced bypass (#1444).
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
	//    defence now that no per-push CI exists. Assert its composition:
	//    vet + build + the race/TCK test pass + lint + the coverage gate.
	if !strings.Contains(makefile, "ci: tidy fmt vet build test-short lint cover-gate") {
		t.Errorf("the `ci` target no longer runs the full `tidy fmt vet build test-short " +
			"lint cover-gate` pipeline; a mandated correctness or coverage gate would be " +
			"skipped on the release path (#1444)")
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

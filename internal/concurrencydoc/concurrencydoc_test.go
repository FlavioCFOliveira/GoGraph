package concurrencydoc

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// baselineMax is the maximum number of exported types (across the six public
// trees scanned by Scan) that may carry NO concurrency clause. It is the
// RATCHET: it was measured immediately after task #1400 documented the 15
// heavy-hitter types, and it must only ever be lowered.
//
// Lowering it: when you document one or more of the remaining undocumented
// types (run TestConcurrencyDocReport to list them), re-measure and set
// baselineMax to the new, smaller count in the same change.
//
// Raising it is forbidden. A new exported type with no concurrency clause must
// be documented (state whether it is safe for concurrent use), not waved past
// by bumping this constant. The CLAUDE.md mandate is explicit: "Every exported
// type carries a godoc clause stating whether it is safe for concurrent use;
// ambiguity is a defect."
const baselineMax = 33

// concurrencyAllowlist names exported types that are exempt from the scan:
// pure-data value types for which a concurrency clause would add no
// information beyond what the type's shape already conveys. KEEP THIS LIST
// MINIMAL: every entry must be a genuine value type with no methods that
// mutate shared state, and must carry an inline justification. Prefer
// documenting a type over exempting it — the allowlist is an escape hatch, not
// a substitute for the mandate. It is empty by default: the ratchet
// (baselineMax) is the primary mechanism, and an empty allowlist keeps the
// gate honest.
var concurrencyAllowlist = map[string]struct{}{
	// (intentionally empty — see the doc comment above)
}

// targetTypes lists the 15 heavy-hitter types that task #1400 required to
// carry an explicit concurrency clause. The gate sub-test below asserts each
// is classified as documented; before the clauses were added these failed,
// which is what makes this a genuine fail-before / pass-after gate.
var targetTypes = []string{
	"graph/query.Engine",
	"graph/query.Pattern",
	"store/txn.Tx",
	"graph/index.Subscriber",
	"store/recovery.Result",
	"store/recovery.Options",
	"cypher.EngineOptions",
	"bolt/server.Options",
	"store/checkpoint.Config",
	"store/checkpoint.Stats",
	"cypher/exec.Eager",
	"cypher/expr.NodeValue",
	"cypher/expr.RelationshipValue",
	"cypher/expr.PathValue",
	"graph/lpg.PropertyValue",
}

// scanRepo runs the scanner from the test's working directory (its own
// package dir under `go test`), walking up to the repository root first.
func scanRepo(t *testing.T) *ScanResult {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := RepoRoot(wd)
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	res, err := Scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res
}

// undocumentedAfterAllowlist returns the exported types that are neither
// documented for concurrency nor on the allowlist, sorted by qualified name.
func undocumentedAfterAllowlist(res *ScanResult) []string {
	var out []string
	for _, ti := range res.Undocumented() {
		q := ti.Qualified()
		if _, ok := concurrencyAllowlist[q]; ok {
			continue
		}
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

// TestConcurrencyDocRatchet is the CI gate. It fails if the number of
// undocumented exported types rises above baselineMax — the ratchet that
// drives the concurrency-contract debt monotonically downward.
func TestConcurrencyDocRatchet(t *testing.T) {
	res := scanRepo(t)

	for _, s := range res.SkippedDirs {
		t.Logf("scan skipped: %s", s)
	}

	undoc := undocumentedAfterAllowlist(res)
	if len(undoc) > baselineMax {
		t.Errorf(
			"undocumented exported types = %d, exceeds baselineMax = %d.\n"+
				"A new exported type carries no concurrency clause. The CLAUDE.md mandate "+
				"requires every exported type to state whether it is safe for concurrent use.\n"+
				"FIX: add a concurrency clause to the new type(s) below (state the real "+
				"contract: 'safe for concurrent use', 'NOT safe for concurrent use', "+
				"'immutable, so safe for concurrent reads', etc.).\n"+
				"Do NOT raise baselineMax to pass this test.\n"+
				"Undocumented types:\n  %s",
			len(undoc), baselineMax, strings.Join(undoc, "\n  "),
		)
	}

	// Sanity: the gate must actually be scanning the trees, never silently
	// finding nothing (e.g. a layout change that breaks the walk).
	if len(res.Types) == 0 {
		t.Fatal("scan found zero exported types; the scanner is misconfigured")
	}
}

// TestConcurrencyDocRatchetIsTight guards the ratchet itself: if the
// undocumented count has dropped well below baselineMax, the baseline should
// be lowered to lock in the gain. This is a soft reminder (it fails loudly so
// the gain is not silently lost), not a behavioural requirement of the module.
func TestConcurrencyDocRatchetIsTight(t *testing.T) {
	res := scanRepo(t)
	undoc := undocumentedAfterAllowlist(res)
	if len(undoc) < baselineMax {
		t.Errorf(
			"undocumented exported types = %d is below baselineMax = %d.\n"+
				"Documentation improved: lower baselineMax to %d to lock in the gain "+
				"(the ratchet only ever goes down).",
			len(undoc), baselineMax, len(undoc),
		)
	}
}

// TestTargetTypesDocumented is the fail-before / pass-after gate for the 15
// heavy-hitter types that task #1400 required to be documented. Before the
// clauses were added, several of these were classified undocumented and this
// test failed; after the fix, every one is documented and it passes.
func TestTargetTypesDocumented(t *testing.T) {
	res := scanRepo(t)
	for _, q := range targetTypes {
		t.Run(q, func(t *testing.T) {
			ti, ok := res.Lookup(q)
			if !ok {
				t.Fatalf("target type %q was not found by the scan; "+
					"it may have been renamed or moved — update targetTypes", q)
			}
			if !ti.Documented {
				t.Errorf("target type %q carries no concurrency clause; "+
					"add one stating its real concurrency contract", q)
			}
		})
	}
}

// TestConcurrencyDocReport prints the current undocumented set. It never
// fails: it is a developer aid for lowering baselineMax. Run it with -v.
func TestConcurrencyDocReport(t *testing.T) {
	res := scanRepo(t)
	undoc := undocumentedAfterAllowlist(res)
	t.Logf("exported types scanned: %d", len(res.Types))
	t.Logf("documented for concurrency: %d", len(res.Documented()))
	t.Logf("undocumented (after allowlist): %d (baselineMax = %d)", len(undoc), baselineMax)
	for _, q := range undoc {
		t.Logf("  undocumented: %s", q)
	}
}

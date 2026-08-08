package exec

// constraint_choke_point_test.go — the structural gate on UNIQUE enforcement
// (rmp #2358).
//
// The motivating property of #2358 is not tidiness: it is that a NEW write site
// cannot silently skip or duplicate constraint enforcement. Semantics alone cannot
// give that property, because nothing in the type system forces a new operator to
// call anything. A source-level assertion can, and this is it.
//
// It is deliberately a test over the package's own source rather than a lint rule,
// so it travels with the code, fails in the same run as everything else, and needs
// no external configuration to stay true.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordPropertySetAllowedFile is the ONE file permitted to call
// [ConstraintRegistry.RecordPropertySet].
//
// There it is the journaled inverse of a release: a rolled-back release must put
// the value back. Everywhere else it is dead weight AND a lock acquisition — see the
// doc comment on RecordPropertySet, and rmp #2357, which verified site by site that
// all eleven former write-path callers had already reserved the identical
// (labels, prop, value) through reserveConstraintValue, which inserts.
const recordPropertySetAllowedFile = "constraint_journal.go"

// TestUniqueEnforcement_RecordPropertySetHasOneCaller fails if a write path calls
// RecordPropertySet.
//
// WHY A GATE AND NOT A COMMENT. The eleven calls this replaces were each individually
// harmless-looking — "record what was just written" reads like bookkeeping, not like a
// second acquisition of a mutex that measured 57 % of all lock delay at sixteen
// writers. A reviewer will not catch the twelfth. The gate will.
//
// If a genuinely new inverse needs it, add that file to the allowlist ALONG WITH the
// reasoning, rather than deleting this test.
func TestUniqueEnforcement_RecordPropertySetHasOneCaller(t *testing.T) {
	offenders := map[string]int{}
	for _, path := range packageGoFiles(t) {
		base := filepath.Base(path)
		if base == recordPropertySetAllowedFile || strings.HasSuffix(base, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // a fixed set of files in this package
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(src)
		// The declaration itself lives in constraints.go and is not a call.
		body = strings.ReplaceAll(body, "func (r *ConstraintRegistry) RecordPropertySet(", "")
		if n := strings.Count(body, "RecordPropertySet("); n > 0 {
			offenders[base] = n
		}
	}
	// Comments naming the function are fine and are expected — the doc comment on
	// RecordPropertySet explains the rule — so only files with a CALL count.
	delete(offenders, "constraints.go")
	if len(offenders) != 0 {
		t.Errorf("RecordPropertySet is called outside %s: %v\n"+
			"A write path must reserve through reserveConstraintValue and nothing else: the "+
			"reserve already inserts the value AND journals its release, so a following "+
			"RecordPropertySet is an idempotent no-op that takes the registry's write lock a "+
			"second time. See rmp #2357.",
			recordPropertySetAllowedFile, offenders)
	}
}

// TestUniqueEnforcement_EveryReserveIsJournalled fails if a write path reaches
// ConstraintRegistry.ReserveSetProperty directly instead of through
// reserveConstraintValue.
//
// The wrapper is what journals the release inverse, so a direct call reserves a value
// that a rollback will never give back — the phantom failure mode #1342 records. This
// is the same class of gate as above, pointed at the other half of the pair.
func TestUniqueEnforcement_EveryReserveIsJournalled(t *testing.T) {
	offenders := map[string]int{}
	for _, path := range packageGoFiles(t) {
		base := filepath.Base(path)
		// The wrapper itself is the one legitimate caller; the declaration lives in
		// constraints.go.
		if base == "constraint_journal.go" || base == "constraints.go" || strings.HasSuffix(base, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // a fixed set of files in this package
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if n := strings.Count(string(src), "ReserveSetProperty("); n > 0 {
			offenders[base] = n
		}
	}
	if len(offenders) != 0 {
		t.Errorf("ReserveSetProperty is called outside constraint_journal.go: %v\n"+
			"Reserve through reserveConstraintValue, which journals the release inverse. A "+
			"direct reserve is not given back on rollback, which leaves a value reserved that "+
			"no live node holds.", offenders)
	}
}

// packageGoFiles lists this package's non-generated .go files.
func packageGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) < 10 {
		t.Fatalf("only %d .go files found in this package; the gate is reading the wrong "+
			"directory and would pass vacuously", len(out))
	}
	return out
}

package sim

import (
	"context"
	"strings"
	"testing"
)

// TestCrossRelease_InProcessHelpersShort is the short-layer smoke for the
// cross-release harness's pure, in-process helper code: op-stream generation,
// param JSON normalisation, the current-engine signature replay, the divergence
// classifier, the result renderers, and the small string/git seams. It
// deliberately avoids the subprocess worktree build (the only genuinely slow
// part, gated to the soak lane in crossrelease_test.go), so it stays well under
// a second while keeping the non-build half of the harness covered on every PR.
func TestCrossRelease_InProcessHelpersShort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// --- GenerateCrossReleaseOps + normaliseOpThroughJSON ---
	ops, err := GenerateCrossReleaseOps(0x5217E, 24)
	if err != nil {
		t.Fatalf("GenerateCrossReleaseOps: %v", err)
	}
	if len(ops) != 24 {
		t.Fatalf("GenerateCrossReleaseOps len = %d, want 24", len(ops))
	}
	// Determinism: the same seed yields the identical op stream.
	ops2, err := GenerateCrossReleaseOps(0x5217E, 24)
	if err != nil {
		t.Fatalf("GenerateCrossReleaseOps (repeat): %v", err)
	}
	for i := range ops {
		if ops[i].Cypher != ops2[i].Cypher {
			t.Fatalf("op %d not deterministic: %q != %q", i, ops[i].Cypher, ops2[i].Cypher)
		}
	}
	// The default branch (n<=0) picks the standard 400-op budget.
	if def, err := GenerateCrossReleaseOps(1, 0); err != nil || len(def) != 400 {
		t.Fatalf("GenerateCrossReleaseOps default = (%d, %v), want (400, nil)", len(def), err)
	}

	// --- replayCurrentSignatures -> currentOpSignature -> canonicalRecordSignature ---
	cur, err := replayCurrentSignatures(ctx, ops)
	if err != nil {
		t.Fatalf("replayCurrentSignatures: %v", err)
	}
	if len(cur.rows) != len(ops) {
		t.Fatalf("replayCurrentSignatures rows = %d, want %d", len(cur.rows), len(ops))
	}
	// Replaying the same op stream twice must yield identical signatures.
	cur2, err := replayCurrentSignatures(ctx, ops)
	if err != nil {
		t.Fatalf("replayCurrentSignatures (repeat): %v", err)
	}
	for i := range cur.rows {
		if cur.rows[i] != cur2.rows[i] {
			t.Fatalf("signature %d not deterministic: %q != %q", i, cur.rows[i], cur2.rows[i])
		}
	}

	// A cancelled context aborts the replay (the loop's ctx.Err() guard).
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := replayCurrentSignatures(cctx, ops); err == nil {
		t.Fatal("replayCurrentSignatures: expected error on a cancelled context")
	}

	// --- classifyDivergence / allBenign / canonicalHelperRowsMatch ---
	if !canonicalHelperRowsMatch("[a|b]", "[a|b]") || canonicalHelperRowsMatch("[a]", "[b]") {
		t.Fatal("canonicalHelperRowsMatch: equality rule broken")
	}
	// An unordered-LIMIT query is a benign, plan-dependent divergence.
	benign := classifyDivergence(3, Op{Kind: OpMatch, Cypher: "MATCH (n) RETURN n LIMIT 5"}, "[x]", "[y]")
	if !benign.Benign {
		t.Errorf("unordered LIMIT divergence should be benign: %+v", benign)
	}
	// Any other difference is an unexpected (non-benign) divergence.
	bad := classifyDivergence(4, Op{Kind: OpMatch, Cypher: "MATCH (n) RETURN n ORDER BY n.id"}, "[x]", "[y]")
	if bad.Benign {
		t.Errorf("ordered query divergence should NOT be benign: %+v", bad)
	}
	if !allBenign([]CrossReleaseDivergence{benign}) {
		t.Error("allBenign: a benign-only set should be all-benign")
	}
	if allBenign([]CrossReleaseDivergence{benign, bad}) {
		t.Error("allBenign: a set containing a non-benign divergence is not all-benign")
	}
	if !allBenign(nil) {
		t.Error("allBenign: an empty set should be all-benign")
	}

	// --- Result renderers (Parity + String) ---
	clean := CrossReleaseUpgradeResult{Tag: "v9.9.9", PriorLiveNodes: 3, PriorSelfNodes: 3, RecoveredNodes: 3}
	if !clean.Parity() {
		t.Error("clean upgrade result should report Parity")
	}
	if !strings.Contains(clean.String(), "v9.9.9") {
		t.Errorf("upgrade String missing tag: %s", clean.String())
	}
	failStop := CrossReleaseUpgradeResult{Tag: "v9.9.9", DataCompatError: context.Canceled, PriorWALFidelityGap: true, CountMismatch: "nodes: current=1 prior-self=2"}
	if failStop.Parity() {
		t.Error("a fail-stop/mismatch result must not report Parity")
	}
	if s := failStop.String(); !strings.Contains(s, "FAIL-STOP") || !strings.Contains(s, "MISMATCH") {
		t.Errorf("upgrade String missing fault detail: %s", s)
	}

	diffAgree := CrossReleaseDiffResult{Tag: "v9.9.9", Agreed: true, FinalCountsMatch: true}
	if !strings.Contains(diffAgree.String(), "AGREED") {
		t.Errorf("diff String should say AGREED: %s", diffAgree.String())
	}
	diffDiverge := CrossReleaseDiffResult{Tag: "v9.9.9", Agreed: false, Divergences: []CrossReleaseDivergence{bad}}
	if !strings.Contains(diffDiverge.String(), "DIVERGED") {
		t.Errorf("diff String should say DIVERGED: %s", diffDiverge.String())
	}

	// --- small string + git seams ---
	if got := sanitiseTag("v1.2/rc.3"); got != "v1_2_rc_3" {
		t.Errorf("sanitiseTag = %q, want v1_2_rc_3", got)
	}
}

// TestCrossRelease_ParseHelperOutputShort covers the helper line-protocol decoder
// (the pure side of the subprocess boundary) without spawning a subprocess: a
// well-formed stream decodes, and the truncated / out-of-order / missing-done
// faults are all hard errors rather than silent short comparisons.
func TestCrossRelease_ParseHelperOutputShort(t *testing.T) {
	t.Parallel()

	good := "" +
		`{"i":0,"committed":true,"rows":"[a]"}` + "\n" +
		"\n" + // blank line tolerated
		`{"i":1,"committed":false,"rows":"[b]"}` + "\n" +
		`{"done":true,"nodes":2,"edges":1}` + "\n"
	res, err := parseHelperOutput([]byte(good), 2)
	if err != nil {
		t.Fatalf("parseHelperOutput(good): %v", err)
	}
	if len(res.Ops) != 2 || res.Nodes != 2 || res.Edges != 1 {
		t.Fatalf("parseHelperOutput(good) = %+v, want 2 ops, nodes=2 edges=1", res)
	}
	if !res.Ops[0].Committed || res.Ops[0].Rows != "[a]" {
		t.Errorf("op 0 decoded wrong: %+v", res.Ops[0])
	}

	tests := []struct {
		name  string
		input string
		nOps  int
	}{
		{"missing done", `{"i":0,"committed":true,"rows":"[a]"}` + "\n", 1},
		{"out of order", `{"i":1,"committed":true,"rows":"[a]"}` + "\n" + `{"done":true}` + "\n", 1},
		{"wrong count", `{"i":0,"committed":true,"rows":"[a]"}` + "\n" + `{"done":true}` + "\n", 2},
		{"bad json", "not-json\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseHelperOutput([]byte(tt.input), tt.nOps); err == nil {
				t.Errorf("parseHelperOutput(%s): expected error, got nil", tt.name)
			}
		})
	}
}

// TestCrossRelease_CommitishExistsShort covers the git rev-parse seam cheaply:
// HEAD resolves in this checkout and a bogus ref does not. It skips when git or a
// checkout is unavailable (environment precondition), matching the harness's only
// sanctioned skip class.
func TestCrossRelease_CommitishExistsShort(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	ctx := context.Background()

	if !commitishExists(ctx, root, "HEAD") {
		t.Error("commitishExists(HEAD) = false in a git checkout")
	}
	if commitishExists(ctx, root, "no-such-ref-9f8e7d6c") {
		t.Error("commitishExists(bogus) = true")
	}
}

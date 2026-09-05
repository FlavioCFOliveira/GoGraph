package contention_test

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/contention"
)

// ─── the level-1 normalisation gate (rmp #2712) ──────────────────────────────
//
// rmp #2712 is a defect of PUBLICATION, not of measurement: the round-2
// inventory published `dst-concurrent-bolt`'s 16.307x ceiling as a lower bound
// while the arm's own level-1 cell read 1.078x, which makes it an upper bound to
// be discounted. The numbers were all in ceiling.tsv; nothing forced the
// document to read the one that gives the others their direction.
//
// These tests are what now forces it. Each of them fails if the refusal is
// removed, which was checked by removing it — see the task's report.

// anchoredRow builds a normalisable row for one pair at one level.
func anchoredRow(base, ceiling string, level int, ratio, baseSpread, ceilSpread float64) probeRow {
	return probeRow{
		Base: base, Ceiling: ceiling, Level: level, Rounds: 5,
		BaseMedian: 1000, CeilMedian: 1000 * ratio,
		BaseSpread: baseSpread, CeilSpread: ceilSpread,
		Ratio: ratio,
	}
}

// TestLadderWithoutTheAnchorLevelIsRefused pins the OUTERMOST of the three
// refusals: the one that stops a campaign which cannot be normalised from being
// measured at all.
//
// It matters most and is the easiest to lose. The other two fire after the
// windows have been paid for — [normaliseByAnchor] when a pair's rows are
// assembled, [writeProbeSummary] when the file is written — so deleting this one
// costs a whole campaign's wall-clock before anything complains, and a refactor
// that dropped it would leave every test in this file still passing. It does not
// any more.
//
// The default ladder is included as a live case: the levels a campaign actually
// walks must satisfy the predicate, or every run of the probe would be refused.
func TestLadderWithoutTheAnchorLevelIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		levels []int
		want   bool
	}{
		{"the package default ladder", contention.Levels(), true},
		{"the round-2 campaign's ladder", []int{1, 8, 1024}, true},
		{"anchor alone", []int{anchorLevel}, true},
		{"anchor last", []int{8, 1024, anchorLevel}, true},
		{"the ladder that produced rmp #2712's un-normalised 16.307x", []int{8, 1024}, false},
		{"a ladder starting one rung too high", []int{2, 8, 64}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ladderCanBeNormalised(c.levels); got != c.want {
				t.Errorf("ladderCanBeNormalised(%v) = %v, want %v", c.levels, got, c.want)
			}
		})
	}
}

// TestCeilingProbeRefusesALadderWithoutTheAnchorLevel covers the CALL SITE that
// [TestLadderWithoutTheAnchorLevelIsRefused] cannot reach.
//
// A pure-predicate test pins the decision; it says nothing about whether anyone
// still consults it. Neutering the `if` in requireAnchorLevel leaves the
// predicate correct and every in-process test green, so the guard has to be
// exercised through the entry point that owns it — the same way this package
// already proves its cache gate, and for the same reason.
//
// The two assertions are the two things the guard exists to do: REFUSE, and
// refuse BEFORE MEASURING. The second is why the artefact directory is checked
// and must still be empty: TestCeilingProbe creates it only after this guard has
// passed, so a single file in it means windows were run — the whole campaign
// paid for, on a ladder whose ceilings could never have been normalised.
//
// -count=1 is passed so the refusal under test is this one and not the cache
// gate's, which sits earlier in the same function.
func TestCeilingProbeRefusesALadderWithoutTheAnchorLevel(t *testing.T) {
	goBin := requireGoToolchain(t)
	root := t.TempDir() // absolute, so the probe's own IsAbs check is not what fires

	env := contentionEnv(map[string]string{
		"GOGRAPH_CONTENTION_CEILING_DIR": root,
		// The A-vs-A control: a valid pair, so parsePairs cannot be what fails.
		"GOGRAPH_CONTENTION_CEILING_PAIRS": "cypher-write-mem:cypher-write-mem",
		"GOGRAPH_CONTENTION_LEVELS":        "8",
	})
	out := runGo(t, goBin, repoRoot(t), env,
		"test", "-count=1", "-run", "^TestCeilingProbe$", "-v", "./bench/contention/")

	if !strings.Contains(out, "omits level 1") {
		t.Fatalf("the anchor-level guard did not fire; output:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("the probe did not fail on a ladder it cannot normalise; output:\n%s", out)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read artefact dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the guard fired only AFTER measuring: %d entries under %s, want 0",
			len(entries), root)
	}
}

// TestNormaliseByAnchorRefusesAPairWithoutItsLevelOneCell is the negative arm:
// a ladder that skipped level 1 cannot be normalised and must be refused rather
// than published raw.
func TestNormaliseByAnchorRefusesAPairWithoutItsLevelOneCell(t *testing.T) {
	rows := []probeRow{
		anchoredRow("dst-concurrent-bolt", "dst-concurrent-bolt-ceiling", 8, 1.6268, 0.0143, 0.0215),
		anchoredRow("dst-concurrent-bolt", "dst-concurrent-bolt-ceiling", 1024, 16.3072, 0.4941, 0.3478),
	}
	err := normaliseByAnchor(rows)
	if err == nil {
		t.Fatal("normaliseByAnchor accepted a pair with no level-1 cell; " +
			"that is exactly the publication rmp #2712 records")
	}
	if !strings.Contains(err.Error(), "level-1 cell") {
		t.Errorf("refusal does not name the missing cell: %v", err)
	}
	for i := range rows {
		if rows[i].RatioAt1 != 0 || rows[i].Normalised != 0 {
			t.Errorf("row %d was normalised despite the refusal: %+v", i, rows[i])
		}
	}
}

// TestNormaliseByAnchorRefusesAnUnusableAnchor covers the anchor that exists but
// cannot divide. A zero level-1 cell means the base arm produced no throughput,
// and dividing by it would publish +Inf as a ceiling.
func TestNormaliseByAnchorRefusesAnUnusableAnchor(t *testing.T) {
	for _, at1 := range []float64{0, math.Inf(1)} {
		rows := []probeRow{
			anchoredRow("a", "a-ceiling", 1, at1, 0.01, 0.01),
			anchoredRow("a", "a-ceiling", 8, 2.0, 0.01, 0.01),
		}
		if err := normaliseByAnchor(rows); err == nil {
			t.Errorf("normaliseByAnchor accepted a level-1 cell of %v", at1)
		}
	}
}

// TestWriteProbeSummaryRefusesAnUnnormalisedCeiling is the file's own guard: a
// row that reached the writer without its anchor must not reach ceiling.tsv,
// because ceiling.tsv is what the documents quote.
func TestWriteProbeSummaryRefusesAnUnnormalisedCeiling(t *testing.T) {
	root := t.TempDir()
	rows := []probeRow{
		anchoredRow("dst-concurrent-bolt", "dst-concurrent-bolt-ceiling", 1024, 16.3072, 0.4941, 0.3478),
	}
	err := writeProbeSummary(root, rows)
	if err == nil {
		t.Fatal("writeProbeSummary published a ceiling with no level-1 cell")
	}
	if !strings.Contains(err.Error(), "16.3072") || !strings.Contains(err.Error(), "#2712") {
		t.Errorf("refusal names neither the figure nor the defect: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ceiling.tsv")); !os.IsNotExist(statErr) {
		t.Errorf("ceiling.tsv exists after a refused publish: %v", statErr)
	}
}

// TestNormaliseByAnchorCorrectsBothDirections is the positive arm, and it is
// pinned to REAL measured cells rather than invented ones, because the whole
// defect was a claim that every arm's bias runs one way.
//
// Both rows below come from the round-2 campaign's own ceiling.tsv (14 pairs x
// 3 levels x 5 interleaved rounds, loadavg 2.18 -> 8.10, verified fresh by rmp
// #2713):
//
//   - `metrics-emit` reads 0.627x at level 1 — the arm is slower before
//     anything is unshared, so its 3.300x at 8 UNDERSTATES;
//   - `dst-concurrent-bolt` reads 1.078x at level 1 — the arm is faster before
//     anything is unshared, so its 16.307x at 1024 OVERSTATES.
//
// One direction each, from the same instrument, in the same campaign.
func TestNormaliseByAnchorCorrectsBothDirections(t *testing.T) {
	cases := []struct {
		name       string
		at1        float64
		at1Base    float64
		at1Ceil    float64
		level      int
		raw        float64
		wantNorm   float64
		wantDirect handicapDirection
	}{
		{
			name: "metrics-emit understates", at1: 0.6270, at1Base: 0.0089, at1Ceil: 0.0125,
			level: 8, raw: 3.3002, wantNorm: 5.2635, wantDirect: dirUnderstates,
		},
		{
			name: "dst-concurrent-bolt overstates", at1: 1.0780, at1Base: 0.0095, at1Ceil: 0.0062,
			level: 1024, raw: 16.3072, wantNorm: 15.1273, wantDirect: dirOverstates,
		},
		{
			// cypher-write-mem's 1.004x does not clear the +/-2.4% floor, so no
			// direction is established and its ceiling is read as measured.
			name: "cypher-write-mem has no direction", at1: 1.0041, at1Base: 0.0173, at1Ceil: 0.0107,
			level: 1024, raw: 1.7018, wantNorm: 1.6948, wantDirect: dirNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := []probeRow{
				anchoredRow("base", "base-ceiling", 1, c.at1, c.at1Base, c.at1Ceil),
				anchoredRow("base", "base-ceiling", c.level, c.raw, 0.02, 0.02),
			}
			if err := normaliseByAnchor(rows); err != nil {
				t.Fatalf("normaliseByAnchor: %v", err)
			}
			got := &rows[1]
			if got.RatioAt1 != c.at1 {
				t.Errorf("RatioAt1 = %v, want %v", got.RatioAt1, c.at1)
			}
			if math.Abs(got.Normalised-c.wantNorm) > 5e-4 {
				t.Errorf("normalised = %.4f, want %.4f", got.Normalised, c.wantNorm)
			}
			if d := direction(got); d != c.wantDirect {
				t.Errorf("direction = %q, want %q", d, c.wantDirect)
			}
			// The anchor row normalises to exactly 1.000x by construction: it
			// is the cell everything else is divided by.
			if math.Abs(rows[0].Normalised-1) > 1e-12 {
				t.Errorf("anchor row normalised to %.6f, want 1.000000", rows[0].Normalised)
			}
		})
	}
}

// TestWriteProbeSummaryPublishesBothFigures pins the published columns: a reader
// must be able to see the raw ceiling, the cell it was corrected by, and the
// corrected figure — the correction, not merely its result.
func TestWriteProbeSummaryPublishesBothFigures(t *testing.T) {
	root := t.TempDir()
	rows := []probeRow{
		anchoredRow("dst-concurrent-bolt", "dst-concurrent-bolt-ceiling", 1, 1.0780, 0.0095, 0.0062),
		anchoredRow("dst-concurrent-bolt", "dst-concurrent-bolt-ceiling", 1024, 16.3072, 0.4941, 0.3478),
	}
	if err := normaliseByAnchor(rows); err != nil {
		t.Fatalf("normaliseByAnchor: %v", err)
	}
	if err := writeProbeSummary(root, rows); err != nil {
		t.Fatalf("writeProbeSummary: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "ceiling.tsv")) //nolint:gosec // G304: root is the operator-supplied artefact directory this suite asserts is absolute; the leaf name is a literal.
	if err != nil {
		t.Fatalf("read ceiling.tsv: %v", err)
	}
	out := string(b)
	for _, want := range []string{
		"ratio_at_1", "ratio_normalised", "anchor_tolerance", "direction",
		"16.3072", "1.0780", "15.1273", string(dirOverstates),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ceiling.tsv does not carry %q:\n%s", want, out)
		}
	}
}

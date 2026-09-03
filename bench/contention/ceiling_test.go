package contention_test

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/contention"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// envCeilingDir names an absolute directory to write the probe's artefacts
// into. When it is unset the probe is skipped, for the same reason TestSweep is
// skipped: it is a measurement campaign, not a unit test.
const envCeilingDir = "GOGRAPH_CONTENTION_CEILING_DIR"

// envCeilingPairs is a comma list of "base:ceiling" workload names.
//
// Naming the SAME workload on both sides is not a mistake, it is the
// instrument's calibration: an arm measured against itself yields the noise
// floor, produced by exactly the machinery that will later produce the ratios,
// which is the only floor a ratio may honestly be compared against.
const envCeilingPairs = "GOGRAPH_CONTENTION_CEILING_PAIRS"

// envCeilingRepeats is how many A/B rounds to run per level (default 5).
const envCeilingRepeats = "GOGRAPH_CONTENTION_CEILING_REPEATS"

// TestCeilingProbe measures one arm against another, INTERLEAVED.
//
// # Why interleaved, and never all of A then all of B
//
// Running an arm to completion and then the other compares two different
// stretches of the host's afternoon: the CPU's thermal state, its frequency,
// the page cache and whatever else the machine was doing all drift over
// minutes, and every one of those drifts lands entirely on the second arm. The
// rounds here alternate, and the ORDER WITHIN a round alternates too — round 0
// runs base then ceiling, round 1 runs ceiling then base — so a residual
// first-position advantage cancels between rounds instead of accumulating in
// one arm's column.
//
// # Only the effect window
//
// A ceiling is a throughput question, so only the unprofiled window is run.
// Attribution is the sweep's job; asking the profiler for it here would both
// double the cost and perturb the very number being compared.
//
// # What is reported
//
// The median of each arm's per-round throughput, their ratio, and the SPREAD of
// each arm's own rounds (min/max as a fraction of the median). A ratio that
// does not clear the arms' own spread is not a result, and the spread is
// printed beside the ratio so that cannot be quietly forgotten.
//
// # Run it with -count=1
//
// Go CACHES test results, and this test is cacheable: its inputs are the test
// binary, its arguments and the environment variables it reads. Re-running a
// campaign with the same env therefore REPLAYS THE PREVIOUS RUN'S OUTPUT
// instead of measuring, and the replay is nearly silent -- the only tell is
// "(cached)" in place of the elapsed time on the final ok line, while every
// throughput number, every loadavg bracket around it and the reported test
// duration all look like a fresh measurement.
//
// This was not hypothesised, it happened: a re-take of the A-vs-A noise floor
// returned nine ratios byte-identical to the run 10 minutes earlier, including
// the 102.86s duration, in a window that actually took under a second. A
// published "re-measured on a quiet host" floor would have been the old
// contaminated one.
//
// This is now ENFORCED, not merely documented: the test refuses to run at all
// when cmd/go has signalled that it will cache the result, and because Go stores
// only passing results the refusal itself can never be cached either. See
// requireFreshRun in cachegate_test.go for the mechanism and its proof.
//
// Always pass -count=1.
func TestCeilingProbe(t *testing.T) {
	root := os.Getenv(envCeilingDir)
	if root == "" {
		t.Skipf("set %s=<abs dir> to run the ceiling probe, and pass -v or its output is discarded", envCeilingDir)
	}
	// The campaign is on. Refuse to produce numbers cmd/go would be entitled
	// to replay in place of the next run's; see cachegate_test.go.
	requireFreshRun(t)
	if !filepath.IsAbs(root) {
		t.Fatalf("%s must be absolute, got %q", envCeilingDir, root)
	}
	pairs := parsePairs(t)
	levels := probeLevels(t)
	repeats := probeRepeats(t)
	requireAnchorLevel(t, levels)

	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	fmt.Printf("ceiling probe artefacts: %s\n", root)

	var rows []probeRow
	for _, p := range pairs {
		// Collected per PAIR, not per row, because a row cannot be normalised
		// until its pair's level-1 cell has been measured: the anchor is a
		// property of the pair, and the ladder does not have to arrive in
		// order.
		var pairRows []probeRow
		for _, level := range levels {
			base := make([]float64, 0, repeats)
			ceil := make([]float64, 0, repeats)
			for r := range repeats {
				// Alternate which arm goes first, so a first-position effect
				// cancels between rounds rather than favouring one column.
				first, second := p.base, p.ceiling
				firstOut, secondOut := &base, &ceil
				if r%2 == 1 {
					first, second = second, first
					firstOut, secondOut = secondOut, firstOut
				}
				a, err := runEffect(t, root, first, level, r)
				if err != nil {
					t.Errorf("%s@%d round %d: %v", first, level, r, err)
					continue
				}
				b, err := runEffect(t, root, second, level, r)
				if err != nil {
					t.Errorf("%s@%d round %d: %v", second, level, r, err)
					continue
				}
				*firstOut = append(*firstOut, a.OpsPerSec)
				*secondOut = append(*secondOut, b.OpsPerSec)
			}
			if len(base) == 0 || len(ceil) == 0 {
				continue
			}
			row := probeRow{
				Base: p.base, Ceiling: p.ceiling, Level: level,
				BaseMedian: median(base), CeilMedian: median(ceil),
				BaseSpread: spread(base), CeilSpread: spread(ceil),
				Rounds: len(base),
			}
			if row.BaseMedian > 0 {
				row.Ratio = row.CeilMedian / row.BaseMedian
			}
			t.Logf("%s -> %s @%-4d  base=%.1f/s (spread %.2f%%)  ceiling=%.1f/s (spread %.2f%%)  ratio=%.3fx",
				row.Base, row.Ceiling, row.Level,
				row.BaseMedian, row.BaseSpread*100, row.CeilMedian, row.CeilSpread*100, row.Ratio)
			pairRows = append(pairRows, row)
		}
		if err := normaliseByAnchor(pairRows); err != nil {
			// The pair is REPORTED and DROPPED, never published raw. A ceiling
			// without its own level-1 cell has no established direction, so
			// publishing it would republish exactly the defect rmp #2712
			// records. Dropping one pair keeps the other pairs' measurements,
			// and the t.Errorf keeps the campaign marked unsound.
			t.Errorf("%s -> %s: %v (rows dropped, not published)", p.base, p.ceiling, err)
			continue
		}
		logNormalised(t, pairRows)
		rows = append(rows, pairRows...)
	}
	if len(rows) == 0 {
		t.Fatal("ceiling probe produced no results")
	}
	if err := writeProbeSummary(root, rows); err != nil {
		t.Fatalf("write probe summary: %v", err)
	}
	fmt.Printf("ceiling probe complete: %s\n", filepath.Join(root, "ceiling.tsv"))
}

// probePair is one base/ceiling comparison.
type probePair struct{ base, ceiling string }

// probeRow is one (pair, level) result.
//
// [probeRow.Ratio] is the RAW ceiling: ceiling throughput over base throughput
// at this level. [probeRow.Normalised] is that ratio divided by the pair's own
// level-1 cell, and [probeRow.RatioAt1] is the cell it was divided by. All
// three are published, so a reader sees the correction and not merely its
// result.
type probeRow struct {
	Base       string
	Ceiling    string
	Level      int
	Rounds     int
	BaseMedian float64
	CeilMedian float64
	BaseSpread float64
	CeilSpread float64
	Ratio      float64

	// RatioAt1 is this pair's level-1 cell, and is zero until
	// [normaliseByAnchor] has filled it in. Zero therefore means "not
	// normalised", which [writeProbeSummary] refuses to publish.
	RatioAt1 float64
	// Normalised is Ratio / RatioAt1.
	Normalised float64
	// AnchorTol is the tolerance the level-1 cell was judged against: the
	// wider of the +/-2.4% working floor and the anchor row's own arm spreads.
	AnchorTol float64
}

func parsePairs(t *testing.T) []probePair {
	t.Helper()
	raw := os.Getenv(envCeilingPairs)
	if raw == "" {
		t.Fatalf("set %s=base:ceiling[,base:ceiling...]", envCeilingPairs)
	}
	parts := strings.Split(raw, ",")
	out := make([]probePair, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		base, ceiling, ok := strings.Cut(part, ":")
		if !ok {
			t.Fatalf("bad pair %q in %s, want base:ceiling", part, envCeilingPairs)
		}
		for _, name := range []string{base, ceiling} {
			if _, found := contention.ByName(name); !found {
				t.Fatalf("unknown workload %q in %s", name, envCeilingPairs)
			}
		}
		out = append(out, probePair{base: base, ceiling: ceiling})
	}
	return out
}

func probeLevels(t *testing.T) []int {
	t.Helper()
	s := os.Getenv(envSweepLevels)
	if s == "" {
		return contention.Levels()
	}
	var levels []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			t.Fatalf("bad level %q in %s", part, envSweepLevels)
		}
		levels = append(levels, n)
	}
	return levels
}

func probeRepeats(t *testing.T) int {
	t.Helper()
	s := os.Getenv(envCeilingRepeats)
	if s == "" {
		return 5
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		t.Fatalf("bad %s=%q", envCeilingRepeats, s)
	}
	return n
}

// runEffect runs one unprofiled window of one workload in a fresh child.
func runEffect(t *testing.T, root, workload string, level, round int) (contention.Metrics, error) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprintf("%s@%d", workload, level), fmt.Sprintf("r%d", round))
	stdout, stderr, err := subproc.RunWithTimeout(t, 20*time.Minute,
		childMode, workload, strconv.Itoa(level), string(contention.WindowEffect), dir)
	if err != nil {
		return contention.Metrics{}, fmt.Errorf("child failed: %w\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	return contention.ReadMetrics(dir)
}

// median returns the middle value of a copy of xs, so the caller's slice keeps
// the order the rounds were run in.
func median(xs []float64) float64 {
	s := slices.Clone(xs)
	sort.Float64s(s)
	return s[len(s)/2]
}

// spread returns the wider of the two deviations from the median, as a
// fraction of it. It is a range, not a standard deviation: with five rounds a
// standard deviation is an estimate of an estimate, whereas the range is the
// thing itself and is the harsher of the two.
func spread(xs []float64) float64 {
	m := median(xs)
	if m == 0 {
		return 0
	}
	var worst float64
	for _, x := range xs {
		worst = math.Max(worst, math.Abs(x-m)/m)
	}
	return worst
}

// ─── normalisation by the level-1 cell (rmp #2712) ───────────────────────────

// anchorLevel is the level at which a ceiling arm has nothing to unshare.
//
// A ceiling arm builds GOMAXPROCS independent copies of the fixture the base
// workload shares and routes worker i to copy i mod N. At level 1 there is
// exactly one worker, so it meets exactly one copy and the arm is running the
// base workload's own code against the base workload's own fixture shape. The
// pair must therefore read 1.000x here, and whatever it reads INSTEAD is the
// arm's construction bias, priced by the same instrument that will price the
// ceilings.
const anchorLevel = 1

// anchorFloor is the working noise floor this package's campaigns apply: an
// A-vs-A calibration of eighteen ratios put sixteen inside +/-2.4%. A departure
// smaller than this is not a direction, it is the instrument.
const anchorFloor = 0.024

// handicapDirection names which way an arm's construction bias points, and
// therefore which way its ceilings must be read.
type handicapDirection string

const (
	// dirNone means the level-1 cell did not clear the floor: no direction is
	// established and the ceilings are read as measured.
	dirNone handicapDirection = "within the floor (no direction established)"
	// dirUnderstates means the arm is SLOWER at level 1 than the base. It pays
	// a cost the base does not, so every ceiling it reports is a LOWER bound.
	dirUnderstates handicapDirection = "arm handicapped at level 1 -> its ceilings are LOWER bounds"
	// dirOverstates means the arm is FASTER at level 1 than the base, before
	// anything has been unshared. It enjoys an advantage the base does not, so
	// every ceiling it reports is an UPPER bound and must be discounted.
	dirOverstates handicapDirection = "arm favoured at level 1 -> its ceilings are UPPER bounds"
)

// ladderCanBeNormalised reports whether a ladder carries the one level every
// ceiling on it will have to be normalised by.
//
// It is a pure predicate, separated from [requireAnchorLevel], for exactly the
// reason the other two refusals in this file are separated from their callers:
// a guard whose decision cannot be called directly is a guard no test can pin,
// and this one is the outermost of the three — it is what stops a campaign that
// cannot be normalised from being MEASURED at all.
func ladderCanBeNormalised(levels []int) bool {
	return slices.Contains(levels, anchorLevel)
}

// requireAnchorLevel refuses a campaign whose ladder omits [anchorLevel].
//
// It fires BEFORE any window runs. The same omission would be caught later by
// [normaliseByAnchor], but only after the whole campaign had been paid for, and
// a campaign that cannot be normalised is a campaign that must not be run.
func requireAnchorLevel(t *testing.T, levels []int) {
	t.Helper()
	if ladderCanBeNormalised(levels) {
		return
	}
	t.Fatalf(`ceiling probe refused: the ladder %v omits level %d.

A ceiling arm's level-%d cell is what says whether its ceilings are lower bounds
or upper ones — see rmp #2712, where a ceiling of 16.307x was published for an
arm that reads 1.078x at level 1 and was therefore an UPPER bound, not the lower
bound the document claimed. Without that cell no ceiling here can be normalised,
so none may be published.

Add %d to %s.`, levels, anchorLevel, anchorLevel, anchorLevel, envSweepLevels)
}

// anchorTolerance is the tolerance a level-1 cell is judged against: the wider
// of the working floor and the anchor row's own two arm spreads. A departure
// inside an arm's own round-to-round spread is not a departure.
func anchorTolerance(r *probeRow) float64 {
	return math.Max(anchorFloor, math.Max(r.BaseSpread, r.CeilSpread))
}

// direction classifies a normalised row's anchor. It reads RatioAt1, so it is
// meaningful only after [normaliseByAnchor].
func direction(r *probeRow) handicapDirection {
	switch {
	case r.RatioAt1 > 1+r.AnchorTol:
		return dirOverstates
	case r.RatioAt1 < 1-r.AnchorTol:
		return dirUnderstates
	default:
		return dirNone
	}
}

// normaliseByAnchor fills in RatioAt1, Normalised and AnchorTol on every row of
// one pair, and REFUSES the pair when its level-[anchorLevel] cell is missing
// or unusable.
//
// The refusal is the point. rmp #2712 records what happens without it: an arm
// whose level-1 cell sat at 1.078x had its 16.307x ceiling published as a lower
// bound, when the cell says it is an upper bound to be discounted to 15.1x. A
// ceiling without its own anchor has no established direction, and a number
// whose direction is unknown must not be published as though it had one.
//
// # What the division does and does not remove
//
// Dividing by the level-1 cell removes the part of the arm's construction bias
// that is CONSTANT across the ladder — replica count, resident state, allocator
// and cache locality — because that part is present at level 1 too. It does NOT
// remove a bias that GROWS with the level. The clearest such bias in this
// package is data distribution: the ladder holds the TOTAL operation count
// fixed and splits it across workers, so at level N>1 an arm's replicas each
// accumulate roughly 1/min(N,replicas) of the writes the base's single fixture
// accumulates, and a smaller fixture can answer a cheaper query. That effect is
// exactly ZERO at level 1 — one worker, one replica, the same writes — so the
// anchor cannot see it and the normalised figure remains an upper bound for any
// arm whose fixture accumulates state.
func normaliseByAnchor(rows []probeRow) error {
	if len(rows) == 0 {
		return fmt.Errorf("no rows")
	}
	var anchor *probeRow
	for i := range rows {
		if rows[i].Level == anchorLevel {
			anchor = &rows[i]
			break
		}
	}
	if anchor == nil {
		levels := make([]int, 0, len(rows))
		for i := range rows {
			levels = append(levels, rows[i].Level)
		}
		return fmt.Errorf(
			"no level-%d cell: levels measured were %v, so the ceilings cannot be "+
				"normalised and their direction is unknown (rmp #2712)", anchorLevel, levels)
	}
	if !(anchor.Ratio > 0) || math.IsInf(anchor.Ratio, 0) {
		return fmt.Errorf(
			"level-%d cell is %v, which cannot normalise anything (rmp #2712)",
			anchorLevel, anchor.Ratio)
	}
	tol := anchorTolerance(anchor)
	at1 := anchor.Ratio
	for i := range rows {
		rows[i].RatioAt1 = at1
		rows[i].AnchorTol = tol
		rows[i].Normalised = rows[i].Ratio / at1
	}
	return nil
}

// logNormalised prints one pair's anchor, its direction, and the raw and
// normalised figure for every level, so a reader of the log sees the correction
// rather than only its result.
func logNormalised(t *testing.T, rows []probeRow) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	r0 := &rows[0]
	t.Logf("%s -> %s  level-%d cell = %.3fx (tolerance +/-%.2f%%): %s",
		r0.Base, r0.Ceiling, anchorLevel, r0.RatioAt1, r0.AnchorTol*100, direction(r0))
	for i := range rows {
		r := &rows[i]
		t.Logf("    @%-4d raw=%.3fx  normalised=%.3fx", r.Level, r.Ratio, r.Normalised)
	}
}

// writeProbeSummary publishes the campaign, and refuses to publish a row that
// carries no level-[anchorLevel] cell.
//
// This guard is deliberately redundant with [normaliseByAnchor]: that function
// is the caller's contract, this one is the file's. ceiling.tsv is what later
// documents quote, so the last thing standing between an un-normalised ceiling
// and a published claim about its direction is here.
func writeProbeSummary(root string, rows []probeRow) error {
	for i := range rows {
		if r := &rows[i]; !(r.RatioAt1 > 0) {
			return fmt.Errorf(
				"%s -> %s @%d: refusing to publish a ceiling of %.4fx with no level-%d "+
					"cell — its direction (lower bound or upper bound) is unknown, which is "+
					"the defect rmp #2712 records",
				r.Base, r.Ceiling, r.Level, r.Ratio, anchorLevel)
		}
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "base\tceiling\tlevel\trounds\tbase_ops_per_sec\tceiling_ops_per_sec\tratio\tbase_spread\tceiling_spread\tratio_at_1\tratio_normalised\tanchor_tolerance\tdirection\n")
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%.1f\t%.1f\t%.4f\t%.4f\t%.4f\t%.4f\t%.4f\t%.4f\t%s\n",
			r.Base, r.Ceiling, r.Level, r.Rounds,
			r.BaseMedian, r.CeilMedian, r.Ratio, r.BaseSpread, r.CeilSpread,
			r.RatioAt1, r.Normalised, r.AnchorTol, direction(r))
	}
	return os.WriteFile(filepath.Join(root, "ceiling.tsv"), b.Bytes(), 0o600)
}

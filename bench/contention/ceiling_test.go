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
// Always pass -count=1.
func TestCeilingProbe(t *testing.T) {
	root := os.Getenv(envCeilingDir)
	if root == "" {
		t.Skipf("set %s=<abs dir> to run the ceiling probe, and pass -v or its output is discarded", envCeilingDir)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("%s must be absolute, got %q", envCeilingDir, root)
	}
	pairs := parsePairs(t)
	levels := probeLevels(t)
	repeats := probeRepeats(t)

	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	fmt.Printf("ceiling probe artefacts: %s\n", root)

	var rows []probeRow
	for _, p := range pairs {
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
			rows = append(rows, row)
		}
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

func writeProbeSummary(root string, rows []probeRow) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "base\tceiling\tlevel\trounds\tbase_ops_per_sec\tceiling_ops_per_sec\tratio\tbase_spread\tceiling_spread\n")
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%.1f\t%.1f\t%.4f\t%.4f\t%.4f\n",
			r.Base, r.Ceiling, r.Level, r.Rounds,
			r.BaseMedian, r.CeilMedian, r.Ratio, r.BaseSpread, r.CeilSpread)
	}
	return os.WriteFile(filepath.Join(root, "ceiling.tsv"), b.Bytes(), 0o600)
}

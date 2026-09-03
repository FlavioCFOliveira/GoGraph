package contention_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/contention"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// envSweepDir names an absolute directory to write the sweep's artefacts into.
// When it is unset the sweep is skipped: it is a measurement campaign, not a
// unit test, and it must never run as a side effect of `go test ./...`.
const envSweepDir = "GOGRAPH_CONTENTION_SWEEP_DIR"

// envSweepLevels optionally overrides the goroutine ladder, as a comma list.
const envSweepLevels = "GOGRAPH_CONTENTION_LEVELS"

// envSweepWorkloads optionally restricts the sweep to a comma list of names.
const envSweepWorkloads = "GOGRAPH_CONTENTION_WORKLOADS"

// TestSweep drives every workload at every level and writes a TSV summary plus
// the per-window profiles.
//
// Each (workload, level) pair costs TWO fresh child processes, one per window.
// That is deliberate and it is what makes the probe_slowdown column meaningful:
// when both windows shared a child, the second inherited the first one's warm
// heap and reported the profiled run as the faster of the two.
//
// It is opt-in via GOGRAPH_CONTENTION_SWEEP_DIR because a full sweep takes
// minutes and saturates the host — running it inside the ordinary short layer
// would both blow the per-package budget and corrupt anything else being
// measured on the machine at the time.
//
// RUN IT WITH -v. `go test` discards everything a PASSING package writes —
// stdout, stderr and t.Log alike — so without -v the run directory and the
// summary path are swallowed along with the rest. This was measured, not
// assumed: fmt.Println, fmt.Fprintln(os.Stderr) and t.Log all vanish from a
// passing package and all three reappear under -v. Printf is used below rather
// than t.Logf only because it prints the path unadorned by a file:line prefix;
// it does not escape the capture, and nothing in the test framework can.
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
func TestSweep(t *testing.T) {
	root := os.Getenv(envSweepDir)
	if root == "" {
		t.Skipf("set %s=<abs dir> to run the contention sweep, and pass -v or its output is discarded", envSweepDir)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("%s must be absolute (the child runs in its own temp cwd), got %q", envSweepDir, root)
	}

	levels := contention.Levels()
	if s := os.Getenv(envSweepLevels); s != "" {
		levels = nil
		for _, part := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 {
				t.Fatalf("bad level %q in %s", part, envSweepLevels)
			}
			levels = append(levels, n)
		}
	}

	workloads := contention.All()
	if s := os.Getenv(envSweepWorkloads); s != "" {
		want := map[string]bool{}
		for _, part := range strings.Split(s, ",") {
			want[strings.TrimSpace(part)] = true
		}
		filtered := make([]contention.Workload, 0, len(workloads))
		for _, w := range workloads {
			if want[w.Name] {
				filtered = append(filtered, w)
			}
		}
		if len(filtered) == 0 {
			t.Fatalf("%s matched no workload", envSweepWorkloads)
		}
		workloads = filtered
	}

	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	// Printed before the work starts, so the path is known even if the sweep
	// aborts partway. Visible only under -v or on failure; see the note on
	// TestSweep.
	fmt.Printf("contention sweep artefacts: %s\n", root)

	rows := make([]contention.Result, 0, len(workloads)*len(levels))
	for _, w := range workloads {
		for _, level := range levels {
			pairDir := filepath.Join(root, fmt.Sprintf("%s@%d", w.Name, level))
			start := time.Now()

			effect, err := runWindow(t, w.Name, level, contention.WindowEffect, pairDir)
			if err != nil {
				t.Errorf("%s@%d %s: %v", w.Name, level, contention.WindowEffect, err)
				continue
			}
			probe, err := runWindow(t, w.Name, level, contention.WindowProbe, pairDir)
			if err != nil {
				t.Errorf("%s@%d %s: %v", w.Name, level, contention.WindowProbe, err)
				continue
			}

			res := contention.Result{
				Effect:     effect,
				Probe:      probe,
				ProfileDir: filepath.Join(pairDir, string(contention.WindowProbe)),
			}
			if err := contention.WriteResult(pairDir, &res); err != nil {
				t.Errorf("%s@%d: write result: %v", w.Name, level, err)
				continue
			}
			t.Logf("%s@%-4d %s  effect=%.1f/s probe=%.1f/s p99=%dus errors=%d",
				w.Name, level, time.Since(start).Round(time.Millisecond),
				effect.OpsPerSec, probe.OpsPerSec, effect.P99Nanos/1000, effect.Errors)
			rows = append(rows, res)
		}
	}

	if len(rows) == 0 {
		t.Fatal("sweep produced no results")
	}
	if err := writeSummary(root, rows); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	fmt.Printf("contention sweep complete: %s\n", filepath.Join(root, "summary.tsv"))
}

// runWindow spawns one fresh child for one window of one measurement and reads
// back the metrics it wrote.
func runWindow(t *testing.T, workload string, level int, win contention.Window, pairDir string) (contention.Metrics, error) {
	t.Helper()
	dir := filepath.Join(pairDir, string(win))
	stdout, stderr, err := subproc.RunWithTimeout(t, 20*time.Minute,
		childMode, workload, strconv.Itoa(level), string(win), dir)
	if err != nil {
		return contention.Metrics{}, fmt.Errorf("child failed: %w\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	return contention.ReadMetrics(dir)
}

// writeSummary emits the scaling table. Throughput comes from the UNPROFILED
// window only; the probe window's throughput is carried alongside purely so a
// reader can see how heavily the profiler perturbed the run, and must never be
// read as the module's throughput.
func writeSummary(root string, rows []contention.Result) error {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Effect.Workload != rows[j].Effect.Workload {
			return rows[i].Effect.Workload < rows[j].Effect.Workload
		}
		return rows[i].Effect.Level < rows[j].Effect.Level
	})

	base := make(map[string]float64, len(rows))
	for i := range rows {
		if e := &rows[i].Effect; e.Level == 1 {
			base[e.Workload] = e.OpsPerSec
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "workload\tsurface\tlevel\tops\tops_per_sec\tscaling_vs_1\tp50_us\tp99_us\terrors\tprobe_ops_per_sec\tprobe_slowdown\n")
	for i := range rows {
		r := &rows[i]
		e := &r.Effect
		scaling := ""
		if b1, ok := base[e.Workload]; ok && b1 > 0 {
			scaling = fmt.Sprintf("%.3f", e.OpsPerSec/b1)
		}
		slow := ""
		if r.Probe.OpsPerSec > 0 {
			slow = fmt.Sprintf("%.2fx", e.OpsPerSec/r.Probe.OpsPerSec)
		}
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%.1f\t%s\t%.1f\t%.1f\t%d\t%.1f\t%s\n",
			e.Workload, e.Surface, e.Level, e.Ops, e.OpsPerSec, scaling,
			float64(e.P50Nanos)/1000, float64(e.P99Nanos)/1000, e.Errors,
			r.Probe.OpsPerSec, slow)
	}
	return os.WriteFile(filepath.Join(root, "summary.tsv"), b.Bytes(), 0o600)
}

// runCmd runs a command and returns its combined output.
func runCmd(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // G204: fixed tool name, paths from the test's own temp dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

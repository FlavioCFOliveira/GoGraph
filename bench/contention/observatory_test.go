package contention_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/contention"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// trivialWorkload is a fixture with a KNOWN contention shape: every operation
// takes one shared mutex and holds it briefly. Under concurrency this must
// show up in the mutex profile. It is the instrument's positive control — an
// observatory that cannot see contention it was handed deliberately cannot be
// trusted when it reports none.
func trivialWorkload(ops int) contention.Workload {
	return contention.Workload{
		Name:    "selftest-shared-mutex",
		Surface: "none (instrument self-test)",
		Ops:     ops,
		Setup: func(_ string) (contention.Op, func() error, error) {
			var mu sync.Mutex
			counter := 0
			op := func(_ context.Context, _, _ int) error {
				mu.Lock()
				counter++
				// A little work under the lock, so the critical section is
				// long enough to actually contend at high concurrency.
				for i := 0; i < 200; i++ {
					counter ^= i
				}
				mu.Unlock()
				return nil
			}
			return op, func() error { return nil }, nil
		},
	}
}

// uncontendedWorkload is the negative control's fixture: purely local work,
// nothing shared, so a correct instrument must attribute no contention to it.
func uncontendedWorkload(ops int) contention.Workload {
	return contention.Workload{
		Name:    "selftest-no-shared-state",
		Surface: "none (instrument self-test)",
		Ops:     ops,
		Setup: func(_ string) (contention.Op, func() error, error) {
			op := func(_ context.Context, _, iter int) error {
				x := iter
				for i := 0; i < 200; i++ {
					x ^= i
				}
				if x == -1 {
					return errNever
				}
				return nil
			}
			return op, func() error { return nil }, nil
		},
	}
}

// controlMode runs one control fixture's PROBE window in a fresh process, so
// its cumulative mutex profile contains that fixture's samples and nothing
// else.
const controlMode = "contention-selftest"

const (
	controlContended   = "contended"
	controlUncontended = "uncontended"
)

func init() {
	subproc.Register(controlMode, func(args []string) int {
		if len(args) != 3 {
			fmt.Fprintf(os.Stderr, "usage: %s <fixture> <level> <outDir>\n", controlMode)
			return 2
		}
		var w contention.Workload
		switch args[0] {
		case controlContended:
			w = trivialWorkload(40000)
		case controlUncontended:
			w = uncontendedWorkload(40000)
		default:
			fmt.Fprintf(os.Stderr, "unknown fixture %q\n", args[0])
			return 2
		}
		level, err := strconv.Atoi(args[1])
		if err != nil || level < 1 {
			fmt.Fprintf(os.Stderr, "bad level %q\n", args[1])
			return 2
		}
		if _, err := contention.Observe(w, level, contention.WindowProbe, args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "observe: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestObserveRestoresProfilingRates is the safety property that matters most:
// a leaked profiling rate silently taxes and distorts every later measurement
// in the same process.
func TestObserveRestoresProfilingRates(t *testing.T) {
	// SetMutexProfileFraction(-1) reports the current value without changing
	// it — the only getter the runtime offers for either rate.
	if got := runtime.SetMutexProfileFraction(-1); got != 0 {
		t.Fatalf("precondition: mutex profile fraction already %d, want 0; "+
			"an earlier test in this process leaked it", got)
	}

	dir := t.TempDir()
	if _, err := contention.Observe(trivialWorkload(2000), 4, contention.WindowProbe, dir); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if got := runtime.SetMutexProfileFraction(-1); got != 0 {
		t.Errorf("mutex profile fraction left at %d after Observe, want 0", got)
	}

	// The runtime exposes no getter for the block profile rate, so verify it
	// behaviourally: with the rate restored to 0, deliberately blocking must
	// add no new samples to the block profile.
	before := blockProfileSamples(t)
	var wg sync.WaitGroup
	ch := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); <-ch }()
	time.Sleep(5 * time.Millisecond) // ensure the receive really blocks
	close(ch)
	wg.Wait()
	if after := blockProfileSamples(t); after != before {
		t.Errorf("block profile grew from %d to %d after Observe returned: rate not restored", before, after)
	}
}

func blockProfileSamples(t *testing.T) int {
	t.Helper()
	n, _ := runtime.BlockProfile(nil)
	for {
		r := make([]runtime.BlockProfileRecord, n+16)
		var ok bool
		n, ok = runtime.BlockProfile(r)
		if ok {
			return n
		}
	}
}

// profileNames are the artefacts the probe window must produce, and which the
// effect window must not.
var profileNames = []string{"cpu.pb.gz", "mutex.pb.gz", "block.pb.gz", "goroutine.pb.gz"}

// TestObserveWritesProfiles checks the instrument produces the artefacts the
// hunt depends on, that they are non-trivial, and — the other half, without
// which "unprofiled window" is only a label — that the effect window produces
// none of them.
func TestObserveWritesProfiles(t *testing.T) {
	probeDir := t.TempDir()
	probe, err := contention.Observe(trivialWorkload(4000), 8, contention.WindowProbe, probeDir)
	if err != nil {
		t.Fatalf("Observe(probe): %v", err)
	}

	for _, name := range profileNames {
		fi, err := os.Stat(filepath.Join(probeDir, name))
		if err != nil {
			t.Errorf("probe window is missing profile %s: %v", name, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("profile %s is empty", name)
		}
	}

	effectDir := t.TempDir()
	effect, err := contention.Observe(trivialWorkload(4000), 8, contention.WindowEffect, effectDir)
	if err != nil {
		t.Fatalf("Observe(effect): %v", err)
	}
	for _, name := range profileNames {
		if _, err := os.Stat(filepath.Join(effectDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("effect window wrote %s (err=%v); the unprofiled window must write no profiles", name, err)
		}
	}

	if !probe.Profiled {
		t.Error("probe window reports Profiled=false")
	}
	if effect.Profiled {
		t.Error("effect window reports Profiled=true")
	}
	for _, m := range []contention.Metrics{effect, probe} {
		label := "effect"
		if m.Profiled {
			label = "probe"
		}
		if m.Errors != 0 {
			t.Errorf("%s window reported %d errors, want 0", label, m.Errors)
		}
		if m.OpsPerSec <= 0 {
			t.Errorf("%s throughput %.1f, want > 0", label, m.OpsPerSec)
		}
		if m.P99Nanos <= 0 {
			t.Errorf("%s p99 %d ns, want > 0", label, m.P99Nanos)
		}
		if m.Level != 8 {
			t.Errorf("%s level = %d, want 8", label, m.Level)
		}
		// Every level must run the workload's full declared op count. Integer
		// division alone used to drop up to level-1 operations silently.
		if m.Ops != 4000 {
			t.Errorf("%s ran %d operations, want the declared 4000", label, m.Ops)
		}
	}
}

// TestObserveSeesKnownContention is the positive control. The self-test
// workload serialises every operation on one mutex, so at high concurrency the
// mutex profile MUST attribute blocked time to that lock. Without this, a clean
// mutex profile from a real workload would prove nothing — it could equally
// mean the instrument is blind.
//
// It runs in a CHILD PROCESS. That is not incidental: the mutex profile is
// cumulative for a process's whole lifetime, so a control executed in the
// shared parent inherits every earlier test's samples. An earlier revision of
// this file made exactly that mistake, and the negative control below caught
// it by "detecting" contention in a workload that has no shared state at all.
//
// The assertion deliberately does NOT match on the string "contention": this
// package is itself named contention, so every frame in it contains that
// substring and such a check could hardly fail.
func TestObserveSeesKnownContention(t *testing.T) {
	top := runControl(t, controlContended, 16)
	// go1.27 names the closure trivialWorkload.1.1; earlier toolchains used
	// trivialWorkload.func1.1. Match the stable prefix, which is still specific
	// to the contended fixture (the negative control's is uncontendedWorkload).
	const wantFrame = "trivialWorkload."
	if !strings.Contains(top, wantFrame) {
		t.Errorf("mutex profile does not attribute the DELIBERATE contention to %s.\n"+
			"The instrument may be blind; a clean profile elsewhere would be meaningless.\n%s",
			wantFrame, top)
	}
	if strings.Contains(top, "of 0 total") {
		t.Errorf("mutex profile is empty despite a deliberately contended mutex.\n%s", top)
	}
	// The ranking must name a concrete file:line lock site, not an aggregate
	// total. This is the criterion the default flat ranking failed: on a real
	// module workload its top four entries were sync.(*Mutex).Unlock,
	// sync.(*RWMutex).Unlock, sync.(*RWMutex).RUnlock and runtime.unlock, with
	// no file and no line anywhere in the output.
	if !strings.Contains(top, "observatory_test.go:") {
		t.Errorf("ranking names no file:line site in this file, so it is attributing to aggregates only.\n%s", top)
	}
}

// TestObserveNegativeControl is the other half of the control pair. An
// uncontended workload, measured in its own fresh process, must NOT produce the
// contended fixture's lock frame — otherwise the positive control proves only
// that the frame always appears, rather than that it appears BECAUSE of
// contention.
//
// Its assertion is an absence, so it is only evidence if something was
// positively observed first: an empty string satisfies "does not contain" just
// as happily as a real profile does. runControl guarantees that pprof actually
// parsed a mutex profile before the absence is read.
func TestObserveNegativeControl(t *testing.T) {
	top := runControl(t, controlUncontended, 16)
	if strings.Contains(top, "trivialWorkload.") {
		t.Errorf("uncontended workload attributed contention to the contended fixture's frame;\n"+
			"the positive control is therefore not evidence.\n%s", top)
	}
}

// TestObserveTopSitesAreStable is the reproducibility criterion. Two runs of
// the same workload at the same level, each in its own fresh process, must
// agree on the top module lock sites: a ranking that moves between runs cannot
// be used to decide whether a code change moved it.
//
// It drives a REAL module workload rather than the synthetic control. The
// question is whether GoGraph's own lock sites rank stably, and a fixture with
// a single mutex could not answer it.
func TestObserveTopSitesAreStable(t *testing.T) {
	const (
		workload = "cypher-write-mem"
		level    = 16
		topN     = 5
	)
	first := moduleSites(t, runObservation(t, workload, level), topN)
	second := moduleSites(t, runObservation(t, workload, level), topN)

	// Non-vacuity. Two empty rankings compare equal, so without this the test
	// would pass most loudly in exactly the case where the instrument had
	// stopped attributing anything at all.
	const wantAtLeast = 3
	if len(first) < wantAtLeast || len(second) < wantAtLeast {
		t.Fatalf("ranking attributed too few module sites to constitute a stability test: %d and %d, want >= %d each\n first: %v\nsecond: %v",
			len(first), len(second), wantAtLeast, first, second)
	}
	if !slices.Equal(first, second) {
		t.Errorf("top-%d module lock sites differ between two runs of %s at level %d:\n first: %v\nsecond: %v",
			topN, workload, level, first, second)
	}
}

// runControl executes one control fixture in a fresh child process and returns
// the ranking of the mutex profile it produced.
func runControl(t *testing.T, fixture string, level int) string {
	t.Helper()
	dir := t.TempDir()
	_, stderr, err := subproc.RunWithTimeout(t, 5*time.Minute, controlMode,
		fixture, strconv.Itoa(level), dir)
	if err != nil {
		t.Fatalf("control child %s: %v\nstderr: %s", fixture, err, stderr)
	}
	return mutexTop(t, filepath.Join(dir, "mutex.pb.gz"))
}

// runObservation performs the probe window of one registered workload in a
// fresh child process and returns the ranking of its mutex profile.
func runObservation(t *testing.T, workload string, level int) string {
	t.Helper()
	dir := t.TempDir()
	_, stderr, err := subproc.RunWithTimeout(t, 10*time.Minute, childMode,
		workload, strconv.Itoa(level), string(contention.WindowProbe), dir)
	if err != nil {
		t.Fatalf("child %s@%d: %v\nstderr: %s", workload, level, err, stderr)
	}
	return mutexTop(t, filepath.Join(dir, "mutex.pb.gz"))
}

// mutexTop ranks one mutex profile, failing the test if the tool cannot read
// it.
//
// The only sanctioned skip is a genuinely absent Go toolchain. An earlier
// revision skipped on ANY pprof error, which silently deleted both controls
// whenever the profile was unreadable — a corrupt or truncated profile, the
// exact failure the controls exist to catch, reported green.
func mutexTop(t *testing.T, profile string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain unavailable, cannot rank profiles: %v", err)
	}
	top, err := pprofTop(t, profile)
	if err != nil {
		t.Fatalf("go tool pprof failed on %s: %v\n%s", profile, err, top)
	}
	// pprof prints this header for every mutex profile it successfully parsed,
	// including a legitimately empty one. It is what makes an assertion of the
	// form "the output does NOT contain X" mean something.
	if !strings.Contains(top, "Type: delay") {
		t.Fatalf("pprof output is not a parsed mutex profile:\n%s", top)
	}
	return top
}

var errNever = errors.New("unreachable: guards the negative control's loop against elimination")

// pprofTop shells out to the canonical tool rather than hand-decoding the
// profile, so the numbers in any report are the tool's numbers.
//
// The ranking is by CUMULATIVE time at LINE granularity, and neither is
// cosmetic. A mutex profile's FLAT time belongs almost entirely to the release
// site inside the standard library, so the default flat ranking is a list of
// aggregate totals: measured on cypher-write-mem at level 16, its top four
// entries were sync.(*Mutex).Unlock (43%), sync.(*RWMutex).Unlock (36%),
// sync.(*RWMutex).RUnlock (13%) and runtime.unlock (8%), with no file:line
// anywhere and every module frame at flat zero. Ranking the same profile by
// cumulative time at line granularity puts graph/lpg/lpg.go:1342 and
// cypher/api.go:18333 at the top. Attribution to a concrete lock site in the
// module is the entire purpose of the instrument.
func pprofTop(t *testing.T, profile string) (string, error) {
	t.Helper()
	return runCmd(t, "go", "tool", "pprof", "-top", "-cum", "-lines", "-nodecount=25", profile)
}

// moduleSites extracts the first n GoGraph "function file:line" sites from a
// pprof ranking, skipping the observatory's own frames: the harness is not the
// subject of the measurement.
func moduleSites(t *testing.T, top string, n int) []string {
	t.Helper()
	const modulePath = "github.com/FlavioCFOliveira/GoGraph/"
	sites := make([]string, 0, n)
	for _, line := range strings.Split(top, "\n") {
		if !strings.Contains(line, modulePath) || strings.Contains(line, "/bench/contention") {
			continue
		}
		var fn, loc string
		for _, f := range strings.Fields(line) {
			switch {
			case strings.HasPrefix(f, modulePath):
				fn = f
			case strings.Contains(f, ".go:"):
				loc = f
			}
		}
		if fn == "" || loc == "" {
			continue
		}
		sites = append(sites, fn+" "+loc)
		if len(sites) == n {
			break
		}
	}
	return sites
}

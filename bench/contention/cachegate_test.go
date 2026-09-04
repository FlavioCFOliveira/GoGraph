package contention_test

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// # The defect this file closes
//
// A re-taken A-vs-A noise floor returned nine ratios byte-identical to a run
// ten minutes earlier, INCLUDING its reported 102.86 s duration, in a window
// that finished in under a second. Go's test cache had served the whole thing.
// The only tell was "(cached)" in place of the elapsed time on the final "ok"
// line, and nothing in the harness looked at it.
//
// That is a measurement-integrity defect, not an inconvenience: the replay
// carries a plausible duration, plausible throughput and plausible latency, and
// republishes numbers taken under host conditions that no longer hold.
//
// Reproduced on this tree with go1.27.1 darwin/arm64, one workload at level 1:
//
//	GOGRAPH_CONTENTION_SWEEP_DIR=/tmp/e3a GOGRAPH_CONTENTION_LEVELS=1 \
//	GOGRAPH_CONTENTION_WORKLOADS=metrics-emit \
//	go test -run TestSweep -v ./bench/contention/     # ok ... 0.624s
//	                                                  # effect=3504928.8/s
//	go test -run TestSweep -v ./bench/contention/     # ok ... (cached)
//	                                                  # effect=3504928.8/s
//
// The second command's whole output — the per-row throughput, the reported
// 0.22 s test duration, everything — was the first command's, and summary.tsv
// on disk kept its original mtime because no child process ever ran.
//
// # Why a detector cannot do this job
//
// On a cache hit cmd/go does not execute the test binary at all: it prints the
// stored output and returns (cmd/go/internal/test/test.go, the `if r.c.buf !=
// nil` branch that returns before the binary is spawned). No code inside the
// package can therefore observe its own replay. Every in-process detector —
// a nonce compared against the wall clock, a loadavg bracket the harness must
// have written itself, an artefact freshness check — is circular: it only runs
// on the runs that were never the problem.
//
// The single point of control is cmd/go's own decision, taken BEFORE the run,
// about whether this invocation's result is eligible to be stored.
//
// # The handshake this gate reads
//
// cmd/go tells the test binary that decision, and it does so unconditionally:
//
//	execCmd := work.FindExecCmd()
//	testlogArg := []string{}
//	if !r.c.disableCache && len(execCmd) == 0 {
//	        testlogArg = []string{"-test.testlogfile=" + a.Objdir + "testlog.txt"}
//	}
//
// (go1.27.1, src/cmd/go/internal/test/test.go:1592-1595.) The flag is the file
// the test binary writes its consulted files and environment variables to, and
// cmd/go asks for it exactly when it intends to key a cache entry off them:
// `saveOutput` reads that same `testlog.txt` and abandons the save when it is
// missing (test.go:2168-2176). So:
//
//	-test.testlogfile non-empty  =>  a PASS here will be stored and can later
//	                                 be replayed in place of a measurement.
//	-test.testlogfile empty      =>  this result can never enter the cache.
//
// Measured on this toolchain rather than assumed (see
// [TestGoTestCacheReplaysAnUngatedMeasurement], which re-establishes it on
// every run):
//
//	go test -run T -v ./.            -> TESTLOGFILE="/…/b001/testlog.txt"
//	go test -run T -v -count=1 ./.   -> TESTLOGFILE=""
//	go test -run T -v               -> TESTLOGFILE=""   (local directory mode:
//	                                    caching does not apply, test.go:1795)
//
// # Why refusing beats detecting
//
// cmd/go stores a result only on success — `saveOutput` is called in the branch
// that prints "ok", and the failure branch below it never calls it
// (test.go:1742). A measurement that FAILS whenever cmd/go would have cached it
// therefore leaves no cache entry behind, so no invocation can ever be answered
// from one. The set of runs that publish numbers and the set of runs that are
// cacheable become disjoint by construction, which is stronger than detection:
// there is nothing left to detect.
//
// That is the difference between IMPOSSIBLE and MERELY DETECTED, and it is why
// this gate clears the bar. A detector would have to run in order to notice a
// replay, and a replay is precisely the case in which it does not run. This
// gate instead removes the cache entry's ability to exist: every run that could
// have been stored fails, every run that publishes numbers was ineligible for
// storage before it started, and so no invocation of a measurement entry point
// can ever be answered from the cache.
//
// Confirmed on this tree: two consecutive `go test ./bench/contention/` runs of
// a gated campaign both re-executed and both failed at the gate; neither said
// "(cached)".
//
// # What this gate deliberately does NOT do
//
// It does not fire when the campaign is not running. The measurement entry
// points skip when their output directory is unset, and a cached skip publishes
// nothing, so the gate is placed after that skip. `go test ./bench/contention/`
// and `make ci` are unaffected.

// cacheableRunFlag is the flag cmd/go passes when, and only when, it intends to
// store this run's result in the test cache. See the notes above.
//
// It is registered by testing.Init as "write test action log to file (for use
// only by cmd/go)", so it is always present under `go test`; its absence means
// the handshake this gate depends on has changed, which is handled as unsafe
// rather than as permission to proceed.
const cacheableRunFlag = "test.testlogfile"

// cacheGateMarker is the first line of the refusal. It is a fixed string so the
// gate's own tests can assert that THIS check fired rather than some later one.
const cacheGateMarker = "CACHEABLE MEASUREMENT REFUSED"

// runIsCacheable reports whether cmd/go will store this run's result, given the
// value of [cacheableRunFlag] and whether that flag is registered at all.
//
// It is separated from [requireFreshRun] so both answers can be tested
// directly: a gate whose negative branch is never exercised is a gate nobody
// has checked.
//
// The unregistered case is reported as cacheable — the fail-closed answer. A
// gate that cannot establish that a run is safe must refuse it; treating an
// unrecognised toolchain as permission would turn the one silent failure this
// file exists to prevent back on.
func runIsCacheable(value string, registered bool) (cacheable bool, why string) {
	if !registered {
		return true, fmt.Sprintf(
			"the -%s flag is not registered by this toolchain, so whether cmd/go "+
				"will cache this run cannot be established", cacheableRunFlag)
	}
	if value != "" {
		return true, fmt.Sprintf(
			"cmd/go passed -%s=%s, which it does only when it intends to store "+
				"this run's result in the test cache", cacheableRunFlag, value)
	}
	return false, ""
}

// requireFreshRun aborts the calling measurement unless cmd/go has already
// committed to not caching its result.
//
// Call it at the top of every entry point that publishes numbers, immediately
// after the skip that decides the campaign is running at all.
func requireFreshRun(t *testing.T) {
	t.Helper()

	value, registered := "", false
	if f := flag.Lookup(cacheableRunFlag); f != nil {
		value, registered = f.Value.String(), true
	}

	cacheable, why := runIsCacheable(value, registered)
	if !cacheable {
		return
	}
	t.Fatalf(`%s

%s.

A cacheable run is not a measurement. On a later invocation with the same test
binary, the same cacheable flags and the same environment variables, cmd/go
would print this run's throughput, latency and duration again without executing
anything, and the only tell would be "(cached)" on the ok line.

Re-run with -count=1, which is the documented way to disable test caching:

    go test -count=1 -v -run %s ./bench/contention/`,
		cacheGateMarker, why, t.Name())
}

// ─── the gate's own proof ────────────────────────────────────────────────────

// TestRunIsCacheableDecision pins both branches of the decision, including the
// fail-closed one, which no ordinary run reaches.
func TestRunIsCacheableDecision(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		registered bool
		want       bool
	}{
		{"count=1 clears the flag", "", true, false},
		{"cmd/go asked for a testlog", "/tmp/go-build1/b001/testlog.txt", true, true},
		{"flag gone: fail closed", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := runIsCacheable(tc.value, tc.registered)
			if got != tc.want {
				t.Fatalf("runIsCacheable(%q, %v) = %v, want %v", tc.value, tc.registered, got, tc.want)
			}
			if got && why == "" {
				t.Error("a refusal must carry a reason")
			}
			if !got && why != "" {
				t.Errorf("a fresh run must carry no reason, got %q", why)
			}
		})
	}
}

// TestMeasurementEntryPointsRefuseACacheableRun drives the REAL entry points
// through `go test` and shows that the second invocation publishes nothing.
//
// Each entry point is run three times with one variable changed:
//
//   - twice with no -count=1, where the gate must fire and neither run may be
//     answered from the cache;
//   - once with -count=1, where the gate must NOT fire — proved by the run
//     reaching the next check and failing with ITS message instead.
//
// The directory handed to the entry points is deliberately relative, so the
// -count=1 arm stops at the pre-existing "must be absolute" check. No workload
// is set up and no child process is spawned in any arm, so the test costs a few
// process launches and cannot perturb a campaign running on the same host.
func TestMeasurementEntryPointsRefuseACacheableRun(t *testing.T) {
	goBin := requireGoToolchain(t)

	entries := []struct {
		test   string
		envVar string
	}{
		{"TestSweep", "GOGRAPH_CONTENTION_SWEEP_DIR"},
		{"TestCeilingProbe", "GOGRAPH_CONTENTION_CEILING_DIR"},
		{"TestTransportNoiseFloor", envTransportFloorDir},
		{"TestTransportAB", envTransportABDir},
	}

	for _, e := range entries {
		t.Run(e.test, func(t *testing.T) {
			env := contentionEnv(map[string]string{e.envVar: "deliberately-relative"})
			args := []string{"test", "-run", "^" + e.test + "$", "-v", "./bench/contention/"}

			for attempt := 1; attempt <= 2; attempt++ {
				out := runGo(t, goBin, repoRoot(t), env, args...)
				if !strings.Contains(out, cacheGateMarker) {
					t.Fatalf("attempt %d: gate did not fire; output:\n%s", attempt, out)
				}
				if answeredFromCache(out) {
					t.Fatalf("attempt %d: cmd/go answered from the cache:\n%s", attempt, out)
				}
				if !strings.Contains(out, "FAIL") {
					t.Fatalf("attempt %d: expected a failing run; output:\n%s", attempt, out)
				}
			}

			// Single variable changed: -count=1.
			fresh := runGo(t, goBin, repoRoot(t), env, append([]string{"test", "-count=1"}, args[1:]...)...)
			if strings.Contains(fresh, cacheGateMarker) {
				t.Fatalf("-count=1 must clear the gate; output:\n%s", fresh)
			}
			if !strings.Contains(fresh, "must be absolute") {
				t.Fatalf("-count=1 run did not reach the check after the gate; output:\n%s", fresh)
			}
		})
	}
}

// TestGoTestCacheReplaysAnUngatedMeasurement is the control arm.
//
// It builds a throwaway module holding two tests that differ ONLY by the gate,
// and shows on this toolchain, in this session, that:
//
//  1. the ungated one is replayed from cache on its second invocation, and the
//     replay republishes the FIRST run's nonce — the defect, reproduced;
//  2. the gated one refuses instead, twice, and is never replayed;
//  3. the gated one runs and publishes a fresh nonce under -count=1 — so the
//     gate blocks replays, not measurements.
//
// Without arm 1 the other two would prove nothing: a gate that never had a
// replay to prevent is untested. Arm 1 also re-establishes the cmd/go handshake
// [requireFreshRun] depends on, so a toolchain that stopped passing
// -test.testlogfile when it caches — which would make the gate silently
// permissive — fails here loudly instead.
func TestGoTestCacheReplaysAnUngatedMeasurement(t *testing.T) {
	goBin := requireGoToolchain(t)
	dir := writeProbeModule(t)
	env := append(os.Environ(), "GOWORK=off", "PROBE_ARMED=1")

	run := func(args ...string) string {
		return runGo(t, goBin, dir, env, args...)
	}

	// Arm 1 — no gate: the second invocation is a replay.
	first := run("test", "-run", "^TestUngatedMeasurement$", "-v", "./.")
	n1 := probeNonce(t, first)
	if answeredFromCache(first) {
		t.Fatalf("the first invocation was itself a replay; output:\n%s", first)
	}
	second := run("test", "-run", "^TestUngatedMeasurement$", "-v", "./.")
	if !answeredFromCache(second) {
		t.Fatalf("expected cmd/go to replay the ungated measurement, so that the "+
			"gate has something to prevent; output:\n%s", second)
	}
	if n2 := probeNonce(t, second); n2 != n1 {
		t.Fatalf("a replay must republish the stored nonce: got %q, first run wrote %q", n2, n1)
	}

	// Arm 2 — same code plus the gate: refused, twice, never replayed.
	for attempt := 1; attempt <= 2; attempt++ {
		out := run("test", "-run", "^TestGatedMeasurement$", "-v", "./.")
		if !strings.Contains(out, cacheGateMarker) {
			t.Fatalf("attempt %d: gate did not fire; output:\n%s", attempt, out)
		}
		if answeredFromCache(out) {
			t.Fatalf("attempt %d: a refusal was cached; output:\n%s", attempt, out)
		}
		if strings.Contains(out, "MEASUREMENT nonce=") {
			t.Fatalf("attempt %d: a refused run published a number; output:\n%s", attempt, out)
		}
	}

	// Arm 3 — the gate is not a blanket refusal.
	fresh := run("test", "-run", "^TestGatedMeasurement$", "-count=1", "-v", "./.")
	if strings.Contains(fresh, cacheGateMarker) {
		t.Fatalf("-count=1 must clear the gate; output:\n%s", fresh)
	}
	n3 := probeNonce(t, fresh)
	if n3 == n1 {
		t.Fatalf("the -count=1 run republished the stored nonce %q", n1)
	}
}

// ─── helpers for the two end-to-end tests ────────────────────────────────────

// requireGoToolchain returns the go binary to drive the child runs with, and
// establishes the two preconditions the arms depend on.
//
// Both skips are environment preconditions, not unfinished work: without a `go`
// binary there is nothing to drive, and with a non-empty GOFLAGS the child
// invocations no longer differ only by the flag under test — GOFLAGS=-count=1
// in particular would clear the gate in the arms that must trip it, and the
// failure would look like a broken gate rather than a broken precondition.
func requireGoToolchain(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	out, err := exec.Command(goBin, "env", "GOFLAGS").Output() //nolint:gosec // G204: resolved from PATH, fixed arguments
	if err != nil {
		t.Fatalf("go env GOFLAGS: %v", err)
	}
	if flags := strings.TrimSpace(string(out)); flags != "" {
		t.Skipf("GOFLAGS=%q would confound the -count=1 comparison this test rests on", flags)
	}
	return goBin
}

// repoRoot returns the module root, which is the parent of this package's
// directory. The child `go test` invocations name ./bench/contention/ from
// there, because caching does not apply in local directory mode
// (src/cmd/go/internal/test/test.go:1795) and the arms need it to apply.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// contentionEnv builds a child environment in which every variable the
// measurement entry points read has a known value, so an ambient campaign in
// the parent's environment cannot change what the child does.
func contentionEnv(set map[string]string) []string {
	known := []string{
		"GOGRAPH_CONTENTION_SWEEP_DIR",
		"GOGRAPH_CONTENTION_CEILING_DIR",
		"GOGRAPH_CONTENTION_CEILING_PAIRS",
		"GOGRAPH_CONTENTION_CEILING_REPEATS",
		"GOGRAPH_CONTENTION_LEVELS",
		"GOGRAPH_CONTENTION_WORKLOADS",
		"GOGRAPH_CONTENTION_OPS_SCALE",
		envTransportFloorDir,
		envTransportABDir,
		envTransportReplicas,
		envTransportLevels,
		envTransportQueries,
		envTransportArms,
	}
	drop := make(map[string]bool, len(known))
	for _, k := range known {
		drop[k] = true
	}
	env := make([]string, 0, len(os.Environ())+len(set)+1)
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GOWORK=off")
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}

// runGo runs one child `go` invocation and returns its combined output. A
// non-zero exit is expected in most arms, so the status is left to the caller
// to read from the output it asserts on.
func runGo(t *testing.T, goBin, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(goBin, args...) //nolint:gosec // G204: fixed argument list, go binary resolved from PATH
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return string(out)
}

// answeredFromCache reports whether cmd/go replayed a stored result, read from
// the PACKAGE RESULT LINE rather than from the output as a whole.
//
// A substring search for "(cached)" is wrong here and was measured to be wrong:
// the gate's own refusal message quotes the marker while explaining it, so a
// naive search reported a cache hit on the very runs that had just refused to
// produce one. The verdict lives in one place only — the tab-separated
// "ok\t<package>\t(cached)" summary cmd/go prints last.
func answeredFromCache(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "ok") && !strings.HasPrefix(line, "FAIL") {
			continue
		}
		for _, field := range strings.Split(line, "\t") {
			if strings.TrimSpace(field) == "(cached)" {
				return true
			}
		}
	}
	return false
}

// probeNonce extracts the single "MEASUREMENT nonce=..." line the throwaway
// module prints, and fails when there is not exactly one.
func probeNonce(t *testing.T, out string) string {
	t.Helper()
	const prefix = "MEASUREMENT nonce="
	var found []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			found = append(found, strings.TrimPrefix(line, prefix))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %q line, got %d; output:\n%s", prefix, len(found), out)
	}
	return found[0]
}

// writeProbeModule writes the throwaway module used by the control arm and
// returns its directory. The two tests differ only by the gate, and the nonce
// they print is unique per execution, so a repeated value is proof that no
// execution happened.
func writeProbeModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	gomod := "module cachegateprobe\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	src := fmt.Sprintf(`package cachegateprobe

import (
	"flag"
	"fmt"
	"os"
	"testing"
	"time"
)

// nonceSalt makes this module's build unique per test run, so the arms below
// cannot be answered from a cache entry left by an earlier session.
const nonceSalt = %q

func armed(t *testing.T) {
	t.Helper()
	if os.Getenv("PROBE_ARMED") == "" {
		t.Skip("PROBE_ARMED unset")
	}
}

func publish() {
	fmt.Printf("MEASUREMENT nonce=%%s-%%d\n", nonceSalt, time.Now().UnixNano())
}

func TestUngatedMeasurement(t *testing.T) {
	armed(t)
	publish()
}

func TestGatedMeasurement(t *testing.T) {
	armed(t)
	f := flag.Lookup(%q)
	if f == nil || f.Value.String() != "" {
		t.Fatalf(%q)
	}
	publish()
}
`, fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()), cacheableRunFlag, cacheGateMarker)

	if err := os.WriteFile(filepath.Join(dir, "probe_test.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write probe_test.go: %v", err)
	}
	return dir
}

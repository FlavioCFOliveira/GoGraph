package exprof

import (
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"testing"
)

// These tests must not run in parallel with one another: the CPU profiler and
// the tracer are process-global, so two concurrent sessions would contend for a
// single resource and the "is it actually running?" oracles below would be
// meaningless.

// cpuProfilerBusy reports whether a CPU profile is currently active, by asking
// the runtime to start a second one. This is the oracle that distinguishes "a
// file was created" from "the profiler is genuinely running": StartCPUProfile
// returns an error when a profile is already in progress, and nil otherwise (in
// which case we immediately stop the one we just started).
func cpuProfilerBusy() bool {
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		return true
	}
	pprof.StopCPUProfile()
	return false
}

// tracerBusy is the same oracle for runtime/trace.
func tracerBusy() bool {
	if err := trace.Start(io.Discard); err != nil {
		return true
	}
	trace.Stop()
	return false
}

// gunzip decompresses a pprof artefact. runtime/pprof writes gzip-compressed
// protobuf, so a file that does not decompress is not a profile at all.
func gunzip(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("%s is not gzip-compressed (len=%d)", path, len(raw))
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gunzip %s: %v", path, err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s decompressed to nothing", path)
	}
	return out
}

// burnCPU does enough work for the CPU profiler to collect at least one sample.
func burnCPU() {
	x := 0
	for i := range 3_000_000 {
		x += i % 7
	}
	_ = x
}

// TestInert is the property every example's regression test depends on: with
// neither flag set, nothing happens and not one byte is written.
func TestInert(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer

	cfg := &Config{}
	if cfg.Enabled() {
		t.Fatal("zero Config reports Enabled")
	}
	if err := cfg.Run(&buf, func() error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("inert session wrote %d bytes: %q", buf.Len(), buf.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("inert session created %d entries", len(entries))
	}
	if cpuProfilerBusy() {
		t.Fatal("inert session left the CPU profiler running")
	}
}

// TestNilConfigIsInert covers the call site that never binds the flags at all.
func TestNilConfigIsInert(t *testing.T) {
	var cfg *Config
	if cfg.Enabled() {
		t.Fatal("nil Config reports Enabled")
	}
	var buf bytes.Buffer
	if err := cfg.Run(&buf, func() error { return nil }); err != nil {
		t.Fatalf("Run on nil Config: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("nil Config wrote %q", buf.String())
	}
}

// TestCPUAndHeapProfiles asserts the artefacts are real profiles of the right
// kind, that the profiler genuinely ran, and that it genuinely stopped.
func TestCPUAndHeapProfiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "prof")
	cfg := &Config{Dir: dir}
	if !cfg.Enabled() {
		t.Fatal("Config with Dir set reports not Enabled")
	}

	var buf bytes.Buffer
	sess, err := cfg.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !cpuProfilerBusy() {
		t.Fatal("Start did not actually start the CPU profiler")
	}
	burnCPU()
	if err := sess.Finish(&buf); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if cpuProfilerBusy() {
		t.Fatal("Finish did not stop the CPU profiler")
	}

	cpu := gunzip(t, filepath.Join(dir, CPUProfileName))
	if !bytes.Contains(cpu, []byte("nanoseconds")) || !bytes.Contains(cpu, []byte("samples")) {
		t.Error("cpu.pprof lacks the CPU profile sample types")
	}
	heap := gunzip(t, filepath.Join(dir, HeapProfileName))
	if !bytes.Contains(heap, []byte("inuse_space")) {
		t.Error("heap.pprof lacks the inuse_space sample type")
	}
	// The two must not be the same kind of profile written twice.
	if bytes.Contains(cpu, []byte("inuse_space")) {
		t.Error("cpu.pprof looks like a heap profile")
	}

	out := buf.String()
	for _, want := range []string{"# pprof.cpu=", "# pprof.heap="} {
		if !strings.Contains(out, want) {
			t.Errorf("telemetry missing %q; got %q", want, out)
		}
	}
	// Every reported line must be telemetry, so a regression test that pins
	// deterministic facts is unaffected by profiling being switched on.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "# ") {
			t.Errorf("non-telemetry line %q", line)
		}
	}
}

// TestTrace asserts the tracer runs, stops, and leaves a file with the Go
// execution-trace header.
func TestTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.trace")
	cfg := &Config{Trace: path}

	var buf bytes.Buffer
	sess, err := cfg.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !tracerBusy() {
		t.Fatal("Start did not actually start the tracer")
	}
	burnCPU()
	if err := sess.Finish(&buf); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if tracerBusy() {
		t.Fatal("Finish did not stop the tracer")
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- test-controlled temp path.
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	// The v2 execution-trace format opens with "go <version> trace". Assert the
	// stable parts, not the version, so a toolchain bump does not fail this.
	if !bytes.HasPrefix(raw, []byte("go ")) || !bytes.Contains(raw[:min(32, len(raw))], []byte("trace")) {
		t.Fatalf("trace header unexpected: %q", raw[:min(32, len(raw))])
	}
	if !strings.Contains(buf.String(), "# trace="+path) {
		t.Errorf("telemetry missing the trace path; got %q", buf.String())
	}
	// Tracing alone must not produce a heap profile.
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), HeapProfileName)); !os.IsNotExist(err) {
		t.Error("trace-only session wrote a heap profile")
	}
}

// TestFinishIsIdempotent pins the property that lets an error path and a success
// path both call Finish without coordinating.
func TestFinishIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	sess, err := (&Config{Dir: dir}).Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	burnCPU()

	var first, second bytes.Buffer
	if err := sess.Finish(&first); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, HeapProfileName)) // #nosec G304 -- test temp path.
	if err != nil {
		t.Fatalf("read heap: %v", err)
	}

	if err := sess.Finish(&second); err != nil {
		t.Fatalf("second Finish: %v", err)
	}
	if second.Len() != 0 {
		t.Errorf("second Finish wrote %q; want nothing", second.String())
	}
	after, err := os.ReadFile(filepath.Join(dir, HeapProfileName)) // #nosec G304 -- test temp path.
	if err != nil {
		t.Fatalf("re-read heap: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("second Finish rewrote the heap profile")
	}
	if first.Len() == 0 {
		t.Error("first Finish wrote no telemetry")
	}
}

// TestRunStopsProfilersWhenTheWorkloadFails is the whole reason Run exists:
// examples end a failed run with log.Fatal, and os.Exit skips deferred calls, so
// a profile taken over a failing run would otherwise be truncated.
func TestRunStopsProfilersWhenTheWorkloadFails(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "run.trace")
	cfg := &Config{Dir: dir, Trace: tracePath}

	sentinel := errors.New("workload failed")
	var buf bytes.Buffer
	err := cfg.Run(&buf, func() error {
		burnCPU()
		if !cpuProfilerBusy() {
			t.Error("CPU profiler not running inside Run")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run returned %v; want the workload error", err)
	}
	if cpuProfilerBusy() {
		t.Fatal("Run left the CPU profiler running after a failed workload")
	}
	if tracerBusy() {
		t.Fatal("Run left the tracer running after a failed workload")
	}
	// The artefacts must still be complete and readable.
	gunzip(t, filepath.Join(dir, CPUProfileName))
	gunzip(t, filepath.Join(dir, HeapProfileName))
	if fi, err := os.Stat(tracePath); err != nil || fi.Size() == 0 {
		t.Fatalf("trace not written on the failure path: %v", err)
	}
}

// TestRunFailsFastOnASetupError pins the first half of the two-phase rule: when
// the profilers cannot be started the workload must not run at all, because the
// operator asked for evidence that will not exist.
func TestRunFailsFastOnASetupError(t *testing.T) {
	// A -profile-dir whose path is occupied by a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	var buf bytes.Buffer
	ran := false
	err := (&Config{Dir: blocker}).Run(&buf, func() error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("Run succeeded with an unusable profile dir")
	}
	if ran {
		t.Error("Run called the workload after the profilers failed to start")
	}
	if buf.Len() != 0 {
		t.Errorf("a failed setup wrote telemetry: %q", buf.String())
	}
	if cpuProfilerBusy() {
		t.Fatal("a failed Start left the CPU profiler running")
	}
}

// TestRunReportsTeardownErrorOnlyWhenTheWorkloadSucceeded pins the second half:
// once the workload has run, its error outranks any profiling error.
func TestRunReportsTeardownErrorOnlyWhenTheWorkloadSucceeded(t *testing.T) {
	// Removing the profile directory after Start makes the heap profile — which
	// Finish writes — impossible to create, while the CPU profile started fine.
	newCfg := func(t *testing.T) *Config {
		t.Helper()
		return &Config{Dir: filepath.Join(t.TempDir(), "prof")}
	}

	t.Run("workload succeeds: teardown error surfaces", func(t *testing.T) {
		cfg := newCfg(t)
		var buf bytes.Buffer
		err := cfg.Run(&buf, func() error {
			return os.RemoveAll(cfg.Dir)
		})
		if err == nil {
			t.Fatal("Run hid the teardown failure")
		}
		if cpuProfilerBusy() {
			t.Fatal("a failed Finish left the CPU profiler running")
		}
	})

	t.Run("workload fails: workload error wins", func(t *testing.T) {
		cfg := newCfg(t)
		sentinel := errors.New("workload failed")
		var buf bytes.Buffer
		err := cfg.Run(&buf, func() error {
			if rmErr := os.RemoveAll(cfg.Dir); rmErr != nil {
				t.Fatalf("remove: %v", rmErr)
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("Run returned %v; the workload error must win", err)
		}
		if cpuProfilerBusy() {
			t.Fatal("a failed Finish left the CPU profiler running")
		}
	})
}

// TestStartLeavesNothingRunningWhenTheTraceFails covers the partial-start path:
// the CPU profile is already running when the trace file cannot be created.
func TestStartLeavesNothingRunningWhenTheTraceFails(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Dir: dir, Trace: filepath.Join(dir, "no-such-dir", "x.trace")}
	if _, err := cfg.Start(); err == nil {
		t.Fatal("Start succeeded with an uncreatable trace path")
	}
	if cpuProfilerBusy() {
		t.Fatal("a failed Start left the CPU profiler running")
	}
	if tracerBusy() {
		t.Fatal("a failed Start left the tracer running")
	}
}

// TestBind pins the flag names and the inert default, which is the contract
// every example inherits.
func TestBind(t *testing.T) {
	fs := flag.NewFlagSet("example", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := Bind(fs)

	for _, name := range []string{"profile-dir", "trace"} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("Bind did not register -%s", name)
		}
		if f.DefValue != "" {
			t.Errorf("-%s defaults to %q; must default to inert", name, f.DefValue)
		}
		if f.Usage == "" {
			t.Errorf("-%s has no usage text", name)
		}
	}
	if err := fs.Parse([]string{"-profile-dir", "/tmp/p", "-trace", "/tmp/t.trace"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Dir != "/tmp/p" || cfg.Trace != "/tmp/t.trace" {
		t.Fatalf("Bind filled %+v", cfg)
	}
	if !cfg.Enabled() {
		t.Error("parsed Config reports not Enabled")
	}
}

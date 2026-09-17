// Package exprof gives every program under examples/ one identical profiling
// contract: a -profile-dir flag that writes cpu.pprof and heap.pprof, a -trace
// flag that writes a runtime/trace, and a -contention flag that adds
// mutex.pprof, block.pprof and goroutine.pprof.
//
// # Responsibility
//
// exprof owns the *instrumentation* of an example: binding the three flags,
// starting and stopping the profilers in the correct order, setting and
// restoring the contention sampling rates, and reporting the artefact paths as
// telemetry. It owns nothing else. It never touches GoGraph, never interprets an
// example's workload, and never writes anything to the example's output when no
// flag is set.
//
// The reason it exists as one package rather than as a copy inside each example
// is that the contract must be *identical* across all of them: a reader who
// learns -profile-dir on one example knows it on every other. Thirty-seven
// hand-maintained copies would be free to drift apart silently; one
// implementation cannot.
//
// # Inert by default
//
// With no flag set every operation is a no-op: no directory is created, no
// profiler runs, no sampling rate is touched, and not one byte reaches the
// example's writer. This is what lets each example's regression test pin its
// deterministic output unedited.
//
// # Why contention is opt-in, and separate
//
// The mutex and block profilers are not free the way the CPU profiler is. Both
// accumulate for as long as their rate is non-zero, and at the resolution worth
// attributing — rate 1, every event recorded — the block profiler charges a
// stack walk to every blocking operation the workload performs. That cost lands
// on the very workload a CPU profile is trying to attribute, so it must never be
// paid by default or implicitly: -profile-dir alone buys CPU and heap and
// nothing else, and a CPU-attribution run is unchanged by this package's
// extension.
//
// -contention therefore requires -profile-dir rather than implying it. Asking
// for contention profiles with nowhere to write them is a usage error, and it is
// reported as one at setup rather than silently ignored.
//
// # Ordering, and why it matters
//
// Four orderings here are load-bearing and are the reason this is a package and
// not a dozen lines at each call site:
//
//   - The CPU profile is stopped *before* the heap profile is written, so the
//     profiler's own teardown allocations are not attributed to the workload.
//   - runtime.GC runs before the heap profile, because a heap profile reports
//     what was live as of the last collection. Without it the profile counts
//     garbage that is merely unswept, which reads as a leak that is not there.
//   - The contention rates are raised *before* the CPU profiler starts and
//     dropped only *after* the profiles that depend on them are written, since
//     both profilers accumulate for exactly as long as their rate is set. A
//     narrower window would under-count the workload's own contention.
//   - The mutex and block profiles are written BEFORE the CPU profile is
//     stopped, and this one is not a nicety. pprof.StopCPUProfile blocks on a
//     channel receive while it drains the profiler's buffer, and with the block
//     rate still live that wait is recorded AS BLOCKING. Measured on this host
//     with the writes ordered the other way round, examples/09_leiden — a
//     single-goroutine program — reported 200.27 ms of block delay of which
//     100% was runtime.chanrecv1 under stopCPU. The instrument was reporting
//     itself. Writing the two contention profiles first costs only that their
//     own small write appears in the CPU profile, where it is visible and
//     attributable instead of swamping the measurement.
//   - The goroutine profile is written from the same Finish, after the workload
//     and after the forced collection, which means it is a POST-workload
//     snapshot. That makes it evidence of goroutine *retention* — what the
//     workload failed to shut down — and not of peak concurrency. Measured, its
//     single sample is usually runtime.goroutineProfileWithLabels itself, which
//     is the correct reading of "nothing was left running". An example that
//     needs the peak must capture it at its own peak; examples/23_bolt_server
//     does exactly that.
//
// # Usage
//
// The common shape is [Config.Run], which guarantees the profilers are stopped
// even when the workload fails — a plain defer would not, because examples end
// in log.Fatal, and os.Exit does not run deferred calls:
//
//	func main() {
//		cfg := defaultConfig()
//		flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of nodes")
//		prof := exprof.Bind(flag.CommandLine)
//		flag.Parse()
//
//		if err := prof.Run(os.Stdout, func() error {
//			return run(context.Background(), os.Stdout, cfg)
//		}); err != nil {
//			log.Fatal(err)
//		}
//	}
//
// Examples that drive several batteries, or that must stop the CPU profile at a
// point of their own choosing, use [Config.Start] and [Session.Finish] directly.
//
// # Concurrency
//
// A Config is bound and read on one goroutine before the workload starts and is
// not safe for concurrent mutation. A Session's Finish is safe to call from any
// goroutine and is idempotent, so an error path and a success path may both call
// it. The underlying profilers are process-global: at most one Session may be
// active at a time, which is the natural shape for a single-purpose example
// binary. The two contention sampling rates are process-global as well; a
// Session restores the mutex fraction it found and resets the block rate, which
// is all the runtime allows (see [Session.Finish]).
package exprof

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sync"
)

// Artefact basenames written into the -profile-dir. They are fixed rather than
// configurable so that a reader, a script, or a later cycle finds the profile of
// any example at the same path without consulting that example's flags.
const (
	CPUProfileName  = "cpu.pprof"
	HeapProfileName = "heap.pprof"
	// The three below are written only when -contention is set.
	MutexProfileName     = "mutex.pprof"
	BlockProfileName     = "block.pprof"
	GoroutineProfileName = "goroutine.pprof"
)

// dirPerm is the mode for a created -profile-dir: owner and group only, since a
// profile discloses the program's call graph and, through it, its inputs.
const dirPerm = 0o750

// Config is the profiling destination set an example binds from its flags.
// The zero Config is valid and inert.
type Config struct {
	// Dir is -profile-dir: the directory to write cpu.pprof and heap.pprof
	// into. Empty disables both CPU and heap profiling.
	Dir string
	// Trace is -trace: the file to write a runtime/trace into. Empty disables
	// tracing.
	Trace string
	// Contention is -contention: the sampling rate for the mutex and block
	// profilers. Zero — the default — disables both and writes no goroutine
	// profile either. A positive value additionally requires Dir, because there
	// would otherwise be nowhere to write the three artefacts; Start reports
	// that combination as a setup error rather than ignoring it.
	//
	// The value is passed to BOTH runtime.SetMutexProfileFraction and
	// runtime.SetBlockProfileRate. The two units differ — a fraction of
	// contention events for the first, nanoseconds spent blocked per sample for
	// the second — but they share the direction that matters to an operator: 1
	// records every event, and a larger value trades resolution for overhead.
	Contention int
}

// Bind registers -profile-dir and -trace on fs and returns the Config they
// fill. Call it before fs.Parse.
//
// The help text is defined here, once, so it reads identically in every
// example's -h output.
func Bind(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.Dir, "profile-dir", "",
		"if set, write "+CPUProfileName+" and "+HeapProfileName+" here (attribute CPU and "+
			"allocations to call sites; inspect with: go tool pprof -http=:0 <file>)")
	fs.StringVar(&c.Trace, "trace", "",
		"if set, write a runtime/trace here (scheduling, blocking and GC over the "+
			"workload's timeline; inspect with: go tool trace <file>)")
	fs.IntVar(&c.Contention, "contention", 0,
		"if > 0, also write "+MutexProfileName+", "+BlockProfileName+" and "+
			GoroutineProfileName+" into -profile-dir, sampling the mutex and block "+
			"profilers at this rate (1 records every event; larger samples less). "+
			"Requires -profile-dir. Off by default: both profilers charge the "+
			"workload for their sampling, which a CPU-attribution run must not pay")
	return c
}

// Enabled reports whether any profiler is requested.
func (c *Config) Enabled() bool {
	return c != nil && (c.Dir != "" || c.Trace != "" || c.Contention != 0)
}

// Session is an active profiling run. Finish must be called before the process
// exits, or the CPU profile is truncated and the trace is unreadable.
type Session struct {
	cfg Config
	cpu *os.File
	tr  *os.File
	// prevMutex is the runtime's mutex profile fraction as it was before this
	// session raised it, captured so Finish can put it back. It is meaningful
	// only when contention is true.
	prevMutex  int
	contention bool
	once       sync.Once
	err        error
}

// Start begins the profilers the Config selects. It must be called after
// flag.Parse and before any work that should be attributed.
//
// When the Config is inert Start allocates nothing observable and returns a
// Session whose Finish is a no-op, so the call site needs no branch.
func (c *Config) Start() (*Session, error) {
	s := &Session{}
	if c == nil {
		return s, nil
	}
	s.cfg = *c

	if c.Contention < 0 {
		return nil, fmt.Errorf("contention rate %d: must not be negative", c.Contention)
	}
	if c.Contention > 0 && c.Dir == "" {
		return nil, fmt.Errorf(
			"contention rate %d needs -profile-dir: there is nowhere to write %s, %s and %s",
			c.Contention, MutexProfileName, BlockProfileName, GoroutineProfileName)
	}

	// Raise the contention rates first. Both profilers accumulate for as long as
	// their rate is non-zero, so starting them before the CPU profiler is what
	// makes their window cover the whole of the workload rather than a suffix of
	// it. Nothing is written here; the profiles are read at Finish.
	if c.Contention > 0 {
		s.prevMutex = runtime.SetMutexProfileFraction(c.Contention)
		runtime.SetBlockProfileRate(c.Contention)
		s.contention = true
	}

	if c.Dir != "" {
		if err := os.MkdirAll(c.Dir, dirPerm); err != nil {
			return nil, fmt.Errorf("profile dir %q: %w", c.Dir, err)
		}
		// #nosec G304 -- operator-supplied -profile-dir with a fixed basename.
		f, err := os.Create(filepath.Join(c.Dir, CPUProfileName))
		if err != nil {
			s.dropContention()
			return nil, fmt.Errorf("create cpu profile: %w", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			s.dropContention()
			return nil, fmt.Errorf("start cpu profile: %w", err)
		}
		s.cpu = f
	}

	if c.Trace != "" {
		// #nosec G304 -- operator-supplied -trace path.
		f, err := os.Create(c.Trace)
		if err != nil {
			s.stopCPU()
			s.dropContention()
			return nil, fmt.Errorf("create trace: %w", err)
		}
		if err := trace.Start(f); err != nil {
			_ = f.Close()
			s.stopCPU()
			s.dropContention()
			return nil, fmt.Errorf("start trace: %w", err)
		}
		s.tr = f
	}
	return s, nil
}

// dropContention puts the contention sampling rates back. The mutex fraction is
// restored to whatever this session found; the block rate is RESET to the
// documented default of 0, because the runtime exposes no getter for it and a
// reset is therefore the most faithful thing available. It is a no-op when this
// session never raised them, and it is called on the Start error paths as well
// as from Finish so that a failed setup leaves the process as it found it.
func (s *Session) dropContention() {
	if !s.contention {
		return
	}
	runtime.SetMutexProfileFraction(s.prevMutex)
	runtime.SetBlockProfileRate(0)
	s.contention = false
}

// writeProfile writes the named runtime/pprof profile into dir under file.
//
// The profile is written in the protobuf format (debug=0) rather than the
// human-readable one, because these artefacts are read by go tool pprof and by
// the sweep driver, and only the protobuf form carries the sample values a
// driver can check for emptiness.
func writeProfile(dir, file, name string) error {
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("pprof.Lookup(%q) returned nil", name)
	}
	// #nosec G304 -- operator-supplied directory with a fixed basename.
	f, err := os.Create(filepath.Join(dir, file))
	if err != nil {
		return fmt.Errorf("create %s profile: %w", name, err)
	}
	if err := p.WriteTo(f, 0); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s profile: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s profile: %w", name, err)
	}
	return nil
}

// stopCPU stops and closes the CPU profile if one is running. It is not
// idempotent on its own; Finish serialises it through s.once.
func (s *Session) stopCPU() {
	if s.cpu == nil {
		return
	}
	pprof.StopCPUProfile()
	_ = s.cpu.Close()
	s.cpu = nil
}

// Finish stops the CPU profile and the trace, writes the heap profile — and,
// when -contention was set, the mutex, block and goroutine profiles — restores
// the contention sampling rates, and reports the artefact paths to w as
// telemetry lines (prefixed with "# ", so an example's regression test ignores
// them).
//
// The goroutine profile it writes is a POST-workload snapshot: it answers which
// goroutines the workload left behind, not how many ran at its peak.
//
// It is idempotent: the profilers are stopped and the heap profile written on
// the first call only, and every later call returns that same result. This lets
// an error path and a success path both call it without coordinating.
//
// Finish writes nothing and returns nil when the session is inert.
func (s *Session) Finish(w io.Writer) error {
	s.once.Do(func() { s.err = s.finish(w) })
	return s.err
}

func (s *Session) finish(w io.Writer) error {
	// Deferred, so the rates are dropped only once every profile that depends on
	// them has been written — and dropped even if writing one of them fails.
	defer s.dropContention()

	// The two contention profiles come FIRST, while the CPU profiler is still
	// running, because stopping it blocks on a channel and that wait would
	// otherwise dominate the block profile. See the ordering note in the package
	// doc for the measurement that established this.
	if s.contention {
		for _, a := range [...]struct{ file, name string }{
			{MutexProfileName, "mutex"},
			{BlockProfileName, "block"},
		} {
			if err := writeProfile(s.cfg.Dir, a.file, a.name); err != nil {
				s.stopCPU()
				return err
			}
			fmt.Fprintf(w, "# pprof.%s=%s\n", a.name, filepath.Join(s.cfg.Dir, a.file))
		}
	}

	// Stop the CPU profile next so the profiler's teardown allocations are not
	// attributed to the workload by the heap profile taken below.
	s.stopCPU()
	if s.tr != nil {
		trace.Stop()
		_ = s.tr.Close()
		s.tr = nil
		fmt.Fprintf(w, "# trace=%s\n", s.cfg.Trace)
	}
	if s.cfg.Dir == "" {
		return nil
	}

	// A heap profile reports what was LIVE as of the last collection, so collect
	// first; otherwise the profile attributes merely-unswept garbage.
	runtime.GC()
	path := filepath.Join(s.cfg.Dir, HeapProfileName)
	// #nosec G304 -- operator-supplied directory with a fixed basename.
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create heap profile: %w", err)
	}
	if err := pprof.WriteHeapProfile(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("write heap profile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close heap profile: %w", err)
	}
	fmt.Fprintf(w, "# pprof.cpu=%s\n", filepath.Join(s.cfg.Dir, CPUProfileName))
	fmt.Fprintf(w, "# pprof.heap=%s\n", path)

	if !s.contention {
		return nil
	}

	// The goroutine profile comes last, deliberately: taken here it answers what
	// the workload LEFT RUNNING once everything else has been shut down and
	// collected, which is the question a goroutine-leak mandate asks. Taken any
	// earlier it would also show this teardown's own transient goroutines.
	if err := writeProfile(s.cfg.Dir, GoroutineProfileName, "goroutine"); err != nil {
		return err
	}
	fmt.Fprintf(w, "# pprof.goroutine=%s\n", filepath.Join(s.cfg.Dir, GoroutineProfileName))
	return nil
}

// Run starts the profilers, calls fn, and stops them — whether fn succeeds or
// fails. It is the shape every example should use unless it needs to control
// where the CPU profile stops.
//
// It exists because the alternative is a hazard rather than a preference:
// examples end a failed run with log.Fatal, os.Exit does not run deferred calls,
// and so a deferred stop would silently truncate the profile on exactly the runs
// worth profiling.
//
// Errors follow the two-phase rule:
//
//   - A SETUP failure is fail-fast: if the profilers cannot be started, fn is
//     not called at all and the error is returned. The operator asked for
//     evidence, so spending the workload's whole runtime only to report at the
//     end that no profile exists would be fail-silent in the way that matters.
//   - A TEARDOWN failure is subordinate: once fn has run, its error takes
//     precedence, so a problem writing a profile can never mask a workload
//     failure.
func (c *Config) Run(w io.Writer, fn func() error) error {
	s, err := c.Start()
	if err != nil {
		return err
	}
	runErr := fn()
	finErr := s.Finish(w)
	if runErr != nil {
		return runErr
	}
	return finErr
}

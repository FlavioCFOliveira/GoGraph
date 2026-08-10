// Package exprof gives every program under examples/ one identical profiling
// contract: a -profile-dir flag that writes cpu.pprof and heap.pprof, and a
// -trace flag that writes a runtime/trace.
//
// # Responsibility
//
// exprof owns the *instrumentation* of an example: binding the two flags,
// starting and stopping the profilers in the correct order, and reporting the
// artefact paths as telemetry. It owns nothing else. It never touches GoGraph,
// never interprets an example's workload, and never writes anything to the
// example's output when both flags are unset.
//
// The reason it exists as one package rather than as a copy inside each example
// is that the contract must be *identical* across all of them: a reader who
// learns -profile-dir on one example knows it on every other. Thirty-seven
// hand-maintained copies would be free to drift apart silently; one
// implementation cannot.
//
// # Inert by default
//
// With neither flag set every operation is a no-op: no directory is created, no
// profiler runs, and not one byte reaches the example's writer. This is what
// lets each example's regression test pin its deterministic output unedited.
//
// # Ordering, and why it matters
//
// Two orderings here are load-bearing and are the reason this is a package and
// not four lines at each call site:
//
//   - The CPU profile is stopped *before* the heap profile is written, so the
//     profiler's own teardown allocations are not attributed to the workload.
//   - runtime.GC runs before the heap profile, because a heap profile reports
//     what was live as of the last collection. Without it the profile counts
//     garbage that is merely unswept, which reads as a leak that is not there.
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
// binary.
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
)

// dirPerm is the mode for a created -profile-dir: owner and group only, since a
// profile discloses the program's call graph and, through it, its inputs.
const dirPerm = 0o750

// Config is the profiling destination pair an example binds from its flags.
// The zero Config is valid and inert.
type Config struct {
	// Dir is -profile-dir: the directory to write cpu.pprof and heap.pprof
	// into. Empty disables both CPU and heap profiling.
	Dir string
	// Trace is -trace: the file to write a runtime/trace into. Empty disables
	// tracing.
	Trace string
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
	return c
}

// Enabled reports whether either profiler is requested.
func (c *Config) Enabled() bool {
	return c != nil && (c.Dir != "" || c.Trace != "")
}

// Session is an active profiling run. Finish must be called before the process
// exits, or the CPU profile is truncated and the trace is unreadable.
type Session struct {
	cfg  Config
	cpu  *os.File
	tr   *os.File
	once sync.Once
	err  error
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

	if c.Dir != "" {
		if err := os.MkdirAll(c.Dir, dirPerm); err != nil {
			return nil, fmt.Errorf("profile dir %q: %w", c.Dir, err)
		}
		// #nosec G304 -- operator-supplied -profile-dir with a fixed basename.
		f, err := os.Create(filepath.Join(c.Dir, CPUProfileName))
		if err != nil {
			return nil, fmt.Errorf("create cpu profile: %w", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("start cpu profile: %w", err)
		}
		s.cpu = f
	}

	if c.Trace != "" {
		// #nosec G304 -- operator-supplied -trace path.
		f, err := os.Create(c.Trace)
		if err != nil {
			s.stopCPU()
			return nil, fmt.Errorf("create trace: %w", err)
		}
		if err := trace.Start(f); err != nil {
			_ = f.Close()
			s.stopCPU()
			return nil, fmt.Errorf("start trace: %w", err)
		}
		s.tr = f
	}
	return s, nil
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

// Finish stops the CPU profile and the trace, writes the heap profile, and
// reports the artefact paths to w as telemetry lines (prefixed with "# ", so an
// example's regression test ignores them).
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
	// Stop the CPU profile first so the profiler's teardown allocations are not
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

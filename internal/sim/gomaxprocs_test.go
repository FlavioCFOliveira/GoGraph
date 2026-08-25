package sim

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gomaxprocs_test.go — the regression battery for rmp #2613: a scenario that
// mutates process-global `GOMAXPROCS` must never run beside another scenario.
//
// The defect it pins was found by a DST exercise on 2026-08-25 (11141 runs at
// `-workers=3`): 38 failures, every one of them caused by `cpu-starvation`
// clamping the process to one core while a neighbour on another swarm worker
// was mid-run. Holding the seeds fixed and varying ONLY whether `cpu-starvation`
// ran alongside them moved `bolt-decode-swarm` from 0/37 to 37/37.
//
// Each test here carries its own RED CONTROL — an arm that removes the guard and
// asserts the detector fires. Without it a green result would prove only that
// the two never overlapped, which is exactly the vacuous pass this suite exists
// to reject.

// clampProbe models a clamping scenario without touching `GOMAXPROCS` at all: it
// publishes the fact that it is inside its critical section, which is the
// property the scheduler must exclude. Mutating the real knob here would clamp
// the whole test binary and redden every parallel test in the package — the very
// hazard under repair.
type clampProbe struct {
	inClamp  atomic.Int64
	breaches atomic.Int64
	samples  atomic.Int64
	clamps   atomic.Int64
}

// scenarios returns the clamping scenario and the observing scenario. When
// guarded is false the clamper does NOT take the exclusive hold, which is the
// pre-fix behaviour and must be detected.
func (p *clampProbe) scenarios(guarded bool) (clamper, observer Scenario) {
	clamper = Scenario{
		Name:             "zz-clamper",
		ClampsGOMAXPROCS: true,
		run: func(context.Context, uint64) (*SimReport, error) {
			if guarded {
				defer holdGOMAXPROCSExclusive()()
			}
			p.clamps.Add(1)
			p.inClamp.Add(1)
			time.Sleep(20 * time.Millisecond)
			p.inClamp.Add(-1)
			return nil, nil
		},
	}
	observer = Scenario{
		Name: "zz-observer",
		run: func(context.Context, uint64) (*SimReport, error) {
			for range 40 {
				p.samples.Add(1)
				if p.inClamp.Load() > 0 {
					p.breaches.Add(1)
				}
				time.Sleep(500 * time.Microsecond)
			}
			return nil, nil
		},
	}
	return clamper, observer
}

// alternatingSelector sends even run indices to the clamper and odd ones to the
// observer, so both are drawn in every swarm regardless of coverage bias.
type alternatingSelector struct{ even, odd string }

func (s alternatingSelector) Select(runIndex int, _ string) string {
	if runIndex%2 == 0 {
		return s.even
	}
	return s.odd
}

// runClampUnscheduled runs the two scenario bodies DIRECTLY and concurrently,
// bypassing [Scenario.Run] and therefore the lock entirely. It is the red
// control: overlap is guaranteed by construction, so a probe that reports no
// breach here is a broken detector rather than a working guard.
//
// It must not route through the swarm. [Scenario.Run] now takes the shared hold
// for every non-clamping scenario, so a sibling test holding the exclusive side
// would block the observer and manufacture a clean control — which is exactly
// what this control exists to rule out.
func runClampUnscheduled(t *testing.T) *clampProbe {
	t.Helper()

	probe := &clampProbe{}
	clamper, observer := probe.scenarios(false)

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for range 10 {
			if _, err := clamper.run(context.Background(), 0); err != nil {
				t.Errorf("clamper: %v", err)
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for range 10 {
			if _, err := observer.run(context.Background(), 0); err != nil {
				t.Errorf("observer: %v", err)
				return
			}
		}
	}()
	<-done
	<-done
	return probe
}

// runClampSwarm drives a swarm of both scenarios across several workers and
// returns the probe's tallies.
func runClampSwarm(t *testing.T, guarded bool) *clampProbe {
	t.Helper()

	probe := &clampProbe{}
	clamper, observer := probe.scenarios(guarded)
	reg, err := NewRegistry(clamper, observer)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sw, err := NewSwarm(reg, &SwarmConfig{
		MasterSeed: 20260825,
		Scenario:   observer.Name,
		Workers:    4,
		Runs:       40,
		Selector:   alternatingSelector{even: clamper.Name, odd: observer.Name},
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}
	if _, err := sw.Run(context.Background()); err != nil {
		t.Fatalf("swarm run: %v", err)
	}

	// Non-vacuity: a swarm that never drew the clamper, or whose observer never
	// sampled, proves nothing either way.
	if got := probe.clamps.Load(); got == 0 {
		t.Fatalf("vacuous: the clamping scenario never ran")
	}
	if got := probe.samples.Load(); got == 0 {
		t.Fatalf("vacuous: the observing scenario never sampled")
	}
	return probe
}

// TestSwarm_DoesNotCoScheduleAClampingScenario is the scheduler-level pin: while
// a scenario declaring ClampsGOMAXPROCS is inside its critical section, no other
// scenario may be running.
func TestSwarm_DoesNotCoScheduleAClampingScenario(t *testing.T) {
	t.Parallel()

	t.Run("unscheduled control goes RED", func(t *testing.T) {
		t.Parallel()
		probe := runClampUnscheduled(t)
		if probe.breaches.Load() == 0 {
			t.Fatalf("control did not fire: the observer never overlapped the clamp "+
				"(clamps=%d samples=%d), so the guarded arm below would pass vacuously",
				probe.clamps.Load(), probe.samples.Load())
		}
	})

	t.Run("swarm-scheduled arm is clean", func(t *testing.T) {
		t.Parallel()
		probe := runClampSwarm(t, true)
		if got := probe.breaches.Load(); got != 0 {
			t.Fatalf("a scenario ran while a clamping scenario held the process: "+
				"%d of %d samples observed the clamp (clamps=%d)",
				got, probe.samples.Load(), probe.clamps.Load())
		}
	})
}

// TestCPUStarvation_HoldsGOMAXPROCSExclusively pins the production path itself:
// `runCPUStarvation` must hold [gomaxprocsMu] exclusively for the whole clamped
// run, so a concurrent shared holder cannot acquire while it is in flight.
//
// Before rmp #2613 the clamp took no lock at all, so the run completed straight
// through the shared hold and this test fails on that build.
func TestCPUStarvation_HoldsGOMAXPROCSExclusively(t *testing.T) {
	t.Parallel()

	// Holding the shared side here makes the result ATTRIBUTABLE: only this
	// goroutine's hold can be what blocks the run. Polling TryRLock instead would
	// also go green when some OTHER test's clamper happened to hold the lock — a
	// pass for the wrong reason.
	release := holdGOMAXPROCSShared()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := runCPUStarvation(context.Background(), 0xC9057A40); err != nil {
			t.Errorf("runCPUStarvation: %v", err)
		}
	}()

	// The run takes ~108 ms measured. This window is ~28x that, so a build whose
	// clamp ignores the lock finishes well inside it. Erring long only ever costs
	// a false GREEN on an absurdly loaded machine, never a false red.
	const unguardedWouldFinishIn = 3 * time.Second
	select {
	case <-done:
		release()
		t.Fatalf("runCPUStarvation completed while a shared hold was held: the clamp is " +
			"not exclusive, so a co-resident scenario would inherit the single core")
	case <-time.After(unguardedWouldFinishIn):
	}

	// Liveness: it must then complete once the shared hold is dropped, so the
	// test cannot be satisfied by a permanently stuck run.
	release()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("runCPUStarvation did not complete after the shared hold was released")
	}
}

// boltDecodeSwarmSeedsThatFailedUnderAForeignClamp are seeds drawn from the
// 2026-08-25 exercise's failing population. Every one of them failed
// `nv-swarm-rejections` / `nv-swarm-pressure-density` while a co-resident
// `cpu-starvation` held the process at one core, and every one passes when run
// without that neighbour — the failure was never a property of the seed.
var boltDecodeSwarmSeedsThatFailedUnderAForeignClamp = []uint64{
	5292452270951340161,
	1081440142160788339,
	6301757959833132467,
	12854603939195957804,
}

// TestBoltDecodeSwarm_SurvivesAConcurrentClamp drives the REAL production
// pairing: `bolt-decode-swarm` on seeds that actually failed, concurrently with
// the real `cpu-starvation`, in one process.
//
// This is the end-to-end form of the defect. On the pre-fix build the clamp made
// the pool pressure unconstructible and all four seeds reported
// ORACLE_DEVIATION; the exclusion must now make the pairing a non-event.
func TestBoltDecodeSwarm_SurvivesAConcurrentClamp(t *testing.T) {
	t.Parallel()

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	victim, ok := reg.Lookup(ScenarioBoltDecodeSwarm)
	if !ok {
		t.Fatalf("scenario %q not in the catalogue", ScenarioBoltDecodeSwarm)
	}
	clamper, ok := reg.Lookup(ScenarioCPUStarvation)
	if !ok {
		t.Fatalf("scenario %q not in the catalogue", ScenarioCPUStarvation)
	}

	// The neighbour runs continuously for as long as the victims do, so every
	// victim run overlaps at least one clamp attempt.
	stop := make(chan struct{})
	neighbourDone := make(chan struct{})
	var clampRuns atomic.Int64
	go func() {
		defer close(neighbourDone)
		for seed := uint64(0); ; seed++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := clamper.Run(context.Background(), 0xC9057A40+seed); err != nil {
				t.Errorf("cpu-starvation neighbour: %v", err)
				return
			}
			clampRuns.Add(1)
		}
	}()

	var failures []string
	for _, seed := range boltDecodeSwarmSeedsThatFailedUnderAForeignClamp {
		report, err := victim.Run(context.Background(), seed)
		if err != nil {
			failures = append(failures, fmt.Sprintf("seed %d: harness error: %v", seed, err))
			continue
		}
		if report != nil {
			failures = append(failures, fmt.Sprintf("seed %d: %s", seed, report.String()))
		}
	}

	close(stop)
	<-neighbourDone

	// Non-vacuity: if the neighbour never completed a clamp, nothing was tested.
	if got := clampRuns.Load(); got == 0 {
		t.Fatalf("vacuous: the cpu-starvation neighbour never completed a run")
	}
	if len(failures) > 0 {
		t.Fatalf("%d of %d seeds failed beside a concurrent clamp (%d clamp runs):\n%s",
			len(failures), len(boltDecodeSwarmSeedsThatFailedUnderAForeignClamp),
			clampRuns.Load(), strings.Join(failures, "\n"))
	}
}

// TestGOMAXPROCS_ConcurrentClampersRestoreTheOriginal drives the package's TWO
// real clamp paths against each other and asserts the process is left exactly as
// it was found.
//
// Unsynchronised, their save/restore pairs can interleave and strand the process
// at the wrong value — A saves 8 and sets 1, B saves 1 and sets 4, A restores 8,
// B restores 1 — after which every later test in the binary runs on one core and
// any claim needing real parallelism passes vacuously. That is the fail-silent
// class this suite exists to catch (rmp #2606).
func TestGOMAXPROCS_ConcurrentClampersRestoreTheOriginal(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	if before < 2 {
		t.Skipf("needs a multi-core process to distinguish a stranded clamp (GOMAXPROCS=%d)", before)
	}

	// Each cpu-starvation run is ~108 ms and the two clampers now serialise, so
	// the round count is what this test costs the short layer. Six is enough to
	// interleave repeatedly while keeping the test a few seconds.
	const rounds = 6
	errs := make(chan error, 2*rounds)
	done := make(chan struct{}, 2)

	// Clamper A: the cpu-starvation path, which takes the exclusive hold itself.
	go func() {
		defer func() { done <- struct{}{} }()
		for range rounds {
			if _, err := runCPUStarvation(context.Background(), 0xC9057A40); err != nil {
				errs <- err
			}
		}
	}()

	// Clamper B: the pagerank-ranker path, whose caller owns the hold across the
	// clamped phase exactly as RunPageRankRanker does.
	go func() {
		defer func() { done <- struct{}{} }()
		for range rounds {
			func() {
				defer holdGOMAXPROCSExclusive()()
				if err := prWithClamp(4, func() error {
					time.Sleep(time.Millisecond)
					return nil
				}); err != nil {
					errs <- err
				}
			}()
		}
	}()

	<-done
	<-done
	close(errs)

	// prWithClamp's read-back turns a foreign clamp into an error rather than a
	// silent wrong regime, so any error here IS the interference.
	for err := range errs {
		t.Errorf("clamp interference: %v", err)
	}
	if after := runtime.GOMAXPROCS(0); after != before {
		t.Fatalf("two concurrent clampers stranded the process: GOMAXPROCS before=%d after=%d",
			before, after)
	}
}

// TestGOMAXPROCSWrites_AreAllDeclaredClampers is the structural tripwire: every
// non-test write to `runtime.GOMAXPROCS` in this package must live in one of the
// two functions known to take the exclusive hold.
//
// A new scenario that clamps without declaring ClampsGOMAXPROCS would reopen rmp
// #2613 silently — the swarm would keep co-scheduling it and the failures would
// surface as ORACLE_DEVIATION in whatever neighbour it starved, pointing away
// from the cause. This fails at the moment such a write is written.
func TestGOMAXPROCSWrites_AreAllDeclaredClampers(t *testing.T) {
	t.Parallel()

	const dir = "."
	allowed := map[string]bool{
		"runCPUStarvation": true, // catalogue.go — holds the exclusive lock itself
		"prWithClamp":      true, // pagerank_ranker.go — caller holds it across the phase
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	var found int

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isGOMAXPROCSCall(call) || len(call.Args) != 1 {
					return true
				}
				// GOMAXPROCS(0) is a READ and is always safe.
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == "0" {
					return true
				}
				found++
				if !allowed[fn.Name.Name] {
					offenders = append(offenders, fmt.Sprintf("%s: %s writes GOMAXPROCS",
						fset.Position(call.Pos()), fn.Name.Name))
				}
				return true
			})
			return false
		})
	}

	// Non-vacuity: the scan must actually have found the known writes, or a
	// broken matcher would report a clean package forever.
	if found < len(allowed) {
		t.Fatalf("vacuous scan: found %d GOMAXPROCS writes, expected at least %d", found, len(allowed))
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("GOMAXPROCS is written outside a declared clamping scenario:\n\t%s\n\n"+
			"Such a write must hold gomaxprocsMu exclusively and its scenario must set "+
			"ClampsGOMAXPROCS, or the swarm will co-schedule it (rmp #2613).",
			strings.Join(offenders, "\n\t"))
	}
}

// isGOMAXPROCSCall reports whether call is `runtime.GOMAXPROCS(...)`.
func isGOMAXPROCSCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "GOMAXPROCS" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "runtime"
}

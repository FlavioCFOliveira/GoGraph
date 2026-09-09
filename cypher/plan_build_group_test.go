package cypher

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Regression tests for rmp #2740.
//
// The defect: the plan cache absorbed a repeated query only from the SECOND
// execution onwards, so N goroutines released together all missed and all
// compiled the same text. Measured on bench/contention, the miss count equalled
// the concurrency level exactly at every rung (8, 64, 128, 192, 256, 384, 512,
// 1024). Those N parses are not independent — the generated ANTLR lexer and
// parser share one ATN for the whole process — so they serialised on a
// process-global mutex: a median 762.68 s of mutex delay inside
// parser.ParseStatement out of 762.90 s process-wide at level 1024 on
// cypher-read-scan-large, over five interleaved replicas.
//
// The fix collapses the concurrent misses into one compilation. These tests
// pin the three properties that makes it correct — one build, shared result,
// no cross-key blocking — and the wiring that puts it on the engine's miss
// path at all.

// TestPlanBuildGroupCollapsesConcurrentBuilds is the core regression: while a
// build for one key is in flight, every other caller for that key waits for it
// instead of running its own.
//
// It is deterministic, not timing-based. The leader's build parks on `release`
// and does not return until every other caller has been observed waiting, so a
// group that failed to collapse would necessarily run a second build before
// the test ever unblocks the first.
func TestPlanBuildGroupCollapsesConcurrentBuilds(t *testing.T) {
	t.Parallel()

	const followers = 64

	g := newPlanBuildGroup()
	var builds atomic.Int64
	leaderIn := make(chan struct{})
	release := make(chan struct{})
	want := newTestEntry("shared")

	build := func(string) (*planCacheEntry, error) {
		if builds.Add(1) == 1 {
			close(leaderIn)
			<-release
		}
		return want, nil
	}

	got := make([]*planCacheEntry, followers+1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		got[0], _ = g.do("k", build)
	}()

	<-leaderIn // the leader is inside build and cannot finish yet

	// Every follower must end up parked on the leader's completion. Waiting
	// for the in-flight record to acquire them is not possible from outside,
	// so instead the followers are started and the leader is held until they
	// have all had ample opportunity to run: if the group did not collapse
	// them, they would complete their own builds here and bump the counter.
	var fwg sync.WaitGroup
	for i := 1; i <= followers; i++ {
		fwg.Add(1)
		go func(i int) {
			defer fwg.Done()
			got[i], _ = g.do("k", build)
		}(i)
	}

	// Give the followers time to reach — and, on the defective behaviour, to
	// finish — their own builds. This bound only risks a FALSE PASS if it is
	// too short, never a false failure, and the assertions below are checked
	// again after every follower has actually returned.
	time.Sleep(50 * time.Millisecond)
	if n := builds.Load(); n != 1 {
		close(release)
		fwg.Wait()
		wg.Wait()
		t.Fatalf("builds while one was in flight = %d, want 1 (the followers compiled their own)", n)
	}

	close(release)
	fwg.Wait()
	wg.Wait()

	if n := builds.Load(); n != 1 {
		t.Errorf("total builds = %d, want 1", n)
	}
	for i, e := range got {
		if e != want {
			t.Errorf("caller %d got entry %p, want the shared %p", i, e, want)
		}
	}
}

// TestPlanBuildGroupSharesTheError pins that a failed compilation is shared
// with the waiters and, crucially, is NOT remembered: the next caller compiles
// again. Caching a parse failure would change observable behaviour.
func TestPlanBuildGroupSharesTheError(t *testing.T) {
	t.Parallel()

	g := newPlanBuildGroup()
	var builds atomic.Int64
	wantErr := errTestBuildFailed

	build := func(string) (*planCacheEntry, error) {
		builds.Add(1)
		return nil, wantErr
	}

	if _, err := g.do("k", build); !errors.Is(err, wantErr) {
		t.Fatalf("first do err = %v, want %v", err, wantErr)
	}
	if _, err := g.do("k", build); !errors.Is(err, wantErr) {
		t.Fatalf("second do err = %v, want %v", err, wantErr)
	}
	if n := builds.Load(); n != 2 {
		t.Errorf("builds = %d, want 2: a finished build must not be remembered", n)
	}
	if n := len(g.inflight); n != 0 {
		t.Errorf("inflight retained %d entries after the builds finished, want 0", n)
	}
}

// TestPlanBuildGroupDoesNotBlockOtherKeys pins that the group's own mutex is
// never held across a build. A compilation of one query must not delay the
// compilation — or the completion — of any other.
//
// Deterministic: the build for "a" does not return until the build for "b" has
// completed, so a group that held its lock across the build would deadlock
// rather than fail slowly.
func TestPlanBuildGroupDoesNotBlockOtherKeys(t *testing.T) {
	t.Parallel()

	g := newPlanBuildGroup()
	bDone := make(chan struct{})
	aIn := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = g.do("a", func(string) (*planCacheEntry, error) {
			close(aIn)
			<-bDone // "b" must be able to finish while "a" is still building
			return newTestEntry("a"), nil
		})
	}()

	<-aIn
	if _, err := g.do("b", func(string) (*planCacheEntry, error) {
		return newTestEntry("b"), nil
	}); err != nil {
		t.Fatalf("do(b) while a was in flight: %v", err)
	}
	close(bDone)
	wg.Wait()
}

// TestPlanBuildGroupPanickingLeaderReleasesWaiters pins that one goroutine's
// panic cannot become a hang for every other goroutine waiting on the same
// key. The panic itself still surfaces — the library does not recover to hide
// bugs — but the waiters are released and rebuild for themselves.
func TestPlanBuildGroupPanickingLeaderReleasesWaiters(t *testing.T) {
	t.Parallel()

	g := newPlanBuildGroup()
	leaderIn := make(chan struct{})
	release := make(chan struct{})
	want := newTestEntry("rebuilt")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = recover() }() // the TEST recovers, the library does not
		_, _ = g.do("k", func(string) (*planCacheEntry, error) {
			close(leaderIn)
			<-release
			panic("build exploded")
		})
	}()

	<-leaderIn

	waiter := make(chan *planCacheEntry, 1)
	go func() {
		e, _ := g.do("k", func(string) (*planCacheEntry, error) { return want, nil })
		waiter <- e
	}()

	close(release)

	select {
	case got := <-waiter:
		if got != want {
			t.Errorf("waiter got %p, want the rebuilt %p", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter did not return: a panicking leader deadlocked its followers")
	}
	wg.Wait()
}

// TestBuildPlanCacheEntryRoutesThroughTheGroup is the WIRING regression. The
// three tests above prove the group collapses builds; this one proves the
// engine's cache-miss path actually goes through it.
//
// It is white-box on purpose. Nothing observable at the API distinguishes one
// compilation from N — the LRU publishes exactly one entry either way and
// hands the same pointer to every caller — so the only honest way to pin the
// route is to plant a result on it and require that the miss path returns
// that, rather than compiling. On the pre-fix code buildPlanCacheEntry parses
// unconditionally and returns its own entry, so this fails.
func TestBuildPlanCacheEntryRoutesThroughTheGroup(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	e := NewEngine(g)

	const q = "MATCH (n) RETURN n"
	planted := newTestEntry("planted")

	done := make(chan struct{})
	close(done) // already complete: do must adopt the result without building
	e.planBuilds.mu.Lock()
	e.planBuilds.inflight[q] = &planBuild{done: done, entry: planted}
	e.planBuilds.mu.Unlock()

	got, err := e.buildPlanCacheEntry(q)
	if err != nil {
		t.Fatalf("buildPlanCacheEntry: %v", err)
	}
	if got != planted {
		t.Fatalf("buildPlanCacheEntry returned %p, want the in-flight %p: the miss path did not go through planBuildGroup", got, planted)
	}
}

// TestConcurrentFirstExecutionsAgree is the behavioural guard on the fix: a
// herd of goroutines racing the very first execution of one query must all get
// the same, correct answer. Sharing a compilation between callers may not
// change what any of them observes.
func TestConcurrentFirstExecutionsAgree(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range 20 {
		id := string(rune('a' + i))
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	e := NewEngine(g)

	const herd = 64
	counts := make([]int64, herd)
	errs := make([]error, herd)

	var wg, ready sync.WaitGroup
	start := make(chan struct{})
	wg.Add(herd)
	ready.Add(herd)
	for i := range herd {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start // release together: every one of them must MISS
			res, err := e.Run(t.Context(), "MATCH (n:N) RETURN count(n) AS c", nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer res.Close()
			for res.Next() {
				v, ok := res.Record()["c"]
				if !ok {
					errs[i] = errMissingColumn
					return
				}
				n, ok := v.(expr.IntegerValue)
				if !ok {
					errs[i] = errNotAnInt
					return
				}
				counts[i] = int64(n)
			}
			errs[i] = res.Err()
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	for i := range herd {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if counts[i] != 20 {
			t.Fatalf("goroutine %d counted %d nodes, want 20", i, counts[i])
		}
	}
}

// Sentinel errors for the tests above, declared rather than created inline so
// the assertions can compare identity.
var (
	errTestBuildFailed = errSentinel("plan build failed")
	errMissingColumn   = errSentinel("result had no column c")
	errNotAnInt        = errSentinel("count was not an integer")
)

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

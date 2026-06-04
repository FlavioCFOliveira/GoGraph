package generation

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recycle is the per-generation backing-storage sentinel used by the
// single-publisher contract test. inUse is raised by a reader across
// its access window; freed is flipped by the publisher that drains the
// generation. A correct drain flips freed only once refcount has
// reached zero, i.e. when no reader is inside its window.
type recycle struct {
	freed atomic.Bool
	inUse atomic.Int64
}

// TestPublisher_ConcurrentPublishWithDrain_NoLostDrain enforces the
// single-publisher contract from the publisher side: under concurrent
// PublishWithDrain calls every superseded generation must be drained on
// by exactly one caller, and a generation must never be "recycled"
// (modelled by recycle.freed) while a reader still holds a live
// reference to it.
//
// Without the publishMu serialisation in PublishWithDrain two publishers
// can read the same predecessor via current.Load() and each store its
// own successor; one successor overwrites the other, so the overwritten
// generation is superseded without ever being any caller's prev and is
// therefore never drained on. This manifests two ways, both asserted
// here: the shared predecessor is drained on twice while its overwritten
// sibling is drained on zero times. After the fix every superseded
// generation is drained exactly once and no reader observes a freed
// generation. Must run under -race.
func TestPublisher_ConcurrentPublishWithDrain_NoLostDrain(t *testing.T) {
	t.Parallel()

	// sentinels maps a generation to its recycle sentinel with a
	// lock-free read path (sync.Map), so neither readers nor the onDrain
	// hook serialise on a single mutex.
	var sentinels sync.Map // map[*Generation[struct{}]]*recycle
	freedFor := func(g *Generation[struct{}]) *recycle {
		if v, ok := sentinels.Load(g); ok {
			return v.(*recycle)
		}
		v, _ := sentinels.LoadOrStore(g, &recycle{})
		return v.(*recycle)
	}

	p := New(makeCSR(t, 0))
	seedGen := p.Current()

	// drainedOn counts, per generation, how many PublishWithDrain calls
	// drained it. everCurrent records every generation that was ever the
	// current one (the seed plus every successfully published next).
	var bookMu sync.Mutex
	drainedOn := make(map[*Generation[struct{}]]int)
	everCurrent := map[*Generation[struct{}]]struct{}{seedGen: {}}
	var readerViolations atomic.Int64

	// onDrain is the white-box seam: it fires with the exact predecessor
	// PublishWithDrain captured, immediately after that predecessor's
	// refcount reached zero. Recycling the predecessor here is sound — no
	// reader can hold it (refcount zero) and none can newly acquire it
	// (it is no longer current).
	p.onDrain = func(prev *Generation[struct{}]) {
		bookMu.Lock()
		drainedOn[prev]++
		bookMu.Unlock()
		rc := freedFor(prev)
		if rc.inUse.Load() != 0 {
			readerViolations.Add(1)
		}
		rc.freed.Store(true)
	}

	// Two concurrent publishers — the minimal configuration that races
	// the load-prev/store-next swap.
	const publishers = 2
	const perPublisher = 2000
	const readers = 4

	stop := make(chan struct{})

	// Light readers exercise the use-after-recycle dimension: acquire the
	// current generation, mark it in use across a short window, assert it
	// was never recycled while held, then release promptly. Releasing
	// keeps predecessors drainable so PublishWithDrain(_, 0) stays fast.
	var readerWG sync.WaitGroup
	readerWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g := p.Acquire()
				if g == nil {
					return
				}
				rc := freedFor(g)
				rc.inUse.Add(1)
				if rc.freed.Load() {
					readerViolations.Add(1)
				}
				if g.CSR() == nil {
					readerViolations.Add(1)
				}
				rc.inUse.Add(-1)
				p.Release(g)
			}
		}()
	}

	var pubWG sync.WaitGroup
	pubWG.Add(publishers)
	for w := 0; w < publishers; w++ {
		go func(seed int) {
			defer pubWG.Done()
			for i := 0; i < perPublisher; i++ {
				next, err := p.PublishWithDrain(makeCSR(t, seed+i), 0)
				if err != nil {
					t.Errorf("PublishWithDrain: %v", err)
					return
				}
				bookMu.Lock()
				everCurrent[next] = struct{}{}
				bookMu.Unlock()
			}
		}(w * 1000000)
	}

	// Hang guard: PublishWithDrain(_, 0) blocks until its predecessor
	// drains, so a regression that loses a drain could hang. Fail loudly.
	pubDone := make(chan struct{})
	go func() {
		pubWG.Wait()
		close(pubDone)
	}()
	select {
	case <-pubDone:
	case <-time.After(45 * time.Second):
		close(stop)
		readerWG.Wait()
		t.Fatal("publishers did not finish within 45s — drain livelock")
	}

	close(stop)
	readerWG.Wait()

	if v := readerViolations.Load(); v != 0 {
		t.Errorf("reader observed %d use-after-recycle / nil-CSR violations", v)
	}

	// Every generation that was ever current, except the final one, must
	// have been drained on exactly once. A lost drain shows up either as
	// a superseded generation that was never drained on (count 0) or as a
	// generation drained on twice (a collision that necessarily leaves a
	// sibling undrained).
	bookMu.Lock()
	defer bookMu.Unlock()
	final := p.Current()
	for g := range everCurrent {
		if g == final {
			if c := drainedOn[g]; c != 0 {
				t.Errorf("the final current generation was drained on %d times", c)
			}
			continue
		}
		switch drainedOn[g] {
		case 1:
			// Exactly-once: correct.
		case 0:
			t.Errorf("superseded generation %p was never drained on (lost drain)", g)
		default:
			t.Errorf("generation %p drained on %d times — a concurrent publisher lost a drain", g, drainedOn[g])
		}
	}
}

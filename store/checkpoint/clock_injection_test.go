package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// waitFor polls cond until it is true or the (real-time) safety deadline
// elapses. It exists only to bridge the asynchronous loop goroutine: the
// checkpoint COUNT is driven purely by fake-clock advances (deterministic), but
// the loop observing a tick on its select happens on another goroutine, so the
// test must wait for that observation. No fake time passes here.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCheckpoint_FakeClockDrivesCadence proves the cadence loop honours an
// injected fake clock: no real time elapses, yet advancing the fake past the
// MaxAge deadline fires exactly one periodic checkpoint per crossed window, and
// staying below the deadline fires none. This is the determinism the DST
// harness relies on.
func TestCheckpoint_FakeClockDrivesCadence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	fake := clock.NewFake(time.Unix(0, 0))
	var mu sync.Mutex
	cp := New(Config{
		Dir:      dir,
		MaxAge:   20 * time.Millisecond,
		Interval: 5 * time.Millisecond,
	}, g, w, &mu, WithClock[string, int64](fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	// The loop registers its ticker on its own goroutine after Start, so a
	// single Advance could race ahead of registration and be missed. Advancing
	// in a poll keeps re-arming the periodic ticker until the loop is up; the
	// fake clock means no real time governs the cadence. lastFire starts at the
	// zero time, so the first observed tick has an elapsed-since of "decades"
	// >= MaxAge and fires exactly one checkpoint (historical behaviour).
	advanceUntil(t, fake, func() bool { return cp.Stats().Checkpoints >= 1 },
		5*time.Millisecond, "first virtual tick checkpoint")
	if got := cp.Stats().Checkpoints; got != 1 {
		t.Fatalf("expected 1 checkpoint after first tick, got %d", got)
	}

	// After the first fire, lastFire is the fake's current time. Advancing
	// strictly less than MaxAge fires NO further checkpoint: no real wall clock
	// can leak in to satisfy the MaxAge gate. Each Advance delivers at most one
	// buffered tick (time.Ticker drop-on-backlog), so cross at most ~MaxAge-ε.
	for i := 0; i < 3; i++ { // 3 * 5ms = 15ms < 20ms since lastFire
		fake.Advance(5 * time.Millisecond)
		time.Sleep(2 * time.Millisecond) // let the loop drain this tick
	}
	if got := cp.Stats().Checkpoints; got != 1 {
		t.Fatalf("checkpoint fired before MaxAge gap under fake clock: got %d", got)
	}

	// Cross MaxAge from lastFire: the elapsed gap reaches >= 20ms and fires
	// exactly the second checkpoint.
	advanceUntil(t, fake, func() bool { return cp.Stats().Checkpoints >= 2 },
		5*time.Millisecond, "second virtual-deadline checkpoint")
	if got := cp.Stats().Checkpoints; got != 2 {
		t.Fatalf("expected exactly 2 checkpoints at the second virtual deadline, got %d", got)
	}
}

// advanceUntil repeatedly steps the fake clock by step and yields real time
// until cond holds or a safety deadline elapses. It bridges the loop
// goroutine's asynchronous ticker registration without letting real wall time
// govern the cadence: the only fires come from these advances.
func advanceUntil(t *testing.T, fake *clock.Fake, cond func() bool, step time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", msg)
		}
		fake.Advance(step)
		time.Sleep(time.Millisecond)
	}
}

// TestCheckpoint_DefaultClockIsReal proves the default path (no WithClock) keeps
// the real clock and the historical ticker behaviour: a short real-time MaxAge
// fires a checkpoint without any fake injection. This guards the
// behaviour-preserving default.
func TestCheckpoint_DefaultClockIsReal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{
		Dir:      dir,
		MaxAge:   20 * time.Millisecond,
		Interval: 5 * time.Millisecond,
	}, g, w, &mu) // no WithClock → clock.Real()
	if cp.clk == nil {
		t.Fatal("default clock not installed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	waitFor(t, func() bool { return cp.Stats().Checkpoints >= 1 }, "real-clock checkpoint")
}

// TestCheckpoint_NilClockIgnored confirms WithClock(nil) does not clear the
// default real clock.
func TestCheckpoint_NilClockIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	var mu sync.Mutex
	cp := New(Config{Dir: dir}, g, w, &mu, WithClock[string, int64](nil))
	if cp.clk == nil {
		t.Fatal("WithClock(nil) cleared the default clock")
	}
}

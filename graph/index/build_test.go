package index

import (
	"errors"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// recordSub records every change it is given, in order, so a test can assert
// exactly WHAT a catch-up replayed rather than merely how many changes it saw.
type recordSub struct {
	seen []Change
	mu   sync.Mutex
}

func (r *recordSub) Apply(c Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, c)
}
func (r *recordSub) Kind() string { return "record" }
func (r *recordSub) nodes() []graph.NodeID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]graph.NodeID, 0, len(r.seen))
	for _, c := range r.seen {
		out = append(out, c.Node)
	}
	return out
}

func setProp(node graph.NodeID) Change {
	return Change{Op: OpSetNodeProperty, Node: node, Property: 1, NewValue: "v"}
}

// TestBuildLog_CatchUpReplaysEveryChangeFannedOutDuringTheBuild is the core
// claim of rmp #2738: a change fanned out while an index is being built reaches
// that index anyway, at the instant it is registered.
func TestBuildLog_CatchUpReplaysEveryChangeFannedOutDuringTheBuild(t *testing.T) {
	t.Parallel()
	m := NewManager()

	bl := m.BeginBuild()
	// Both delivery paths must be recorded, not just the batch one.
	m.Apply(setProp(7))
	m.ApplyBatch([]Change{setProp(8), setProp(9)})
	if got := bl.Len(); got != 3 {
		t.Fatalf("build log recorded %d changes, want 3", got)
	}

	fresh := &recordSub{}
	if err := m.FinishBuild(bl, func(reg RegisterFunc) error {
		return reg("built", fresh)
	}); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	want := []graph.NodeID{7, 8, 9}
	got := fresh.nodes()
	if len(got) != len(want) {
		t.Fatalf("the catch-up replayed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the catch-up replayed %v, want %v (mutation order must be preserved)", got, want)
		}
	}
	// Registered, and now maintained by the live fan-out rather than the log.
	if _, err := m.GetIndex("built"); err != nil {
		t.Fatalf("GetIndex after FinishBuild: %v", err)
	}
	m.Apply(setProp(10))
	if n := len(fresh.nodes()); n != 4 {
		t.Fatalf("a change fanned out after FinishBuild reached the index %d times, want the index to hold 4 changes", n)
	}
}

// TestBuildLog_IndexUnderConstructionIsUnreachable pins the property the whole
// design's correctness rests on: while an index is being built it is reachable
// through NO manager accessor, so nothing can consult a half-built index.
func TestBuildLog_IndexUnderConstructionIsUnreachable(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bl := m.BeginBuild()
	m.Apply(setProp(1))

	if n := m.Count(); n != 0 {
		t.Fatalf("Count = %d during a build, want 0", n)
	}
	if names := m.ListIndexes(); len(names) != 0 {
		t.Fatalf("ListIndexes = %v during a build, want none", names)
	}
	if _, err := m.GetIndex("built"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("GetIndex during a build = %v, want ErrIndexNotFound", err)
	}
	if err := m.FinishBuild(bl, func(reg RegisterFunc) error { return reg("built", &recordSub{}) }); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	if n := m.Count(); n != 1 {
		t.Fatalf("Count = %d after FinishBuild, want 1 — the assertions above would otherwise "+
			"hold for an index that was never registered at all", n)
	}
}

// TestBuildLog_AbandonStopsRecordingAndRegistersNothing covers the failure path
// every caller defers: a cancelled or failed build must leave no trace, and must
// above all stop the manager recording into a log nobody will drain.
func TestBuildLog_AbandonStopsRecordingAndRegistersNothing(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bl := m.BeginBuild()
	m.Apply(setProp(1))
	m.AbandonBuild(bl)

	if n := bl.Len(); n != 0 {
		t.Fatalf("an abandoned log still holds %d changes, want 0", n)
	}
	m.Apply(setProp(2))
	if n := bl.Len(); n != 0 {
		t.Fatalf("the manager kept recording into an abandoned log (%d changes)", n)
	}
	if n := m.Count(); n != 0 {
		t.Fatalf("an abandoned build registered %d indexes, want 0", n)
	}
	// Idempotent, so a caller can defer it alongside FinishBuild.
	m.AbandonBuild(bl)
	m.AbandonBuild(nil)
}

// TestBuildLog_AbandonAfterFinishIsANoOp pins the deferred-cleanup idiom: every
// caller defers AbandonBuild and then calls FinishBuild on the success path, so
// abandoning an already-retired log must not disturb what was registered.
func TestBuildLog_AbandonAfterFinishIsANoOp(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bl := m.BeginBuild()
	if err := m.FinishBuild(bl, func(reg RegisterFunc) error { return reg("built", &recordSub{}) }); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	m.AbandonBuild(bl)
	if _, err := m.GetIndex("built"); err != nil {
		t.Fatalf("AbandonBuild after FinishBuild disturbed the registration: %v", err)
	}
}

// TestBuildLog_OverflowRefusesToRegister proves the saturation answer is a typed
// error and NOT a silent truncation. Registering an index built against an
// incomplete log is exactly the defect this mechanism exists to prevent, so
// overflow must fail the build rather than quietly lose the overflowed writes.
func TestBuildLog_OverflowRefusesToRegister(t *testing.T) {
	t.Parallel()
	m := NewManager()
	bl := m.BeginBuild()
	for i := 0; i <= MaxBuildLogChanges; i++ {
		m.Apply(setProp(graph.NodeID(i)))
	}
	if !bl.Overflowed() {
		t.Fatalf("the log did not overflow after %d changes", MaxBuildLogChanges+1)
	}
	called := false
	err := m.FinishBuild(bl, func(RegisterFunc) error { called = true; return nil })
	if !errors.Is(err, ErrIndexBuildOverflow) {
		t.Fatalf("FinishBuild on an overflowed log = %v, want ErrIndexBuildOverflow", err)
	}
	if called {
		t.Fatal("FinishBuild ran the registration closure despite an overflowed log")
	}
	if n := m.Count(); n != 0 {
		t.Fatalf("an overflowed build registered %d indexes, want 0", n)
	}
	// The build is retired even on the error path, so the manager stops paying to
	// record into it.
	m.Apply(setProp(1))
	if bl.Len() != 0 {
		t.Fatal("the manager kept recording into a log FinishBuild had already rejected")
	}
}

// TestBuildLog_NoChangeIsLostAcrossFinishBuild is the concurrency claim: a change
// fanned out while FinishBuild is running is either recorded (and replayed) or
// delivered to the now-registered index — never neither. Run with -race.
func TestBuildLog_NoChangeIsLostAcrossFinishBuild(t *testing.T) {
	t.Parallel()
	const writers, perWriter = 8, 200
	const want = writers * perWriter

	for attempt := 0; attempt < 20; attempt++ {
		m := NewManager()
		bl := m.BeginBuild()
		fresh := &recordSub{}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				for i := 0; i < perWriter; i++ {
					m.Apply(setProp(graph.NodeID(w*perWriter + i)))
				}
			}(w)
		}
		close(start)
		// Finish somewhere in the middle of the write storm.
		if err := m.FinishBuild(bl, func(reg RegisterFunc) error { return reg("built", fresh) }); err != nil {
			t.Fatalf("attempt %d: FinishBuild: %v", attempt, err)
		}
		wg.Wait()

		if got := len(fresh.nodes()); got != want {
			t.Fatalf("attempt %d: the index saw %d changes, want %d — a change fanned out "+
				"across FinishBuild was neither recorded nor delivered", attempt, got, want)
		}
	}
}

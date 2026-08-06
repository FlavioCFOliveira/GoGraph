package checkpoint

// concurrent_capture_test.go — rmp #2310: a checkpoint captures ONE transactional
// instant while writers keep committing.
//
// Layer: short.
//
// # The property, and why it needs its own test
//
// Until this task the capture got its consistency from EXCLUSION: phase 1 ran inside
// RunUnderCommitLock, so no transaction could commit while the image was serialised.
// That is the mechanism this sprint removes. The capture now reads every component
// through an MVCC snapshot, so the image describes the instant the snapshot was taken
// at however many transactions commit while it runs.
//
// The failure it guards against is not "the snapshot is stale" — a stale image is
// perfectly correct, it is just an older instant. It is a TORN image: components read
// at different instants, so the snapshot holds the endpoints of a transaction but not
// its edge, or an edge whose endpoint was removed. That is a state no serial schedule
// produced, and it is what a lock-free capture over live structures produces.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// TestCapture_IsOneInstantWhileWritersCommit drives writers throughout a capture and
// asserts the resulting image is internally consistent: every edge's endpoints are
// live in the same image, and the node count matches what the capture's own instant
// says.
//
// The writers create a NODE PAIR AND THE EDGE BETWEEN THEM in one transaction, which
// is the shape that makes a torn image visible: a capture that reads the mapper at one
// instant and the adjacency at another folds an edge whose endpoints it never
// recorded, or endpoints whose edge it lost.
func TestCapture_IsOneInstantWhileWritersCommit(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	// A base population, so the capture has real work to do and the writers below have
	// a window to commit inside.
	const base = 2000
	for i := 0; i < base; i++ {
		if err := g.AddNode(fmt.Sprintf("base-%05d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}

	stop := make(chan struct{})
	var committed atomic.Int64
	// Every successful commit records the INSTANT it published at. That list is the
	// test's ABSOLUTE ORACLE: the number of entries at or below the capture's instant
	// is exactly how many edges the image must hold, computed independently of
	// anything the capture did. Without it the only available check is a structural
	// inequality, and the first version of this test measured 4872 nodes against 201
	// edges — Order >= 2*Size held trivially and could not have detected a torn image.
	var mu sync.Mutex
	var instants []uint64
	var wg sync.WaitGroup
	// quiesce emulates what the commit serialiser gives the checkpointer: writers
	// hold it SHARED for the whole of their transaction, and the capture takes it
	// EXCLUSIVELY just long enough to open the instant. Nothing else is done under
	// it — the serialisation below runs with it released, while writers commit.
	//
	// It is not scaffolding to make the test pass. Opening an instant while a write
	// transaction is still open is precisely what [snapshot.ErrCaptureNotQuiesced]
	// forbids, because an id interned by that transaction sits BELOW ids later
	// transactions have already interned and committed, and dropping it leaves a
	// hole the recovered mapper rejects. Production gets this from
	// txn.Store.RunUnderCommitLock, which closes admission and drains; a test
	// driving lpg directly has to provide it, and this is the same guarantee in the
	// smallest form.
	var quiesce sync.RWMutex
	for wtr := 0; wtr < 4; wtr++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := g.NewSession()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				a := fmt.Sprintf("w%d-a-%06d", id, n)
				b := fmt.Sprintf("w%d-b-%06d", id, n)
				// ONE TRANSACTION: both endpoints and the edge. Either all three are in
				// the image or none of them is.
				quiesce.RLock()
				err := s.ApplyVersioned(func(tx lpg.WriteTx) error {
					wv := g.Writer(tx)
					if err := wv.AddNode(a); err != nil {
						return err
					}
					if err := wv.AddNode(b); err != nil {
						return err
					}
					return wv.AddEdge(a, b, 1)
				})
				quiesce.RUnlock()
				if err != nil {
					continue // a serialization conflict is retriable; keep going
				}
				// The session's floor IS this transaction's commit instant.
				mu.Lock()
				instants = append(instants, s.Floor())
				mu.Unlock()
				committed.Add(1)
			}
		}(wtr)
	}

	// Let the writers get going, so the capture genuinely overlaps them.
	waitForCommits(t, &committed, 200)

	// The instant, and ONLY the instant, is taken under the exclusive hold.
	quiesce.Lock()
	at := g.BeginRead()
	quiesce.Unlock()

	// Everything below runs with writers free to commit again — which is the
	// property under test.
	cs := csr.BuildFromAdjListAsOf(g.AdjList(),
		func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	capt, err := snapshot.CaptureGraph[string, float64](g, cs, txn.NewStringCodec(), at)
	if err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("CaptureGraph: %v", err)
	}
	instant := at.StartTS()
	g.EndRead(at)

	// Writers keep going for a while AFTER the capture, so anything they commit now
	// must be absent from an image taken at `instant`.
	waitForCommits(t, &committed, committed.Load()+200)
	close(stop)
	wg.Wait()

	// THE ABSOLUTE ORACLE. Each writer transaction created exactly one edge, so the
	// image taken at `instant` must hold exactly as many edges as there are recorded
	// commit instants at or below it. Every writer transaction that committed AFTER
	// the capture must be absent, and every one before it present — a count that is
	// too low means the capture lost committed work, and one that is too high means it
	// folded a transaction that had not committed at its instant.
	mu.Lock()
	var want uint64
	var after int
	for _, ts := range instants {
		if ts <= instant {
			want++
		} else {
			after++
		}
	}
	mu.Unlock()

	order, size := capt.Order(), capt.Size()
	if size != want {
		t.Errorf("the image taken at instant %d holds %d edges, but %d transactions had "+
			"committed at or before that instant. A capture reads ONE instant: a lower count "+
			"lost committed work, a higher one folded a transaction that had not committed "+
			"yet — either way the components were not read at the same instant",
			instant, size, want)
	}
	// The oracle must be non-trivial in BOTH directions, or a capture that simply
	// froze at the start, or one that read the present, would satisfy it.
	if want == 0 {
		t.Fatal("no writer transaction had committed by the capture instant: the capture did " +
			"not overlap the writers and this test proved nothing")
	}
	if after == 0 {
		t.Fatal("no writer transaction committed after the capture instant: nothing could " +
			"have leaked into the image, so this test proved nothing")
	}
	if capt.CommitTS() != instant {
		t.Errorf("the capture recorded instant %d but was taken at %d: recovery derives its "+
			"clock floor from this number, so it must be the instant the components were "+
			"read at", capt.CommitTS(), instant)
	}
	t.Logf("captured %d nodes / %d edges at instant %d; %d writer transactions committed "+
		"at or before it and %d after it, and none of the latter is in the image",
		order, size, instant, want, after)
}

// TestCapture_ReadingThePresentIsDetectedByTheOracle validates the instrument in the
// OTHER direction: it captures the PRESENT rather than a transactional instant and
// asserts the oracle above catches it.
//
// # Why this test exists
//
// An instrument that cannot fail on the defective build proves nothing, and this
// project has caught three of its own instruments reporting a number they could only
// ever have produced. TestCapture_IsOneInstantWhileWritersCommit passes; that alone
// does not establish that it would have failed before the fix.
//
// # Why it is deterministic and not a race
//
// The broken behaviour is reproduced WITHOUT concurrency: the instant is taken, then a
// known number of transactions commit serially, and only then is the image captured
// reading the present. A present-time image must therefore hold exactly those extra
// edges, and an instant-read image must not. There is no interleaving to get lucky
// with, so this control cannot flake in either direction — which matters, because a
// negative control that passes intermittently is indistinguishable from one that is
// asserting nothing.
func TestCapture_ReadingThePresentIsDetectedByTheOracle(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	s := g.NewSession()
	commitPair := func(tag string) {
		t.Helper()
		a, b := "a-"+tag, "b-"+tag
		if err := s.ApplyVersioned(func(tx lpg.WriteTx) error {
			wv := g.Writer(tx)
			if err := wv.AddNode(a); err != nil {
				return err
			}
			if err := wv.AddNode(b); err != nil {
				return err
			}
			return wv.AddEdge(a, b, 1)
		}); err != nil {
			t.Fatalf("commit %s: %v", tag, err)
		}
	}

	// before: the transactions the image MUST hold, whichever way it is read.
	const before, after = 20, 7
	for i := 0; i < before; i++ {
		commitPair(fmt.Sprintf("pre-%03d", i))
	}

	at := g.BeginRead()
	instant := at.StartTS()

	// after: committed AFTER the instant was taken. An image read at `instant` must
	// not hold any of these; an image reading the present holds all of them.
	for i := 0; i < after; i++ {
		commitPair(fmt.Sprintf("post-%03d", i))
	}

	// ARM A — the defective shape: build the adjacency from the present and capture
	// with no instant, which is exactly what the checkpointer did before rmp #2310.
	presentCSR := csr.BuildFromAdjList(g.AdjList())
	presentCapt, err := snapshot.CaptureGraph[string, float64](g, presentCSR, txn.NewStringCodec(), nil)
	if err != nil {
		g.EndRead(at)
		t.Fatalf("present-time CaptureGraph: %v", err)
	}

	// ARM B — the fixed shape: both the adjacency and every component resolved at the
	// captured instant.
	instantCSR := csr.BuildFromAdjListAsOf(g.AdjList(),
		func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	instantCapt, err := snapshot.CaptureGraph[string, float64](g, instantCSR, txn.NewStringCodec(), at)
	if err != nil {
		g.EndRead(at)
		t.Fatalf("instant CaptureGraph: %v", err)
	}
	g.EndRead(at)

	// The oracle: an image taken at `instant` holds exactly the transactions that had
	// committed by then, two nodes and one edge apiece.
	if got, want := instantCapt.Size(), uint64(before); got != want {
		t.Errorf("instant-read image holds %d edges at instant %d, want %d", got, instant, want)
	}
	if got, want := instantCapt.Order(), uint64(2*before); got != want {
		t.Errorf("instant-read image holds %d nodes, want %d", got, want)
	}

	// THE CONTROL. The present-time image must hold the later transactions too, and
	// the oracle must therefore reject it. If this passes, the oracle above is
	// satisfied by an image that read the present and so asserts nothing.
	if presentCapt.Size() == uint64(before) {
		t.Fatalf("a capture reading the PRESENT reported %d edges — the same count an image "+
			"taken at instant %d must report — so the oracle cannot tell the two apart and "+
			"TestCapture_IsOneInstantWhileWritersCommit proves nothing",
			presentCapt.Size(), instant)
	}
	if got, want := presentCapt.Size(), uint64(before+after); got != want {
		t.Errorf("present-time image holds %d edges, want %d (every committed transaction)", got, want)
	}
	t.Logf("instant %d: instant-read image %d nodes / %d edges; present-time image %d nodes / %d edges "+
		"— the oracle separates them by the %d transactions committed after the instant",
		instant, instantCapt.Order(), instantCapt.Size(),
		presentCapt.Order(), presentCapt.Size(), after)
}

// waitForCommits blocks until the counter reaches want, with a deadline, so the test
// never hangs on writers that failed to start.
//
// A DEADLINE and not a fixed spin: what is being waited for is another goroutine's
// progress, and an attempt count measures this machine's speed instead.
func waitForCommits(t *testing.T, c *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d transactions committed, wanted %d: the writers are not making "+
				"progress, so nothing this test asserts is about a concurrent capture",
				c.Load(), want)
		}
	}
}

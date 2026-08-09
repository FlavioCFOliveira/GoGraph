package txn

import (
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestIsolation_Commit_NoPartialTransactionObservable is the F3 isolation
// integration test for the txn layer (docs/isolation-design.md). It proves that a
// reader never observes a partially-applied multi-op transaction committed via
// [Tx.Commit].
//
// # What mechanism it pins, and why that changed (rmp #2320)
//
// The invariant is unchanged: every committed transaction sets node "a".v and
// node "b".v to the SAME value in one transaction, and a reader that reads both
// must never see the new "a".v with the old "b".v.
//
// What changed is where the guarantee comes from. Until rmp #2320 the reader used
// [lpg.Graph.View] plus unversioned accessors, and the guarantee came from the
// WRITER's side of that lock: [Tx.Commit] applied under
// [lpg.Graph.ApplyAtomically], which held the barrier exclusively, so a reader
// here and a writer could not overlap. rmp #2320 moved the apply to
// [lpg.Graph.ApplyVersioned] — a SHARED hold — so exclusion is gone and the
// guarantee is delivered by the MVCC snapshot instead: every version of one
// transaction points at one commit record, published with a single atomic store,
// so a snapshot resolves either all of the transaction or none of it.
//
// This test therefore reads through a snapshot, which is what the module now
// documents as the way to get a consistent view of DATA ([lpg.Graph.View]'s own
// doc, and the visMu field comment). Its negative control,
// TestIsolation_ViewWithUnversionedReadIsNotAtomic, pins the OTHER half — that
// View plus an unversioned accessor no longer provides this — so the move is
// recorded as a deliberate relocation of the guarantee and not as a test that was
// loosened to go green.
//
// The commit count is bounded because each commit fsyncs the WAL; the lock-free
// mechanism itself is stress-tested without I/O in
// graph/lpg/TestIsolation_ApplyAtomically_View_NoPartialReads.
func TestIsolation_Commit_NoPartialTransactionObservable(t *testing.T) {
	t.Parallel()

	g, store, cleanup := newIsolationStore(t)
	defer cleanup()

	violation, reads := runIsolationWorkload(t, g, store, func(g *lpg.Graph[string, int64]) (int64, int64, bool) {
		// A SNAPSHOT read: registered with the reclamation horizon, resolved
		// through the version chains, and released exactly once.
		snap := g.BeginRead()
		defer g.EndRead(snap)
		v := g.ReadAt(snap)
		va, oka := v.GetNodeProperty("a", "v")
		vb, okb := v.GetNodeProperty("b", "v")
		if !oka || !okb {
			return 0, 0, false
		}
		ia, _ := va.Int64()
		ib, _ := vb.Int64()
		return ia, ib, true
	})

	if violation != 0 {
		t.Fatalf("observed %d partial-transaction violations (a.v != b.v inside one snapshot) over %d reads", violation, reads)
	}
	if reads == 0 {
		t.Fatal("readers never observed both properties; test did not exercise the invariant")
	}
}

// TestIsolation_ViewWithUnversionedReadIsNotAtomic is the NEGATIVE CONTROL for
// the test above, and it exists so that the relocation of the atomic-visibility
// guarantee (rmp #2320) is pinned from both sides rather than asserted in a
// comment.
//
// It runs the identical workload with the identical invariant, reading through
// [lpg.Graph.View] plus UNVERSIONED accessors, and requires that this DOES observe
// partial transactions. GoGraph updates the stored value in place and keeps the
// inverse in the version chain, so an accessor that resolves no version reads the
// newest value — another transaction's uncommitted work included — and a shared
// View no longer excludes the writer that is making it.
//
// If this test ever goes green, one of two things has happened: ordinary writes
// have gone back to excluding readers (in which case the write-scaling gate
// should have failed first), or the unversioned accessors have gained an implicit
// snapshot. Either is a change of architecture and must be a deliberate decision,
// not a silent drift — which is exactly what a negative control is for.
//
// Being a race, it is inherently probabilistic: the workload is sized so that the
// interleaving is overwhelmingly likely (300 commits against 8 spinning readers
// measured 7040 and 14 500 violations on two runs), and a run that happens to
// observe none is reported as inconclusive rather than as a failure, so this can
// never be the flake that reddens the gate.
func TestIsolation_ViewWithUnversionedReadIsNotAtomic(t *testing.T) {
	t.Parallel()

	g, store, cleanup := newIsolationStore(t)
	defer cleanup()

	violation, reads := runIsolationWorkload(t, g, store, func(g *lpg.Graph[string, int64]) (ia, ib int64, ok bool) {
		va, oka := g.GetNodeProperty("a", "v")
		vb, okb := g.GetNodeProperty("b", "v")
		if !oka || !okb {
			return
		}
		ia, _ = va.Int64()
		ib, _ = vb.Int64()
		ok = true
		return ia, ib, ok
	})

	if reads == 0 {
		t.Fatal("readers never observed both properties; test did not exercise the invariant")
	}
	if violation == 0 {
		t.Skipf("inconclusive: %d View reads observed no partial transaction this run. "+
			"The guarantee is NOT claimed for View plus unversioned accessors (rmp #2320); "+
			"a run that misses the interleaving proves nothing either way", reads)
	}
	t.Logf("as designed: %d of %d View reads observed a partial transaction; "+
		"atomic visibility comes from a snapshot, not from this lock", violation, reads)
}

// newIsolationStore builds the graph and WAL-backed store both isolation tests
// drive, seeded with the two nodes whose properties carry the invariant.
func newIsolationStore(t *testing.T) (*lpg.Graph[string, int64], *Store[string, int64], func()) {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	store := NewStoreWithOptions[string, int64](g, w, Options[string, int64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewInt64WeightCodec(),
	})

	tx := store.Begin()
	mustNoErr(t, tx.SetNodeProperty("a", "v", lpg.Int64Value(0)))
	mustNoErr(t, tx.SetNodeProperty("b", "v", lpg.Int64Value(0)))
	mustNoErr(t, tx.Commit())

	return g, store, func() { _ = w.Close() }
}

// runIsolationWorkload commits `commits` two-property transactions while
// `readers` goroutines poll the pair through read, and reports how many reads
// observed the two properties disagreeing and how many observed them at all.
//
// read returns the two values it observed and whether both were present, so the
// two tests differ ONLY in the read mechanism — which is what makes the negative
// control a control rather than a second, differently-shaped test.
func runIsolationWorkload(
	t *testing.T,
	g *lpg.Graph[string, int64],
	store *Store[string, int64],
	read func(*lpg.Graph[string, int64]) (ia, ib int64, ok bool),
) (violations, reads int64) {
	t.Helper()

	const (
		commits    = 300
		readerGoro = 8
	)
	var (
		wg          sync.WaitGroup
		done        atomic.Bool
		violation   atomic.Int64
		readCount   atomic.Int64
		writerFatal atomic.Int64
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for i := int64(1); i <= commits; i++ {
			tx := store.Begin()
			if err := tx.SetNodeProperty("a", "v", lpg.Int64Value(i)); err != nil {
				writerFatal.Add(1)
				return
			}
			if err := tx.SetNodeProperty("b", "v", lpg.Int64Value(i)); err != nil {
				writerFatal.Add(1)
				return
			}
			if err := tx.Commit(); err != nil {
				writerFatal.Add(1)
				return
			}
		}
	}()

	for r := 0; r < readerGoro; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				ia, ib, ok := read(g)
				if !ok {
					continue
				}
				readCount.Add(1)
				if ia != ib {
					violation.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	// A writer failure is always a test failure, in either direction: the
	// workload is a single writer, so nothing here can conflict with it.
	if n := writerFatal.Load(); n != 0 {
		t.Fatalf("the single writer failed %d time(s); the workload did not run", n)
	}
	return violation.Load(), readCount.Load()
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tx op: %v", err)
	}
}

// TestIsolation_NoReaderObservesANonSerialisableState is rmp #2306's AC3: under
// CONCURRENT commits, no reader may observe a state that no serial schedule of those
// commits could produce.
//
// # The invariant, and the one that does NOT fit this layer
//
// A conserved SUM is the natural expression of "no serial schedule produces this", and
// it was tried first — and it does not belong here. Conservation needs each transfer to
// read the balances it adjusts, and [Tx] is an op buffer with no read-your-own-snapshot
// surface: a writer must read through the graph, outside its transaction, so two
// concurrent writers read the same balance and both write. The sum then changes for a
// reason that has nothing to do with what a reader can observe — a LOST UPDATE in the
// workload, not an isolation failure. Measured on the first attempt: 772 035 of
// 1 073 756 observations "torn", which is the workload being wrong rather than the
// engine. Conservation is a read-modify-write invariant and therefore a CYPHER-level
// one, where `SET a.bal = a.bal - $amt` is atomic within the statement and conflicts are
// retried — which is exactly what examples/27_concurrent_txn asserts.
//
// What DOES fit this layer is an invariant carried entirely by values the writer
// chooses: each transaction stamps EVERY one of its node's properties with the same
// generation. A reader that sees two different generations on one node has seen half a
// transaction, and no ordering of any subset of the commits can produce that.
//
// # How this differs from the single-writer test above
//
// TestIsolation_Commit_NoPartialTransactionObservable has ONE writer and asks whether a
// single transaction can be seen half-applied. Here MANY writers commit at once through
// the durable path, so the question is whether the COMBINATION becomes observable in an
// impossible state — which the shared commit record and the apply gate must jointly rule
// out. Each writer owns its own node so nothing collides, isolating the visibility
// question from the conflict question.
//
// # It is a GUARD, not a discriminator, and that is a property of this layer
//
// Removing the shared commit record (building every WriteView with no transaction)
// leaves this test PASSING, and why is worth recording. TWO mechanisms provide atomic
// visibility here, not one: the commit record, AND the apply gate, which serialises the
// applies so only one write bracket is open at a time — and while that holds, an ambient
// resolution names the right transaction anyway. Removing BOTH is not a valid control
// either: without the gate, durable commits are refused at apply time
// (TestApplyGate_ADurableCommitIsNeverRefusedByConflictDetection).
//
// So the property is over-determined at this layer and cannot be isolated by
// subtraction — the same situation graph/lpg/mvcc_index_ordering_test.go recorded for
// rmp #2303. The DISCRIMINATING version lives one layer down, where brackets genuinely
// overlap: graph/lpg TestWriteView_ConcurrentWritersEachPublishTheirOwnTransaction,
// verified to fail against a build whose writes carry no transaction. What THIS test
// adds is the DURABLE path — Tx.Commit, the WAL, the apply gate — which that one does
// not exercise.
func TestIsolation_NoReaderObservesANonSerialisableState(t *testing.T) {
	t.Parallel()

	g, store, cleanup := newIsolationStore(t)
	defer cleanup()

	// Sized so a reader actually lands mid-transaction. The first attempt used one
	// reader and 120 commits and did NOT discriminate: with a WAL fsync per commit the
	// reader spent almost all its time between transactions. More readers is what
	// closes that, not more writers — the writers are rate-limited by fsync either way.
	const (
		writers = 6
		props   = 4
		rounds  = 60
		readers = 6
	)
	seed := store.Begin()
	for w := 0; w < writers; w++ {
		for p := 0; p < props; p++ {
			mustNoErr(t, seed.SetNodeProperty(acct(w), gen(p), lpg.Int64Value(0)))
		}
	}
	mustNoErr(t, seed.Commit())

	var (
		writersWG sync.WaitGroup
		readerWG  sync.WaitGroup
		stop      = make(chan struct{})
		torn      atomic.Int64
		observed  atomic.Int64
		writeErrs atomic.Int64
	)

	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go readerLoop(g, stop, &readerWG, writers, props, &torn, &observed)
	}

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for r := 1; r <= rounds; r++ {
				tx := store.Begin()
				for p := 0; p < props; p++ {
					if e := tx.SetNodeProperty(acct(w), gen(p), lpg.Int64Value(int64(r))); e != nil {
						writeErrs.Add(1)
						return
					}
				}
				if e := tx.Commit(); e != nil {
					writeErrs.Add(1)
					return
				}
			}
		}(w)
	}

	writersWG.Wait()
	close(stop)
	readerWG.Wait()

	if n := writeErrs.Load(); n != 0 {
		t.Fatalf("%d write(s) failed; each writer owns its own node, so nothing here can "+
			"legitimately conflict and the workload did not run as intended", n)
	}
	if observed.Load() == 0 {
		t.Fatal("the reader made no observation; the invariant was never exercised")
	}
	if n := torn.Load(); n != 0 {
		t.Fatalf("%d of %d snapshot observation(s) saw two different generations on ONE "+
			"node, i.e. half a transaction — a state no serial schedule of these commits can "+
			"produce.", n, observed.Load())
	}
}

// acct names the w-th writer's node; gen names the p-th generation-stamped property.
func acct(w int) string { return "acct" + strconv.Itoa(w) }
func gen(p int) string  { return "g" + strconv.Itoa(p) }

// readerLoop is the reader every AC3 arm runs: it snapshots and checks that all of a
// node's generation-stamped properties agree, which is what "half a transaction" would
// break. Extracted so several readers share one body — the discrimination depends on
// having enough of them, not on what each does.
func readerLoop(
	g *lpg.Graph[string, int64],
	stop <-chan struct{},
	wg *sync.WaitGroup,
	writers, props int,
	torn, observed *atomic.Int64,
) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		default:
		}
		snap := g.BeginRead()
		v := g.ReadAt(snap)
		for w := 0; w < writers; w++ {
			first, ok := v.GetNodeProperty(acct(w), gen(0))
			if !ok {
				continue
			}
			want, _ := first.Int64()
			for p := 1; p < props; p++ {
				got, ok := v.GetNodeProperty(acct(w), gen(p))
				if !ok {
					torn.Add(1)
					continue
				}
				observed.Add(1)
				if n, _ := got.Int64(); n != want {
					torn.Add(1)
				}
			}
		}
		g.EndRead(snap)
	}
}

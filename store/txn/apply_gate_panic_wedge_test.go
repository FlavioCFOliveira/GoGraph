package txn_test

// apply_gate_panic_wedge_test.go — regression gate for rmp #2727.
//
// THE DEFECT. [txn.Tx.appendOnly] MINTS the store's dense transaction sequence
// (`seq = t.store.txnSeq.Add(1)`) and the caller — [txn.Tx.Commit] or
// [txn.Tx.CommitWALOnly] — only AFTERWARDS takes and advances the
// sequence-ordered apply gate. Neither the mint nor the advance was deferred, so
// a panic anywhere between them left the dense chain with a PERMANENT hole:
// appliedSeq never reaches seq, and every committer holding a higher sequence
// parks in [txn.Tx.waitApplyTurn] for ever.
//
// The consequence is store-wide and unrecoverable, not transaction-local. The
// parked committer holds its writer registration, so [txn.Store.drainInflight]
// — an unconditional, UNCANCELLABLE wait — never completes either: the next
// [txn.Store.RunUnderCommitLock] wedges, which is the seam the checkpointer and
// store.DB.Close both take. WAL truncation stops and shutdown hangs for ever
// while the WAL grows unbounded. ACID Durability and liveness.
//
// WHY Rollback CANNOT REPAIR IT, and why this gate is independent of rmp #2707.
// The containment boundary rmp #2707 added (cypher's recoverFinishPanic calling
// walTx.Rollback) clears the WRITER REGISTRATION of the panicking transaction,
// and nothing more: [txn.Tx.Rollback] never touches the apply gate. This test
// therefore performs that rollback EXPLICITLY, exactly as the boundary does, and
// still requires the next commit to complete — so it measures the apply gate
// alone and passes or fails independently of #2707's fix.
//
// The injection point is deliberately POST-MINT: the latency sample
// [wal.Writer.AppendRun] emits from its own deferred Stopwatch, which fires
// after the sequence has been minted and after AppendRun released the writer
// mutex, but before appendOnly has marked the transaction finished and before
// the caller has taken its apply-gate turn. rmp #2707's sibling gate injects
// PRE-mint for the same reason in reverse.
//
// The panic is raised by a metrics.Backend — a public extension point the
// embedder supplies — on a genuine production latency sample, not by stubbing an
// internal. A third-party backend that panics is an ordinary embedder fault; the
// module's contract is that such a panic must not convert into a permanent
// store-wide wedge.
//
// THE WATCHDOG IS NOT OPTIONAL. The wait under test is an uncancellable channel
// receive, so a regression would hang the whole package rather than fail it. Both
// assertions below run their work in a goroutine and fail on a timeout.

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// appendRunLatency is the latency sample [wal.Writer.AppendRun] records from its
// own deferred Stopwatch. Panicking there lands the panic in the exact window rmp
// #2727 describes — see the file header for why each of these holds:
//
//   - the transaction's WAL sequence HAS been minted (appendOnly mints it before
//     calling AppendRun), so the apply gate now owes it an advance;
//   - AppendRun's `defer w.mu.Unlock()` was registered AFTER the Stopwatch, so it
//     runs FIRST on unwind and the WAL writer mutex is already released; and
//   - appendOnly has not yet called markFinished, so a subsequent Rollback is a
//     real rollback (it clears the writer registration) rather than an
//     ErrTxFinished no-op — which is precisely what makes the surviving wedge
//     attributable to the apply gate and to nothing else.
const appendRunLatency = "store.wal.AppendRun"

// gateWatchdog bounds every wait in this file. It matches rmp #2707's sibling
// gate so a wedge is reported as a failure at a predictable cost.
const gateWatchdog = 10 * time.Second

// panicOnLatencyBackend raises exactly one panic, the first time it observes a
// latency sample under the named operation, and is a no-op otherwise.
type panicOnLatencyBackend struct {
	target string
	fired  atomic.Bool
}

func (b *panicOnLatencyBackend) IncCounter(string, uint64) {}

func (b *panicOnLatencyBackend) ObserveLatency(name string, _ time.Duration) {
	if name == b.target && b.fired.CompareAndSwap(false, true) {
		panic("injected metrics-backend panic for rmp #2727")
	}
}

func (b *panicOnLatencyBackend) SetGauge(string, float64) {}

func (b *panicOnLatencyBackend) didFire() bool { return b.fired.Load() }

// TestApplyGate_PanicAfterMint_DoesNotWedgeLaterCommitters is the gate for
// [txn.Tx.CommitWALOnly] — the path the Cypher engine's commitUnderBarrier takes.
//
// Gate invariant:
//   - On the UNFIXED code the store is wedged: the doomed transaction consumed a
//     sequence and never advanced the gate, so the NEXT commit parks for ever in
//     waitApplyTurn and the test fails on its own watchdog (goleak additionally
//     reports the parked goroutine).
//   - On the FIXED code the gate is completed from a defer — the turn is still
//     TAKEN before it is advanced, so nothing is published out of order — the
//     chain stays dense, and the subsequent commit completes.
//
// It installs the process-wide metrics backend, so it must NOT run in parallel.
func TestApplyGate_PanicAfterMint_DoesNotWedgeLaterCommitters(t *testing.T) {
	s, _ := newWedgeStore(t)

	// One ordinary commit first, so the gate is warm: appliedSeq is non-zero and
	// the doomed transaction below is not the store's first sequence.
	commitOne(t, s, "warm-src", "warm-dst", commitWALOnly)

	// The doomed transaction. It mints a sequence, appends its frames, and then
	// panics out of AppendRun's latency sample.
	tx := s.Begin()
	if err := tx.AddEdge("doomed-src", "doomed-dst", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	fired, recovered := commitWALOnlyUnderPanickingBackend(tx)

	// (a) The injection must have landed where the test claims. Without this the
	// gate could pass vacuously on a build where the sample moved or was renamed.
	if !fired {
		t.Fatalf("the injected backend never saw the %q latency sample: the commit did not "+
			"reach wal.Writer.AppendRun, so this test proves nothing", appendRunLatency)
	}
	// (b) And it must have reached this frame as a panic, not been swallowed.
	if recovered == nil {
		t.Fatal("CommitWALOnly returned normally; the injected panic did not unwind through it, " +
			"so this test proves nothing")
	}

	// EXACTLY what the containment boundary does (cypher's recoverFinishPanic,
	// rmp #2707) — and it is not enough. Rollback clears the writer registration
	// and returns; it never touches the apply gate. Running it here removes the
	// writer-registration variable from the experiment, so any wedge that
	// survives is the apply gate's alone.
	_ = tx.Rollback()

	// THE GATE. A subsequent commit on the same store must complete.
	assertCommitCompletes(t, s, "after-wedge-src", "after-wedge-dst",
		"a panic between the sequence mint and the apply-gate advance left a permanent hole "+
			"in the dense sequence chain (rmp #2727, ACID Durability/liveness)")

	// And the quiesce seam the checkpointer and store.DB.Close take must complete
	// too: a committer parked in waitApplyTurn still holds its writer
	// registration, so a gate hole wedges drainInflight as well.
	assertQuiesceCompletes(t, s)
}

// TestApplyGate_PanicAfterMint_CommitPath_DoesNotWedgeLaterCommitters is the same
// gate for [txn.Tx.Commit], the sibling entry point. Commit already deferred its
// ADVANCE, but the advance is only reachable once the turn has been taken — so a
// panic raised before waitApplyTurn (which the post-mint injection here is) left
// the identical hole. Both paths mint from the same counter, so a hole opened by
// either wedges both.
func TestApplyGate_PanicAfterMint_CommitPath_DoesNotWedgeLaterCommitters(t *testing.T) {
	s, _ := newWedgeStore(t)

	commitOne(t, s, "warm2-src", "warm2-dst", commitFull)

	tx := s.Begin()
	if err := tx.AddEdge("doomed2-src", "doomed2-dst", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	fired, recovered := commitUnderPanickingBackend(tx, commitFull)
	if !fired {
		t.Fatalf("the injected backend never saw the %q latency sample", appendRunLatency)
	}
	if recovered == nil {
		t.Fatal("Commit returned normally; the injected panic did not unwind through it")
	}
	_ = tx.Rollback()

	assertCommitCompletes(t, s, "after-wedge2-src", "after-wedge2-dst",
		"a panic between the sequence mint and the apply-gate turn wedged Tx.Commit's chain "+
			"(rmp #2727)")
	assertQuiesceCompletes(t, s)
}

// commitKind selects which durable commit entry point a helper drives.
type commitKind int

const (
	commitFull commitKind = iota // Tx.Commit — WAL + in-memory apply
	commitWALOnly
)

func (k commitKind) run(tx *txn.Tx[string, int64]) error {
	if k == commitWALOnly {
		return tx.CommitWALOnly(0)
	}
	return tx.Commit()
}

func (k commitKind) String() string {
	if k == commitWALOnly {
		return "Tx.CommitWALOnly"
	}
	return "Tx.Commit"
}

// newWedgeStore builds a WAL-backed store on a temporary directory.
func newWedgeStore(t *testing.T) (*txn.Store[string, int64], *wal.Writer) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	return txn.NewStoreWithCodec(g, w, txn.NewStringCodec()), w
}

// commitOne runs one ordinary edge commit and requires it to succeed.
func commitOne(t *testing.T, s *txn.Store[string, int64], src, dst string, kind commitKind) {
	t.Helper()
	tx := s.Begin()
	if err := tx.AddEdge(src, dst, 0); err != nil {
		t.Fatalf("AddEdge(%q,%q): %v", src, dst, err)
	}
	if err := kind.run(tx); err != nil {
		t.Fatalf("commit(%q,%q): %v", src, dst, err)
	}
}

// commitWALOnlyUnderPanickingBackend is the CommitWALOnly spelling of
// [commitUnderPanickingBackend].
func commitWALOnlyUnderPanickingBackend(tx *txn.Tx[string, int64]) (fired bool, recovered any) {
	return commitUnderPanickingBackend(tx, commitWALOnly)
}

// commitUnderPanickingBackend drives one commit under a backend that panics on
// the AppendRun latency sample, and reports whether the injection actually fired
// and what the commit panicked with.
func commitUnderPanickingBackend(tx *txn.Tx[string, int64], kind commitKind) (fired bool, recovered any) {
	backend := &panicOnLatencyBackend{target: appendRunLatency}
	recovered = commitUnderBackend(tx, kind, backend)
	return backend.didFire(), recovered
}

// assertCommitCompletes runs one ordinary commit under a watchdog. The wait it
// guards — [txn.Tx.waitApplyTurn]'s channel receive — is uncancellable, so a
// wedge must be reported as a failure rather than allowed to hang the package.
func assertCommitCompletes(t *testing.T, s *txn.Store[string, int64], src, dst, why string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		tx := s.Begin()
		if err := tx.AddEdge(src, dst, 0); err != nil {
			done <- err
			return
		}
		done <- tx.CommitWALOnly(0)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("subsequent commit failed: %v", err)
		}
	case <-time.After(gateWatchdog):
		// Deliberately not joining the goroutine: it is parked in an
		// uncancellable channel receive and will never finish. Failing is the
		// point; goleak additionally reports the parked goroutine.
		t.Fatalf("the subsequent commit did not complete within %s: %s", gateWatchdog, why)
	}
}

// recordingBackend captures, in order, the name of every metrics event the
// backend observes. It is used to ENUMERATE the commit path's panic-injection
// surface rather than to assume it.
type recordingBackend struct{ events []string }

func (b *recordingBackend) IncCounter(name string, _ uint64) {
	b.events = append(b.events, "counter:"+name)
}

func (b *recordingBackend) ObserveLatency(name string, _ time.Duration) {
	b.events = append(b.events, "latency:"+name)
}

func (b *recordingBackend) SetGauge(name string, _ float64) {
	b.events = append(b.events, "gauge:"+name)
}

// panicAtNthBackend raises exactly one panic, at the n-th (0-based) event it
// observes, and records which event that was.
type panicAtNthBackend struct {
	n     int
	seen  int
	fired bool
	where string
}

func (b *panicAtNthBackend) hit(kind, name string) {
	if b.fired {
		return
	}
	if b.seen == b.n {
		b.fired, b.where = true, kind+":"+name
		panic("injected metrics-backend panic for rmp #2727 at " + b.where)
	}
	b.seen++
}

func (b *panicAtNthBackend) IncCounter(name string, _ uint64) { b.hit("counter", name) }

func (b *panicAtNthBackend) ObserveLatency(name string, _ time.Duration) { b.hit("latency", name) }

func (b *panicAtNthBackend) SetGauge(name string, _ float64) { b.hit("gauge", name) }

// TestApplyGate_NoCommitPathPanicSiteWedgesTheStore widens the two gates above
// from ONE injection point to EVERY one the durable commit path exposes.
//
// The panic-injection surface is ENUMERATED rather than assumed: a recording
// backend first observes one clean commit and reports the ordered list of
// metrics events it emits, and the sweep then panics at each of them in turn.
// Measured on the fixed tree, the surface is
//
//	Tx.CommitWALOnly  6 events: wal.Encode, wal.Encode, wal.AppendRun,
//	                            wal.SyncGroup.leader, wal.SyncGroup,
//	                            txn.CommitWALOnly
//	Tx.Commit         7 events: the same five, then lpg.ApplyVersioned,
//	                            then txn.Commit
//
// Everything from wal.Encode onwards is POST-MINT, so the sweep covers the whole
// of rmp #2727's window and not merely its far end. That was verified by
// mutation: with [Tx.closeApplyGate] neutered, every one of those points wedges
// the store — so a pass here is a real result and not a vacuous one.
//
// The sweep also settles, by measurement, the one window rmp #2707 judged by
// reading: a panic landing strictly between appendOnly's markFinished() and the
// commit's writer release. The `finished` flag is observable from outside —
// Rollback returns ErrTxFinished once it is set — and the sweep reads it at every
// injection point. It flips at wal.SyncGroup.leader, which appendOnly does not
// emit: it is emitted by the caller, after appendOnly has returned. So no metrics
// site — the module's entire embedder-reachable injection surface — lands inside
// that window. The only statement that executes there is appendOnly's own
// `defer putEncodeScratch(scratch)`, which resets a slice header and calls
// sync.Pool.Put, neither of which can panic. The window is unreachable; it is
// nonetheless CLOSED structurally, because [Tx.finishCommit] is deferred before
// the mint and therefore covers every instant inside appendOnly.
//
// It installs the process-wide metrics backend, so it must NOT run in parallel.
func TestApplyGate_NoCommitPathPanicSiteWedgesTheStore(t *testing.T) {
	for _, kind := range []commitKind{commitWALOnly, commitFull} {
		surface := enumerateCommitEvents(t, kind)
		if len(surface) == 0 {
			t.Fatalf("%v: the commit emitted no metrics event, so this sweep would prove nothing", kind)
		}
		t.Logf("%v exposes %d panic-injection sites: %v", kind, len(surface), surface)

		for n := range surface {
			s, _ := newWedgeStore(t)
			commitOne(t, s, "sweep-warm-src", "sweep-warm-dst", commitWALOnly)

			tx := s.Begin()
			if err := tx.AddEdge("sweep-src", "sweep-dst", 0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			backend := &panicAtNthBackend{n: n}
			recovered := commitUnderBackend(tx, kind, backend)
			if !backend.fired {
				t.Fatalf("%v site %d (%s): the injection did not fire", kind, n, surface[n])
			}
			if recovered == nil {
				t.Fatalf("%v site %d (%s): the injected panic did not unwind out of the commit",
					kind, n, surface[n])
			}
			// The containment boundary's one repair (rmp #2707). Whether it is a
			// real rollback or an ErrTxFinished no-op depends on where the panic
			// landed relative to appendOnly's markFinished — which is exactly the
			// discriminator the file header uses.
			rbErr := tx.Rollback()
			t.Logf("%v site %2d %-34s finishedAtPanic=%v", kind, n, surface[n],
				errors.Is(rbErr, txn.ErrTxFinished))

			assertCommitCompletes(t, s, "sweep-after-src", "sweep-after-dst",
				"a panic at commit-path metrics site "+surface[n]+" wedged the store (rmp #2727)")
			assertQuiesceCompletes(t, s)
		}
	}
}

// enumerateCommitEvents runs one clean commit under a recording backend and
// returns the ordered names of the metrics events it emitted.
func enumerateCommitEvents(t *testing.T, kind commitKind) []string {
	t.Helper()
	s, _ := newWedgeStore(t)
	tx := s.Begin()
	if err := tx.AddEdge("enum-src", "enum-dst", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	rec := &recordingBackend{}
	metrics.SetBackend(rec)
	err := kind.run(tx)
	metrics.SetBackend(nil)
	if err != nil {
		t.Fatalf("enumeration commit (%v): %v", kind, err)
	}
	return rec.events
}

// commitUnderBackend installs backend for the duration of one commit, recovers
// whatever the commit panics with — standing in for the embedder's own
// containment boundary — and restores the default backend immediately.
func commitUnderBackend(tx *txn.Tx[string, int64], kind commitKind, backend metrics.Backend) (recovered any) {
	metrics.SetBackend(backend)
	defer metrics.SetBackend(nil)
	defer func() { recovered = recover() }()
	_ = kind.run(tx)
	return nil
}

// assertQuiesceCompletes runs a no-op RunUnderCommitLock under a watchdog — the
// direct observation of [txn.Store]'s in-flight writer count reaching zero.
func assertQuiesceCompletes(t *testing.T, s *txn.Store[string, int64]) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.RunUnderCommitLock(func() error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunUnderCommitLock: %v", err)
		}
	case <-time.After(gateWatchdog):
		t.Fatalf("RunUnderCommitLock did not complete within %s: an admitted writer is still "+
			"registered, so drainInflight waits for ever and the checkpointer and store.DB.Close "+
			"are wedged permanently", gateWatchdog)
	}
}

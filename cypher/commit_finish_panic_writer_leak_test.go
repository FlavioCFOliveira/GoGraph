package cypher_test

// commit_finish_panic_writer_leak_test.go — regression gate for rmp #2707's
// second defect: ExplicitTx.recoverFinishPanic did not roll the WAL transaction
// back, so a panic inside ExplicitTx.Commit's in-barrier finalisation leaked the
// store's writer registration PERMANENTLY.
//
// Why the leak is permanent, and why it is store-wide rather than
// transaction-local:
//
//   - txn.Store registers an admitted writer in Store.BeginCtx (inflight++) and
//     deregisters it in Store.exitWriter, which runs from exactly two places:
//     txn.Tx.Commit/CommitWALOnly (via defer) and txn.Tx.Rollback.
//   - cypher's ExplicitTx.release() does NOT deregister it — its own godoc says
//     so verbatim: "on a WAL-backed engine the store's writer registration is
//     cleared by walTx's own Commit/Rollback".
//   - On the panic path neither ran, so inflight stayed at one for ever.
//   - txn.Store.drainInflight is `for s.inflight != 0 { s.inflightCond.Wait() }`
//     — unconditional and UNCANCELLABLE — so the next
//     txn.Store.RunUnderCommitLock never returns. That is the seam the
//     checkpointer and store.DB.Close both take: WAL truncation stops and
//     shutdown hangs for ever while the WAL grows unbounded.
//
// The trigger is a panic in the PRE-FSYNC window, which is a latent engine bug
// rather than something an embedder raises on demand. That is precisely why the
// boundary exists, and a containment boundary that converts a contained panic
// into a permanent store-wide wedge is itself the defect. The test therefore
// injects the panic at a real, public extension point — a metrics.Backend, which
// the embedder supplies and the finalisation calls on a genuine production
// branch — rather than by stubbing engine internals.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// notNullViolationCounter is the counter ExplicitTx.Commit increments, INSIDE the
// visibility barrier and BEFORE the WAL fsync, when the commit-time NOT NULL
// check rejects the transaction. Panicking there lands the panic in the exact
// window rmp #2707 describes:
//
//   - the visibility barrier is held (ApplyInVersionedTx), so the panic must
//     unwind through it;
//   - txn.Tx.CommitWALOnly has NOT been entered, so no WAL sequence has been
//     minted, no frame appended and no fsync taken — the transaction is entirely
//     unfinished as far as the store is concerned; and
//   - rollbackInBarrierLocked — the ONLY other thing on this path that would have
//     rolled the WAL transaction back — has not run yet.
//
// Keeping the injection before the sequence mint is deliberate: it isolates the
// writer-registration leak this gate is about from the independent apply-gate
// hole a panic AFTER the mint opens (see the report for rmp #2707).
const notNullViolationCounter = "cypher.ExplicitTx.constraint.notNullViolations"

// panicOnceBackend is a metrics.Backend that raises exactly one panic, the first
// time it observes the named counter, and behaves as a no-op otherwise. A
// third-party backend that panics is an ordinary embedder fault; the module's own
// contract is that a panic must be contained, not that it cannot happen.
type panicOnceBackend struct {
	target string
	fired  atomic.Bool
}

func (b *panicOnceBackend) IncCounter(name string, _ uint64) {
	if name == b.target && b.fired.CompareAndSwap(false, true) {
		panic("injected metrics-backend panic for rmp #2707")
	}
}

func (b *panicOnceBackend) ObserveLatency(string, time.Duration) {}

func (b *panicOnceBackend) SetGauge(string, float64) {}

func (b *panicOnceBackend) didFire() bool { return b.fired.Load() }

// TestCommit_FinalisationPanic_DoesNotWedgeTheStore is the gate.
//
// Gate invariant:
//   - On the UNFIXED code the store is wedged: RunUnderCommitLock parks for ever
//     inside drainInflight and the test fails on its own watchdog (goleak
//     additionally reports the parked goroutine). It reports a FAILURE rather
//     than hanging the package, which is why the watchdog is not optional — the
//     wait it is waiting on is uncancellable by construction.
//   - On the FIXED code recoverFinishPanic rolls the WAL transaction back,
//     exitWriter runs exactly once, inflight returns to zero, and both
//     RunUnderCommitLock and a subsequent ordinary write complete.
//
// It installs the process-wide metrics backend, so it must NOT run in parallel.
func TestCommit_FinalisationPanic_DoesNotWedgeTheStore(t *testing.T) {
	quietLogs(t)
	eng, store := newBoomWALEngine(t)
	ctx := context.Background()

	// A NOT NULL constraint, declared BEFORE the transaction opens so the
	// transaction tracks touched nodes and runs the commit-time check.
	mustRunDDL(t, ctx, eng, `CREATE CONSTRAINT wedge_p_nn FOR (n:Wedge) REQUIRE n.p IS NOT NULL`)

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// One ordinary write that VIOLATES the constraint, so the finalisation takes
	// its rejection branch. The store now holds this transaction's writer
	// registration (inflight == 1); nothing has been appended to the WAL.
	res, execErr := tx.Exec("CREATE (:Wedge)", nil)
	if execErr != nil {
		t.Fatalf("Exec CREATE: %v", execErr)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if drainErr := res.Err(); drainErr != nil {
		t.Fatalf("result drain: %v", drainErr)
	}
	_ = res.Close()

	fired, commitErr := commitUnderPanickingBackend(tx)

	// (a) The injection must have landed where the test claims. Without this the
	// gate could pass vacuously on a build where the branch moved.
	if !fired {
		t.Fatalf("the injected backend never saw %s: the commit did not take the "+
			"NOT NULL rejection branch, so this test proves nothing", notNullViolationCounter)
	}

	// (b) The panic must have been CONTAINED and converted, not propagated.
	if commitErr == nil {
		t.Fatal("Commit returned nil; expected an error wrapping cypher.ErrInternalPanic")
	}
	if !errors.Is(commitErr, cypher.ErrInternalPanic) {
		t.Fatalf("Commit error %v does not wrap cypher.ErrInternalPanic", commitErr)
	}

	// (c) THE GATE. The store's in-flight writer count must be back at zero, so a
	// quiesce completes. RunUnderCommitLock closes the admission gate and then
	// drains inflight to zero; that drain is uncancellable, so it runs under a
	// watchdog and a leak is reported as a test failure rather than as a hang.
	assertQuiesceCompletes(t, store, "after a panic in ExplicitTx.Commit's finalisation")

	// (d) And the store must still be usable: a subsequent ordinary write goes
	// through. This also proves (c) did not merely observe a transient zero — the
	// write re-enters and re-exits the admission gate, and takes the WAL commit
	// path end to end.
	assertWriteCompletes(t, eng)
}

// commitUnderPanickingBackend installs the panicking backend for the duration of
// tx.Commit and restores the default immediately afterwards, so no other test —
// and no later step of this one — can observe the injected fault. It reports
// whether the injection actually fired.
func commitUnderPanickingBackend(tx *cypher.ExplicitTx) (fired bool, err error) {
	backend := &panicOnceBackend{target: notNullViolationCounter}
	cmetrics.SetBackend(backend)
	defer cmetrics.SetBackend(nil)
	err = tx.Commit()
	return backend.didFire(), err
}

// mustRunDDL executes one autocommit DDL statement and drains its result.
func mustRunDDL(t *testing.T, ctx context.Context, eng *cypher.Engine, query string) { //nolint:revive // t is first by testing convention; ctx follows
	t.Helper()
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", query, err)
	}
	for res.Next() {
		_ = res.Record()
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Run %q drain: %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Run %q close: %v", query, err)
	}
}

// assertQuiesceCompletes runs a no-op RunUnderCommitLock under a watchdog. It is
// the direct observation of txn.Store.inflight reaching zero: the quiesce closes
// the admission gate and then blocks in drainInflight until the count is zero, so
// it returns if and only if every admitted writer has been deregistered.
func assertQuiesceCompletes(t *testing.T, store *txn.Store[string, float64], when string) {
	t.Helper()
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- store.RunUnderCommitLock(func() error { return nil })
	}()

	select {
	case err := <-done:
		wg.Wait()
		if err != nil {
			t.Fatalf("RunUnderCommitLock %s: %v", when, err)
		}
	case <-time.After(10 * time.Second):
		// Deliberately NOT waiting on wg here: the goroutine is parked in an
		// uncancellable sync.Cond wait and will never finish. Failing is the
		// point; goleak additionally reports the parked goroutine.
		t.Fatalf("RunUnderCommitLock did not complete within 10s %s: the store's writer registration leaked, "+
			"so drainInflight waits for ever and the checkpointer and DB.Close are wedged permanently "+
			"(rmp #2707, ACID Durability/liveness)", when)
	}
}

// assertWriteCompletes runs one ordinary autocommit write under a watchdog.
func assertWriteCompletes(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		res, err := eng.RunInTx(context.Background(), "CREATE (:AfterWedge) RETURN 1", nil)
		if err != nil {
			done <- err
			return
		}
		for res.Next() {
			_ = res.Record()
		}
		if rerr := res.Err(); rerr != nil {
			done <- rerr
			return
		}
		done <- res.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("subsequent write failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subsequent write did not complete within 10s: the store is still wedged")
	}
}

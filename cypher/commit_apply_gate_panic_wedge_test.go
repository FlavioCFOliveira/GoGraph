package cypher_test

// commit_apply_gate_panic_wedge_test.go — full-stack gate for rmp #2727, on the
// production path the defect was actually reported from:
//
//	store/txn.(*Tx[...]).waitApplyTurn   [chan receive]
//	store/txn.(*Tx[...]).CommitWALOnly
//	cypher.(*Result).commitUnderBarrier
//
// The mechanism, the injection point and the ordering argument are documented
// once, in store/txn/apply_gate_panic_wedge_test.go, which gates the defect at
// the layer that owns it. This file adds the one thing that file cannot show:
// that the two containment boundaries COMPOSE. A panic in the pre-fsync window
// leaves two obligations outstanding — the store's writer registration (rmp
// #2707, discharged by cypher's recoverFinishPanic calling walTx.Rollback) and
// the apply gate's advance (rmp #2727, which Rollback cannot reach) — and the
// engine must survive both being owed at once.
//
// The injection is POST-MINT, which is what makes this gate independent of
// #2707's: #2707's sibling injects PRE-mint, deliberately, so that the two
// boundaries are proven separately rather than one masking the other.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// appendRunLatencySample is the latency sample [wal.Writer.AppendRun] records
// from its own deferred Stopwatch. It fires AFTER the transaction's WAL sequence
// has been minted and after AppendRun released the writer mutex, but before the
// commit has taken its apply-gate turn — the window rmp #2727 describes.
const appendRunLatencySample = "store.wal.AppendRun"

// panicOnceLatencyBackend raises exactly one panic, the first time it observes a
// latency sample under the named operation, and is a no-op otherwise. A
// third-party backend that panics is an ordinary embedder fault; the module's
// contract is that the panic must be contained, not that it cannot happen.
type panicOnceLatencyBackend struct {
	target string
	fired  atomic.Bool
}

func (b *panicOnceLatencyBackend) IncCounter(string, uint64) {}

func (b *panicOnceLatencyBackend) ObserveLatency(name string, _ time.Duration) {
	if name == b.target && b.fired.CompareAndSwap(false, true) {
		panic("injected metrics-backend panic for rmp #2727")
	}
}

func (b *panicOnceLatencyBackend) SetGauge(string, float64) {}

func (b *panicOnceLatencyBackend) didFire() bool { return b.fired.Load() }

// TestCommit_PanicAfterSequenceMint_DoesNotWedgeTheStore is the gate.
//
// Gate invariant:
//   - On the UNFIXED code the doomed transaction consumed a sequence and never
//     advanced the apply gate, so the NEXT write parks for ever inside
//     txn.Tx.waitApplyTurn and the test fails on its own watchdog.
//   - On the FIXED code the gate is discharged from a defer registered before the
//     mint — the turn is still TAKEN before it is advanced, so nothing publishes
//     out of order — and both the quiesce and the subsequent write complete.
//
// It installs the process-wide metrics backend, so it must NOT run in parallel.
func TestCommit_PanicAfterSequenceMint_DoesNotWedgeTheStore(t *testing.T) {
	quietLogs(t)
	eng, store := newBoomWALEngine(t)
	ctx := context.Background()

	// One ordinary write first, so the apply gate is warm and the doomed
	// transaction below is not the store's first sequence.
	assertWriteCompletes(t, eng)

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, execErr := tx.Exec("CREATE (:Wedge2)", nil)
	if execErr != nil {
		t.Fatalf("Exec CREATE: %v", execErr)
	}
	for res.Next() { // intentional full drain
	}
	if drainErr := res.Err(); drainErr != nil {
		t.Fatalf("result drain: %v", drainErr)
	}
	_ = res.Close()

	fired, commitErr := commitUnderPanickingLatencyBackend(tx)

	// (a) The injection must have landed where the test claims. Without this the
	// gate could pass vacuously on a build where the sample moved or was renamed.
	if !fired {
		t.Fatalf("the injected backend never saw the %q latency sample: the commit did not reach "+
			"wal.Writer.AppendRun, so this test proves nothing", appendRunLatencySample)
	}
	// (b) The panic must have been CONTAINED and converted, not propagated.
	if commitErr == nil {
		t.Fatal("Commit returned nil; expected an error wrapping cypher.ErrInternalPanic")
	}
	if !errors.Is(commitErr, cypher.ErrInternalPanic) {
		t.Fatalf("Commit error %v does not wrap cypher.ErrInternalPanic", commitErr)
	}

	// (c) The rmp #2707 obligation: the writer registration must be back, so a
	// quiesce completes. This is the boundary the panic ALSO crosses, and it must
	// still hold with the apply-gate obligation outstanding at the same instant.
	assertQuiesceCompletes(t, store, "after a panic between the WAL sequence mint and the apply gate")

	// (d) THE rmp #2727 GATE. A subsequent write must complete. Its own sequence
	// is the doomed one's successor, so a hole in the dense chain parks it for
	// ever inside txn.Tx.waitApplyTurn.
	assertWriteCompletes(t, eng)
}

// commitUnderPanickingLatencyBackend installs the panicking backend for the
// duration of tx.Commit and restores the default immediately afterwards, so no
// other test — and no later step of this one — can observe the injected fault. It
// reports whether the injection actually fired.
func commitUnderPanickingLatencyBackend(tx *cypher.ExplicitTx) (fired bool, err error) {
	backend := &panicOnceLatencyBackend{target: appendRunLatencySample}
	cmetrics.SetBackend(backend)
	defer cmetrics.SetBackend(nil)
	err = tx.Commit()
	return backend.didFire(), err
}

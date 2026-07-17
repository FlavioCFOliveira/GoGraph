package proto_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// filledPayload returns a deterministic n-byte payload so a reassembled message
// can be checked for a faithful round-trip.
func filledPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

// TestChunkedReader_ReassemblyChargedAndReleased confirms a within-budget
// message reassembles faithfully AND that its transient reassembly buffer is
// released symmetrically: after the message is assembled the shared pool is
// fully restored, so a subsequent full-pool reservation succeeds. This is the
// headline behaviour of #1891 — reassembly bytes are now accounted against the
// aggregate InboundBudget, charge and release balanced. The payload spans
// several chunks so the per-chunk charging loop is exercised.
func TestChunkedReader_ReassemblyChargedAndReleased(t *testing.T) {
	t.Parallel()
	const limit = 4 << 20               // 4 MiB
	payload := filledPayload(256 << 10) // 256 KiB ≈ 4 chunks, well within budget

	var wire bytes.Buffer
	writeChunkedMessage(&wire, payload)

	budget := packstream.NewInboundBudget(limit)
	cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(wire.Bytes()), proto.DefaultMaxMessageBytes)
	cr.SetInboundBudget(budget)

	msg, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage within budget: %v", err)
	}
	if !bytes.Equal(msg, payload) {
		t.Fatalf("reassembled payload mismatch: got %d bytes, want %d", len(msg), len(payload))
	}
	// Symmetric release: the whole pool must be available again.
	if !budget.TryReserve(limit) {
		t.Fatal("budget not fully restored after a successful reassembly (charge leaked)")
	}
	budget.Release(limit)
}

// TestChunkedReader_ReassemblyRejectedWhenBudgetExhausted confirms that a
// message which charges part-way and then meets an exhausted pool is rejected
// with the typed ErrInboundBudgetExceeded — a transient backpressure signal, not
// ErrMessageTooLarge — and that the partial charge is released symmetrically so
// the pool is left exactly balanced. The message is far below the per-message
// MaxMessageBytes cap, so the rejection is driven purely by the aggregate budget.
func TestChunkedReader_ReassemblyRejectedWhenBudgetExhausted(t *testing.T) {
	t.Parallel()
	const limit = 150000                // fits two 65535-byte chunks but not three
	payload := filledPayload(3 * 65535) // three full chunks: 196605 bytes

	var wire bytes.Buffer
	writeChunkedMessage(&wire, payload)

	budget := packstream.NewInboundBudget(limit)
	cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(wire.Bytes()), proto.DefaultMaxMessageBytes)
	cr.SetInboundBudget(budget)

	msg, err := cr.ReadMessage()
	if !errors.Is(err, packstream.ErrInboundBudgetExceeded) {
		t.Fatalf("ReadMessage over aggregate budget: err = %v, want ErrInboundBudgetExceeded", err)
	}
	if errors.Is(err, proto.ErrMessageTooLarge) {
		t.Fatal("rejection misclassified as ErrMessageTooLarge; the per-message cap was not reached, only the aggregate budget")
	}
	if msg != nil {
		t.Fatalf("rejected reassembly returned a non-nil payload (%d bytes); contract requires nil", len(msg))
	}
	// Symmetric release of the partial charge: the whole pool is available again.
	if !budget.TryReserve(limit) {
		t.Fatal("partial reassembly charge leaked after a budget rejection")
	}
	budget.Release(limit)
}

// TestChunkedReader_ReassemblySharesAggregatePool proves the reassembly buffer
// draws on the SAME engine-wide pool as everything else charging the budget: a
// reservation held elsewhere (standing in for another connection's decode or
// reassembly) shrinks what this reader may take, forcing a rejection; releasing
// it lets the identical message through. This is the single-ceiling guarantee —
// aggregate inbound memory (reassembly + decode) bounded centrally.
func TestChunkedReader_ReassemblySharesAggregatePool(t *testing.T) {
	t.Parallel()
	const limit = 100000
	payload := filledPayload(65535) // one full chunk

	var wire bytes.Buffer
	writeChunkedMessage(&wire, payload)

	budget := packstream.NewInboundBudget(limit)

	// Simulate another party (a decode or a second connection's reassembly)
	// holding 60 KiB of the shared pool, leaving < 65535 for this reader.
	const heldElsewhere = 60000
	if !budget.TryReserve(heldElsewhere) {
		t.Fatalf("setup: TryReserve(%d) failed", heldElsewhere)
	}

	cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(wire.Bytes()), proto.DefaultMaxMessageBytes)
	cr.SetInboundBudget(budget)
	if _, err := cr.ReadMessage(); !errors.Is(err, packstream.ErrInboundBudgetExceeded) {
		t.Fatalf("reassembly with pool held elsewhere: err = %v, want ErrInboundBudgetExceeded", err)
	}

	// Release the foreign hold; the identical message now fits.
	budget.Release(heldElsewhere)
	cr = proto.NewChunkedReaderWithLimit(bytes.NewReader(wire.Bytes()), proto.DefaultMaxMessageBytes)
	cr.SetInboundBudget(budget)
	msg, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("reassembly after releasing the foreign hold: %v", err)
	}
	if !bytes.Equal(msg, payload) {
		t.Fatal("reassembled payload mismatch after pool freed")
	}
	if !budget.TryReserve(limit) {
		t.Fatal("pool not fully restored after the round trip")
	}
	budget.Release(limit)
}

// TestChunkedReader_ReassemblyAggregateBoundConcurrent is the stress guard: many
// connections' readers share one budget and reassemble large messages
// concurrently. Aggregate demand exceeds the ceiling, so the pool is contended;
// every outcome must be either a faithful message or a clean
// ErrInboundBudgetExceeded (never a partial read, panic, or other error), the
// tracked total never dips below zero (guaranteed by the atomic reservation),
// and once every reader has finished the pool is exactly balanced — no charge
// leaked under concurrency. Run under -race, it also proves the accounting is
// race-free; the package's goleak TestMain proves no goroutine leaks.
func TestChunkedReader_ReassemblyAggregateBoundConcurrent(t *testing.T) {
	t.Parallel()
	const (
		limit     = 4 << 20 // 4 MiB shared ceiling
		msgBytes  = 1 << 20 // 1 MiB per message — a few concurrent ones exhaust the pool
		workers   = 16
		perWorker = 40
	)
	payload := filledPayload(msgBytes)
	var wireBuf bytes.Buffer
	writeChunkedMessage(&wireBuf, payload)
	wire := wireBuf.Bytes()

	budget := packstream.NewInboundBudget(limit)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(wire), proto.DefaultMaxMessageBytes)
				cr.SetInboundBudget(budget)
				msg, err := cr.ReadMessage()
				switch {
				case err == nil:
					if !bytes.Equal(msg, payload) {
						t.Errorf("concurrent reassembly corrupted the payload")
						return
					}
				case errors.Is(err, packstream.ErrInboundBudgetExceeded):
					// Expected backpressure under contention.
				default:
					t.Errorf("unexpected reassembly error under contention: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// No charge may survive: every ReadMessage released its reservation on
	// return, so the whole pool is reclaimable.
	if !budget.TryReserve(limit) {
		t.Fatal("shared pool leaked under concurrent reassembly (not fully restored)")
	}
	budget.Release(limit)
}

// TestChunkedReader_DisabledBudget_NoOp confirms a nil or disabled budget imposes
// no aggregate bound: a large message (bounded only by MaxMessageBytes) still
// reassembles, and SetInboundBudget stores a disabled budget as no accounting so
// the reader pays nothing. This guards against a regression that would reject
// legitimate traffic when the operator has not opted into an inbound ceiling.
func TestChunkedReader_DisabledBudget_NoOp(t *testing.T) {
	t.Parallel()
	payload := filledPayload(2 << 20) // 2 MiB — many chunks, no ceiling

	var wire bytes.Buffer
	writeChunkedMessage(&wire, payload)
	wireBytes := wire.Bytes()

	for name, b := range map[string]*packstream.InboundBudget{
		"nil":        nil,
		"zero-limit": packstream.NewInboundBudget(0),
		"neg-limit":  packstream.NewInboundBudget(-1),
	} {
		t.Run(name, func(t *testing.T) {
			cr := proto.NewChunkedReaderWithLimit(bytes.NewReader(wireBytes), proto.DefaultMaxMessageBytes)
			cr.SetInboundBudget(b)
			msg, err := cr.ReadMessage()
			if err != nil {
				t.Fatalf("disabled budget rejected a legitimate message: %v", err)
			}
			if !bytes.Equal(msg, payload) {
				t.Fatal("payload mismatch under disabled budget")
			}
		})
	}
}

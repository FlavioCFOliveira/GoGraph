package packstream

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

// encodeIntList builds a PackStream list of n small integers and returns the
// wire bytes. Decoding it charges the decoder's collection budget by ~n
// elements, enough to exercise the inbound-budget ceiling.
func encodeIntList(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.WriteListHeader(n); err != nil {
		t.Fatalf("WriteListHeader: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := enc.WriteInt(int64(i & 0x7f)); err != nil {
			t.Fatalf("WriteInt: %v", err)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

// TestInboundBudget_RejectsOverBudget confirms the headline fix: a decode whose
// collection charge exceeds the shared ceiling fails fast with
// ErrInboundBudgetExceeded rather than allocating. #1845 (CWE-770).
func TestInboundBudget_RejectsOverBudget(t *testing.T) {
	t.Parallel()
	payload := encodeIntList(t, 200_000) // charges several MiB of collection budget
	d := NewDecoder(bytes.NewReader(payload))
	d.SetInboundBudget(NewInboundBudget(64 * 1024)) // 64 KiB — far below the charge

	if _, err := d.ReadValue(); !errors.Is(err, ErrInboundBudgetExceeded) {
		t.Fatalf("ReadValue over budget: err = %v, want ErrInboundBudgetExceeded", err)
	}
}

// TestInboundBudget_UnlimitedAllows confirms a disabled budget (nil or limit<=0)
// imposes no aggregate bound: the same large list decodes fine.
func TestInboundBudget_UnlimitedAllows(t *testing.T) {
	t.Parallel()
	payload := encodeIntList(t, 200_000)

	for _, b := range []*InboundBudget{nil, NewInboundBudget(0), NewInboundBudget(-1)} {
		d := NewDecoder(bytes.NewReader(payload))
		d.SetInboundBudget(b)
		v, err := d.ReadValue()
		if err != nil {
			t.Fatalf("ReadValue with disabled budget: unexpected error %v", err)
		}
		if lst, ok := v.([]Value); !ok || len(lst) != 200_000 {
			t.Fatalf("decoded value is not a 200000-element list: %T", v)
		}
	}
}

// TestInboundBudget_ReleaseRestores confirms a completed decode returns every
// reserved byte to the shared pool, so the budget is balanced for reuse.
func TestInboundBudget_ReleaseRestores(t *testing.T) {
	t.Parallel()
	budget := NewInboundBudget(128 << 20) // 128 MiB — comfortably above the charge
	before := budget.remaining.Load()

	payload := encodeIntList(t, 100_000)
	d := NewDecoder(bytes.NewReader(payload))
	d.SetInboundBudget(budget)
	if _, err := d.ReadValue(); err != nil {
		t.Fatalf("ReadValue within budget: %v", err)
	}
	if held := budget.remaining.Load(); held >= before {
		t.Fatalf("expected some budget reserved during decode; remaining %d not < %d", held, before)
	}
	d.ReleaseInboundBudget()
	if got := budget.remaining.Load(); got != before {
		t.Fatalf("budget not fully restored after release: got %d, want %d", got, before)
	}
}

// TestInboundBudget_ResetSelfHeals confirms Reset returns held bytes even when
// ReleaseInboundBudget was not called — a pooled decoder cannot strand a shared
// budget when it is recycled.
func TestInboundBudget_ResetSelfHeals(t *testing.T) {
	t.Parallel()
	budget := NewInboundBudget(128 << 20)
	before := budget.remaining.Load()

	d := NewDecoder(bytes.NewReader(encodeIntList(t, 100_000)))
	d.SetInboundBudget(budget)
	if _, err := d.ReadValue(); err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	// Recycle the decoder without an explicit release.
	d.Reset(bytes.NewReader(nil))
	if got := budget.remaining.Load(); got != before {
		t.Fatalf("Reset did not self-heal the budget: got %d, want %d", got, before)
	}
}

// TestInboundBudget_ConcurrentBalanced runs many decoders sharing one budget and
// asserts the pool is fully restored afterwards — the aggregate accounting is
// race-free (run under -race) and leak-free.
func TestInboundBudget_ConcurrentBalanced(t *testing.T) {
	t.Parallel()
	budget := NewInboundBudget(256 << 20)
	before := budget.remaining.Load()
	payload := encodeIntList(t, 50_000)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := NewDecoder(bytes.NewReader(payload))
			d.SetInboundBudget(budget)
			_, _ = d.ReadValue()
			d.ReleaseInboundBudget()
		}()
	}
	wg.Wait()

	if got := budget.remaining.Load(); got != before {
		t.Fatalf("budget leaked under concurrency: got %d, want %d", got, before)
	}
}

// TestInboundBudget_tryReserveRelease unit-tests the pool primitive: reservation
// draws down the pool, over-reservation fails without side effects, release
// restores, and a disabled budget always succeeds without accounting.
func TestInboundBudget_tryReserveRelease(t *testing.T) {
	t.Parallel()
	b := NewInboundBudget(1000)
	if !b.tryReserve(600) {
		t.Fatal("tryReserve(600) of 1000 = false, want true")
	}
	if b.tryReserve(500) {
		t.Fatal("tryReserve(500) with 400 left = true, want false (fail-fast)")
	}
	if b.remaining.Load() != 400 {
		t.Fatalf("failed reserve consumed budget: remaining = %d, want 400", b.remaining.Load())
	}
	b.release(600)
	if b.remaining.Load() != 1000 {
		t.Fatalf("release did not restore: remaining = %d, want 1000", b.remaining.Load())
	}

	disabled := NewInboundBudget(0)
	if !disabled.tryReserve(1 << 40) {
		t.Fatal("disabled budget tryReserve = false, want true (no-op)")
	}
}

// TestInboundBudget_ExportedReserveRelease confirms the exported TryReserve /
// Release / Enabled surface (used by the Bolt message-reassembly reader in the
// proto package to charge the SAME shared pool) mirrors the internal primitives
// exactly and is nil-safe: a nil *InboundBudget must be a safe no-op so an
// unbudgeted reader never panics.
func TestInboundBudget_ExportedReserveRelease(t *testing.T) {
	t.Parallel()

	b := NewInboundBudget(1000)
	if !b.Enabled() {
		t.Fatal("Enabled() = false for a positive-limit budget, want true")
	}
	if !b.TryReserve(700) {
		t.Fatal("TryReserve(700) of 1000 = false, want true")
	}
	if b.TryReserve(400) {
		t.Fatal("TryReserve(400) with 300 left = true, want false (fail-fast)")
	}
	if got := b.remaining.Load(); got != 300 {
		t.Fatalf("failed TryReserve consumed budget: remaining = %d, want 300", got)
	}
	b.Release(700)
	if got := b.remaining.Load(); got != 1000 {
		t.Fatalf("Release did not restore: remaining = %d, want 1000", got)
	}

	// Disabled and nil budgets: every exported method is an inert no-op.
	for _, d := range []*InboundBudget{nil, NewInboundBudget(0), NewInboundBudget(-1)} {
		if d.Enabled() {
			t.Fatalf("Enabled() = true for disabled/nil budget %v, want false", d)
		}
		if !d.TryReserve(1 << 40) {
			t.Fatalf("disabled/nil budget TryReserve = false, want true (no-op) for %v", d)
		}
		d.Release(1 << 40) // must not panic
	}
}

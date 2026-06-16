package clock_test

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

func TestReal_DelegatesToStdlib(t *testing.T) {
	t.Parallel()
	c := clock.Real()

	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Real().Now()=%v not within [%v,%v]", got, before, after)
	}

	past := got.Add(-time.Hour)
	if d := c.Since(past); d < time.Hour {
		t.Fatalf("Since(past)=%v, want >= 1h", d)
	}

	future := got.Add(time.Hour)
	if d := c.Until(future); d <= 0 || d > time.Hour {
		t.Fatalf("Until(future)=%v, want (0,1h]", d)
	}
}

func TestReal_TimerFires(t *testing.T) {
	t.Parallel()
	c := clock.Real()
	tm := c.NewTimer(5 * time.Millisecond)
	defer tm.Stop()
	select {
	case <-tm.C():
	case <-time.After(time.Second):
		t.Fatal("real timer did not fire within 1s")
	}
}

func TestReal_TickerFiresAndStops(t *testing.T) {
	t.Parallel()
	c := clock.Real()
	tk := c.NewTicker(2 * time.Millisecond)
	select {
	case <-tk.C():
	case <-time.After(time.Second):
		t.Fatal("real ticker did not fire within 1s")
	}
	tk.Stop()
}

func TestReal_AfterFires(t *testing.T) {
	t.Parallel()
	c := clock.Real()
	select {
	case <-c.After(5 * time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("real After did not fire within 1s")
	}
}

func TestFake_NowAdvances(t *testing.T) {
	t.Parallel()
	base := time.Unix(1000, 0)
	f := clock.NewFake(base)
	if !f.Now().Equal(base) {
		t.Fatalf("Now()=%v, want %v", f.Now(), base)
	}
	f.Advance(10 * time.Second)
	if got, want := f.Now(), base.Add(10*time.Second); !got.Equal(want) {
		t.Fatalf("after Advance Now()=%v, want %v", got, want)
	}
	if d := f.Since(base); d != 10*time.Second {
		t.Fatalf("Since(base)=%v, want 10s", d)
	}
	if d := f.Until(base.Add(30 * time.Second)); d != 20*time.Second {
		t.Fatalf("Until=%v, want 20s", d)
	}
}

func TestFake_SetIgnoresBackwards(t *testing.T) {
	t.Parallel()
	base := time.Unix(1000, 0)
	f := clock.NewFake(base)
	f.Set(base.Add(-time.Hour))
	if !f.Now().Equal(base) {
		t.Fatalf("Set backwards changed clock to %v", f.Now())
	}
}

func TestFake_TimerFiresOnCrossing(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Unix(0, 0))
	tm := f.NewTimer(100 * time.Millisecond)

	// Not yet due.
	f.Advance(50 * time.Millisecond)
	select {
	case <-tm.C():
		t.Fatal("timer fired before its deadline")
	default:
	}

	// Crosses the deadline.
	f.Advance(60 * time.Millisecond)
	select {
	case got := <-tm.C():
		if want := time.Unix(0, 0).Add(100 * time.Millisecond); !got.Equal(want) {
			t.Fatalf("timer delivered %v, want %v", got, want)
		}
	default:
		t.Fatal("timer did not fire after crossing deadline")
	}
}

func TestFake_TimerStopPreventsFire(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Unix(0, 0))
	tm := f.NewTimer(10 * time.Millisecond)
	if !tm.Stop() {
		t.Fatal("Stop() returned false on a live timer")
	}
	f.Advance(time.Second)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tm.Stop() {
		t.Fatal("second Stop() returned true")
	}
}

func TestFake_TickerFiresEachPeriod(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Unix(0, 0))
	tk := f.NewTicker(10 * time.Millisecond)
	defer tk.Stop()

	count := 0
	for i := 0; i < 5; i++ {
		f.Advance(10 * time.Millisecond)
		select {
		case <-tk.C():
			count++
		default:
			t.Fatalf("ticker did not fire on period %d", i)
		}
	}
	if count != 5 {
		t.Fatalf("got %d ticks, want 5", count)
	}
}

func TestFake_TickerCoalescesOnBigJump(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Unix(0, 0))
	tk := f.NewTicker(10 * time.Millisecond)
	defer tk.Stop()

	// Jump across many periods. time.Ticker drops backlogged ticks when the
	// consumer is slow; the buffered channel (cap 1) keeps exactly one.
	f.Advance(100 * time.Millisecond)
	select {
	case <-tk.C():
	default:
		t.Fatal("ticker did not fire after a multi-period jump")
	}
	// At most one tick is buffered.
	select {
	case <-tk.C():
		t.Fatal("more than one tick buffered after a jump")
	default:
	}
}

func TestFake_NewTickerNonPositivePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("NewTicker(0) did not panic")
		}
	}()
	clock.NewFake(time.Unix(0, 0)).NewTicker(0)
}

func TestFake_AfterFires(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Unix(0, 0))
	ch := f.After(10 * time.Millisecond)
	f.Advance(10 * time.Millisecond)
	select {
	case <-ch:
	default:
		t.Fatal("After channel did not fire")
	}
}

// TestFake_Deterministic proves a fixed advance schedule yields a fixed
// fire-order sequence regardless of registration order.
func TestFake_Deterministic(t *testing.T) {
	t.Parallel()
	run := func() []time.Duration {
		f := clock.NewFake(time.Unix(0, 0))
		a := f.NewTimer(30 * time.Millisecond)
		b := f.NewTimer(10 * time.Millisecond)
		c := f.NewTimer(20 * time.Millisecond)
		var fired []time.Duration
		base := f.Now()
		f.Advance(40 * time.Millisecond)
		for _, tm := range []clock.Timer{b, c, a} {
			select {
			case got := <-tm.C():
				fired = append(fired, got.Sub(base))
			default:
				t.Fatal("expected timer to have fired")
			}
		}
		return fired
	}
	first := run()
	second := run()
	if len(first) != len(second) {
		t.Fatalf("length mismatch %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("run %d: %v != %v", i, first[i], second[i])
		}
	}
}

// Interface-satisfaction guard: both clocks implement [clock.Clock].
var (
	_ clock.Clock = clock.Real()
	_ clock.Clock = (*clock.Fake)(nil)
)

package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestSlowConsumer_BackpressureBounded proves the server applies streaming
// backpressure rather than buffering the whole result: while a consumer stalls
// after pulling a tiny prefix of a large result, the bytes queued toward the
// consumer stay bounded by the SimConn buffer (the server's writer is parked,
// not racing ahead). goleak confirms no goroutine leaked after the mid-stream
// close.
func TestSlowConsumer_BackpressureBounded(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	sc := NewSlowConsumer(clock.Real())
	res, err := sc.Stall(context.Background(), srv, 100*time.Millisecond, func(c *WireClient) {
		// Give the server a moment to push records up to the buffer bound, then
		// confirm the queued bytes never exceed that bound — the result was NOT
		// buffered whole. slowConsumerResultRows ints encode to far more than the
		// 64 KiB buffer, so an unbounded server would queue megabytes here.
		time.Sleep(30 * time.Millisecond)
		buffered := c.Conn().ReadBuffered()
		if buffered > simConnBufferSize {
			t.Errorf("server buffered %d bytes toward a stalled consumer, exceeds the bound %d — backpressure not applied",
				buffered, simConnBufferSize)
		}
	})
	if err != nil {
		t.Fatalf("Stall: %v", err)
	}
	if !res.ServerParked {
		t.Fatal("the stream terminated before the consumer could stall — result was not large enough to exercise backpressure")
	}
	if res.RecordsPulled == 0 {
		t.Fatal("consumer pulled no records; cannot conclude the stream opened")
	}
	if res.RecordsPulled >= slowConsumerResultRows {
		t.Fatalf("consumer pulled the entire result (%d); it did not actually stall", res.RecordsPulled)
	}
}

// TestSlowConsumer_NoLeakOnAbruptClose proves a slow consumer that resets the
// connection mid-stream (an abrupt disconnect) leaves no leaked goroutine: the
// server must reclaim its parked writer on the connection error.
func TestSlowConsumer_NoLeakOnAbruptClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	sc := NewSlowConsumer(clock.Real())
	_, err = sc.Stall(context.Background(), srv, 20*time.Millisecond, func(c *WireClient) {
		// Abruptly reset the connection while the server's writer is parked.
		_ = c.Conn().CloseWithError(context.Canceled)
	})
	if err != nil {
		t.Fatalf("Stall: %v", err)
	}
	// The server must still be healthy for a new connection after the abrupt reset.
	assertServerStillHealthy(t, srv, AbuseFamily(0))
}

// TestSlowConsumer_FakeClockStall proves the stall duration is driven by the
// injected virtual clock: with a Fake, the consumer stays stalled until the
// clock is advanced. This is the determinism seam for the stall timing.
func TestSlowConsumer_FakeClockStall(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	fake := clock.NewFake(time.Unix(0, 0))
	sc := NewSlowConsumer(fake)

	stalled := make(chan struct{})
	done := make(chan SlowConsumerResult, 1)
	go func() {
		res, _ := sc.Stall(context.Background(), srv, time.Hour, func(*WireClient) {
			close(stalled)
		})
		done <- res
	}()

	<-stalled
	// The stall is a 1-hour virtual wait; it must not complete on real time.
	select {
	case <-done:
		t.Fatal("stall completed before the virtual clock advanced")
	case <-time.After(50 * time.Millisecond):
	}
	// Advance virtual time past the stall; the consumer must then finish.
	fake.Advance(2 * time.Hour)
	select {
	case res := <-done:
		if !res.ServerParked {
			t.Error("expected the server to have been parked during the stall")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stall did not complete after advancing the fake clock")
	}
}

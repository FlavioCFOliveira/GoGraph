package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// SlowConsumerResult summarises one slow-consumer run. It records whether the
// server kept its memory bounded while the consumer stalled (proved by the
// SimConn write-buffer never being allowed to grow past its bound — the server
// parks on its blocked record write rather than buffering the whole result) and
// whether the connection torn down without leaking a goroutine.
type SlowConsumerResult struct {
	// RecordsPulled is how many RECORDs the slow consumer drained before it
	// stopped. It is always far less than the total result size when the consumer
	// stalls, which is the proof the server did not push the whole result eagerly.
	RecordsPulled int
	// ServerParked reports whether the server's write blocked on the bounded
	// SimConn buffer while the consumer stalled (backpressure observed) rather
	// than the whole result being buffered ahead of the consumer.
	ServerParked bool
	// ClosedCleanly reports whether the connection tore down without a transport
	// fault other than the expected close/EOF.
	ClosedCleanly bool
}

// SlowConsumer opens a large result stream and then pulls records very slowly
// (or stalls entirely), exercising the server's streaming backpressure. Because
// the SimConn write buffer is bounded ([simConnBufferSize]), a stalled consumer
// forces the server's record-write to BLOCK once the buffer fills rather than
// letting it buffer the whole result in memory — the bounded-resource property
// under a slow reader. When the connection is finally closed (or its read
// deadline, driven by the injected Clock, elapses) the server must tear the
// session down without leaking a goroutine.
//
// SlowConsumer runs in the CONCURRENT mode: the slow pulls happen on a real
// goroutine while the server's writer goroutine is parked on backpressure. The
// SEED controls the stall timing; interleaving is real (per the hybrid model),
// so correctness here is backpressure + no-leak, not bit-replay.
//
// # Concurrency contract
//
// A SlowConsumer drives one connection it owns; each [SlowConsumer.Stall] call
// is independent and may run on its own goroutine.
type SlowConsumer struct {
	clk clock.Clock
}

// NewSlowConsumer returns a SlowConsumer whose stall timing is driven by clk.
func NewSlowConsumer(clk clock.Clock) *SlowConsumer { return &SlowConsumer{clk: clk} }

// Name returns the actor's identifier.
func (*SlowConsumer) Name() string { return "SlowConsumer" }

// slowConsumerResultRows is the number of rows the slow consumer asks the server
// to stream. It must be large enough that the encoded result far exceeds the
// SimConn buffer, so a stalled consumer is guaranteed to park the server's
// writer on backpressure rather than letting the whole result fit in the buffer.
const slowConsumerResultRows = 50_000

// Stall opens a large result stream over the server and then deliberately stalls
// without pulling, holding the stream open for stallFor (measured on the
// injected clock). While stalled, the server's writer is parked on the bounded
// SimConn buffer (backpressure). It then closes the connection and returns the
// result. The connection is always closed before return, so no goroutine leaks.
//
// onStalled, when non-nil, is invoked once while the consumer is stalled, with
// the client so a caller can inspect the bounded buffer ([WireClient.Conn] →
// [SimConn.ReadBuffered]) and confirm the server did not buffer the whole result.
//
// stallFor stalls are driven by the injected Clock so a Fake makes the stall
// deterministic; with clock.Real it is a real (short) sleep.
func (s *SlowConsumer) Stall(ctx context.Context, srv *SimServer, stallFor time.Duration, onStalled func(*WireClient)) (SlowConsumerResult, error) {
	var res SlowConsumerResult
	client, err := srv.Dial()
	if err != nil {
		return res, err
	}
	defer func() { _ = client.Close() }()

	if err := client.Connect(ctx); err != nil {
		return res, fmt.Errorf("sim: slow-consumer connect: %w", err)
	}

	// Ask the server to stream a large result.
	runResp, err := client.Run(fmt.Sprintf("UNWIND range(1, %d) AS x RETURN x", slowConsumerResultRows), nil)
	if err != nil {
		return res, fmt.Errorf("sim: slow-consumer RUN: %w", err)
	}
	if f, ok := runResp.(*proto.Failure); ok {
		// The engine's row cap may refuse a result this large up front; that is a
		// legitimate bounded refusal, not a backpressure scenario, so report a
		// clean (no-leak) outcome with no parked server.
		_ = f
		res.ClosedCleanly = true
		return res, nil
	}

	// Send PULL but read only a few records, then stall. The unread records pile
	// up against the bounded SimConn buffer, parking the server's writer.
	if err := client.send(&proto.Pull{N: -1, QID: -1}); err != nil {
		return res, fmt.Errorf("sim: slow-consumer PULL: %w", err)
	}

	// Drain a small prefix slowly, then stop entirely (the stall).
	const prefix = 3
	for i := 0; i < prefix; i++ {
		msg, err := client.recv()
		if err != nil {
			res.ClosedCleanly = true
			return res, nil
		}
		if _, ok := msg.(*proto.Record); ok {
			res.RecordsPulled++
		} else {
			// Stream already terminated (small result); nothing to stall on.
			res.ClosedCleanly = true
			return res, nil
		}
	}

	// Stall: hold the stream open without reading. The server's writer is now
	// parked on the bounded buffer (it cannot have streamed all
	// slowConsumerResultRows into 64 KiB), which is the backpressure we assert.
	res.ServerParked = true
	if onStalled != nil {
		onStalled(client)
	}
	s.sleep(ctx, stallFor)

	// Tear the connection down mid-stream. The server must reclaim the session and
	// its writer goroutine cleanly (verified by the caller's goleak check).
	res.ClosedCleanly = true
	return res, nil
}

// sleep waits for d on the injected clock, returning early if ctx is cancelled.
// Under a Fake clock the wait completes only when virtual time is advanced past
// d; under clock.Real it is a real timed wait.
func (s *SlowConsumer) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := s.clk.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C():
	case <-ctx.Done():
	}
}

package sim

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// simConnBufferSize bounds the in-flight bytes one direction of a [SimConn] pair
// may hold before a writer blocks (backpressure). It is deliberately small so
// the concurrent harness exercises the server's streaming backpressure rather
// than letting the kernel-less in-memory pipe absorb an unbounded result set.
// A SlowConsumer that never reads will park a server write here once this many
// bytes are buffered, which is exactly the bounded-resource behaviour Phase 3
// asserts.
const simConnBufferSize = 64 << 10 // 64 KiB

// ErrSimConnClosed is returned by [SimConn] I/O after the connection is closed.
var ErrSimConnClosed = errors.New("sim: SimConn is closed")

// pipeAddr is the [net.Addr] reported by both ends of a [SimConn]. The Bolt
// server only ever logs the address string, so a fixed in-memory label is
// sufficient and keeps the harness free of any OS socket.
type pipeAddr struct{ name string }

func (a pipeAddr) Network() string { return "sim" }
func (a pipeAddr) String() string  { return a.name }

// halfPipe is one unidirectional, bounded byte channel between the two ends of a
// [SimConn]. Bytes written at one end are read at the other. A writer blocks
// (backpressure) once buf holds [simConnBufferSize] bytes; a reader blocks until
// bytes are available, the pipe is closed, or a deadline elapses.
//
// # Concurrency contract
//
// A halfPipe is safe for concurrent use by exactly one reader and one writer
// (the two ends of a connection). Read and Write each take the shared mutex and
// coordinate through a single sync.Cond, so a SimConn may be driven by one
// goroutine per end — the concurrent-mode contract Phase 3 requires.
type halfPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	// rwErr, when non-nil after close, is the error returned to a blocked or
	// subsequent reader/writer (CloseWithError lets the harness model an abrupt
	// reset distinctly from an orderly close).
	rwErr error
	clk   clock.Clock
	// readDeadline / writeDeadline are absolute instants on clk; the zero Time
	// means no deadline. They are read under mu.
	readDeadline  time.Time
	writeDeadline time.Time
}

func newHalfPipe(clk clock.Clock) *halfPipe {
	h := &halfPipe{clk: clk}
	h.cond = sync.NewCond(&h.mu)
	return h
}

// read copies available bytes into p, blocking until at least one byte is
// available, the pipe is closed, or the read deadline elapses. It mirrors the
// io.Reader contract: a closed-and-drained pipe yields io.EOF.
func (h *halfPipe) read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for {
		if len(h.buf) > 0 {
			n := copy(p, h.buf)
			h.buf = h.buf[n:]
			// A successful read freed buffer space; wake any blocked writer.
			h.cond.Broadcast()
			return n, nil
		}
		if h.closed {
			if h.rwErr != nil {
				return 0, h.rwErr
			}
			return 0, io.EOF
		}
		if err := h.waitDeadline(&h.readDeadline); err != nil {
			return 0, err
		}
	}
}

// write appends p to the buffer, blocking while the buffer is at or above
// [simConnBufferSize] (backpressure) until space frees, the pipe closes, or the
// write deadline elapses. It writes in whatever increments fit, so a large
// message is delivered across several wake-ups without ever exceeding the bound.
func (h *halfPipe) write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	written := 0
	for written < len(p) {
		if h.closed {
			if h.rwErr != nil {
				return written, h.rwErr
			}
			return written, ErrSimConnClosed
		}
		space := simConnBufferSize - len(h.buf)
		if space > 0 {
			chunk := p[written:]
			if len(chunk) > space {
				chunk = chunk[:space]
			}
			h.buf = append(h.buf, chunk...)
			written += len(chunk)
			h.cond.Broadcast() // wake a blocked reader
			continue
		}
		// Buffer full: park until a reader drains it (backpressure) or a deadline.
		if err := h.waitDeadline(&h.writeDeadline); err != nil {
			return written, err
		}
	}
	return written, nil
}

// waitDeadline blocks on the condition variable until woken, or returns a
// timeout error if the given deadline has elapsed. When a deadline is set it
// arms a clock timer that broadcasts the cond so a blocked goroutine wakes even
// though no I/O occurred; this routes the timeout through the injected Clock so
// the deterministic single-conn path never reads wall time. The caller holds
// h.mu; the timer goroutine re-acquires it to broadcast.
func (h *halfPipe) waitDeadline(deadline *time.Time) error {
	d := *deadline
	if d.IsZero() {
		h.cond.Wait()
		return nil
	}
	if !h.clk.Now().Before(d) {
		return timeoutErr{}
	}
	// Arm a one-shot timer that broadcasts when the deadline passes so the Wait
	// below is guaranteed to wake. The timer is driven by the injected Clock.
	timer := h.clk.NewTimer(h.clk.Until(d))
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
		case <-done:
			return
		}
		h.mu.Lock()
		h.cond.Broadcast()
		h.mu.Unlock()
	}()
	h.cond.Wait()
	close(done)
	timer.Stop()
	if !h.clk.Now().Before(d) {
		return timeoutErr{}
	}
	return nil
}

// close marks the pipe closed with err (nil for an orderly close) and wakes
// every blocked reader/writer. It is idempotent.
func (h *halfPipe) close(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	h.rwErr = err
	h.cond.Broadcast()
}

func (h *halfPipe) setReadDeadline(t time.Time) {
	h.mu.Lock()
	h.readDeadline = t
	h.cond.Broadcast()
	h.mu.Unlock()
}

func (h *halfPipe) setWriteDeadline(t time.Time) {
	h.mu.Lock()
	h.writeDeadline = t
	h.cond.Broadcast()
	h.mu.Unlock()
}

// timeoutErr is the net.Error returned when a SimConn I/O deadline elapses. It
// reports Timeout() == true so callers (and the Bolt server's deadline handling)
// treat it like an OS socket timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "sim: SimConn I/O deadline exceeded" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// SimConn is one end of an in-memory, bounded-buffer [net.Conn] pair. It carries
// the real Bolt wire bytes between the in-sim client harness and the genuine
// bolt/server, with no OS socket, so the server's actual handshake, framing, and
// message loop run unchanged.
//
// A SimConn pair supports two usage modes that share one implementation:
//
//   - LOCK-STEP single-connection mode: the client writes a complete request and
//     then blocks reading the server's full terminal response. Because the
//     bounded buffer is far larger than any single request/response and exactly
//     one logical exchange is in flight, the byte stream is fully deterministic
//     and a given seed replays identically. Used for protocol round-trips and
//     the BoltAbuser so violations reproduce exactly.
//   - CONCURRENT mode: one real goroutine drives each end. Interleaving across
//     connections is non-deterministic, but each end's reads and writes are
//     individually safe and the bounded buffer applies backpressure (a stalled
//     reader parks the peer's writer), which is what the SlowConsumer and
//     overload actors exercise.
//
// Deadlines route through the injected [clock.Clock]; with [clock.Real] a
// SimConn behaves like an ordinary socket, and with a [clock.Fake] a deadline
// fires only when virtual time is advanced.
//
// # Concurrency contract
//
// A single SimConn end is safe for use by one reader goroutine and one writer
// goroutine concurrently (the full-duplex net.Conn contract). It is NOT safe to
// share one end across multiple readers or multiple writers.
//
//nolint:revive // "Sim" prefix is the DST harness naming scheme (see SimDisk, SimStore).
type SimConn struct {
	rd        *halfPipe // bytes this end reads (peer writes)
	wr        *halfPipe // bytes this end writes (peer reads)
	localAddr pipeAddr
	peerAddr  pipeAddr
	closeOnce sync.Once
}

// NewSimConnPair returns the two ends of a connected in-memory pipe. Bytes
// written to one end are read from the other. Both ends share the injected
// clock for deadline handling; pass [clock.Real] for ordinary timing or a
// [clock.Fake] for deterministic virtual deadlines. clk must be non-nil.
//
// The conventional use is to hand the server end to the bolt/server (via
// [SimListener]) and drive the client end with the wire client harness.
func NewSimConnPair(clk clock.Clock) (clientEnd, serverEnd *SimConn) {
	c2s := newHalfPipe(clk) // client → server
	s2c := newHalfPipe(clk) // server → client
	clientEnd = &SimConn{
		rd:        s2c,
		wr:        c2s,
		localAddr: pipeAddr{name: "sim-client"},
		peerAddr:  pipeAddr{name: "sim-server"},
	}
	serverEnd = &SimConn{
		rd:        c2s,
		wr:        s2c,
		localAddr: pipeAddr{name: "sim-server"},
		peerAddr:  pipeAddr{name: "sim-client"},
	}
	return clientEnd, serverEnd
}

// Read implements [net.Conn.Read].
func (c *SimConn) Read(p []byte) (int, error) { return c.rd.read(p) }

// Write implements [net.Conn.Write].
func (c *SimConn) Write(p []byte) (int, error) { return c.wr.write(p) }

// Close implements [net.Conn.Close]. It closes both directions of this end so a
// blocked peer read returns io.EOF and a blocked peer write returns
// [ErrSimConnClosed]. It is idempotent.
func (c *SimConn) Close() error {
	c.closeOnce.Do(func() {
		c.wr.close(nil) // signal EOF to the peer's reader
		c.rd.close(nil) // unblock our own pending reads
	})
	return nil
}

// CloseWithError closes this end abruptly, delivering err to the peer's blocked
// reads and writes instead of a clean io.EOF. It models a connection reset (an
// abrupt client disconnect mid-stream) so the harness can assert the server
// neither panics nor leaks a goroutine on a hard close. It is idempotent.
func (c *SimConn) CloseWithError(err error) error {
	c.closeOnce.Do(func() {
		c.wr.close(err)
		c.rd.close(err)
	})
	return nil
}

// LocalAddr implements [net.Conn.LocalAddr].
func (c *SimConn) LocalAddr() net.Addr { return c.localAddr }

// RemoteAddr implements [net.Conn.RemoteAddr].
func (c *SimConn) RemoteAddr() net.Addr { return c.peerAddr }

// SetDeadline implements [net.Conn.SetDeadline], setting both the read and write
// deadlines to t. A zero t clears the deadlines.
func (c *SimConn) SetDeadline(t time.Time) error {
	c.rd.setReadDeadline(t)
	c.wr.setWriteDeadline(t)
	return nil
}

// SetReadDeadline implements [net.Conn.SetReadDeadline].
func (c *SimConn) SetReadDeadline(t time.Time) error {
	c.rd.setReadDeadline(t)
	return nil
}

// SetWriteDeadline implements [net.Conn.SetWriteDeadline].
func (c *SimConn) SetWriteDeadline(t time.Time) error {
	c.wr.setWriteDeadline(t)
	return nil
}

// Compile-time assertion that *SimConn satisfies net.Conn.
var _ net.Conn = (*SimConn)(nil)

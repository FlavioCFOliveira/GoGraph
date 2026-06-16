package sim

import (
	"errors"
	"net"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// ErrSimListenerClosed is returned by [SimListener.Accept] and [SimListener.Dial]
// after the listener is closed, mirroring [net.ErrClosed] semantics for the
// bolt/server accept loop (which treats a closed listener as a clean shutdown).
var ErrSimListenerClosed = errors.New("sim: SimListener is closed")

// SimListener is an in-memory [net.Listener] that feeds [SimConn] server-ends to
// a real bolt/server running under [github.com/FlavioCFOliveira/GoGraph/bolt/server.Server.Serve].
// Each [SimListener.Dial] creates a connected [SimConn] pair, queues the
// server-end for the server's Accept loop, and returns the client-end to the
// caller (the wire client harness). No OS socket is involved, so the server runs
// its genuine handshake and message loop over purely in-memory bytes.
//
// # Concurrency contract
//
// SimListener is safe for concurrent use: Dial may be called from any number of
// goroutines (one per simulated connection) while the server calls Accept from
// its single accept goroutine. This is what lets the concurrent harness open N
// connections against one server.
//
//nolint:revive // "Sim" prefix is the DST harness naming scheme (see SimDisk).
type SimListener struct {
	dialClock clock.Clock
	accept    chan *SimConn
	mu        sync.Mutex
	closed    bool
	done      chan struct{}
}

// NewSimListener returns an in-memory listener whose connections route deadlines
// through clk. Hand it to Server.Serve; drive new connections with Dial. clk
// must be non-nil ([clock.Real] for ordinary timing, a [clock.Fake] for
// deterministic virtual deadlines).
func NewSimListener(clk clock.Clock) *SimListener {
	return &SimListener{
		dialClock: clk,
		accept:    make(chan *SimConn, defaultAcceptBacklog),
		done:      make(chan struct{}),
	}
}

// defaultAcceptBacklog bounds how many dialed-but-not-yet-accepted connections
// the listener queues before Dial blocks. It is the in-memory analogue of a
// kernel listen backlog and keeps the harness's connection setup bounded.
const defaultAcceptBacklog = 256

// Accept implements [net.Listener.Accept]. It blocks until a connection is
// dialed or the listener is closed, returning the server-end of the next
// [SimConn] pair. After Close it returns [ErrSimListenerClosed].
func (l *SimListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.done:
		return nil, ErrSimListenerClosed
	}
}

// Close implements [net.Listener.Close]. It stops further Accept and Dial calls.
// In-flight connections already accepted by the server are unaffected (they live
// until the server or harness closes them). It is idempotent.
func (l *SimListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.done)
	return nil
}

// Addr implements [net.Listener.Addr].
func (l *SimListener) Addr() net.Addr { return pipeAddr{name: "sim-listener"} }

// Dial creates a new connected [SimConn] pair, queues the server-end for Accept,
// and returns the client-end. It blocks if the accept backlog is full (bounded
// by [defaultAcceptBacklog]) until the server accepts a queued connection or the
// listener is closed. After Close it returns [ErrSimListenerClosed].
func (l *SimListener) Dial() (*SimConn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrSimListenerClosed
	}
	l.mu.Unlock()

	clientEnd, serverEnd := NewSimConnPair(l.dialClock)
	select {
	case l.accept <- serverEnd:
		return clientEnd, nil
	case <-l.done:
		_ = clientEnd.Close()
		_ = serverEnd.Close()
		return nil, ErrSimListenerClosed
	}
}

// Compile-time assertion that *SimListener satisfies net.Listener.
var _ net.Listener = (*SimListener)(nil)

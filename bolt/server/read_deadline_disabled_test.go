package server

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// read_deadline_disabled_test.go — rmp #2807.
//
// Disabling Options.ConnTimeout by default puts one specific mistake on the path
// every server takes: SetReadDeadline(time.Now().Add(0)) is an IMMEDIATE
// deadline, not an absent one. Armed at the top of the reader loop it would have
// expired before the read began, so every read on every connection would fail
// with os.ErrDeadlineExceeded and the server would serve nothing at all. The
// absent deadline is the ZERO time.
//
// Layer: short. Pure function, no I/O.

// TestReadDeadlineAt_DisabledClearsRatherThanArms is the direct gate on that
// distinction. The disabled arms must yield a zero time.Time; the enabled arm
// must yield a real future instant, which is what stops the function being
// "return time.Time{}" and passing.
func TestReadDeadlineAt_DisabledClearsRatherThanArms(t *testing.T) {
	t.Parallel()

	t.Run("zero-clears", func(t *testing.T) {
		t.Parallel()
		got := readDeadlineAt(0)
		if !got.IsZero() {
			t.Fatalf("readDeadlineAt(0) = %v; want the ZERO time. A non-zero value here is an IMMEDIATE deadline: "+
				"time.Now().Add(0) has already expired by the time the read starts, so every read on every "+
				"connection would fail with os.ErrDeadlineExceeded", got)
		}
	})

	t.Run("negative-clears", func(t *testing.T) {
		t.Parallel()
		// NewServer refuses a negative Options.ConnTimeout, so this arm is
		// defence in depth for any future caller that reaches the helper with
		// one: a deadline in the past is strictly worse than none.
		if got := readDeadlineAt(-time.Second); !got.IsZero() {
			t.Fatalf("readDeadlineAt(-1s) = %v; want the ZERO time", got)
		}
	})

	t.Run("positive-arms", func(t *testing.T) {
		t.Parallel()
		const bound = 250 * time.Millisecond
		before := time.Now()
		got := readDeadlineAt(bound)
		after := time.Now()
		if got.IsZero() {
			t.Fatalf("readDeadlineAt(%v) = the zero time; want an armed deadline. Without this clause the "+
				"function could return time.Time{} unconditionally and still pass the two clauses above", bound)
		}
		if got.Before(before.Add(bound)) || got.After(after.Add(bound)) {
			t.Fatalf("readDeadlineAt(%v) = %v; want an instant in [%v, %v]",
				bound, got, before.Add(bound), after.Add(bound))
		}
	})
}

// deadlineRecorderConn is a net.Conn that records every deadline the server
// installs on it — and by WHICH method — then, once the scripted handshake bytes
// are exhausted, blocks every read until Close.
//
// It exists because asserting on [readDeadlineAt] alone would prove only that
// the helper is correct, not that the reader goroutine calls it. This conn is
// what watches the CALL SITE.
//
// The method matters. handleConn brackets the handshake with SetDeadline (both
// directions at once) and writeResponse uses SetWriteDeadline; only the reader
// goroutine calls SetReadDeadline. Recording the method is therefore what
// separates the reader's deadline from the handshake's, without any guesswork
// about ordering.
type deadlineRecorderConn struct {
	mu        sync.Mutex
	recorded  []recordedDeadline
	script    []byte // remaining handshake bytes to hand to the server
	unblocked chan struct{}
	closeOnce sync.Once
}

// recordedDeadline is one deadline installation: the net.Conn method used and
// the instant passed to it.
type recordedDeadline struct {
	method string // "SetDeadline", "SetReadDeadline" or "SetWriteDeadline"
	at     time.Time
}

func newDeadlineRecorderConn(script []byte) *deadlineRecorderConn {
	return &deadlineRecorderConn{script: script, unblocked: make(chan struct{})}
}

func (c *deadlineRecorderConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.script) > 0 {
		n := copy(p, c.script)
		c.script = c.script[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	<-c.unblocked // the client is silent for ever; only Close ends this
	return 0, io.EOF
}

func (c *deadlineRecorderConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *deadlineRecorderConn) Close() error {
	c.closeOnce.Do(func() { close(c.unblocked) })
	return nil
}

func (c *deadlineRecorderConn) LocalAddr() net.Addr  { return fakeAddr{} }
func (c *deadlineRecorderConn) RemoteAddr() net.Addr { return fakeAddr{} }

func (c *deadlineRecorderConn) record(method string, t time.Time) error {
	c.mu.Lock()
	c.recorded = append(c.recorded, recordedDeadline{method: method, at: t})
	c.mu.Unlock()
	return nil
}

func (c *deadlineRecorderConn) SetDeadline(t time.Time) error {
	return c.record("SetDeadline", t)
}

func (c *deadlineRecorderConn) SetReadDeadline(t time.Time) error {
	return c.record("SetReadDeadline", t)
}

func (c *deadlineRecorderConn) SetWriteDeadline(t time.Time) error {
	return c.record("SetWriteDeadline", t)
}

// readerDeadlines returns every deadline installed with SetReadDeadline, i.e.
// exactly those the reader goroutine installed before a read.
func (c *deadlineRecorderConn) readerDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Time
	for _, r := range c.recorded {
		if r.method == "SetReadDeadline" {
			out = append(out, r.at)
		}
	}
	return out
}

// allRecorded returns a copy of every recorded installation, for diagnostics.
func (c *deadlineRecorderConn) allRecorded() []recordedDeadline {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]recordedDeadline, len(c.recorded))
	copy(out, c.recorded)
	return out
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "recorder" }
func (fakeAddr) String() string  { return "recorder:0" }

// handshakeScript is the 20-byte Bolt handshake a client sends: the magic
// preamble followed by four version slots, the first offering the highest
// version this server supports.
func handshakeScript() []byte {
	v := proto.SupportedVersions[0]
	b := make([]byte, 20)
	binary.BigEndian.PutUint32(b[:4], proto.Magic)
	b[4], b[5], b[6], b[7] = 0x00, 0x00, v.Minor, v.Major
	return b
}

// TestReaderGoroutine_DoesNotArmAnImmediateDeadlineWhenDisabled watches the CALL
// SITE, not the helper: it hands [Server.handleConn] a connection that records
// every deadline the server installs, and asserts what the reader goroutine
// actually did.
//
// The disabled arm is the gate: every read deadline the reader installs must be
// the ZERO time. An implementation that armed time.Now().Add(s.opts.ConnTimeout)
// unconditionally would record a NON-zero instant here, which is the immediate
// deadline that would break every connection.
//
// The enabled arm is the non-vacuity control. Without it the disabled arm would
// pass against a reader that installed no deadline at all, or against a recorder
// that observed nothing.
func TestReaderGoroutine_DoesNotArmAnImmediateDeadlineWhenDisabled(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, connTimeout time.Duration) []time.Time {
		t.Helper()
		srv, err := NewServer(newTestEngine(t), Options{
			Auth:        NoAuthHandler{},
			ConnTimeout: connTimeout,
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		conn := newDeadlineRecorderConn(handshakeScript())
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.handleConn(context.Background(), conn)
		}()

		// Wait until the reader goroutine has reached its first read, i.e. has
		// installed at least one deadline after the handshake was cleared.
		deadline := time.Now().Add(5 * time.Second)
		var got []time.Time
		for {
			got = conn.readerDeadlines()
			if len(got) > 0 {
				break
			}
			if time.Now().After(deadline) {
				all := conn.allRecorded()
				_ = conn.Close()
				<-done
				t.Fatalf("the reader goroutine called SetReadDeadline not once within 5s (ConnTimeout=%v); "+
					"the recorder saw these installations: %+v", connTimeout, all)
			}
			time.Sleep(2 * time.Millisecond)
		}
		_ = conn.Close()
		<-done
		return got
	}

	t.Run("disabled-clears", func(t *testing.T) {
		t.Parallel()
		for i, dl := range run(t, 0) {
			if !dl.IsZero() {
				t.Fatalf("read deadline #%d installed by the reader with ConnTimeout disabled = %v; want the ZERO time. "+
					"A non-zero deadline here is time.Now().Add(0) — already expired when the read starts — and would "+
					"fail every read on every connection", i, dl)
			}
		}
	})

	t.Run("enabled-arms", func(t *testing.T) {
		t.Parallel()
		const bound = 30 * time.Second
		got := run(t, bound)
		if got[0].IsZero() {
			t.Fatalf("with ConnTimeout=%v the reader installed the ZERO time; want an armed deadline. "+
				"Without this control the disabled arm would pass against a reader that never armed anything", bound)
		}
		if until := time.Until(got[0]); until <= 0 || until > bound {
			t.Fatalf("with ConnTimeout=%v the reader installed a deadline %v away; want it within (0, %v]",
				bound, until, bound)
		}
	})
}

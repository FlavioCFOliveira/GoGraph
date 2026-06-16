package sim

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestSimConn_RoundTripBytes proves the basic full-duplex byte contract: bytes
// written at one end are read at the other, in both directions.
func TestSimConn_RoundTripBytes(t *testing.T) {
	t.Parallel()
	a, b := NewSimConnPair(clock.Real())

	want := []byte("hello bolt wire")
	go func() {
		_, _ = a.Write(want)
	}()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Reverse direction.
	rev := []byte("response")
	go func() { _, _ = b.Write(rev) }()
	got2 := make([]byte, len(rev))
	if _, err := io.ReadFull(a, got2); err != nil {
		t.Fatalf("reverse ReadFull: %v", err)
	}
	if !bytes.Equal(got2, rev) {
		t.Fatalf("reverse got %q, want %q", got2, rev)
	}
}

// TestSimConn_CloseEOF proves a clean Close delivers io.EOF to the peer reader.
func TestSimConn_CloseEOF(t *testing.T) {
	t.Parallel()
	a, b := NewSimConnPair(clock.Real())
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := b.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after clean close: got %v, want io.EOF", err)
	}
}

// TestSimConn_CloseWithError delivers a custom reset error to the peer.
func TestSimConn_CloseWithError(t *testing.T) {
	t.Parallel()
	a, b := NewSimConnPair(clock.Real())
	reset := errors.New("connection reset")
	if err := a.CloseWithError(reset); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	if _, err := b.Read(make([]byte, 4)); !errors.Is(err, reset) {
		t.Fatalf("peer read after reset: got %v, want %v", err, reset)
	}
}

// TestSimConn_Backpressure proves a writer blocks once the bounded buffer is
// full and unblocks when the reader drains it — the bounded-resource property
// the SlowConsumer actor relies on.
func TestSimConn_Backpressure(t *testing.T) {
	t.Parallel()
	a, b := NewSimConnPair(clock.Real())
	defer func() { _ = a.Close(); _ = b.Close() }()

	// Write more than one buffer's worth; the second write must not complete
	// until the reader drains the first.
	payload := bytes.Repeat([]byte{0xAB}, simConnBufferSize+4096)
	writeDone := make(chan int, 1)
	go func() {
		n, err := a.Write(payload)
		if err != nil {
			t.Errorf("blocked write: %v", err)
		}
		writeDone <- n
	}()

	// The write cannot have fully completed yet: only simConnBufferSize bytes fit.
	select {
	case <-writeDone:
		t.Fatal("write completed without the reader draining: buffer is not bounded")
	case <-time.After(50 * time.Millisecond):
		// Expected: writer is parked on backpressure.
	}

	// Drain everything; the writer must then finish.
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("drain ReadFull: %v", err)
	}
	select {
	case n := <-writeDone:
		if n != len(payload) {
			t.Fatalf("short write: got %d, want %d", n, len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not unblock after the reader drained the buffer")
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("drained bytes differ from written payload")
	}
}

// TestSimConn_ReadDeadlineReal proves a read deadline fires under the real clock.
func TestSimConn_ReadDeadlineReal(t *testing.T) {
	t.Parallel()
	a, b := NewSimConnPair(clock.Real())
	defer func() { _ = a.Close(); _ = b.Close() }()

	_ = b.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	_, err := b.Read(make([]byte, 4))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read after deadline: got %v, want a timeout net.Error", err)
	}
}

// TestSimConn_ReadDeadlineFakeClock proves the deadline is driven by the
// injected virtual clock: it does NOT fire on real time, only when the fake is
// advanced past it. This is the determinism seam.
func TestSimConn_ReadDeadlineFakeClock(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Unix(0, 0))
	a, b := NewSimConnPair(fake)
	defer func() { _ = a.Close(); _ = b.Close() }()

	_ = b.SetReadDeadline(fake.Now().Add(time.Second))

	readErr := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 4))
		readErr <- err
	}()

	// Real time passes but virtual time does not: the read must remain blocked.
	select {
	case err := <-readErr:
		t.Fatalf("read returned before the virtual deadline: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Advance the fake past the deadline; the read must now time out.
	fake.Advance(2 * time.Second)
	select {
	case err := <-readErr:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("read after virtual deadline: got %v, want timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not time out after advancing the fake clock")
	}
}

// TestSimConn_Addrs proves the net.Conn address contract is satisfied with the
// stable in-memory labels.
func TestSimConn_Addrs(t *testing.T) {
	t.Parallel()
	a, b := NewSimConnPair(clock.Real())
	if a.LocalAddr().String() != "sim-client" || a.RemoteAddr().String() != "sim-server" {
		t.Fatalf("client addrs wrong: local=%s remote=%s", a.LocalAddr(), a.RemoteAddr())
	}
	if b.LocalAddr().String() != "sim-server" || b.RemoteAddr().String() != "sim-client" {
		t.Fatalf("server addrs wrong: local=%s remote=%s", b.LocalAddr(), b.RemoteAddr())
	}
	if a.LocalAddr().Network() != "sim" {
		t.Fatalf("network: got %s, want sim", a.LocalAddr().Network())
	}
}

// TestSimListener_DialAccept proves the listener queues a dialed connection's
// server-end for Accept and returns a connected client-end.
func TestSimListener_DialAccept(t *testing.T) {
	t.Parallel()
	l := NewSimListener(clock.Real())
	defer func() { _ = l.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	var serverConn net.Conn
	var acceptErr error
	go func() {
		defer wg.Done()
		serverConn, acceptErr = l.Accept()
	}()

	clientConn, err := l.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	wg.Wait()
	if acceptErr != nil {
		t.Fatalf("Accept: %v", acceptErr)
	}

	// The accepted server-end and the returned client-end must be a connected pair.
	go func() { _, _ = clientConn.Write([]byte("ping")) }()
	got := make([]byte, 4)
	if _, err := io.ReadFull(serverConn, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("server read %q, want ping", got)
	}
}

// TestSimListener_ClosedRejectsAccept proves Accept returns ErrSimListenerClosed
// after Close, mirroring net.ErrClosed for the server's clean-shutdown branch.
func TestSimListener_ClosedRejectsAccept(t *testing.T) {
	t.Parallel()
	l := NewSimListener(clock.Real())
	_ = l.Close()
	if _, err := l.Accept(); !errors.Is(err, ErrSimListenerClosed) {
		t.Fatalf("Accept after Close: got %v, want ErrSimListenerClosed", err)
	}
	if _, err := l.Dial(); !errors.Is(err, ErrSimListenerClosed) {
		t.Fatalf("Dial after Close: got %v, want ErrSimListenerClosed", err)
	}
}

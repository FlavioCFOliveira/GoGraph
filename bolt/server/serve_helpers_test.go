package server

import (
	"io"
	"net"
	"testing"
)

// mockNetError implements net.Error for isTemporary tests.
type mockNetError struct{ timeout bool }

func (e *mockNetError) Error() string   { return "mock net error" }
func (e *mockNetError) Timeout() bool   { return e.timeout }
func (e *mockNetError) Temporary() bool { return false }

func TestIsTemporary_Timeout(t *testing.T) {
	if !isTemporary(&mockNetError{timeout: true}) {
		t.Error("expected true for timeout net.Error")
	}
}

func TestIsTemporary_NotTimeout(t *testing.T) {
	if isTemporary(&mockNetError{timeout: false}) {
		t.Error("expected false for non-timeout net.Error")
	}
}

func TestIsTemporary_NonNetError(t *testing.T) {
	if isTemporary(io.EOF) {
		t.Error("expected false for non-net.Error")
	}
}

func TestIsConnClosed_Nil(t *testing.T) {
	if isConnClosed(nil) {
		t.Error("expected false for nil error")
	}
}

func TestIsConnClosed_ErrClosed(t *testing.T) {
	if !isConnClosed(net.ErrClosed) {
		t.Error("expected true for net.ErrClosed")
	}
}

func TestIsConnClosed_ErrClosedPipe(t *testing.T) {
	if !isConnClosed(io.ErrClosedPipe) {
		t.Error("expected true for io.ErrClosedPipe")
	}
}

func TestIsConnClosed_OtherError(t *testing.T) {
	if isConnClosed(io.EOF) {
		t.Error("expected false for io.EOF")
	}
}

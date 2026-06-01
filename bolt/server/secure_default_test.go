package server

import (
	"errors"
	"testing"
)

// TestNewServer_FailsClosedWithoutAuth verifies the secure-by-default
// contract: constructing a server with a nil Auth handler must fail closed
// with ErrNoAuthHandler rather than silently installing an accept-everyone
// handler. This is the first acceptance criterion — a careless embedder
// writing NewServer(eng, Options{}) must not ship an open server.
func TestNewServer_FailsClosedWithoutAuth(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(newInProcEngine(), Options{})
	if !errors.Is(err, ErrNoAuthHandler) {
		t.Fatalf("NewServer(Options{}): got err %v, want ErrNoAuthHandler", err)
	}
	if srv != nil {
		t.Fatalf("NewServer must return a nil *Server when it fails closed, got %#v", srv)
	}
}

// TestNewServer_ExplicitNoAuthHandlerAdmitted verifies the explicit
// development opt-in: setting Auth to a NoAuthHandler{} value succeeds and the
// open-door handler is preserved, keeping the dev/test path alive. The
// explicit value is itself the opt-in. The loud warning is emitted by the
// constructor in this branch; this test asserts the resolved handler rather
// than the log line.
func TestNewServer_ExplicitNoAuthHandlerAdmitted(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(newInProcEngine(), Options{Auth: NoAuthHandler{}})
	if err != nil {
		t.Fatalf("NewServer(Options{Auth: NoAuthHandler{}}): unexpected error %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer must return a non-nil *Server when Auth is NoAuthHandler{}")
	}
	if _, ok := srv.opts.Auth.(NoAuthHandler); !ok {
		t.Fatalf("explicit NoAuthHandler{} must be preserved, got %T", srv.opts.Auth)
	}
}

// TestNewServer_RealAuthHandlerUnchanged verifies that supplying a real
// AuthHandler is unchanged by the secure-by-default rule: the handler is used
// as-is and no error is returned.
func TestNewServer_RealAuthHandlerUnchanged(t *testing.T) {
	t.Parallel()

	want := BasicAuthHandler{
		Validate: ConstantTimeValidate("alice", "correct-horse-battery-staple"),
	}

	srv, err := NewServer(newInProcEngine(), Options{Auth: want})
	if err != nil {
		t.Fatalf("NewServer with real Auth: unexpected error %v", err)
	}
	if _, ok := srv.opts.Auth.(BasicAuthHandler); !ok {
		t.Fatalf("real Auth handler must be preserved, got %T", srv.opts.Auth)
	}
}

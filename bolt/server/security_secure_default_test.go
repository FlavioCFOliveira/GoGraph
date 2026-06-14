package server

// security_secure_default_test.go — DEFENSE LOCK-IN for the secure-by-default
// constructor contract (security audit, supply/config cluster).
//
// secure_default_test.go already pins that NewServer(Options{}) fails closed
// with ErrNoAuthHandler and that an explicit NoAuthHandler{} is admitted, but it
// deliberately asserts the resolved handler rather than the log line. This file
// closes the remaining gap: the loud warning that fires when an open-door
// handler is installed must actually be emitted, and it must name the
// authentication-disabled condition so an operator grepping logs cannot miss
// it. A future refactor that drops the warning (silently shipping an open
// server) is then caught.
//
// Layer: short. The warning is captured through an injected slog handler
// (Options.Logger), so the test opens no sockets and spawns no goroutines.

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// secBoltCaptureLogger returns a *slog.Logger that records every record into
// buf, together with a function that returns the accumulated text. The handler
// records at LevelWarn and above so a stray Info/Debug line cannot pollute the
// assertion.
func secBoltCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestSec_Bolt_NoAuthHandlerEmitsLoudWarning asserts that constructing a server
// with an explicit NoAuthHandler{} emits a WARN-level log naming the
// no-authentication condition.
func TestSec_Bolt_NoAuthHandlerEmitsLoudWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	srv, err := NewServer(newInProcEngine(), Options{
		Auth:   NoAuthHandler{},
		Logger: secBoltCaptureLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewServer(NoAuthHandler{}): unexpected error %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer must return a non-nil server for an explicit NoAuthHandler{}")
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("expected a WARN-level log on NoAuthHandler{}, got: %q", logged)
	}
	// The warning must name the disabled-authentication condition. Match on
	// stable, human-meaningful tokens rather than the exact sentence so a
	// reword that preserves the meaning does not break the test, while dropping
	// the warning entirely does.
	for _, want := range []string{"bolt:", "no authentication"} {
		if !strings.Contains(logged, want) {
			t.Errorf("warning %q does not contain expected token %q", logged, want)
		}
	}
}

// TestSec_Bolt_RealAuthHandlerNoWarning is the converse safety pin: a server
// built with a real AuthHandler must NOT emit the no-authentication warning, so
// the signal stays meaningful (it fires only when the door is actually open).
func TestSec_Bolt_RealAuthHandlerNoWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	_, err := NewServer(newInProcEngine(), Options{
		Auth:   BasicAuthHandler{Validate: ConstantTimeValidate("alice", "correct-horse-battery-staple")},
		Logger: secBoltCaptureLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewServer with real Auth: unexpected error %v", err)
	}

	if strings.Contains(buf.String(), "no authentication") {
		t.Errorf("real AuthHandler must not emit the no-authentication warning, got: %q", buf.String())
	}
}

// TestSec_Bolt_NilAuthFailsClosedNoServer reaffirms the fail-closed boundary in
// this security file so the secure-default invariant is self-contained: a nil
// Auth yields ErrNoAuthHandler and never a usable server, even when a logger is
// supplied (a careless embedder cannot trade the error for a warning).
func TestSec_Bolt_NilAuthFailsClosedNoServer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	srv, err := NewServer(newInProcEngine(), Options{Logger: secBoltCaptureLogger(&buf)})
	if !errors.Is(err, ErrNoAuthHandler) {
		t.Fatalf("NewServer(nil Auth): got err %v, want ErrNoAuthHandler", err)
	}
	if srv != nil {
		t.Fatalf("NewServer must return nil *Server when it fails closed, got %#v", srv)
	}
}

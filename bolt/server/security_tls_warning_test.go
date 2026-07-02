package server

// security_tls_warning_test.go — DEFENSE LOCK-IN for the plaintext-transport
// startup warning (security assessment 2026-07-02, finding #1848).
//
// A default-constructed server with a nil Options.TLSConfig serves plain TCP,
// so Bolt LOGON credentials travel in cleartext. That is a deliberate default
// for an embeddable engine, but it must be a conscious choice: NewServer emits a
// loud WARN, mirroring the NoAuthHandler warning, so a silent plaintext
// exposure is caught. This file pins both halves — the warning fires with no
// TLS, and stays silent once a TLSConfig is supplied — so the signal remains
// meaningful.
//
// Layer: short. The warning is captured through an injected slog handler; no
// socket is opened.

import (
	"bytes"
	"strings"
	"testing"
)

// tlsWarnFragment is a stable, human-meaningful token of the plaintext warning.
const tlsWarnFragment = "no TLS"

// TestSec_Bolt_NoTLSEmitsWarning asserts that a server built with a real auth
// handler but no TLSConfig still emits a WARN naming the plaintext condition.
// A real auth handler is used so the NoAuthHandler warning cannot supply the
// token by accident.
func TestSec_Bolt_NoTLSEmitsWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	srv, err := NewServer(newInProcEngine(), Options{
		Auth:   BasicAuthHandler{Validate: ConstantTimeValidate("alice", "correct-horse-battery-staple")},
		Logger: secBoltCaptureLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewServer(no TLS): unexpected error %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer must return a non-nil server when only TLS is absent")
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("expected a WARN-level log on nil TLSConfig, got: %q", logged)
	}
	for _, want := range []string{"bolt:", tlsWarnFragment} {
		if !strings.Contains(logged, want) {
			t.Errorf("warning %q does not contain expected token %q", logged, want)
		}
	}
}

// TestSec_Bolt_TLSConfigNoWarning is the converse pin: once a TLSConfig is
// supplied the plaintext warning must NOT fire, so the signal stays meaningful.
func TestSec_Bolt_TLSConfigNoWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	_, err := NewServer(newInProcEngine(), Options{
		Auth:      BasicAuthHandler{Validate: ConstantTimeValidate("alice", "correct-horse-battery-staple")},
		TLSConfig: DefaultTLSConfig(),
		Logger:    secBoltCaptureLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewServer(with TLS): unexpected error %v", err)
	}
	if strings.Contains(buf.String(), tlsWarnFragment) {
		t.Errorf("a server with a TLSConfig must not emit the plaintext warning, got: %q", buf.String())
	}
}

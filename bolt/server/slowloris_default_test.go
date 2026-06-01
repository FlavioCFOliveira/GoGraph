package server

import (
	"testing"
	"time"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newInProcEngine builds a minimal Cypher engine for the in-package option
// tests. It is kept separate from the external test helper of the same intent
// because the resolved-default assertions must read the unexported
// Server.opts, which is only reachable from within package server.
func newInProcEngine() *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// TestNewServer_DefaultsConnTimeout verifies that NewServer fills a non-zero
// ConnTimeout when the embedder leaves it at zero. This is the second
// acceptance criterion: a default server must enforce an idle deadline so a
// connection that completes the handshake but then stalls cannot hold its slot
// and goroutine forever. The test inspects the resolved option directly.
func TestNewServer_DefaultsConnTimeout(t *testing.T) {
	srv := NewServer(newInProcEngine(), Options{})
	if srv.opts.ConnTimeout != DefaultConnTimeout {
		t.Fatalf("ConnTimeout: got %v, want default %v", srv.opts.ConnTimeout, DefaultConnTimeout)
	}
	if srv.opts.ConnTimeout <= 0 {
		t.Fatalf("ConnTimeout must be non-zero by default, got %v", srv.opts.ConnTimeout)
	}
}

// TestHandshakeTimeoutDefaults verifies that the unauthenticated handshake is
// always bounded: the exported DefaultHandshakeTimeout const is non-zero and
// the package-level handshakeTimeout var (applied in handleConn) is seeded from
// it. The handshake bound is intentionally a fixed package value rather than a
// configurable Options field, so the Options struct stays small; this test is
// the in-package guard that the bound is never zero by default.
func TestHandshakeTimeoutDefaults(t *testing.T) {
	if DefaultHandshakeTimeout <= 0 {
		t.Fatalf("DefaultHandshakeTimeout must be non-zero, got %v", DefaultHandshakeTimeout)
	}
	if DefaultConnTimeout <= 0 {
		t.Fatalf("DefaultConnTimeout must be non-zero, got %v", DefaultConnTimeout)
	}
	if got := time.Duration(handshakeTimeout.Load()); got != DefaultHandshakeTimeout {
		t.Fatalf("handshakeTimeout: got %v, want seed %v", got, DefaultHandshakeTimeout)
	}
	// The handshake bound is deliberately shorter than the post-handshake idle
	// bound: a legitimate client sends its 20-byte handshake immediately, so a
	// stalled handshake should be reclaimed sooner than an idle session.
	if DefaultHandshakeTimeout >= DefaultConnTimeout {
		t.Errorf("DefaultHandshakeTimeout (%v) should be shorter than DefaultConnTimeout (%v)",
			DefaultHandshakeTimeout, DefaultConnTimeout)
	}
}

// TestNewServer_RespectsExplicitTimeouts verifies the ConnTimeout default is
// overridable: an embedder that sets an explicit positive value keeps it
// untouched.
func TestNewServer_RespectsExplicitTimeouts(t *testing.T) {
	const connTO = 7 * time.Second
	srv := NewServer(newInProcEngine(), Options{
		ConnTimeout: connTO,
	})
	if srv.opts.ConnTimeout != connTO {
		t.Errorf("ConnTimeout: got %v, want %v (explicit value must be preserved)", srv.opts.ConnTimeout, connTO)
	}
}

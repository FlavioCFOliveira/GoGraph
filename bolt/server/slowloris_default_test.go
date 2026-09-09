package server

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newInProcEngine builds a minimal Cypher engine for the in-package option
// tests. It is kept separate from the external test helper of the same intent
// because the resolved-default assertions must read the unexported
// Server.opts, which is only reachable from within package server.
func newInProcEngine() *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// TestNewServer_LeavesConnTimeoutDisabled verifies that NewServer does NOT fill
// ConnTimeout when the embedder leaves it at zero.
//
// It used to assert the opposite. Until rmp #2807 a default server armed a 30 s
// idle read deadline so a connection that completed the handshake and then
// stalled could not hold its slot and goroutine for ever. That deadline is gone,
// because it also ran while the message loop executed the client's own statement
// and closed busy connections; TCP keep-alive reclaims a connection whose PEER
// has vanished, and Options.ConnTimeout is what an operator sets to reclaim one
// that is merely silent. [DefaultConnTimeout] carries the full reasoning and the
// cost.
//
// The unauthenticated version-negotiation handshake is still bounded
// unconditionally — TestHandshakeTimeoutIsAlwaysArmed below — so what changed is
// the post-handshake message loop, not the pre-handshake one.
func TestNewServer_LeavesConnTimeoutDisabled(t *testing.T) {
	srv, err := NewServer(newInProcEngine(), Options{Auth: NoAuthHandler{}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.opts.ConnTimeout != 0 {
		t.Fatalf("ConnTimeout: got %v, want 0 (rmp #2807 disabled the idle read deadline by default)", srv.opts.ConnTimeout)
	}
}

// TestHandshakeTimeoutIsAlwaysArmed verifies that the unauthenticated
// version-negotiation handshake is bounded unconditionally: the exported
// DefaultHandshakeTimeout const is non-zero and the package-level
// handshakeTimeout var (applied in handleConn) is seeded from it. The handshake
// bound is intentionally a fixed package value rather than a configurable
// Options field, so the Options struct stays small; this test is the in-package
// guard that the bound is never zero.
//
// It used to also assert DefaultHandshakeTimeout < DefaultConnTimeout — the
// handshake bound being the tighter of two pre-authentication deadlines.
// rmp #2807 set DefaultConnTimeout to 0, so there is no second bound to be
// shorter than, and this is now the ONLY pre-authentication deadline a
// default-configured server applies. It covers version negotiation only, not the
// window between a completed handshake and a successful LOGON; see
// [DefaultConnTimeout] for that exposure.
func TestHandshakeTimeoutIsAlwaysArmed(t *testing.T) {
	if DefaultHandshakeTimeout <= 0 {
		t.Fatalf("DefaultHandshakeTimeout must be non-zero, got %v", DefaultHandshakeTimeout)
	}
	if got := time.Duration(handshakeTimeout.Load()); got != DefaultHandshakeTimeout {
		t.Fatalf("handshakeTimeout: got %v, want seed %v", got, DefaultHandshakeTimeout)
	}
}

// TestNewServer_RespectsExplicitTimeouts verifies the ConnTimeout default is
// overridable: an embedder that sets an explicit positive value keeps it
// untouched.
func TestNewServer_RespectsExplicitTimeouts(t *testing.T) {
	const connTO = 7 * time.Second
	srv, err := NewServer(newInProcEngine(), Options{
		ConnTimeout: connTO,
		Auth:        NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.opts.ConnTimeout != connTO {
		t.Errorf("ConnTimeout: got %v, want %v (explicit value must be preserved)", srv.opts.ConnTimeout, connTO)
	}
}

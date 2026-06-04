package server

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newEngineWithRowCap builds a minimal in-process Cypher engine whose result-row
// cap is set from the supplied EngineOptions.MaxResultRows value. It mirrors
// newInProcEngine but threads the cap through so the in-package warning tests can
// distinguish a capped engine from an explicitly-uncapped one.
func newEngineWithRowCap(maxRows int64) *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: maxRows})
}

// rowCapWarnFragment is a stable substring of the uncapped-engine warning. The
// tests match on it rather than the whole line so the human-readable wording can
// evolve without breaking the assertion, while still pinning the warning to its
// subject (the missing result-row cap).
//
//nolint:gosec // G101 false positive: this is a fixed log-message substring used in assertions, not a credential.
const rowCapWarnFragment = "no result-row cap"

// captureWarnLogger returns a slog.Logger that writes to buf, so a test can
// assert on the emitted warning text. Text handler output is line-oriented and
// includes the level and message verbatim.
func captureWarnLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestNewServer_WarnsOnUncappedEngine is the acceptance test for the second
// deliverable of task #1293: NewServer must emit a loud warning when it is handed
// an engine that was built with cypher.MaxResultRowsUnlimited (no result-row
// cap), because such an engine lets a single client query materialise an
// unbounded result set inside the visibility barrier and exhaust server memory
// before the first RECORD is chunked out. The server cannot retrofit a bound onto
// a pre-built engine, so the warning is the only safety surface available.
func TestNewServer_WarnsOnUncappedEngine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	eng := newEngineWithRowCap(cypher.MaxResultRowsUnlimited)
	if got := eng.ResultRowCap(); got != 0 {
		t.Fatalf("precondition: uncapped engine ResultRowCap() = %d, want 0 (unlimited)", got)
	}

	srv, err := NewServer(eng, Options{
		Auth:   NoAuthHandler{},
		Logger: captureWarnLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewServer with uncapped engine: unexpected error %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer must return a non-nil *Server for an uncapped engine")
	}

	if !strings.Contains(buf.String(), rowCapWarnFragment) {
		t.Errorf("NewServer with uncapped engine: expected a warning containing %q, got log output:\n%s",
			rowCapWarnFragment, buf.String())
	}
	// The warning must be emitted at WARN level (the capture handler is gated at
	// LevelWarn, so any captured line is at least WARN; assert the level token is
	// present to pin it explicitly).
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("uncapped-engine warning must be at WARN level, got:\n%s", buf.String())
	}
}

// TestNewServer_NoWarnOnCappedEngine is the negative half of the acceptance
// criterion: an engine built with a finite result-row cap (here the explicit
// small value 100) must NOT trigger the uncapped-engine warning. This guards
// against a warning that fires unconditionally and trains operators to ignore it.
func TestNewServer_NoWarnOnCappedEngine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	eng := newEngineWithRowCap(100)
	if got := eng.ResultRowCap(); got != 100 {
		t.Fatalf("precondition: capped engine ResultRowCap() = %d, want 100", got)
	}

	srv, err := NewServer(eng, Options{
		Auth:   NoAuthHandler{},
		Logger: captureWarnLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewServer with capped engine: unexpected error %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer must return a non-nil *Server for a capped engine")
	}

	if strings.Contains(buf.String(), rowCapWarnFragment) {
		t.Errorf("NewServer with a finite cap must NOT emit the uncapped-engine warning, got:\n%s", buf.String())
	}
}

// TestNewServer_DefaultEngineIsCapped documents that the engine built by the
// default constructor (cypher.NewEngine, used by every other server test and by
// the common embedder) carries the finite DefaultMaxResultRows and therefore does
// not trip the warning. This keeps the default, out-of-the-box server quiet while
// still bounded — the uncapped warning is reserved for a deliberate opt-out.
func TestNewServer_DefaultEngineIsCapped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	eng := newInProcEngine() // cypher.NewEngine — zero-value options
	if got := eng.ResultRowCap(); got != cypher.DefaultMaxResultRows {
		t.Fatalf("default engine ResultRowCap() = %d, want DefaultMaxResultRows (%d)", got, cypher.DefaultMaxResultRows)
	}

	if _, err := NewServer(eng, Options{Auth: NoAuthHandler{}, Logger: captureWarnLogger(&buf)}); err != nil {
		t.Fatalf("NewServer with default engine: %v", err)
	}
	if strings.Contains(buf.String(), rowCapWarnFragment) {
		t.Errorf("default (finitely-capped) engine must not emit the uncapped-engine warning, got:\n%s", buf.String())
	}
}

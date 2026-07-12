package cypher_test

// constraint_recovered_warn_test.go — regression for #1918: opening a store that
// has durable constraints with the plain NewEngineWithStore constructor (which
// does not re-register them) must warn, because the constraints are silently NOT
// enforced. Uses the WAL harness helpers from constraint_durability_test.go.

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestNewEngineWithStore_WarnsOnUnregisteredRecoveredConstraints(t *testing.T) {
	dir := t.TempDir()

	// Cycle 1: declare a UNIQUE constraint so it is durable in the WAL.
	if err := cdCycle(t, dir, false, `CREATE CONSTRAINT u FOR (n:User) REQUIRE n.email IS UNIQUE`); err != nil {
		t.Fatalf("declare constraint: %v", err)
	}

	// Reopen; recovery re-surfaces the durable constraint and seeds
	// Graph.HasConstraints via AddStoreConstraint.
	res, err := recovery.Open[string, float64](dir, cdRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if len(res.Constraints) == 0 {
		t.Fatal("expected recovered constraints")
	}
	if !res.Graph.HasConstraints() {
		t.Fatal("recovered graph should report HasConstraints")
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, cdStoreOpts())

	// Capture warnings emitted at engine construction.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	// The trap constructor: builds the engine but re-registers no constraints.
	_ = cypher.NewEngineWithStore(store)

	if !strings.Contains(buf.String(), "constraint enforcement is DISABLED") {
		t.Fatalf("expected an unregistered-recovered-constraints warning, got: %q", buf.String())
	}
}

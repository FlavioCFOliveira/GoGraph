package cypher_test

// constraint_recovered_warn_test.go — regression for #1918 / #1981: opening a
// store that has durable constraints with the plain NewEngineWithStore
// constructor now AUTO-REGISTERS them (with synthesised names) so UNIQUE / NOT
// NULL are enforced rather than silently dropped, and logs a warning. Uses the
// WAL harness helpers from constraint_durability_test.go.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestNewEngineWithStore_AutoEnforcesRecoveredConstraints(t *testing.T) {
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

	// The plain constructor now auto-registers the recovered constraints.
	eng := cypher.NewEngineWithStore(store)

	if !strings.Contains(buf.String(), "auto-registering them for enforcement") {
		t.Fatalf("expected an auto-registration warning, got: %q", buf.String())
	}

	// Enforcement must be ACTIVE: a duplicate email is rejected even though the
	// caller did not thread the recovered constraints explicitly (#1981).
	ctx := context.Background()
	if err := drainWrite(eng.RunInTx(ctx, `CREATE (:User {email:'a@b.com'})`, nil)); err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}
	if err := drainWrite(eng.RunInTx(ctx, `CREATE (:User {email:'a@b.com'})`, nil)); err == nil {
		t.Fatal("BYPASS: duplicate email accepted — recovered UNIQUE constraint not enforced by NewEngineWithStore")
	}
}

// drainWrite drains a write Result and returns the error surfaced by Run or the
// lazy stream (Volcano evaluation defers errors to the first pull).
func drainWrite(r *cypher.Result, e error) error {
	if e != nil {
		return e
	}
	if r == nil {
		return nil
	}
	for r.Next() { // drain to surface the lazy error / commit
	}
	err := r.Err()
	r.Close()
	return err
}

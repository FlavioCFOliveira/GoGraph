package server_test

// driver_compat_db_test.go — rmp #2172.
//
// The server never sent the `db` field in result metadata. The official
// neo4j-go-driver builds its ResultSummary from the SUCCESS that terminates the
// stream, and ResultSummary.Database() returns a NIL DatabaseInfo when `db` is
// absent (resultsummary.go:499 — `if database == "" { return nil }`), so the
// idiomatic
//
//	summary.Database().Name()
//
// panicked with a nil pointer dereference INSIDE the driver. That is worse than
// an error: the caller cannot defend against it without knowing the field is
// missing.
//
// These tests drive the real driver against a real server over a real socket, so
// they fail if the field is dropped anywhere along the metadata path rather than
// only if the map literal changes.
//
// Layer: short.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// newNamedDriverForTest starts a server whose Options.DatabaseName is dbName
// (empty selects the server default) and connects the official driver to it.
func newNamedDriverForTest(t *testing.T, dbName string) neo4j.DriverWithContext {
	t.Helper()
	addr := startTestServer(t, server.Options{
		ConnTimeout:  10 * time.Second,
		DatabaseName: dbName,
	})
	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 5
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() {
		if cerr := driver.Close(context.Background()); cerr != nil {
			t.Logf("driver.Close: %v", cerr)
		}
	})
	return driver
}

// databaseNameFromSummary reads summary.Database().Name() the way an application
// would — with no nil guard — and converts the panic that used to produce into a
// test failure that names it, so the regression is unmistakable.
func databaseNameFromSummary(t *testing.T, summary neo4j.ResultSummary) (name string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("summary.Database().Name() panicked (%v); the server sent no `db` field, "+
				"so the driver returned a nil DatabaseInfo", r)
		}
	}()
	return summary.Database().Name()
}

// TestDriverCompat_DatabaseNameOnAutocommit covers the autocommit path: RUN then
// PULL, with the summary built from the terminal PULL SUCCESS.
func TestDriverCompat_DatabaseNameOnAutocommit(t *testing.T) {
	t.Parallel()
	driver := newNamedDriverForTest(t, "")
	ctx := context.Background()
	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = sess.Close(ctx) }()

	res, err := sess.Run(ctx, "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := databaseNameFromSummary(t, summary); got != server.DefaultDatabaseName {
		t.Fatalf("Database().Name() = %q, want %q", got, server.DefaultDatabaseName)
	}
}

// TestDriverCompat_DatabaseNameInExplicitTransaction covers the explicit-
// transaction path, where the driver sends `db` on BEGIN and not on the
// subsequent RUNs — so the server must carry the selection across the whole
// transaction rather than reading it per statement.
func TestDriverCompat_DatabaseNameInExplicitTransaction(t *testing.T) {
	t.Parallel()
	driver := newNamedDriverForTest(t, "")
	ctx := context.Background()
	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = sess.Close(ctx) }()

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	res, err := tx.Run(ctx, "RETURN 2 AS n", nil)
	if err != nil {
		t.Fatalf("tx.Run: %v", err)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := databaseNameFromSummary(t, summary); got != server.DefaultDatabaseName {
		t.Fatalf("Database().Name() = %q, want %q", got, server.DefaultDatabaseName)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestDriverCompat_DatabaseNameEchoesClientSelection proves the name the client
// asked for comes back, so its own bookkeeping stays consistent. GoGraph serves
// one graph per server, so the name selects nothing — but reporting a DIFFERENT
// name than the client requested would be actively misleading.
func TestDriverCompat_DatabaseNameEchoesClientSelection(t *testing.T) {
	t.Parallel()
	driver := newNamedDriverForTest(t, "")
	ctx := context.Background()

	const requested = "movies"
	sess := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: requested})
	defer func() { _ = sess.Close(ctx) }()

	res, err := sess.Run(ctx, "RETURN 3 AS n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := databaseNameFromSummary(t, summary); got != requested {
		t.Fatalf("Database().Name() = %q, want the requested %q", got, requested)
	}
}

// TestDriverCompat_DatabaseNameHonoursServerOption proves Options.DatabaseName
// is what an unselecting client is told, so an operator serving a differently
// named graph is not forced to report "neo4j".
func TestDriverCompat_DatabaseNameHonoursServerOption(t *testing.T) {
	t.Parallel()
	const configured = "gograph"
	driver := newNamedDriverForTest(t, configured)
	ctx := context.Background()
	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = sess.Close(ctx) }()

	res, err := sess.Run(ctx, "RETURN 4 AS n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := databaseNameFromSummary(t, summary); got != configured {
		t.Fatalf("Database().Name() = %q, want the configured %q", got, configured)
	}
}

// TestDriverCompat_DatabaseNameAfterExplicitTransactionEnds proves the
// transaction-scoped selection does not leak: an autocommit statement issued on
// the same session after a transaction that named a database must report the
// server's name again, not the transaction's.
func TestDriverCompat_DatabaseNameAfterExplicitTransactionEnds(t *testing.T) {
	t.Parallel()
	driver := newNamedDriverForTest(t, "")
	ctx := context.Background()

	// A session bound to a named database, then a second session bound to none,
	// over the same pooled connections.
	named := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: "scoped"})
	tx, err := named.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	res, err := tx.Run(ctx, "RETURN 5 AS n", nil)
	if err != nil {
		t.Fatalf("tx.Run: %v", err)
	}
	if _, err := res.Consume(ctx); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := named.Close(ctx); err != nil {
		t.Fatalf("named session Close: %v", err)
	}

	plain := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = plain.Close(ctx) }()
	res2, err := plain.Run(ctx, "RETURN 6 AS n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	summary, err := res2.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := databaseNameFromSummary(t, summary); got != server.DefaultDatabaseName {
		t.Fatalf("Database().Name() = %q after a named transaction on a reused connection, want %q — "+
			"the transaction's selection leaked", got, server.DefaultDatabaseName)
	}
}

package ldbc

// ic_helpers_test.go — shared helpers for LDBC Interactive Complex (IC)
// Bolt end-to-end tests (T719, T725, T729).
//
// Each IC test starts a fresh in-memory Bolt server, seeds the canonical
// small social network through the neo4j-go-driver (ExecuteWrite), and then
// exercises the query pipeline via the same driver.
//
// Seeding is done over Bolt (ExecuteWrite) rather than via eng.RunInTx. Both
// approaches go through the full Cypher pipeline. The MATCH+CREATE queries
// for KNOWS relationships use the WHERE-clause form (WHERE a.id=X AND b.id=Y)
// because inline integer property filters in a comma-joined MATCH pattern
// (MATCH (a:P {id:1}),(b:P {id:2})) are silently no-op in the current engine
// when both patterns carry integer filters.

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// socialGraph is a small deterministic social network used by all IC tests.
//
// Nodes (Person):
//
//	id=1  Alice Smith
//	id=2  Bob   Jones
//	id=3  Alice Brown
//	id=4  Carol Davis
//	id=5  Dave  Wilson
//
// Edges (KNOWS, single direction per pair — undirected MATCH patterns
// traverse both forward and reverse CSR sides; storing each pair once is
// the Neo4j-canonical model):
//
//	1→2, 1→4, 2→3, 2→5, 4→5
//
// NOTE: KNOWS edges use the WHERE-clause form (MATCH (a:P),(b:P) WHERE a.id=X
// AND b.id=Y) rather than inline property filters ({id:X}). Inline integer
// property filters in a comma-joined MATCH pattern are silently ignored by the
// current engine — the WHERE form is the supported workaround.
var socialGraphSeedQueries = []string{
	`CREATE (n:Person {id: 1, firstName: 'Alice', lastName: 'Smith'})`,
	`CREATE (n:Person {id: 2, firstName: 'Bob',   lastName: 'Jones'})`,
	`CREATE (n:Person {id: 3, firstName: 'Alice', lastName: 'Brown'})`,
	`CREATE (n:Person {id: 4, firstName: 'Carol', lastName: 'Davis'})`,
	`CREATE (n:Person {id: 5, firstName: 'Dave',  lastName: 'Wilson'})`,
	// KNOWS relationships — stored once per pair. Undirected MATCH patterns
	// `-[:KNOWS]-` traverse both forward and reverse CSR sides correctly
	// since 51feab7 (cypher/exec/expand.go reverseEdgePassesFilter), so the
	// earlier two-direction workaround is no longer needed and would emit
	// duplicate bindings under undirected single-relationship patterns.
	`MATCH (a:Person),(b:Person) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:KNOWS]->(b)`,
	`MATCH (a:Person),(b:Person) WHERE a.id = 1 AND b.id = 4 CREATE (a)-[:KNOWS]->(b)`,
	`MATCH (a:Person),(b:Person) WHERE a.id = 2 AND b.id = 3 CREATE (a)-[:KNOWS]->(b)`,
	`MATCH (a:Person),(b:Person) WHERE a.id = 2 AND b.id = 5 CREATE (a)-[:KNOWS]->(b)`,
	`MATCH (a:Person),(b:Person) WHERE a.id = 4 AND b.id = 5 CREATE (a)-[:KNOWS]->(b)`,
}

// startICServer starts a fresh empty Bolt server on a random loopback port
// and seeds the social graph through the driver. It returns the address and
// a connected driver ready for use.
//
// Goroutine-leak checking is registered as the FIRST t.Cleanup (runs last,
// after server shutdown) so all server goroutines are gone when goleak fires.
func startICServer(t *testing.T) (addr string, driver neo4j.DriverWithContext) {
	t.Helper()

	// Register goleak as the first cleanup — it runs last in LIFO order,
	// after the server and driver have been shut down.
	t.Cleanup(func() {
		goleak.VerifyNone(t)
	})

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	srv := server.NewServer(eng, server.Options{
		MaxConnections: 16,
		ConnTimeout:    15 * time.Second,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startICServer listen: %v", err)
	}
	addr = ln.Addr().String()

	srvCtx, srvCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(srvCtx, ln)
	}()

	// Allow the server to enter Accept before the driver connects.
	time.Sleep(10 * time.Millisecond)

	driver, err = neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 4
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		srvCancel()
		t.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}

	// Register server and driver teardown (LIFO — driver closes first, then server).
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx) //nolint:errcheck // shutdown errors not actionable in teardown
		srvCancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("startICServer: Serve goroutine did not exit in cleanup")
		}
	})
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Logf("startICServer: driver.Close: %v", err)
		}
	})

	// Seed the social graph through the Bolt driver (one write tx per query).
	seedSocialGraphViaDriver(t, driver)

	return addr, driver
}

// seedSocialGraphViaDriver executes all seed queries through ExecuteWrite so
// that the full Cypher pipeline (parse → plan → execute → commit) handles
// MATCH+CREATE relationship creation correctly.
func seedSocialGraphViaDriver(t *testing.T, driver neo4j.DriverWithContext) {
	t.Helper()
	ctx := context.Background()
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	for _, q := range socialGraphSeedQueries {
		q := q // capture
		_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			result, err := tx.Run(ctx, q, nil)
			if err != nil {
				return nil, err
			}
			_, err = result.Consume(ctx)
			return nil, err
		})
		if err != nil {
			t.Fatalf("seedSocialGraphViaDriver %q: %v", q, err)
		}
	}
}

// runICQuery executes a Cypher read query via ExecuteRead and returns each
// record as a map[string]any keyed by column alias. If the query returns an
// error the caller must handle it (typically t.Skipf for unsupported features).
func runICQuery(ctx context.Context, session neo4j.SessionWithContext, query string) ([]map[string]any, error) {
	rows, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, nil)
		if err != nil {
			return nil, err
		}
		records, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, len(records))
		for i, rec := range records {
			out[i] = rec.AsMap()
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return rows.([]map[string]any), nil
}

// requireInt64 asserts that v is an int64 and returns it, fatally failing the
// test if the assertion fails.
func requireInt64(t *testing.T, field string, v any) int64 {
	t.Helper()
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("field %q: expected int64, got %T (%v)", field, v, v)
	}
	return n
}

// requireString asserts that v is a string and returns it, fatally failing
// the test if the assertion fails.
func requireString(t *testing.T, field string, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field %q: expected string, got %T (%v)", field, v, v)
	}
	return s
}

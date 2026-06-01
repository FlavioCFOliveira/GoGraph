// Example 23_bolt_server starts a GoGraph Bolt v5 server backed by an
// in-memory labelled property graph, then drives a full client round-trip
// against it with the official neo4j-go-driver/v5.
//
// The example seeds a handful of :Person nodes through the engine, brings
// the server up on a kernel-assigned 127.0.0.1:0 port, connects a real Bolt
// client, runs a Cypher MATCH query over a session, and prints the rows the
// server returns. It then closes the client, shuts the server down
// gracefully, and drains the serve goroutine so no goroutine leaks — the
// same teardown discipline as bolt/server/example_test.go.
//
// Sample output: run `go run ./examples/23_bolt_server` and capture the
// stdout. The query result (the ordered list of names) is deterministic and
// serves as the regression baseline a future change should preserve; the
// listener address line varies per run because the port is OS-assigned.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"time"

	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// people are the names seeded into the graph before the server starts.
// The MATCH query returns them ordered, so the printed result is stable.
var people = []string{"Alice", "Bob", "Carol"}

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run seeds an in-memory graph, starts a Bolt v5 server on an ephemeral
// port, connects a neo4j-go-driver client, runs a MATCH query, prints the
// returned names to w, and tears everything down cleanly. All output goes to
// w so a test can capture and assert it; run returns wrapped errors rather
// than terminating the process.
func run(w io.Writer) error {
	ctx := context.Background()

	// Engine over an in-memory labelled property graph.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Seed a few :Person nodes so the client's query returns rows.
	if err := seed(ctx, eng); err != nil {
		return fmt.Errorf("seed graph: %w", err)
	}

	// Bolt v5 server with a per-connection idle timeout. The server is
	// secure-by-default, so the explicit NoAuthHandler{} value is the opt-in
	// that lets this development example run without credentials; a production
	// deployment would instead set Options.Auth to a real AuthHandler.
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: 64,
		ConnTimeout:    5 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	// Kernel-assigned port; ln.Addr() reveals the chosen port for the client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()

	fmt.Fprintf(w, "GoGraph Bolt v5 server listening on %s\n", addr)
	fmt.Fprintln(w, "Compatible with neo4j-go-driver v5 and cypher-shell.")

	// Serve in the background under a cancellable context so Serve exits
	// cleanly once the client disconnects and the context is cancelled.
	serveCtx, serveCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	// Run the client round-trip. queryNames connects, queries, prints, and
	// closes its own driver and session before returning.
	queryErr := queryNames(ctx, w, addr)

	// Graceful shutdown with a deadline, then cancel Serve and drain its
	// goroutine. Serve only returns after every connection goroutine has
	// finished, so the drain guarantees no leaked goroutine.
	shutCtx, shutCancel := context.WithTimeout(ctx, 5*time.Second)
	shutErr := srv.Shutdown(shutCtx)
	shutCancel()
	serveCancel()
	serveDrain := <-serveErr

	// Surface the first meaningful failure, preferring the client round-trip.
	if queryErr != nil {
		return queryErr
	}
	if shutErr != nil {
		return fmt.Errorf("shutdown: %w", shutErr)
	}
	if serveDrain != nil {
		return fmt.Errorf("serve: %w", serveDrain)
	}

	fmt.Fprintln(w, "Server shut down cleanly.")
	return nil
}

// seed creates one :Person node per name in people via an in-process Cypher
// write transaction, so the served graph is populated before any client
// connects.
func seed(ctx context.Context, eng *cypher.Engine) error {
	for _, name := range people {
		res, err := eng.RunInTxAny(ctx,
			`CREATE (n:Person {name: $name})`,
			map[string]any{"name": name},
		)
		if err != nil {
			return fmt.Errorf("CREATE Person %q: %w", name, err)
		}
		for res.Next() {
			// Drain: CREATE returns no rows, but the cursor must be exhausted.
		}
		if err := res.Err(); err != nil {
			_ = res.Close()
			return fmt.Errorf("CREATE Person %q: %w", name, err)
		}
		if err := res.Close(); err != nil {
			return fmt.Errorf("CREATE Person %q close: %w", name, err)
		}
	}
	return nil
}

// queryNames connects a neo4j-go-driver client to the server at addr, runs an
// ordered MATCH over a read session, prints each returned name to w, and
// closes the session and driver before returning. The query result is
// deterministic; only the surrounding network timing is not.
func queryNames(ctx context.Context, w io.Writer, addr string) error {
	driver, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth())
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}
	defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown

	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx) //nolint:errcheck // best-effort close on teardown

	result, err := sess.Run(ctx,
		`MATCH (n:Person) RETURN n.name AS name ORDER BY name`, nil)
	if err != nil {
		return fmt.Errorf("run query: %w", err)
	}
	records, err := result.Collect(ctx)
	if err != nil {
		return fmt.Errorf("collect rows: %w", err)
	}

	// ORDER BY makes the rows ordered on the wire; sort defensively so the
	// printed output is stable regardless of any server-side reordering.
	names := make([]string, 0, len(records))
	for _, rec := range records {
		v, _ := rec.Get("name")
		name, ok := v.(string)
		if !ok {
			return fmt.Errorf("column 'name': expected string, got %T", v)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(w, "Client query: MATCH (n:Person) RETURN n.name AS name ORDER BY name\n")
	fmt.Fprintf(w, "Returned %d rows:\n", len(names))
	for _, name := range names {
		fmt.Fprintf(w, "  name = %s\n", name)
	}
	return nil
}

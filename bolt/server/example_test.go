package server_test

// example_test.go — runnable godoc example for the Bolt server (#1119).
// It starts a server on an ephemeral port, drives one session through the
// neo4j-go-driver, and shuts everything down cleanly so no goroutine leaks.

import (
	"context"
	"fmt"
	"net"
	"time"

	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// ExampleServer_Serve starts a Bolt server backed by an in-memory graph,
// connects a Bolt client, and runs a query over the session. The listener
// binds to 127.0.0.1:0 so the OS assigns a free port. Teardown closes the
// client first, then cancels Serve and waits for it to drain every connection
// goroutine — leaving no leaked goroutine behind.
//
// The network round-trip is non-deterministic in timing, so the example
// asserts the deterministic query result rather than any wire-level output.
func ExampleServer_Serve() {
	// Engine over an empty in-memory labelled property graph.
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// The explicit NoAuthHandler{} value is the opt-in that lets this example
	// run without credentials; the server is secure-by-default and otherwise
	// refuses to start with a nil Auth handler.
	srv, err := server.NewServer(eng, server.Options{ConnTimeout: 5 * time.Second, Auth: server.NoAuthHandler{}})
	if err != nil {
		fmt.Println("new server:", err)
		return
	}

	// Ephemeral port; ln.Addr() reveals the chosen port for the client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()

	// Connect a Bolt client and run a trivial read query.
	driver, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth())
	if err != nil {
		fmt.Println("driver:", err)
		cancel()
		<-serveErr
		return
	}

	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	result, err := sess.Run(ctx, "RETURN 1 AS n", nil)
	if err != nil {
		fmt.Println("run:", err)
	} else if rec, err := result.Single(ctx); err != nil {
		fmt.Println("single:", err)
	} else {
		n, _ := rec.Get("n")
		fmt.Println("n =", n)
	}
	_ = sess.Close(ctx)

	// Clean shutdown: close the client so server-side connection goroutines
	// observe EOF, then cancel Serve and wait for it to return. Serve only
	// returns after every connection goroutine has finished.
	_ = driver.Close(ctx)
	cancel()
	<-serveErr
	// Output:
	// n = 1
}

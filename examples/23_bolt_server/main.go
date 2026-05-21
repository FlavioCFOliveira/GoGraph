// Example 23_bolt_server demonstrates starting a GoGraph Bolt v5 server,
// accepting a connection on a random port, and shutting down gracefully.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

func main() {
	// Build graph and engine.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Create the Bolt v5 server.
	srv := server.NewServer(eng, server.Options{MaxConnections: 64})

	// Listen on a kernel-assigned port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	fmt.Printf("GoGraph Bolt v5 server listening on %s\n", ln.Addr())
	fmt.Println("Compatible with neo4j-go-driver v5 and cypher-shell.")
	fmt.Println("Accepting connections for 100ms...")

	// Serve in the background; use a cancellable context so Serve exits cleanly
	// when Shutdown is called.
	serveCtx, serveCancel := context.WithCancel(context.Background())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(serveCtx, ln)
	}()

	// Let the server run for 100 ms.
	time.Sleep(100 * time.Millisecond)

	// Graceful shutdown with a 5-second deadline. Cancel the serve context
	// explicitly after shutdown so the goroutine always terminates.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutErr := srv.Shutdown(shutCtx)
	shutCancel()
	serveCancel()

	if shutErr != nil {
		// Drain the serve goroutine before exiting to avoid leaks.
		<-serveErr
		log.Printf("shutdown: %v", shutErr)
		os.Exit(1)
	}

	// Drain the serve goroutine; Serve returns nil on clean context cancellation.
	if serveErrVal := <-serveErr; serveErrVal != nil {
		log.Printf("serve: %v", serveErrVal)
		os.Exit(1)
	}

	fmt.Println("Server shut down cleanly.")
}

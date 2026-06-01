package server_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// sharedServerAddr is the address of the package-level test server started in
// TestMain. All TestBoltSmokeTest_* tests that do not require special server
// configuration (e.g. TLS) connect to this shared server so that only one TCP
// listener is created for the entire smoke suite, avoiding macOS ephemeral
// port exhaustion when many tests run concurrently.
var sharedServerAddr string

// TestMain starts a single shared server for the test binary, runs all tests,
// and shuts the server down cleanly before checking for goroutine leaks.
//
// Tests that require a dedicated server configuration (e.g. TLS, MaxConnections)
// create their own server via startTestServer and are responsible for its cleanup.
func TestMain(m *testing.M) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	srv, err := server.NewServer(eng, server.Options{ConnTimeout: 5 * time.Second, Auth: server.NoAuthHandler{}})
	if err != nil {
		panic("TestMain: new server: " + err.Error())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("TestMain: listen: " + err.Error())
	}
	sharedServerAddr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, ln)
	}()

	// Give the server a moment to enter Accept before tests start.
	time.Sleep(10 * time.Millisecond)

	// Run all tests.
	code := m.Run()

	// Shut down the shared server and drain the Serve goroutine before
	// goleak checks for leaks. cancel() triggers listener close, which
	// unblocks Accept and allows Serve to return.
	cancel()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		// Best effort; do not block exit indefinitely on a stuck server.
	}

	// Check for goroutine leaks after all servers have been shut down.
	if leakErr := goleak.Find(); leakErr != nil {
		os.Stderr.WriteString("goleak: " + leakErr.Error() + "\n") //nolint:errcheck
		code = 1
	}

	os.Exit(code)
}

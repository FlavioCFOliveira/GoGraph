package sim

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// SimServer runs a real [github.com/FlavioCFOliveira/GoGraph/bolt/server.Server]
// over an in-memory [SimListener]. It exists so the Phase-3 actors drive the
// GENUINE Bolt wire path — handshake, framing, message loop, streaming — with no
// OS socket and no reimplementation of the server. New client connections are
// obtained with [SimServer.Dial].
//
// The server is started with [server.NoAuthHandler] (development/testing mode):
// the DST harness asserts robustness and ACID under abuse, not credential
// handling, which has its own dedicated test battery in bolt/server. A finite
// result-row cap is configured on the engine so a single overload query cannot
// materialise an unbounded result set.
//
// # Concurrency contract
//
// SimServer is safe for concurrent use: [SimServer.Dial] may be called from many
// goroutines (the concurrent harness opens one connection per goroutine) while
// the embedded server's accept loop runs. [SimServer.Close] is idempotent and
// drains the server before returning.
//
//nolint:revive // "Sim" prefix is the DST harness naming scheme (see SimDisk).
type SimServer struct {
	srv      *server.Server
	ln       *SimListener
	cancel   context.CancelFunc
	serveErr chan error
	clk      clock.Clock

	closeOnce sync.Once
	closeErr  error
}

// defaultSimResultRowCap bounds the rows a single query result materialises in
// the engine backing a [SimServer]. It must be finite so [server.NewServer] does
// not warn about an unbounded engine and so the OverloadActor's large reads hit
// a typed cap rather than exhausting memory.
const defaultSimResultRowCap = 100_000

// NewSimServer builds a SimServer over the given engine and starts it serving on
// an in-memory listener whose connection deadlines route through clk. The engine
// must be non-nil; callers typically pass an engine with a finite result-row cap
// (see [SimEngineForServer]). The returned server is already accepting; obtain
// connections with [SimServer.Dial] and tear it down with [SimServer.Close].
func NewSimServer(eng *cypher.Engine, clk clock.Clock) (*SimServer, error) {
	if eng == nil {
		return nil, fmt.Errorf("sim: NewSimServer: nil engine")
	}
	srv, err := server.NewServer(eng, server.Options{
		Auth:        server.NoAuthHandler{},
		ConnTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("sim: NewSimServer: %w", err)
	}
	ln := NewSimListener(clk)
	ctx, cancel := context.WithCancel(context.Background())
	s := &SimServer{
		srv:      srv,
		ln:       ln,
		cancel:   cancel,
		serveErr: make(chan error, 1),
		clk:      clk,
	}
	go func() { s.serveErr <- srv.Serve(ctx, ln) }()
	return s, nil
}

// SimEngineForServer builds a fresh in-memory directed-multigraph engine with a
// finite result-row cap, suitable for backing a [SimServer]. The multigraph
// model matches openCypher's additive-CREATE relationship semantics that the
// Bolt e2e path expects.
//
//nolint:revive // "Sim" prefix is the DST harness naming scheme (see SimDisk, SimStore, SimConn); the apparent stutter is intentional and consistent across the package.
func SimEngineForServer() *cypher.Engine {
	return cypher.NewEngineWithOptions(
		newSimServerGraph(),
		cypher.EngineOptions{MaxResultRows: defaultSimResultRowCap},
	)
}

// Dial opens a new client connection to the server over the in-memory listener,
// returning a [WireClient] ready to negotiate. The caller must Close the client
// when done. It returns an error only if the listener is closed.
func (s *SimServer) Dial() (*WireClient, error) {
	conn, err := s.ln.Dial()
	if err != nil {
		return nil, err
	}
	return NewWireClient(conn, s.clk), nil
}

// DialConn opens a new client connection and returns the raw [SimConn], for
// callers (notably the BoltAbuser) that need to write malformed bytes the
// [WireClient] would never produce.
func (s *SimServer) DialConn() (*SimConn, error) { return s.ln.Dial() }

// Close stops accepting new connections, cancels the serve context, and waits
// for the server to drain. It is idempotent and returns the server's exit error
// (nil on a clean shutdown).
func (s *SimServer) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.ln.Close()
		select {
		case err := <-s.serveErr:
			s.closeErr = err
		case <-time.After(10 * time.Second):
			s.closeErr = fmt.Errorf("sim: SimServer.Close: serve goroutine did not exit")
		}
	})
	return s.closeErr
}

// newSimServerGraph builds the directed multigraph the SimServer engine runs on,
// matching the additive-CREATE relationship model the Bolt e2e path expects.
func newSimServerGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

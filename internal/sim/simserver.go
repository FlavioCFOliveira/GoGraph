package sim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
// [NewSimServer] starts the server with [server.NoAuthHandler]
// (development/testing mode), which is what the robustness and ACID scenarios
// want: they abuse the wire and the transaction machinery, not the credential
// check. The credential surface itself is driven by [NewSimServerAuth], which
// takes an arbitrary [server.AuthHandler] so the auth scenarios can present a
// server that genuinely REFUSES a wrong password (rmp #2481) — against a
// NoAuthHandler a bad-credential probe would pass by admitting everything, which
// proves nothing. A finite result-row cap is configured on the engine so a single
// overload query cannot materialise an unbounded result set.
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
	clk       clock.Clock
	closeErr  error
	srv       *server.Server
	ln        *SimListener
	cancel    context.CancelFunc
	serveErr  chan error
	closeOnce sync.Once
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
//
// Authentication is [server.NoAuthHandler]; use [NewSimServerAuth] to drive a
// server that validates credentials.
func NewSimServer(eng *cypher.Engine, clk clock.Clock) (*SimServer, error) {
	return newSimServer(eng, clk, simServerOptions{})
}

// NewSimServerAuth builds a SimServer whose sessions authenticate through auth
// instead of [server.NoAuthHandler], so a scenario can drive the credential
// surface over the genuine wire: a wrong password, an unknown scheme, LOGOFF
// followed by a write, and re-authentication.
//
// A nil auth is REFUSED rather than defaulted. The internal constructor treats a
// nil handler as NoAuthHandler (which is what every pre-existing scenario wants),
// but silently doing that here would hand a credential-validating scenario a
// server that admits everything — and every refusal it then failed to observe
// would look like a passing test. A caller that wants NoAuth asks for it by name
// with [NewSimServer].
//
// The server's own log is discarded, because every rejected credential is
// reported at ERROR level by design and an auth scenario provokes dozens of them
// on purpose (see [quietSimLogger]).
func NewSimServerAuth(eng *cypher.Engine, clk clock.Clock, auth server.AuthHandler) (*SimServer, error) {
	if auth == nil {
		return nil, fmt.Errorf("sim: NewSimServerAuth: nil auth handler (use NewSimServer for an unauthenticated server)")
	}
	return newSimServer(eng, clk, simServerOptions{
		auth:      auth,
		log:       quietSimLogger(),
		maxTxIdle: simAuthMaxTxIdle,
	})
}

// simAuthMaxTxIdle is the idle bound [NewSimServerAuth] installs. See
// [simServerOptions.maxTxIdle] for why an auth scenario lifts it well clear of
// its own runtime instead of racing the default 5 s.
const simAuthMaxTxIdle = 10 * time.Minute

// Server exposes the embedded [server.Server] so a scenario can drive its
// operator API — [server.Server.Transactions] and
// [server.Server.TerminateTransaction], both of which take the registry's own lock
// and are safe on a serving server. It is the accessor rmp #2482 needs.
//
// The returned server is owned by the SimServer and must not be Shutdown by the
// caller ([SimServer.Close] does that).
//
// # Do NOT call SetClock on it
//
// [server.Server.SetClock] writes s.clk AND replaces s.txReg, and the accept path
// reads both unguarded (bolt/server/serve.go: sess.setClock(s.clk) and
// sess.setTxRegistry(s.txReg, remote)). This constructor has already started the
// serve goroutine by the time it returns, so injecting a clock through this
// accessor is a data race the detector will report. A scenario that needs a fake
// clock must have it installed BEFORE Serve starts, i.e. from inside the
// constructor.
func (s *SimServer) Server() *server.Server { return s.srv }

// quietSimLogger returns a logger that discards everything. The durable-commit /
// checkpoint scenarios back a SimServer with an engine built by OpenSimStore,
// which — unlike [SimEngineForServer] — carries no result-row cap, so
// [server.NewServer] logs a loud (correct, informational) warning per
// construction, plus the standing NoAuth and no-TLS warnings. Across the
// multi-iteration DST tests that is a wall of noise on stderr that obscures a
// real failure; the durable scenarios pass this logger so the wiring is
// otherwise byte-identical to [NewSimServer] but quiet.
func quietSimLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// simServerOptions carries the parts of [server.Options] a SimServer scenario is
// allowed to vary. The zero value reproduces the historical wiring exactly: a
// [server.NoAuthHandler] and a nil logger (so [server.NewServer] falls back to
// slog.Default). Everything else — the connection timeout, the listener, the
// serve goroutine — is fixed by the harness.
type simServerOptions struct {
	// auth overrides the session auth handler. Nil means [server.NoAuthHandler].
	auth server.AuthHandler
	// log routes the server's events (including the unbounded-engine / NoAuth /
	// no-TLS warnings) somewhere other than slog.Default. Nil keeps the default.
	log *slog.Logger
	// maxTxIdle overrides [server.Options.MaxTxIdleTime]. Zero keeps
	// [server.DefaultMaxTxIdleTime] (5 s of REAL time, since a SimServer runs on
	// clock.Real).
	//
	// A scenario that holds an explicit transaction open across several round trips
	// while pinning an exact failure code needs this: if a scheduling stall lets the
	// idle reaper fire mid-arm, the client is answered
	// Neo.ClientError.Transaction.TransactionTimedOut and the arm reports a
	// violation of something it was not testing. The reaper itself is the subject of
	// rmp #2482, not of an auth arm, so an auth scenario lifts the bound rather than
	// racing it.
	maxTxIdle time.Duration
}

// newSimServerWithLogger is the logger-only entry point kept for the durable and
// checkpoint scenarios, which want the standard NoAuth wiring but a quiet log.
func newSimServerWithLogger(eng *cypher.Engine, clk clock.Clock, log *slog.Logger) (*SimServer, error) {
	return newSimServer(eng, clk, simServerOptions{log: log})
}

// newSimServer is the single constructor behind every SimServer entry point.
func newSimServer(eng *cypher.Engine, clk clock.Clock, opts simServerOptions) (*SimServer, error) {
	if eng == nil {
		return nil, fmt.Errorf("sim: NewSimServer: nil engine")
	}
	auth := opts.auth
	if auth == nil {
		auth = server.NoAuthHandler{}
	}
	srv, err := server.NewServer(eng, server.Options{
		Auth:          auth,
		ConnTimeout:   30 * time.Second,
		Logger:        opts.log,
		MaxTxIdleTime: opts.maxTxIdle,
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

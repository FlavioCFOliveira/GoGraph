package sim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	return newSimServer(eng, clk, &simServerOptions{})
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
	return newSimServer(eng, clk, &simServerOptions{
		auth:      auth,
		log:       quietSimLogger(),
		maxTxIdle: simAuthMaxTxIdle,
	})
}

// simAuthMaxTxIdle is the idle bound [NewSimServerAuth] installs. See
// [simServerOptions.maxTxIdle] for why an auth scenario lifts it well clear of
// its own runtime instead of racing the default 5 s.
const simAuthMaxTxIdle = 10 * time.Minute

// NewSimServerTxRegistry builds a SimServer wired for the transaction-registry and
// idle-reaper surface of rmp #2482: the server's own clock is serverClk, so the
// message loop's timeout timer and the registry's StartedAt/Elapsed run on VIRTUAL
// time, while the in-memory listener keeps listenerClk. maxTxIdle,
// defaultTxTimeout and maxOpenTxPerPrincipal are passed straight through to
// [server.Options]; a zero value in any of them takes that option's own documented
// default, so this constructor can also stand in for [NewSimServer] with only a
// clock changed. Authentication is [server.NoAuthHandler] and the server log is
// discarded ([quietSimLogger]), because the reaper reports every reap at WARN and a
// reaper arm provokes them deliberately.
//
// # Why the two clocks MUST be different objects
//
// The listener clock is not a spare copy of the server clock: it is the clock every
// SimConn's blocked I/O uses, and pointing it at the same [clock.Fake] breaks the
// instrument the reaper arm depends on. Both halves of this are verified in the
// code, not assumed:
//
//   - EVERY server-side read has a deadline. The reader goroutine calls
//     conn.SetReadDeadline(time.Now().Add(ConnTimeout)) before every single read
//     (bolt/server/serve.go:1109), and a SimServer sets ConnTimeout to 30 s, so the
//     branch is always taken.
//   - A deadline-bearing blocked read arms a timer ON THE LISTENER CLOCK.
//     halfPipe.waitDeadline does timer := h.clk.NewTimer(h.clk.Until(d)) and spawns
//     a goroutine to broadcast on it (internal/sim/simconn.go:143), where h.clk is
//     the clock handed to [NewSimListener] and shared by both ends of every
//     [SimConn]. On a shared fake, every connection parked waiting for its next
//     request therefore registers a timer, and a NewTimer-counting decorator like
//     txClockProbe can no longer attribute its count to the transaction reaper —
//     which is the whole point of counting.
//   - The deadline instant is WALL-CLOCK. It comes from time.Now(), not from the
//     injected clock, so on a shared fake the comparison h.clk.Now().Before(d) is
//     between virtual and real time. A fake started at time.Now() would then time
//     the connection out as soon as the arm advanced past ConnTimeout — reaping the
//     socket instead of the transaction; a fake started at the Unix epoch would
//     instead put the deadline ~56 years of virtual time away, so the arm's whole
//     advance budget is silently inert against it. Neither is production behaviour.
//   - Nothing on the server side needs a fake listener. All three of the server's
//     socket deadlines are real-time by construction — the handshake
//     conn.SetDeadline (bolt/server/serve.go:965), the per-read deadline (:1109) and
//     the per-write deadline (:1394) all read time.Now() — so leaving listenerClk on
//     [clock.Real] leaves connection liveness behaving exactly as it does in
//     production while the transaction machinery runs on virtual time.
//
// The clock the returned SimServer hands to each [WireClient] from
// [SimServer.Dial] is listenerClk, matching the connection it is built on.
func NewSimServerTxRegistry(
	eng *cypher.Engine,
	listenerClk, serverClk clock.Clock,
	maxTxIdle, defaultTxTimeout time.Duration,
	maxOpenTxPerPrincipal int,
) (*SimServer, error) {
	return newSimServer(eng, listenerClk, &simServerOptions{
		log:                   quietSimLogger(),
		clk:                   serverClk,
		maxTxIdle:             maxTxIdle,
		defaultTxTimeout:      defaultTxTimeout,
		maxOpenTxPerPrincipal: maxOpenTxPerPrincipal,
	})
}

// NewSimServerOwnedCloser builds a SimServer that OWNS a store-level teardown
// closer: closer is installed as [server.Options.Closer], so the embedded server
// closes it itself once it has drained every connection — the documented
// "drain the connections, then close the DB" ordering that store/db.go says a
// Bolt server provides (store/db.go:54-57). It is the constructor rmp #2483
// needs, and nothing in the module passed Options.Closer outside
// bolt/server's own tests before it.
//
// wrapConn, when non-nil, decorates every connection the server's accept loop
// receives. It is the only observable in the harness that can time the closer
// against the connection drain: the per-connection handler's FIRST deferred call
// is conn.Close (bolt/server/serve.go:1063 and the outer defer at :904), which
// runs strictly BEFORE the accept-loop wrapper's s.wg.Done (:798), so a decorator
// counting Close calls sees a connection leave before the WaitGroup the drain
// waits on can drop to zero. Counting accepts and closes therefore yields a
// one-sided oracle: a closer entered while a decorated connection is still open
// is a genuine drain-ordering breach, and the nanosecond window between a
// connection's Close and its wg.Done can only read as drained, never as a false
// breach.
//
// Authentication is [server.NoAuthHandler] and the server log is discarded
// ([quietSimLogger]), because a store-backed engine carries no result-row cap and
// [server.NewServer] warns about that on every construction.
func NewSimServerOwnedCloser(
	eng *cypher.Engine,
	clk clock.Clock,
	closer io.Closer,
	wrapConn func(net.Conn) net.Conn,
) (*SimServer, error) {
	if closer == nil {
		return nil, fmt.Errorf("sim: NewSimServerOwnedCloser: nil closer (use NewSimServer for a server that owns no teardown)")
	}
	return newSimServer(eng, clk, &simServerOptions{
		log:      quietSimLogger(),
		closer:   closer,
		wrapConn: wrapConn,
	})
}

// NewSimServerInFlight builds a SimServer whose per-connection in-flight cursor
// cap is maxInFlight instead of [server.DefaultMaxInFlightPerConnection], so a
// scenario can drive the cap to its refusal over the genuine wire rather than by
// reaching into a [server.Session]. It is the constructor rmp #2484 needs; before
// it, nothing in the harness passed Options.MaxInFlightPerConnection at all, so
// the only cap the DST could ever have reached was 1024 cursors deep inside one
// transaction.
//
// A non-positive maxInFlight is REFUSED rather than defaulted. The server option
// treats zero as "take the default", so silently passing it through would hand a
// cap-driving scenario a cap of 1024 — and the refusal it then failed to observe
// would read as a passing test rather than as an unreached bound.
//
// Authentication is [server.NoAuthHandler] and the server log is discarded
// ([quietSimLogger]), because a store-backed engine carries no result-row cap and
// [server.NewServer] warns about that on every construction.
func NewSimServerInFlight(eng *cypher.Engine, clk clock.Clock, maxInFlight int) (*SimServer, error) {
	if maxInFlight <= 0 {
		return nil, fmt.Errorf("sim: NewSimServerInFlight: maxInFlight must be positive, got %d "+
			"(zero would take the server's own 1024 default and leave the cap unreachable)", maxInFlight)
	}
	return newSimServer(eng, clk, &simServerOptions{
		log:         quietSimLogger(),
		maxInFlight: maxInFlight,
	})
}

// NewSimServerInboundBudget builds a SimServer whose ENGINE-WIDE inbound-decode
// ceiling is maxInboundDecodeBytes: one [packstream.InboundBudget] pool shared by
// every connection the server accepts.
//
// That sharing is the whole point. The per-message decoded-collection cap
// (packstream's maxDecodedCollectionBytes, 128 MiB) bounds a SINGLE message; the
// pool this sets bounds the SUM in flight across the fleet, which is the CWE-770
// vector the per-message cap cannot see. The server creates the pool once, in
// [server.NewServer] (bolt/server/serve.go:654), and hands the same pointer to
// every connection's reassembly reader and pooled decoder.
//
// A non-positive value is REFUSED rather than defaulted. Zero means "derive a
// default" to the server option and -1 means "unlimited"
// ([server.MaxInboundDecodeBytesUnlimited]), so silently passing either through
// would hand a pressure-driving scenario a ceiling of 1 GiB or none at all — and
// the rejection it then failed to observe would read as a passing test rather
// than as an unreached bound. This mirrors [NewSimServerInFlight]'s refusal for
// the same reason.
//
// Authentication is [server.NoAuthHandler] and the server log is discarded
// ([quietSimLogger]): a scenario that provokes the ceiling makes the server log
// "inbound decode memory budget exceeded" at WARN by design (serve.go:1264), so
// the noise is expected output rather than a signal.
func NewSimServerInboundBudget(eng *cypher.Engine, clk clock.Clock, maxInboundDecodeBytes int64) (*SimServer, error) {
	if maxInboundDecodeBytes <= 0 {
		return nil, fmt.Errorf("sim: NewSimServerInboundBudget: maxInboundDecodeBytes must be positive, got %d "+
			"(zero takes the server's 1 GiB-scale default and -1 disables the ceiling; either leaves it unreachable)",
			maxInboundDecodeBytes)
	}
	return newSimServer(eng, clk, &simServerOptions{
		log:                   quietSimLogger(),
		maxInboundDecodeBytes: maxInboundDecodeBytes,
	})
}

// Shutdown stops the embedded server through [server.Server.Shutdown]: it stops
// accepting, drains every active connection, and — when the SimServer was built
// by [NewSimServerOwnedCloser] — closes the owned store-level closer on its
// drain-success branch only. It returns Shutdown's own error verbatim, including
// the drain-timeout error and a context expiry, so a scenario can adjudicate
// WHICH branch was taken.
//
// It does NOT join the serve goroutine: on the two failure branches Shutdown
// leaves a still-blocked [server.Server.Serve] waiting on the same drain, and
// that goroutine's own exit path is what eventually performs the post-drain close
// (bolt/server/serve.go:725-738). Call [SimServer.Close] afterwards to join it.
//
// Shutdown may be called more than once; the second call observes the same cached
// close result from the server's own sync.Once.
func (s *SimServer) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Server exposes the embedded [server.Server] so a scenario can drive its
// operator API — [server.Server.Transactions] and
// [server.Server.TerminateTransaction], both of which take the registry's own lock
// and are safe on a serving server. It is the accessor rmp #2482 needs.
//
// The returned server is owned by the SimServer: tear it down with
// [SimServer.Shutdown] (the graceful drain) or [SimServer.Close] (cancel the
// serve context and join), never by calling Shutdown on the value returned here.
// An earlier version of this godoc said [SimServer.Close] called Shutdown; it
// never did — Close cancels the serve context and closes the listener, which is
// the ctx-cancellation stop path, not the drain path (rmp #2483).
//
// # Do NOT call SetClock on it
//
// [server.Server.SetClock] writes s.clk AND replaces s.txReg, and the accept path
// reads both unguarded (bolt/server/serve.go: sess.setClock(s.clk) and
// sess.setTxRegistry(s.txReg, remote)). This constructor has already started the
// serve goroutine by the time it returns, so injecting a clock through this
// accessor is a data race the detector will report. A scenario that needs a fake
// clock must have it installed BEFORE Serve starts, i.e. from inside the
// constructor — which is what [NewSimServerTxRegistry] does.
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
// allowed to vary. It is passed to [newSimServer] by POINTER: it is over
// gocritic's hugeParam threshold, and nothing mutates it. The zero value reproduces the historical wiring exactly: a
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

	// clk is the SERVER-side clock: the one [server.Server.SetClock] installs, which
	// drives the message loop's transaction-timeout timer (bolt/server/serve.go
	// syncTxTimer) and the transaction registry's StartedAt/Elapsed
	// (bolt/server/txregistry.go register/list). Nil keeps [clock.Real], which is
	// what every pre-existing SimServer scenario wants.
	//
	// It is NOT the listener clock. See [NewSimServerTxRegistry] for why the two
	// must be different objects.
	clk clock.Clock

	// defaultTxTimeout overrides [server.Options.DefaultTxTimeout], the TOTAL
	// lifetime bound handleBegin applies when the client sends no tx_timeout. Zero
	// keeps [server.DefaultTxTimeout] (30 s).
	//
	// A scenario driving the IDLE reaper must set it ABOVE maxTxIdle: the serve
	// loop's effectiveTxDeadline takes the EARLIER of the total and idle deadlines,
	// so leaving it at 30 s while asking for an idle bound above that would arm the
	// timer for the total bound and the reap would not be an idle reap at all.
	defaultTxTimeout time.Duration

	// maxOpenTxPerPrincipal overrides [server.Options.MaxOpenTxPerPrincipal]. Zero
	// keeps [server.DefaultMaxOpenTxPerPrincipal] (2048); a NEGATIVE value disables
	// enforcement, exactly as the server option documents.
	maxOpenTxPerPrincipal int

	// closer is installed as [server.Options.Closer]: the store-level teardown
	// owner the server closes after its connection drain completes. Nil leaves the
	// server owning nothing beyond its connections, which is what every
	// pre-#2483 scenario wants. See [NewSimServerOwnedCloser].
	closer io.Closer

	// wrapConn decorates each connection handed to the server's accept loop. Nil
	// hands the raw [SimConn] through, byte-for-byte the historical wiring. See
	// [NewSimServerOwnedCloser] for why the decoration is the drain-ordering
	// observable.
	wrapConn func(net.Conn) net.Conn

	// maxInFlight overrides [server.Options.MaxInFlightPerConnection], the cap on
	// how many result cursors one explicit transaction may accumulate before it
	// must COMMIT or ROLLBACK. Zero keeps
	// [server.DefaultMaxInFlightPerConnection] (1024).
	//
	// A scenario that wants to OBSERVE the cap refusing a RUN must lower it: the
	// default would need 1024 RUN+PULL round trips inside one transaction to
	// reach, which is neither a short-layer budget nor a legible report. See
	// [NewSimServerInFlight].
	maxInFlight int

	// maxInboundDecodeBytes overrides [server.Options.MaxInboundDecodeBytes], the
	// ENGINE-WIDE (per-Server) ceiling on inbound decode memory in flight across
	// every connection at once. Zero keeps the server's own resolution, which
	// derives a ceiling from GOMEMLIMIT or falls back to
	// [server.DefaultMaxInboundDecodeBytes] (1 GiB);
	// [server.MaxInboundDecodeBytesUnlimited] (-1) opts out entirely.
	//
	// A scenario that wants to OBSERVE the aggregate ceiling refusing a decode must
	// lower it drastically. The default is 1 GiB and the largest message a client
	// may send is 16 MiB, so provoking it at the default would need dozens of
	// concurrent maximal messages — neither a short-layer budget nor a legible
	// report. See [NewSimServerInboundBudget].
	maxInboundDecodeBytes int64
}

// simServeListener decorates a [SimListener] so every accepted connection passes
// through wrap before the server sees it. Close and Addr are the embedded
// listener's, so [server.Server.Shutdown] closing s.ln closes the real listener.
type simServeListener struct {
	*SimListener
	wrap func(net.Conn) net.Conn
}

// Accept implements [net.Listener.Accept], returning the decorated connection.
func (l *simServeListener) Accept() (net.Conn, error) {
	c, err := l.SimListener.Accept()
	if err != nil {
		return nil, err
	}
	return l.wrap(c), nil
}

// newSimServerWithLogger is the logger-only entry point kept for the durable and
// checkpoint scenarios, which want the standard NoAuth wiring but a quiet log.
func newSimServerWithLogger(eng *cypher.Engine, clk clock.Clock, log *slog.Logger) (*SimServer, error) {
	return newSimServer(eng, clk, &simServerOptions{log: log})
}

// newSimServer is the single constructor behind every SimServer entry point.
func newSimServer(eng *cypher.Engine, clk clock.Clock, opts *simServerOptions) (*SimServer, error) {
	if eng == nil {
		return nil, fmt.Errorf("sim: NewSimServer: nil engine")
	}
	auth := opts.auth
	if auth == nil {
		auth = server.NoAuthHandler{}
	}
	srv, err := server.NewServer(eng, server.Options{
		Auth:                     auth,
		ConnTimeout:              30 * time.Second,
		Logger:                   opts.log,
		MaxTxIdleTime:            opts.maxTxIdle,
		DefaultTxTimeout:         opts.defaultTxTimeout,
		MaxOpenTxPerPrincipal:    opts.maxOpenTxPerPrincipal,
		Closer:                   opts.closer,
		MaxInFlightPerConnection: opts.maxInFlight,
		MaxInboundDecodeBytes:    opts.maxInboundDecodeBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("sim: NewSimServer: %w", err)
	}
	// The server clock is installed HERE — after construction and STRICTLY BEFORE
	// the serve goroutine below — because [server.Server.SetClock] writes s.clk and
	// REPLACES s.txReg (bolt/server/serve.go setClock), and the accept path reads
	// both with no synchronisation at all: sess.setClock(s.clk) and
	// sess.setTxRegistry(s.txReg, remote) (bolt/server/serve.go:1013 and :1017, in
	// handleConn). [Server.Transactions] reads s.txReg unguarded too
	// (bolt/server/txregistry.go). Calling SetClock once no connection can yet be
	// accepted is what makes those reads race-free; doing it through
	// [SimServer.Server] after this constructor returns is a genuine data race the
	// detector reports.
	if opts.clk != nil {
		srv.SetClock(opts.clk)
	}
	ln := NewSimListener(clk)
	// The listener the SERVER sees may be decorated; the listener the harness
	// dials stays the SimListener itself, so [SimServer.Dial] is untouched.
	var serveLn net.Listener = ln
	if opts.wrapConn != nil {
		serveLn = &simServeListener{SimListener: ln, wrap: opts.wrapConn}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &SimServer{
		srv:      srv,
		ln:       ln,
		cancel:   cancel,
		serveErr: make(chan error, 1),
		clk:      clk,
	}
	go func() { s.serveErr <- srv.Serve(ctx, serveLn) }()
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

// ListenerAddr returns the address of the in-memory listener the embedded server
// serves on, as [net.Listener.Addr] renders it.
//
// It exists as the INDEPENDENT REFERENCE for the ROUTE payload arm of rmp #2485.
// The routing table a client receives is built from the address the accept loop
// copied off the listener — localAddr = s.ln.Addr().String() at
// bolt/server/serve.go:1000-1005, handed to newSession and read back by handleRoute
// as RoutingTable(s.localAddr) (bolt/server/session.go:1751). Reading the listener
// HERE therefore reaches that same source of truth by a different route than the
// reply does, so "the routing table names THIS server" is a comparison between two
// independently obtained values rather than a constant restated. A checker that
// instead compared the reply against
// [github.com/FlavioCFOliveira/GoGraph/bolt/server.RoutingTable]'s own output would
// be comparing that function with itself.
func (s *SimServer) ListenerAddr() string { return s.ln.Addr().String() }

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

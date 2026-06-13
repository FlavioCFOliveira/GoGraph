package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

const (
	// defaultMaxConnections is the default upper bound on concurrent connections.
	defaultMaxConnections = 1024

	// DefaultMaxInFlightPerConnection is the default value applied to
	// Options.MaxInFlightPerConnection when the caller leaves it at
	// zero. The count tracks all Result cursors appended to the
	// in-progress explicit transaction since BEGIN (both open and
	// already-drained), so it bounds the total number of RUN statements
	// a client may issue without committing. The default of 1024 allows
	// any legitimate workload while still bounding pathological
	// RUN-loop attacks that grow tx.results without bound. Operators
	// that need a stricter limit may lower this value explicitly.
	DefaultMaxInFlightPerConnection = 1024

	// shutdownDrainTimeout is the maximum time Shutdown waits for active
	// connections to finish after stopping the accept loop.
	shutdownDrainTimeout = 30 * time.Second

	// DefaultConnTimeout is the default value applied to Options.ConnTimeout
	// when the caller leaves it at zero. It is the per-message idle deadline
	// applied throughout the post-handshake message loop: the server resets it
	// before each read, so it bounds the time a connection may sit silent
	// between messages, not the total session duration. A non-zero default is
	// mandatory: with no deadline a client that completes the handshake but then
	// stops sending bytes would hold its connection slot and goroutine forever,
	// a Slowloris-style denial of service. The default of 30 s is generous
	// enough not to disturb a legitimate interactive session pausing between
	// queries while still reclaiming abandoned connections. Operators may set a
	// larger value for long-lived idle sessions or a smaller one to reclaim
	// connections more aggressively.
	DefaultConnTimeout = 30 * time.Second

	// DefaultTxTimeout is the default value applied to Options.DefaultTxTimeout
	// when the caller leaves it at zero. It bounds an explicit transaction
	// (opened by BEGIN) when the client supplies no tx_timeout of its own. A
	// finite default is mandatory: an explicit transaction holds the engine's
	// single-writer serialisation from BEGIN until COMMIT/ROLLBACK, so a client
	// that issues BEGIN and then stalls — never sending COMMIT, ROLLBACK, or even
	// RESET — would otherwise block every other writer on the server forever, a
	// liveness denial of service (#1302). The default of 30 s is generous enough
	// not to disturb a legitimate interactive transaction while still guaranteeing
	// the global write lock is reclaimed if a transaction is abandoned. Operators
	// may set a larger value for long-lived batch transactions or a smaller one
	// to reclaim the writer lock more aggressively; the per-statement
	// MaxStatementTimeout, when set, additionally clamps it.
	DefaultTxTimeout = 30 * time.Second

	// DefaultHandshakeTimeout is the deadline that bounds the unauthenticated
	// version-negotiation handshake — the cheapest phase for an attacker to
	// abuse, since it requires no valid protocol bytes (a client may open a
	// socket, send a single byte, and otherwise stall). The deadline is applied
	// to the connection before [proto.Negotiate] and cleared on success so it
	// never bleeds into normal operation. It is deliberately shorter than
	// DefaultConnTimeout: a legitimate client sends its 20-byte handshake
	// immediately, so 10 s is ample, while a stalled handshake is reclaimed
	// promptly. The handshake bound is fixed (not configurable via Options) to
	// keep the Options struct small; the package var handshakeTimeout is seeded
	// from this const and overridable only by tests.
	DefaultHandshakeTimeout = 10 * time.Second
)

// handshakeTimeout holds, in nanoseconds, the effective deadline applied to the
// unauthenticated handshake in handleConn. It is seeded from the exported
// [DefaultHandshakeTimeout] const and is overridable only from within the
// package (see export_test.go) so that tests can drive the Slowloris reclaim
// path quickly and deterministically. Production code never mutates it. It is
// an atomic because handleConn reads it from per-connection worker goroutines
// while a test may overwrite it on the main goroutine; the atomic keeps the
// hot-path read lock-free and the race detector clean.
var handshakeTimeout atomic.Int64

func init() { handshakeTimeout.Store(int64(DefaultHandshakeTimeout)) }

// Options configures a Server. It is a plain configuration value read once
// by [NewServer]; it is safe for concurrent read use once constructed, but
// must not be mutated after being passed to NewServer. The referenced
// TLSConfig, Auth, Logger, and Closer carry their own concurrency
// contracts.
type Options struct {
	// MaxConnections is the upper bound on concurrent accepted connections.
	// Zero or negative values default to 1024.
	MaxConnections int

	// MaxMessageBytes caps the cumulative payload size of a single Bolt
	// message reassembled from per-chunk fragments. Zero or negative
	// values default to [proto.DefaultMaxMessageBytes] (16 MiB).
	// Bolt's wire format limits each chunk to 65535 bytes but the
	// chunk count is unbounded; this cap closes the Slowloris-style
	// DoS vector in which a malicious client streams non-zero chunks
	// indefinitely until the server OOMs.
	MaxMessageBytes int

	// MaxInFlightPerConnection caps the total number of RUN statements
	// that may be issued within a single explicit transaction before
	// COMMIT or ROLLBACK. Zero or negative values default to
	// [DefaultMaxInFlightPerConnection] (1024). The count includes both
	// open (not yet fully PULL'd) and already-drained cursors
	// accumulated in tx.results since BEGIN; auto-commit cursors are
	// not counted (the Bolt v5 state machine already prevents two
	// concurrent auto-commit streams). The cap surfaces as a typed
	// Bolt FAILURE with code "Neo.ClientError.General.LimitExceeded".
	MaxInFlightPerConnection int

	// ConnTimeout is the per-connection idle read deadline applied throughout
	// the post-handshake message loop. Each time the server is about to read
	// the next message, the deadline is reset to now+ConnTimeout, so it bounds
	// the silent gap between messages rather than the total session duration.
	// Zero or negative values default to [DefaultConnTimeout] (30 s); a
	// non-zero deadline is always applied so an idle connection cannot hold its
	// slot and goroutine forever. Set a larger value for long-lived idle
	// sessions. The unauthenticated handshake phase is bounded separately and
	// is not configurable here; see [DefaultHandshakeTimeout].
	ConnTimeout time.Duration

	// MaxStatementTimeout is the server-side upper bound on per-statement
	// execution time. When a client supplies a timeout via the RUN or BEGIN
	// extra metadata, it is silently clamped to MaxStatementTimeout. When
	// a client supplies no timeout and MaxStatementTimeout is positive, the
	// server applies MaxStatementTimeout unconditionally. Zero means no
	// server-side cap (client controls its own timeout).
	MaxStatementTimeout time.Duration

	// DefaultTxTimeout is the bounded timeout applied to an explicit transaction
	// (opened by BEGIN) when the client supplies no tx_timeout. It guarantees the
	// engine's single-writer serialisation, which an explicit transaction holds
	// from BEGIN until COMMIT/ROLLBACK, can never be held indefinitely by an
	// abandoned transaction (#1302). Zero or negative values default to
	// [DefaultTxTimeout] (30 s). A client-supplied tx_timeout takes precedence;
	// MaxStatementTimeout, when set, additionally clamps the effective value. Set
	// a larger value for long-lived batch transactions.
	DefaultTxTimeout time.Duration

	// TLSConfig, when non-nil, wraps accepted connections with TLS using
	// the given configuration verbatim. nil means plain TCP (no TLS).
	//
	// The server applies no MinVersion or cipher policy of its own: whatever
	// config is supplied here is used as-is. To start from a hardened baseline
	// (TLS 1.2 floor, modern AEAD/ECDHE cipher list), begin with
	// [DefaultTLSConfig] and add your own Certificates or GetCertificate before
	// assigning it here.
	TLSConfig *tls.Config

	// Auth is the authentication handler invoked during HELLO/LOGON. It is
	// the security boundary of the server: every client must satisfy it
	// before any Cypher statement executes.
	//
	// Auth must be set; leave it nil and [NewServer] returns
	// [ErrNoAuthHandler]. The server is secure-by-default: a nil Auth is NOT
	// silently replaced with an open, accept-everyone handler, so a careless
	// embedder writing Options{} cannot accidentally expose an
	// unauthenticated server. To enforce credentials, set Auth to a real
	// [AuthHandler] such as [BasicAuthHandler]. To run without authentication
	// (development or testing only) set Auth: [NoAuthHandler]{} explicitly:
	// the explicit NoAuthHandler value is itself the opt-in, it is
	// self-documenting at the call site, and it is impossible to set by
	// accident. In that case [NewServer] still emits a loud warning.
	Auth AuthHandler

	// Logger is the structured logger for server events. When nil, the
	// default slog handler is used.
	Logger *slog.Logger

	// Closer, when non-nil, is the store-level teardown owner for the
	// durability stack backing this server's engine — typically a
	// *[github.com/FlavioCFOliveira/GoGraph/store.DB] bundling the WAL writer
	// and the background checkpointer. The server closes it AFTER it has
	// drained every active connection, so it runs the one crash-safe teardown
	// order (stop the checkpoint goroutine, then close the WAL) only once no
	// in-flight transaction can still be writing. Both documented stop
	// mechanisms reach that teardown: [Server.Shutdown] closes it on its
	// drain-success branch, and [Server.Serve] closes it on its own exit path
	// once its connection drain completes (e.g. when the Serve context is
	// cancelled). The close is guarded by a [sync.Once] inside the server, so
	// the closer's Close runs exactly once regardless of which path wins or
	// whether both run; it need not be idempotent itself. Leave it nil for a
	// store-less engine or when the embedder tears the durability stack down
	// itself; the server then closes nothing beyond its connections.
	Closer io.Closer
}

// ErrNoAuthHandler is returned by [NewServer] when Options.Auth is nil. The
// server is secure-by-default: running without authentication must be an
// explicit opt-in, never an accidental default. Set Options.Auth to a real
// [AuthHandler] to require credentials, or set Options.Auth to a
// [NoAuthHandler]{} value to run the open-door handler on purpose
// (development and testing only).
var ErrNoAuthHandler = errors.New("bolt: no auth handler configured; set Options.Auth to a real AuthHandler, or to NoAuthHandler{} to run without authentication")

// Server is the Bolt v5 TCP server. It accepts connections from a
// net.Listener, negotiates the protocol version, and runs the Bolt message
// loop on each connection.
//
// Server is safe for concurrent use by multiple goroutines.
type Server struct {
	eng    *cypher.Engine
	opts   Options
	sem    chan struct{} // capacity == MaxConnections
	log    *slog.Logger
	closer io.Closer // optional store-level teardown owner; closed after drain by Serve's exit path or Shutdown
	// closeOnce guards the one-and-only Close of closer: Serve's drained-exit
	// path and Shutdown's drain-success branch may both reach closeOwned (ctx
	// cancellation plus an explicit Shutdown, or a double Shutdown), and the
	// closer must be torn down exactly once, race-free. closeErr caches the
	// result so every caller observes the same outcome; sync.Once's
	// happens-before guarantee makes the cached read race-free.
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	ln        net.Listener // guarded by mu; non-nil while Serve is running
	wg        sync.WaitGroup
	cancel    context.CancelFunc // guarded by mu; stops the accept loop
}

// NewServer creates a Server backed by eng. Zero-value Options fields are
// filled with sensible defaults.
//
// NewServer is secure-by-default: it never silently installs an
// accept-everyone authentication handler. If Options.Auth is nil it fails
// closed and returns [ErrNoAuthHandler] so that an unauthenticated server is
// never started by accident. To run without authentication on purpose
// (development or testing), set Options.Auth to a [NoAuthHandler]{} value
// explicitly: NewServer then admits every client and logs a loud warning that
// the operator has knowingly disabled authentication. The explicit
// NoAuthHandler value is itself the opt-in — self-documenting at the call site
// and impossible to set by accident. When Options.Auth is any other
// (real) handler it is used as-is.
//
//nolint:gocritic // hugeParam: Options is passed by value intentionally; NewServer is the public constructor and the by-value signature is its stable contract.
func NewServer(eng *cypher.Engine, opts Options) (*Server, error) {
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = defaultMaxConnections
	}
	if opts.MaxInFlightPerConnection <= 0 {
		opts.MaxInFlightPerConnection = DefaultMaxInFlightPerConnection
	}
	if opts.ConnTimeout <= 0 {
		opts.ConnTimeout = DefaultConnTimeout
	}
	if opts.DefaultTxTimeout <= 0 {
		opts.DefaultTxTimeout = DefaultTxTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Auth == nil {
		// Secure-by-default: a missing auth handler is a hard error. A
		// zero-value Options can never accidentally run open.
		return nil, ErrNoAuthHandler
	}
	if _, ok := opts.Auth.(NoAuthHandler); ok {
		// Explicit opt-in: the embedder constructed NoAuthHandler{} on purpose.
		// Admit every client but warn loudly that authentication is disabled.
		log.Warn("bolt: server started with no authentication (Options.Auth is NoAuthHandler) — every client is admitted without credential validation; set Options.Auth to a real AuthHandler before exposing this server on a network")
	}
	if eng != nil && eng.ResultRowCap() == 0 {
		// The engine was built with cypher.MaxResultRowsUnlimited, so a single
		// authenticated RUN/PULL of a Cartesian product or whole-graph MATCH can
		// materialise an unbounded result set inside the engine's visibility
		// barrier and exhaust memory before the first RECORD is chunked out. Warn
		// loudly: the server cannot retrofit a bound onto a pre-built engine, so
		// the operator must rebuild it with a finite cypher.EngineOptions.MaxResultRows.
		log.Warn("bolt: server backed by an engine with no result-row cap (cypher.EngineOptions.MaxResultRows is unlimited) — a single client query can materialise an unbounded result set and exhaust server memory; rebuild the engine with a finite MaxResultRows before exposing this server on a network")
	}
	return &Server{
		eng:    eng,
		opts:   opts,
		sem:    make(chan struct{}, opts.MaxConnections),
		log:    log,
		closer: opts.Closer,
	}, nil
}

// Serve accepts connections from ln until ctx is cancelled or Shutdown is
// called. It blocks until all active connections have closed. The provided
// ln is closed by Serve when the accept loop exits.
//
// Once every connection has drained, Serve also closes the owned
// [Options.Closer] (the store-level teardown owner for the durability stack,
// typically a *[github.com/FlavioCFOliveira/GoGraph/store.DB]), so stopping
// the server by cancelling ctx tears the WAL/checkpoint stack down in its
// crash-safe order exactly as [Server.Shutdown] does — no checkpoint goroutine
// or WAL handle outlives Serve. The close happens strictly after the drain, so
// it can never race an in-flight write, and it is once-guarded, so a
// subsequent (or concurrent) Shutdown does not close the closer again. A
// failed close is returned (joined with any accept error) rather than
// swallowed. After Serve returns, the durability stack is closed: the Server
// must not be reused to serve writes again.
func (s *Server) Serve(ctx context.Context, ln net.Listener) (err error) {
	acceptCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.ln = ln
	s.cancel = cancel
	s.mu.Unlock()

	// Track the listener-close goroutine so we can wait for it in the deferred
	// cleanup and avoid spurious goroutine-leak reports.
	var closeWG sync.WaitGroup
	closeWG.Add(1)
	go pprof.Do(acceptCtx, pprof.Labels("component", "bolt-server-close-waiter"), func(_ context.Context) {
		defer closeWG.Done()
		<-acceptCtx.Done()
		_ = ln.Close() //nolint:errcheck // closing to unblock Accept; error is not actionable
	})

	defer func() {
		cancel()       // signals the close goroutine to run
		closeWG.Wait() // wait for the close goroutine to finish
		s.wg.Wait()    // wait for all connection goroutines to finish
		// Every connection has finished, so no transaction can still be
		// writing: tear the owned durability stack down in its crash-safe
		// order (stop the checkpoint goroutine, then close the WAL). This is
		// the same post-drain point Shutdown's drain-success branch reaches;
		// the once-guard in closeOwned keeps the two paths from double-closing.
		// Without this, stopping the server via ctx cancellation (a documented
		// mechanism) leaked the checkpoint goroutine and never closed the WAL
		// (#1351). A close failure must surface, not be swallowed.
		err = errors.Join(err, s.closeOwned())
		s.mu.Lock()
		s.ln = nil
		s.cancel = nil
		s.mu.Unlock()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Check if we were asked to stop (context cancelled or Shutdown).
			select {
			case <-acceptCtx.Done():
				return nil
			default:
			}
			// net.ErrClosed means the listener was closed (e.g. by the goroutine above
			// after context cancellation); treat as clean shutdown.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Transient errors: keep accepting.
			if isTemporary(err) {
				s.log.Warn("bolt: accept transient error", slog.String("err", err.Error()))
				continue
			}
			return fmt.Errorf("bolt: accept: %w", err)
		}

		// Acquire semaphore slot — non-blocking; reject immediately if full.
		select {
		case s.sem <- struct{}{}:
		default:
			// Refused because the MaxConnections semaphore is full. Count it so an
			// operator can correlate a connection flood: a rejected connection never
			// becomes live, so the accepted/closed gauge alone cannot reveal it.
			incCounter(metricConnRejected)
			s.log.Warn("bolt: max connections reached, rejecting", slog.String("remote", conn.RemoteAddr().String()))
			_ = conn.Close() //nolint:errcheck // best-effort close on rejection path
			continue
		}

		// The connection is admitted past the semaphore and will start a handler
		// goroutine below. Count it as accepted; the matching closed increment in
		// the goroutine's deferred cleanup balances the live-connection derivation
		// (accepted − closed) back to zero on every exit path.
		incCounter(metricConnAccepted)

		if s.opts.TLSConfig != nil {
			conn = tls.Server(conn, s.opts.TLSConfig)
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer func() {
				// Pair the accepted increment: the live-connection gauge is derived
				// as accepted − closed, so this runs on EVERY exit (clean close,
				// read/write error, idle timeout, recovered panic) to keep the
				// derivation balanced and free of a phantom live connection.
				incCounter(metricConnClosed)
				<-s.sem
				s.wg.Done()
			}()
			s.handleConn(acceptCtx, c)
		}(conn)
	}
}

// ListenAndServe creates a TCP listener on addr and calls Serve. It blocks
// until the server stops. The listener is closed when Serve returns.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("bolt: listen %s: %w", addr, err)
	}
	defer ln.Close() //nolint:errcheck // listener close error is not actionable
	return s.Serve(ctx, ln)
}

// Shutdown gracefully stops accepting new connections and waits for active
// connections to finish. If connections do not finish within 30 seconds, it
// closes the listener forcefully and returns an error.
//
// When the server was constructed with [Options.Closer] (the store-level
// teardown owner for the durability stack, typically a
// *[github.com/FlavioCFOliveira/GoGraph/store.DB]), Shutdown closes it AFTER
// every active connection has drained — so the WAL/checkpoint teardown runs in
// its crash-safe order only once no in-flight transaction can still be writing.
// Closing it before the drain could let a still-executing write race the WAL
// close. The closer is therefore NOT torn down on the timeout or
// ctx-cancellation paths: an undrained connection may still hold a transaction,
// so tearing the WAL down underneath it is exactly what must be avoided; in
// those cases the connections are abandoned. A still-running [Server.Serve]
// remains blocked on the same drain, and when the abandoned connections do
// eventually finish (idle timeout, transaction reap, client exit), Serve's own
// exit path performs the post-drain close — so the closer is torn down as soon
// as a full drain truly completes, and is left for process exit only if it
// never does. The close is once-guarded: whichever of Serve or Shutdown drains
// first runs it, and the other observes the same cached result, so the closer
// is never closed twice (including on a double Shutdown). A failed WAL close
// is surfaced rather than swallowed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	ln := s.ln
	cancel := s.cancel
	s.mu.Unlock()

	// Stop the accept loop.
	if cancel != nil {
		cancel()
	}
	if ln != nil {
		// Unblock Accept so the accept loop can observe context cancellation.
		_ = ln.Close() //nolint:errcheck // close error is not actionable during shutdown
	}

	// Wait for active connections with a drain timeout.
	done := make(chan struct{})
	go pprof.Do(ctx, pprof.Labels("component", "bolt-server-drain"), func(_ context.Context) {
		s.wg.Wait()
		close(done)
	})

	drainTimeout := shutdownDrainTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < drainTimeout {
			drainTimeout = remaining
		}
	}

	select {
	case <-done:
		// Every connection has finished, so no transaction can still be
		// writing: it is now safe to tear the durability stack down in its
		// crash-safe order (the closer stops the checkpoint goroutine, then
		// closes the WAL).
		return s.closeOwned()
	case <-time.After(drainTimeout):
		return errors.New("bolt: shutdown: drain timeout exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeOwned closes the optional store-level teardown owner ([Options.Closer]),
// if any. It is called only from fully-drained stop paths — Serve's deferred
// exit cleanup (after its connection WaitGroup drains) and Shutdown's
// drain-success branch — where no connection, and therefore no transaction,
// can still be writing, so the closer's WAL teardown cannot race an in-flight
// write. A nil closer is a no-op. The close body runs exactly once under
// s.closeOnce (the two paths can both be reached: ctx cancellation plus an
// explicit Shutdown, or a double Shutdown); every call returns the same cached
// result, with sync.Once's happens-before edge making the cached read
// race-free. closeOwned is safe for concurrent use.
func (s *Server) closeOwned() error {
	if s.closer == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if err := s.closer.Close(); err != nil {
			s.closeErr = fmt.Errorf("bolt: close owned store: %w", err)
		}
	})
	return s.closeErr
}

// handleConn runs the full Bolt lifecycle for one accepted connection.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close() //nolint:errcheck // close error is not actionable on teardown

	remote := conn.RemoteAddr().String()

	// Defence-in-depth panic boundary (H7). A recoverable panic raised while
	// decoding, dispatching, or executing a frame (e.g. an index-out-of-range
	// on a malformed message or a future bug) would otherwise unwind past this
	// goroutine and crash the whole process, taking down every other live
	// connection — a direct violation of the CLAUDE.md "the library must never
	// crash" mandate. Per the CLAUDE.md exception for goroutines the library
	// owns, we recover ONLY to (a) log the panic with session/remote labels and
	// a stack trace, (b) increment a metric counter, and (c) terminate this one
	// goroutine cleanly. The deferred conn.Close above (registered first, so it
	// runs after this recover) closes the offending connection; the accept-loop
	// wrapper's semaphore/WaitGroup release still runs because handleConn
	// returns normally. We do not swallow-and-continue: the connection dies.
	//
	// This guards RECOVERABLE panics only. A Go fatal runtime error (an
	// uncatchable stack overflow) cannot be recovered here; that class is
	// handled upstream by the depth/length guards in the PackStream decoder
	// and the Cypher parser.
	defer func() {
		if r := recover(); r != nil {
			incCounter(metricConnPanics)
			s.log.Error("bolt: recovered panic in connection handler; closing connection",
				slog.String("remote", remote),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()

	s.log.Debug("bolt: connection accepted", slog.String("remote", remote))

	// Label the per-connection goroutine so pprof goroutine dumps
	// group connections by purpose and remote endpoint. Per CLAUDE.md,
	// every long-lived goroutine is observable; this is the cheapest
	// way to keep that promise for the Bolt server's per-conn workers.
	// The labels are visible in pprof's "goroutine?debug=2" output and
	// in goroutine profile listings.
	pprof.SetGoroutineLabels(
		pprof.WithLabels(ctx,
			pprof.Labels("component", "bolt-server-conn", "remote", remote)),
	)

	// ── 1. Version negotiation ───────────────────────────────────────────
	//
	// Bound the unauthenticated handshake with a dedicated deadline. This is
	// the cheapest phase for an attacker to abuse: a client may open a socket,
	// send a single byte, and otherwise stall, holding a connection slot and
	// goroutine forever (a Slowloris-style denial of service). The handshake
	// bound is a fixed package value (handshakeTimeout, seeded from
	// DefaultHandshakeTimeout) rather than a configurable Options field, so it
	// is always applied regardless of the accept context (which carries no
	// deadline) and independently of ConnTimeout. proto.Negotiate may install
	// its own deadline from ctx and clear it on return, but we do not rely on
	// that — we set the deadline here and clear it ourselves after a successful
	// handshake so it never bleeds into the post-handshake message loop, which
	// ConnTimeout governs.
	if hsTO := time.Duration(handshakeTimeout.Load()); hsTO > 0 {
		_ = conn.SetDeadline(time.Now().Add(hsTO)) //nolint:errcheck
	}

	ver, err := proto.Negotiate(ctx, conn)
	if err != nil {
		s.log.Warn("bolt: handshake failed",
			slog.String("remote", remote),
			slog.String("err", err.Error()))
		return
	}

	// Clear the handshake deadline so it does not constrain the message loop;
	// the loop resets the idle ConnTimeout before every read below.
	_ = conn.SetDeadline(time.Time{}) //nolint:errcheck
	s.log.Debug("bolt: negotiated",
		slog.String("remote", remote),
		slog.Uint64("major", uint64(ver.Major)),
		slog.Uint64("minor", uint64(ver.Minor)))

	// ── 2. Set up framing and session ────────────────────────────────────
	cr := proto.NewChunkedReaderWithLimit(conn, s.opts.MaxMessageBytes)
	cw := proto.NewChunkedWriter(conn)
	// Pass the listener address so ROUTE responses can populate the routing table.
	localAddr := ""
	s.mu.Lock()
	if s.ln != nil {
		localAddr = s.ln.Addr().String()
	}
	s.mu.Unlock()
	sess := newSession(s.eng, s.opts.Auth, localAddr)
	sess.setBoltVersion(ver)
	sess.setMaxInFlight(s.opts.MaxInFlightPerConnection)
	sess.setMaxStmtTimeout(s.opts.MaxStatementTimeout)
	sess.setDefaultTxTimeout(s.opts.DefaultTxTimeout)

	// Stream RECORD messages incrementally: handlePull hands each record to
	// this sink, which encodes and writes it to the connection immediately
	// under the per-message write deadline — so a large PULL is written row by
	// row instead of being duplicated into a second in-memory copy, and a slow
	// reader exerts backpressure (the producing loop blocks on the TCP write)
	// until the write deadline trips for THIS connection only (task #1350).
	// Flushing per record adds no write amplification: the buffered path also
	// issued one chunked write per RECORD after the handler returned. A write
	// failure was already logged by writeResponse; returning errRecordWrite
	// makes handlePull stop streaming and the message loop below tear the
	// connection down.
	sess.setRecordSink(func(rec *proto.Record) error {
		if !s.writeResponse(cw, conn, rec, remote) {
			return errRecordWrite
		}
		return nil
	})

	// Per-connection cancellable context. Cancelling it stops any in-flight
	// statement promptly (the engine honours context cancellation per result
	// row), so a long-running RUN/PULL no longer keeps consuming CPU/memory
	// after the client has vanished. It is cancelled on teardown AND as soon as
	// the reader goroutine below observes the client has gone — even while this
	// goroutine is blocked executing a statement (task #1348).
	connCtx, cancelConn := context.WithCancel(ctx)

	// The reader goroutine owns the connection read side and reports each framed
	// message (or the terminal read error) to the message loop. msgCh is
	// unbuffered, so the reader runs at most one message ahead of processing;
	// readerDone is closed when it exits.
	msgCh := make(chan readResultT)
	readerDone := make(chan struct{})

	// Tear the connection down on EVERY exit path — clean GOODBYE, client
	// disconnect, read/write error, idle timeout, or a recovered panic. The
	// order matters: cancel the connection context first (stopping any in-flight
	// statement and signalling the reader), then close the socket (unblocking a
	// reader stuck in ReadMessage), then JOIN the reader goroutine (so it is
	// never left running at teardown — goleak-clean), then roll back any open
	// explicit transaction via sess.Close. Rolling back releases the engine's
	// single-writer serialisation immediately rather than leaving it for GC to
	// reclaim from a leaked transaction (#1309). sess.Close is idempotent.
	defer func() {
		cancelConn()
		_ = conn.Close() //nolint:errcheck // close error is not actionable on teardown
		<-readerDone
		sess.Close()
	}()

	go func() {
		defer close(readerDone)
		pprof.SetGoroutineLabels(
			pprof.WithLabels(connCtx,
				pprof.Labels("component", "bolt-server-reader", "remote", remote)),
		)
		for {
			// Idle ConnTimeout bound on the read. The transaction-timeout reaper
			// is handled by the message loop's timer below, so the reader only
			// enforces the idle bound here.
			if s.opts.ConnTimeout > 0 {
				if err := conn.SetReadDeadline(time.Now().Add(s.opts.ConnTimeout)); err != nil {
					cancelConn()
					sendRead(connCtx, msgCh, readResultT{err: err})
					return
				}
			}
			raw, err := cr.ReadMessage()
			if err != nil {
				// Cancel the connection context so an in-flight statement in the
				// message loop stops promptly, then report the error.
				cancelConn()
				sendRead(connCtx, msgCh, readResultT{err: err})
				return
			}
			if !sendRead(connCtx, msgCh, readResultT{raw: raw}) {
				return // connection torn down; stop reading
			}
		}
	}()

	// ── 3. Message loop ──────────────────────────────────────────────────
	//
	// txTimer reaps an idle-but-open explicit transaction at its wall-clock
	// deadline so it cannot hold the engine writer lock past the timeout while a
	// client keeps the connection alive with no-op messages (task #1346). It is
	// armed when a transaction with a finite deadline is open and stopped when
	// the transaction ends. The reap runs on THIS goroutine (no race with the
	// single-threaded session) and the connection survives so the client may
	// RESET — preserving the #1302 behaviour that a statement issued after the
	// timeout still receives a typed FAILURE.
	var txTimer *time.Timer
	var txTimerC <-chan time.Time
	syncTxTimer := func() {
		switch {
		case sess.txActive && !sess.txDeadline.IsZero():
			if txTimer == nil {
				d := time.Until(sess.txDeadline)
				if d < 0 {
					d = 0
				}
				txTimer = time.NewTimer(d)
				txTimerC = txTimer.C
			}
		case txTimer != nil:
			txTimer.Stop()
			txTimer = nil
			txTimerC = nil
		}
	}

	for {
		select {
		case <-connCtx.Done():
			// Server shutdown or teardown: stop serving. The reader observes the
			// same cancellation and exits; the deferred cleanup joins it.
			return
		case <-txTimerC:
			// The open transaction reached its wall-clock deadline.
			txTimer = nil
			txTimerC = nil
			if sess.txActive {
				s.log.Warn("bolt: explicit transaction timed out; rolled back to release the writer lock",
					slog.String("remote", remote))
				sess.reapTimedOutTx()
			}
			continue
		case res := <-msgCh:
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					s.log.Debug("bolt: connection closed by client", slog.String("remote", remote))
				} else if !isConnClosed(res.err) && !isTimeout(res.err) {
					s.log.Warn("bolt: read error",
						slog.String("remote", remote),
						slog.String("err", res.err.Error()))
				}
				return
			}

			// Decode request from PackStream bytes.
			dec := packstream.NewDecoder(bytes.NewReader(res.raw))
			msg, decErr := proto.DecodeRequest(dec)
			if decErr != nil {
				// A message that fails to decode is a CLIENT fault (a malformed or
				// truncated PackStream frame). The status code already says so
				// (Neo.ClientError.Request.Invalid); the message must match that
				// classification rather than the generic internal-error text the
				// sanitiser produces for unrecognised errors — the raw decode error
				// would leak internal framing detail, but the generic
				// internal-error text wrongly implies a server bug. A fixed,
				// non-sensitive string ("malformed Bolt message") is honest about
				// the fault and discloses nothing internal (task #1435). The real
				// decode error is logged server-side for correlation. The
				// connection is not torn down: the client may send a fresh message
				// or RESET.
				s.log.Warn("bolt: decode error",
					slog.String("remote", remote),
					slog.String("err", decErr.Error()))
				if !s.writeResponse(cw, conn, &proto.Failure{
					Code:    "Neo.ClientError.Request.Invalid",
					Message: "malformed Bolt message",
				}, remote) {
					return
				}
				continue
			}

			// Dispatch to session. HandleMessage's error return is reserved for
			// internal-only failures (currently none: handlers surface
			// client-visible errors via *proto.Failure in 'responses'). The
			// defensive branch keeps a future internal-failure path from
			// disappearing silently.
			responses, handlerErr := sess.HandleMessage(connCtx, msg)
			if handlerErr != nil {
				// A failed record-stream write means the wire may hold a partial
				// chunked message: no further well-formed message can be sent.
				// Tear the connection down; the write error was already logged
				// by writeResponse and the session has reclaimed its cursor and
				// any open transaction (see Session.abortStream).
				if errors.Is(handlerErr, errRecordWrite) {
					return
				}
				s.log.Error("bolt: handler error",
					slog.String("remote", remote),
					slog.String("err", handlerErr.Error()))
				if !s.writeResponse(cw, conn, &proto.Failure{
					Code:    "Neo.DatabaseError.General.UnknownError",
					Message: handlerErr.Error(),
				}, remote) {
					return
				}
			}

			// Send all response messages.
			done := false
			for _, resp := range responses {
				if !s.writeResponse(cw, conn, resp, remote) {
					done = true
					break
				}
			}
			if done {
				return
			}

			// Re-arm or stop the transaction-timeout reaper to match the new state.
			syncTxTimer()

			// Exit the loop when the session transitions to DEFUNCT.
			if sess.state == StateDefunct {
				s.log.Debug("bolt: session defunct, closing", slog.String("remote", remote))
				return
			}
		}
	}
}

// readResultT is one outcome of a reader-goroutine read handed to the message
// loop: a framed message (raw, err == nil) or the terminal read error (err).
type readResultT struct {
	raw []byte
	err error
}

// sendRead delivers a read result to the message loop, or returns false if the
// connection context was cancelled first (the loop has gone away). It is the
// reader goroutine's single hand-off point.
func sendRead(ctx context.Context, ch chan<- readResultT, res readResultT) bool {
	select {
	case ch <- res:
		return true
	case <-ctx.Done():
		return false
	}
}

// writeResponse sets the per-message write deadline (idle ConnTimeout) and
// writes one response, logging and returning false on a write error so the
// caller tears the connection down. It centralises the write-deadline handling
// the per-read deadline (now owned by the reader goroutine) no longer covers.
func (s *Server) writeResponse(cw *proto.ChunkedWriter, conn net.Conn, msg any, remote string) bool {
	if s.opts.ConnTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(s.opts.ConnTimeout)) //nolint:errcheck // a write error below tears the conn down anyway
	}
	if err := sendResponse(cw, msg); err != nil {
		s.log.Warn("bolt: write error",
			slog.String("remote", remote),
			slog.String("err", err.Error()))
		return false
	}
	return true
}

// sendResponse encodes a single proto response message and writes it as a
// chunked Bolt message.
func sendResponse(cw *proto.ChunkedWriter, msg any) error {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeResponse(enc, msg); err != nil {
		return fmt.Errorf("bolt: encode response %T: %w", msg, err)
	}
	// Encoder uses an internal bufio.Writer; flush to buf before reading bytes.
	if err := enc.Flush(); err != nil {
		return fmt.Errorf("bolt: flush encoder %T: %w", msg, err)
	}
	if err := cw.WriteMessage(buf.Bytes()); err != nil {
		return fmt.Errorf("bolt: write response %T: %w", msg, err)
	}
	return nil
}

// isTemporary reports whether an Accept error is transient.
func isTemporary(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isTimeout reports whether err is an I/O deadline (timeout) error, e.g. the
// read deadline set before each message firing. It is used to distinguish a
// transaction-timeout reap from a hard read error in the message loop.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isConnClosed reports whether an error is a benign connection-closed error
// (used to suppress noisy log lines on clean teardown).
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

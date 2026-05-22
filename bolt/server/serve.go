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
	"runtime/pprof"
	"sync"
	"time"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
	"gograph/cypher"
)

const (
	// defaultMaxConnections is the default upper bound on concurrent connections.
	defaultMaxConnections = 1024

	// DefaultMaxInFlightPerConnection is the default value applied to
	// Options.MaxInFlightPerConnection when the caller leaves it at
	// zero. The very low default (1) reflects the fact that a single
	// Result cursor at a time is sufficient for every Bolt v5 client
	// the server is intended to serve: drivers issue RUN, drain via
	// PULL/DISCARD, and only then issue the next RUN. Operators
	// running a workload that legitimately needs many in-flight
	// cursors per connection (long-running explicit transactions that
	// accumulate cursors before COMMIT) must raise this value
	// explicitly.
	DefaultMaxInFlightPerConnection = 1

	// shutdownDrainTimeout is the maximum time Shutdown waits for active
	// connections to finish after stopping the accept loop.
	shutdownDrainTimeout = 30 * time.Second
)

// Options configures a Server.
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

	// MaxInFlightPerConnection caps the number of Result cursors a
	// single connection may have open at the same time. Zero or
	// negative values default to [DefaultMaxInFlightPerConnection]
	// (1). Cursors are counted across both auto-commit and explicit
	// transactions: in auto-commit mode the cap is one cursor at
	// most (already implied by the Bolt v5 state machine), but in
	// explicit transactions a malicious client could otherwise RUN
	// many queries in sequence, drain each one with PULL, and let
	// the cursors accumulate in tx.results until Commit. The cap
	// surfaces as a typed Bolt FAILURE with code
	// "Neo.ClientError.General.LimitExceeded".
	MaxInFlightPerConnection int

	// ConnTimeout is the per-connection idle read deadline. Each time the
	// server is about to read the next message, the deadline is reset to
	// now+ConnTimeout. Zero means no timeout.
	ConnTimeout time.Duration

	// TLSConfig, when non-nil, wraps accepted connections with TLS using
	// the given configuration. nil means plain TCP.
	TLSConfig *tls.Config

	// Auth is the authentication handler used during HELLO/LOGON. When nil,
	// NoAuthHandler is used.
	Auth AuthHandler

	// Logger is the structured logger for server events. When nil, the
	// default slog handler is used.
	Logger *slog.Logger
}

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
	mu     sync.Mutex
	ln     net.Listener // guarded by mu; non-nil while Serve is running
	wg     sync.WaitGroup
	cancel context.CancelFunc // guarded by mu; stops the accept loop
}

// NewServer creates a Server backed by eng. Zero-value Options fields are
// filled with sensible defaults.
func NewServer(eng *cypher.Engine, opts Options) *Server {
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = defaultMaxConnections
	}
	if opts.MaxInFlightPerConnection <= 0 {
		opts.MaxInFlightPerConnection = DefaultMaxInFlightPerConnection
	}
	if opts.Auth == nil {
		opts.Auth = NoAuthHandler{}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		eng:  eng,
		opts: opts,
		sem:  make(chan struct{}, opts.MaxConnections),
		log:  log,
	}
}

// Serve accepts connections from ln until ctx is cancelled or Shutdown is
// called. It blocks until all active connections have closed. The provided
// ln is closed by Serve when the accept loop exits.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	acceptCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.ln = ln
	s.cancel = cancel
	s.mu.Unlock()

	// Track the listener-close goroutine so we can wait for it in the deferred
	// cleanup and avoid spurious goroutine-leak reports.
	var closeWG sync.WaitGroup
	closeWG.Add(1)
	go func() {
		defer closeWG.Done()
		<-acceptCtx.Done()
		_ = ln.Close() //nolint:errcheck // closing to unblock Accept; error is not actionable
	}()

	defer func() {
		cancel()       // signals the close goroutine to run
		closeWG.Wait() // wait for the close goroutine to finish
		s.wg.Wait()    // wait for all connection goroutines to finish
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
			s.log.Warn("bolt: max connections reached, rejecting", slog.String("remote", conn.RemoteAddr().String()))
			_ = conn.Close() //nolint:errcheck // best-effort close on rejection path
			continue
		}

		if s.opts.TLSConfig != nil {
			conn = tls.Server(conn, s.opts.TLSConfig)
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer func() {
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
	go func() {
		s.wg.Wait()
		close(done)
	}()

	drainTimeout := shutdownDrainTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < drainTimeout {
			drainTimeout = remaining
		}
	}

	select {
	case <-done:
		return nil
	case <-time.After(drainTimeout):
		return errors.New("bolt: shutdown: drain timeout exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleConn runs the full Bolt lifecycle for one accepted connection.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close() //nolint:errcheck // close error is not actionable on teardown

	remote := conn.RemoteAddr().String()
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
	if s.opts.ConnTimeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.opts.ConnTimeout)) //nolint:errcheck
	}

	ver, err := proto.Negotiate(ctx, conn)
	if err != nil {
		s.log.Warn("bolt: handshake failed",
			slog.String("remote", remote),
			slog.String("err", err.Error()))
		return
	}
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
	sess.setMaxInFlight(s.opts.MaxInFlightPerConnection)

	// ── 3. Message loop ──────────────────────────────────────────────────
	for {
		// Reset the per-message I/O deadline.
		if s.opts.ConnTimeout > 0 {
			if err := conn.SetDeadline(time.Now().Add(s.opts.ConnTimeout)); err != nil {
				s.log.Debug("bolt: SetDeadline failed, closing",
					slog.String("remote", remote),
					slog.String("err", err.Error()))
				return
			}
		}

		// Read raw chunked message.
		raw, err := cr.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.log.Debug("bolt: connection closed by client", slog.String("remote", remote))
			} else if !isConnClosed(err) {
				s.log.Warn("bolt: read error",
					slog.String("remote", remote),
					slog.String("err", err.Error()))
			}
			return
		}

		// Decode request from PackStream bytes.
		dec := packstream.NewDecoder(bytes.NewReader(raw))
		msg, decErr := proto.DecodeRequest(dec)
		if decErr != nil {
			// Send FAILURE for malformed message.
			s.log.Warn("bolt: decode error",
				slog.String("remote", remote),
				slog.String("err", decErr.Error()))
			_ = sendResponse(cw, &proto.Failure{
				Code:    "Neo.ClientError.Request.Invalid",
				Message: decErr.Error(),
			})
			continue
		}

		// Dispatch to session.
		responses, handlerErr := sess.HandleMessage(ctx, msg)
		if handlerErr != nil {
			s.log.Error("bolt: handler error",
				slog.String("remote", remote),
				slog.String("err", handlerErr.Error()))
			// handlerErr from HandleMessage is reserved for internal errors;
			// the session has already set state to FAILED.
			_ = sendResponse(cw, &proto.Failure{
				Code:    "Neo.DatabaseError.General.UnknownError",
				Message: handlerErr.Error(),
			})
		}

		// Send all response messages.
		for _, resp := range responses {
			if err := sendResponse(cw, resp); err != nil {
				s.log.Warn("bolt: write error",
					slog.String("remote", remote),
					slog.String("err", err.Error()))
				return
			}
		}

		// Exit the loop when the session transitions to DEFUNCT.
		if sess.state == StateDefunct {
			s.log.Debug("bolt: session defunct, closing", slog.String("remote", remote))
			return
		}
	}
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

// isConnClosed reports whether an error is a benign connection-closed error
// (used to suppress noisy log lines on clean teardown).
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

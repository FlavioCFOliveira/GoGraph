package server

import (
	"log/slog"
	"net"
)

// configureKeepAlive installs the server's TCP keep-alive parameters on one
// accepted connection.
//
// # Why the server does this at all
//
// Until rmp #2807 the server's liveness check on an established connection was
// Options.ConnTimeout, a read deadline armed before every read. That deadline
// ran while the message loop was executing the client's own statement, so it
// could not distinguish a peer that had vanished from a client waiting for the
// records it had just asked for, and it closed busy connections. It is disabled
// by default now, and TCP keep-alive takes its place: the kernel probes a silent
// connection and drops it when the peer stops answering, which is precisely the
// "is the peer still there?" question, asked of the peer instead of of the
// clock. It is the mechanism PostgreSQL relies on for the same purpose.
//
// # Why it is not left to the listener
//
// [Server.Serve] takes a caller-supplied [net.Listener]. A connection accepted
// from one built by [net.Listen] inherits Go's own defaults (15 s idle, 15 s
// interval, 9 probes), but one accepted from a listener the embedder configured
// with net.ListenConfig{KeepAlive: -1} has keep-alive OFF and falls back to the
// platform default (7200 s idle on this project's development hosts) if it is
// ever enabled. Setting the parameters per accepted connection makes the budget
// a property of the server rather than of how the embedder built the listener.
//
// # Failure handling
//
// A connection that is not a *[net.TCPConn] — a [net.Pipe] from a test, a Unix
// socket, an embedder's own transport — is left alone without error: keep-alive
// is a TCP-level concept and there is nothing to configure. A real
// SetKeepAliveConfig failure is logged and the connection is served anyway: the
// keep-alive budget is a liveness optimisation, and refusing to serve a client
// because the kernel declined a socket option would turn a degraded liveness
// check into an outage.
func (s *Server) configureKeepAlive(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		// Not a TCP connection (net.Pipe, Unix socket, a test transport):
		// keep-alive does not apply. Not an error, and not worth a log line on
		// the accept path.
		return
	}
	if err := tcp.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     DefaultKeepAliveIdle,
		Interval: DefaultKeepAliveInterval,
		Count:    DefaultKeepAliveCount,
	}); err != nil {
		// RemoteAddr().String() allocates, so it is formatted only on this
		// branch rather than on every accepted connection.
		s.log.Warn("bolt: could not enable TCP keep-alive on accepted connection; a peer that vanishes will be detected only by the platform default, or not at all",
			slog.String("remote", conn.RemoteAddr().String()),
			slog.String("err", err.Error()),
			slog.Duration("idle", DefaultKeepAliveIdle),
			slog.Duration("interval", DefaultKeepAliveInterval),
			slog.Int("count", DefaultKeepAliveCount))
	}
}

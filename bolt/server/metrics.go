package server

import "github.com/FlavioCFOliveira/GoGraph/internal/metrics"

// Server-level observability metric names emitted through the shared
// [github.com/FlavioCFOliveira/GoGraph/internal/metrics] backend (the same sink
// that carries [metricConnPanics]). They give an operator the signals needed to
// correlate a connection flood or a transaction leak: a rejected-connection
// counter, a live-connection gauge, a live-transaction gauge, and an
// abnormally-abandoned-transaction counter.
//
// Gauges as paired counters. The internal/metrics [metrics.Backend] interface
// exposes only IncCounter (a monotonic, non-decrementing counter) and
// ObserveLatency — it has no decrementable gauge primitive. The two quantities
// that are conceptually gauges (the number of live connections and the number
// of open transactions) are therefore each emitted as a pair of monotonic
// counters, and the live value is the derivation
//
//	live = <opened-counter> − <closed-counter>
//
// This is the standard Prometheus "created/closed → in-use = created − closed"
// pattern. It keeps the metrics sink lock-free (counters only) and the two
// derived gauges balanced by construction: every increment of an opened-side
// counter has exactly one matching increment of its closed-side counter on
// every exit path (clean close, read/write error, idle timeout, recovered
// panic), so the derived live value always returns to zero once the server is
// quiescent. A persistently non-zero derivation is itself the leak signal.
const (
	// metricConnPanics counts recovered panics in a connection handler
	// goroutine (security fix H7). It predates the other server-level metrics
	// and is kept here so every bolt.server.* name lives in one place.
	metricConnPanics = "bolt.server.conn.panics"

	// metricConnAccepted counts connections admitted past the MaxConnections
	// semaphore — one increment per connection that starts a per-connection
	// handler goroutine. Paired with [metricConnClosed] it derives the live
	// connection count (accepted − closed).
	metricConnAccepted = "bolt.server.conn.accepted"

	// metricConnRejected counts connections refused because the MaxConnections
	// semaphore was already full. A rising value correlates a connection flood
	// that the accepted/closed gauge alone cannot reveal (a rejected connection
	// never becomes live).
	metricConnRejected = "bolt.server.conn.rejected"

	// metricConnClosed counts per-connection handler goroutines that have
	// exited, for any reason. Paired with [metricConnAccepted] it derives the
	// live connection count (accepted − closed); the pair is balanced because
	// the increment is emitted from the accept-loop goroutine's deferred
	// cleanup, which runs on every exit path.
	metricConnClosed = "bolt.server.conn.closed"

	// metricTxOpened counts explicit transactions opened by a BEGIN that
	// successfully acquired the engine writer serialisation. Paired with
	// [metricTxClosed] it derives the number of open transactions
	// (opened − closed).
	metricTxOpened = "bolt.server.tx.opened"

	// metricTxClosed counts explicit transactions that have ended — committed,
	// rolled back, discarded by RESET/GOODBYE, or rolled back on connection
	// teardown. Paired with [metricTxOpened] it derives the number of open
	// transactions (opened − closed); the pair is balanced because every open
	// transaction is accounted closed exactly once on whichever path ends it
	// (see [Session.txClosed]).
	metricTxClosed = "bolt.server.tx.closed"

	// metricTxAbandoned counts explicit transactions that were still open when
	// the connection tore down — an abnormal disconnect (client dropped the
	// socket, read/write error, idle timeout, recovered panic) that never sent
	// COMMIT, ROLLBACK, or RESET. It is the signal that distinguishes an orderly
	// transaction end from a leaked one reclaimed by the teardown rollback
	// (#1309). It is incremented only on the [Session.Close] path, never on the
	// FAILED-transition reclaim (#1312), which is an in-session state change
	// rather than a disconnect. metricTxAbandoned is a strict subset of
	// metricTxClosed: an abandoned transaction is also counted closed.
	metricTxAbandoned = "bolt.server.tx.abandoned"
)

// incCounter forwards to the shared metrics backend. It exists as a single
// package-internal seam so every bolt.server.* emission site reads uniformly
// and a future change to the metrics call convention touches one line.
func incCounter(name string) { metrics.IncCounter(name, 1) }

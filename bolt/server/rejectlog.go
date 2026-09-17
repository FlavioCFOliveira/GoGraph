package server

import (
	"sync/atomic"
	"time"
)

// rejectlog.go — bounding the accept loop's connection-refused log (rmp #2835).
//
// The reject branch of Serve runs on the accept goroutine, which is the one
// resource a connection flood already contends for. Writing one WARN per refusal
// measured 1.670 µs ± 8% per rejection (80 B, 4 allocations), capping the accept
// loop at roughly 599 000 rejections per second and writing 89 bytes of log for
// every connection refused. Under a sustained flood that is not an operational
// signal; it is an amplifier, paid in the one place that can least afford it.
//
// What the operator must not lose is the FACT of the refusals and their COUNT.
// Neither lives in the log line: bolt.server.conn.rejected counts every refusal
// exactly, unsampled, and is untouched by anything here. The log line carries
// the detail — which peer, when — and detail is what a flood makes both
// worthless and expensive. So the line is rate-limited, and every line states
// how many refusals it stands for.
//
// Prior art. Neo4j, PostgreSQL and MariaDB all keep an exact refusal counter and
// none of them bounds the log the way this does: PostgreSQL logs every refusal
// ("sorry, too many clients already"), which it can afford because it forks a
// backend per connection and the log is noise beside the fork; MariaDB logs the
// refusal and exposes Connection_errors_max_connections as the count. GoGraph
// refuses a connection for the price of a channel send, so the log line is the
// dominant cost rather than a rounding error, and the bound is the design that
// fits THIS server rather than the one that fits theirs.
//
// The bound is on TIME, not on a 1-in-N sample. A count-based sample still grows
// linearly with the flood's rate, which is precisely the property that has to go.

// teardownFlushTimeout bounds the final, best-effort flush of a connection's
// response buffer when its handler exits (see handleConn).
//
// It exists because Options.ConnTimeout defaults to zero, which means NO socket
// deadline: without a bound of its own, a teardown flush to a peer that has
// stopped reading would pin the handler goroutine and its MaxConnections
// semaphore slot indefinitely. Two seconds is far longer than a few hundred
// buffered bytes need on any working socket and short enough that a wedged peer
// cannot hold a slot in any way that matters. It is deliberately not
// configurable: it bounds a teardown, it does not express a delivery policy.
const teardownFlushTimeout = 2 * time.Second

// rejectLogInterval is the shortest gap between two connection-refused WARN
// lines from one Server. At one line per second an operator still sees a flood
// begin, still sees it persist, and still sees it end, while the accept loop
// pays the formatting cost once per second instead of once per refusal —
// bounded by wall-clock time rather than by the flooder's rate.
const rejectLogInterval = time.Second

// rejectLogLimiter rate-limits the accept loop's connection-refused log line and
// tallies the refusals it swallows, so the next line that IS written can say how
// many it stands for.
//
// It is safe for concurrent use. One Server runs one accept loop, so the common
// case is a single caller; the atomics keep the type correct without that
// assumption and cost around 4 ns on a path that is refusing a TCP connection.
type rejectLogLimiter struct {
	// lastNanos is the clock reading, in nanoseconds, at which the most recent
	// line was emitted. Its zero value is what makes the FIRST refusal — of the
	// server's life, and of any burst that follows a quiet period longer than
	// the interval — always produce a line.
	lastNanos atomic.Int64
	// suppressed counts the refusals swallowed since the most recent emitted
	// line. It is handed to that line's successor and reset in the same step.
	suppressed atomic.Uint64
}

// allow reports whether this refusal should produce a log line and, when it
// should, how many refusals have gone unlogged since the previous line.
//
// nowNanos is passed in rather than read inside, so that the bound can be tested
// exactly rather than approximately, and so that log rate-limiting never becomes
// a second consumer of the Server's injectable session clock — that clock governs
// the transaction reaper's deadlines and nothing else.
//
// When it returns false the caller must not log and must not treat the refusal
// as lost: it has already been added to the tally and, separately, to
// bolt.server.conn.rejected.
func (l *rejectLogLimiter) allow(nowNanos int64, every time.Duration) (emit bool, suppressed uint64) {
	last := l.lastNanos.Load()
	if nowNanos-last < int64(every) {
		l.suppressed.Add(1)
		return false, 0
	}
	// Claim the slot. A failed swap means a concurrent caller claimed it first
	// and is writing the line; this refusal joins the tally that line's
	// successor will report, so no refusal is ever dropped from the count.
	if !l.lastNanos.CompareAndSwap(last, nowNanos) {
		l.suppressed.Add(1)
		return false, 0
	}
	return true, l.suppressed.Swap(0)
}

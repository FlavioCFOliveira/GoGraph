package server

import (
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// SetClockForTest overrides the server's wall-clock source (used by the
// explicit-transaction timeout reaper) so a test can drive a session timeout by
// virtual time deterministically. It must be called before Serve starts
// accepting connections, since each session captures the clock at construction.
// Production code never sets a non-default clock; only tests do, through this
// seam.
func (s *Server) SetClockForTest(clk clock.Clock) { s.setClock(clk) }

// SetHandshakeTimeoutForTest overrides the package-level handshake deadline and
// returns a function that restores the previous value. It exists solely so the
// external (package server_test) Slowloris regression can drive the
// unauthenticated-handshake reclaim path quickly and deterministically without
// growing the Options struct with a configurable handshake field. Production
// code never mutates handshakeTimeout; only tests do, through this seam.
func SetHandshakeTimeoutForTest(d time.Duration) (restore func()) {
	prev := handshakeTimeout.Load()
	handshakeTimeout.Store(int64(d))
	return func() { handshakeTimeout.Store(prev) }
}

// SetReaderPanicHookForTest installs a hook invoked once at the top of each
// per-connection reader-goroutine read iteration, and returns a function that
// clears it. It exists solely so the reader-panic-boundary regression (#1491)
// can drive a recoverable panic onto the reader goroutine — a panic that is not
// reachable from adversarial bytes today (the read/framing path is panic-free),
// so there is no production seam for it. Production code never reads a non-nil
// value here; only tests install one, through this function.
func SetReaderPanicHookForTest(h func()) (restore func()) {
	prev := readerPanicHookForTest
	readerPanicHookForTest = h
	return func() { readerPanicHookForTest = prev }
}

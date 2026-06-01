package server

import "time"

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

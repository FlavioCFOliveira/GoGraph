//go:build !darwin && !linux

package server_test

import (
	"net"
	"time"
)

// readKeepAliveSockopts reports that this platform's keep-alive socket options
// are not known to the test suite, so the keep-alive gate states that plainly
// instead of asserting something it cannot observe. Add a
// keepalive_sockopt_<goos>_test.go with the three option numbers to enable it.
func readKeepAliveSockopts(*net.TCPConn) (keepAliveSockopts, bool, error) {
	return keepAliveSockopts{}, false, nil
}

// keepAliveSockopts mirrors the darwin/linux declaration so the shared test
// compiles on a platform without a socket-option reader.
type keepAliveSockopts struct {
	Enabled  bool
	Idle     time.Duration
	Interval time.Duration
	Count    int
}

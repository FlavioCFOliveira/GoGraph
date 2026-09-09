//go:build darwin

package server_test

import "syscall"

// The three TCP-level socket options that carry the keep-alive parameters on
// Darwin. syscall names the idle one TCP_KEEPALIVE, where Linux calls it
// TCP_KEEPIDLE; the other two match. Each is read in SECONDS.
const (
	tcpKeepIdleOpt  = syscall.TCP_KEEPALIVE
	tcpKeepIntvlOpt = syscall.TCP_KEEPINTVL
	tcpKeepCntOpt   = syscall.TCP_KEEPCNT
)

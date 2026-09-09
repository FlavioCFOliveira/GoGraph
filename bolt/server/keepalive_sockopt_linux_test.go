//go:build linux

package server_test

import "syscall"

// The three TCP-level socket options that carry the keep-alive parameters on
// Linux. Each is read in SECONDS.
const (
	tcpKeepIdleOpt  = syscall.TCP_KEEPIDLE
	tcpKeepIntvlOpt = syscall.TCP_KEEPINTVL
	tcpKeepCntOpt   = syscall.TCP_KEEPCNT
)

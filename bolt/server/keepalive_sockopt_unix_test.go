//go:build darwin || linux

package server_test

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

// keepAliveSockopts is what the kernel reports for one socket's keep-alive
// configuration, read back through getsockopt. It is the only evidence this
// package accepts that keep-alive is on: the server's own call cannot vouch for
// itself.
type keepAliveSockopts struct {
	// Enabled is SO_KEEPALIVE. The kernel reports the option's BIT, not 1, on
	// some platforms (8 on Darwin), so this is normalised to a bool.
	Enabled bool
	// Idle, Interval and Count are TCP_KEEPIDLE/TCP_KEEPALIVE, TCP_KEEPINTVL and
	// TCP_KEEPCNT. The first two are reported in seconds and converted here.
	Idle     time.Duration
	Interval time.Duration
	Count    int
}

// readKeepAliveSockopts reads the four keep-alive socket options from conn.
//
// The second return reports whether this platform's options are known to the
// test at all; on a platform where they are not, the caller states that
// explicitly rather than asserting nothing.
func readKeepAliveSockopts(conn *net.TCPConn) (keepAliveSockopts, bool, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return keepAliveSockopts{}, true, fmt.Errorf("SyscallConn: %w", err)
	}
	var (
		out  keepAliveSockopts
		serr error
	)
	get := func(fd uintptr, level, opt int) int {
		v, err := syscall.GetsockoptInt(int(fd), level, opt)
		if err != nil && serr == nil {
			serr = fmt.Errorf("getsockopt(level=%d opt=%d): %w", level, opt, err)
		}
		return v
	}
	if cerr := rc.Control(func(fd uintptr) {
		out.Enabled = get(fd, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE) != 0
		out.Idle = time.Duration(get(fd, syscall.IPPROTO_TCP, tcpKeepIdleOpt)) * time.Second
		out.Interval = time.Duration(get(fd, syscall.IPPROTO_TCP, tcpKeepIntvlOpt)) * time.Second
		out.Count = get(fd, syscall.IPPROTO_TCP, tcpKeepCntOpt)
	}); cerr != nil {
		return keepAliveSockopts{}, true, fmt.Errorf("RawConn.Control: %w", cerr)
	}
	return out, true, serr
}

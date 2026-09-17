//go:build darwin

package main

import (
	"encoding/binary"
	"syscall"

	"golang.org/x/sys/unix"
)

// loadavgStructSize is the size of the darwin kernel's struct loadavg as
// vm.loadavg returns it:
//
//	struct loadavg { fixpt_t ldavg[3]; long fscale; };
//
// fixpt_t is a uint32, so the three averages occupy bytes 0..12; fscale is a
// 64-bit long, which the ABI aligns to an 8-byte boundary, so four padding
// bytes precede it and it occupies bytes 16..24.
const (
	loadavgStructSize = 24
	loadavgScaleOff   = 16
)

// readLoadAvg reads the host's 1, 5, and 15-minute load averages from the
// vm.loadavg sysctl.
//
// The sysctl is read directly rather than by running sysctl(8): a laboratory
// that shells out once per run pays a fork and an exec inside the window it is
// timing, and inherits the locale of the operator's shell (this host prints
// "{ 2,69 1,81 2,01 }" under a comma decimal separator, which a naive parse
// would read as six values).
func readLoadAvg() ([3]float64, bool) {
	var out [3]float64
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < loadavgStructSize {
		return out, false
	}
	scale := float64(binary.NativeEndian.Uint64(raw[loadavgScaleOff : loadavgScaleOff+8]))
	if scale == 0 {
		return out, false
	}
	for i := range out {
		out[i] = float64(binary.NativeEndian.Uint32(raw[i*4:i*4+4])) / scale
	}
	return out, true
}

// readFDLimit reports the process's soft and hard RLIMIT_NOFILE.
func readFDLimit() (soft, hard uint64, ok bool) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, 0, false
	}
	return rl.Cur, rl.Max, true
}

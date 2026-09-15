//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readLoadAvg reads the host's 1, 5, and 15-minute load averages from
// /proc/loadavg, whose first three whitespace-separated fields are exactly
// those three values in C locale.
func readLoadAvg() ([3]float64, bool) {
	var out [3]float64
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return out, false
	}
	for i := range out {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return [3]float64{}, false
		}
		out[i] = v
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

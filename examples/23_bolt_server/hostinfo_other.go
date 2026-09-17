//go:build !darwin && !linux

package main

// readLoadAvg reports that this platform's load average is not readable. The
// caller must NOT treat an unread load average as an idle host; [hostInfo.finish]
// records the run as not certified idle instead.
func readLoadAvg() ([3]float64, bool) { return [3]float64{}, false }

// readFDLimit reports that this platform's descriptor ceiling is not readable.
func readFDLimit() (soft, hard uint64, ok bool) { return 0, 0, false }

package main

import (
	"fmt"
	"os"
	"runtime"
)

// idleLoadFraction is the share of the machine's cores that may already be busy
// before a run is still called idle: the 1-minute load average must sit at or
// below NumCPU × idleLoadFraction.
//
// It is a FRACTION OF THE CORES rather than an absolute number because a load
// average of 2 means something entirely different on a 4-core and on a 64-core
// host. One tenth is deliberately strict: this laboratory measures a server
// under connection saturation, where a competing process steals exactly the
// scheduler latency the measurement is trying to attribute.
const idleLoadFraction = 0.10

// hostInfo is the environment record written beside every run's profiles. It
// exists so a number can never be read back without the conditions that
// produced it: the core count it was measured on, the load the machine was
// already carrying, and the descriptor ceiling that bounds how many connections
// could have been opened at all.
type hostInfo struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	GoVersion  string `json:"go_version"`
	NumCPU     int    `json:"num_cpu"`
	GOMAXPROCS int    `json:"gomaxprocs"`

	// LoadAvgBefore and LoadAvgAfter are the 1, 5, and 15-minute load averages
	// read immediately before the first measurement window and immediately
	// after the last. LoadAvgReadable is false on a platform where they could
	// not be read at all, in which case Idle is false and IdleNote says so —
	// an unread load average is never reported as an idle one.
	LoadAvgBefore   []float64 `json:"loadavg_before,omitempty"`
	LoadAvgAfter    []float64 `json:"loadavg_after,omitempty"`
	LoadAvgReadable bool      `json:"loadavg_readable"`

	// IdleThreshold is the 1-minute load average at or below which the host
	// counts as idle (NumCPU × idleLoadFraction). Idle is the verdict for THIS
	// run and IdleNote states how it was reached, in both directions.
	IdleThreshold float64 `json:"idle_threshold"`
	Idle          bool    `json:"idle"`
	IdleNote      string  `json:"idle_note"`

	// FDLimitSoft and FDLimitHard are the process's RLIMIT_NOFILE. The soft
	// limit is the real bound on the connection ladder: this example runs the
	// client and the server in ONE process, so a rung of N connections needs
	// about 2N descriptors plus the runtime's own.
	FDLimitSoft     uint64 `json:"fd_limit_soft,omitempty"`
	FDLimitHard     uint64 `json:"fd_limit_hard,omitempty"`
	FDLimitReadable bool   `json:"fd_limit_readable"`

	// Note records the structural caveat that applies to every number in the
	// run, so it travels with the artefacts rather than living only in a README.
	Note string `json:"note"`
}

// hostNote is the standing caveat on every measurement this example produces.
const hostNote = "client and server run in the same process and share these cores; " +
	"throughput and latency include the client driver's own cost"

// newHostInfo captures the static host facts and the pre-run load average.
// Call [hostInfo.finish] once the last measurement window has closed to add the
// post-run reading and settle the idle verdict.
func newHostInfo() hostInfo {
	h := hostInfo{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		GoVersion:     runtime.Version(),
		NumCPU:        runtime.NumCPU(),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		IdleThreshold: float64(runtime.NumCPU()) * idleLoadFraction,
		Note:          hostNote,
	}
	if la, ok := readLoadAvg(); ok {
		h.LoadAvgBefore = la[:]
		h.LoadAvgReadable = true
	}
	if soft, hard, ok := readFDLimit(); ok {
		h.FDLimitSoft, h.FDLimitHard, h.FDLimitReadable = soft, hard, true
	}
	return h
}

// finish takes the post-run load average and decides whether the run may be
// called idle.
//
// The verdict is taken on the BEFORE reading: the run's own load is what the
// after reading mostly measures, so judging on it would label every successful
// saturation run contaminated by itself. The after reading is recorded anyway,
// because a large rise beyond what the run can explain is how a competing
// process that started mid-run is caught.
func (h *hostInfo) finish() {
	if la, ok := readLoadAvg(); ok {
		h.LoadAvgAfter = la[:]
	}
	switch {
	case !h.LoadAvgReadable:
		h.Idle = false
		h.IdleNote = "load average could not be read on " + runtime.GOOS + "; the host is NOT certified idle"
	case h.LoadAvgBefore[0] <= h.IdleThreshold:
		h.Idle = true
		h.IdleNote = fmt.Sprintf("pre-run loadavg1 %.2f <= threshold %.2f (%d cores x %.2f)",
			h.LoadAvgBefore[0], h.IdleThreshold, h.NumCPU, idleLoadFraction)
	default:
		h.Idle = false
		h.IdleNote = fmt.Sprintf("NOT IDLE: pre-run loadavg1 %.2f > threshold %.2f (%d cores x %.2f)",
			h.LoadAvgBefore[0], h.IdleThreshold, h.NumCPU, idleLoadFraction)
	}
}

// openFDs returns the number of descriptors the process currently holds.
//
// It counts the entries of the kernel's per-process descriptor directory —
// /proc/self/fd on Linux, /dev/fd on macOS and the BSDs. Reading that directory
// itself needs a descriptor, so the count is one higher than the number held
// before the call; it is reported as measured rather than adjusted, because the
// quantity that matters here is the trend across the connection ladder, not an
// absolute to the unit.
//
// The second result is false on a platform that exposes neither directory.
func openFDs() (int, bool) {
	for _, dir := range [...]string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		return len(entries), true
	}
	return 0, false
}

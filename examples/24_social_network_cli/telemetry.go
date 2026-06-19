package main

import (
	"fmt"
	"io"
	"runtime"
	"time"
)

// This file holds the evidence-collection helpers shared by the seed and
// stats subcommands. They implement the "# "-prefixed telemetry convention
// from docs/examples-standard.md: deterministic facts are printed as bare
// lines (and pinned by the regression tests), while volatile telemetry —
// durations, throughput, and live-heap figures that vary per run and per
// machine — is printed only as "# " lines so a test can ignore it.
//
// The helpers are deliberately kept tiny and dependency-free (the same
// shape as example 26's readMem / rate / humanBytes) so the evidence the
// CLI reports is comparable across the example set.

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage. This is
// the same measurement example 26 uses; calling it before and after a
// build isolates the graph's resident footprint from the recovery
// machinery's transient allocations.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval. Used to report build/seed throughput as nodes/s and edges/s.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// writeTelemetry emits a single "# key=value" telemetry line to w. Every
// volatile measurement the CLI reports goes through this helper so the
// "# " prefix and the key=value shape are applied in exactly one place,
// keeping the lines uniformly greppable and uniformly ignorable by the
// regression tests. Write errors are intentionally discarded: telemetry is
// best-effort diagnostic output layered after the deterministic facts have
// already been written, and a broken telemetry write must not mask the
// success of the operation itself.
func writeTelemetry(w io.Writer, key, value string) {
	_, _ = fmt.Fprintf(w, "# %s=%s\n", key, value)
}

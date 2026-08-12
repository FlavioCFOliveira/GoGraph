// Package memlimit resolves the memory this process may actually use, so the
// module's engine-wide ceilings can be derived from the smallest bound that is
// observable rather than from a constant chosen without knowledge of the host.
//
// # Why a library must not simply set GOMEMLIMIT
//
// GOMEMLIMIT is process-global state and belongs to the embedder: a library that
// sets it silently changes the GC behaviour of an application that may have many
// other components. So this package only ever READS a limit, and the module uses
// what it reads to lower its OWN ceilings. Setting GOMEMLIMIT remains the right
// move for a program — the ggserver binary — and the wrong move here.
//
// # Why the constants were not enough (rmp #2421)
//
// The engine-wide ceilings landed by the 2026-08-10 certification are finite, and
// that closed a real denial-of-service hole. But they are FIXED: 4 GiB of results
// and 1 GiB of inbound decode, applied whenever the process has no Go soft memory
// limit to derive from — which is the default state of every Go process. Inside a
// container capped below those numbers the ceiling is larger than the whole
// container, so it cannot bind before the kernel's OOM killer does, and the
// failure mode is a killed process rather than a typed error. That is the
// opposite of the graceful degradation the module promises.
//
// It is not a theoretical concern: a 4M-relationship Cypher load was OOM-killed
// in an 8 GB container on 2026-08-11 ("Killed process (ggserver)
// anon-rss:8370788kB"), and re-running the identical fixture with GOMEMLIMIT=6GiB
// completed at 4520 MB. The cap was the missing input, not the workload.
//
// Memgraph solves the same problem by reading the host: its global memory limit
// defaults to 100% of physical memory with swap and 90% without, via
// utils::sysinfo::InstalledMemory (src/flags/memory_limit.cpp:20-38, v3.9.0). The
// shape adopted here is the same — derive from what is observable — while the
// magnitudes stay this module's own.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// cgroup v2 and v1 paths. Both are read; v2 wins when present, because a host
// running v2 may still mount a v1 hierarchy with a stale or absent value.
const (
	cgroupV2Max = "/sys/fs/cgroup/memory.max"
	cgroupV1Max = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// cgroupV1Unlimited is the sentinel cgroup v1 reports when no limit is set. The
// kernel writes a page-aligned maximum rather than a flag, and the exact value
// depends on the page size, so anything implausibly large is treated as absent
// rather than compared for equality with one constant.
const cgroupV1Unlimited int64 = 1 << 62

var (
	once   sync.Once
	cached int64
	found  bool
	// resolveCalls counts how many times the underlying lookup actually ran. The
	// bound this package reads sits behind a filesystem read, and a filesystem
	// read on a query path would be a defect rather than a cost, so the
	// once-per-process property is asserted by a test rather than promised by a
	// comment.
	resolveCalls atomic.Int64
)

// ResolveCount reports how many times the underlying lookup has run. It exists
// for the test that pins the once-per-process contract.
func ResolveCount() int64 { return resolveCalls.Load() }

// Available returns the smallest observable bound, in bytes, on the memory this
// process may use, and whether one was found at all.
//
// The sources are consulted in order of authority:
//
//  1. the Go soft memory limit, when the embedder has set one (GOMEMLIMIT or
//     [runtime/debug.SetMemoryLimit]) — an explicit statement of intent;
//  2. the cgroup v2 memory.max of the process's own cgroup;
//  3. the cgroup v1 memory.limit_in_bytes.
//
// It deliberately does NOT fall back to installed RAM. A bound this package
// cannot vouch for is worse than no bound: the caller's own finite constant is
// the documented last resort, and reporting false here selects it.
//
// The answer is computed ONCE and cached: it is consulted while constructing an
// engine or a server, never on a query path, and the underlying limit cannot
// change for a running process in the cases that matter.
//
// Safe for concurrent use.
func Available() (int64, bool) {
	once.Do(func() { cached, found = resolve() })
	return cached, found
}

// resolve performs the one-time lookup. Split out so the tests can exercise the
// parsing without the sync.Once.
func resolve() (int64, bool) {
	resolveCalls.Add(1)
	// debug.SetMemoryLimit(-1) is read-only: -1 asks for the current value rather
	// than installing one. An unset limit reads as math.MaxInt64, which is why the
	// upper bound below matters — see [Available].
	if lim := debug.SetMemoryLimit(-1); lim > 0 && lim < cgroupV1Unlimited {
		return lim, true
	}
	if v, ok := readCgroupLimit(cgroupV2Max); ok {
		return v, true
	}
	if v, ok := readCgroupLimit(cgroupV1Max); ok {
		return v, true
	}
	return 0, false
}

// readCgroupLimit parses one cgroup memory-limit file, reporting whether it names
// a real, finite limit.
//
// A missing file is the ordinary case off Linux and outside a container, so it is
// not an error. The literal "max" is cgroup v2's way of spelling "no limit", and
// v1 spells the same thing as an implausibly large integer.
func readCgroupLimit(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v >= cgroupV1Unlimited {
		return 0, false
	}
	return v, true
}

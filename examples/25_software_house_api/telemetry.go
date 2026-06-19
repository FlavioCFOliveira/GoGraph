package main

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
)

// This file holds the evidence-collection machinery the ex-26 examples
// standard mandates (docs/examples-standard.md, Pillar 3). A JSON REST API
// cannot use the "# " bare-line/telemetry convention in its response body, so
// the split is expressed structurally instead: deterministic FACTS (node and
// edge counts) live in their own response fields, and volatile TELEMETRY
// (live heap, per-endpoint latency, request counters) lives in a separate
// "telemetry" object. The example's own startup log on stderr still follows
// the "# " convention for its volatile lines, so a reader sees the same
// fact-vs-telemetry distinction there.
//
// The accumulator is lock-free: every counter is a sync/atomic value, updated
// on the request path without taking any lock, in keeping with the project's
// low-contention hot-path mandate.

// metrics is a lock-free accumulator of volatile per-process telemetry. All
// fields are read and written via sync/atomic, so a handler can record a
// sample without serialising against the store's RWMutex. The values are
// telemetry, never facts: they vary per run and per machine and are never
// pinned by a test.
type metrics struct {
	queryCount     atomic.Int64 // POST /query requests served
	writeCount     atomic.Int64 // of those, writes (exclusive hold)
	statsCount     atomic.Int64 // GET /stats requests served
	lastQueryNanos atomic.Int64 // wall-clock of the most recent /query, ns
	lastStatsNanos atomic.Int64 // wall-clock of the most recent /stats sweep, ns
	maxQueryNanos  atomic.Int64 // slowest /query observed, ns
	seedNanos      atomic.Int64 // wall-clock of the (one) successful seed, ns
}

// recordQuery records one served POST /query: its wall-clock latency and
// whether it was a write. Lock-free.
func (m *metrics) recordQuery(d time.Duration, write bool) {
	m.queryCount.Add(1)
	if write {
		m.writeCount.Add(1)
	}
	ns := d.Nanoseconds()
	m.lastQueryNanos.Store(ns)
	for {
		cur := m.maxQueryNanos.Load()
		if ns <= cur || m.maxQueryNanos.CompareAndSwap(cur, ns) {
			break
		}
	}
}

// recordStats records one served GET /stats sweep and its wall-clock latency.
func (m *metrics) recordStats(d time.Duration) {
	m.statsCount.Add(1)
	m.lastStatsNanos.Store(d.Nanoseconds())
}

// recordSeed records the wall-clock of the single successful seed.
func (m *metrics) recordSeed(d time.Duration) {
	m.seedNanos.Store(d.Nanoseconds())
}

// telemetryBody is the volatile-telemetry half of the GET /stats response. It
// is deliberately separate from statsResponse's deterministic node/edge
// counts: a consumer (or a test) can read the facts and ignore everything
// here, mirroring the "# " telemetry convention of the non-server examples.
// Every field varies per run and per machine.
type telemetryBody struct {
	// Live Go heap after a forced GC, in bytes and human form.
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapAllocHuman string `json:"heap_alloc_human"`
	HeapSysBytes   uint64 `json:"heap_sys_bytes"`
	NumGC          uint32 `json:"num_gc"`

	// Approximate bytes of live heap per stored graph element (node+edge),
	// the structures evidence the standard asks for. 0 when the graph is empty.
	BytesPerElement float64 `json:"bytes_per_element"`

	// Request counters and the latency of the most recent operation of each
	// kind, in milliseconds. The stats latency is the sweep this very response
	// measured.
	QueryCount       int64   `json:"query_count"`
	WriteCount       int64   `json:"write_count"`
	StatsCount       int64   `json:"stats_count"`
	LastQueryMillis  float64 `json:"last_query_ms"`
	MaxQueryMillis   float64 `json:"max_query_ms"`
	StatsSweepMillis float64 `json:"stats_sweep_ms"`
	SeedMillis       float64 `json:"seed_ms"`
}

// snapshotTelemetry reads the current heap and the accumulated counters into a
// telemetryBody. totalElements is the node+edge count the caller already
// computed (the deterministic facts), used only to derive bytes-per-element.
func (m *metrics) snapshotTelemetry(totalElements int64) telemetryBody {
	mem := readMem()
	return telemetryBody{
		HeapAllocBytes:   mem.HeapAlloc,
		HeapAllocHuman:   humanBytes(mem.HeapAlloc),
		HeapSysBytes:     mem.HeapSys,
		NumGC:            mem.NumGC,
		BytesPerElement:  safeDiv(float64(mem.HeapAlloc), float64(totalElements)),
		QueryCount:       m.queryCount.Load(),
		WriteCount:       m.writeCount.Load(),
		StatsCount:       m.statsCount.Load(),
		LastQueryMillis:  millis(m.lastQueryNanos.Load()),
		MaxQueryMillis:   millis(m.maxQueryNanos.Load()),
		StatsSweepMillis: millis(m.lastStatsNanos.Load()),
		SeedMillis:       millis(m.seedNanos.Load()),
	}
}

// millis converts a nanosecond count to fractional milliseconds.
func millis(ns int64) float64 { return float64(ns) / 1e6 }

// readMem returns a memory snapshot after forcing a GC, so HeapAlloc reflects
// live (reachable) bytes rather than floating garbage. Copied from example 26
// per the examples standard, which lists it as a helper worth sharing.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// safeDiv divides a by b, returning 0 when b is 0.
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval. count is a uint64 to take a live node count (lpg.Graph.LiveOrder)
// directly, with no narrowing conversion.
func rate(count uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix. Copied
// from example 26 per the examples standard.
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

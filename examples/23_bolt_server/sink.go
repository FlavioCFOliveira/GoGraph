package main

import (
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// serverCounters are the bolt.server.* counter names the laboratory records.
// They are the ones bolt/server/metrics.go emits; the list is fixed at compile
// time so the sink's map is read-only once built and IncCounter needs no lock
// on the server's hot accept path.
//
// The connection four are the reason this sink exists: bolt.server.conn.rejected
// is the ONLY evidence that the MaxConnections semaphore refused a connection —
// a rejected connection never becomes live, so neither the accepted/closed
// derivation nor any client-side count can reveal it. The transaction seven are
// carried because they cost nothing to collect and a saturation run that also
// leaks transactions should not need a second experiment to show it.
var serverCounters = [...]string{
	"bolt.server.conn.accepted",
	"bolt.server.conn.rejected",
	"bolt.server.conn.closed",
	"bolt.server.conn.panics",
	"bolt.server.tx.opened",
	"bolt.server.tx.closed",
	"bolt.server.tx.abandoned",
	"bolt.server.tx.timedout",
	"bolt.server.tx.idlereaped",
	"bolt.server.tx.quotarejected",
	"bolt.server.tx.terminated",
}

// counterSink is a [metrics.Backend] that records the counters in
// [serverCounters] and discards everything else.
//
// # Why it discards the rest
//
// bolt/server emits a latency sample per inbound Bolt message
// (msgmetrics.go), so a sink that recorded them would add a map lookup and a
// histogram update to every RUN and every PULL — on the very path this
// laboratory measures. ObserveLatency and SetGauge are therefore empty, which
// leaves this sink the same shape as the no-op default it replaces: one
// interface call that returns immediately. IncCounter is reached only on
// connection accept/reject/close and on transaction boundaries, which are
// orders of magnitude rarer than a message.
//
// # Concurrency
//
// Safe for concurrent use by any number of goroutines. The name map is built
// once in newCounterSink and never written again, so concurrent reads need no
// synchronisation; each counter is an atomic.Uint64.
type counterSink struct {
	counters map[string]*atomic.Uint64
}

// newCounterSink builds a sink with one zeroed counter per name in
// [serverCounters].
func newCounterSink() *counterSink {
	m := make(map[string]*atomic.Uint64, len(serverCounters))
	for _, name := range serverCounters {
		m[name] = new(atomic.Uint64)
	}
	return &counterSink{counters: m}
}

// IncCounter records delta against name when name is one of the counters this
// sink carries, and ignores it otherwise.
func (s *counterSink) IncCounter(name string, delta uint64) {
	if c, ok := s.counters[name]; ok {
		c.Add(delta)
	}
}

// ObserveLatency discards the sample. See the type documentation.
func (s *counterSink) ObserveLatency(string, time.Duration) {}

// SetGauge discards the value. See the type documentation.
func (s *counterSink) SetGauge(string, float64) {}

// snapshot reads every counter into a plain map. The read is not atomic across
// counters — a concurrent emission can land between two reads — so a snapshot
// is only exact when taken while the server is quiescent. The laboratory takes
// its snapshots around a measurement window, not inside one, and reports the
// difference (see [diffCounters]).
func (s *counterSink) snapshot() map[string]uint64 {
	out := make(map[string]uint64, len(s.counters))
	for name, c := range s.counters {
		out[name] = c.Load()
	}
	return out
}

// diffCounters returns after − before for every key of after. Counters are
// monotonic, so the difference is the window's own contribution: this is what
// makes per-repetition counter attribution possible in a process where the
// counters themselves can never be reset.
//
// A key missing from before is treated as zero.
func diffCounters(before, after map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(after))
	for name, a := range after {
		out[name] = a - before[name]
	}
	return out
}

// installCounterSink makes sink the process-global metrics backend and returns
// a function that restores the no-op default.
//
// The backend is process-global by design (it is how bolt/server reaches any
// sink at all), so this must be called once, from the goroutine that owns the
// run, and the returned restore must run before the process does anything else
// that might care about the sink.
func installCounterSink(sink *counterSink) func() {
	metrics.SetBackend(sink)
	return func() { metrics.SetBackend(nil) }
}

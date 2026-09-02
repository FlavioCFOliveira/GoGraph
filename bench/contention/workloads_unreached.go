package contention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/generation"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// unreachedWorkloads drive the three module surfaces that rmp #2679 recorded as
// never having been driven directly: graph/generation, the index Manager
// (graph/index/manager.go), and internal/metrics.
//
// Each is driven at its own API rather than through a query, because that is
// the only way the surface's own synchronisation can be attributed: reached
// through Cypher, every one of them sits under the write barrier and its
// contribution is indistinguishable from the barrier's.
func unreachedWorkloads() []Workload {
	return []Workload{
		generationWorkload(),
		indexManagerWorkload(),
		metricsWorkload(),
	}
}

// generationPublishEvery is the reciprocal of the publish fraction: one
// operation in this many publishes a new generation, the rest acquire, read and
// release the current one.
//
// It is 1000, not the 10 the index workloads use, because the package
// documents itself as "the read-mostly equivalent of an MVCC snapshot" and a
// 10% publish rate would measure a workload nothing would ever run. The
// ceiling probe varies it rather than the baseline guessing at it.
const generationPublishEvery = 1000

// generationCSRNodes is the size of the immutable CSR the generation workload
// publishes and reads.
const generationCSRNodes = 20000

// generationWorkload drives [generation.Publisher]: a lock-free
// acquire/read/release read path over an [atomic.Pointer], a per-generation
// refcount, and a publishMu that serialises publishers.
//
// The read path takes no lock at all, so a mutex profile is expected to be
// SILENT here even if the workload scales badly — the refcount is one atomic
// on one cache line shared by every reader, and cache-line coherence is not a
// lock. That asymmetry is the reason this workload exists: a surface can be
// contended without a single blocked nanosecond, and only the scaling column
// can say so.
func generationWorkload() Workload {
	return Workload{
		Name:    "generation-publish-read",
		Surface: "graph/generation Publisher, graph/csr",
		// Measured: 150M operations/s at level 1, so 60M keeps the level-1
		// window near 0.4 s while the contended rungs above it run for
		// seconds. A smaller count made the level-1 window 133 ms, and every
		// scaling ratio in the column divides by that number.
		Ops: 60000000,
		Setup: func(_ string) (Op, func() error, error) {
			c := seedCSR(generationCSRNodes)
			p := generation.New(c)
			op := func(_ context.Context, worker, iter int) error {
				return generationOp(p, c, worker, iter)
			}
			teardown := func() error {
				p.Close()
				return nil
			}
			return op, teardown, nil
		},
	}
}

// generationOp is one generation-publish-read operation. It is a named
// function because the ceiling arm must run the IDENTICAL body against an
// unshared publisher; two copies of it would let the arms drift apart and the
// ratio between them would then measure the drift.
func generationOp[W any](p *generation.Publisher[W], c *csr.CSR[W], worker, iter int) error {
	if (worker+iter)%generationPublishEvery == 0 {
		// Republishing the SAME immutable CSR: the workload is about the
		// publish protocol and the refcount handover, not about building a
		// snapshot.
		if _, err := p.Publish(c); err != nil {
			return fmt.Errorf("generation: publish: %w", err)
		}
		return nil
	}
	g := p.Acquire()
	if g == nil {
		return errors.New("generation: Acquire returned nil")
	}
	// One real read through the held generation, so the acquire is not dead
	// work the compiler could sink.
	n := g.CSR().Order()
	p.Release(g)
	if n == 0 {
		return errors.New("generation: acquired an empty CSR")
	}
	return nil
}

const (
	// indexManagerSubscribers is how many real subscribers the Manager fans
	// each change out to.
	indexManagerSubscribers = 3
	// indexManagerReadEvery is the reciprocal of the Manager-only read
	// fraction: one operation in this many asks the Manager about itself
	// (GetIndex + Count) instead of pushing a change through it.
	indexManagerReadEvery = 10
	// indexManagerDDLEvery is the reciprocal of the DDL fraction: one
	// operation in this many creates and drops an index, taking the
	// Manager's EXCLUSIVE lock. DDL under load is the only way that lock is
	// ever taken while the fan-out is running.
	indexManagerDDLEvery = 2000
	// indexManagerLabels bounds the per-worker label space so the
	// subscribers' postings stay bounded over a long window.
	indexManagerLabels = 16
	// indexManagerNodes bounds the node space each change names.
	indexManagerNodes = 1024
)

// indexManagerWorkload drives [index.Manager], the fan-out point that owns one
// sync.RWMutex over every registered subscriber.
//
// # The subscribers are deliberately made easy
//
// Every change names a label drawn from the CALLING WORKER's own disjoint label
// space, so the label indexes' per-entry locks are as uncontended as this
// harness can make them. That is the experimental design, not an oversight: the
// label index's own synchronisation was already measured (rmp #2685, and the
// per-entry rework at commit df7755fc), and leaving it contended here would
// bury the Manager's shared lock underneath it. What remains shared between
// workers is the Manager's RWMutex and nothing else.
//
// [label.NewNodeIndex] is used as the subscriber because it reacts to
// OpAddNodeLabel with no binding to configure; the hash and B-tree indexes
// ignore every change until a Binding is installed, so an unbound one would
// have made the fan-out vacuous.
func indexManagerWorkload() Workload {
	return Workload{
		Name:    "index-manager-fanout",
		Surface: "graph/index Manager fan-out (one RWMutex), graph/index/label",
		Ops:     8000000,
		Setup: func(_ string) (Op, func() error, error) {
			m := index.NewManager()
			for i := range indexManagerSubscribers {
				if err := m.CreateIndex(fmt.Sprintf("label-%d", i), label.NewNodeIndex()); err != nil {
					return nil, nil, fmt.Errorf("CreateIndex: %w", err)
				}
			}
			op := func(_ context.Context, worker, iter int) error {
				return indexManagerOp(m, worker, iter)
			}
			return op, func() error { return nil }, nil
		},
	}
}

// indexManagerOp is one index-manager-fanout operation. It is a named function
// for the same reason [generationOp] is: the ceiling arm must run the identical
// body against an unshared Manager.
func indexManagerOp(m *index.Manager, worker, iter int) error {
	switch {
	case (worker+iter)%indexManagerDDLEvery == 0:
		// Names are per worker AND per iteration, so two workers never collide
		// on the same name and ErrIndexExists is a real defect rather than an
		// expected race.
		name := fmt.Sprintf("ddl-%d-%d", worker, iter)
		if err := m.CreateIndex(name, label.NewNodeIndex()); err != nil {
			return fmt.Errorf("CreateIndex: %w", err)
		}
		if n := len(m.ListIndexes()); n < indexManagerSubscribers {
			return fmt.Errorf("ListIndexes returned %d, want >= %d", n, indexManagerSubscribers)
		}
		return m.DropIndex(name)
	case (worker+iter)%indexManagerReadEvery == 0:
		if _, err := m.GetIndex("label-0"); err != nil {
			return fmt.Errorf("GetIndex: %w", err)
		}
		if m.Count() < indexManagerSubscribers {
			return fmt.Errorf("Manager.Count() = %d, want >= %d", m.Count(), indexManagerSubscribers)
		}
		return nil
	default:
		// Disjoint per worker: see the doc comment on [indexManagerWorkload].
		lbl := uint32(worker*indexManagerLabels + iter%indexManagerLabels) //nolint:gosec // G115: bounded by the loop indices
		m.Apply(index.Change{
			Op:    index.OpAddNodeLabel,
			Label: lbl,
			Node:  graph.NodeID(uint64(iter % indexManagerNodes)), //nolint:gosec // G115: bounded by indexManagerNodes
		})
		return nil
	}
}

// metricsWorkload drives internal/metrics with a REAL backend installed.
//
// # Driving it as shipped would measure nothing
//
// The default backend is a no-op behind an atomic.Pointer, so every call is a
// pointer load and an empty method. That is worth knowing and is not worth a
// workload. What a consumer actually runs is the Prometheus-compatible backend,
// so the workload installs [prometheus.New] and drives the three Backend
// methods through the package-level entry points every wired call site uses.
//
// # It mutates process-global state
//
// [metrics.SetBackend] is process-global. That is safe here only because the
// observatory runs exactly one workload per child process (see the package
// documentation); the teardown restores the no-op default so an in-process
// caller — a unit test — cannot leak the registry into a later measurement.
//
// # No clock is read
//
// [metrics.Time] would put a time.Now pair inside the measured operation and
// the clock would then dominate the backend. The latency observation is fed a
// synthetic duration instead, so what is measured is the histogram, which is
// what the surface is.
func metricsWorkload() Workload {
	return Workload{
		Name:    "metrics-emit",
		Surface: "internal/metrics, internal/metrics/prometheus registry",
		Ops:     40000000,
		Setup: func(_ string) (Op, func() error, error) {
			reg := prometheus.New()
			metrics.SetBackend(reg)
			op := func(_ context.Context, worker, iter int) error {
				metricsOp("", worker, iter)
				return nil
			}
			teardown := func() error {
				metrics.SetBackend(nil)
				return nil
			}
			return op, teardown, nil
		},
	}
}

const (
	// metricsLatencyEvery and metricsGaugeEvery are the reciprocals of the
	// latency-observation and gauge fractions. Every operation increments the
	// counter; the other two are sampled, because that is the ratio the wired
	// call sites emit at (a latency per call, a counter only on the error
	// path, a gauge only on a reclamation pass).
	metricsLatencyEvery = 4
	metricsGaugeEvery   = 16
)

// metricsOp is one metrics-emit operation. nameSuffix is appended to every
// metric name, which is how the ceiling arm gives each replica its own counter
// through the same shared registry; the base workload passes "".
func metricsOp(nameSuffix string, worker, iter int) {
	metrics.IncCounter("bench.contention.ops"+nameSuffix, 1)
	if (worker+iter)%metricsLatencyEvery == 0 {
		metrics.ObserveLatency("bench.contention.latency"+nameSuffix,
			time.Duration(iter%1000)*time.Microsecond)
	}
	if (worker+iter)%metricsGaugeEvery == 0 {
		metrics.SetGauge("bench.contention.gauge"+nameSuffix, float64(iter))
	}
}

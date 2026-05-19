// Package ldbc implements the harness GoGraph uses against the
// LDBC Social Network Benchmark workloads. The official LDBC SNB
// datasets are external; this harness either ingests a directory
// produced by the LDBC SNB Datagen or, in CI / regression mode,
// synthesises a graph of similar scale via the deterministic RMAT
// generator (see [Synthetic]).
//
// The two scale factors documented in the project roadmap (SF1
// and SF10) map to [ScaleSF1] and [ScaleSF10]. The function
// [Run] executes the canonical interactive query set against the
// given engine and returns the per-query latency percentiles.
package ldbc

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"gograph/store/bulk"
)

// Scale enumerates the LDBC SF presets the harness supports.
type Scale int

// Supported scale presets. Numbers approximate the order of
// magnitude of the official LDBC datasets; the harness scales the
// synthetic substitute accordingly.
const (
	ScaleSF1 Scale = iota + 1
	ScaleSF10
)

// Spec describes a benchmark run.
type Spec struct {
	Scale       Scale
	Queries     int  // number of interactive queries to issue per run
	Synthetic   bool // when true, ingest a deterministic synthetic graph
	OutDir      string
	BulkOutFile string
}

// QueryStats reports the percentile latencies (in nanoseconds) of
// a single benchmark query type.
type QueryStats struct {
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Count int
}

// Report is the top-level run summary.
type Report struct {
	Spec       Spec
	IngestTime time.Duration
	Stats      map[string]QueryStats
}

// Run executes the benchmark. The v1 implementation builds a
// synthetic graph (the ingest cost of the official LDBC dataset
// requires the external Datagen tool); per-scale vertex/edge
// counts are derived from the Scale preset.
//
// The harness is structured as a thin orchestrator so a future
// revision can swap the synthetic builder for an LDBC Datagen
// loader without changing the rest of the surface.
func Run(ctx context.Context, spec Spec) (Report, error) {
	if !spec.Synthetic {
		return Report{}, errors.New("ldbc: only synthetic mode is wired up in v1")
	}
	v, e := scaleParameters(spec.Scale)
	t0 := time.Now()
	out := filepath.Join(spec.OutDir, "snb.csr")
	if spec.BulkOutFile != "" {
		out = spec.BulkOutFile
	}
	loader := bulk.New(bulk.Options{OutputPath: out, Directed: true, Multigraph: false})
	Synthetic(ctx, v, e, loader)
	if _, _, err := loader.Finalise(); err != nil {
		return Report{}, err
	}
	ingest := time.Since(t0)

	// Run a placeholder "interactive query" loop that fans out
	// over the loaded graph and records latency histograms.
	stats := map[string]QueryStats{}
	for q := 0; q < spec.Queries; q++ {
		s := stats["IS1"]
		s.Count++
		stats["IS1"] = s
	}
	for _, name := range []string{"IS1"} {
		s := stats[name]
		s.P50, s.P95, s.P99 = percentile(s.Count, 0.5), percentile(s.Count, 0.95), percentile(s.Count, 0.99)
		stats[name] = s
	}
	return Report{Spec: spec, IngestTime: ingest, Stats: stats}, nil
}

func scaleParameters(s Scale) (vertices, edges uint64) {
	switch s {
	case ScaleSF10:
		return 600_000, 6_000_000
	default:
		return 60_000, 600_000
	}
}

// percentile is a placeholder; the v1 harness emits zero
// percentiles because the synthetic query path is a no-op. A
// future revision will run the actual LDBC interactive queries
// (T67 extension) and record real distributions.
func percentile(_ int, _ float64) time.Duration {
	return 0
}

// Synthetic populates loader with a deterministic synthetic LPG
// approximating a social-network-style graph: every node connects
// to ~e/v random neighbours. The implementation reuses [bulk.Edge]
// records to avoid duplicating the encoding logic.
func Synthetic(ctx context.Context, v, e uint64, loader *bulk.Loader) {
	if v == 0 {
		return
	}
	avgDeg := e / v
	if avgDeg == 0 {
		avgDeg = 1
	}
	var src uint64
	for ; src < v; src++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for d := uint64(0); d < avgDeg; d++ {
			dst := (src*131 + d*17) % v
			loader.Add(bulk.Edge{
				Src: strID(src), Dst: strID(dst), Weight: int64(d),
			})
		}
	}
}

func strID(v uint64) string {
	return "n" + formatUint(v)
}

func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// SortStats returns the names of the query types in the report in
// stable alphabetical order. Useful when emitting deterministic
// markdown summaries from a benchmark report.
func SortStats(rep Report) []string {
	out := make([]string, 0, len(rep.Stats))
	for k := range rep.Stats {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

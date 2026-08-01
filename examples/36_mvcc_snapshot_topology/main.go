// Example 36 — snapshot isolation on the TOPOLOGY dimension.
//
// See README.md for the scenario. In one sentence: this is the instrument that
// checks whether a read observes a structurally consistent graph while the
// graph's structure is being rewritten beneath it, which is the half of MVCC
// that example 27 does not cover (rmp #2293).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// config is the example's tunable surface.
//
// The defaults run in roughly two seconds so the example can run unattended and
// inside `go test -race`, while still committing enough structural writes that a
// reader is overwhelmingly likely to straddle one.
type config struct {
	// spokes is how many edges the ingest commits, one per commit. It is also
	// the final LINK count, because every commit adds exactly one.
	spokes int
	// readers is the size of the pool running the observation query.
	readers int
	// duration bounds the reader pool; the run ends when the ingest finishes or
	// this elapses, whichever comes first.
	duration time.Duration
	// newNodeEvery makes every Nth commit create its spoke node in the SAME
	// transaction as the edge, instead of linking a pre-created one. That
	// exercises the node-birth dimension of visibility — a reader must not see
	// an arc to a node that did not exist at its instant — alongside the
	// edge-birth dimension. 1 means every commit; 0 means none.
	newNodeEvery int
}

func defaultConfig() config {
	return config{
		spokes:       400,
		readers:      4,
		duration:     30 * time.Second,
		newNodeEvery: 2,
	}
}

func (c config) validate() error {
	switch {
	case c.spokes <= 0:
		return errors.New("-spokes must be > 0")
	case c.readers <= 0:
		return errors.New("-readers must be > 0")
	case c.duration <= 0:
		return errors.New("-duration must be > 0")
	case c.newNodeEvery < 0:
		return errors.New("-new-node-every must be >= 0")
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.spokes, "spokes", cfg.spokes, "structural writes the ingest commits, one edge per commit")
	flag.IntVar(&cfg.readers, "readers", cfg.readers, "size of the observing reader pool")
	flag.DurationVar(&cfg.duration, "duration", cfg.duration, "upper bound on the concurrent phase")
	flag.IntVar(&cfg.newNodeEvery, "new-node-every", cfg.newNodeEvery,
		"create the spoke node in the same transaction as the edge every Nth commit (0 = never)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "example 36: %v\n", err)
		os.Exit(1)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The invariant
// ─────────────────────────────────────────────────────────────────────────────

// observation is one reader's bracketed look at the graph's structure.
//
// # Why a BRACKET rather than a fixed expected value
//
// The reader runs concurrently with the ingest, so there is no single correct
// answer to "how many LINK edges are there" — the honest answer is a RANGE, and
// the range is what snapshot isolation actually promises. The reader samples the
// ingest's acknowledged-commit counter before and after its query:
//
//   - lo, sampled BEFORE the query starts. Every one of those commits had
//     already returned to the writer, so each is acknowledged and durable, and a
//     transaction starting later MUST see it. An observation below lo is a
//     committed write made INVISIBLE.
//   - hi, sampled AFTER the query returns. The snapshot began before this
//     sample, so it cannot legitimately contain a write acknowledged after it.
//     An observation above hi is a read of the FUTURE.
//
// Anything inside [lo, hi] is a legal serialisation and is not a finding.
// Writes acknowledged during the query may or may not be visible, and either is
// correct — which is exactly why pinning one expected number would produce false
// failures, and why an earlier draft of this example that counted the same
// pattern TWICE INSIDE ONE QUERY would have found nothing at all: both counts
// are served from one derived snapshot built once per query, so they agree even
// when both are wrong.
type observation struct {
	lo, hi int64
	// links is the LINK count the query returned.
	links int64
	// distinctSpokes is the number of distinct far endpoints the same query saw.
	// The ingest gives every spoke exactly one LINK, so it must equal links.
	// This is the second invariant, and it is not redundant: it catches a
	// derived structure whose per-arc side data has desynchronised from the
	// topology it indexes, which a count alone cannot see.
	distinctSpokes int64
}

// invisibleCommit reports a committed, acknowledged edge the reader could not
// see.
func (o observation) invisibleCommit() bool { return o.links < o.lo }

// futureRead reports an edge the reader saw before it was acknowledged.
func (o observation) futureRead() bool { return o.links > o.hi }

// misalignedFarEndpoints reports a LINK count that disagrees with the number of
// distinct spokes reached.
func (o observation) misalignedFarEndpoints() bool { return o.links != o.distinctSpokes }

func (o observation) violates() bool {
	return o.invisibleCommit() || o.futureRead() || o.misalignedFarEndpoints()
}

// ─────────────────────────────────────────────────────────────────────────────
// Queries
// ─────────────────────────────────────────────────────────────────────────────

// qObserve is the observation query. It is deliberately the shape the defect
// lived in: an anchored, relationship-TYPED expand, which is what drives the
// forward/reverse CSR pair and the per-arc relationship-type filter derived from
// it. An untyped or unanchored variant exercises neither.
const qObserve = `MATCH (:Hub {id: 0})-[r:LINK]->(s:Spoke)
RETURN count(r) AS links, count(DISTINCT s) AS spokes`

// stats is everything the run measures.
type stats struct {
	committed        int64
	observations     int64
	invisibleCommits int64
	futureReads      int64
	misaligned       int64
	// firstViolation is the first offending observation, kept for the error
	// message so a failure is diagnosable without a re-run.
	firstViolation observation
	haveViolation  bool
	elapsed        time.Duration
	readLatencies  []time.Duration
	versionsPeak   int64
	// readErrors counts observation queries that FAILED, which is a distinct
	// finding from one that returned an illegal answer.
	readErrors   int64
	firstReadErr error
}

func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	base := readMem()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	// An index on :Hub(id) so the observation query anchors by seek rather than
	// by label scan. Without it the query's cost is dominated by the scan and
	// the expand — the path under test — is a rounding error.
	if _, err := eng.RunInTx(ctx, "CREATE INDEX FOR (n:Hub) ON (n.id)", nil); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	if err := runWrite(ctx, eng, `CREATE (:Hub {id: 0})`); err != nil {
		return fmt.Errorf("create hub: %w", err)
	}
	// Pre-create the spokes that the ingest will merely LINK to, so that the
	// edge-birth dimension is exercised on its own for those commits: both
	// endpoints already exist, so nothing about node liveness can mask an arc
	// that appeared after a reader's snapshot.
	for i := 0; i < cfg.spokes; i++ {
		if cfg.newNodeEvery > 0 && i%cfg.newNodeEvery == 0 {
			continue // this one is created inside its own commit, below
		}
		if err := runWrite(ctx, eng, fmt.Sprintf(`CREATE (:Spoke {id: %d})`, i)); err != nil {
			return fmt.Errorf("create spoke %d: %w", i, err)
		}
	}

	var (
		committed atomic.Int64
		st        stats
		mu        sync.Mutex // guards st's slices and violation fields
		wg        sync.WaitGroup
	)

	runCtx, stop := context.WithTimeout(ctx, cfg.duration)
	defer stop()

	// ── the observing readers ────────────────────────────────────────────────
	for i := 0; i < cfg.readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for runCtx.Err() == nil {
				lo := committed.Load()
				start := time.Now()
				links, spokes, err := observe(runCtx, eng)
				lat := time.Since(start)
				if err != nil {
					if runCtx.Err() != nil {
						return // shutting down: a cancelled query is not a finding
					}
					// A failed query is its OWN finding, counted separately from
					// an isolation violation. Folding it into haveViolation — as
					// a first draft did — left firstViolation at its zero value
					// and made the run report "first offender saw 0 LINK edges
					// … bracketed by [0, 0]", which is not a violation at all.
					// An instrument that mislabels what it found is worse than
					// one that finds nothing.
					mu.Lock()
					st.readErrors++
					if st.firstReadErr == nil {
						st.firstReadErr = err
					}
					mu.Unlock()
					continue
				}
				// hi AFTER the query: see [observation] for why the bracket is
				// sampled this way round.
				o := observation{lo: lo, hi: committed.Load(), links: links, distinctSpokes: spokes}

				mu.Lock()
				st.observations++
				st.readLatencies = append(st.readLatencies, lat)
				if o.invisibleCommit() {
					st.invisibleCommits++
				}
				if o.futureRead() {
					st.futureReads++
				}
				if o.misalignedFarEndpoints() {
					st.misaligned++
				}
				if o.violates() && !st.haveViolation {
					st.haveViolation, st.firstViolation = true, o
				}
				if v := g.VersionCount(); v > st.versionsPeak {
					st.versionsPeak = v
				}
				mu.Unlock()
			}
		}()
	}

	// ── the ingest: one structural commit at a time ──────────────────────────
	begun := time.Now()
	var ingestErr error
	for i := 0; i < cfg.spokes && runCtx.Err() == nil; i++ {
		var q string
		if cfg.newNodeEvery > 0 && i%cfg.newNodeEvery == 0 {
			// Node AND edge in one transaction: a reader must see both or
			// neither, never an arc to a node it considers unborn.
			q = fmt.Sprintf(
				`MATCH (h:Hub {id: 0}) CREATE (h)-[:LINK]->(:Spoke {id: %d})`, i)
		} else {
			q = fmt.Sprintf(
				`MATCH (h:Hub {id: 0}), (s:Spoke {id: %d}) CREATE (h)-[:LINK]->(s)`, i)
		}
		if err := runWrite(runCtx, eng, q); err != nil {
			if runCtx.Err() != nil {
				break
			}
			ingestErr = fmt.Errorf("ingest commit %d: %w", i, err)
			break
		}
		// AFTER the commit returned, so the counter only ever names writes that
		// are acknowledged. Incrementing before would make lo include a write
		// still in flight, and the bracket would reject correct behaviour.
		committed.Add(1)
	}
	st.elapsed = time.Since(begun)
	stop()
	wg.Wait()
	st.committed = committed.Load()

	if ingestErr != nil {
		return ingestErr
	}
	return report(ctx, w, eng, cfg, &st, &base)
}

// observe runs the observation query and returns its two counts.
func observe(ctx context.Context, eng *cypher.Engine) (links, spokes int64, err error) {
	res, err := eng.Run(ctx, qObserve, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		rec := res.Record()
		l, okL := rec["links"].(expr.IntegerValue)
		s, okS := rec["spokes"].(expr.IntegerValue)
		if !okL || !okS {
			return 0, 0, fmt.Errorf("observe: unexpected column types %T/%T", rec["links"], rec["spokes"])
		}
		links, spokes = int64(l), int64(s)
	}
	if err := res.Err(); err != nil {
		return 0, 0, err
	}
	return links, spokes, nil
}

// report emits the facts and the telemetry, then fails loudly on any violation.
func report(ctx context.Context, w io.Writer, eng *cypher.Engine, cfg config, st *stats, base *runtime.MemStats) error {
	finalLinks, finalSpokes, err := observe(ctx, eng)
	if err != nil {
		return fmt.Errorf("final observation: %w", err)
	}

	// Deterministic facts — pinned by the regression test.
	fmt.Fprintf(w, "config.spokes=%d\n", cfg.spokes)
	fmt.Fprintf(w, "config.readers=%d\n", cfg.readers)
	fmt.Fprintf(w, "config.new_node_every=%d\n", cfg.newNodeEvery)
	fmt.Fprintf(w, "links.committed=%d\n", st.committed)
	fmt.Fprintf(w, "links.final=%d\n", finalLinks)
	// Read-your-writes at the end of the run: every acknowledged commit must be
	// visible to a transaction that starts after all of them.
	fmt.Fprintf(w, "final_read_sees_every_commit=%d\n", b2i(finalLinks == st.committed))
	fmt.Fprintf(w, "final_far_endpoints_align=%d\n", b2i(finalLinks == finalSpokes))
	fmt.Fprintf(w, "invisible_commits=%d\n", st.invisibleCommits)
	fmt.Fprintf(w, "future_reads=%d\n", st.futureReads)
	fmt.Fprintf(w, "misaligned_far_endpoints=%d\n", st.misaligned)
	fmt.Fprintf(w, "read_errors=%d\n", st.readErrors)
	fmt.Fprintf(w, "snapshot_topology_invariant_holds=%d\n", b2i(!st.haveViolation))

	// Volatile telemetry — never pinned.
	after := readMem()
	fmt.Fprintf(w, "# run.elapsed=%s\n", st.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# writer.commits_per_s=%.0f\n", rate(st.committed, st.elapsed))
	fmt.Fprintf(w, "# reader.observations=%d\n", st.observations)
	fmt.Fprintf(w, "# reader.observations_per_s=%.0f\n", rate(st.observations, st.elapsed))
	for _, q := range []struct {
		name string
		p    float64
	}{{"p50", 0.50}, {"p99", 0.99}, {"max", 1.0}} {
		fmt.Fprintf(w, "# reader.%s=%s\n", q.name, percentile(st.readLatencies, q.p).Round(time.Nanosecond))
	}
	fmt.Fprintf(w, "# mvcc.versions_peak=%d\n", st.versionsPeak)
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(after.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(after.HeapAlloc, base.HeapAlloc)))

	// A run that observed nothing proves nothing, and must not report success.
	if st.observations == 0 {
		return errors.New("readers made no observation; the isolation check did not run")
	}
	if st.readErrors > 0 {
		return fmt.Errorf("%d observation quer(ies) FAILED; first error: %w", st.readErrors, st.firstReadErr)
	}
	if st.haveViolation {
		o := st.firstViolation
		return fmt.Errorf("ISOLATION VIOLATION on topology: %d invisible commit(s), %d future read(s), "+
			"%d misaligned far-endpoint count(s) over %d observations; first offender saw %d LINK edges "+
			"and %d distinct spokes with the acknowledged-commit count bracketed by [%d, %d] — "+
			"a read observed a structure no serial schedule produces",
			st.invisibleCommits, st.futureReads, st.misaligned, st.observations,
			o.links, o.distinctSpokes, o.lo, o.hi)
	}
	if finalLinks != st.committed {
		return fmt.Errorf("READ-YOUR-WRITES VIOLATION: %d LINK edges committed and acknowledged, "+
			"a later transaction saw %d", st.committed, finalLinks)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Plumbing
// ─────────────────────────────────────────────────────────────────────────────

// runWrite executes a writing statement and drains it, so the write is applied
// and its commit acknowledged before the call returns.
func runWrite(ctx context.Context, eng *cypher.Engine, q string) error {
	res, err := eng.RunAny(ctx, q, nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
	}
	return res.Err()
}

func readMem() runtime.MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func rate(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / d.Seconds()
}

func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// percentile returns the p-quantile of ds, sorting a COPY so the caller's slice
// keeps its arrival order.
func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	val, exp := float64(b), 0
	for val >= unit && exp < 4 {
		val /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", val, "KMGT"[exp-1])
}

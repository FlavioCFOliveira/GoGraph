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
	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
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
	// duration is a SAFETY NET, not the normal exit. The run ends when the ingest
	// has committed every spoke and the churn phase has collected minChecks
	// self-contradiction observations — both COUNTS, so the run does the same
	// amount of checking on a fast machine and a slow one.
	//
	// It has to be that way round. With a 30 s cap the example passed under -race
	// and FAILED under coverage instrumentation, where a reader's query is slower
	// again: the cap expired before the churn phase collected a single
	// observation, and the run correctly refused to report success on a check it
	// had not performed. A time-bounded instrument measures the machine; a
	// count-bounded one measures the engine.
	duration time.Duration
	// newNodeEvery makes every Nth commit create its spoke node in the SAME
	// transaction as the edge, instead of linking a pre-created one. That
	// exercises the node-birth dimension of visibility — a reader must not see
	// an arc to a node that did not exist at its instant — alongside the
	// edge-birth dimension. 1 means every commit; 0 means none.
	newNodeEvery int
	// churn is how many delete-then-re-add pairs run after the ingest. Each pair
	// leaves the LINK count unchanged, so it does not disturb the bracket, and it
	// is what makes the self-contradiction query detectable at all.
	churn int
	// minChecks is how many self-contradiction observations must complete before
	// the churn phase may end. It exists because the check is the only one that
	// can detect an intra-query instant split, and a run that never performed it
	// must not report success.
	minChecks int
}

func defaultConfig() config {
	return config{
		spokes:       80,
		readers:      4,
		duration:     4 * time.Minute,
		newNodeEvery: 2,
		churn:        60,
		minChecks:    5,
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
	case c.churn < 0:
		return errors.New("-churn must be >= 0")
	case c.minChecks < 1:
		return errors.New("-min-checks must be >= 1: a run that never performs the check proves nothing")
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
	flag.IntVar(&cfg.minChecks, "min-checks", cfg.minChecks,
		"self-contradiction observations that must complete before the churn phase ends")
	flag.IntVar(&cfg.churn, "churn", cfg.churn,
		"delete-then-re-add pairs run after the ingest, which is what makes the self-contradiction query detectable")
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()

	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
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
//   - lo, sampled BEFORE the query starts, from the ACKNOWLEDGED counter. Every
//     one of those commits had already returned to the writer, so each is
//     acknowledged and durable, and a transaction starting later MUST see it. An
//     observation below lo is a committed write made INVISIBLE.
//   - hi, sampled AFTER the query returns, from the STARTED counter. A write the
//     ingest has not even begun cannot be visible to anybody, so it is a true
//     upper bound. An observation above hi is a read of the FUTURE.
//
// # Why hi comes from a SEPARATE counter, and what happened when it did not
//
// An earlier version sampled both ends from the single acknowledged counter, and
// it reported `2 future read(s) over 170 observations` against a CORRECT engine
// during a loaded `make ci` run — while passing 8/8 standalone under -race.
//
// The reason is that publication precedes acknowledgement: the engine makes a
// commit visible, THEN returns from the write, and only then does the ingest
// increment its counter. A reader whose snapshot lands inside that window
// legitimately sees the commit while hi has not yet counted it. Normally the
// window is nanoseconds; under -race on a saturated machine the ingest goroutine
// can be descheduled between the write returning and the increment for
// milliseconds, and the reader's query takes microseconds. So the false positive
// is not merely possible, it is expected under exactly the load a CI run applies.
//
// Sampling hi from a counter incremented BEFORE the write removes the window by
// construction: nothing can be visible that has not been started. The cost is
// that the ceiling is looser by the number of in-flight writes — one, because
// the ingest is sequential — and the invisible-commit floor, which is the half
// that catches a lost commit, is not weakened at all.
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

// qContradiction costs O(spokes²) predicate evaluations — one per expanded row,
// each walking the hub's adjacency — which is why the default -spokes is modest.
// At 200 spokes this query alone took the `go test -coverpkg=./...` gate 188
// seconds, and that gate runs on every change; a correctness check nobody can
// afford to run is not a correctness check. Raise -spokes for a deeper run.
//
// qContradiction is a SELF-CONTRADICTORY query: it expands an arc and then asks a
// pattern predicate whether that same arc exists. At any single instant the
// answer is necessarily zero, so it needs no external oracle at all.
//
// # Why the bracket cannot replace it
//
// The bracket above catches a read that lands OUTSIDE every legal instant —
// below the acknowledged-commit floor, or above the ceiling. It is structurally
// blind to a read of the WRONG legal instant: a clause that answers from the
// PRESENT is still answering from some instant, and the present always sits
// inside [lo, hi] because hi is sampled after the query returns. Measured, not
// assumed: with the pattern predicate reading the present (before rmp #2294) a
// full run of this example reported invisible_commits=0, future_reads=0 and a
// green invariant.
//
// The defect that hides there is INTRA-QUERY inconsistency — the expand
// answering from the snapshot while the predicate in the same query answers from
// the present. Two clauses, two instants, one query. This query makes exactly
// that observable: the expand finds an arc as of the snapshot, and if the
// predicate resolves the same arc at a later instant in which it has been
// DELETED, the NOT holds and the row survives. Which is why the churn phase
// below deletes arcs: with an add-only writer the two instants can never
// disagree in the direction this query detects.
const qContradiction = `MATCH (h:Hub {id: 0})-[r:LINK]->(s:Spoke)
WHERE NOT (h)-[:LINK]->(s)
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
	// contradictionChecks counts runs of qContradiction, and contradictions the
	// rows they returned. Any non-zero count means one query answered its expand
	// and its predicate at two different instants.
	contradictionChecks int64
	contradictions      int64
	haveContradiction   bool
	// churnDeletes counts arcs the churn phase deleted and re-added.
	churnDeletes int64
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
		churning  atomic.Bool
		checksRun atomic.Int64
		committed atomic.Int64
		// started counts writes the ingest has BEGUN. It is the bracket's
		// ceiling; see [observation] for why the acknowledged counter cannot
		// serve that role. Always >= committed.
		started atomic.Int64
		st      stats
		mu      sync.Mutex // guards st's slices and violation fields
		wg      sync.WaitGroup
	)

	runCtx, stop := context.WithTimeout(ctx, cfg.duration)
	defer stop()

	// ── the observing readers ────────────────────────────────────────────────
	for i := 0; i < cfg.readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for runCtx.Err() == nil {
				// The two checks run in SEPARATE phases, and that separation is
				// load-bearing rather than tidy. A first version alternated them
				// throughout and reported invisible_commits=3 against a CORRECT
				// engine: mid-churn, between a delete and its re-add, the true
				// LINK count really is one below the acknowledged-commit count,
				// so the bracket's premise — that the two are equal — does not
				// hold while arcs are being removed. The bracket therefore runs
				// only against the monotone ingest, and the contradiction query
				// only against the churn, which is the phase that can detect it.
				q, contradiction := qObserve, false
				if churning.Load() {
					q, contradiction = qContradiction, true
				}

				lo := committed.Load()
				start := time.Now()
				links, spokes, err := observeWith(runCtx, eng, q)
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
				if contradiction {
					// A self-contradiction needs no bracket: the only correct
					// answer is zero, at every instant.
					mu.Lock()
					st.observations++
					st.contradictionChecks++
					checksRun.Add(1)
					st.readLatencies = append(st.readLatencies, lat)
					if links != 0 {
						st.contradictions += links
						if !st.haveContradiction {
							st.haveContradiction = true
						}
					}
					mu.Unlock()
					continue
				}
				// hi AFTER the query, and from the STARTED counter: see
				// [observation] for why the acknowledged counter cannot serve as
				// the ceiling, and for the false positive that proved it.
				o := observation{lo: lo, hi: started.Load(), links: links, distinctSpokes: spokes}

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
		// BEFORE the write, so the ceiling can never lag a published commit.
		started.Add(1)
		if err := runWrite(runCtx, eng, q); err != nil {
			if runCtx.Err() != nil {
				break
			}
			ingestErr = fmt.Errorf("ingest commit %d: %w", i, err)
			break
		}
		// AFTER the commit returned, so the FLOOR only ever names writes that
		// are acknowledged. Incrementing before would make lo include a write
		// still in flight, and the bracket would reject correct behaviour.
		committed.Add(1)
	}
	// ── the churn phase: DELETE an arc and put it back ───────────────────────
	//
	// The bracket above is satisfied by an add-only writer, but the
	// self-contradiction query is not detectable without deletes: it fires when
	// an arc a read found as of its snapshot has since been removed, so with a
	// monotone writer the two instants can never disagree in that direction.
	//
	// Each pair leaves the committed LINK count as it was at the pair's
	// BOUNDARIES — but not in between, where the arc is genuinely absent while
	// the acknowledged-commit counter still names it. That is why readers switch
	// to the contradiction check for the duration instead of continuing to
	// bracket: the bracket would report a violation the engine did not commit.
	if ingestErr == nil {
		churning.Store(true)
		// The loop keeps churning until enough contradiction checks have actually
		// COMPLETED, and the only bound on that is the context deadline.
		//
		// Both of the obvious alternatives were tried and both measure the machine
		// instead of the engine. A fixed pair count ended the phase after roughly
		// a second of wall-clock; under `go test -coverpkg=./...`, which
		// instruments the whole module, a single reader query takes longer than
		// that, so the window opened and closed between two of them and the run
		// reported zero checks. Capping the extension in WRITER iterations has the
		// same flaw one level up, because it is still a writer-side count standing
		// in for a reader-side event.
		//
		// A reader picks the contradiction query up on its next iteration, so the
		// phase ends a bounded time after the target is met.
		for i := 0; runCtx.Err() == nil &&
			(i < cfg.churn || checksRun.Load() < int64(cfg.minChecks)); i++ {
			id := i % cfg.spokes // cycles, so the phase may run longer than -churn
			del := fmt.Sprintf(
				`MATCH (:Hub {id: 0})-[r:LINK]->(:Spoke {id: %d}) DELETE r`, id)
			add := fmt.Sprintf(
				`MATCH (h:Hub {id: 0}), (s:Spoke {id: %d}) CREATE (h)-[:LINK]->(s)`, id)
			if err := runWrite(runCtx, eng, del); err != nil {
				if runCtx.Err() != nil {
					break
				}
				ingestErr = fmt.Errorf("churn delete %d: %w", id, err)
				break
			}
			if err := runWrite(runCtx, eng, add); err != nil {
				if runCtx.Err() != nil {
					break
				}
				ingestErr = fmt.Errorf("churn re-add %d: %w", id, err)
				break
			}
			st.churnDeletes++
		}
		churning.Store(false)
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
	return observeWith(ctx, eng, qObserve)
}

// observeWith runs q and returns its two counts. Both observation queries return
// the same two columns, so one reader serves both.
func observeWith(ctx context.Context, eng *cypher.Engine, q string) (links, spokes int64, err error) {
	res, err := eng.Run(ctx, q, nil)
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
	fmt.Fprintf(w, "config.churn=%d\n", cfg.churn)
	fmt.Fprintf(w, "config.min_checks=%d\n", cfg.minChecks)
	fmt.Fprintf(w, "contradiction_checks_met=%d\n", b2i(st.contradictionChecks >= int64(cfg.minChecks)))
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
	fmt.Fprintf(w, "intra_query_contradictions=%d\n", st.contradictions)
	// The headline verdict covers BOTH checks. It reported 1 alongside a non-zero
	// contradiction count until this was fixed, because it only ever consulted the
	// bracket — a summary fact that can disagree with the detail below it is worse
	// than no summary at all.
	fmt.Fprintf(w, "snapshot_topology_invariant_holds=%d\n",
		b2i(!st.haveViolation && !st.haveContradiction))

	// Volatile telemetry — never pinned.
	after := readMem()
	fmt.Fprintf(w, "# run.elapsed=%s\n", st.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# writer.commits_per_s=%.0f\n", rate(st.committed, st.elapsed))
	fmt.Fprintf(w, "# reader.observations=%d\n", st.observations)
	fmt.Fprintf(w, "# reader.contradiction_checks=%d\n", st.contradictionChecks)
	fmt.Fprintf(w, "# writer.churn_deletes=%d\n", st.churnDeletes)
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
	if st.contradictionChecks == 0 {
		return errors.New("the self-contradiction query never ran; that half of the check did not happen")
	}
	if st.haveContradiction {
		return fmt.Errorf("INTRA-QUERY INCONSISTENCY: the self-contradiction query returned %d row(s) "+
			"over %d runs — one query expanded an arc as of its snapshot and then asked a pattern "+
			"predicate about that same arc at a DIFFERENT instant, so two clauses of one query "+
			"disagreed about whether the arc exists", st.contradictions, st.contradictionChecks)
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

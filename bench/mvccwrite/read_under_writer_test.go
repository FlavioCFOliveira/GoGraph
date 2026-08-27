package mvccwrite

// read_under_writer_test.go — rmp #2292: what a saturating writer costs a reader,
// measured on an instrument that is not confounded.
//
// # Why BenchmarkEngReadUnderWriter cannot answer this
//
// That benchmark runs readers doing `MATCH (n) RETURN count(n)` — whose cost grows with
// the node count — against an untothrottled writer whose every commit does
// `CREATE (:W {id:...})`, which ADDS a node. The graph size is therefore an
// UNCONTROLLED VARIABLE, and not merely a noisy one: the MVCC work changed it by two
// orders of magnitude, because the writer is no longer starved by the readers. Its
// recorded +39.02% mixes "reads got slower" with "the graph got bigger", in unknown
// proportion, and no optimisation measured against it would be falsifiable.
//
// # What this measures instead
//
// The graph size is FIXED, and the independent variable is the WRITE RATE. The writer
// SETs a property on a node that already exists, so it produces version churn — new
// versions on existing chains, which is exactly the version-walk cost this task is
// about — without changing the node count. The reader's scan cost is therefore constant
// across every arm, and any movement in read latency is attributable to concurrent
// writing rather than to graph growth.
//
// The zero-writer arm is the baseline, and it is the reason no pre-MVCC worktree is
// needed to reach a verdict: if read cost is flat in write rate, there is no material
// version-walk cost to reduce, whatever an absolute comparison against a pre-MVCC
// commit would say about other changes made since.

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// readUnderWriterNodes is the fixed population. Large enough that a scan is not
// dominated by fixed query overhead, small enough that an arm completes quickly.
const readUnderWriterNodes = 2000

// seedFixedPopulation creates the invariant node population and returns nothing that
// varies between arms.
func seedFixedPopulation(tb testing.TB, eng *cypher.Engine) {
	tb.Helper()
	ctx := context.Background()
	// An INDEX on :Acct(id) so the writer's MATCH is a SEEK, not a scan. The id is an
	// INTEGER, so the seek is the RANGE path and not the hash one: a Cypher CREATE
	// INDEX builds a STRING-keyed hash index and tryNewHashSeek declines a non-string
	// seek against it. That matters because the range path is population-gated — it was
	// suppressed below 1024 nodes until #2367 lowered the floor to 64 — so
	// readUnderWriterNodes must stay above the floor or the writer silently becomes
	// O(population) per commit again. Without it the
	// writer's own query costs O(readUnderWriterNodes) per commit, which made a previous
	// version of this benchmark confounded in a second way: the arms differed in total
	// CPU demand as well as in write rate, and a CPU profile showed ~39% of all time in
	// the writers' Filter/newRowPredicate path rather than in anything MVCC. The reader
	// is a full label scan by design — that is the workload — but the WRITER has to be
	// constant-cost or the independent variable is not the write rate.
	if _, err := eng.RunInTx(ctx, "CREATE INDEX acct_id FOR (n:Acct) ON (n.id)", nil); err != nil {
		tb.Fatalf("CREATE INDEX: %v", err)
	}
	for i := 0; i < readUnderWriterNodes; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id, bal: 0})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); err != nil {
			tb.Fatalf("seed %d: %v", i, err)
		}
	}
}

// BenchmarkReadUnderConstantSizeWriter measures read latency against a rising write
// rate with the node count held constant.
//
// Reported per arm: the reader's ns/op (the benchmark's own metric), plus the writes
// the background writers actually landed, so a reader-latency figure can never be read
// without knowing how much writing produced it — the confound that made the previous
// instrument unusable.
func BenchmarkReadUnderConstantSizeWriter(b *testing.B) {
	for _, writers := range []int{0, 1, 2, 4, 8} {
		b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
			r := newRig(b, wiringMem)
			defer func() { _ = r.close() }()
			seedFixedPopulation(b, r.eng)

			ctx := context.Background()
			var (
				stop    = make(chan struct{})
				wg      sync.WaitGroup
				written atomic.Int64
			)
			// Writers SET an existing node's property: version churn on live chains,
			// with the node count invariant.
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						// Disjoint node per writer, so the writers do not conflict with
						// each other -- this measures what writing costs a READER, not
						// what writers cost each other (that is #2323).
						id := int64((w*97 + i) % readUnderWriterNodes)
						if _, err := r.eng.RunInTx(ctx,
							"MATCH (n:Acct {id: $id}) SET n.bal = $v",
							map[string]expr.Value{
								"id": expr.IntegerValue(id),
								"v":  expr.IntegerValue(int64(i)),
							}); err == nil {
							written.Add(1)
						}
					}
				}(w)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				res, err := r.eng.Run(ctx, "MATCH (n:Acct) RETURN count(n) AS c", nil)
				if err != nil {
					b.Fatalf("read: %v", err)
				}
				drain(b, res)
			}
			b.StopTimer()

			close(stop)
			wg.Wait()
			b.ReportMetric(float64(written.Load()), "writes")
			if writers > 0 && written.Load() == 0 {
				b.Fatalf("%d writers landed ZERO writes: the arm measured an idle graph and "+
					"its read figure is not a figure for a contended one", writers)
			}
		})
	}
}

// drain consumes a result set so the read is actually performed rather than merely
// planned.
func drain(b *testing.B, res *cypher.Result) {
	b.Helper()
	for res.Next() {
		_ = res.Record()
	}
	if err := res.Err(); err != nil {
		b.Fatalf("drain: %v", err)
	}
}

// realisticWriteRate is the per-writer commit rate the AC 4 bound is measured at, in
// commits per second.
//
// The saturating arms above answer "what does a writer running flat out cost a reader",
// which is the worst case and the wrong question for a production bound: at 232.9k
// commits/s a single saturating writer is doing more writing than any OLTP workload this
// module targets. 1000 commits/s per writer is a rate a real service might sustain, and
// it is ~230x below saturation, so it separates "MVCC costs reads something structural"
// from "a CPU-bound writer competes for cores".
const realisticWriteRate = 1000

// BenchmarkReadAtRealisticWriteRate measures read cost against a THROTTLED writer, which
// is what rmp #2292's 2.5% bound is about.
//
// Throttling is by sleep between commits rather than by a token bucket: the writer is not
// the thing being measured, so the simplest mechanism that produces a stable rate is the
// right one. The achieved rate is reported so the bound is never read without evidence
// that the intended rate was actually delivered.
func BenchmarkReadAtRealisticWriteRate(b *testing.B) {
	for _, writers := range []int{0, 1, 4} {
		b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
			r := newRig(b, wiringMem)
			defer func() { _ = r.close() }()
			seedFixedPopulation(b, r.eng)

			ctx := context.Background()
			var (
				stop    = make(chan struct{})
				wg      sync.WaitGroup
				written atomic.Int64
			)
			interval := time.Second / realisticWriteRate
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					tick := time.NewTicker(interval)
					defer tick.Stop()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						case <-tick.C:
						}
						id := int64((w*97 + i) % readUnderWriterNodes)
						if _, err := r.eng.RunInTx(ctx,
							"MATCH (n:Acct {id: $id}) SET n.bal = $v",
							map[string]expr.Value{
								"id": expr.IntegerValue(id),
								"v":  expr.IntegerValue(int64(i)),
							}); err == nil {
							written.Add(1)
						}
					}
				}(w)
			}

			start := time.Now()
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				res, err := r.eng.Run(ctx, "MATCH (n:Acct) RETURN count(n) AS c", nil)
				if err != nil {
					b.Fatalf("read: %v", err)
				}
				drain(b, res)
			}
			b.StopTimer()
			elapsed := time.Since(start)

			close(stop)
			wg.Wait()
			got := written.Load()
			b.ReportMetric(float64(got), "writes")
			if writers > 0 {
				rate := float64(got) / elapsed.Seconds()
				b.ReportMetric(rate, "writes/s")
				// The bound is meaningless if the throttle did not hold. Allow generous
				// slack for scheduling, but reject an arm that silently saturated.
				if ceiling := float64(writers*realisticWriteRate) * 2; rate > ceiling {
					b.Fatalf("the throttle did not hold: %0.f writes/s against an intended "+
						"%d, so this arm is a saturating one and its figure cannot be read "+
						"as a realistic-rate bound", rate, writers*realisticWriteRate)
				}
			}
		})
	}
}

// BenchmarkIndexedReadAtRealisticWriteRate is the arm that decides whether the AC 4
// figure is a FIXED per-read overhead or a PER-ROW one.
//
// The sibling benchmark's reader is a full label scan costing ~68 us over 2000 nodes. A
// realistic OLTP reader is an indexed point lookup costing a few microseconds. The two
// respond oppositely to the two candidate mechanisms:
//
//   - a FIXED per-read cost (a snapshot acquisition, a horizon slot, a filter decision
//     taken once per query) is a constant number of microseconds, so it is a SMALL
//     percentage of a 68 us scan and a LARGE percentage of a 3 us seek;
//   - a PER-ROW cost (a visibility test per row, a version walk per node touched) scales
//     with rows examined, so it is a similar percentage of both — and near-zero on a seek
//     that touches one row.
//
// Comparing this arm's percentage against the scan arm's therefore separates the two
// without needing to profile either.
func BenchmarkIndexedReadAtRealisticWriteRate(b *testing.B) {
	for _, writers := range []int{0, 1} {
		b.Run("writers="+strconv.Itoa(writers), func(b *testing.B) {
			r := newRig(b, wiringMem)
			defer func() { _ = r.close() }()
			seedFixedPopulation(b, r.eng)

			ctx := context.Background()
			var (
				stop    = make(chan struct{})
				wg      sync.WaitGroup
				written atomic.Int64
			)
			interval := time.Second / realisticWriteRate
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					tick := time.NewTicker(interval)
					defer tick.Stop()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						case <-tick.C:
						}
						// Writers touch the TOP half of the id space and the reader the
						// bottom, so the reader never seeks a node a writer is versioning:
						// this isolates the cost of writing happening AT ALL from the cost
						// of reading a contended row.
						id := int64(readUnderWriterNodes/2 + (w*97+i)%(readUnderWriterNodes/2))
						if _, err := r.eng.RunInTx(ctx,
							"MATCH (n:Acct {id: $id}) SET n.bal = $v",
							map[string]expr.Value{
								"id": expr.IntegerValue(id),
								"v":  expr.IntegerValue(int64(i)),
							}); err == nil {
							written.Add(1)
						}
					}
				}(w)
			}

			start := time.Now()
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// An indexed point lookup in the bottom half of the id space.
				res, err := r.eng.Run(ctx, "MATCH (n:Acct {id: $id}) RETURN n.bal AS b",
					map[string]expr.Value{
						"id": expr.IntegerValue(int64(i % (readUnderWriterNodes / 2))),
					})
				if err != nil {
					b.Fatalf("read: %v", err)
				}
				drain(b, res)
			}
			b.StopTimer()
			elapsed := time.Since(start)

			close(stop)
			wg.Wait()
			got := written.Load()
			b.ReportMetric(float64(got), "writes")
			if writers > 0 {
				rate := float64(got) / elapsed.Seconds()
				b.ReportMetric(rate, "writes/s")
				if ceiling := float64(writers*realisticWriteRate) * 2; rate > ceiling {
					b.Fatalf("the throttle did not hold: %0.f writes/s against an intended %d",
						rate, writers*realisticWriteRate)
				}
			}
		})
	}
}

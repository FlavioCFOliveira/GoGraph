package mvccwrite

// unique_scaling_test.go — write scaling under a UNIQUE constraint (rmp #2366, #2424).
//
// # Why this exists beside BenchmarkWriteScaling
//
// [BenchmarkWriteScaling] measures a schema that declares NOTHING, which is the right
// control: it shows what the constraint machinery costs a workload that has no
// constraints. It cannot show what it costs a workload that HAS one, and rmp #2366
// changed exactly that path — a UNIQUE release is now recorded on the transaction and
// applied at commit, instead of taking the constraint registry's global write lock
// inline on every release.
//
// That registry's lock is on record as 57 % of ALL lock delay at sixteen writers on a
// schema with no constraints at all (cypher/exec/label_constraints.go), so a change to
// how often it is taken is a contention change and has to be measured as one.
//
// # The workload, and what it deliberately does NOT scale with
//
// Every writer owns its own pool of nodes and moves each one's constrained property to
// a fresh value, which drives BOTH directions per commit — release the old value,
// reserve the new one — and that is the path the fix changed. A create-only workload
// would exercise the reserve alone and miss it.
//
// The pool is PER WRITER and of constant size, so the population the MATCH resolves
// against does not grow with the writer count. A setup that scaled with the independent
// variable is how a previous sweep manufactured a 0.145x "collapse" that was really an
// unindexed label scan getting bigger with its peers; the tell was allocs/op climbing
// with the writer count, which cannot happen for a per-op metric. Here the UNIQUE
// constraint supplies a backing hash index on the constrained property, and the seek key
// IS that property, so every arm seeks rather than scans. See [updateUnique] for what
// happened when it was not.
//
// Values are globally distinct across writers, so the arms never collide on the
// constraint: a colliding workload would measure retry rather than throughput.

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// uniquePoolPerWriter is how many nodes each writer cycles its updates over. Small
// enough that the whole pool stays hot, large enough that consecutive updates do not
// all land on one node and serialise on its version chain.
const uniquePoolPerWriter = 64

// seedUniqueSchema declares the constraint and the per-writer pools.
//
// It is charged to the benchmark's SETUP, not its timer: the pools exist so the timed
// unit is a property update on a node that already holds a constrained value, which is
// the shape that exercises both the release and the reserve.
func seedUniqueSchema(tb testing.TB, eng *cypher.Engine, writers int) {
	tb.Helper()
	ctx := context.Background()
	run := func(q string, params map[string]expr.Value) {
		tb.Helper()
		res, err := eng.RunInTx(ctx, q, params)
		if err != nil {
			tb.Fatalf("%s: %v", q, err)
		}
		if err := res.Err(); err != nil {
			_ = res.Close()
			tb.Fatalf("%s: %v", q, err)
		}
		if err := res.Close(); err != nil {
			tb.Fatalf("%s: close: %v", q, err)
		}
	}
	run(`CREATE CONSTRAINT acct_email_uq ON (n:Acct) ASSERT n.email IS UNIQUE`, nil)
	// PROVE THE CONSTRAINT IS LIVE before measuring anything through it. Without this
	// the whole benchmark would silently degrade into an ordinary property update if
	// the DDL ever stopped registering, and it would still produce a plausible number —
	// which is the failure mode a fixture check exists to prevent.
	res, err := eng.RunInTx(ctx, `CREATE (n:Acct {k: 'probe1', email: 'probe@x'})`, nil)
	if err == nil {
		if err = res.Err(); err == nil {
			_ = res.Close()
		}
	}
	if err != nil {
		tb.Fatalf("seeding the constraint probe failed: %v", err)
	}
	dup, dupErr := eng.RunInTx(ctx, `CREATE (n:Acct {k: 'probe2', email: 'probe@x'})`, nil)
	if dupErr == nil {
		dupErr = dup.Err()
		_ = dup.Close()
	}
	if dupErr == nil {
		tb.Fatal("a duplicate constrained value was ACCEPTED: the UNIQUE constraint is not " +
			"in force, so this benchmark would measure an unconstrained workload")
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < uniquePoolPerWriter; i++ {
			run(`CREATE (n:Acct {k: $k, email: $email})`, map[string]expr.Value{
				"k":     expr.StringValue(uniqueKey(w, i)),
				"email": expr.StringValue(uniqueValue(w, i, 0)),
			})
		}
	}
}

// uniqueKey identifies one writer's pool member.
func uniqueKey(writer, i int) string {
	return "w" + strconv.Itoa(writer) + "n" + strconv.Itoa(i)
}

// uniqueValue is the constrained value, globally distinct by construction so the arms
// never contend on the constraint itself.
func uniqueValue(writer, i, round int) string {
	return "w" + strconv.Itoa(writer) + "i" + strconv.Itoa(i) + "r" + strconv.Itoa(round) + "@x"
}

// updateUnique is the timed unit: one autocommit transaction that moves one node's
// constrained value to a fresh one, releasing the old and reserving the new.
//
// # It seeks by the CONSTRAINED property, and that is the whole fixture
//
// The node is found by `email`, which the UNIQUE constraint backs with a hash index, so
// the lookup is a SEEK whose cost does not depend on how many nodes exist. Finding it
// by `k` instead — which nothing indexes — makes it a LABEL SCAN over a population of
// 64 x writers, and the benchmark then reports the scan growing rather than the write
// path contending: measured that way, ns/commit rose 41 us to 131 us from 1 to 32
// writers and the "scaling" looked like a 0.32x collapse. A per-operation cost that
// moves with the peer count is a fixture bug every time.
//
// The round number is derived, not stored: iteration i touches pool member i%pool for
// the (i/pool)-th time, so the value it currently holds is computable and every update
// is a genuine release-and-reserve rather than a no-op the equality test would skip.
func updateUnique(ctx context.Context, eng *cypher.Engine, writer, i int) error {
	j, round := i%uniquePoolPerWriter, i/uniquePoolPerWriter
	res, err := eng.RunInTx(ctx, `MATCH (n:Acct {email: $old}) SET n.email = $new`,
		map[string]expr.Value{
			"old": expr.StringValue(uniqueValue(writer, j, round)),
			"new": expr.StringValue(uniqueValue(writer, j, round+1)),
		})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// BenchmarkWriteScalingUnique reports commits/s and ns/commit for a constrained
// property update at each writer count, so a change to the constraint registry's
// locking can be compared against the unconstrained control in
// [BenchmarkWriteScaling].
//
// Memory wiring only: the WAL's fsync dominates its own arm and would mask the
// registry, which is the quantity under measurement here.
func BenchmarkWriteScalingUnique(b *testing.B) {
	for _, writers := range scalingWriters {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			r := newRig(b, wiringMem)
			defer func() {
				if err := r.close(); err != nil {
					b.Errorf("close rig: %v", err)
				}
			}()
			seedUniqueSchema(b, r.eng, writers)
			ctx := context.Background()
			warmUp(b, r.eng)

			perWriter := (b.N + writers - 1) / writers
			b.ResetTimer()
			got, err := runArm(writers, perWriter, func(writer, i int) error {
				return updateUnique(ctx, r.eng, writer, i)
			})
			b.StopTimer()
			if err != nil {
				b.Fatalf("writers=%d: %v", writers, err)
			}
			if got.commits == 0 {
				b.Fatal("no commit landed, so this arm measures nothing")
			}
			b.ReportMetric(float64(got.commits)/got.elapsed.Seconds(), "commits/s")
			b.ReportMetric(float64(got.elapsed.Nanoseconds())/float64(got.commits), "ns/commit")
		})
	}
}

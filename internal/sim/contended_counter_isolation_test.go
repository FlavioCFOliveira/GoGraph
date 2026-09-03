package sim

// contended_counter_isolation_test.go — regression gate for rmp #2729.
//
// The contended transactional role collides its connections on a small shared
// counter key space, and the zero-lost-updates oracle — each counter's value at
// quiescence equals the increments THIS call acknowledged — is what proves the
// engine refused every lost update.
//
// The counters were named by a FIXED string, `mv-wire-counter-<k>`, with no
// component identifying the run. That is the same shape rmp #2728 removed from
// the Bolt parameter probe, and it breaks the same two things whenever two
// [RunConcurrent] calls drive ONE shared [SimServer] at the same time:
//
//   - SEEDING. [seedContendedCounter] is a check-then-CREATE over two separate
//     autocommit statements with no cross-caller atomicity. Two callers that
//     both read count 0 both CREATE, so two nodes answer the same
//     `MATCH (n:Person {name:$name})`. The read half of every subsequent
//     read-modify-write then returns two rows, fails its single-row contract,
//     and the contended role stops exercising contention at all — silently,
//     because a failed transaction is an accounted outcome.
//
//   - TRUTH. Even with the seeding won by one caller, every caller's increments
//     land in the SAME counter while each compares that counter against only its
//     OWN acknowledged tally. Measured with the counters pre-seeded so no
//     duplicate exists, four runs sharing one namespace got 8 of 8 counter
//     comparisons wrong: each read final=[13 13] against its own acknowledged
//     [3 4], [3 6], [4 0] and [3 2]. The counters held the exact sum, so the
//     engine had lost nothing — the harness simply attributed every other run's
//     increments to each run in turn, and would have reported four lost-update
//     violations that never happened.
//
// The defect was latent at HEAD: the only caller that runs many [RunConcurrent]
// calls concurrently against one server (bench/contention's dst-concurrent-bolt)
// passes no Mix, and [defaultConcurrentMix] leaves TxContendedWeight zero, so
// the seeding loop never runs there. This file removes the latency by driving
// exactly that combination, so the shape cannot come back unnoticed.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// Size of the concurrent-callers gate. Sized to the base rate of the event, not
// for spectacle: on the shared fixture ANY two overlapping calls that each
// acknowledge one increment already make both their oracles false, so six
// callers with four connections each makes a miss essentially impossible while
// keeping the test inside the short layer's budget.
const (
	contendedIsolationCallers    = 6
	contendedIsolationConns      = 4
	contendedIsolationOps        = 8
	contendedIsolationCounters   = 2
	contendedIsolationSeedStride = 0x9E3779B97F4A7C15
)

// TestRunConcurrent_ContendedCountersArePrivatePerRun runs several
// [RunConcurrent] calls at once against ONE shared server, every one of them
// with a contended population, and requires each call's zero-lost-updates
// oracle to hold on its own evidence.
//
// This is the regression gate for rmp #2729. It FAILS on the fixed-name counter
// fixture and passes only while each call's counter key space is private to it.
//
// It asserts something WAS seen before it asserts the oracle: a run in which no
// connection drew the contended role, or in which no increment was ever
// acknowledged, proves nothing about lost updates and is failed as vacuous
// rather than passed as clean.
func TestRunConcurrent_ContendedCountersArePrivatePerRun(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	type callResult struct {
		res ConcurrentResult
		err error
	}
	results := make([]callResult, contendedIsolationCallers)

	// A start barrier, so the calls genuinely overlap instead of queueing behind
	// each other's setup. Without it the first caller can finish its whole run
	// before the last one seeds, and the sharing would go unobserved.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < contendedIsolationCallers; i++ {
		wg.Add(1)
		go func(call int) {
			defer wg.Done()
			<-start
			res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
				// Distinct per call, as every caller driving one shared server
				// must be: two calls replaying the same stream would collide on
				// their marker names too, not only on their counters.
				Seed:              uint64(call+1) * contendedIsolationSeedStride,
				Connections:       contendedIsolationConns,
				OpsPerConn:        contendedIsolationOps,
				ContendedCounters: contendedIsolationCounters,
				// Every connection contended: the role under test is the whole
				// population, so the draw cannot leave the case unexercised.
				Mix: &ConcurrentMix{TxContendedWeight: 1.0},
			})
			results[call] = callResult{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	// Non-vacuity, counted across the whole battery: increments really were
	// acknowledged, so the equality below had something to be wrong about.
	var (
		totalAcked   int64
		totalChecks  int
		contendedRun int
		tx           struct{ issued, committed, conflicted, failed int64 }
	)
	for call := range results {
		r := &results[call]
		if r.err != nil {
			t.Fatalf("call %d: RunConcurrent: %v", call, r.err)
		}
		if r.res.Panics != 0 || r.res.TransportErrors != 0 {
			t.Errorf("call %d: panics=%d transportErrors=%d (want 0,0)",
				call, r.res.Panics, r.res.TransportErrors)
		}
		tx.issued += r.res.TxIssued
		tx.committed += r.res.TxCommitted
		tx.conflicted += r.res.TxConflicted
		tx.failed += r.res.TxFailed
		if r.res.ContendedConnections == 0 {
			t.Errorf("call %d: no connection drew the contended role under a mix that selects"+
				" nothing else — the run does not exercise the case", call)
			continue
		}
		contendedRun++
		if len(r.res.ContendedFinal) != len(r.res.ContendedAcked) {
			t.Fatalf("call %d: %d acknowledged tallies against %d finals",
				call, len(r.res.ContendedAcked), len(r.res.ContendedFinal))
		}
		for k := range r.res.ContendedAcked {
			totalAcked += r.res.ContendedAcked[k]
			totalChecks++
			if r.res.ContendedFinal[k] != r.res.ContendedAcked[k] {
				t.Errorf("call %d counter %d: final=%d, this call acknowledged %d —"+
					" the counter key space is shared with another concurrent call (rmp #2729)",
					call, k, r.res.ContendedFinal[k], r.res.ContendedAcked[k])
			}
		}
	}

	if contendedRun != contendedIsolationCallers {
		t.Fatalf("%d of %d calls exercised the contended role", contendedRun, contendedIsolationCallers)
	}
	if totalChecks == 0 {
		t.Fatal("the oracle made no observation at all — this gate proved nothing")
	}
	if totalAcked == 0 {
		t.Fatalf("no contended increment was acknowledged across the whole battery"+
			" (tx issued=%d committed=%d conflicted=%d failed=%d): every counter reads its"+
			" acknowledged total trivially, so the equality above is vacuous. A shared counter"+
			" name is answered by more than one node, which fails the single-row contract of"+
			" the read half of every read-modify-write and retires the role (rmp #2729)",
			tx.issued, tx.committed, tx.conflicted, tx.failed)
	}
	t.Logf("%d concurrent calls, %d counter comparisons, %d acknowledged increments;"+
		" tx issued=%d committed=%d conflicted=%d failed=%d",
		contendedIsolationCallers, totalChecks, totalAcked,
		tx.issued, tx.committed, tx.conflicted, tx.failed)
}

// TestSeedContendedCounter_ConcurrentSeedingLeavesOneNode isolates the seeding
// half of rmp #2729 from the oracle half.
//
// [seedContendedCounter] is a check-then-CREATE across two autocommit
// statements. Concurrent callers that share a counter name both observe count 0
// and both CREATE, and the duplicate is invisible to every later assertion
// except the single-row contract of the read half. This test looks at the node
// count directly, so it names the cause rather than one of its symptoms.
func TestSeedContendedCounter_ConcurrentSeedingLeavesOneNode(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	const (
		seeders  = 8
		counters = 2
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, seeders)
	for i := 0; i < seeders; i++ {
		wg.Add(1)
		go func(call int) {
			defer wg.Done()
			<-start
			res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
				Seed:              uint64(call+1) * contendedIsolationSeedStride,
				Connections:       1,
				OpsPerConn:        1,
				ContendedCounters: counters,
				Mix:               &ConcurrentMix{TxContendedWeight: 1.0},
			})
			if err != nil {
				errs[call] = err
				return
			}
			if res.ContendedConnections == 0 {
				errs[call] = fmt.Errorf("call %d drew no contended connection, so it seeded nothing", call)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("seeding call: %v", err)
		}
	}

	// Every counter name that exists must name exactly one node. A name answered
	// by two nodes breaks the single-row contract of the read half of every
	// read-modify-write, which silently retires the contended role.
	names, err := contendedCounterNamesInEngine(srv)
	if err != nil {
		t.Fatalf("counter census: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no counter node exists after 8 contended runs — the census found nothing to check")
	}
	for name, n := range names {
		if n != 1 {
			t.Errorf("counter %q is answered by %d nodes; concurrent check-then-CREATE seeded it"+
				" more than once (rmp #2729)", name, n)
		}
	}
	t.Logf("%d distinct counter name(s) after %d concurrent seeding calls", len(names), seeders)
}

// contendedCounterNamesInEngine returns, for every node whose name looks like a
// contended counter, how many nodes carry that exact name. It reads over the
// wire like every other oracle in this package, so it sees what a client sees.
func contendedCounterNamesInEngine(srv *SimServer) (map[string]int, error) {
	c, err := srv.Dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		return nil, err
	}
	recs, err := wireQuery(c,
		"MATCH (n:Person) WHERE n.val IS NOT NULL RETURN n.name AS name", nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(recs))
	for _, r := range recs {
		if len(r.Data) != 1 {
			return nil, fmt.Errorf("counter census: got %d columns, want 1", len(r.Data))
		}
		name, ok := r.Data[0].(string)
		if !ok {
			return nil, fmt.Errorf("counter census: name is %T, not a string", r.Data[0])
		}
		out[name]++
	}
	return out, nil
}

// TestCounterSpace_NamespaceSeparatesRuns pins the property the whole fix rests
// on, without any concurrency: two runs that do not share a namespace do not
// share a counter node name, and every counter within one run is distinct.
//
// It is deliberately separate from the concurrent gate above. A refactor that
// dropped the namespace from the name would still pass a lightly loaded
// concurrency test by luck; it cannot pass this one at all.
func TestCounterSpace_NamespaceSeparatesRuns(t *testing.T) {
	t.Parallel()

	a := counterSpace{ns: defaultContendedNamespace(0xC0117E57), n: 2}
	b := counterSpace{ns: defaultContendedNamespace(0xD1570177), n: 2}
	if a.ns == b.ns {
		t.Fatalf("two seeds derived the same namespace %q — every run would share one counter"+
			" key space (rmp #2729)", a.ns)
	}

	seen := map[string]string{}
	for _, sp := range []counterSpace{a, b} {
		for k := 0; k < sp.n; k++ {
			name := sp.name(k)
			if owner, dup := seen[name]; dup {
				t.Errorf("counter name %q is produced by both namespace %q and namespace %q",
					name, owner, sp.ns)
			}
			seen[name] = sp.ns
			if !strings.HasPrefix(name, contendedCounterPrefix) {
				t.Errorf("counter name %q does not carry the %q prefix, so a counter node is no"+
					" longer distinguishable from a workload's node", name, contendedCounterPrefix)
			}
			if !strings.Contains(name, sp.ns) {
				t.Errorf("counter name %q does not carry its namespace %q", name, sp.ns)
			}
		}
	}

	// A caller-supplied namespace is honoured verbatim, because
	// runProductionProfileEvidence forms the same names independently in order to
	// read its counters after recovery.
	named := counterSpace{ns: productionProfileCounterNS, n: 1}
	if got, want := named.name(0), contendedCounterName(productionProfileCounterNS, 0); got != want {
		t.Errorf("counterSpace.name(0)=%q but contendedCounterName=%q — a caller reading a"+
			" counter outside RunConcurrent would look up a name the run never wrote", got, want)
	}
}

// TestContendedSpaceLease_RefusesAnOverlappingRun proves the lease can actually
// refuse, and that it refuses only what it must: the same namespace against the
// same server, never the same namespace against a different one.
//
// The lease is what turns a namespace convention into a guarantee. Without this
// test it would be a mechanism nothing exercises, and a lease that always grants
// is indistinguishable from no lease at all.
func TestContendedSpaceLease_RefusesAnOverlappingRun(t *testing.T) {
	t.Parallel()

	// Distinct non-nil server identities; the lease keys on the pointer and never
	// dereferences it, so no server has to be started to exercise the registry.
	srvA, srvB := &SimServer{}, &SimServer{}
	var leases contendedSpaceLeaseSet

	if !leases.acquire(srvA, "ns") {
		t.Fatal("the first acquire was refused on an empty registry")
	}
	if leases.acquire(srvA, "ns") {
		t.Error("a second run acquired a namespace already held against the same server:" +
			" both runs would adjudicate one counter against half its increments (rmp #2729)")
	}
	if !leases.acquire(srvA, "other") {
		t.Error("a different namespace against the same server was refused; runs that share no" +
			" counter node must not block each other")
	}
	if !leases.acquire(srvB, "ns") {
		t.Error("the same namespace against a DIFFERENT server was refused; those runs share no" +
			" nodes at all")
	}

	leases.release(srvA, "ns")
	if !leases.acquire(srvA, "ns") {
		t.Error("the namespace was still held after release — a sequential caller such as" +
			" runProductionProfileEvidence would be refused on its second cycle")
	}
}

// TestRunConcurrent_RefusesAnOverlappingContendedNamespace drives the lease
// through the public entry point: while one run holds a named counter space
// against a server, a second run asking for the same one is refused with a typed
// error rather than silently invalidating both oracles.
//
// The overlap is made deterministic by holding the lease directly rather than by
// racing two runs and hoping they meet.
func TestRunConcurrent_RefusesAnOverlappingContendedNamespace(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	const ns = "held-by-another-run"
	if !contendedSpaceLeases.acquire(srv, ns) {
		t.Fatalf("could not take the namespace %q for the test", ns)
	}

	_, err = RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:               0x2729,
		Connections:        2,
		OpsPerConn:         2,
		ContendedCounters:  2,
		ContendedNamespace: ns,
		Mix:                &ConcurrentMix{TxContendedWeight: 1.0},
	})
	if err == nil {
		contendedSpaceLeases.release(srv, ns)
		t.Fatal("RunConcurrent shared a counter namespace another run was holding, and reported" +
			" success: its zero-lost-updates oracle would adjudicate this run's acknowledged" +
			" increments against a counter both runs wrote (rmp #2729)")
	}
	if !strings.Contains(err.Error(), ns) {
		t.Errorf("the refusal does not name the contested namespace: %v", err)
	}
	contendedSpaceLeases.release(srv, ns)

	// The very same call must now succeed, so the refusal above was the lease and
	// not some unrelated failure of this configuration.
	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:               0x2729,
		Connections:        2,
		OpsPerConn:         2,
		ContendedCounters:  2,
		ContendedNamespace: ns,
		Mix:                &ConcurrentMix{TxContendedWeight: 1.0},
	})
	if err != nil {
		t.Fatalf("the identical run was refused once the namespace was free: %v", err)
	}
	if res.ContendedConnections == 0 {
		t.Fatal("the run drew no contended connection, so it never reached the lease at all")
	}
	for k := range res.ContendedAcked {
		if res.ContendedFinal[k] != res.ContendedAcked[k] {
			t.Errorf("counter %d: final=%d acked=%d", k, res.ContendedFinal[k], res.ContendedAcked[k])
		}
	}
}

// TestProductionProfile_CounterAdjudicationIsNotVacuous proves the production
// profile really exercises the counter adjudication it claims to, at the seed
// its catalogue entry runs.
//
// The profile is the one caller that NAMES its counter namespace
// ([productionProfileCounterNS]) rather than taking the seed-derived default,
// because it is one logical run spread over several [RunConcurrent] calls: it
// accumulates each cycle's acknowledged increments and compares the total
// against the counter's value read back after crash and recovery, through a name
// it forms itself with [contendedCounterName].
//
// That comparison holds trivially at zero. A run in which no connection drew the
// contended role, or none of whose increments was acknowledged, would compare
// nothing against nothing and pass whatever the counters were called — so the
// name the run WRITES and the name the adjudication READS could drift apart
// unnoticed. This test asserts the witness that makes the existing clean-run
// gates load-bearing: increments really were acknowledged, so a name that did
// not match would surface as a lost update in
// [TestProductionProfile_ShortRunsClean] rather than as silence.
func TestProductionProfile_CounterAdjudicationIsNotVacuous(t *testing.T) {
	defer goleak.VerifyNone(t)

	report, ev, err := runProductionProfileEvidence(
		context.Background(), productionProfileScenario().DefaultSeed, shortProductionProfile())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("production profile failed:\n%s", report.String())
	}

	var conns int
	var acked int64
	for _, c := range ev.cycles {
		t.Logf("cycle %d: %d contended connection(s), %d acknowledged increment(s)",
			c.cycle, c.contendedConnections, c.contendedAcked)
		conns += c.contendedConnections
		acked += c.contendedAcked
	}
	if conns == 0 {
		t.Fatalf("no cycle drew a contended writer at seed %#x, so the profile never touched a"+
			" counter: pick a seed that does, or this gate proves nothing",
			productionProfileScenario().DefaultSeed)
	}
	if acked == 0 {
		t.Fatalf("%d contended connection(s) acknowledged no increment at all: the accumulated"+
			" total is zero, so the post-recovery counter read is compared against zero and"+
			" would pass under any counter name (rmp #2729)", conns)
	}
}

package cypher_test

// session_own_writes_under_load_test.go — does Session actually deliver
// read-your-own-writes under sustained multi-writer load? (rmp #2369)
//
// # What this is, and what it is not
//
// #2369 was filed as a P9 ACID CONSISTENCY defect and RETRACTED: the probe used
// bare eng.RunInTx, which cypher/session.go documents as promising nothing
// across statements, so it measured the documented contract rather than a
// violation of it. Engine gives every statement snapshot isolation; Session is
// the read-your-own-writes surface.
//
// What remained genuinely open was narrower: whether Session DELIVERS that
// guarantee under load. It was not established, because the first attempt to
// test it was BROKEN — the count was extracted with a hand-rolled type assertion
// instead of by column name, so the read always yielded 0 and every round
// "failed". That run proved nothing and was discarded.
//
// # Why the load matters
//
// With a quiet graph a commit becomes visible immediately and a SESSIONLESS read
// would pass too, so the guarantee is indistinguishable. It bites only while the
// contiguous frontier is held back by an unrelated in-flight commit — and
// mvcc.TestClock_FrontierStallsBehindOneInFlightCommit proves deterministically,
// on a bare Clock, that the frontier does stall exactly that way. This test
// supplies the load that makes the condition ordinary rather than exceptional.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

const (
	// sessionLoadWriters is the background writer count the task specifies.
	sessionLoadWriters = 16
	// sessionLoadCommits is the number of ACKNOWLEDGED commits the subject
	// session makes and reads back. The task asks for "at least 400"; this is ten
	// times that, and the multiplier is not decoration.
	//
	// MEASURED on the BARE surface under this same load, the stale-read rate is
	// 1 to 4 per 4 000 rounds — about 0.05% — which corroborates the 2-of-400
	// the original probe recorded. At 400 rounds a BROKEN Session would therefore
	// be expected to produce about 0.2 stale reads, so the test would usually
	// MISS it: 400 satisfies the letter of the criterion with roughly 20% power.
	// At 4 000 the expected count is about 2 and the chance of missing a break of
	// that size is around a tenth. Power is the point of the number, not scale.
	sessionLoadCommits = 4000
)

// sessionCount reads a count by COLUMN NAME, the way countQ does. It is written
// this way deliberately: the discarded first attempt at this test extracted the
// value positionally through a type assertion that never matched, so every read
// yielded 0 and the whole run was noise. Reading the named column and failing
// loudly when it is absent is what makes a zero here mean zero.
func sessionCount(t *testing.T, s *cypher.Session, q string) int64 {
	t.Helper()
	res, err := s.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("session Run %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("%q returned no row (err=%v)", q, res.Err())
	}
	v, ok := res.Record()["c"]
	if !ok {
		t.Fatalf("%q: missing column c; the query must project AS c or this reads nothing", q)
	}
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("%q: column c is %T, want an integer", q, v)
	}
	return int64(iv)
}

// TestSession_DeliversReadYourOwnWritesUnderSixteenWriters is the acceptance
// criterion: 16 concurrent writers, at least 400 acknowledged commits, zero
// stale reads.
func TestSession_DeliversReadYourOwnWritesUnderSixteenWriters(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	var (
		noiseCommits atomic.Int64
		noiseErrs    atomic.Int64
	)
	stop := make(chan struct{})
	var noise sync.WaitGroup
	for w := 0; w < sessionLoadWriters; w++ {
		noise.Add(1)
		go func(id int) {
			defer noise.Done()
			ns := eng.NewSession()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := ns.RunInTx(ctx,
					fmt.Sprintf("CREATE (:Noise {w: %d, n: %d})", id, n), nil); err != nil {
					noiseErrs.Add(1)
					continue
				}
				noiseCommits.Add(1)
			}
		}(w)
	}
	defer func() { close(stop); noise.Wait() }()

	// The SUBJECT: one session that commits and immediately reads its own work.
	s := eng.NewSession()
	stale := 0
	firstStale := ""
	for i := 0; i < sessionLoadCommits; i++ {
		if _, err := s.RunInTx(ctx, fmt.Sprintf("CREATE (:Mine {i: %d})", i), nil); err != nil {
			t.Fatalf("acknowledged-commit %d failed: %v; the guarantee is about ACKNOWLEDGED "+
				"commits, so a failed write makes this round meaningless rather than stale", i, err)
		}
		// The commit above returned, so the caller has been told it happened.
		want := int64(i + 1)
		got := sessionCount(t, s, "MATCH (n:Mine) RETURN count(n) AS c")
		if got != want {
			stale++
			if firstStale == "" {
				firstStale = fmt.Sprintf("round %d: saw %d of its own %d committed nodes",
					i, got, want)
			}
		}
	}

	// THE BRACKET. Everything above is satisfied by a run in which the background
	// writers never landed a commit, because with a quiet graph the frontier is
	// never held back and a sessionless read would pass too. The guarantee is
	// only under test while unrelated commits are in flight.
	landed := noiseCommits.Load()
	if landed < int64(sessionLoadCommits) {
		t.Fatalf("the background writers landed only %d commits against %d subject commits "+
			"(%d errors); the frontier was never meaningfully contended, so this run could not "+
			"distinguish Session's guarantee from an ordinary read and proves nothing",
			landed, sessionLoadCommits, noiseErrs.Load())
	}

	t.Logf("%d acknowledged subject commits against %d concurrent background commits from %d "+
		"writers; %d stale reads", sessionLoadCommits, landed, sessionLoadWriters, stale)

	if stale != 0 {
		t.Errorf("Session did NOT deliver read-your-own-writes: %d of %d reads missed a commit "+
			"the caller had already been told succeeded. First: %s. This is the P9 #2369 was "+
			"retracted from, now with a SESSION reproduction as its evidence",
			stale, sessionLoadCommits, firstStale)
	}
}

// TestSession_BareEngineIsTheDistinguishingControl runs the identical pattern on
// the BARE engine surface, which promises nothing across statements.
//
// It does not assert that a stale read occurs — that is a race and requiring it
// would be flaky. It REPORTS the count, so a reader of a green run can see
// whether the load was in fact capable of exposing the difference. The
// deterministic proof that the hazard is reachable lives in
// mvcc.TestClock_FrontierStallsBehindOneInFlightCommit; this is the engine-level
// view of the same fact.
func TestSession_BareEngineIsTheDistinguishingControl(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	stop := make(chan struct{})
	var noise sync.WaitGroup
	var noiseCommits atomic.Int64
	for w := 0; w < sessionLoadWriters; w++ {
		noise.Add(1)
		go func(id int) {
			defer noise.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := eng.RunInTx(ctx,
					fmt.Sprintf("CREATE (:Noise {w: %d, n: %d})", id, n), nil); err == nil {
					noiseCommits.Add(1)
				}
			}
		}(w)
	}
	defer func() { close(stop); noise.Wait() }()

	rounds := sessionLoadCommits
	stale := 0
	for i := 0; i < rounds; i++ {
		if _, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (:Bare {i: %d})", i), nil); err != nil {
			continue
		}
		res, err := eng.Run(ctx, "MATCH (n:Bare) RETURN count(n) AS c", nil)
		if err != nil {
			t.Fatalf("bare read: %v", err)
		}
		var got int64
		if res.Next() {
			if v, ok := res.Record()["c"].(expr.IntegerValue); ok {
				got = int64(v)
			}
		}
		_ = res.Close()
		if got < int64(i+1) {
			stale++
		}
	}
	t.Logf("BARE surface under %d writers: %d stale reads in %d rounds (%d background commits). "+
		"A non-zero count here is the documented Engine contract, not a defect: Engine promises "+
		"snapshot isolation per statement and nothing across statements",
		sessionLoadWriters, stale, rounds, noiseCommits.Load())
}

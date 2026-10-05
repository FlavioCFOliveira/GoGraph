package cypher

// merge_session_contract_test.go — what a MERGE may and may not observe of an
// earlier MERGE of the same key, with and without a [Session] (rmp #2977).
//
// Layer: short.
//
// The contract is docs/isolation-design.md, "Commit visibility and the session
// contract": every entry point gives snapshot isolation, and only a Session also
// guarantees that its next operation observes the commits it has made. The
// visible frontier is contiguous, so a commit whose instant lies above a commit
// still in flight is acknowledged before it is visible. Without a UNIQUE
// constraint, two MERGEs of one key whose snapshots both precede both commits
// each create a node, and that is permitted (docs/mvcc-scenario-catalogue.md
// §0.1, F10 and F11).
//
// Every interleaving below is constructed, never waited for: the in-flight
// commit is an explicit transaction paused by the engine's commit-decided seam,
// after its WAL record allocated its instant and before it publishes.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// mergeContractExec runs one autocommit statement through run and drains it.
func mergeContractExec(run func(context.Context, string, map[string]any) (*Result, error), q string) error {
	res, err := run(context.Background(), q, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	for res.Next() {
	}
	err = res.Err()
	if cerr := res.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	return nil
}

// mergeContractRun is [mergeContractExec] failing t on error. Test goroutine only.
func mergeContractRun(t *testing.T, run func(context.Context, string, map[string]any) (*Result, error), q string) {
	t.Helper()
	if err := mergeContractExec(run, q); err != nil {
		t.Fatal(err)
	}
}

// mergeContractCount returns how many :L nodes carry s = v, by scan.
func mergeContractCount(t *testing.T, eng *Engine, v string) int64 {
	t.Helper()
	return commitStateCount(t, eng, `MATCH (n:L) WHERE n.s + '' = '`+v+`' RETURN count(n) AS c`)
}

// awaitVisibleParked reports whether some goroutine is blocked in the commit
// clock's visibility wait, which is where a Session parks until its own commit
// is visible.
func awaitVisibleParked() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Contains(string(buf[:n]), "mvcc.(*Clock).AwaitVisible")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func TestMergeVisibility_SessionContract(t *testing.T) {
	ctx := context.Background()
	// The WAL wiring: its explicit commit allocates its instant before the fsync,
	// so pausing it after the decision holds the visible frontier below it.
	eng := buildRaceEngine(t, true)

	// (1) Both snapshots precede both commits: two explicit transactions, each
	// opened before either MERGE runs. Neither can see the other's node, there is
	// no constraint, and the two nodes are distinct, so both commit and the key
	// has two nodes. Any other count is a lost write or a snapshot that saw an
	// uncommitted node.
	t1, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range []*ExplicitTx{t1, t2} {
		res, err := tx.ExecAny(`MERGE (:L {s: 'concurrent'})`, nil)
		if err != nil {
			t.Fatal(err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}
	for i, tx := range []*ExplicitTx{t1, t2} {
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i+1, err)
		}
	}
	if c := mergeContractCount(t, eng, "concurrent"); c != 2 {
		t.Fatalf("two MERGEs whose snapshots precede both commits left %d nodes, want 2", c)
	}

	// Hold a commit in flight: it has allocated its instant and not published it,
	// so the frontier cannot pass it until it is released.
	held, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := held.ExecAny(`CREATE (:L {s: 'held'})`, nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	_ = res.Close()
	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	eng.commitDecidedHookForTest = func() { once.Do(func() { close(paused); <-resume }) }
	heldDone := make(chan error, 1)
	go func() { heldDone <- held.Commit() }()
	<-paused
	eng.commitDecidedHookForTest = nil
	released := false
	release := func() {
		if !released {
			released = true
			close(resume)
			if err := <-heldDone; err != nil {
				t.Errorf("held commit: %v", err)
			}
		}
	}
	defer release()

	// (2) Sessionless: the second MERGE starts at the frontier, which is below the
	// first MERGE's acknowledged commit, so it does not see that node and creates
	// another. Permitted: Engine.RunAny gives no cross-statement guarantee.
	mergeContractRun(t, eng.RunAny, `MERGE (:L {s: 'sessionless'})`)
	mergeContractRun(t, eng.RunAny, `MERGE (:L {s: 'sessionless'})`)

	// (3) One Session: the second MERGE must see the first.
	sess := eng.NewSession()
	mergeContractRun(t, sess.RunAny, `MERGE (:L {s: 'session'})`)
	// Precondition, asserted: the session's commit is not visible to a fresh
	// snapshot, so a statement that did not wait would miss it exactly as (2) did.
	snap := eng.g.BeginRead()
	start := snap.StartTS()
	eng.g.EndRead(snap)
	if floor := sess.Floor(); start >= floor {
		t.Fatalf("precondition: a fresh snapshot at %d already sees the session's commit at %d; "+
			"the held commit did not hold the frontier", start, floor)
	}
	second := make(chan struct{})
	var secondErr error
	go func() {
		defer close(second)
		secondErr = mergeContractExec(sess.RunAny, `MERGE (:L {s: 'session'})`)
	}()
	// Release the held commit only once the second MERGE has either finished — it
	// did not wait, which the count below then reports — or parked waiting for the
	// session's own commit to become visible.
	for {
		select {
		case <-second:
		default:
			if !awaitVisibleParked() {
				runtime.Gosched()
				continue
			}
		}
		break
	}
	release()
	<-second
	if secondErr != nil {
		t.Fatal(secondErr)
	}

	if c := mergeContractCount(t, eng, "sessionless"); c != 2 {
		t.Errorf("sessionless: %d nodes, want 2: the second MERGE started below the first commit "+
			"and cannot have seen it", c)
	}
	if c := mergeContractCount(t, eng, "session"); c != 1 {
		t.Errorf("session: %d nodes, want 1: the session's second MERGE did not observe its first", c)
	}
}

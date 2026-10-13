package cypher

// constraint_notnull_escape_test.go — two windows through which a write
// escaped a live NOT NULL constraint (rmp #3127). Each test constructs its
// window through a test seam, so each fails on every run against the code
// that had it.
//
// # A registration between BeginTx's two catalogue reads
//
// BeginTx reads the catalogue's generation, which the commit compares to
// detect a straddled registration, and the active-constraint gates, which
// decide whether the transaction records the nodes the commit-time existence
// check revisits. The generation used to be read after the gates, so a NOT
// NULL registration landing between them left the transaction with neither
// check, and a REMOVE of the constrained property committed under the live
// constraint.
//
// # A removal whose probe read a peer's uncommitted removal
//
// A property removal decided whether to record its node for the commit-time
// existence check from a probe of the PRESENT state, which carries a peer's
// uncommitted removal. When the peer rolled back before the removal landed,
// the probe said "absent", the removal still deleted the committed value, and
// the node was never checked. This is the window
// TestConstraintDDLvsDML_NoViolationAdmitted hit at about 3 runs in 100.
//
// Layer: short.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

func TestConstraintBeginTx_RegistrationBetweenCatalogueReadsBinds(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		eng, _, _ := straddleEngine(t, walBacked)
		if err := straddleRun(eng, `CREATE (:P {id: 1, s: 'a'})`); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		var ddlErr error
		eng.beginTxGatesHookForTest = func() {
			once.Do(func() {
				ddlErr = straddleRun(eng, `CREATE CONSTRAINT c FOR (n:P) REQUIRE n.s IS NOT NULL`)
			})
		}
		tx, err := eng.BeginTx(context.Background())
		eng.beginTxGatesHookForTest = nil
		if err != nil {
			t.Fatal(err)
		}
		if ddlErr != nil {
			_ = tx.Rollback()
			t.Fatalf("CREATE CONSTRAINT inside BeginTx: %v", ddlErr)
		}
		straddleExec(t, tx, `MATCH (n:P {id: 1}) REMOVE n.s`)
		err = tx.Commit()
		if err == nil {
			t.Fatal("COMMIT of REMOVE n.s accepted under a live NOT NULL constraint registered during BeginTx")
		}
		if !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("COMMIT: want a constraint violation, got %v", err)
		}
		if c := commitStateCount(t, eng, `MATCH (n:P) WHERE n.s IS NULL RETURN count(n) AS c`); c != 0 {
			t.Fatalf("%d :P nodes without s committed under a live NOT NULL constraint", c)
		}
	})
}

func TestConstraintNotNull_RemovalAfterPeerRollbackIsChecked(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		for _, q := range []string{
			`CREATE (:P {id: 1, s: 'a'})`,
			`CREATE CONSTRAINT c FOR (n:P) REQUIRE n.s IS NOT NULL`,
		} {
			if err := straddleRun(eng, q); err != nil {
				t.Fatal(err)
			}
		}
		// The peer's uncommitted removal is what the probe below reads.
		peer, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, peer, `MATCH (n:P {id: 1}) REMOVE n.s`)

		probed, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		eng.delPropProbedHookForTest = func() {
			once.Do(func() {
				close(probed)
				<-resume
			})
		}
		done := make(chan error, 1)
		go func() { done <- straddleRun(eng, `MATCH (n:P {id: 1}) REMOVE n.s`) }()
		select {
		case <-probed:
		case err := <-done:
			t.Fatalf("the removal finished without reaching its probe: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the removal never reached its probe")
		}
		// Roll the peer back and withdraw its aborted version, so the removal
		// lands on the committed value its probe did not see.
		if err := peer.Rollback(); err != nil {
			t.Fatal(err)
		}
		eng.g.ReclaimNow()
		close(resume)
		err = <-done
		eng.delPropProbedHookForTest = nil
		if !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("REMOVE n.s under a live NOT NULL constraint: want a constraint violation, got %v", err)
		}
		if c := commitStateCount(t, eng, `MATCH (n:P) WHERE n.s IS NULL RETURN count(n) AS c`); c != 0 {
			t.Fatalf("%d :P nodes without s committed under a live NOT NULL constraint", c)
		}
	})
}

// TestRemoval_RolledBackAfterPeerRollbackKeepsCommittedValue: the removal of
// TestConstraintNotNull_RemovalAfterPeerRollbackIsChecked records no undo
// closure, because its probe read the peer's uncommitted removal and found
// nothing to re-set. When its own transaction then rolls back, the committed
// value it deleted must survive. The MVCC abort, not the closure, restores it:
// the abort withdraws the transaction's property delta synchronously
// (lpg.Graph.abortWake). The value is read back once, then again after a later
// write that builds on the stored value.
func TestRemoval_RolledBackAfterPeerRollbackKeepsCommittedValue(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		if err := straddleRun(eng, `CREATE (:P {id: 1, s: 'a'})`); err != nil {
			t.Fatal(err)
		}
		peer, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, peer, `MATCH (n:P {id: 1}) REMOVE n.s`)
		ours, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The peer rolls back between our probe and our removal, so the probe
		// reads "absent" and the removal deletes the committed value. The peer is
		// rolled back from THIS goroutine while our statement is parked in its
		// bracket on another: a rollback from inside that bracket would nest the
		// graph's barrier.
		probed, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		eng.delPropProbedHookForTest = func() {
			once.Do(func() {
				close(probed)
				<-resume
			})
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			straddleExec(t, ours, `MATCH (n:P {id: 1}) REMOVE n.s`)
		}()
		select {
		case <-probed:
		case <-done:
			t.Fatal("the removal finished without reaching its probe")
		case <-time.After(10 * time.Second):
			t.Fatal("the removal never reached its probe")
		}
		if err := peer.Rollback(); err != nil {
			t.Fatal(err)
		}
		close(resume)
		<-done
		eng.delPropProbedHookForTest = nil
		// The removal took effect: our own view no longer has s. Without this
		// the rollback below could pass because the removal was refused.
		res, err := ours.ExecAny(`MATCH (n:P {id: 1}) WHERE n.s IS NULL RETURN count(n) AS c`, nil)
		if err != nil {
			t.Fatal(err)
		}
		var own any
		for res.Next() {
			own = res.Record()["c"]
		}
		_ = res.Close()
		if fmt.Sprint(own) != "1" {
			t.Fatalf("our removal did not take effect in our own view (count %v)", own)
		}
		if c := commitStateCount(t, eng, `MATCH (n:P {id: 1}) WHERE n.s IS NULL RETURN count(n) AS c`); c != 0 {
			t.Fatalf("our uncommitted removal is visible outside the transaction (%d)", c)
		}
		if err := ours.Rollback(); err != nil {
			t.Fatal(err)
		}
		const q = `MATCH (n:P {id: 1}) WHERE n.s = 'a' RETURN count(n) AS c`
		if c := commitStateCount(t, eng, q); c != 1 {
			t.Fatalf("after rollback: committed s = 'a' lost (count %d)", c)
		}
		if err := straddleRun(eng, `MATCH (n:P {id: 1}) SET n.z = 1`); err != nil {
			t.Fatal(err)
		}
		if c := commitStateCount(t, eng, q); c != 1 {
			t.Fatalf("after a later write: committed s = 'a' lost (count %d)", c)
		}
	})
}

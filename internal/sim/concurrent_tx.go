package sim

// concurrent_tx.go — the transactional writer roles of the concurrent mode
// (rmp #2439): explicit multi-statement transactions over the REAL Bolt wire,
// with contended and disjoint key modes and explicit conflict accounting.
//
// A transaction here is what a production client does: BEGIN, a handful of
// statements, COMMIT — or a deliberate ROLLBACK. Its outcome is classified
// into exactly one bucket (see [ConcurrentResult]):
//
//   - COMMITTED  — the server acknowledged COMMIT; every effect must be
//     visible at quiescence (the marker ledger asserts it).
//   - CONFLICTED — a FAILURE whose Bolt code is the driver-verified retriable
//     serialization conflict, "Neo.TransientError.Transaction.Outdated"
//     (bolt/server/errors.go); an EXPECTED outcome under contention, counted
//     apart from every other failure, and the transaction must leave no
//     trace.
//   - ROLLED BACK — the client chose ROLLBACK; no trace.
//   - FAILED — any other explicit FAILURE; the transaction applied nothing.
//   - AMBIGUOUS — the connection died mid-transaction (transport error or
//     cancellation); the outcome is unknowable client-side and is never
//     asserted.
//
// The CONTENDED mode is the lost-update shape at the wire: read a shared
// counter inside the transaction, write back read+1, and also create a unique
// marker. Zero lost updates means every counter's final value equals its
// acknowledged increments exactly.

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// boltConflictCode is the serialization-conflict failure code the Bolt server
// emits (bolt/server/errors.go maps [mvcc.ErrSerializationConflict] to it) and
// the real neo4j driver retries. Classifying by this code is what separates an
// expected conflict from a genuine failure.
const boltConflictCode = "Neo.TransientError.Transaction.Outdated"

// Contended-counter wire templates (the counter nodes are seeded by
// [seedContendedCounter] before any connection spawns).
const (
	tmplWireCounterRead = "MATCH (n:Person {name:$name}) RETURN n.val"
	tmplWireCounterSet  = "MATCH (n:Person {name:$name}) SET n.val=$v"
)

// seedContendedCounter ensures one shared counter node exists, over a fresh
// setup connection, committed before the contended connections spawn. It is
// IDEMPOTENT — a counter that already exists (a multi-cycle scenario reusing
// one durable store) is left untouched — and reports whether it created the
// node, so the caller's eventual-consistency accounting stays exact.
//
// # Why the check-then-CREATE is safe here, and only here
//
// The MATCH and the CREATE are two separate autocommit statements with no
// atomicity between them, so two callers seeding the SAME name at the same time
// both see count 0 and both create. That is not a hypothetical: measured on the
// pre-#2729 fixed name, eight concurrent runs left three nodes answering counter
// 0 and two answering counter 1, after which the read half of every
// read-modify-write failed its single-row contract and all 192 contended
// transactions failed with none committed — silently, because a counter no
// statement can read also reports no lost update.
//
// The seeding stays a check-then-CREATE, and is sound, because a [counterSpace]
// is leased to exactly one run at a time (see contended_counter_space.go): no
// second seeder for the same name exists to race with. Widening the name back to
// something two runs can share reinstates the race, which is why
// [TestSeedContendedCounter_ConcurrentSeedingLeavesOneNode] counts the nodes
// each counter name answers.
func seedContendedCounter(srv *SimServer, space counterSpace, k int) (created bool, err error) {
	c, err := srv.Dial()
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		return false, err
	}
	run := func(q string, params map[string]any) ([]*proto.Record, error) {
		if _, err := c.Run(q, params); err != nil {
			return nil, err
		}
		records, term, err := c.PullAll()
		if err != nil {
			return nil, err
		}
		if f, ok := term.(*proto.Failure); ok {
			return nil, fmt.Errorf("seed counter %d refused: %s %s", k, f.Code, f.Message)
		}
		return records, nil
	}
	records, err := run("MATCH (n:Person {name:$name}) RETURN count(n)",
		map[string]any{"name": space.name(k)})
	if err != nil {
		return false, err
	}
	if len(records) == 1 && len(records[0].Data) == 1 {
		if n, ok := records[0].Data[0].(int64); ok && n > 0 {
			return false, nil // already seeded by an earlier cycle
		}
	}
	if _, err := run("CREATE (n:Person {name:$name, val:$v})",
		map[string]any{"name": space.name(k), "v": int64(0)}); err != nil {
		return false, err
	}
	return true, nil
}

// txOutcome is the terminal classification of one wire transaction.
type txOutcome int

const (
	txOutcomeCommitted txOutcome = iota
	txOutcomeConflicted
	txOutcomeRolledBack
	txOutcomeFailed
	txOutcomeAmbiguous
)

// txWriterOp runs ONE whole explicit transaction: BEGIN, one to three
// uniquely-named marker creates (plus, in contended mode, a read-modify-write
// on a shared counter first — the lost-update shape), then COMMIT or a
// seed-chosen ROLLBACK (one in ten). Every statement's outcome is classified;
// a typed conflict concedes the transaction with a RESET (the Bolt connection
// is in the FAILED state after any FAILURE), and the marker ledger records the
// transaction's names as acked or refused so quiescence can assert
// all-or-nothing at transaction granularity.
//
// Returns true when the connection must stop (transport error): the open
// transaction's outcome is then AMBIGUOUS — the server may or may not have
// processed what was in flight — and is deliberately never asserted.
func txWriterOp(client *WireClient, seed *Seed, uniq uint64, op int, contended bool, space counterSpace, c *counters, wl *writerLog) (stop bool) {
	wl.txIssued++
	rollback := seed.Float64() < 0.10
	counter := 0
	if contended && space.n > 0 {
		counter = seed.IntN(space.n)
	}
	nMarkers := 1 + seed.IntN(3)
	markers := make([]string, 0, nMarkers)
	for i := 0; i < nMarkers; i++ {
		markers = append(markers, fmt.Sprintf("tx-c%d-o%d-m%d-%d", uniq, op, i, seed.Uint64N(1<<32)))
	}

	refuse := func(outcome txOutcome) {
		switch outcome {
		case txOutcomeConflicted:
			wl.txConflicted++
		case txOutcomeRolledBack:
			wl.txRolledBack++
		case txOutcomeFailed:
			wl.txFailed++
		case txOutcomeAmbiguous:
			wl.txAmbiguous++
		case txOutcomeCommitted:
			// unreachable; committed transactions take the success path below.
		}
		if outcome != txOutcomeAmbiguous {
			wl.txMarkersRefused = append(wl.txMarkersRefused, markers...)
		}
	}

	if outcome, stop := txRequest(client, func() (any, error) { return client.Begin() }); outcome != txOutcomeCommitted {
		refuse(outcome)
		if outcome == txOutcomeConflicted || outcome == txOutcomeFailed {
			return txReset(client, c)
		}
		return stop
	}

	if contended {
		// The read half of the read-modify-write: the value this transaction's
		// snapshot serves. A concurrent increment committed after BEGIN makes
		// the write-back below a lost update, which the engine must refuse.
		v, outcome, stop := txReadCounter(client, space, counter)
		if outcome != txOutcomeCommitted {
			refuse(outcome)
			if outcome == txOutcomeConflicted || outcome == txOutcomeFailed {
				return txReset(client, c)
			}
			return stop
		}
		if outcome, stop := txStatement(client, tmplWireCounterSet,
			map[string]any{"name": space.name(counter), "v": v + 1}); outcome != txOutcomeCommitted {
			refuse(outcome)
			if outcome == txOutcomeConflicted || outcome == txOutcomeFailed {
				return txReset(client, c)
			}
			return stop
		}
	}

	for _, m := range markers {
		if outcome, stop := txStatement(client, tmplCreatePerson,
			map[string]any{"name": m, "age": int64(seed.IntN(100))}); outcome != txOutcomeCommitted {
			refuse(outcome)
			if outcome == txOutcomeConflicted || outcome == txOutcomeFailed {
				return txReset(client, c)
			}
			return stop
		}
	}

	if rollback {
		if outcome, stop := txRequest(client, func() (any, error) { return client.Rollback() }); outcome != txOutcomeCommitted {
			refuse(outcome)
			if outcome == txOutcomeConflicted || outcome == txOutcomeFailed {
				return txReset(client, c)
			}
			return stop
		}
		refuse(txOutcomeRolledBack)
		return false
	}

	outcome, stop := txRequest(client, func() (any, error) { return client.Commit() })
	switch outcome {
	case txOutcomeCommitted:
		wl.txCommitted++
		wl.txMarkersAcked = append(wl.txMarkersAcked, markers...)
		c.ackedCreates.Add(int64(len(markers)))
		if contended && wl.contendedAcked != nil {
			wl.contendedAcked[counter]++
		}
		return false
	case txOutcomeConflicted, txOutcomeFailed:
		refuse(outcome)
		return txReset(client, c)
	default:
		refuse(outcome)
		return stop
	}
}

// txStatement runs one statement (RUN + PULL) inside the open transaction and
// classifies its outcome.
func txStatement(client *WireClient, query string, params map[string]any) (txOutcome, bool) {
	resp, err := client.Run(query, params)
	if err != nil {
		return txOutcomeAmbiguous, true
	}
	if out, refused := classifyResponse(resp); refused {
		return out, false
	}
	_, term, err := client.PullAll()
	if err != nil {
		return txOutcomeAmbiguous, true
	}
	if out, refused := classifyResponse(term); refused {
		return out, false
	}
	return txOutcomeCommitted, false
}

// txReadCounter reads the contended counter inside the open transaction and
// returns its value.
func txReadCounter(client *WireClient, space counterSpace, k int) (int64, txOutcome, bool) {
	resp, err := client.Run(tmplWireCounterRead, map[string]any{"name": space.name(k)})
	if err != nil {
		return 0, txOutcomeAmbiguous, true
	}
	if out, refused := classifyResponse(resp); refused {
		return 0, out, false
	}
	records, term, err := client.PullAll()
	if err != nil {
		return 0, txOutcomeAmbiguous, true
	}
	if out, refused := classifyResponse(term); refused {
		return 0, out, false
	}
	if len(records) != 1 || len(records[0].Data) != 1 {
		return 0, txOutcomeFailed, false
	}
	v, ok := records[0].Data[0].(int64)
	if !ok {
		return 0, txOutcomeFailed, false
	}
	return v, txOutcomeCommitted, false
}

// txRequest sends one transaction-control message (BEGIN/COMMIT/ROLLBACK) and
// classifies its response.
func txRequest(_ *WireClient, send func() (any, error)) (txOutcome, bool) {
	resp, err := send()
	if err != nil {
		return txOutcomeAmbiguous, true
	}
	if out, refused := classifyResponse(resp); refused {
		return out, false
	}
	return txOutcomeCommitted, false
}

// classifyResponse separates the EXPECTED, retriable serialization conflict —
// by its exact Bolt code, the one the real neo4j driver's retry classifier
// accepts — from every other refusal. An IGNORED response (the connection is
// already in the Bolt FAILED state) classifies as a failure too: it is never
// a success, and like every explicit refusal it requires a RESET before the
// connection can proceed.
func classifyResponse(resp any) (txOutcome, bool) {
	switch f := resp.(type) {
	case *proto.Failure:
		if f.Code == boltConflictCode {
			return txOutcomeConflicted, true
		}
		return txOutcomeFailed, true
	case *proto.Ignored:
		return txOutcomeFailed, true
	}
	return txOutcomeCommitted, false
}

// txReset clears the Bolt FAILED state after an explicit failure so the
// connection can run its next transaction. A transport error stops the
// connection.
func txReset(client *WireClient, c *counters) (stop bool) {
	if _, err := client.Reset(); err != nil {
		c.transportErrors.Add(1)
		return true
	}
	return false
}

// verifyTxQuiescence verifies, over one fresh connection, the transaction
// ledgers at quiescence: every acknowledged marker present (a miss is a lost
// committed transaction), every refused marker absent (a hit is a phantom —
// a refused transaction that left a trace), and each contended counter's
// final value (compared by the caller against the acknowledged increments).
func verifyTxQuiescence(srv *SimServer, acked, refused []string, space counterSpace) (missing, phantom int64, finals []int64, err error) {
	c, err := srv.Dial()
	if err != nil {
		return 0, 0, nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		return 0, 0, nil, err
	}
	countByName := func(name string) (int64, error) {
		if _, err := c.Run("MATCH (n:Person {name:$name}) RETURN count(n)", map[string]any{"name": name}); err != nil {
			return 0, err
		}
		records, _, err := c.PullAll()
		if err != nil {
			return 0, err
		}
		if len(records) != 1 || len(records[0].Data) != 1 {
			return 0, fmt.Errorf("count query shape unexpected")
		}
		n, ok := records[0].Data[0].(int64)
		if !ok {
			return 0, fmt.Errorf("count not an int64: %T", records[0].Data[0])
		}
		return n, nil
	}
	for _, m := range acked {
		n, err := countByName(m)
		if err != nil {
			return 0, 0, nil, err
		}
		if n == 0 {
			missing++
		}
	}
	for _, m := range refused {
		n, err := countByName(m)
		if err != nil {
			return 0, 0, nil, err
		}
		if n != 0 {
			phantom++
		}
	}
	finals = make([]int64, space.n)
	for k := 0; k < space.n; k++ {
		if _, err := c.Run(tmplWireCounterRead, map[string]any{"name": space.name(k)}); err != nil {
			return 0, 0, nil, err
		}
		records, _, err := c.PullAll()
		if err != nil {
			return 0, 0, nil, err
		}
		if len(records) == 1 && len(records[0].Data) == 1 {
			if v, ok := records[0].Data[0].(int64); ok {
				finals[k] = v
			}
		}
	}
	return missing, phantom, finals, nil
}

package sim

// concurrent_iso.go — during-run isolation oracles for the concurrent mode
// (rmp #2440). Production correctness is observed DURING execution, not only
// at quiescence; three per-connection oracles run inside the genuinely
// parallel run, each keeping goroutine-owned state reconciled after the join
// (the writerLog pattern):
//
//   - MONOTONIC READS — a reader connection repeatedly reads a shared
//     contended counter; the observed value must never go backwards. Counter
//     values only ever grow (the contended role's increments are the sole
//     writers), and the Bolt session's read-your-own-writes floor plus commit
//     monotonicity make a regression a genuine isolation violation.
//   - READ-YOUR-OWN-WRITES — a connection that creates a uniquely-named node
//     (autocommit) and immediately reads it back ON THE SAME CONNECTION must
//     observe it.
//   - ATOMIC BATCH VISIBILITY — a batch writer creates exactly
//     [isoBatchSize] nodes per explicit transaction, every one tagged
//     batch:true; concurrent readers count the tagged population and must
//     only ever observe multiples of the batch size (and, per connection, a
//     non-decreasing count).
//
// Violations are typed counters in [ConcurrentResult], asserted zero on HEAD;
// the observation functions are factored pure so the injection tests can
// drive each oracle to fire.

import (
	"fmt"
)

// isoBatchSize is the atomic batch writer's fixed transaction size: readers
// must only ever observe multiples of it.
const isoBatchSize = 4

// Batch and RYOW wire templates.
const (
	tmplBatchCreate = "CREATE (n:Person {name:$name, batch:true})"
	tmplBatchCount  = "MATCH (n:Person) WHERE n.batch = true RETURN count(n)"
)

// observeMonotonic classifies one observation of a monotone series: it
// returns true when the new value regressed below the last observed one, and
// advances the floor otherwise. Factored pure so the injection test can feed
// a regressing sequence.
func observeMonotonic(last *int64, v int64) (regressed bool) {
	if v < *last {
		return true
	}
	*last = v
	return false
}

// observeBatch classifies one observation of the atomic-batch population:
// a count that is not a multiple of the batch size is a torn batch — a reader
// caught a transaction's partial effects.
func observeBatch(batchSize int, v int64) (torn bool) {
	return v%int64(batchSize) != 0
}

// isoReaderOp performs one during-run oracle read: the shared counter's
// monotonicity (when contended counters exist) and the batch population's
// atomicity + per-connection monotonicity. Read failures under overload are
// bounded rejects, not oracle violations.
func isoReaderOp(client *WireClient, seed *Seed, space counterSpace, c *counters, wl *writerLog) (stop bool) {
	if space.n > 0 {
		k := seed.IntN(space.n)
		v, ok, stop := wireScalar(client, tmplWireCounterRead, map[string]any{"name": space.name(k)}, c)
		if stop {
			return true
		}
		if ok {
			wl.isoReads++
			if observeMonotonic(&wl.isoLastCounter[k], v) {
				wl.isoMonotonicViolations++
			}
		}
	}
	v, ok, stop := wireScalar(client, tmplBatchCount, nil, c)
	if stop {
		return true
	}
	if ok {
		wl.isoReads++
		if observeBatch(isoBatchSize, v) {
			wl.isoBatchViolations++
		}
		if observeMonotonic(&wl.isoLastBatchCount, v) {
			wl.isoMonotonicViolations++
		}
	}
	return false
}

// ryowWriterOp creates one uniquely-named node (autocommit) and immediately
// reads it back over the SAME connection: the Bolt session contract makes the
// write visible to the connection's next statement, so a miss is a
// read-your-own-writes violation.
func ryowWriterOp(client *WireClient, seed *Seed, uniq uint64, op int, c *counters, wl *writerLog) (stop bool) {
	name := fmt.Sprintf("ryow-c%d-n%d-%d", uniq, op, seed.Uint64N(1<<32))
	wl.issued = append(wl.issued, name)
	resp, err := client.Run(tmplCreatePerson, map[string]any{"name": name, "age": int64(seed.IntN(100))})
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if _, refused := classifyResponse(resp); refused {
		c.boundedRejects.Add(1)
		return txReset(client, c)
	}
	_, term, err := client.PullAll()
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if _, refused := classifyResponse(term); refused {
		c.boundedRejects.Add(1)
		return txReset(client, c)
	}
	c.ackedCreates.Add(1)
	wl.acked = append(wl.acked, name)

	// The read-back, same connection: the acknowledged write must be visible.
	v, ok, stop := wireScalar(client, "MATCH (n:Person {name:$name}) RETURN count(n)", map[string]any{"name": name}, c)
	if stop {
		return true
	}
	if ok {
		wl.isoReads++
		if v != 1 {
			wl.isoRYOWViolations++
		}
	}
	return false
}

// batchWriterOp runs ONE atomic batch: an explicit transaction creating
// exactly [isoBatchSize] tagged nodes, always committed. Its ledger reuses the
// transactional marker discipline so quiescence still verifies it.
func batchWriterOp(client *WireClient, seed *Seed, uniq uint64, op int, c *counters, wl *writerLog) (stop bool) {
	wl.txIssued++
	markers := make([]string, 0, isoBatchSize)
	for i := 0; i < isoBatchSize; i++ {
		markers = append(markers, fmt.Sprintf("batch-c%d-o%d-m%d-%d", uniq, op, i, seed.Uint64N(1<<32)))
	}
	refuse := func(outcome txOutcome) {
		switch outcome {
		case txOutcomeConflicted:
			wl.txConflicted++
		case txOutcomeFailed:
			wl.txFailed++
		case txOutcomeAmbiguous:
			wl.txAmbiguous++
		case txOutcomeCommitted, txOutcomeRolledBack:
			// unreachable on this role's refusal path.
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
	for _, m := range markers {
		if outcome, stop := txStatement(client, tmplBatchCreate,
			map[string]any{"name": m}); outcome != txOutcomeCommitted {
			refuse(outcome)
			if outcome == txOutcomeConflicted || outcome == txOutcomeFailed {
				return txReset(client, c)
			}
			return stop
		}
	}
	outcome, stop := txRequest(client, func() (any, error) { return client.Commit() })
	switch outcome {
	case txOutcomeCommitted:
		wl.txCommitted++
		wl.txMarkersAcked = append(wl.txMarkersAcked, markers...)
		c.ackedCreates.Add(int64(len(markers)))
		return false
	case txOutcomeConflicted, txOutcomeFailed:
		refuse(outcome)
		return txReset(client, c)
	default:
		refuse(outcome)
		return stop
	}
}

// wireScalar runs one single-scalar read over the wire and returns the
// integer. ok is false when the statement was refused (a bounded reject — the
// oracle skips the observation rather than misreading a refusal as a value);
// stop is true on a transport error.
func wireScalar(client *WireClient, query string, params map[string]any, c *counters) (v int64, ok, stop bool) {
	resp, err := client.Run(query, params)
	if err != nil {
		c.transportErrors.Add(1)
		return 0, false, true
	}
	if _, refused := classifyResponse(resp); refused {
		c.boundedRejects.Add(1)
		return 0, false, txReset(client, c)
	}
	records, term, err := client.PullAll()
	if err != nil {
		c.transportErrors.Add(1)
		return 0, false, true
	}
	if _, refused := classifyResponse(term); refused {
		c.boundedRejects.Add(1)
		return 0, false, txReset(client, c)
	}
	if len(records) != 1 || len(records[0].Data) != 1 {
		return 0, false, false
	}
	n, isInt := records[0].Data[0].(int64)
	if !isInt {
		return 0, false, false
	}
	return n, true, false
}

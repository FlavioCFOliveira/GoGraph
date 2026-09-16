package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// ─────────────────────────────────────────────────────────────────────────────
// Workload shapes — the dimension that decides WHAT each connection sends
// ─────────────────────────────────────────────────────────────────────────────

// The four shapes the laboratory can drive. They exist because a server
// certified on one shape of work is certified on one shape of work: rounds 1
// and 2 of this campaign drove nothing but workloadCount, so the RECORD-heavy
// path and the explicit-transaction path had never been exercised at
// concurrency at all and bolt/server/txregistry.go had never appeared in a
// profile.
//
// The shapes are deliberately nested so that consecutive pairs differ by ONE
// thing, which is what lets a difference between two ladders be attributed:
//
//   - workloadCount   → workloadTxRead  differ ONLY by BEGIN/COMMIT: the
//     statement, the result, and the row count are identical.
//   - workloadCount   → workloadRecords differ ONLY by how many RECORDs one
//     reply carries.
//   - workloadTxRead  → workloadTxWrite differ by the statement being a write.
const (
	// workloadCount is the original shape and the default: one auto-commit
	// read returning ONE row. Every rung published before this dimension
	// existed was measured with it, so leaving it the default keeps every
	// earlier number meaning what it said.
	workloadCount = "count"

	// workloadRecords is the RECORD-heavy read: one auto-commit read returning
	// exactly config.rows rows. It is what exercises the response batching —
	// bolt/server/serve.go disables auto-flush for the connection's writer, so
	// a K-row result is meant to cost O(bytes/bufsize) writes rather than K.
	workloadRecords = "records"

	// workloadTxRead is the explicit READ transaction: BEGIN, RUN, PULL,
	// COMMIT, around the same statement workloadCount runs. It is the shape
	// that makes bolt.server.tx.opened non-zero and therefore the one that
	// puts bolt/server/txregistry.go and bolt/server/txquota.go on the
	// measured path at all.
	workloadTxRead = "txread"

	// workloadTxWrite is the explicit WRITE transaction: BEGIN, RUN, PULL,
	// COMMIT, around a statement that increments a per-connection counter.
	// Its committed effect is read back and verified after every window (see
	// counterTotal), so a transaction that reported success without committing
	// would fail the run rather than pass unnoticed.
	workloadTxWrite = "txwrite"
)

// workloadKinds is the accepted set, in the order the nesting above reads.
var workloadKinds = [...]string{workloadCount, workloadRecords, workloadTxRead, workloadTxWrite}

// Counter model, used by workloadTxWrite alone.
//
//	(:Counter {slot, hits})
//
// One node per write slot, each incremented by the connection that owns it.
const (
	labelCounter = "Counter"
	propSlot     = "slot"
	propHits     = "hits"

	// paramSlot names the bound parameter carrying the connection's slot.
	//
	// It is a PARAMETER and not a literal on purpose. A per-connection literal
	// would give every connection its own query text, so the engine's plan
	// cache would hold one entry per connection and its size would grow with
	// the connection count — an N-dependence introduced by the instrument, on
	// the very axis the ladder measures. One parameterised text keeps the plan
	// cache at one entry at every rung.
	paramSlot = "s"
)

// writeSlots is how many :Counter nodes the txwrite shape seeds by default, and
// it is a FIXED value rather than a function of the connection count.
//
// That is the whole point of it. Seeding one counter per connection would make
// the statement's label scan linear in the connection count, and the write
// shape's per-query cost would then grow with N because of the instrument
// rather than because of the server — precisely the confound this campaign
// exists to avoid. With a fixed 1024 the scan is identical at every rung, and
// 1024 is also the top of the published ladder, so no two connections ever
// share a counter and no transaction ever collides with another.
//
// -write-slots overrides it, and exists for ONE experiment: separating "many
// written NODES" from "many concurrent WRITERS". Raising the slot count at a
// fixed connection count leaves the number of concurrent writers unchanged
// while multiplying the nodes they write, so the two explanations for a
// write-path cost that grows with N predict opposite results.
const writeSlots = 1024

// defaultRows is the RECORD-heavy shape's default reply cardinality.
//
// 100 rows of roughly 48 bytes each is about 4.8 KB, so a reply spans the
// connection writer's 4096-byte buffer more than once. That is deliberate: a
// value that fitted inside one buffer would be indistinguishable from a
// one-row reply at the syscall level and would test nothing about batching.
const defaultRows = 100

// writeSlotCount is how many :Counter nodes this configuration seeds: the
// configured slot count for the write shape and none for every other, so the
// read shapes' seeded graph stays byte-identical to the one every earlier rung
// measured.
func (c *config) writeSlotCount() int {
	if !c.writesGraph() {
		return 0
	}
	if c.writeSlots > 0 {
		return c.writeSlots
	}
	return writeSlots
}

// personIDLen is the length of a :Person id, in characters (12 random bytes
// rendered as lowercase hex). The RECORD-heavy shape checks it on every row so
// that "the rows arrived" is a claim about their CONTENT, not only their count.
const personIDLen = 24

// The statements each shape runs. They are constants so a run cannot drift
// from what a report says it measured.
const (
	// countPersonQuery is the fixed read: the count of :Person nodes, which is
	// deterministic over the seeded data (it equals cfg.nodes). It is the
	// regression baseline the test pins, and workloadTxRead runs exactly this
	// statement inside an explicit transaction so the two ladders differ by the
	// transaction and by nothing else.
	countPersonQuery = "MATCH (n:Person) RETURN count(n) AS c"

	// recordsQueryFmt is the RECORD-heavy read. The row count is formatted into
	// the text rather than bound as a parameter so the shape has exactly one
	// query text per -rows value, and LIMIT receives an integer literal, which
	// is the form openCypher is unambiguous about.
	recordsQueryFmt = "MATCH (n:Person) RETURN n.id AS id, n.name AS name LIMIT %d"

	// incCounterQuery is the explicit write. It finds this connection's own
	// counter and increments it, returning the new value so the client can
	// check the write happened rather than assume it.
	incCounterQuery = "MATCH (c:" + labelCounter + " {" + propSlot + ": $" + paramSlot + "}) " +
		"SET c." + propHits + " = c." + propHits + " + 1 RETURN c." + propHits + " AS h"

	// counterSumQuery reads the committed total back. It is run IN PROCESS
	// through the engine, never over the wire, so verifying a window costs the
	// server no connection and the measurement no time.
	counterSumQuery = "MATCH (c:" + labelCounter + ") RETURN sum(c." + propHits + ") AS t"
)

// workload is the resolved shape one run drives: the statement, how the reply
// is consumed, and what makes a reply correct.
//
// It is built once per run by [config.workload] and shared, read-only, by every
// worker goroutine. It is therefore safe for concurrent use.
type workload struct {
	// kind is one of workloadKinds.
	kind string

	// query is the statement every unit of work runs.
	query string

	// mode is the driver session's access mode. It decides the `mode` field the
	// driver puts on BEGIN, which bolt/server/session.go handleBegin reads as a
	// capability restriction: "r" refuses writes.
	mode neo4j.AccessMode

	// explicit reports whether a unit is BEGIN/RUN/PULL/COMMIT rather than one
	// auto-commit RUN. It is the flag that decides whether the transaction
	// registry and the per-principal quota are on the measured path at all.
	explicit bool

	// rows is how many RECORDs one reply must carry. It is 1 for every shape
	// but workloadRecords.
	rows int

	// wantCount is the value column `c` must hold, for the two shapes that read
	// the :Person count. Zero for the others.
	wantCount int64

	// slots is how many :Counter nodes exist, so a worker can map its slot into
	// range. It is one for every shape but the explicit write, which never
	// divides by it anyway.
	slots int
}

// validWorkload reports whether kind is one of workloadKinds. The empty string
// is accepted and means the default, so a config built by hand in a test need
// not name this dimension.
func validWorkload(kind string) bool {
	if kind == "" {
		return true
	}
	for _, k := range workloadKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// workloadKind is the configured shape with the empty string resolved to the
// default, so the machine-readable record never carries an ambiguous value.
func (c *config) workloadKind() string {
	if c.workload == "" {
		return workloadCount
	}
	return c.workload
}

// rowsPerQuery is how many RECORDs one reply carries under this configuration:
// the configured row count for the RECORD-heavy shape, and one for every other.
//
// A zero resolves to [defaultRows] — the same value the flag defaults to — for
// the reason config states about every laboratory dimension: a config built by
// hand in a test names only the dimensions it cares about, and an unnamed one
// must mean the documented default rather than a failure.
func (c *config) rowsPerQuery() int {
	if c.workloadKind() != workloadRecords {
		return 1
	}
	if c.rows > 0 {
		return c.rows
	}
	return defaultRows
}

// writesGraph reports whether this configuration's shape commits writes, and
// therefore whether the run must seed the :Counter nodes and verify the
// committed effect of every window.
func (c *config) writesGraph() bool { return c.workloadKind() == workloadTxWrite }

// explicitTx reports whether this configuration's shape opens explicit
// transactions, and therefore whether bolt.server.tx.opened must be non-zero in
// every window. A zero there invalidates any claim about the registry.
func (c *config) explicitTx() bool {
	k := c.workloadKind()
	return k == workloadTxRead || k == workloadTxWrite
}

// workloadSpec resolves the configuration into the shape the workers drive.
func (c *config) workloadSpec() *workload {
	switch c.workloadKind() {
	case workloadRecords:
		return &workload{
			kind:  workloadRecords,
			query: fmt.Sprintf(recordsQueryFmt, c.rowsPerQuery()),
			mode:  neo4j.AccessModeRead,
			rows:  c.rowsPerQuery(),
			slots: 1,
		}
	case workloadTxRead:
		return &workload{
			kind:      workloadTxRead,
			query:     countPersonQuery,
			mode:      neo4j.AccessModeRead,
			explicit:  true,
			rows:      1,
			wantCount: int64(c.nodes),
			slots:     1,
		}
	case workloadTxWrite:
		return &workload{
			kind:     workloadTxWrite,
			query:    incCounterQuery,
			mode:     neo4j.AccessModeWrite,
			explicit: true,
			rows:     1,
			slots:    c.writeSlotCount(),
		}
	default:
		return &workload{
			kind:      workloadCount,
			query:     countPersonQuery,
			mode:      neo4j.AccessModeRead,
			rows:      1,
			wantCount: int64(c.nodes),
			slots:     1,
		}
	}
}

// params returns the bound parameters one worker sends with every unit, or nil
// when the shape binds none. The map is built once per worker and reused for
// every unit: the driver serialises it into the outbound message and retains no
// reference, so reusing it costs one allocation per worker instead of one per
// query — which matters because the client shares this process's cores with the
// server it is measuring.
func (w *workload) params(slot int) map[string]any {
	if w.kind != workloadTxWrite {
		return nil
	}
	return map[string]any{paramSlot: int64(slot % w.slots)}
}

// ─────────────────────────────────────────────────────────────────────────────
// Driving one unit of work
// ─────────────────────────────────────────────────────────────────────────────

// runUnit executes exactly one unit of w over sess and returns how many RECORDs
// it consumed.
//
// An auto-commit unit is one RUN and its reply. An explicit unit is
// BEGIN, RUN, PULL, COMMIT — three client round trips instead of one, which is
// why the explicit shapes measure roughly a third of the auto-commit shape's
// queries per second at the same rung and why nothing but rung-to-rung
// comparison WITHIN a shape is meaningful.
//
// Every failure path rolls the transaction back before returning, so a unit that
// fails mid-stream never leaves a transaction open behind it. That matters to
// the measurement and not only to hygiene: an abandoned transaction would leave
// bolt.server.tx.opened and bolt.server.tx.closed unbalanced and would show up
// as a leak that the client, not the server, caused.
func runUnit(ctx context.Context, sess neo4j.SessionWithContext, w *workload, params map[string]any) (int, error) {
	if !w.explicit {
		res, err := sess.Run(ctx, w.query, params)
		if err != nil {
			return 0, fmt.Errorf("run: %w", err)
		}
		return w.consume(ctx, res)
	}

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	res, err := tx.Run(ctx, w.query, params)
	if err != nil {
		// Best-effort unwind: the run error below is the one worth reporting.
		_ = tx.Rollback(ctx)
		return 0, fmt.Errorf("run: %w", err)
	}
	n, cerr := w.consume(ctx, res)
	if cerr != nil {
		// Best-effort unwind: the consume error below is the one worth reporting.
		_ = tx.Rollback(ctx)
		return n, cerr
	}
	if err := tx.Commit(ctx); err != nil {
		return n, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}

// consume drains one reply and verifies it, returning the number of RECORDs it
// carried.
//
// Verification is per shape and is part of the measurement, not a side check: a
// run that reported 190,000 queries per second while silently returning the
// wrong rows would have measured nothing worth knowing.
func (w *workload) consume(ctx context.Context, res neo4j.ResultWithContext) (int, error) {
	switch w.kind {
	case workloadRecords:
		return w.consumeRecords(ctx, res)
	case workloadTxWrite:
		return w.consumeWrite(ctx, res)
	default:
		return w.consumeCount(ctx, res)
	}
}

// consumeCount checks the single-row :Person count. It is the check both
// workloadCount and workloadTxRead use, so the two shapes verify identically
// and differ only in the transaction wrapped around them.
func (w *workload) consumeCount(ctx context.Context, res neo4j.ResultWithContext) (int, error) {
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, fmt.Errorf("single: %w", err)
	}
	v, ok := rec.Get("c")
	if !ok {
		return 0, fmt.Errorf("column 'c' missing")
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("column 'c': expected int64, got %T", v)
	}
	if n != w.wantCount {
		return 0, fmt.Errorf("count=%d, want %d", n, w.wantCount)
	}
	return 1, nil
}

// consumeRecords drains a RECORD-heavy reply and checks BOTH its cardinality
// and its content: exactly w.rows rows, each carrying two columns whose first
// is a :Person id of the expected length.
//
// It iterates rather than calling Collect so the client allocates no slice of
// the whole result. The client shares this process's cores with the server it
// is measuring, so every avoidable client allocation is charged to the
// measurement.
//
// The values are read POSITIONALLY (rec.Values[0]) rather than by name. Record.Get
// scans the key slice on every call, and at a thousand rows a query that cost is
// the client's, spent inside the window.
func (w *workload) consumeRecords(ctx context.Context, res neo4j.ResultWithContext) (int, error) {
	n := 0
	for res.Next(ctx) {
		rec := res.Record()
		if len(rec.Values) != 2 {
			return n, fmt.Errorf("row %d: %d columns, want 2", n, len(rec.Values))
		}
		id, ok := rec.Values[0].(string)
		if !ok {
			return n, fmt.Errorf("row %d: column 'id': expected string, got %T", n, rec.Values[0])
		}
		if len(id) != personIDLen {
			return n, fmt.Errorf("row %d: id %q is %d chars, want %d", n, id, len(id), personIDLen)
		}
		n++
	}
	if err := res.Err(); err != nil {
		return n, fmt.Errorf("stream: %w", err)
	}
	if n != w.rows {
		return n, fmt.Errorf("%d records, want %d", n, w.rows)
	}
	return n, nil
}

// consumeWrite checks the value the increment returned. A committed increment
// is always at least one, so a zero or negative reading means the statement
// reported success over a write that did not happen.
//
// This is the per-unit half of the write verification; the per-window half —
// that the committed total really moved by the number of units that succeeded —
// is [verifyWriteEffect], which reads the graph back through the engine.
func (w *workload) consumeWrite(ctx context.Context, res neo4j.ResultWithContext) (int, error) {
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, fmt.Errorf("single: %w", err)
	}
	v, ok := rec.Get("h")
	if !ok {
		return 0, fmt.Errorf("column 'h' missing")
	}
	h, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("column 'h': expected int64, got %T", v)
	}
	if h <= 0 {
		return 0, fmt.Errorf("counter returned %d after an increment, want >= 1", h)
	}
	return 1, nil
}

// workloadList renders the accepted shapes for a flag's usage string.
func workloadList() string { return strings.Join(workloadKinds[:], " | ") }

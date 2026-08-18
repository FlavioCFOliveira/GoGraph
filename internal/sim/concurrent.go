package sim

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// ConcurrentConfig parameterises a concurrent multi-connection run. Every field
// is bounded: the harness spawns exactly Connections goroutines, each performing
// at most OpsPerConn operations, so total work is Connections×OpsPerConn and the
// connection count and per-connection work are both explicit upper bounds (the
// reliability mandate's bounded-resources rule).
type ConcurrentConfig struct {
	// Mix selects the per-connection actor behaviour. When nil, an honest
	// read/write mix is used.
	Mix *ConcurrentMix
	// Seed controls WHAT each connection sends and WHEN its faults fire. Goroutine
	// interleaving is NOT seed-controlled (see the package note on the hybrid
	// determinism model): this mode is robustness/liveness/leak-checked, not
	// bit-reproducible.
	Seed uint64
	// Connections is the number of concurrent client connections (one goroutine
	// each). Values <= 0 are normalised to 1.
	Connections int
	// OpsPerConn is the number of operations each connection performs before it
	// closes. Values <= 0 are normalised to 1. For the transactional roles one
	// operation is one whole transaction.
	OpsPerConn int
	// ContendedCounters is the size of the shared counter key space the
	// contended transactional role collides on (rmp #2439). Values <= 0
	// normalise to 2. Fewer counters means more conflicts.
	ContendedCounters int
}

// ConcurrentMix is the per-connection actor selection for a concurrent run. Each
// connection draws one role from its own seed-derived sub-stream and plays it
// for the whole connection, so the population is a deterministic function of the
// master seed even though interleaving is not.
type ConcurrentMix struct {
	// WriterWeight, ReaderWeight, OverloadWeight are the relative weights for the
	// three honest-ish roles. They need not sum to 1.
	WriterWeight   float64
	ReaderWeight   float64
	OverloadWeight float64
	// TxWriterWeight selects the DISJOINT transactional writer (rmp #2439):
	// explicit multi-statement transactions over the real Bolt wire (BEGIN, one
	// to three uniquely-named creates, COMMIT or a seed-chosen ROLLBACK), whose
	// keys never collide with another connection's.
	TxWriterWeight float64
	// TxContendedWeight selects the CONTENDED transactional writer: an explicit
	// read-modify-write on a small shared counter space (the lost-update shape)
	// plus a unique marker create, so serialization conflicts are an expected,
	// typed outcome the run accounts for separately from failures.
	TxContendedWeight float64
	// BatchWriterWeight selects the atomic-batch writer (rmp #2440): one
	// explicit transaction of exactly isoBatchSize tagged creates, always
	// committed, so readers can assert batch-multiple visibility.
	BatchWriterWeight float64
	// IsoReaderWeight selects the during-run oracle reader (rmp #2440):
	// per-connection monotonic reads over the shared counters and the
	// batch-multiple atomic-visibility check.
	IsoReaderWeight float64
	// RYOWWriterWeight selects the read-your-own-writes probe role
	// (rmp #2440): write then immediately read back on the same connection.
	RYOWWriterWeight float64
}

// defaultConcurrentMix is a write/read/overload population that keeps the graph
// growing while exercising reads and the bounded overload path.
func defaultConcurrentMix() *ConcurrentMix {
	return &ConcurrentMix{WriterWeight: 0.5, ReaderWeight: 0.4, OverloadWeight: 0.1}
}

// ConcurrentResult summarises a concurrent run for assertions and reports. It is
// the eventual-consistency oracle at quiescence: AckedCreates is the number of
// node-creating operations connections acknowledged as committed, which must
// equal the engine's live node count once every goroutine has drained (no
// committed write lost, no phantom write gained).
type ConcurrentResult struct {
	// AckedNames is the UNION, across every writer connection, of the unique
	// node names whose create was acknowledged (a SUCCESS-terminated PULL). It is
	// the durability oracle at the NAME granularity the durable-commit crash
	// scenario needs: every name here MUST survive a crash+recovery (recovered ⊇
	// acked). Each writer op uses a globally-unique name and is never retried, so
	// the union is a set with no duplicates. Populated only for the writer role;
	// nil-safe for read/overload-only runs (stays empty).
	AckedNames []string
	// IssuedNames is the UNION of every create name a writer connection SENT
	// (called RUN for), regardless of outcome. It is the phantom oracle: a
	// recovered name absent from IssuedNames would be a phantom the durable layer
	// invented (recovered ⊆ issued).
	IssuedNames []string
	// FailedNames is the UNION of every create name whose client observed an
	// explicit typed FAILURE (a [proto.Failure] at RUN or as the PULL terminal).
	// It is the atomicity oracle: a commit the client saw fail must have applied
	// nothing durable, so every FailedNames entry MUST be absent after recovery.
	// A name that received an IGNORED terminal (the connection was already in the
	// Bolt FAILED state) is deliberately NOT recorded here — its outcome is
	// ambiguous, so it is left as merely issued, never asserted-absent.
	FailedNames []string

	// TxMarkersAcked / TxMarkersRefused are the transaction-granular ledgers
	// (rmp #2439): the unique marker names created inside transactions whose
	// COMMIT the server acknowledged, and inside transactions the server
	// REFUSED (a typed conflict, an explicit rollback, or a failed commit).
	// At quiescence every acked marker must be present and every refused
	// marker absent — the all-or-nothing assertion at transaction granularity.
	TxMarkersAcked   []string
	TxMarkersRefused []string
	// ContendedAcked is, per shared counter, the number of read-modify-write
	// increments acknowledged; ContendedFinal is each counter's value read at
	// quiescence. Equality is the zero-lost-updates oracle.
	//
	// Both are ALWAYS ConcurrentCounters long, on every path, so a caller may
	// index them by the same k without checking (rmp #2552). A counter no
	// connection touched reads 0 rather than being absent.
	ContendedAcked []int64
	ContendedFinal []int64
	// ContendedConnections is how many connections the seeded role draw gave the
	// contended-writer role. It is POPULATION evidence, not a result: a run that
	// drew none never touches a shared counter, so any counter oracle over it is
	// vacuous — and the harness must still produce well-formed counter evidence
	// for it. It is what lets a test assert it really entered that case instead
	// of replicating the draw and hoping (rmp #2552).
	ContendedConnections int

	Seed             uint64
	Connections      int
	AckedCreates     int64 // nodes connections committed (eventual oracle)
	EngineNodeCount  int64 // engine's live node count at quiescence
	Panics           int64 // recovered panics across all connection goroutines (must be 0)
	TransportErrors  int64 // unexpected transport errors (must be 0 on a healthy run)
	BoundedRejects   int64 // typed bound errors (overload caps) — acceptable, not a fault
	BaselineRoutines int   // goroutine count captured before the run
	FinalRoutines    int   // goroutine count after teardown

	// Transaction outcome accounting (rmp #2439). TxIssued counts explicit
	// transactions BEGUN; every one ends in exactly one bucket: committed
	// (COMMIT acknowledged), conflicted (the typed retriable serialization
	// conflict, classified by its Bolt code), rolled back (a deliberate
	// client ROLLBACK), failed (any other explicit FAILURE), or ambiguous
	// (the connection stopped mid-transaction on a transport error or
	// cancellation — its outcome is unknowable client-side and is never
	// asserted). Conservation: TxIssued == the sum of the five.
	TxIssued     int64
	TxCommitted  int64
	TxConflicted int64
	TxRolledBack int64
	TxFailed     int64
	TxAmbiguous  int64
	// TxMissingAcked / TxPhantomRefused are the quiescence verification
	// outcomes: acknowledged markers absent from the engine, and refused
	// markers present in it. Both must be zero.
	TxMissingAcked   int64
	TxPhantomRefused int64

	// During-run isolation oracle tallies (rmp #2440), reconciled from the
	// per-connection state after the join and asserted zero on HEAD:
	// a monotonic-read regression (a shared counter or the batch population
	// observed going backwards on one connection), a same-connection
	// read-your-own-writes miss, and a torn atomic batch (a count that is not
	// a multiple of the batch size).
	IsoMonotonicViolations int64
	IsoRYOWViolations      int64
	IsoBatchViolations     int64
	// IsoReads counts the oracle observations actually made, so a green run
	// is provably non-vacuous.
	IsoReads int64

	// WireParamFailures holds one description per divergence found by the Bolt
	// parameter type matrix ([probeWireParamTypes], rmp #2462): every PackStream
	// kind a driver can bind — String, Integer, Float, Boolean, Null, List, Map —
	// is sent over the real wire and verified by read-back before any connection
	// spawns. Empty on a clean run; a non-empty slice breaks [Consistent].
	WireParamFailures []string
}

// TxConserved reports whether every issued transaction landed in exactly one
// outcome bucket.
func (r *ConcurrentResult) TxConserved() bool {
	return r.TxIssued == r.TxCommitted+r.TxConflicted+r.TxRolledBack+r.TxFailed+r.TxAmbiguous
}

// Consistent reports whether the eventual-consistency oracle holds: the engine's
// node count equals the acknowledged creates, with no panics and no unexpected
// transport errors, and every Bolt parameter kind round-tripped as specified.
// Bounded rejects (overload caps) are expected and do not break consistency
// because a rejected write is never acknowledged and so is never counted in
// AckedCreates.
func (r *ConcurrentResult) Consistent() bool {
	return r.Panics == 0 &&
		r.TransportErrors == 0 &&
		r.EngineNodeCount == r.AckedCreates &&
		len(r.WireParamFailures) == 0
}

// RunConcurrent drives cfg.Connections concurrent client connections through the
// real Bolt server srv, one goroutine per connection, each performing cfg.OpsPerConn
// seed-derived operations, then waits for every goroutine to finish (quiescence)
// and reconciles the eventual-consistency oracle against the engine. It honours
// ctx cancellation: a cancelled context stops connections at their next op
// boundary and the harness still drains every goroutine before returning.
//
// # Determinism
//
// Per the hybrid model, this mode is NOT bit-reproducible: the SEED fixes each
// connection's role and op sequence, but goroutine interleaving is real and
// non-deterministic. Correctness is guarded by the returned [ConcurrentResult]
// (no panic, no unexpected transport error, eventual oracle==engine) plus the
// caller's goleak check — not by replay.
//
// # Concurrency contract
//
// RunConcurrent spawns exactly cfg.Connections goroutines, each with a defined
// lifecycle bounded by cfg.OpsPerConn and ctx; all are joined before return, so
// no goroutine outlives the call. Every goroutine recovers a panic (recording it
// in the result and terminating cleanly) so one connection's bug cannot crash
// the harness or mask a leak.
func RunConcurrent(ctx context.Context, srv *SimServer, cfg ConcurrentConfig) (ConcurrentResult, error) {
	if cfg.Connections <= 0 {
		cfg.Connections = 1
	}
	if cfg.OpsPerConn <= 0 {
		cfg.OpsPerConn = 1
	}
	mix := cfg.Mix
	if mix == nil {
		mix = defaultConcurrentMix()
	}

	res := ConcurrentResult{
		Seed:             cfg.Seed,
		Connections:      cfg.Connections,
		BaselineRoutines: runtime.NumGoroutine(),
	}

	// The master seed seeds a role/op-stream sub-seed PER CONNECTION up front (on
	// this single goroutine), so the population and each connection's op sequence
	// are a deterministic function of cfg.Seed even though the goroutines then run
	// concurrently. Drawing the sub-seeds here — not inside the goroutines — keeps
	// the draw order independent of the non-deterministic scheduling.
	master := NewSeed(cfg.Seed)
	connSeeds := make([]uint64, cfg.Connections)
	roles := make([]concurrentRole, cfg.Connections)
	haveContended := false
	for i := range connSeeds {
		connSeeds[i] = master.Uint64N(^uint64(0))
		roles[i] = pickRole(master, mix)
		if roles[i] == roleTxContended {
			haveContended = true
			res.ContendedConnections++
		}
	}

	// Bolt parameter type matrix (rmp #2462), before any connection spawns: every
	// PackStream kind a driver can bind is sent over the real wire and verified
	// by read-back. The probe is population-neutral (it deletes the single node
	// it creates), so it perturbs neither the node-count oracle below nor the
	// per-connection seed streams.
	res.WireParamFailures = probeWireParamTypes(ctx, srv)

	if cfg.ContendedCounters <= 0 {
		cfg.ContendedCounters = 2
	}
	// Seed the shared counters BEFORE any contended connection spawns, over a
	// setup connection on this goroutine, so every contended transaction finds
	// its counter committed. The seeded nodes are acknowledged creates like any
	// other, so they enter the eventual-consistency oracle (added to the tally
	// after the post-join load below).
	seededCounters := 0
	if haveContended {
		for k := 0; k < cfg.ContendedCounters; k++ {
			created, err := seedContendedCounter(srv, k)
			if err != nil {
				return res, fmt.Errorf("sim: seed contended counter %d: %w", k, err)
			}
			if created {
				seededCounters++
			}
		}
	}

	var (
		ackedCreates    atomic.Int64
		panics          atomic.Int64
		transportErrors atomic.Int64
		boundedRejects  atomic.Int64
		wg              sync.WaitGroup
	)

	// Per-connection name logs: each goroutine writes ONLY to its own element,
	// so there is no sharing and no lock on the hot path (the reliability
	// mandate's no-hidden-shared-state rule). The harness unions them into the
	// result AFTER wg.Wait, where the writes are already published by the
	// wait's happens-before edge. Model: store/txn group_commit_durability_test's
	// ackedByWorker pattern.
	writerLogs := make([]writerLog, cfg.Connections)

	for i := 0; i < cfg.Connections; i++ {
		wg.Add(1)
		go func(connSeed uint64, role concurrentRole, wl *writerLog) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			runConnection(ctx, srv, connSeed, role, cfg.OpsPerConn, cfg.ContendedCounters, &counters{
				ackedCreates:    &ackedCreates,
				transportErrors: &transportErrors,
				boundedRejects:  &boundedRejects,
			}, wl)
		}(connSeeds[i], roles[i], &writerLogs[i])
	}
	wg.Wait()

	res.AckedCreates = ackedCreates.Load() + int64(seededCounters)
	res.Panics = panics.Load()
	res.TransportErrors = transportErrors.Load()
	res.BoundedRejects = boundedRejects.Load()

	// Union the per-connection name logs now that every writer goroutine has been
	// joined (wg.Wait publishes their appends). The order is not significant — the
	// durable-commit scenario compares SETS — so a simple concatenation suffices.
	res.ContendedAcked = make([]int64, cfg.ContendedCounters)
	for i := range writerLogs {
		res.AckedNames = append(res.AckedNames, writerLogs[i].acked...)
		res.IssuedNames = append(res.IssuedNames, writerLogs[i].issued...)
		res.FailedNames = append(res.FailedNames, writerLogs[i].failed...)
		res.TxMarkersAcked = append(res.TxMarkersAcked, writerLogs[i].txMarkersAcked...)
		res.TxMarkersRefused = append(res.TxMarkersRefused, writerLogs[i].txMarkersRefused...)
		res.TxIssued += writerLogs[i].txIssued
		res.TxCommitted += writerLogs[i].txCommitted
		res.TxConflicted += writerLogs[i].txConflicted
		res.TxRolledBack += writerLogs[i].txRolledBack
		res.TxFailed += writerLogs[i].txFailed
		res.TxAmbiguous += writerLogs[i].txAmbiguous
		res.IsoMonotonicViolations += writerLogs[i].isoMonotonicViolations
		res.IsoRYOWViolations += writerLogs[i].isoRYOWViolations
		res.IsoBatchViolations += writerLogs[i].isoBatchViolations
		res.IsoReads += writerLogs[i].isoReads
		for k, v := range writerLogs[i].contendedAcked {
			res.ContendedAcked[k] += v
		}
	}

	// Transaction-granular quiescence verification (rmp #2439): every
	// acknowledged marker present, every refused marker absent, and every
	// contended counter equal to its acknowledged increments (zero lost
	// updates). Runs over a fresh connection, like the node-count oracle.
	//
	// It reads all cfg.ContendedCounters counters UNCONDITIONALLY, so
	// ContendedFinal comes back sized exactly like ContendedAcked whatever the
	// seeded role draw produced and whether or not any transaction was issued
	// (rmp #2552). Sizing the two by different predicates — the acknowledged
	// tally by the configured counter count, the finals by whether a contended
	// writer happened to be drawn — is what let a caller indexing both by the
	// same k run off the end of one of them, killing the PROCESS rather than
	// failing the run.
	//
	// Reading a counter no connection touched costs one query and is not
	// vacuous: it has no node, so it reads 0, which is exactly its acknowledged
	// total — and a counter that nonetheless holds a value nobody acknowledged is
	// now caught instead of never being looked at.
	missing, phantom, finals, err := verifyTxQuiescence(srv,
		res.TxMarkersAcked, res.TxMarkersRefused, cfg.ContendedCounters)
	if err != nil {
		return res, fmt.Errorf("sim: concurrent tx quiescence verification: %w", err)
	}
	res.TxMissingAcked = missing
	res.TxPhantomRefused = phantom
	res.ContendedFinal = finals

	// Reconcile the eventual-consistency oracle at quiescence: count the engine's
	// live nodes over a fresh connection and compare to the acknowledged creates.
	n, err := queryNodeCount(srv)
	if err != nil {
		return res, fmt.Errorf("sim: concurrent quiescence node-count: %w", err)
	}
	res.EngineNodeCount = n
	res.FinalRoutines = runtime.NumGoroutine()
	return res, nil
}

// concurrentRole is the behaviour a single connection plays for its whole
// lifetime.
type concurrentRole int

const (
	roleWriter concurrentRole = iota
	roleReader
	roleOverload
	roleTxWriter
	roleTxContended
	roleBatchWriter
	roleIsoReader
	roleRYOWWriter
)

// pickRole draws a role from the weighted mix using one float64 from seed.
func pickRole(seed *Seed, mix *ConcurrentMix) concurrentRole {
	weights := [...]struct {
		w    float64
		role concurrentRole
	}{
		{mix.WriterWeight, roleWriter},
		{mix.ReaderWeight, roleReader},
		{mix.OverloadWeight, roleOverload},
		{mix.TxWriterWeight, roleTxWriter},
		{mix.TxContendedWeight, roleTxContended},
		{mix.BatchWriterWeight, roleBatchWriter},
		{mix.IsoReaderWeight, roleIsoReader},
		{mix.RYOWWriterWeight, roleRYOWWriter},
	}
	total := 0.0
	for _, e := range weights {
		total += e.w
	}
	if total <= 0 {
		_ = seed.Float64()
		return roleWriter
	}
	t := seed.Float64() * total
	acc := 0.0
	for _, e := range weights {
		acc += e.w
		if t < acc {
			return e.role
		}
	}
	return roleRYOWWriter
}

// counters bundles the atomic tallies a connection goroutine updates. The
// tallies are SHARED across every connection (they aggregate the whole run), so
// they are atomics; the per-connection name log is separate and unshared (see
// [writerLog]).
type counters struct {
	ackedCreates    *atomic.Int64
	transportErrors *atomic.Int64
	boundedRejects  *atomic.Int64
}

// writerLog records the create names a single writer connection issued, had
// acknowledged, and saw explicitly failed. It is owned by exactly one goroutine
// (never shared), so it needs no synchronisation; the harness reads it only
// after joining that goroutine. Read/overload connections leave it empty.
type writerLog struct {
	issued []string // every create name sent (RUN issued), any outcome
	acked  []string // create names with a SUCCESS-terminated PULL
	failed []string // create names with an explicit proto.Failure (never IGNORED)

	// Transaction-granular ledger (rmp #2439), used by the transactional
	// roles and empty for the others. Same ownership rule: one goroutine,
	// no synchronisation, read only after the join.
	txMarkersAcked   []string
	txMarkersRefused []string
	txIssued         int64
	txCommitted      int64
	txConflicted     int64
	txRolledBack     int64
	txFailed         int64
	txAmbiguous      int64
	// contendedAcked is the per-shared-counter tally of acknowledged
	// read-modify-write increments this connection made (len = the run's
	// contended counter count; nil for other roles).
	contendedAcked []int64

	// During-run isolation oracle state (rmp #2440), goroutine-owned like the
	// rest: last-observed floors, violation tallies, and the observation count
	// that proves a green run non-vacuous.
	isoLastCounter         []int64
	isoLastBatchCount      int64
	isoMonotonicViolations int64
	isoRYOWViolations      int64
	isoBatchViolations     int64
	isoReads               int64
}

// runConnection opens one client connection, plays its role for up to opsPerConn
// operations (stopping early on ctx cancellation), and closes the connection. It
// never panics out: a transport error stops the connection cleanly (recorded in
// the counters), so a connection reset by the server does not crash the harness.
func runConnection(ctx context.Context, srv *SimServer, connSeed uint64, role concurrentRole, opsPerConn, contendedCounters int, c *counters, wl *writerLog) {
	client, err := srv.Dial()
	if err != nil {
		c.transportErrors.Add(1)
		return
	}
	defer func() { _ = client.Close() }()

	if err := client.Connect(ctx); err != nil {
		// A connect failure during shutdown (listener closing) is not a fault if
		// the context is already cancelled; otherwise it is a transport error.
		if ctx.Err() == nil {
			c.transportErrors.Add(1)
		}
		return
	}

	if role == roleTxContended {
		wl.contendedAcked = make([]int64, contendedCounters)
	}
	if role == roleIsoReader {
		wl.isoLastCounter = make([]int64, contendedCounters)
	}
	seed := NewSeed(connSeed)
	uniq := connSeed // per-connection namespace so writers never collide on names
	for op := 0; op < opsPerConn; op++ {
		if ctx.Err() != nil {
			return
		}
		if stop := playOneOp(client, role, seed, uniq, op, contendedCounters, c, wl); stop {
			return
		}
	}
}

// playOneOp performs one operation for the connection's role and returns true if
// the connection should stop (a transport error indicating the server closed it).
func playOneOp(client *WireClient, role concurrentRole, seed *Seed, uniq uint64, op, contendedCounters int, c *counters, wl *writerLog) (stop bool) {
	switch role {
	case roleWriter:
		return writerOp(client, seed, uniq, op, c, wl)
	case roleReader:
		return readerOp(client, seed, c)
	case roleOverload:
		return overloadOp(client, seed, c)
	case roleTxWriter:
		return txWriterOp(client, seed, uniq, op, false, contendedCounters, c, wl)
	case roleTxContended:
		return txWriterOp(client, seed, uniq, op, true, contendedCounters, c, wl)
	case roleBatchWriter:
		return batchWriterOp(client, seed, uniq, op, c, wl)
	case roleIsoReader:
		return isoReaderOp(client, seed, contendedCounters, c, wl)
	case roleRYOWWriter:
		return ryowWriterOp(client, seed, uniq, op, c, wl)
	default:
		return false
	}
}

// writerOp creates one uniquely-named node and counts it as an acknowledged
// create only when the server confirms the commit (SUCCESS-terminated PULL). It
// also records the name into the connection's own [writerLog]: as issued on
// every attempt, as acked on a SUCCESS terminal, and as failed only on an
// EXPLICIT [proto.Failure] (never on IGNORED, whose outcome is ambiguous). The
// name embeds the per-connection seed and the op index, so it is globally unique
// and never reused, which is what lets the durable-commit scenario compare
// recovered/acked/issued/failed as sets.
func writerOp(client *WireClient, seed *Seed, uniq uint64, op int, c *counters, wl *writerLog) (stop bool) {
	name := fmt.Sprintf("c%d-n%d-%d", uniq, op, seed.Uint64N(1<<32))
	wl.issued = append(wl.issued, name)
	resp, err := client.Run(tmplCreatePerson, map[string]any{"name": name, "age": int64(seed.IntN(100))})
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if _, ok := resp.(*proto.Failure); ok {
		// A typed failure at RUN: the statement never streamed, so nothing was
		// committed. Record it as a bounded reject (visible without being a
		// transport fault) and as an explicit failure (asserted absent after a
		// crash). The connection continues; the Bolt server will IGNORE its later
		// messages until RESET, which the durable-commit scenario tolerates.
		c.boundedRejects.Add(1)
		wl.failed = append(wl.failed, name)
		return false
	}
	_, term, err := client.PullAll()
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	switch term.(type) {
	case *proto.Success:
		c.ackedCreates.Add(1)
		wl.acked = append(wl.acked, name)
	case *proto.Failure:
		// The commit reached the durability point and failed there (e.g. a WAL
		// fsync fault poisoned the writer, or a subsequent commit hit the sticky
		// error): the transaction rolled back, so this name applied nothing.
		c.boundedRejects.Add(1)
		wl.failed = append(wl.failed, name)
	default:
		// IGNORED (connection already FAILED) or any other terminal: ambiguous
		// outcome, counted as a bounded reject but NOT asserted absent.
		c.boundedRejects.Add(1)
	}
	return false
}

// readerOp runs a bounded read; a typed failure is a bounded reject, a transport
// error stops the connection.
func readerOp(client *WireClient, seed *Seed, c *counters) (stop bool) {
	q := readTemplates[seed.IntN(len(readTemplates))]
	var params map[string]any
	if q.needsAge {
		params = map[string]any{"age": int64(seed.IntN(100))}
	}
	if _, err := client.Run(q.cypher, params); err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if _, term, err := client.PullAll(); err != nil {
		c.transportErrors.Add(1)
		return true
	} else if _, ok := term.(*proto.Failure); ok {
		c.boundedRejects.Add(1)
	}
	return false
}

// overloadOp issues one bounded overload read; the engine's cap (a typed bound
// error) is the expected, acceptable outcome and is counted as a bounded reject,
// not a fault.
func overloadOp(client *WireClient, seed *Seed, c *counters) (stop bool) {
	fam := OverloadFamily(seed.IntN(overloadFamilyCount))
	// Only the read-shaped families here; a writer connection handles writes. Map
	// a write family to the large-result read so an overload connection stays
	// read-only and does not perturb the acked-create oracle.
	if fam == OverloadLargeCreateTx {
		fam = OverloadLargeResultSet
	}
	out, err := (OverloadActor{}).Run(client, fam)
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if out.BoundedError {
		c.boundedRejects.Add(1)
	}
	return false
}

// queryNodeCount counts the engine's live nodes over a fresh connection at
// quiescence.
func queryNodeCount(srv *SimServer) (int64, error) {
	c, err := srv.Dial()
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		return 0, err
	}
	if _, err := c.Run("MATCH (n) RETURN count(n)", nil); err != nil {
		return 0, err
	}
	records, _, err := c.PullAll()
	if err != nil {
		return 0, err
	}
	if len(records) != 1 || len(records[0].Data) != 1 {
		return 0, fmt.Errorf("sim: node-count query shape unexpected")
	}
	n, ok := records[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("sim: node-count not an int64: %T", records[0].Data[0])
	}
	return n, nil
}

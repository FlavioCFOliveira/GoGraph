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
	// Seed controls WHAT each connection sends and WHEN its faults fire. Goroutine
	// interleaving is NOT seed-controlled (see the package note on the hybrid
	// determinism model): this mode is robustness/liveness/leak-checked, not
	// bit-reproducible.
	Seed uint64
	// Connections is the number of concurrent client connections (one goroutine
	// each). Values <= 0 are normalised to 1.
	Connections int
	// OpsPerConn is the number of operations each connection performs before it
	// closes. Values <= 0 are normalised to 1.
	OpsPerConn int
	// Mix selects the per-connection actor behaviour. When nil, an honest
	// read/write mix is used.
	Mix *ConcurrentMix
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
	Seed             uint64
	Connections      int
	AckedCreates     int64 // nodes connections committed (eventual oracle)
	EngineNodeCount  int64 // engine's live node count at quiescence
	Panics           int64 // recovered panics across all connection goroutines (must be 0)
	TransportErrors  int64 // unexpected transport errors (must be 0 on a healthy run)
	BoundedRejects   int64 // typed bound errors (overload caps) — acceptable, not a fault
	BaselineRoutines int   // goroutine count captured before the run
	FinalRoutines    int   // goroutine count after teardown
}

// Consistent reports whether the eventual-consistency oracle holds: the engine's
// node count equals the acknowledged creates, with no panics and no unexpected
// transport errors. Bounded rejects (overload caps) are expected and do not
// break consistency because a rejected write is never acknowledged and so is
// never counted in AckedCreates.
func (r ConcurrentResult) Consistent() bool {
	return r.Panics == 0 &&
		r.TransportErrors == 0 &&
		r.EngineNodeCount == r.AckedCreates
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
	for i := range connSeeds {
		connSeeds[i] = master.Uint64N(^uint64(0))
		roles[i] = pickRole(master, mix)
	}

	var (
		ackedCreates    atomic.Int64
		panics          atomic.Int64
		transportErrors atomic.Int64
		boundedRejects  atomic.Int64
		wg              sync.WaitGroup
	)

	for i := 0; i < cfg.Connections; i++ {
		wg.Add(1)
		go func(connSeed uint64, role concurrentRole) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			runConnection(ctx, srv, connSeed, role, cfg.OpsPerConn, &counters{
				ackedCreates:    &ackedCreates,
				transportErrors: &transportErrors,
				boundedRejects:  &boundedRejects,
			})
		}(connSeeds[i], roles[i])
	}
	wg.Wait()

	res.AckedCreates = ackedCreates.Load()
	res.Panics = panics.Load()
	res.TransportErrors = transportErrors.Load()
	res.BoundedRejects = boundedRejects.Load()

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
)

// pickRole draws a role from the weighted mix using one float64 from seed.
func pickRole(seed *Seed, mix *ConcurrentMix) concurrentRole {
	total := mix.WriterWeight + mix.ReaderWeight + mix.OverloadWeight
	if total <= 0 {
		_ = seed.Float64()
		return roleWriter
	}
	t := seed.Float64() * total
	if t < mix.WriterWeight {
		return roleWriter
	}
	if t < mix.WriterWeight+mix.ReaderWeight {
		return roleReader
	}
	return roleOverload
}

// counters bundles the atomic tallies a connection goroutine updates.
type counters struct {
	ackedCreates    *atomic.Int64
	transportErrors *atomic.Int64
	boundedRejects  *atomic.Int64
}

// runConnection opens one client connection, plays its role for up to opsPerConn
// operations (stopping early on ctx cancellation), and closes the connection. It
// never panics out: a transport error stops the connection cleanly (recorded in
// the counters), so a connection reset by the server does not crash the harness.
func runConnection(ctx context.Context, srv *SimServer, connSeed uint64, role concurrentRole, opsPerConn int, c *counters) {
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

	seed := NewSeed(connSeed)
	uniq := connSeed // per-connection namespace so writers never collide on names
	for op := 0; op < opsPerConn; op++ {
		if ctx.Err() != nil {
			return
		}
		if stop := playOneOp(client, role, seed, uniq, op, c); stop {
			return
		}
	}
}

// playOneOp performs one operation for the connection's role and returns true if
// the connection should stop (a transport error indicating the server closed it).
func playOneOp(client *WireClient, role concurrentRole, seed *Seed, uniq uint64, op int, c *counters) (stop bool) {
	switch role {
	case roleWriter:
		return writerOp(client, seed, uniq, op, c)
	case roleReader:
		return readerOp(client, seed, c)
	case roleOverload:
		return overloadOp(client, seed, c)
	default:
		return false
	}
}

// writerOp creates one uniquely-named node and counts it as an acknowledged
// create only when the server confirms the commit (SUCCESS-terminated PULL).
func writerOp(client *WireClient, seed *Seed, uniq uint64, op int, c *counters) (stop bool) {
	name := fmt.Sprintf("c%d-n%d-%d", uniq, op, seed.Uint64N(1<<32))
	resp, err := client.Run(tmplCreatePerson, map[string]any{"name": name, "age": int64(seed.IntN(100))})
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if _, ok := resp.(*proto.Failure); ok {
		// A typed failure on an honest create is unexpected here; record it as a
		// bounded reject so a flood of them is visible without being a transport
		// fault.
		c.boundedRejects.Add(1)
		return false
	}
	_, term, err := client.PullAll()
	if err != nil {
		c.transportErrors.Add(1)
		return true
	}
	if _, ok := term.(*proto.Success); ok {
		c.ackedCreates.Add(1)
	} else {
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

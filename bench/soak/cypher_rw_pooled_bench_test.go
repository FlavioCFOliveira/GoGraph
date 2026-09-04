// Package main_test — POOLED-connection Bolt benchmark arm.
//
// # Which arm answers which question
//
// There are two arms, deliberately, and they measure different things:
//
//   - The CHURN arm (BenchmarkBoltReadOnly / WriteOnly / Mixed, in
//     cypher_rw_bench_test.go) opens a fresh TCP connection and completes a full
//     Bolt handshake and HELLO for every single operation. It answers: what does
//     connection establishment cost, and does the server stay correct under
//     connection churn at extreme concurrency? It is NOT a measure of query
//     throughput — the 2026-08-10 certification reported 64 KB and 333
//     allocations per operation for counting 16 nodes, which is the connection,
//     not the query (rmp #2397).
//   - The POOLED arm below establishes one connection per goroutine BEFORE the
//     timer starts, completes the handshake and HELLO outside the timed region,
//     and then loops query round-trips on that live connection. It answers: what
//     does the ENGINE cost per query at this concurrency? This is the arm to
//     quote for queries per second, and it is what real Bolt drivers do, because
//     they all pool connections.
//
// Reporting both is the point: the difference between them IS the handshake cost,
// so the pair localises where the time goes instead of conflating it.
//
// # Reading ns/op
//
// Under b.RunParallel, ns/op is wall-time divided by TOTAL iterations across all
// goroutines, i.e. the inverse of AGGREGATE throughput — not per-client latency.
// Aggregate ops/s is 1e9/ns_per_op; mean per-client latency is ns_per_op x
// concurrency. Conflating the two overstates throughput by a factor of the
// concurrency level.
//
// Usage:
//
//	go test -run='^$' -bench=BenchmarkBoltPooled -benchmem -count=3 \
//	  -timeout=3600s ./bench/soak/...
package main_test

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// Two seed sizes, because they answer two different questions and mixing them
// into one arm would confound both. A first draft did exactly that — it changed
// connection handling AND the workload in the same arm, and measured 1923
// allocs/op against the churn arm's 333, i.e. it looked like a REGRESSION when in
// fact the 2000-node scan had simply swamped the handshake it was meant to
// isolate. Two arms that differ in one respect each:
//
//   - benchSeedMatched (16) is the churn arm's own seed and query, so
//     churn-vs-pooled at this size differs ONLY in connection reuse. The delta is
//     the handshake, which is what rmp #2397 set out to remove from the
//     measurement.
//   - benchSeedWork (2000) makes the label scan real engine work per operation,
//     so the pooled figures at this size are ENGINE throughput rather than a
//     count over 16 nodes.
const (
	benchSeedMatched = 16
	benchSeedWork    = 2000
)

// The matched queries are the churn arm's, verbatim, so the comparison holds.
const (
	benchMatchedReadQuery  = "MATCH (n) RETURN count(n)"
	benchMatchedWriteQuery = "CREATE (n:BenchNode)"
)

// The work-sized read is label-scoped on PURPOSE. `MATCH (n) RETURN count(n)`
// scans every node — including the :BenchNode nodes a write arm keeps creating —
// so its per-operation cost drifts upward as the benchmark runs and the
// measurement becomes a function of how long it has been running. Counting
// :BenchSeed keeps the read's cost constant while writers grow the graph.
const benchWorkReadQuery = "MATCH (n:BenchSeed) RETURN count(n)"

// boltConn is a live, handshaken Bolt session usable for many operations.
type boltConn struct {
	conn net.Conn
	cr   *proto.ChunkedReader
	cw   *proto.ChunkedWriter
	// ops counts operations run on this connection. Exactly one goroutine owns a
	// connection for the whole run, so this needs no synchronisation and gives the
	// mixed arm its 80/20 split without a shared atomic.
	ops int
}

// boltOpen dials addr, negotiates Bolt v5 and completes HELLO, leaving the
// connection ready to run queries. Everything it does is what the pooled arm
// moves OUT of the timed region.
//
// The deadline is absolute and generous rather than per-operation: a
// SetDeadline syscall inside the timed loop would be charged to the query it is
// meant to measure.
func boltOpen(addr string, life time.Duration) (*boltConn, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("boltOpen dial: %w", err)
	}
	// Deliberately NOT SetLinger(0), unlike the churn arm. The churn arm needs an
	// RST-on-close to avoid TIME_WAIT port exhaustion because it opens a
	// connection per operation; the pool opens only `conc` of them and holds them
	// for the whole run, so a graceful FIN costs nothing here and lets the server
	// finish its reply instead of logging a broken pipe on every teardown — noise
	// that would mask a real error.
	if err := conn.SetDeadline(time.Now().Add(life)); err != nil {
		_ = conn.Close() // failing path
		return nil, fmt.Errorf("boltOpen SetDeadline: %w", err)
	}
	if err := boltHandshakeRaw(conn); err != nil {
		_ = conn.Close() // failing path
		return nil, fmt.Errorf("boltOpen negotiate: %w", err)
	}
	c := &boltConn{
		conn: conn,
		cr:   proto.NewChunkedReader(conn),
		cw:   proto.NewChunkedWriter(conn),
	}
	if err := sendMsg(c.cw, &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "bench",
			"credentials": "",
			"agent":       "bench-pooled/1.0",
		},
	}); err != nil {
		_ = conn.Close() // failing path
		return nil, fmt.Errorf("boltOpen sendHello: %w", err)
	}
	if _, err := recvSuccess(c.cr); err != nil {
		_ = conn.Close() // failing path
		return nil, fmt.Errorf("boltOpen recvHello: %w", err)
	}
	return c, nil
}

// runRead executes one auto-commit read: RUN then PULL. This is the whole timed
// operation in the pooled read arm.
func (c *boltConn) runRead(query string) error {
	if err := sendMsg(c.cw, &proto.Run{Query: query, Extra: map[string]interface{}{}}); err != nil {
		return fmt.Errorf("runRead sendRun: %w", err)
	}
	if _, err := recvSuccess(c.cr); err != nil {
		return fmt.Errorf("runRead recvRun: %w", err)
	}
	if err := sendMsg(c.cw, &proto.Pull{N: -1, QID: -1}); err != nil {
		return fmt.Errorf("runRead sendPull: %w", err)
	}
	if err := drainPull(c.cr); err != nil {
		return fmt.Errorf("runRead drainPull: %w", err)
	}
	return nil
}

// runWrite executes one write as its own transaction: BEGIN, RUN, PULL, COMMIT.
//
// BEGIN and COMMIT stay INSIDE the timed region even though the task's sketch put
// BEGIN outside, because a write that never commits is not a write: holding one
// transaction open across every iteration would measure an ever-growing
// uncommitted transaction rather than write throughput. What the pooled arm
// removes is the connection and handshake, not the transaction. A real driver
// doing autocommit writes also opens a transaction per statement.
func (c *boltConn) runWrite(query string) error {
	if err := sendMsg(c.cw, &proto.Begin{Extra: map[string]interface{}{}}); err != nil {
		return fmt.Errorf("runWrite sendBegin: %w", err)
	}
	if _, err := recvSuccess(c.cr); err != nil {
		return fmt.Errorf("runWrite recvBegin: %w", err)
	}
	if err := sendMsg(c.cw, &proto.Run{Query: query, Extra: map[string]interface{}{}}); err != nil {
		return fmt.Errorf("runWrite sendRun: %w", err)
	}
	if _, err := recvSuccess(c.cr); err != nil {
		return fmt.Errorf("runWrite recvRun: %w", err)
	}
	if err := sendMsg(c.cw, &proto.Pull{N: -1, QID: -1}); err != nil {
		return fmt.Errorf("runWrite sendPull: %w", err)
	}
	if err := drainPull(c.cr); err != nil {
		return fmt.Errorf("runWrite drainPull: %w", err)
	}
	if err := sendMsg(c.cw, &proto.Commit{}); err != nil {
		return fmt.Errorf("runWrite sendCommit: %w", err)
	}
	if _, err := recvSuccess(c.cr); err != nil {
		return fmt.Errorf("runWrite recvCommit: %w", err)
	}
	return nil
}

// close says GOODBYE and closes the socket. Errors are ignored: teardown.
func (c *boltConn) close() {
	_ = sendMsg(c.cw, &proto.Goodbye{}) // teardown
	_ = c.conn.Close()                  // teardown
}

// benchPool holds one pre-opened connection per parallel goroutine.
type benchPool struct {
	ch    chan *boltConn
	conns []*boltConn
}

// newBenchPool opens n connections to addr before the caller starts the timer,
// and registers teardown. Opening is CONCURRENT: at n=1024, opening serially
// takes long enough that the first connections would be reaped by the server's
// idle ConnTimeout before the benchmark began.
func newBenchPool(b *testing.B, addr string, n int, life time.Duration) *benchPool {
	b.Helper()
	p := &benchPool{ch: make(chan *boltConn, n), conns: make([]*boltConn, n)}
	errs := make(chan error, n)
	done := make(chan struct{})
	// Dial concurrency is BOUNDED, not n-wide. The host's listen backlog is
	// kern.ipc.somaxconn (128 on the certification host), so firing 1024
	// simultaneous connects overruns the accept queue and the surplus is refused
	// or times out — pool construction would fail for a reason that has nothing to
	// do with the engine. The server accepts continuously, so a bounded window
	// keeps the queue draining while still opening the pool quickly enough that no
	// connection idles past the server's ConnTimeout.
	sem := make(chan struct{}, 64)
	for i := range n {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := boltOpen(addr, life)
			if err != nil {
				errs <- err
				return
			}
			p.conns[i] = c
		}(i)
	}
	for range n {
		<-done
	}
	select {
	case err := <-errs:
		b.Fatalf("newBenchPool: opening %d connections: %v", n, err)
	default:
	}
	for _, c := range p.conns {
		p.ch <- c
	}
	b.Cleanup(func() {
		for _, c := range p.conns {
			if c != nil {
				c.close()
			}
		}
	})
	return p
}

// acquire takes this goroutine's connection. It is deliberately NON-blocking and
// fails loudly: if RunParallel ever spawns more goroutines than the pool was
// sized for, blocking would deadlock the benchmark and dialling a replacement
// would silently put a handshake back inside the timed region — measuring the
// very thing this arm exists to exclude. A loud failure is the only acceptable
// third option.
func (p *benchPool) acquire(b *testing.B) (*boltConn, bool) {
	select {
	case c := <-p.ch:
		return c, true
	default:
		b.Error("benchPool exhausted: more parallel goroutines than pooled connections; " +
			"the pool is sized to GOMAXPROCS, which is what RunParallel uses")
		return nil, false
	}
}

func (p *benchPool) release(c *boltConn) { p.ch <- c }

// runPooledBench is the shared body of the three pooled arms: size the pool to
// the concurrency level, hand each goroutine its own live connection, and time
// only op.
func runPooledBench(b *testing.B, conc, seed int, op func(*boltConn) error) {
	addr := newBenchServerSeeded(b, seed)
	prev := runtime.GOMAXPROCS(conc)
	b.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	// RunParallel spawns GOMAXPROCS x b.parallelism goroutines and parallelism is
	// 1 here, so the pool is sized to conc. GOMAXPROCS is set FIRST so the pool
	// and the goroutine count cannot disagree.
	pool := newBenchPool(b, addr, conc, 30*time.Minute)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c, ok := pool.acquire(b)
		if !ok {
			return
		}
		defer pool.release(c)
		for pb.Next() {
			if err := op(c); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkBoltPooledRead is the pooled counterpart of BenchmarkBoltReadOnly,
// matched to it in seed and query so the only difference is connection reuse.
func BenchmarkBoltPooledRead(b *testing.B) {
	for _, conc := range concurrencyLevels {
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			runPooledBench(b, conc, benchSeedMatched, func(c *boltConn) error {
				return c.runRead(benchMatchedReadQuery)
			})
		})
	}
}

// BenchmarkBoltPooledWrite is the pooled counterpart of BenchmarkBoltWriteOnly:
// one committed transaction per operation, over a live connection.
func BenchmarkBoltPooledWrite(b *testing.B) {
	for _, conc := range concurrencyLevels {
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			runPooledBench(b, conc, benchSeedMatched, func(c *boltConn) error {
				return c.runWrite(benchMatchedWriteQuery)
			})
		})
	}
}

// BenchmarkBoltPooledMixed is the pooled counterpart of BenchmarkBoltMixed: an
// 80/20 read/write mix. Each goroutine counts its own iterations, so the split
// needs no shared atomic.
func BenchmarkBoltPooledMixed(b *testing.B) {
	for _, conc := range concurrencyLevels {
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			runPooledBench(b, conc, benchSeedMatched, mixedOp(
				benchMatchedReadQuery, benchMatchedWriteQuery))
		})
	}
}

// BenchmarkBoltPooledReadWork is the ENGINE-throughput arm: the same pooled
// connection handling, but over benchSeedWork nodes so the query does measurable
// work. These are the figures to quote for reads per second, because neither the
// connection nor a 16-node count dominates them.
func BenchmarkBoltPooledReadWork(b *testing.B) {
	for _, conc := range concurrencyLevels {
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			runPooledBench(b, conc, benchSeedWork, func(c *boltConn) error {
				return c.runRead(benchWorkReadQuery)
			})
		})
	}
}

// mixedOp returns an operation that runs an 80/20 read/write mix, keeping its
// own per-goroutine counter. It is a closure per goroutine because
// runPooledBench calls it once per parallel goroutine.
func mixedOp(readQuery, writeQuery string) func(*boltConn) error {
	// One counter per returned closure would be shared across goroutines, so the
	// counter lives on the connection instead: each goroutine owns exactly one
	// connection for the whole run, which makes the connection the natural
	// per-goroutine state and keeps the split free of any shared atomic.
	return func(c *boltConn) error {
		c.ops++
		if c.ops%5 == 0 { // 1 in 5 → write (20 %)
			return c.runWrite(writeQuery)
		}
		return c.runRead(readQuery) // 4 in 5 → read (80 %)
	}
}

package contention

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
)

// The transport A/B (rmp #2711).
//
// # The question these arms exist to settle
//
// EVERY committed Bolt workload in this package runs over [sim.SimListener], an
// in-memory pipe, so no published Bolt scaling number has ever been taken over a
// socket. Two observations put that in doubt:
//
//   - bolt-wire-rows scales 1.087x at 8, the worst Bolt path measured, and its
//     block profile is 38-53% sync.(*Cond).Wait in the pipe's OWN condvar
//     (internal/sim/simconn.go, halfPipe.waitDeadline).
//   - over a real TCP socket with the official neo4j-go-driver,
//     examples/23_bolt_server scales 2.96x at 8 and 3.97x at 64.
//
// Those two measurements differ in TRANSPORT and in CLIENT and in QUERY, so the
// gap between them cannot be assigned to any one of the three. That is the whole
// reason nothing was established and a spike was opened instead of a fix.
//
// # What makes these arms single-variable
//
// A transport arm here differs from its partner in the [net.Conn] and the
// [net.Listener], and in nothing else:
//
//   - the same [server.Server], built by [newTransportServer] from one options
//     literal, with the same engine constructor ([sim.SimEngineForServer]), the
//     same [server.NoAuthHandler], the same 30 s ConnTimeout and the same
//     discarding logger on BOTH arms (so the slog.Default confound priced by
//     bolt-connect-churn-quiet cannot appear here at all);
//   - the same client: [sim.WireClient], whose [sim.NewWireClientNetConn]
//     constructor exists for exactly this, so both arms emit byte-identical
//     request bytes through the same packstream encoder and the same chunked
//     framer;
//   - the same Cypher TEXT, character for character, and the same expected row
//     count, asserted on every operation;
//   - the same operation count, the same [drive] machinery, and connections
//     PRE-ESTABLISHED in Setup so that no part of connection setup lands inside
//     the measured window. That last point matters asymmetrically: a TCP connect
//     costs far more than a [sim.SimListener] Dial, and leaving it in the window
//     would depress the TCP arm at high levels — biasing the result towards the
//     very conclusion the spike must be able to refute.
//
// # Two queries, because one cannot separate per-message from per-record cost
//
// transport-*-rows runs bolt-wire-rows' exact statement (100 rows, so the
// per-RECORD streaming sink is on the path) and transport-*-count runs
// bolt-wire-read's exact statement (1 row, so it is not). If the pipe taxes the
// per-record path specifically, the rows pair must move and the count pair must
// not. One query alone could not tell those apart.
//
// # These arms are deliberately NOT in [All]
//
// They are a diagnostic pair, not an inventory row: their throughput is a
// property of the harness's transport as much as of the module, and a sweep that
// walked them would publish that as a module number. [TransportWorkload] is the
// only way to reach them, and it takes the level because the pre-dial needs it.

// transportRowLimit is the row count the rows arms ask for. It is
// [boltRowLimit] rather than a second constant so the two cannot drift: an arm
// that no longer matched bolt-wire-rows' row count would no longer be measuring
// bolt-wire-rows' path.
const transportRowLimit = boltRowLimit

const (
	// TransportSim is the in-memory [sim.SimListener] pipe every committed
	// Bolt workload runs over.
	TransportSim = "sim"
	// TransportTCP is a real loopback socket on 127.0.0.1.
	TransportTCP = "tcp"

	// The five arms below are ATTRIBUTION PROBES, not proposals. Each removes
	// one part of the deadline machinery from the SERVER side of the
	// connection and changes nothing else, so the delta prices that part.
	//
	// They are sound as probes for this workload and unsound as production
	// behaviour, and the distinction rests on a fact rather than on a hope: a
	// [transportConnTimeout] of 30 s cannot fire inside a measured window that
	// never reaches 3 s, so for these runs the deadline the server installs is
	// dead weight by construction. An arm that dropped a deadline which COULD
	// fire would be measuring a different server.
	//
	// # What each one isolates
	//
	// bolt/server calls conn.SetReadDeadline before EVERY read
	// (bolt/server/serve.go:1109) and conn.SetWriteDeadline before every
	// response record (serve.go:1409). On a [sim.SimConn] each of those costs
	// more than it does on a socket, and for two independent reasons:
	//
	//  1. halfPipe.setReadDeadline / setWriteDeadline take the pipe's mutex and
	//     Broadcast its condvar (internal/sim/simconn.go). The two directions
	//     of a SimConn each have ONE mutex and ONE cond shared by that
	//     direction's reader and writer, so a Broadcast issued to arm a
	//     deadline also wakes the peer that is parked waiting for BYTES. That
	//     is a spurious wake-up, and on a 100-row response there is one per
	//     record.
	//  2. halfPipe.waitDeadline, when the deadline is non-zero, arms a
	//     clock.Timer AND SPAWNS A GOROUTINE, per blocking wait
	//     (simconn.go:143-154). A request/response server blocks on every
	//     inbound read, so that is one goroutine, one timer and one channel per
	//     inbound message. A socket pays neither: SetReadDeadline on a netFD
	//     updates a runtime poller deadline and wakes nobody.
	//
	// Reading a deadline of ZERO takes waitDeadline's IsZero fast path, so
	// clearing the read deadline removes (2) while KEEPING (1); dropping the
	// call removes both. That is what separates the two mechanisms.

	// TransportSimReadDeadlineCleared converts the server's per-read deadline
	// to the zero Time: the mutex and Broadcast still happen, the per-wait
	// timer and goroutine do not.
	TransportSimReadDeadlineCleared = "sim-rdl-clear"
	// TransportSimReadDeadlineDropped drops the per-read deadline call
	// entirely: neither the Broadcast nor the per-wait timer happens.
	TransportSimReadDeadlineDropped = "sim-rdl-drop"
	// TransportSimWriteDeadlineDropped drops the per-record write deadline
	// call entirely, which on the pipe is one mutex acquisition and one
	// spurious Broadcast per record.
	TransportSimWriteDeadlineDropped = "sim-wdl-drop"
	// TransportSimNoDeadlines drops both.
	TransportSimNoDeadlines = "sim-no-dl"
	// TransportTCPNoDeadlines drops both on the socket. It is the CONTROL: the
	// same removal, on a transport where the deadline is cheap, must buy
	// little or nothing. Without it, a gain on the pipe arms could be the
	// removal of any per-message work rather than of the pipe's own.
	TransportTCPNoDeadlines = "tcp-no-dl"

	// TransportCommitted is not a transport at all: it is the COMMITTED
	// workload of the same shape, taken straight from [ByName], driven through
	// this campaign's replica machinery.
	//
	// It is the FIDELITY control, and it is here because without it the whole
	// campaign rests on an unchecked assumption. The sim arm is built to stand
	// in for bolt-wire-rows and bolt-wire-read, but it is not identical to
	// them: it discards the server log where they let it fall through to
	// slog.Default, it pre-dials its connections where they dial lazily inside
	// the first operation, and it reaches the client through
	// [sim.NewWireClientNetConn] rather than [sim.NewWireClient]. Whether those
	// three differences move the number is a question, not a given — and it
	// became a live one when the sim arm read 0.920 at level 8 against the
	// published bolt-wire-rows row's 1.087.
	//
	// The arm overrides the committed workload's Ops with this campaign's, so
	// the two arms do the same total work. A scaling ratio is a rate and is
	// therefore invariant to that, but the WALL CLOCK of a cell is not, and two
	// arms of one interleaved campaign must cost the same or the interleaving
	// stops being balanced.
	TransportCommitted = "committed"

	// The three arms below BISECT the gap between the sim arm and the
	// committed workload it stands in for. Each reverts exactly one of the
	// three known construction differences and leaves the other two, so the
	// delta against plain sim prices that one difference.
	//
	// They exist because the fidelity control found a gap and an unexplained
	// gap between two instruments measuring the same thing is a defect in at
	// least one of them.

	// TransportSimLazyDial dials each worker's connection inside its FIRST
	// operation through a padded [perWorker] table, exactly as boltClient
	// does, instead of pre-dialling every connection in Setup.
	TransportSimLazyDial = "sim-lazydial"
	// TransportSimDefaultLog leaves Options.Logger nil, so server.NewServer
	// falls through to slog.Default as [sim.NewSimServer] does, instead of
	// installing a discarding handler.
	TransportSimDefaultLog = "sim-defaultlog"
	// TransportSimConnClient builds the client with [sim.NewWireClient] over
	// the concrete *SimConn, as every committed Bolt workload does, instead of
	// [sim.NewWireClientNetConn] over the net.Conn interface.
	TransportSimConnClient = "sim-simconn-client"

	// TransportTCPLazyDial is [TransportSimLazyDial] on the socket, and it is
	// REQUIRED, not optional.
	//
	// The bisect above found that dialling in Setup rather than inside the
	// worker's first operation costs the pipe arm 25% of its level-8
	// throughput. If that penalty is a property of the pipe, then a pre-dialled
	// sim arm compared against a pre-dialled tcp arm understates the pipe and
	// inflates the headline. The A/B must therefore be taken with the dial site
	// held constant AND known to be neutral on both sides, which is what this
	// arm establishes.
	TransportTCPLazyDial = "tcp-lazydial"
)

// deadlineMode says what a decorated connection does with a deadline the server
// asks it to install.
type deadlineMode int

const (
	// deadlinePass installs the deadline the server asked for.
	deadlinePass deadlineMode = iota
	// deadlineClear installs the ZERO time instead, which means "no deadline"
	// to both a [sim.SimConn] and a socket.
	deadlineClear
	// deadlineDrop does not call the underlying connection at all.
	deadlineDrop
)

// transportSpec is how a named arm is built.
type transportSpec struct {
	// base is the transport underneath: [TransportSim] or [TransportTCP].
	base string
	// read and write are what the arm does with the server's deadline calls.
	readMode, writeMode deadlineMode
	// lazyDial dials each worker's connection inside its first operation
	// rather than in Setup. See [TransportSimLazyDial].
	lazyDial bool
	// defaultLog leaves Options.Logger nil. See [TransportSimDefaultLog].
	defaultLog bool
	// simConnClient builds the client over the concrete *SimConn. See
	// [TransportSimConnClient].
	simConnClient bool
}

// transportSpecs is the complete set of arms, keyed by name.
var transportSpecs = map[string]transportSpec{
	TransportSim:                     {base: TransportSim},
	TransportCommitted:               {base: TransportCommitted},
	TransportTCP:                     {base: TransportTCP},
	TransportSimReadDeadlineCleared:  {base: TransportSim, readMode: deadlineClear},
	TransportSimReadDeadlineDropped:  {base: TransportSim, readMode: deadlineDrop},
	TransportSimWriteDeadlineDropped: {base: TransportSim, writeMode: deadlineDrop},
	TransportSimNoDeadlines:          {base: TransportSim, readMode: deadlineDrop, writeMode: deadlineDrop},
	TransportTCPNoDeadlines:          {base: TransportTCP, readMode: deadlineDrop, writeMode: deadlineDrop},
	TransportSimLazyDial:             {base: TransportSim, lazyDial: true},
	TransportSimDefaultLog:           {base: TransportSim, defaultLog: true},
	TransportSimConnClient:           {base: TransportSim, simConnClient: true},
	TransportTCPLazyDial:             {base: TransportTCP, lazyDial: true},
}

// deadlineConn decorates the SERVER side of a connection, applying the arm's
// deadline policy. Every other net.Conn method is the embedded connection's.
type deadlineConn struct {
	net.Conn
	readMode, writeMode deadlineMode
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	switch c.readMode {
	case deadlineDrop:
		return nil
	case deadlineClear:
		return c.Conn.SetReadDeadline(time.Time{})
	default:
		return c.Conn.SetReadDeadline(t)
	}
}

func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	switch c.writeMode {
	case deadlineDrop:
		return nil
	case deadlineClear:
		return c.Conn.SetWriteDeadline(time.Time{})
	default:
		return c.Conn.SetWriteDeadline(t)
	}
}

// SetDeadline routes each half through its own policy, so an arm that drops one
// direction cannot silently reinstate it through the combined setter. The Bolt
// server calls this once, for the handshake (bolt/server/serve.go:965), which
// on these arms happens during the pre-dial and therefore outside the measured
// window.
func (c *deadlineConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// deadlineListener decorates every accepted connection with the arm's policy.
type deadlineListener struct {
	net.Listener
	readMode, writeMode deadlineMode
}

func (l *deadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &deadlineConn{Conn: c, readMode: l.readMode, writeMode: l.writeMode}, nil
}

// transportQuery is one of the two byte-identical statements the arms drive.
type transportQuery struct {
	// cypher is the statement text, which MUST be identical across transports.
	cypher string
	// wantRecords is asserted on every operation. Without it an arm would pass
	// just as happily against a server that returned nothing, and would then be
	// measuring the message path instead of the streaming path.
	wantRecords int
	// ops is the total operation count, fixed per query rather than per level
	// so every level does the same total work.
	ops int
	// surface names what the statement puts under the ladder.
	surface string
	// committed is the name of the registry workload of the same shape, which
	// [TransportCommitted] drives. It is the fidelity control's target.
	committed string
}

// transportQueries are the two statements, keyed by the short name the child
// process is given on its command line.
//
// The rows statement is byte-identical to bolt-wire-rows' and the count
// statement byte-identical to bolt-wire-read's, so each arm measures a committed
// workload's own path rather than a near neighbour of it.
//
// The operation counts differ between the two queries and that is deliberate:
// what must be equal is the count across TRANSPORTS, which is what the ratio
// divides out. The rows count matches bolt-wire-rows exactly (20000) so its sim
// arm is directly comparable with the published row. The count query's is 40000
// rather than bolt-wire-read's 100000 because the socket arm runs it at roughly
// a quarter of the pipe arm's rate and 100000 would put the level-1 TCP cell
// well past the campaign's affordable window; the reduction is applied to BOTH
// arms, so no ratio is affected by it.
var transportQueries = map[string]transportQuery{
	"rows": {
		cypher:      fmt.Sprintf("UNWIND range(1, %d) AS i RETURN i", transportRowLimit),
		wantRecords: transportRowLimit,
		ops:         20000,
		surface:     "bolt/server streaming sink, bolt/proto chunking, bolt/packstream encode",
		committed:   "bolt-wire-rows",
	},
	// rows1k is the THIRD point on the per-operation cost lever, and it is the
	// experiment that discriminates between the two explanations of the
	// transport gap.
	//
	// A socket adds a roughly FIXED cost per operation that a pipe does not.
	// If the scaling difference between the two transports is that fixed cost
	// diluting a shared serial component, then making the operation itself far
	// more expensive must SHRINK the ratio between the two arms — the added
	// transport cost becomes a smaller share of the whole. If instead the pipe
	// serialises something, the ratio should hold or grow with the work.
	//
	// count and rows already give two points at about 13 and 92 microseconds
	// per operation on the pipe. rows1k gives a third an order of magnitude
	// above rows, so the trend is read from three points rather than asserted
	// from two. 1000 rows of small integers stay well inside the pipe's 64 KiB
	// buffer, so the arm does not silently change regime by provoking the
	// backpressure path as well.
	"rows1k": {
		cypher:      "UNWIND range(1, 1000) AS i RETURN i",
		wantRecords: 1000,
		ops:         2000,
		surface:     "bolt/server streaming sink, bolt/proto chunking, bolt/packstream encode [1000 rows]",
		committed:   "",
	},
	"count": {
		cypher:      "MATCH (n) RETURN count(n)",
		wantRecords: 1,
		ops:         40000,
		surface:     "bolt/server message loop, bolt/proto, cypher read path",
		committed:   "bolt-wire-read",
	},
}

// TransportQueryNames returns the query keys [TransportWorkload] accepts, sorted.
//
// It returns a fresh slice on every call and is safe for concurrent use.
func TransportQueryNames() []string {
	names := make([]string, 0, len(transportQueries))
	for k := range transportQueries {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// TransportKinds returns the two transports the A/B compares. It deliberately
// excludes the attribution probes: a campaign that swept them alongside the two
// real transports would report probe throughput in the same table as module
// throughput.
//
// It returns a fresh slice on every call and is safe for concurrent use.
func TransportKinds() []string { return []string{TransportSim, TransportTCP} }

// TransportArmNames returns every arm [TransportWorkload] accepts, sorted.
//
// It returns a fresh slice on every call and is safe for concurrent use.
func TransportArmNames() []string {
	names := make([]string, 0, len(transportSpecs))
	for k := range transportSpecs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// TransportWorkloadName is the stable name of one arm, used in filenames and in
// the report.
func TransportWorkloadName(kind, query string) string {
	return fmt.Sprintf("transport-%s-%s", kind, query)
}

// TransportWorkload builds one arm of the transport A/B.
//
// kind is [TransportSim] or [TransportTCP]; query is one of
// [TransportQueryNames]. level is the goroutine count the arm will be driven at,
// and it is a parameter because the arm PRE-ESTABLISHES one connection per
// worker during Setup, keeping connection cost out of the measured window on
// both arms.
//
// The returned [Workload] is an immutable description and is safe for concurrent
// use; everything mutable lives in what its Setup builds.
func TransportWorkload(kind, query string, level int) (Workload, error) {
	q, ok := transportQueries[query]
	if !ok {
		return Workload{}, fmt.Errorf("unknown transport query %q, want one of %s",
			query, strings.Join(TransportQueryNames(), ", "))
	}
	if _, ok := transportSpecs[kind]; !ok {
		return Workload{}, fmt.Errorf("unknown transport %q, want one of %s",
			kind, strings.Join(TransportArmNames(), ", "))
	}
	if level < 1 {
		return Workload{}, fmt.Errorf("level must be >= 1, got %d", level)
	}
	if kind == TransportCommitted {
		if q.committed == "" {
			return Workload{}, fmt.Errorf("query %q has no committed counterpart to compare against", query)
		}
		w, ok := ByName(q.committed)
		if !ok {
			return Workload{}, fmt.Errorf("committed workload %q not in the registry", q.committed)
		}
		w.Name = TransportWorkloadName(kind, query)
		w.Surface += " [committed registry workload]"
		w.Ops = q.ops
		return w, nil
	}
	name := TransportWorkloadName(kind, query)
	return Workload{
		Name:    name,
		Surface: q.surface + " [transport=" + kind + "]",
		Ops:     q.ops,
		Setup: func(_ string) (Op, func() error, error) {
			return setupTransportArm(kind, q, level)
		},
	}, nil
}

// setupTransportArm starts one server on the requested transport and
// pre-connects one client per worker.
func setupTransportArm(kind string, q transportQuery, level int) (Op, func() error, error) {
	spec := transportSpecs[kind]
	ts, err := newTransportServer(kind)
	if err != nil {
		return nil, nil, err
	}
	// connect turns a fresh transport connection into a ready-to-query client.
	connect := func(ctx context.Context) (*sim.WireClient, error) {
		conn, err := ts.dial()
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", kind, err)
		}
		var c *sim.WireClient
		if spec.simConnClient {
			sc, ok := conn.(*sim.SimConn)
			if !ok {
				_ = conn.Close()
				return nil, fmt.Errorf("arm %s wants a *sim.SimConn, got %T", kind, conn)
			}
			c = sim.NewWireClient(sc, clock.Real())
		} else {
			c = sim.NewWireClientNetConn(conn, clock.Real())
		}
		if err := c.Connect(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("connect %s: %w", kind, err)
		}
		return c, nil
	}

	if spec.lazyDial {
		return setupTransportArmLazy(q, ts, connect)
	}

	clients := make([]*sim.WireClient, 0, level)
	teardown := func() error {
		var errs []error
		for _, c := range clients {
			if err := c.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		errs = append(errs, ts.Close())
		return errors.Join(errs...)
	}
	ctx := context.Background()
	for i := 0; i < level; i++ {
		c, err := connect(ctx)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("worker %d: %w", i, err), teardown())
		}
		clients = append(clients, c)
	}
	op := func(_ context.Context, worker, _ int) error {
		c := clients[worker]
		reply, err := c.Run(q.cypher, nil)
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		// A FAILURE arrives as a value with a nil error, and the PULL that
		// follows it is IGNORED. The record-count assertion below would catch
		// that too, but only as "0 records", which reads as an empty result
		// rather than as a refused statement.
		if f, ok := reply.(*proto.Failure); ok {
			return fmt.Errorf("run refused: %s %s", f.Code, f.Message)
		}
		recs, _, err := c.PullAll()
		if err != nil {
			return fmt.Errorf("pull: %w", err)
		}
		if len(recs) != q.wantRecords {
			return fmt.Errorf("pull: got %d records, want %d", len(recs), q.wantRecords)
		}
		return nil
	}
	return op, teardown, nil
}

// setupTransportArmLazy is the [TransportSimLazyDial] variant: connections are
// established inside each worker's first operation, through the same padded
// [perWorker] table the committed Bolt workloads use, so the arm differs from
// the committed construction in the logger and the client constructor only.
func setupTransportArmLazy(
	q transportQuery,
	ts *transportServer,
	connect func(context.Context) (*sim.WireClient, error),
) (Op, func() error, error) {
	clients := newPerWorker[*sim.WireClient]()
	teardown := func() error {
		var errs []error
		clients.each(func(c **sim.WireClient) {
			if *c != nil {
				if err := (*c).Close(); err != nil {
					errs = append(errs, err)
				}
			}
		})
		errs = append(errs, ts.Close())
		return errors.Join(errs...)
	}
	op := func(ctx context.Context, worker, _ int) error {
		slot, err := clients.get(worker)
		if err != nil {
			return err
		}
		if *slot == nil {
			c, err := connect(ctx)
			if err != nil {
				return fmt.Errorf("worker %d: %w", worker, err)
			}
			*slot = c
		}
		c := *slot
		reply, err := c.Run(q.cypher, nil)
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		if f, ok := reply.(*proto.Failure); ok {
			return fmt.Errorf("run refused: %s %s", f.Code, f.Message)
		}
		recs, _, err := c.PullAll()
		if err != nil {
			return fmt.Errorf("pull: %w", err)
		}
		if len(recs) != q.wantRecords {
			return fmt.Errorf("pull: got %d records, want %d", len(recs), q.wantRecords)
		}
		return nil
	}
	return op, teardown, nil
}

// transportServer is a real bolt/server serving on one of the two transports.
//
// Both arms are built by this one function from one [server.Options] literal, so
// no server-side option can differ between them by accident. That is not a
// stylistic preference: bolt-connect-churn-quiet exists in this package because
// two Bolt arms DID differ in their logger, and the delta had to be priced
// separately before either number could be read.
type transportServer struct {
	srv      *server.Server
	ln       net.Listener
	dial     func() (net.Conn, error)
	cancel   context.CancelFunc
	serveErr chan error
}

// transportConnTimeout matches the 30 s a [sim.SimServer] installs, so the
// server-side per-read deadline behaves identically on both arms.
const transportConnTimeout = 30 * time.Second

// newTransportServer starts a bolt/server on the given transport and returns a
// handle that can dial clients to it.
func newTransportServer(kind string) (*transportServer, error) {
	spec, ok := transportSpecs[kind]
	if !ok {
		return nil, fmt.Errorf("unknown transport %q", kind)
	}
	if spec.base == TransportCommitted {
		return nil, fmt.Errorf("transport %q is served by the registry workload, not by this constructor", kind)
	}
	var (
		ln   net.Listener
		dial func() (net.Conn, error)
	)
	switch spec.base {
	case TransportSim:
		sl := sim.NewSimListener(clock.Real())
		ln = sl
		dial = func() (net.Conn, error) {
			c, err := sl.Dial()
			if err != nil {
				return nil, err
			}
			return c, nil
		}
	case TransportTCP:
		tl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen tcp: %w", err)
		}
		ln = tl
		addr := tl.Addr().String()
		dial = func() (net.Conn, error) {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				return nil, err
			}
			// Go enables TCP_NODELAY by default; setting it explicitly records
			// the assumption in the code rather than relying on a default that
			// a reader would otherwise have to take on trust. Without it,
			// Nagle would batch the client's small request messages and the
			// arm would measure the algorithm, not the transport.
			if tc, ok := c.(*net.TCPConn); ok {
				if err := tc.SetNoDelay(true); err != nil {
					_ = c.Close()
					return nil, fmt.Errorf("set nodelay: %w", err)
				}
			}
			return c, nil
		}
	default:
		return nil, fmt.Errorf("unknown base transport %q", spec.base)
	}

	// The decoration goes on the listener the SERVER sees, never on the client
	// end: the deadline calls under test are the server's.
	serveLn := ln
	if spec.readMode != deadlinePass || spec.writeMode != deadlinePass {
		serveLn = &deadlineListener{Listener: ln, readMode: spec.readMode, writeMode: spec.writeMode}
	}

	var logger *slog.Logger
	if !spec.defaultLog {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv, err := server.NewServer(sim.SimEngineForServer(), server.Options{
		Auth:        server.NoAuthHandler{},
		ConnTimeout: transportConnTimeout,
		// Discarded on both A/B arms. server.NewServer logs a NoAuth and a
		// no-TLS warning per construction through slog.Default otherwise, and
		// slog's default handler serialises on one mutex writing to a pipe.
		// [TransportSimDefaultLog] is the arm that prices leaving it nil.
		Logger: logger,
	})
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("new server (%s): %w", kind, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := &transportServer{
		srv:      srv,
		ln:       ln,
		dial:     dial,
		cancel:   cancel,
		serveErr: make(chan error, 1),
	}
	go func() { ts.serveErr <- srv.Serve(ctx, serveLn) }()
	return ts, nil
}

// Close cancels the serve context and waits for the server to drain.
//
// [server.Server.Serve] closes the listener itself once its accept context is
// cancelled and then waits for every connection goroutine, so the listener is
// deliberately NOT closed here: doing so would race the server's own close.
func (t *transportServer) Close() error {
	t.cancel()
	select {
	case err := <-t.serveErr:
		return err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("transport server (%T): serve goroutine did not exit", t.ln)
	}
}

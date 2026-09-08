# Bolt v5 Server

GoGraph includes a Bolt v5 server compatible with `neo4j-go-driver` v5 and `cypher-shell`.

## Quick start

```go
import (
    "context"
    "log"

    "github.com/FlavioCFOliveira/GoGraph/bolt/server"
    "github.com/FlavioCFOliveira/GoGraph/cypher"
    "github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
    "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Directed and Multigraph are both required for openCypher semantics:
// relationships are directed, and the data model is a multigraph, so a CREATE
// always adds a relationship (including a parallel edge between an existing
// node pair). Weightless is the right choice here because Cypher has no
// edge-weight concept, so the per-node weight column carries no information;
// see docs/cypher.md "Graph configuration" for the full rationale and the one
// case that excludes it.
g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true, Weightless: true})
eng := cypher.NewEngine(g)

// NewServer returns an error and is secure-by-default: it refuses a nil Auth
// with ErrNoAuthHandler rather than silently accepting every client. Use the
// explicit NoAuthHandler{} value to opt out, for development only — it logs a
// warning. See Authentication below for a real handler.
srv, err := server.NewServer(eng, server.Options{
    MaxConnections: 1024,
    Auth:           server.NoAuthHandler{},
})
if err != nil {
    log.Fatalf("bolt: %v", err)
}
go func() {
    if err := srv.ListenAndServe(context.Background(), ":7687"); err != nil {
        log.Printf("bolt: %v", err)
    }
}()
```

## Supported Bolt versions

- Bolt 5.0–5.6 (preferred)
- Bolt 4.4 (fallback)

## Authentication

```go
opts := server.Options{
    Auth: server.BasicAuthHandler{
        Validate: func(user, pass string) error {
            if user != "neo4j" || pass != "password" {
                return server.ErrAuthFailed
            }
            return nil
        },
    },
}
```

## TLS

The server wraps an accepted connection with whatever `*tls.Config` it is given,
verbatim: it imposes no minimum version and no cipher policy of its own, and a
nil `TLSConfig` means plain TCP (`NewServer` logs a warning in that case).
`DefaultTLSConfig` is the hardened starting point — a TLS 1.2 floor and an
AEAD-only ECDHE cipher list for the 1.2 handshake, with TLS 1.3 negotiated
automatically. It carries no certificate, so the caller must supply one:

```go
cfg := server.DefaultTLSConfig()
cfg.Certificates = []tls.Certificate{cert}
opts := server.Options{TLSConfig: cfg}
```

To rotate certificates without restarting the server, wire the on-disk
reloader instead of a fixed certificate. `CertReloader` swaps the live
certificate only once the new pair reads, parses, pairs, and its leaf is valid
at the current instant; a reload that fails any of those leaves the working
certificate in service and reports the error through `onError`:

```go
reloader, err := server.NewCertReloader("/etc/gograph/tls.crt", "/etc/gograph/tls.key", nil)
if err != nil {
    log.Fatalf("bolt: tls: %v", err)
}
go reloader.Watch(1*time.Minute, stop)

cfg := server.DefaultTLSConfig()
cfg.GetCertificate = reloader.GetCertificate
opts := server.Options{TLSConfig: cfg}
```

## Limits and backpressure

`Options` exposes the bounds that protect the server under load. All of them
fall back to a default when left at the zero value:

| Field | Default | Effect |
|---|---|---|
| `MaxConnections` | 1024 | Upper bound on concurrent connections. When the limit is reached, a newly accepted connection is closed immediately rather than queued. |
| `MaxMessageBytes` | `proto.DefaultMaxMessageBytes` (16 MiB) | Caps the cumulative payload of one Bolt message reassembled across chunks, closing the Slowloris-style vector of an unbounded chunk stream. |
| `MaxInFlightPerConnection` | `DefaultMaxInFlightPerConnection` (1024) | Caps the number of `RUN` statements issued inside a single explicit transaction before `COMMIT`/`ROLLBACK`. Exceeding it returns a `Neo.ClientError.General.LimitExceeded` failure. Auto-commit cursors are not counted. |
| `ConnTimeout` | `DefaultConnTimeout` (30 s) | Per-connection idle read deadline, reset before each message read. It cannot be disabled: a zero or negative value takes the default, because a connection that completes the handshake and then falls silent would otherwise hold its slot and goroutine forever. The unauthenticated handshake is bounded separately and is not configurable here (`DefaultHandshakeTimeout`, 10 s). |
| `DatabaseName` | `DefaultDatabaseName` (`neo4j`) | The name reported in result metadata for a client that selects no database. A client that names one has its own name echoed back. See the `db` note under [Protocol conformance notes](#protocol-conformance-notes). |
| `MaxTxIdleTime` | `DefaultMaxTxIdleTime` (30 min) | How long an **open** explicit transaction may go without the client sending a message, after which it is rolled back. Distinct from `DefaultTxTimeout`, which caps total lifetime however busy the transaction is. Cannot be disabled. Raised from 5 s by rmp #2806; under the default configuration it is usually not the bound that fires, because `ConnTimeout` (30 s) tears a silent connection down first — see [Abandoned transactions](#abandoned-transactions). |
| `MaxOpenTxPerPrincipal` | `DefaultMaxOpenTxPerPrincipal` (2048) | How many explicit transactions one authenticated principal may hold **open** at once, across all its connections. Exceeding it fails the `BEGIN` with `Neo.TransientError.Transaction.MaximumTransactionLimitReached` — Neo4j's own TRANSIENT code, so a driver retries — and the session stays in **READY**, so the retry needs no `RESET` (rmp #2561). A negative value disables it. Under the default configuration the quota cannot bind: a connection holds at most one open transaction and `MaxConnections` defaults to 1024, so the connection ceiling is reached first. It binds again for an operator who raises `MaxConnections` above 2048, and it is what isolates one principal from another (rmp #2419). |
| `MaxInboundDecodeBytes` | derived, or `DefaultMaxInboundDecodeBytes` (1 GiB) | Engine-wide ceiling on the decoded-collection memory in flight across **all** connections while messages are being decoded, so the per-message cap multiplied by `MaxConnections` is not the real bound. Zero derives one eighth of `GOMEMLIMIT` when the operator has set one, and falls back to 1 GiB when they have not; `MaxInboundDecodeBytesUnlimited` (-1) opts out. When the pool is drawn down, further inbound decodes fail fast with a retryable transient error rather than allocating. |
| `DefaultTxTimeout` | `DefaultTxTimeout` (30 min) | Total wall-clock lifetime of an explicit transaction when the client sends no `tx_timeout` of its own, however busy it is. A client-supplied `tx_timeout` takes precedence. Raised from 30 s by rmp #2806, so a long batch transaction completes under the default configuration; the cost is that a transaction which keeps talking but never finishes holds its versions and its horizon slot for up to half an hour. |
| `DefaultStatementTimeout` | `DefaultStatementTimeout` (30 min) | The autocommit counterpart: the bound applied to a bare `RUN` outside an explicit transaction when the client supplies no `timeout`. Without it, autocommit `RUN` was the one unbounded-runtime path on a default server, because a super-linear query with a single-row result never trips the row or byte caps. Raised from 30 s by rmp #2806, which matches it to `DefaultTxTimeout` again; the cost is that such a statement pins a CPU core for up to half an hour. Note that `ConnTimeout` bounds a single statement too — the read deadline runs while the message loop executes it, and a client waiting for its own records sends nothing — so raising this past `ConnTimeout` (30 s) does nothing for one long statement unless `ConnTimeout` is raised with it. Set `MaxStatementTimeout`, or this field, to bound it more tightly. |
| `MaxStatementTimeout` | 0 (no server-side cap) | Server-side ceiling on per-statement execution time. A client-supplied timeout is silently clamped to it, and when positive it is applied unconditionally to a client that supplies none. |

The four remaining fields are not bounds and so are not in the table: `Auth`
(required — see [Authentication](#authentication)), `TLSConfig` (see [TLS](#tls)),
`Closer` (see [Deployment](#deployment)), and `Logger`, the structured logger that
receives both accept-loop and session-level events; leave it nil for the default
`slog` handler.

### Abandoned transactions

**An abandoned transaction no longer blocks anybody.** Until rmp #2305/#2306 an
open explicit **write** transaction held the engine's writer serialisation and its
global visibility barrier for its whole lifetime, so a client that sent `BEGIN` and
then stopped talking stalled every other reader and writer on the server; that is
the outage the bounds below were built for, and it is gone. Two gates in
`bolt/server/e2e_concurrent_write_tx_test.go` assert its absence against the
official driver: `TestE2E_TwoExplicitWriteTransactionsOverlap` and
`TestE2E_AnIdleExplicitTransactionDoesNotStallAnotherWriter`.

What an abandoned transaction still costs is **memory**, not other clients'
progress: it pins an MVCC read snapshot, so no version it could still reach is
reclaimable while it lives, and it occupies one of the reclamation horizon's fixed
number of slots. An availability failure became a memory-and-slot failure, and
neither is acceptable without a bound — which is why both bounds remain, and why
raising `MaxTxIdleTime` is now a memory-growth risk rather than an availability one.

**rmp #2806 took that risk deliberately.** `MaxTxIdleTime` went from 5 s to
30 minutes, and `DefaultTxTimeout` and `DefaultStatementTimeout` from 30 s to the
same 30 minutes. Where the idle bound is what fires, one abandoned transaction now
holds its versions and one horizon slot for **half an hour** after its last message
— 360 times longer than before — and *N* silent clients hold *N* slots for that
same half hour, so the horizon's capacity can be exhausted by silent clients alone.

Under the **default** configuration the idle bound is usually not what fires.
`ConnTimeout` is 30 s, the read deadline is armed once per message read, and a Bolt
NOOP keep-alive chunk is consumed inside that read rather than resetting it, so a
client that sends nothing the message loop can dispatch trips the connection
deadline first; the teardown rolls the transaction back and frees the slot.
`TestReclaim_ConnTimeoutBeatsALongerIdleBound`
(`bolt/server/default_timeouts_reach_test.go`) measures that at 411 ms against a
400 ms `ConnTimeout` and a one-hour idle bound. The half hour above is therefore
the exposure of a deployment that **also** raises `ConnTimeout` past the idle
bound for long-lived idle sessions. An operator who wants the previous protection
sets `MaxTxIdleTime` (and, for the total bound, `DefaultTxTimeout`) explicitly.

`DefaultTxTimeout` alone cannot separate an abandoned transaction from legitimate
long work: lowering it shortens the exposure and kills slow-but-healthy
transactions at the same time. `MaxTxIdleTime` distinguishes the two, because a
working client sends messages and an abandoned one does not — every inbound
message pushes the idle deadline forward, while the total-lifetime deadline is
untouched.

`MaxOpenTxPerPrincipal` bounds the other dimension, and it binds on **both** modes.
It once could not be the binding constraint for a write transaction, because the
engine capped concurrently-open write transactions at one server-wide; rmp #2305
retired that hold, so write transactions now overlap freely and the quota is a real
limit for them too. It counts open transactions, not `BEGIN`s waiting to be
admitted — a burst of concurrent `BEGIN`s from one principal is bounded by
`MaxConnections`.

Since rmp #2307 a read transaction does hold one thing: an MVCC read snapshot,
pinned at `BEGIN` for its whole lifetime. That is what gives it **snapshot
isolation across all of its statements** — a commit landing between two `RUN`s is
invisible to the second — and it is also why the two bounds above matter more
than they did: while the handle is open, no version it can still reach may be
reclaimed. Both reaps roll the transaction back, which returns the snapshot;
`lpg.MVCCStats.ActiveSnapshots` and `OldestSnapshotAge()` are where a leak would
show. (Those two were `ActiveReaders` and `OldestReaderAge()` until sprint 334
renamed them: the horizon holds a writer's snapshot as well as a reader's, so the
old names under-reported what was pinning it.)

### Read-your-own-writes IS guaranteed per connection

**A statement run on a connection observes every commit already made on that
connection** — autocommit or explicit transaction, in either order. The server binds
one `cypher.Session` per connection (rmp #2329), a connection being a session by
definition: one client, one ordered conversation.

Without it a client could observe two things, and both were measured (rmp #2328):

- a write followed by a read on the same connection might not see the write;
- a connection writing repeatedly to one key might get a retriable serialization
  error with nothing else contending for it.

Both follow from the commit frontier being CONTIGUOUS: a commit is acknowledged at an
instant that may not have published yet, because an *earlier* in-flight commit holds
the frontier back, so the connection's next transaction could begin below its own
commit. The session closes it by making the connection's next operation wait for the
frontier to reach its own last commit before taking a snapshot.

**Across connections nothing is promised beyond snapshot isolation**, which is the
same contract any client-server database gives. A client that needs two connections
to agree must coordinate them itself.

The wait costs nothing when it is not needed — measured at 32.06 µs against 32.13 µs
sessionless on a read-after-write loop
([`benchmarks/session-ryow-2026-08-06.md`](benchmarks/session-ryow-2026-08-06.md)) —
because it returns after one atomic load whenever the frontier has already passed.
`lpg.mvcc.sessions.waiting` reports whether connections are actually waiting.

Both events are separately observable: `bolt.server.tx.idlereaped` counts
transactions reaped for silence, `bolt.server.tx.timedout` those that exceeded
their total lifetime, and `bolt.server.tx.quotarejected` the refused `BEGIN`s.

### Inspecting and terminating transactions

The automatic bounds above reclaim an abandoned transaction eventually. When an
operator needs to act sooner — or simply to find out *which* client is holding
the barrier — the server exposes the two primitives directly:

```go
for _, tx := range srv.Transactions() { // oldest first
    log.Printf("%s principal=%s mode=%s state=%s elapsed=%v query=%q",
        tx.ID, tx.Principal, tx.Mode, tx.State, tx.Elapsed, tx.Query)
}

if err := srv.TerminateTransaction(id); err != nil {
    // errors.Is(err, server.ErrNoSuchTransaction) when it already ended
}
```

`Transactions` returns a point-in-time snapshot of every open explicit
transaction, oldest first, so the one most likely to be blocking others comes
first. `TerminateTransaction` rolls one back atomically — every statement of it,
exactly as a client `ROLLBACK` would. It does **not** release a writer lock: the
engine's writer serialisation and visibility barrier were retired, so what an
abandoned transaction actually pins is version memory (no version it can still
reach is reclaimable while it lives), not other clients' progress.

**What the client is told.** On its next request-phase message the terminated
connection receives `Neo.ClientError.Transaction.Terminated` — "the transaction has
been terminated by an operator request". That is deliberately *not* the code an
expired bound produces (`Neo.ClientError.Transaction.TransactionTimedOut`), so an
operator who terminates a transaction and then reads the client's logs sees what
actually happened. A statement caught in flight is failed through the same code,
because the termination cancels the transaction's context; one termination
therefore reports one reason regardless of timing.

The code is a `ClientError` rather than a `TransientError` on purpose. Neo4j's
own Go driver rewrites `Neo.TransientError.Transaction.Terminated` to exactly this
code as part of mapping pre-5.x classifications forward, and it does so *before*
reading the classification — so the `TransientError` spelling is both the legacy
one and one no driver would actually retry.

Termination is delivered rather than performed inline: a `Session` is
single-threaded by contract, so the rollback runs on the owning connection's own
goroutine. The transaction's context is cancelled synchronously, so a statement
already executing is interrupted immediately. Call `Transactions` again to confirm
it has gone. Both calls are safe from any goroutine while the server is serving.

(A 2026-07 measurement recorded here — a reader blocked behind an abandoned
`BEGIN` served 203 ms after an operator terminated it — described the pre-rmp
#2305 engine, in which an open write transaction blocked readers. On the current
engine there is no such reader to release, so the figure no longer measures
anything and has been withdrawn rather than restated.)

Neo4j offers the equivalent as `SHOW TRANSACTIONS` and `TERMINATE TRANSACTIONS`
in Community, and Memgraph offers both; the Go API is the embeddable form of the
same capability. Terminations are counted by `bolt.server.tx.terminated`, kept
separate from the automatic reaps because a deliberate intervention and an
expired bound are different operational events.

## Message support

| Message    | Direction       | Notes                                   |
|------------|-----------------|-----------------------------------------|
| HELLO      | Client → Server | Authenticates the session               |
| LOGON      | Client → Server | Re-authenticates on an established conn |
| LOGOFF     | Client → Server | Clears session identity                 |
| GOODBYE    | Client → Server | Orderly teardown                        |
| RESET      | Client → Server | Returns connection to READY state       |
| RUN        | Client → Server | Executes a Cypher query                 |
| PULL       | Client → Server | Fetches rows from an open cursor        |
| DISCARD    | Client → Server | Discards rows without streaming them    |
| BEGIN      | Client → Server | Opens an explicit transaction           |
| COMMIT     | Client → Server | Commits an explicit transaction         |
| ROLLBACK   | Client → Server | Rolls back an explicit transaction      |
| ROUTE      | Client → Server | Requests the routing table              |
| SUCCESS    | Server → Client | Request succeeded                       |
| FAILURE    | Server → Client | Request failed (typed error code)       |
| IGNORED    | Server → Client | Request was ignored (failed state)      |
| RECORD     | Server → Client | One row of result data                  |

`IGNORED` is emitted for a request-phase message (RUN/PULL/DISCARD/BEGIN/
COMMIT/ROLLBACK/ROUTE) received on an authenticated connection that is in the
FAILED state, until the client sends `RESET` (per the Bolt v5 spec).

### Protocol conformance notes

The server is strictly single-stream (a `RUN` is rejected while a result is
already streaming), which bounds the following intentional limitations:

- **`qid`** — the single open result stream always has `qid = -1`. A `PULL` or
  `DISCARD` carrying an explicit `qid >= 0` names a stream that does not exist
  and is rejected with `Neo.ClientError.Request.Invalid`; `qid = -1` (the
  default, "current stream") is served normally.
- **`tx_metadata`** — accepted in `BEGIN`/`RUN` extras and silently ignored; the
  server stores and echoes no transaction metadata.
- **`db`** — reported in the `RUN` and terminal `PULL`/`DISCARD` `SUCCESS`
  metadata, so `ResultSummary.Database().Name()` is populated. GoGraph serves one
  graph per server, so the name is a label and not a selector: a client that
  names a database has that name echoed back, and one that names none is told
  `Options.DatabaseName` (default `neo4j`). An unknown name is echoed rather than
  refused, where Neo4j would answer
  `Neo.ClientError.Database.DatabaseNotFound`.
- **`stats`** — sent on the terminal `PULL`/`DISCARD` `SUCCESS` of a statement
  that changed something (rmp #2190), so the driver's `ResultSummary` write
  counters and `ContainsUpdates()` are populated. Only non-zero counters are
  sent, as Neo4j does, so a read-only statement's `SUCCESS` is unchanged. One
  mapping is lossy at the protocol boundary: openCypher counts a property
  removal as its own `-properties` effect and Bolt has no `properties-removed`,
  so removals are summed into `properties-set`.
- **`bookmark`** — minted on the terminal `SUCCESS` of an **autocommit**
  statement and on the `COMMIT` `SUCCESS`, and omitted on a statement inside an
  explicit transaction, where the Bolt specification puts the bookmark on the
  `COMMIT` (rmp #2563). The token names work that is already durable. The server
  is single-host, so incoming bookmarks are logged and otherwise ignored.
- **`notifications`** — sent when the statement produced any, in Neo4j's
  notification shape.
- **`plan`** / **`profile`** — sent when the statement carried an `EXPLAIN` or a
  `PROFILE` prefix, never both, so `ResultSummary.Plan()` and `Profile()` are
  populated (rmp #2721). Alongside the operator tree, `args` carries `Details`,
  `EstimatedRows` and its provenance, and — for a `PROFILE` — `RowsRemovedByFilter`.
  The page-cache figures Neo4j reports are absent because GoGraph measures none of
  them, and a fabricated zero would read as a measurement. Two limitations sit
  inside the pair: `PROFILE` refuses a writing statement, and neither prefix may
  precede a schema statement.
- **`type`** — not sent, so `ResultSummary.StatementType` reads
  `StatementTypeUnknown`.
- **`t_first`** / **`t_last`** — not sent; the server does not measure them, so
  the driver reports -1 ms.

## Auto-commit and explicit transactions

Both read and write queries may run in auto-commit mode (no `BEGIN`/`COMMIT`).
Each auto-commit `RUN` is executed as its own atomic transaction through the
write-aware planner, so `CREATE`, `MERGE`, `SET`, and `DELETE` are durable
without an enclosing `BEGIN`/`COMMIT`:

```text
RUN  CREATE (n:Person {name: "Alice"})
PULL
```

Use an explicit transaction to group several statements so they commit or roll
back together:

```text
BEGIN
RUN  CREATE (n:Person {name: "Alice"})
PULL
RUN  CREATE (m:Person {name: "Bob"})
PULL
COMMIT
```

Nested transactions are not supported: a `BEGIN` while a transaction is already
open is rejected with `Neo.ClientError.Statement.SemanticError`.

## Routing

The server responds to `ROUTE` with a single-host routing table pointing all
roles (WRITE, READ, ROUTE) at its own listener address, with a TTL of 300
seconds. This satisfies drivers that require a routing table before sending
queries.

`ROUTE` is gated on authentication and on session state: an unauthenticated
connection, or one that is neither in READY nor in TX_READY, is answered with
`Neo.ClientError.Request.Invalid` and receives no routing table. That is
wire-compatible with the official driver, which completes `HELLO` (and `LOGON`
on Bolt 5.1 and above) before issuing `ROUTE`.

## Concurrency contract

`Server` is safe for concurrent use. Each accepted connection runs in its own
goroutine backed by an independent `Session`. `Session` is NOT safe for
concurrent use; the per-connection message loop is single-threaded.

## Graceful shutdown

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
if err := srv.Shutdown(ctx); err != nil {
    log.Printf("shutdown: %v", err)
}
```

`Shutdown` stops accepting new connections and waits for all active connections
to close. The wait is the **shorter** of the context deadline and a fixed 30 s
drain timeout, so a context deadline longer than 30 s does not extend it. If the
drain does not complete it returns an error and does not forcibly close
connections; a still-running `Serve` stays blocked on the same drain and
performs the post-drain teardown itself when the abandoned connections finally
finish.

When `Options.Closer` is set, the drain-success path also closes it — exactly
once, whichever of `Serve` or `Shutdown` gets there first — so the durability
stack is torn down in its crash-safe order only after no in-flight transaction
can still be writing. A failed close is returned, not swallowed.

---

## Deployment

### Standalone binary

There is no standalone binary in this repository. Embed the server in your own
`cmd/` entry-point:

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/FlavioCFOliveira/GoGraph/bolt/server"
    "github.com/FlavioCFOliveira/GoGraph/cypher"
    "github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
    "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func main() {
    // Directed + Multigraph — openCypher relationships are directed and the
    // data model is a multigraph (a CREATE always adds a relationship,
    // including a parallel edge between a pair). Weightless drops the per-node
    // edge-weight column, which carries no information for the Cypher engine.
    g   := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true, Weightless: true})
    eng := cypher.NewEngine(g)
    srv, err := server.NewServer(eng, server.Options{
        MaxConnections: 1024,
        Auth: server.BasicAuthHandler{Validate: func(user, pass string) error {
            if user != os.Getenv("GOGRAPH_USER") || pass != os.Getenv("GOGRAPH_PASSWORD") {
                return server.ErrAuthFailed
            }
            return nil
        }},
    })
    if err != nil {
        log.Fatalf("bolt: %v", err)
    }

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
    defer stop()

    go func() {
        if err := srv.ListenAndServe(ctx, ":7687"); err != nil {
            log.Printf("bolt: %v", err)
        }
    }()

    <-ctx.Done()
    shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    if err := srv.Shutdown(shutCtx); err != nil {
        log.Printf("shutdown: %v", err)
    }
}
```

The entry-point above embeds the in-memory engine, which owns no files. When the
engine is backed by the durable stack instead, hand the `*store.DB` to the server
as `Options.Closer` rather than closing it from the entry-point: the server then
closes it only after every connection has drained, which is the one teardown order
in which no in-flight transaction can still be writing. See
[Graceful shutdown](#graceful-shutdown).

### Docker

Build a minimal image from your entry-point binary:

```dockerfile
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -o /gograph ./cmd/server

FROM alpine:3.21
COPY --from=builder /gograph /usr/local/bin/gograph
EXPOSE 7687
ENTRYPOINT ["/usr/local/bin/gograph"]
```

Pass TLS certificates and configuration via environment variables or mounted
volumes; do not bake secrets into the image.

### systemd unit

```ini
[Unit]
Description=GoGraph Bolt server
After=network.target

[Service]
ExecStart=/usr/local/bin/gograph --addr :7687
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
# Environment=GOGRAPH_TLS_CERT=/etc/gograph/tls.crt
# Environment=GOGRAPH_TLS_KEY=/etc/gograph/tls.key

[Install]
WantedBy=multi-user.target
```

Place the unit file at `/etc/systemd/system/gograph.service`, then:

```bash
systemctl daemon-reload
systemctl enable --now gograph
```

---

## Observability

### Metrics

GoGraph emits latency histograms and counters via the `internal/metrics`
package. The default backend is a no-op; install a `metrics.Backend`
implementation to activate collection. See [docs/metrics.md](metrics.md) for
the full metric inventory.

The Bolt server emits the following server-level counters, which give an
operator the signals needed to correlate a connection flood or a transaction
leak:

| Metric | Meaning |
|---|---|
| `bolt.server.conn.accepted` | Connections admitted past the `MaxConnections` semaphore (one per per-connection handler goroutine started). |
| `bolt.server.conn.closed` | Per-connection handler goroutines that have exited, for any reason. |
| `bolt.server.conn.rejected` | Connections refused because the `MaxConnections` semaphore was already full. |
| `bolt.server.tx.opened` | Explicit transactions opened by a `BEGIN` that was admitted. Paired with `tx.closed` it derives the number of open transactions. |
| `bolt.server.tx.closed` | Explicit transactions that ended — committed, rolled back, discarded by `RESET`/`GOODBYE`, or rolled back on connection teardown. |
| `bolt.server.tx.abandoned` | Explicit transactions still open at an abnormal disconnect (the client dropped the connection, hit the idle timeout, or the handler recovered a panic) without sending `COMMIT`, `ROLLBACK`, or `RESET`. A strict subset of `tx.closed`. |
| `bolt.server.tx.timedout` | Explicit transactions reaped for exceeding their **total** wall-clock deadline (`DefaultTxTimeout` or a client `tx_timeout`) while the connection stayed alive. A strict subset of `tx.closed`. |
| `bolt.server.tx.idlereaped` | Explicit transactions reaped for **silence** — no inbound message for `MaxTxIdleTime`. Separated from `tx.timedout` so an abandoned `BEGIN` is distinguishable from a legitimately long transaction. A strict subset of `tx.closed`. |
| `bolt.server.tx.quotarejected` | `BEGIN`s refused because the authenticated principal already held `MaxOpenTxPerPrincipal` open transactions. A rising count means one principal is monopolising transactions. |
| `bolt.server.tx.terminated` | Explicit transactions rolled back because an operator called `Server.TerminateTransaction`. Kept separate from the automatic reaps: only this one means a human had to intervene. A strict subset of `tx.closed`. |
| `bolt.server.conn.panics` | Recovered panics in a connection handler goroutine (defence-in-depth boundary). |

The three server-initiated endings — `tx.timedout`, `tx.idlereaped`, and
`tx.terminated` — are mutually **disjoint**: exactly one of them counts any given
transaction. Until rmp #2560 `tx.timedout` was incremented by the shared teardown and
was therefore a superset of the other two, which silently undid the separation the
other two exist to provide.

The server also publishes a **per-message latency histogram** (rmp #2715), so a
Bolt latency regression is visible to the module itself rather than only to an
example's own instrumentation. The `metrics.Backend` interface has no label
dimension, so the message type is part of the series name:

```
bolt.server.HandleMessage.message.<kind>
```

where `<kind>` is one of `hello`, `logon`, `logoff`, `goodbye`, `reset`, `run`,
`pull`, `discard`, `begin`, `commit`, `rollback`, `route`, and `other`.
Cardinality is bounded by construction: the label is chosen by a type switch over
the message types `bolt/proto` defines, never taken from the wire, and the
thirteen names are built once at init so an emission allocates nothing.

Two of these quantities are conceptually gauges — the number of live
connections and the number of open transactions. The `metrics.Backend`
interface exposes only a monotonic, non-decrementing counter (`IncCounter`), so
each gauge is emitted as a pair of counters and the live value is the
derivation:

```
live connections   = bolt.server.conn.accepted − bolt.server.conn.closed
open transactions  = bolt.server.tx.opened     − bolt.server.tx.closed
```

This is the standard Prometheus "created/closed → in-use = created − closed"
pattern. Each pair is balanced by construction: every increment of an
opened-side counter has exactly one matching increment of its closed-side
counter on every exit path (clean close, read/write error, idle timeout,
recovered panic), so each derived gauge returns to zero once the server is
quiescent. A derivation that stays persistently above zero is itself the leak
signal — a phantom live connection or an unreleased open transaction.

### Health check

The Bolt server does not expose an HTTP health endpoint. To verify liveness,
open a Bolt connection and send a `HELLO` / `RESET` sequence; a `SUCCESS`
response confirms the server is ready.

With `cypher-shell`:

```bash
cypher-shell -a bolt://localhost:7687 -u neo4j -p password \
    "RETURN 1 AS ok"
```

With `neo4j-go-driver`:

```go
driver, _ := neo4j.NewDriverWithContext(
    "bolt://localhost:7687",
    neo4j.BasicAuth("neo4j", "password", ""),
)
if err := driver.VerifyConnectivity(ctx); err != nil {
    log.Fatalf("not reachable: %v", err)
}
```

---

## Troubleshooting

### Common error codes

The server maps internal errors to Neo4j-style dot-delimited error codes sent
in `FAILURE` messages. The mapping (from `bolt/server/errors.go`) is:

The rules are tested in the order below; the first match wins.

| Go error | Neo4j error code |
|---|---|
| `context.DeadlineExceeded` | `Neo.ClientError.Transaction.TransactionTimedOut` |
| `context.Canceled` | `Neo.ClientError.Transaction.Terminated` |
| `server.ErrAuthFailed` | `Neo.ClientError.Security.Unauthorized` |
| `server.ErrInvalidTransition` | `Neo.ClientError.Request.InvalidFormat` |
| `*parser.ParseError` | `Neo.ClientError.Statement.SyntaxError` |
| `*parser.SemaError` | `Neo.ClientError.Statement.SemanticError` |
| `*sema.SemanticError` | `Neo.ClientError.Statement.SyntaxError`, `.TypeError` or `.SemanticError`, from its TCK-pinned `Category` |
| `*expr.EvalError` | `Neo.ClientError.Statement.TypeError`, `.EntityNotFound`, `.ArgumentError` or `.ArithmeticError`, from its message's TCK-pinned prefix |
| `cypher.ErrUnsupportedParamType` | `Neo.ClientError.Statement.TypeError` |
| `cypher.ErrWriteInReadOnlyTx` | `Neo.ClientError.Request.Invalid` |
| `txn.ErrTransactionTooLarge` | `Neo.ClientError.General.TransactionOutOfMemoryError` |
| `wal.ErrDurabilityFailed` | `Neo.DatabaseError.General.UnknownError` |
| `mvcc.ErrSerializationConflict` | `Neo.TransientError.Transaction.Outdated` |
| `cypher.ErrResultRowsExceeded`, `cypher.ErrResultBytesExceeded`, `funcs.ErrCollectItemsExceeded` | `Neo.ClientError.General.LimitExceeded` |
| `*exec.ConstraintViolationError` | `Neo.ClientError.Schema.ConstraintValidationFailed` |
| `exec.ErrConstraintNotFound` | `Neo.ClientError.Schema.ConstraintDropFailed` |
| `exec.ErrConstraintAlreadyExists` | `Neo.ClientError.Schema.ConstraintAlreadyExists` |
| `exec.ErrConstraintNameConflict` | `Neo.ClientError.Schema.ConstraintWithNameAlreadyExists` |
| `index.ErrIndexExists` | `Neo.ClientError.Schema.IndexAlreadyExists` |
| `index.ErrIndexNotFound` | `Neo.ClientError.Schema.IndexNotFound` |
| `procs.ErrProcNotFound` | `Neo.ClientError.Procedure.ProcedureNotFound` |
| an untyped engine error whose message carries a TCK category (`cypher: SyntaxError.`, `SemanticError.`, `TypeError.`, `ArgumentError.`) | the matching `Neo.ClientError.Statement.*` code |
| (any other error) | `Neo.DatabaseError.General.UnknownError` |

Two of these classifications are load-bearing for a driver's retry decision, and
both were chosen against the driver's own source rather than by appearance.
`mvcc.ErrSerializationConflict` is `TransientError` because a fresh snapshot
would include the change it collided with, and `neo4j-go-driver` v5.28.4 retries
on exactly that classification; `Neo.TransientError.Transaction.Terminated` was
rejected as the code for it, because the same driver rewrites that spelling to a
`ClientError` *before* reading the classification, so it would never be retried.
`wal.ErrDurabilityFailed` is deliberately **not** transient: the writer is
poisoned and the next attempt fails the same way.

Error matching uses `errors.Is` and `errors.As`, so wrapped errors are matched
correctly.

A few codes are produced directly by the session handlers rather than by the
`FailureCode` map above:

| Condition | Neo4j error code |
|---|---|
| Malformed message, unrecognised message type, or illegal state transition | `Neo.ClientError.Request.Invalid` |
| In-flight cursor cap exceeded (`MaxInFlightPerConnection`) | `Neo.ClientError.General.LimitExceeded` |
| Nested `BEGIN` | `Neo.ClientError.Statement.SemanticError` |
| Unknown auth scheme | `Neo.ClientError.Security.AuthProviderFailed` |
| Context cancelled mid-request or mid-`PULL` | `Neo.TransientError.General.RequestInterrupted` |

### Connection refused

- Verify the server is running and listening on the expected port
  (`netstat -tlnp | grep 7687` or `ss -tlnp | grep 7687`).
- Check that `ListenAndServe` has not returned early; the goroutine may have
  exited due to a bind error (port in use, permission denied).
- Confirm the `MaxConnections` semaphore is not exhausted: the accept loop
  acquires a slot without blocking, so when all slots are occupied a newly
  accepted connection is closed immediately (a warning is logged with the
  remote address). The client sees the connection dropped right after the TCP
  accept, not a slow response.

### TLS certificate errors

- The server accepts any `*tls.Config` in `Options.TLSConfig`. Ensure the
  certificate chain is complete (leaf + intermediates).
- Drivers that perform hostname verification require the certificate `CN` or a
  `SAN` entry to match the address used by the driver.
- For development, pass `neo4j.TrustAll()` (Go driver) or
  `--encryption=false` (cypher-shell) to skip certificate verification.

### Driver compatibility

| Driver | Supported versions |
|---|---|
| `neo4j-go-driver` | v5.x (pinned at v5.28.4 in `go.mod`) |
| `cypher-shell` | 5.x (ships with Neo4j 5) |
| Bolt 4.4 clients | Supported via the Bolt 4.4 fallback handshake |

Drivers that negotiate Bolt 3.x or earlier are not supported.

Compatibility is measured, not asserted. `bolt/server/driver_compat_test.go` is a
standing, ratcheted suite that drives the official driver against an in-process
server across 37 checks and fails if the passing count drops below a recorded
floor. It is slower than the short test layer's budget allows, so it is gated
behind a build tag:

```bash
go test -tags=drivercompat -run TestDriverCompatibility -v ./bolt/server/
```

The position at the time of this review is **PASS=30, FAIL=6, DEGRADED=1**
against a floor of 30. Each remaining failure is a known gap with a cause:

- temporal, `Point2D` and `[]byte` **parameter** round-trips — `cypher.BindParams`
  rejects `packstream.Struct` and `[]uint8`, so inbound temporal and spatial
  parameters are not implemented. Outbound temporal values already work.
- `ResultSummary.StatementType` — the server does not send the `type` field.
- `SHOW TRANSACTIONS` — the DDL parser has no such form; the operator API above
  is the reachable equivalent.
- `CALL dbms.components()` — the `dbms.*` procedure namespace is not implemented.
- DEGRADED: `ResultSummary` timing (`t_first` / `t_last`) — not measured, so the
  driver reports -1 ms. A missing feature, not a defect.

---

## See also

- [docs/cypher.md](cypher.md) — Cypher language reference
- [docs/benchmarks/cypher.md](benchmarks/cypher.md) — IC1–IC14 benchmark results
- [docs/metrics.md](metrics.md) — observability metrics
- [examples/23_bolt_server](../examples/23_bolt_server) — runnable embedding example (start + graceful shutdown)


---

*Last reviewed: 2026-09-08 against commit `728770ff`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*

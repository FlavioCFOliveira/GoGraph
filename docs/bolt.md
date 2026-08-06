# Bolt v5 Server

GoGraph includes a Bolt v5 server compatible with `neo4j-go-driver` v5 and `cypher-shell`.

## Quick start

```go
import (
    "context"
    "github.com/FlavioCFOliveira/GoGraph/bolt/server"
    "github.com/FlavioCFOliveira/GoGraph/cypher"
    "github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
    "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Multigraph: true is required — openCypher's data model is a multigraph, so a
// CREATE always adds a relationship (including a parallel edge between an
// existing node pair).
g := lpg.New[string, float64](adjlist.Config{Multigraph: true})
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

```go
opts := server.Options{
    TLSConfig: &tls.Config{...},
}
```

## Limits and backpressure

`Options` exposes the bounds that protect the server under load. All of them
fall back to a default when left at the zero value:

| Field | Default | Effect |
|---|---|---|
| `MaxConnections` | 1024 | Upper bound on concurrent connections. When the limit is reached, a newly accepted connection is closed immediately rather than queued. |
| `MaxMessageBytes` | `proto.DefaultMaxMessageBytes` (16 MiB) | Caps the cumulative payload of one Bolt message reassembled across chunks, closing the Slowloris-style vector of an unbounded chunk stream. |
| `MaxInFlightPerConnection` | `DefaultMaxInFlightPerConnection` (1024) | Caps the number of `RUN` statements issued inside a single explicit transaction before `COMMIT`/`ROLLBACK`. Exceeding it returns a `Neo.ClientError.General.LimitExceeded` failure. Auto-commit cursors are not counted. |
| `ConnTimeout` | 0 (disabled) | Per-connection idle read deadline, reset before each message read. |
| `DatabaseName` | `DefaultDatabaseName` (`neo4j`) | The name reported in result metadata for a client that selects no database. A client that names one has its own name echoed back. See the `db` note under [Protocol conformance notes](#protocol-conformance-notes). |
| `MaxTxIdleTime` | `DefaultMaxTxIdleTime` (5 s) | How long an **open** explicit transaction may go without the client sending a message, after which it is rolled back. Distinct from `DefaultTxTimeout`, which caps total lifetime however busy the transaction is. Cannot be disabled. |
| `MaxOpenTxPerPrincipal` | `DefaultMaxOpenTxPerPrincipal` (16) | How many explicit transactions one authenticated principal may hold **open** at once, across all its connections. Exceeding it fails the `BEGIN` with `Neo.ClientError.General.LimitExceeded`. A negative value disables it. |

### Abandoned transactions

An open explicit **write** transaction holds the engine's global visibility
barrier, so while one is open every reader on every other connection waits. A
client that sends `BEGIN` and then stops talking therefore stalls the whole
server for as long as the transaction lives.

`DefaultTxTimeout` alone cannot separate that from legitimate long work: lowering
it shortens the outage and kills slow-but-healthy transactions at the same time.
`MaxTxIdleTime` distinguishes the two, because a working client sends messages and
an abandoned one does not — every inbound message pushes the idle deadline
forward, while the total-lifetime deadline is untouched.

Measured with one client abandoning a `BEGIN` and another reading, both bounds at
their defaults except a 20 s total timeout: before the idle bound existed the
reader received no response at all within 10 s; with it, the reader is served
after 5.0 s, the idle bound.

`MaxOpenTxPerPrincipal` bounds the other dimension. Note where it binds: a write
transaction holds the writer serialisation, so the engine already caps those at
one server-wide, and this limit is therefore about **read** transactions
(`BEGIN` with `mode: "r"`), which take no writer serialisation and no barrier and
can genuinely be concurrent. It counts open transactions, not `BEGIN`s queued on
the writer — concurrent `BEGIN`s are bounded by `MaxConnections`.

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

### Read-your-own-writes is NOT yet guaranteed per connection

A Bolt connection is a session in every sense except this one. GoGraph's commit
frontier is contiguous, so a commit is acknowledged at an instant that may not
have published yet, and the same connection's next transaction can begin *below
its own commit*. Two consequences a client can observe:

- a write followed by a read on the same connection may not see the write;
- a connection writing repeatedly to one key may get a retriable serialization
  error with nothing else contending for it.

`lpg.Session` (rmp #2328) is the mechanism that closes this — it makes a caller
wait for its own commits to become visible — but the Bolt server does not carry
one per connection yet (rmp #2329). Until it does, a client that needs
read-your-own-writes must read inside the same transaction as the write.

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
exactly as a client `ROLLBACK` would — releasing the writer serialisation and the
visibility barrier.

Termination is delivered rather than performed inline: a `Session` is
single-threaded by contract, so the rollback runs on the owning connection's own
goroutine. The transaction's context is cancelled synchronously, so a statement
already executing is interrupted immediately. Call `Transactions` again to confirm
it has gone. Both calls are safe from any goroutine while the server is serving.

Measured: with both automatic bounds set to 5 minutes, a reader blocked behind an
abandoned `BEGIN` was served 203 ms after the `BEGIN` — released by the
termination, since nothing else could have.

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
- **`stats`** — not sent. Write counters on the driver's `ResultSummary`
  therefore read zero and `ContainsUpdates()` is false even after a successful
  write. Verify write effects with a follow-up `MATCH`.

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
roles (WRITE, READ, ROUTE) at its own listener address. This satisfies drivers
that require a routing table before sending queries.

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

`Shutdown` stops accepting new connections and waits up to the context deadline
for all active connections to close. If the deadline is exceeded it returns an
error but does not forcibly close connections.

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
    // Multigraph: true — openCypher's data model is a multigraph (a CREATE
    // always adds a relationship, including a parallel edge between a pair).
    g   := lpg.New[string, float64](adjlist.Config{Multigraph: true})
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

### Docker

Build a minimal image from your entry-point binary:

```dockerfile
FROM golang:1.26-alpine AS builder
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
| `bolt.server.tx.opened` | Explicit transactions opened by a `BEGIN` that acquired the engine writer serialisation. |
| `bolt.server.tx.closed` | Explicit transactions that ended — committed, rolled back, discarded by `RESET`/`GOODBYE`, or rolled back on connection teardown. |
| `bolt.server.tx.abandoned` | Explicit transactions still open at an abnormal disconnect (the client dropped the connection, hit the idle timeout, or the handler recovered a panic) without sending `COMMIT`, `ROLLBACK`, or `RESET`. A strict subset of `tx.closed`. |
| `bolt.server.tx.timedout` | Explicit transactions reaped for exceeding their **total** wall-clock deadline (`DefaultTxTimeout` or a client `tx_timeout`) while the connection stayed alive. A strict subset of `tx.closed`. |
| `bolt.server.tx.idlereaped` | Explicit transactions reaped for **silence** — no inbound message for `MaxTxIdleTime`. Separated from `tx.timedout` so an abandoned `BEGIN` is distinguishable from a legitimately long transaction. A strict subset of `tx.closed`. |
| `bolt.server.tx.quotarejected` | `BEGIN`s refused because the authenticated principal already held `MaxOpenTxPerPrincipal` open transactions. A rising count means one principal is monopolising transactions. |
| `bolt.server.tx.terminated` | Explicit transactions rolled back because an operator called `Server.TerminateTransaction`. Kept separate from the automatic reaps: only this one means a human had to intervene. A strict subset of `tx.closed`. |
| `bolt.server.conn.panics` | Recovered panics in a connection handler goroutine (defence-in-depth boundary). |

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

| Go error | Neo4j error code |
|---|---|
| `context.DeadlineExceeded` | `Neo.ClientError.Transaction.TransactionTimedOut` |
| `context.Canceled` | `Neo.ClientError.Transaction.Terminated` |
| `server.ErrAuthFailed` | `Neo.ClientError.Security.Unauthorized` |
| `server.ErrInvalidTransition` | `Neo.ClientError.Request.InvalidFormat` |
| `*parser.ParseError` | `Neo.ClientError.Statement.SyntaxError` |
| `*parser.SemaError` | `Neo.ClientError.Statement.SemanticError` |
| `*exec.ConstraintViolationError` | `Neo.ClientError.Schema.ConstraintViolationOnCreate` |
| `index.ErrIndexExists` | `Neo.ClientError.Schema.IndexAlreadyExists` |
| `index.ErrIndexNotFound` | `Neo.ClientError.Schema.IndexNotFound` |
| `procs.ErrProcNotFound` | `Neo.ClientError.Procedure.ProcedureNotFound` |
| (any other error) | `Neo.DatabaseError.General.UnknownError` |

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
| `neo4j-go-driver` | v5.x |
| `cypher-shell` | 5.x (ships with Neo4j 5) |
| Bolt 4.4 clients | Supported via the Bolt 4.4 fallback handshake |

Drivers that negotiate Bolt 3.x or earlier are not supported.

---

## See also

- [docs/cypher.md](cypher.md) — Cypher language reference
- [docs/benchmarks/cypher.md](benchmarks/cypher.md) — IC1–IC14 benchmark results
- [docs/metrics.md](metrics.md) — observability metrics
- [examples/23_bolt_server](../examples/23_bolt_server) — runnable embedding example (start + graceful shutdown)


---

*Last reviewed: 2026-07-26 against commit `01f5bea`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*

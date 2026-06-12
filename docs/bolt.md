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

g := lpg.New[string, float64](adjlist.Config{})
eng := cypher.NewEngine(g)
srv := server.NewServer(eng, server.Options{MaxConnections: 1024})
go srv.ListenAndServe(context.Background(), ":7687")
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

## Auto-commit and explicit transactions

Both read and write queries may run in auto-commit mode (no `BEGIN`/`COMMIT`).
Each auto-commit `RUN` is executed as its own atomic transaction through the
write-aware planner, so `CREATE`, `MERGE`, `SET`, and `DELETE` are durable
without an enclosing `BEGIN`/`COMMIT`:

```cypher
RUN  CREATE (n:Person {name: "Alice"})
PULL
```

Use an explicit transaction to group several statements so they commit or roll
back together:

```cypher
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
    g   := lpg.New[string, float64](adjlist.Config{})
    eng := cypher.NewEngine(g)
    srv := server.NewServer(eng, server.Options{MaxConnections: 1024})

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

*Last reviewed: 2026-06-12 against commit `ec76e6f`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*

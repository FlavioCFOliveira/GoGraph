# Bolt v5 Server

GoGraph includes a Bolt v5 server compatible with `neo4j-go-driver` v5 and `cypher-shell`.

## Quick start

```go
import (
    "context"
    "gograph/bolt/server"
    "gograph/cypher"
    "gograph/graph/adjlist"
    "gograph/graph/lpg"
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

## Write queries

Write queries (`CREATE`, `MERGE`, `SET`, `DELETE`) must be executed inside an
explicit transaction:

```cypher
BEGIN
RUN  CREATE (n:Person {name: "Alice"})
PULL
COMMIT
```

Read-only queries (`MATCH`, `RETURN`) may be executed in auto-commit mode (no
`BEGIN`/`COMMIT` required).

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

    "gograph/bolt/server"
    "gograph/cypher"
    "gograph/graph/adjlist"
    "gograph/graph/lpg"
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

The Bolt server itself does not emit dedicated server-level metrics (connection
count, session count). Those are tracked at the application level via the
`MaxConnections` semaphore channel capacity exposed in `Options`.

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

### Connection refused

- Verify the server is running and listening on the expected port
  (`netstat -tlnp | grep 7687` or `ss -tlnp | grep 7687`).
- Check that `ListenAndServe` has not returned early; the goroutine may have
  exited due to a bind error (port in use, permission denied).
- Confirm the `MaxConnections` semaphore is not exhausted: if all connection
  slots are occupied, new connections are accepted at the TCP level but the
  goroutine is not started until a slot is released. The caller sees slow
  responses, not a refused connection.

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


---

*Last reviewed: 2026-05-22 against commit `cd97f07`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*

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

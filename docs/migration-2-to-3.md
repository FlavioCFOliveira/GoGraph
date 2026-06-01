# Migration guide: GoGraph 2.x → 3.x

This document collects the breaking changes between the 2.x stable
line and the 3.0.0 release, with the upgrade steps required to land
them.

**TL;DR.** v3.0.0 has exactly **two** breaking changes, both mechanical
to adopt:

1. The Go **module import path** changed from `gograph` to
   `github.com/FlavioCFOliveira/GoGraph` — a search-and-replace on your
   `import` lines.
2. **`bolt/server.NewServer`** now returns `(*Server, error)` and fails
   closed when no authentication handler is configured.

There are **no on-disk format changes** in v3.0.0: existing WAL,
snapshot, and CSR files load unchanged. If you do not run the Bolt
server, the entire migration is the import-path rewrite.

## Compatibility matrix

| Concern                       | 2.x behaviour                                  | 3.0.0 behaviour                                                                                  |
|-------------------------------|------------------------------------------------|--------------------------------------------------------------------------------------------------|
| Module import path            | `gograph/...` (import base `github.com/xumiga/gograph`) | **`github.com/FlavioCFOliveira/GoGraph/...`** — fetchable with `go get`.                  |
| Package identifiers           | `lpg`, `cypher`, `server`, …                   | **Unchanged.** Only the import *path* changes; your call sites are untouched.                    |
| `bolt/server.NewServer`       | `func NewServer(eng, opts) *Server`            | **`func NewServer(eng, opts) (*Server, error)`** — fails closed; nil `Auth` → `ErrNoAuthHandler`. |
| Running the Bolt server without auth | Implicit: `Options{}` silently installed `NoAuthHandler` | **Explicit only:** set `Auth: NoAuthHandler{}` (dev/test); still logs the insecure-default warning. |
| On-disk formats (WAL / snapshot / CSR) | v2/v3 WAL, v2 manifest                  | **Unchanged** — files written by 2.x load in 3.0.0 with no conversion.                           |
| New persistence-file permissions | `0o644` (world-/group-readable)             | `0o600` (owner-only). Affects *newly written* files; existing files keep their mode.             |
| Toolchain                     | `go 1.26` + `toolchain go1.26.3`               | **Unchanged** — `go 1.26` + `toolchain go1.26.3`.                                                |

## Step-by-step upgrade

### 1. Rewrite the module import path

The module path changed from the short `gograph` to the conventional,
repository-matching `github.com/FlavioCFOliveira/GoGraph`. Rewrite every
import in your codebase:

```diff
- import "gograph/graph/lpg"
- import "gograph/graph/adjlist"
- import "gograph/cypher"
- import "gograph/bolt/server"
+ import "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
+ import "github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
+ import "github.com/FlavioCFOliveira/GoGraph/cypher"
+ import "github.com/FlavioCFOliveira/GoGraph/bolt/server"
```

The **package identifiers do not change** — your code still writes
`lpg.New(...)`, `cypher.NewEngine(...)`, `server.NewServer(...)`, and so
on. Only the path inside the `import` string is different.

A repository-wide rewrite does the whole job:

```bash
# Preview the affected files first.
grep -rl '"gograph/' --include='*.go' .

# Apply the rewrite (GNU sed; on macOS use `sed -i ''`).
grep -rl '"gograph/' --include='*.go' . \
  | xargs sed -i 's#"gograph/#"github.com/FlavioCFOliveira/GoGraph/#g'

# Re-sort and re-group imports.
goimports -w .
```

Then update your module's dependency on GoGraph:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v3.0.0
go mod tidy
```

> **Note — no `/v3` path suffix.** The import base is
> `github.com/FlavioCFOliveira/GoGraph` with **no** major-version suffix.
> Do **not** import `github.com/FlavioCFOliveira/GoGraph/v3/...`. This
> matches the v2.0.0 precedent, which was likewise tagged without a
> major-version path suffix.

### 2. Update `bolt/server.NewServer` call sites

`NewServer` is now secure by default. It returns an `error`, and a nil
`Options.Auth` is rejected with the `ErrNoAuthHandler` sentinel instead
of silently installing the open-door `NoAuthHandler`.

In 2.x:

```go
// A nil Auth silently produced a fully open server.
srv := server.NewServer(eng, server.Options{
    MaxConnections: 64,
    ConnTimeout:    5 * time.Second,
})
```

In 3.0.0 — production (a real handler):

```go
srv, err := server.NewServer(eng, server.Options{
    MaxConnections: 64,
    ConnTimeout:    5 * time.Second,
    Auth:           myAuthHandler, // implements server.AuthHandler
})
if err != nil {
    return err
}
```

In 3.0.0 — development or testing only (explicit no-auth opt-in):

```go
// The explicit NoAuthHandler{} value is the opt-in to run without
// credentials. NewServer still logs the loud insecure-default warning.
srv, err := server.NewServer(eng, server.Options{
    MaxConnections: 64,
    ConnTimeout:    5 * time.Second,
    Auth:           server.NoAuthHandler{},
})
if err != nil {
    return err
}
```

If you forget to set `Auth`, `NewServer` now fails fast:

```go
srv, err := server.NewServer(eng, server.Options{}) // no Auth set
// err == server.ErrNoAuthHandler; srv is nil. This is intentional —
// the server fails closed rather than starting fully open.
```

### 3. (Optional) Adopt the hardened TLS baseline

v3.0.0 adds the exported `server.DefaultTLSConfig()` helper (finding L3).
It returns a fresh hardened `*tls.Config` — TLS 1.2 floor, modern
AEAD/ECDHE cipher suites, TLS 1.3 auto-negotiated — for operators to
start from:

```go
tlsCfg := server.DefaultTLSConfig()
tlsCfg.Certificates = []tls.Certificate{cert}

srv, err := server.NewServer(eng, server.Options{
    Auth:      myAuthHandler,
    TLSConfig: tlsCfg,
})
```

This is not required to upgrade: a nil `TLSConfig` still means plaintext,
and callers that pass their own config are unaffected. It is the
recommended baseline for any TLS-terminating deployment.

### 4. Rebuild and verify

```bash
go build ./...
go vet ./...
go test ./...
```

No on-disk format changed, so there is no data-migration step: WAL,
snapshot, and CSR files written by 2.x open unchanged in 3.0.0. The
`0o600` permission change (finding L2) applies only to files newly
written by 3.0.0; existing files retain their current mode until they are
rewritten.

## Cross-references

- [CHANGELOG.md](../CHANGELOG.md) — per-release entries with full
  rationale for each breaking change and every security finding.
- [release-notes/v3.0.0.md](../release-notes/v3.0.0.md) — the full v3.0.0
  release narrative, including the 20-finding security-audit table.
- [SECURITY.md](../SECURITY.md) — security policy and supported-version
  table.
- [docs/bolt.md](bolt.md) — Bolt v5 server operator guide, auth, and TLS.
- [docs/semver.md](semver.md) — versioning policy and release gates.
- [docs/release.md](release.md) — release workflow, dependency policy,
  toolchain pin, SBOM.

---

*Last reviewed: 2026-06-01 against commit `1a535bb`.*

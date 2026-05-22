# Migration guide: GoGraph 1.x → 2.x

This document collects the API and on-disk format changes between
the 1.x stable line and the 2.x release line, with the upgrade
steps required to land them.

**TL;DR.** Most 1.x code compiles unchanged against 2.x because the
breaking-change surface is intentionally narrow: a few error-return
additions on previously infallible methods, the on-disk manifest
version bump, and a small handful of newly required `Options`
fields. Consumers who only used the in-memory graph and search
algorithms can move from 1.x to 2.x by re-running `go build`.

## Compatibility matrix

| Concern                 | 1.x behaviour                            | 2.x behaviour                                                                   |
|-------------------------|------------------------------------------|---------------------------------------------------------------------------------|
| `graph.Graph.AddNode`   | `func (g) AddNode(N)`                    | `func (g) AddNode(N) error` — returns `adjlist.ErrShardFull` on bounded growth. |
| `graph.Graph.AddEdge`   | `func (g) AddEdge(N, N, W)`              | `func (g) AddEdge(N, N, W) error` — same bound surface as `AddNode`.            |
| `adjlist.Config`        | `Directed`, `Multigraph`                 | Same, plus `MaxShardCapacity` (zero = unbounded; positive = hard cap).          |
| `cypher.Engine`         | (not present in 1.x)                     | New in 2.x — see [docs/cypher.md](cypher.md).                                   |
| `cypher.Result.Close`   | (n/a)                                    | Mandatory; finalizer-backed safety net increments `cypher.result.leaked`.       |
| `bolt/server`           | (not present in 1.x)                     | New in 2.x — see [docs/bolt.md](bolt.md). Per-connection cursor cap = 1.        |
| `store/txn.NewStore`    | Single-type (`string` keys, `int64` w.)  | Generic over node and weight codecs (`NewStoreWithCodec`, `NewStoreWithOptions`). |
| Snapshot manifest       | v1 (CSR-only)                            | v2 (CSR + labels + properties + indexes). v1 manifests still load.               |
| WAL frames              | `OpRecord = 0x01`                        | Adds `OpRecordV2 = 0xFE` (typed codec) and `OpAddEdgeWeighted = 0x04`.          |
| Toolchain               | `go 1.21`                                | `go 1.26` + `toolchain go1.26.3` pin (see [release.md](release.md)).            |

## Step-by-step upgrade

### 1. Bump the Go module

```diff
- go 1.21
+ go 1.26
+ toolchain go1.26.3
```

Run `go mod tidy && go mod verify` after the edit. The dependency
policy in [CONTRIBUTING.md](../CONTRIBUTING.md#dependency-policy)
governs subsequent dependency upgrades.

### 2. Handle the new error returns on `AddNode` / `AddEdge`

In 1.x:

```go
g.AddNode("alice")
g.AddEdge("alice", "bob", 1.0)
```

In 2.x:

```go
if err := g.AddNode("alice"); err != nil {
    return err
}
if err := g.AddEdge("alice", "bob", 1.0); err != nil {
    return err
}
```

Loaders that build large graphs in tight loops can preserve the
hot path by checking the error only when `adjlist.Config.MaxShardCapacity`
is set; with the default zero cap `AddNode` and `AddEdge` never
return an error.

### 3. Adopt the Cypher / Bolt surface (optional)

These are entirely new in 2.x; pre-2.x callers are unaffected.
When adopting:

- [docs/cypher.md](cypher.md) — language reference.
- [docs/bolt.md](bolt.md) — Bolt v5 server, TLS, auth.
- `cypher.Result.Close()` is mandatory — see the
  [Result lifecycle contract](cypher.md) in the API reference and
  the safety-net finalizer (`cypher.result.leaked` metric).

### 4. Migrate the persistence layer (if used)

Existing v1 snapshot directories load unchanged in 2.x because the
reader accepts both manifest versions. Writers always emit v2.
Code that pinned `store/txn.NewStore` with positional arguments
should adopt `NewStoreWithCodec` or `NewStoreWithOptions`:

```diff
- s, err := txn.NewStore[string, int64](walPath)
+ s, err := txn.NewStoreWithOptions[string, int64](txn.Options{
+     WALPath:     walPath,
+     KeyCodec:    txn.StringCodec,
+     WeightCodec: txn.Int64Codec,
+ })
```

The single-type constructor is retained for source compatibility
but is deprecated and will be removed in 3.x.

### 5. Adopt the new bounded-resource defaults

2.x adds explicit caps to several previously unbounded surfaces:

- `bolt/server.Options.MaxConnections` (default 1 024).
- `bolt/server.Options.MaxMessageBytes` (default 16 MiB; see
  `proto.DefaultMaxMessageBytes`).
- `bolt/server.Options.MaxInFlightPerConnection` (default 1).
- `cypher.EngineOptions.PlanCacheCapacity` (default 1 024).
- `adjlist.Config.MaxShardCapacity` (zero = unbounded; opt-in).

Consumers who legitimately need to raise these caps must do so
explicitly via the `Options` struct passed to the relevant
constructor.

## Cross-references

- [CHANGELOG.md](../CHANGELOG.md) — per-release entries with full
  rationale for each breaking change.
- [docs/semver.md](semver.md) — versioning policy and v2.0.0
  stable gate.
- [docs/release.md](release.md) — release workflow, dependency
  policy, toolchain pin, SBOM.

---

*Last reviewed: 2026-05-22 against commit `b783b71`.*

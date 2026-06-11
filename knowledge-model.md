# Knowledge Model — GoGraph

Authoritative description of the GoGraph knowledge graph (a Label Property Graph stored
in `rmp graph`). The graph and this file **must mirror each other**: whenever a label,
edge type, or property is added or removed, update both in the same change.

- **Roadmap:** `gograph` (all commands take `-r gograph`).
- **Module:** `github.com/FlavioCFOliveira/GoGraph`.
- **Scope:** *full code graph* — every package, type, function, method, test, benchmark,
  fuzz target, and runnable example in the module, plus a curated layer of Features and
  Specs above them.
- **Provenance:** every node **and** every edge carries `gitCommit` (full HEAD hash when the
  element was last confirmed) and `gitDate` (ISO `YYYY-MM-DD`).

Counts as of commit `dd20de6` (2026-06-02): **11,418 nodes**, **14,778 edges**.

---

## Node labels

| Label | Meaning | Properties (beyond `gitCommit`, `gitDate`) |
|---|---|---|
| `Package` | A Go package (one per source directory). | `name` (package clause), `path` (repo-relative dir, `"."` for root), `importPath` (full), `kind` |
| `Type` | A `type` declaration. | `name`, `pkg` (importPath), `file` (repo-relative), `kind`, `exported` (bool), `generic` (bool) |
| `Function` | A top-level `func` with no receiver that is not a Test/Benchmark/Fuzz/Example. | `name`, `pkg`, `file`, `exported`, `generic` |
| `Method` | A `func` with a receiver. | `name`, `pkg`, `file`, `recv` (receiver type, `*` stripped), `exported` |
| `Test` | A `func TestXxx(*testing.T)`-style function (name prefix `Test`). | `name`, `pkg`, `file` |
| `Benchmark` | A `func BenchmarkXxx` (name prefix `Benchmark`). | `name`, `pkg`, `file` |
| `FuzzTarget` | A `func FuzzXxx` (name prefix `Fuzz`). | `name`, `pkg`, `file` |
| `Example` | A runnable godoc `func ExampleXxx` (name prefix `Example`). | `name`, `pkg`, `file` |
| `Spec` | A documentation/specification file under `docs/` (plus root `README.md`/`CHANGELOG.md`). | `name` (basename), `path` (repo-relative), `title` (first `# ` heading) |
| `Feature` | A curated major capability of the module. | `name`, `description` |
| `Sprint` | A planning sprint from the `rmp` roadmap. | `id` (int), `name`, `status` (`OPEN`\|`CLOSED`\|`PENDING`), `objective` |
| `Commit` | A git commit that delivered one or more tasks. | `hash` (short 7-char), `fullHash`, `message`, `sprintId` (int) |

### Enumerated property values

- `Package.kind` ∈ `library` \| `example` \| `internal` \| `command` \| `bench`.
- `Type.kind` ∈ `struct` \| `interface` \| `alias` (i.e. `type A = B`) \| `signature`
  (function type) \| `defined` (any other named/defined type).

### Counts by label (commit `dd20de6`)

| Label | Count |
|---|---|
| `Test` | 3990 |
| `Method` | 3301 |
| `Function` | 2813 |
| `Type` | 948 |
| `Example` | 118 |
| `Benchmark` | 105 |
| `Package` | 92 |
| `Spec` | 30 |
| `Feature` | 16 |
| `FuzzTarget` | 5 |

---

## Edge types

All edges carry `gitCommit` and `gitDate`.

| Type | Endpoints | Meaning |
|---|---|---|
| `CONTAINS` | `(Package)-[:CONTAINS]->(Package)` | Directory nesting: parent package → nearest descendant package. |
| `CONTAINS` | `(Package)-[:CONTAINS]->(Type\|Function\|Method\|Test\|Benchmark\|FuzzTarget\|Example)` | A package contains a symbol declared in one of its files. |
| `HAS_METHOD` | `(Type)-[:HAS_METHOD]->(Method)` | A method's receiver type, matched within the same package (`Method.recv == Type.name`). |
| `IMPLEMENTS` | `(Package)-[:IMPLEMENTS]->(Feature)` | A package realises a curated feature (path-prefix rules below). |
| `SPECIFIED_IN` | `(Feature)-[:SPECIFIED_IN]->(Spec)` | A feature is documented in a specification file. |
| `CONTAINS` | `(Sprint)-[:CONTAINS]->(Commit)` | A sprint contains a commit that delivered work within it. |
| `FIXES` | `(Commit)-[:FIXES]->(Feature)` | A commit fixes a bug in (or hardens) a feature area. |

### Counts by edge type (commit `dd20de6`)

| Type | Count |
|---|---|
| `CONTAINS` | 11371 |
| `HAS_METHOD` | 3301 |
| `IMPLEMENTS` | 87 |
| `SPECIFIED_IN` | 19 |

---

## Feature taxonomy

The 16 curated `Feature` nodes (a deliberately small, reviewed set — not auto-derived):

`Core Graph Model`, `Search & Path-finding`, `Persistence Backends`, `WAL & Recovery`,
`ACID Transactions`, `Cypher Engine`, `openCypher TCK Compliance`, `Bolt Protocol`,
`Data Structures`, `Benchmarking & Profiling`, `Production-Readiness Test Battery`,
`Stable Element Identity`, `Observability & Metrics`, `CLI Tooling`,
`Examples & Tutorials`, `Release & Versioning`.

### `IMPLEMENTS` mapping rules (package path → feature)

A package maps to features by its repo-relative directory prefix (a package may map to
several features):

| Path prefix | Feature(s) |
|---|---|
| `graph` | Core Graph Model |
| `search` | Search & Path-finding |
| `cypher/tck` | openCypher TCK Compliance **and** Cypher Engine |
| `cypher` (other) | Cypher Engine |
| `store/wal`, `store/recovery` | WAL & Recovery **and** ACID Transactions |
| `store/txn` | ACID Transactions |
| `store` (other) | Persistence Backends |
| `bolt` | Bolt Protocol |
| `ds` | Data Structures |
| `bench` | Benchmarking & Profiling |
| `examples/*` | Examples & Tutorials |
| `cmd`, `tools` | CLI Tooling |
| `internal/crashinject` | ACID Transactions **and** Production-Readiness Test Battery |
| `internal/testlayers` (or any `crashinject`) | Production-Readiness Test Battery |

Packages outside these prefixes (e.g. assorted `internal/*` helpers) implement no feature
and have no `IMPLEMENTS` edge — this is expected, not a defect.

### `SPECIFIED_IN` mapping (feature → spec path)

| Feature | Spec(s) |
|---|---|
| Cypher Engine | `docs/cypher.md` |
| openCypher TCK Compliance | `docs/cypher.md` |
| ACID Transactions | `docs/acid-audit.md`, `docs/isolation-design.md` |
| WAL & Recovery | `docs/persistence.md`, `docs/csrfile-v1.md` |
| Persistence Backends | `docs/persistence.md`, `docs/io.md` |
| Search & Path-finding | `docs/algorithms.md` |
| Bolt Protocol | `docs/bolt.md` |
| Benchmarking & Profiling | `docs/profiling.md`, `docs/optimisations.md` |
| Production-Readiness Test Battery | `docs/test-battery.md`, `docs/test-layers.md` |
| Stable Element Identity | `docs/maxnodeid.md` |
| Observability & Metrics | `docs/metrics.md` |
| Examples & Tutorials | `docs/examples-standard.md` |
| Release & Versioning | `docs/semver.md`, `docs/release.md` |

Some `Spec` nodes (e.g. `docs/tier2.md`) are intentionally unlinked — not every document
maps onto a feature.

---

## ⚠️ Guard-rail gotcha — `set`/`delete`/`remove`/`detach`

`rmp graph` enforces operation-class guard-rails by **scanning the raw Cypher text** for the
write-keywords `SET`, `DELETE`, `REMOVE`, `DETACH` (whole-word, case-insensitive). This trips
on those words appearing **inside string data** — both when writing and when reading:

- `create`/`update`/`delete` reject a query if a forbidden keyword for the wrong class
  appears anywhere, including inside a quoted literal.
- `query`/`search` reject a read whose literals contain `SET`/`DELETE`/`REMOVE`/`DETACH`
  (e.g. `WHERE m.name = 'Delete'` is rejected).

GoGraph's own source is full of such identifiers (`Delete`, `Set`, `RemoveLabel`,
`detach_delete.go`, …). **Workaround: split the keyword with Cypher string concatenation** so
the raw text never contains the contiguous token, while the evaluated value is byte-identical:

```cypher
-- write (creation):
CREATE (m:Method {name:'Dele'+'te', ...})
-- read:
MATCH (m:Method) WHERE m.name = 'Dele'+'te' RETURN m
MATCH (n) WHERE n.file ENDS WITH 'se'+'t.go' RETURN n
```

When querying for symbols whose names contain these tokens, prefer a guard-safe substring
(`CONTAINS 'elete'`, `CONTAINS 'emove'`) or the split-literal form above.

---

## Maintenance

### Bootstrap / full rebuild

The graph was materialised from an AST extractor (`go/parser`, stdlib-only) that emits
batched `UNWIND … CREATE` Cypher files; the extractor lives at `/tmp/kgextract.go` (a
throwaway tool, not part of the module) and is run as:

```bash
COMMIT=$(git log -1 --format="%H"); DATE=$(git log -1 --format="%ad" --date=format:"%Y-%m-%d")
go run /tmp/kgextract.go "$PWD" "github.com/FlavioCFOliveira/GoGraph" "$COMMIT" "$DATE" /tmp/kgcypher
for f in $(ls /tmp/kgcypher/*.cypher | sort); do rmp graph create -r gograph < "$f"; done
```

The `q()` helper in the extractor applies the concatenation split described above to every
string value, so creation never trips the guard-rail.

### Post-commit sync

Reconcile only what changed:

```bash
git diff --name-only HEAD~1 HEAD
```

For each changed `.go` file: bump the provenance of its package and surviving symbols,
`CREATE` new symbols (+ `CONTAINS`/`HAS_METHOD`), and `DETACH DELETE` removed ones; refresh
`Feature`/`Spec` provenance when their backing files change. Because the graph is large,
a full rebuild (wipe + re-materialise) is also acceptable and is the simplest way to stay
exactly in sync after broad changes.

---

## Known limitations (faithful, by design)

- **Build-tag duplicates.** The extractor parses every `.go` file regardless of build
  constraints, so a symbol declared once per platform/tag (e.g. `Reader.setHint` in
  `store/csrfile`) appears as multiple nodes that differ only by `file`. This is faithful
  to the source tree. `HAS_METHOD` edges are de-duplicated to one per `(Type, Method)` pair.
- **No `TESTS` edges.** Tests/benchmarks/fuzz/examples are linked to their package via
  `CONTAINS` only; they are **not** linked to the specific function/feature they exercise,
  because that mapping cannot be derived faithfully from the AST without guessing.
- **Curated layers.** `Feature` nodes and the `IMPLEMENTS`/`SPECIFIED_IN` edges are a
  human-reviewed interpretation, not a mechanical extraction; revise the mapping tables
  above when the architecture changes.

# CSR File Format (v1)

This document specifies the binary, mmap-friendly on-disk format
used by GoGraph's Tier 2 (out-of-core) CSR storage. The format is
versioned and stable.

## Design goals

- Direct mmap into typed `[]uint64` slices without parsing.
- 64-byte alignment for every section so SIMD loads and cache-line
  reads are well-aligned.
- Single fsync point at the file tail (CRC32C) so partial writes
  are detected on open.
- Read-only after creation: the format does not accommodate
  in-place updates; new generations land in new files.

## Header (64 bytes total, all little-endian)

| Offset | Size | Field           | Description                              |
|--------|------|-----------------|------------------------------------------|
| 0      | 4 B  | magic           | ASCII `GGCS`                             |
| 4      | 2 B  | version         | uint16 — currently 1                     |
| 6      | 1 B  | byteOrder       | 0 = little-endian (only supported value) |
| 7      | 1 B  | alignment       | uint8 — section alignment in bytes (64) |
| 8      | 8 B  | nVertices       | uint64                                   |
| 16     | 8 B  | nEdges          | uint64                                   |
| 24     | 1 B  | weightKind      | 0 = absent, 1 = u32, 2 = u64, 3 = f32, 4 = f64, 5 = u8, 6 = u16 |
| 25     | 7 B  | reserved        | must be zero                             |
| 32     | 8 B  | verticesOffset  | uint64, multiple of alignment            |
| 40     | 8 B  | edgesOffset     | uint64, multiple of alignment            |
| 48     | 8 B  | weightsOffset   | uint64, multiple of alignment (0 if absent) |
| 56     | 8 B  | tailCRCOffset   | uint64 — location of the file-tail CRC  |

## Sections

Each section is 64-byte aligned. Padding bytes between sections are
zero.

| Section  | Size                       |
|----------|----------------------------|
| vertices | 8 × nVertices bytes        |
| edges    | 8 × nEdges bytes           |
| weights  | weightSize × nEdges bytes  |

`weightSize` is 0 for `weightKind=0`, 1 for u8, 2 for u16, 4 for u32/f32,
8 for u64/f64.

Kinds 5 and 6 were **added** by rmp #2529, to reconcile this format's accepted
weight set with `store/snapshot`'s — the two durable representations of the same
graph previously disagreed about which weight types they would persist. The
addition is backward compatible in both directions that matter: values 0-4 keep
their meaning, so every file written by an earlier build reads unchanged, and an
earlier reader meeting a 5 or a 6 refuses it with `ErrUnknownWeightKind` rather
than misreading it. The format `version` is therefore still 1.

The 1- and 2-byte kinds carry the signed and unsigned type of that width, and
`u8` additionally carries `bool`; the on-disk bytes are the value's native
little-endian representation in every case.

`int`, `uint` and `uintptr` are persisted under `u64`. They are
**platform-dependent widths**, accepted deliberately because `store/snapshot`
accepts them and the two sets are reconciled — a file carrying them, written on a
64-bit build, would be misread by a 32-bit one.

### Within-source order is DERIVED, never trusted from disk

The `edges` section is grouped by source: source `i` owns the half-open range
`vertices[i] .. vertices[i+1]`. **The order of entries WITHIN one source's range
is derived at build time from the adjacency by `csr.BuildFromAdjList`; it is
never read from, nor trusted from, disk.**

Since rmp #2141 that build orders each source's run by the total key
`(destination, handle)` (`csr.OrderRuns`). A reader must not depend on it:
`store/snapshot.ApplyCSRToGraph` replays the file in whatever order it finds,
and the next build re-derives the order from the rebuilt adjacency.

Two consequences matter for this format:

- **A change to the ordering rule needs no version bump.** A file written before
  #2141 carries insertion-ordered runs and reopens correctly under the current
  reader, because the reader never assumes an order. This is asserted by a
  recovery test that reopens a deliberately UNORDERED snapshot (built via
  `csr.FromArrays`, which by contract does not order) and requires an identical
  recovered graph.
- **Byte-identity holds only between builds that apply the same rule.** Two
  snapshots of the same logical graph are byte-identical only if both were
  produced by a build that orders. `csr.FromArrays` deliberately does **not**
  order — it is the zero-copy path and carries no O(E) pass — so a caller that
  assembles arrays itself and wants byte-identity with `BuildFromAdjList` must
  call `csr.OrderRuns` on them first, as `store/bulk` does.

The same rule is stated for the snapshot as a whole in
[`persistence.md`](persistence.md).

## Trailer

The last 4 bytes of the file are a uint32 LE CRC32C (Castagnoli)
covering every byte from offset 0 up to but not including the
CRC. Readers verify on open; mismatch returns `ErrFileCorrupted`.

## Versioning

Version is bumped when any header field is removed, repurposed, or
when a new section is added that cannot be skipped by an older
reader. Backwards-compatible additions (new optional sections
described by additional header fields) keep the same version when
the new fields default cleanly to 0 / absent.

## Rolling-upgrade harness

`store/csrfile/format_compat_test.go` consumes a frozen v1 fixture at
`store/csrfile/testdata/v1/sample.csr` to assert that any future
build still maps the file correctly. A complementary test bumps the
version byte in memory and verifies the decoder returns
[`ErrUnsupportedVersion`] without silent mis-parsing.

The fixture itself is regenerated by:

```bash
go run ./cmd/fmtfixture -pkg csrfile
```

Commit the refreshed `testdata/v1/sample.csr` alongside any writer
change that intentionally alters the on-disk shape.

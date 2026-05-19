# Tier 2 — Out-of-core CSR

Tier 2 stores the CSR view on disk in an mmap-friendly binary file
and runs algorithms over the mapped region without first loading
the graph into the Go heap. It is the substrate for graphs that
exceed RAM.

## Packages

| Package              | Role                                                |
|----------------------|-----------------------------------------------------|
| `store/csrfile`      | On-disk format, writer, mmap-backed reader.         |
| `store/csrfile`      | `Reinterpret[T]` zero-copy helper.                  |
| `store/csrfile`      | `BuildFixture` deterministic test generator.        |
| `graph/generation`   | Atomic pointer swap for snapshot rotation.          |
| `search/extern`      | Semi-external BFS and PageRank.                     |

## File format

The full on-disk layout is documented in
[`docs/csrfile-v1.md`](csrfile-v1.md). At a glance:

- 64-byte header: magic `GGCS`, version, alignment, nVertices,
  nEdges, weightKind, section offsets, tail-CRC offset.
- 64-byte-aligned sections: vertices, edges, optional weights.
- Trailing uint32 CRC32C (Castagnoli) over the entire body.

`store/csrfile.WriteToFile` writes the file atomically: data lands
in `<path>.tmp`, the file is fsynced, then `os.Rename` publishes
it. Concurrent readers see either the old file or the new file,
never a partial write.

## Reading via mmap

```go
r, err := csrfile.Open("/data/graph.csr")
if err != nil { return err }
defer r.Close()

verts := r.Vertices()
edges := r.Edges()
w, ok := r.WeightsUint64()
```

`Vertices`, `Edges`, and the typed weight slices alias the
read-only mmap region — they must not be mutated, and they remain
valid only while the `Reader` is open.

## madvise hints

`Reader.SetHint(pattern)` issues `madvise` on Linux / Darwin / BSDs
to inform the kernel about the expected access pattern:

| Pattern              | Meaning                                                  |
|----------------------|----------------------------------------------------------|
| `AccessSequential`   | Pages will be read in order; aggressive read-ahead.      |
| `AccessRandom`       | No read-ahead; pages may be paged out aggressively.      |
| `AccessWillNeed`     | Prefault the entire range into RAM.                      |
| `AccessDontNeed`     | Hint the kernel can evict the range.                     |
| `AccessDefault`      | Reset to the kernel default.                             |

On platforms without madvise (Windows, Plan 9, …) `SetHint` is a
build-tag-gated no-op so cross-platform code compiles unchanged.

## Semi-external algorithms

Semi-external algorithms keep vertex-sized state (visited bitset,
rank arrays) in RAM and stream adjacency from the mmap region.

- `search/extern.BFS(reader, src, visit)` — wavefront BFS with a
  packed uint64 visited bitset; the per-level frontier is sorted
  before expansion to keep edge access sequential.
- `search/extern.PageRank(reader, opts)` — power-iteration
  PageRank. Rank arrays are allocated up-front; each iteration
  is one sequential pass over the edges section.

Both algorithms benefit from `SetHint(AccessSequential)` on the
reader before they run.

## Generation rotation

`graph/generation.Publisher[W]` holds the current CSR snapshot
under an `atomic.Pointer[Generation[W]]`. Readers `Acquire` the
current generation (with retry-on-swap to avoid leaking refcounts
on a stale generation) and `Release` when done. A publisher prepares
the next generation in a fresh allocation and `Publish` swaps the
pointer; `PublishWithDrain(c, timeout)` additionally blocks until
the previous generation's refcount drops to zero, which is the
right primitive for unmapping the old Tier 2 file.

## Producing a Tier 2 file

```go
g := lpg.New[string, int64](adjlist.Config{Directed: true})
// ... populate ...
c := csr.BuildFromAdjList(g.AdjList())
if _, err := csrfile.WriteToFile("/data/graph.csr", c); err != nil {
    return err
}
```

## Reading and querying a Tier 2 file

```go
r, err := csrfile.Open("/data/graph.csr")
if err != nil { return err }
defer r.Close()
_ = r.SetHint(csrfile.AccessSequential)

ranks, iters := extern.PageRank(r, extern.DefaultPageRankOptions())
fmt.Printf("PageRank converged in %d iterations\n", iters)
_ = ranks
```

## Limitations of v1

- The Tier 2 file carries only the CSR adjacency; labels and
  properties remain in the in-memory LPG (a future revision will
  extend the format).
- `Reinterpret` requires the source byte slice to be naturally
  aligned for the destination type; the writer guarantees this for
  every section.
- `madvise` is a hint; on platforms without it, performance falls
  back to the kernel default. This is documented and intentional.

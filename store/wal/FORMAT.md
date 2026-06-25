# GoGraph Write-Ahead Log Format

This document specifies the on-disk binary format of a GoGraph
Write-Ahead Log (WAL) file. The format is **versioned**: each frame
declares its version, and readers refuse versions newer than they
know how to parse.

## File-level layout

A WAL file is a sequence of frames written back-to-back. There is no
file-level header beyond the first frame's header (every frame is
self-describing).

## Frame layout

Each frame has the following byte layout:

| Field   | Size  | Description                                                |
|---------|-------|------------------------------------------------------------|
| magic   |  4 B  | ASCII `GGWA` (`0x47 0x47 0x57 0x41`)                       |
| version |  2 B  | uint16, little-endian. The current format version is `1`.  |
| length  |  4 B  | uint32, little-endian. Length of the payload in bytes.     |
| crc32c  |  4 B  | uint32, little-endian. CRC32C of magic+version+length+payload using the Castagnoli polynomial (`0x1EDC6F41`). |
| payload | N B   | `length` bytes of opaque payload supplied by the caller.   |

Total frame size: `14 + length` bytes.

## Versioning

Version `1` carries the format described above. Future versions must
either reuse this header (preserving magic+version+length+crc32c) or
introduce a new magic. Readers that encounter a version higher than
their highest supported version return `ErrUnsupportedVersion`.

## Integrity

The CRC32C field covers the magic bytes, the version, the length,
and the payload. Any single-bit flip in any of these fields is
detected with the probability guaranteed by CRC32C (effectively
2⁻³² for unrelated bit flips).

Torn writes — partial last frames — are detected by the length /
CRC mismatch and reported as `ErrTornFrame`. Readers stop cleanly
at the last fully-readable frame; recovery resumes from there.

A corrupt `length` field that *over-declares* past the end of the
file produces the same end-of-input that a genuine torn tail does,
because the decoder reaches EOF while reading the (impossibly long)
payload before it can verify the CRC that covers the length field.
To stop such corruption from masquerading as a benign tail — which
would silently discard every durable frame physically located after
the corrupt one — the decoder inspects the bytes the over-long read
actually consumed. If a structurally complete, CRC-valid frame begins
anywhere inside those bytes, the consumed region was not opaque
payload but the durable frames that follow, so the decoder returns
`ErrTornFrameMasksData` (a hard error) instead of `ErrTornFrame`.
A real torn tail's opaque payload bytes match a CRC-valid frame only
with the ~2⁻³² per-offset probability of a CRC collision, so a
legitimate crash tail is not misclassified.

As a defence-in-depth measure, the decoder rejects any frame whose
declared `length` exceeds 1 GiB (`maxFrameSize`) with `ErrFrameTooLarge`
*before* allocating the payload buffer. The `length` field is a uint32,
so the format already bounds a payload to ~4 GiB; the 1 GiB ceiling
caps the pathological case where a corrupted or crafted length would
otherwise force a large one-shot allocation ahead of the CRC check. The
ceiling sits far above any legitimate WAL frame, which carries a single
transaction rather than bulk data.

## Concurrency

The format itself imposes no concurrency model. The `wal.Writer`
implementation is single-writer; multiple goroutines must serialise
their writes externally. The `wal.Reader` is read-only and may be
shared by multiple goroutines provided each holds its own offset.

## Forward compatibility

- New top-level fields may be added by bumping the version.
- The payload structure is opaque to the WAL; higher-level callers
  (transaction codec, snapshot consolidator) may freely evolve the
  payload encoding without bumping the frame version, provided the
  payload remains a self-describing byte sequence.

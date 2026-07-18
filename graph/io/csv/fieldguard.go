package csv

import (
	"errors"
	"io"
)

// maxFieldsPerRecord bounds the number of delimiter-separated fields the reader
// tolerates in a single CSV record.
//
// [encoding/csv] allocates O(fields) of per-record metadata — its fieldIndexes
// and fieldPositions slices plus the returned record slice, roughly 40 bytes
// per field — while this importer consumes at most three columns (src, dst,
// weight). With no bound, a single record made of nothing but delimiters
// amplifies its input by ~40x: at the 128 MiB [DefaultMaxBytes] cap that is
// several GiB of transient heap from one accepted file — a memory-exhaustion
// denial of service on untrusted input (CWE-789 / CWE-1284).
//
// 65536 is far beyond any realistic edge-list CSV (even a wide, many-column
// file), so the guard never rejects legitimate input, yet it caps a hostile
// record's per-record working set to a few MiB — comfortably within the
// [DefaultMaxBytes] peak-memory contract. Fields beyond the third are ignored
// by the importer anyway; this cap rejects only records whose field count is
// itself the attack.
const maxFieldsPerRecord = 65536

// ErrTooManyFields is returned by [ReadInto] / [ReadIntoCtx] (wrapped with the
// offending row) when a single CSV record exceeds [maxFieldsPerRecord]
// delimiter-separated fields.
var ErrTooManyFields = errors.New("csv: record exceeds maximum field count")

// fieldGuardReader wraps an [io.Reader] and fails with [ErrTooManyFields] as
// soon as any single record's field count would exceed [maxFieldsPerRecord],
// BEFORE [encoding/csv] can allocate the per-field metadata for that record.
//
// It tracks CSV quote state so that delimiters and newlines inside a quoted
// field count as literal content — they neither open a new field nor end the
// record — matching [encoding/csv]'s non-lazy-quote semantics closely enough to
// never miscount a well-formed record. A single large quoted field (few
// delimiters) is therefore unaffected; its size is already bounded by the byte
// cap ([DefaultMaxBytes]).
//
// The guard is byte-oriented, so the caller engages it only for a single-byte
// (ASCII) delimiter — which covers ',', '\t', ';', '|' and ' '. For a
// multi-byte delimiter rune the caller leaves the guard off and only the byte
// cap applies; the delimiter is chosen by the importing application, never by
// the untrusted file, so that is not an attacker-reachable gap.
//
// A fieldGuardReader is single-use and not safe for concurrent use; one is
// created per import.
type fieldGuardReader struct {
	r            io.Reader
	err          error // sticky: once tripped, every subsequent Read returns it
	fields       int   // fields seen in the current record; a record has ≥1
	delim        byte
	inQuotes     bool // inside a quoted field
	atFieldStart bool // at the first byte of a field (where a '"' opens a quote)
	pendingQuote bool // inside quotes, saw a '"' awaiting the disambiguating byte
}

// newFieldGuardReader wraps r, counting records delimited by single-byte delim.
func newFieldGuardReader(r io.Reader, delim byte) *fieldGuardReader {
	return &fieldGuardReader{r: r, delim: delim, fields: 1, atFieldStart: true}
}

// Read implements [io.Reader]. It reads from the wrapped reader and inspects
// every byte to maintain the per-record field count. As soon as a record would
// exceed [maxFieldsPerRecord] it returns [ErrTooManyFields] and latches, so the
// pathological record is rejected before encoding/csv finishes allocating it.
func (g *fieldGuardReader) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.r.Read(p)
	for i := 0; i < n; i++ {
		if ferr := g.feed(p[i]); ferr != nil {
			g.err = ferr
			return 0, ferr
		}
	}
	return n, err
}

// feed advances the quote/field state machine by one byte and returns
// [ErrTooManyFields] when the current record's field count crosses the cap.
func (g *fieldGuardReader) feed(b byte) error {
	if g.inQuotes {
		if g.pendingQuote {
			g.pendingQuote = false
			if b == '"' {
				return nil // "" — an escaped quote; still inside the quoted field
			}
			g.inQuotes = false // the previous '"' closed the field
			// fall through: process b as an out-of-quotes byte (b != '"' here)
		} else if b == '"' {
			g.pendingQuote = true
			return nil
		} else {
			return nil // literal content inside quotes (delimiters/newlines included)
		}
	}
	switch {
	case g.atFieldStart && b == '"':
		g.inQuotes = true
		g.atFieldStart = false
	case b == g.delim:
		g.fields++
		g.atFieldStart = true
		if g.fields > maxFieldsPerRecord {
			return ErrTooManyFields
		}
	case b == '\n':
		g.fields = 1
		g.atFieldStart = true
	case b == '\r':
		// neutral: part of a CRLF terminator or a lone CR
	default:
		g.atFieldStart = false
	}
	return nil
}

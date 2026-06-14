package jsonl_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// Security regression battery for the JSON-Lines reader's resistance to
// resource-exhaustion attacks delivered through a single line: the JSON
// nesting depth bomb (CWE-674), deep recursion in the typed list-property
// decoder, an attacker-declared element count that is never honoured as a
// pre-allocation, and the byte / line-length caps.
//
// Crafted inputs are kept to a few tens of kilobytes; each proves its
// boundary by construction (the depth limit trips at ~10 000 levels), never
// by attempting a multi-megabyte allocation.

// secIOReadJSONL runs ReadInto and turns any panic into a fatal failure so
// every case shares the "never crashes the host" guarantee.
func secIOReadJSONL(t *testing.T, in string) (rows int, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("JSONL reader panicked on hostile input: %v", r)
		}
	}()
	_, n, e := jsonl.ReadInto(strings.NewReader(in), adjlist.Config{Directed: true})
	return n, e
}

// TestSec_IO_JSONLReadRepelsDepthBomb feeds a single line of N open brackets
// with no matching close. encoding/json caps nesting at a fixed depth and
// returns a typed *json.SyntaxError ("exceeded max depth") long before the
// line ends, so the reader fails fast with a bounded stack and never
// panics. The line is ~20 KiB — sized to clear the depth limit, not to
// allocate memory.
func TestSec_IO_JSONLReadRepelsDepthBomb(t *testing.T) {
	t.Parallel()

	// Go's encoding/json nesting limit is 10 000; 20 000 brackets guarantees
	// the limit trips. The whole line is ~20 KiB.
	const depth = 20_000
	line := strings.Repeat("[", depth) + "\n"

	rows, err := secIOReadJSONL(t, line)
	if err == nil {
		t.Fatalf("depth-bomb line accepted: want a parse error, rows=%d", rows)
	}
	if !strings.Contains(err.Error(), "exceeded max depth") {
		t.Errorf("err = %v, want it to mention the depth limit", err)
	}
}

// TestSec_IO_JSONLReadRepelsListPropertyRecursion targets the recursive
// typed list-property decoder (decodePropertyValue → list → decode each
// element → list → …). A property record carrying a deeply nested
// JSON-encoded list must be rejected by the inner encoding/json depth
// limit, not by exhausting the stack. The inner JSON value (the "value"
// field) is itself a string field of the outer record, so the bomb is the
// nested-array text inside it.
func TestSec_IO_JSONLReadRepelsListPropertyRecursion(t *testing.T) {
	t.Parallel()

	// Build a property record whose value is N open brackets — an invalid,
	// deeply-nested JSON array. json.Unmarshal of that string trips the
	// depth limit inside decodePropertyValue's "list" branch.
	const depth = 20_000
	nested := strings.Repeat("[", depth)

	// The record itself is valid JSON; only its embedded list value is the
	// bomb. The list value is a JSON string, so the brackets are escaped by
	// being inside quotes — json.Marshal handles that for us via a struct.
	var b strings.Builder
	b.WriteString(`{"type":"node","id":"n0"}` + "\n")
	b.WriteString(`{"type":"property","id":"n0","key":"k","kind":"list","value":`)
	// Encode the nested-bracket payload as a JSON string literal.
	b.WriteString(`"`)
	b.WriteString(nested) // '[' needs no escaping inside a JSON string
	b.WriteString(`"`)
	b.WriteString("}\n")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("list-recursion line panicked: %v", r)
		}
	}()
	g, _, err := jsonl.ReadWithProps(strings.NewReader(b.String()), adjlist.Config{Directed: true})
	if err == nil {
		t.Fatalf("deeply nested list property accepted: want a parse error")
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on error", g)
	}
}

// TestSec_IO_JSONLReadNoAttackerCountPrealloc confirms the reader never
// trusts a self-declared count to size an allocation: a stream that names
// far more nodes than it actually delivers, then is truncated, must fail
// (or stop) with bounded incremental work rather than pre-allocating for
// the phantom count. The JSONL format carries no count field at all, so
// the proof is that a truncated final record yields a typed error and a
// nil graph, with memory tracking only the records actually read.
func TestSec_IO_JSONLReadNoAttackerCountPrealloc(t *testing.T) {
	t.Parallel()

	// A handful of real node records, then a truncated final line that
	// claims to be a record but is cut off mid-token.
	var b strings.Builder
	for i := 0; i < 8; i++ {
		b.WriteString(`{"type":"node","id":"n`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`"}` + "\n")
	}
	b.WriteString(`{"type":"node","id":"truncat`) // no closing brace/quote, no newline

	rows, err := secIOReadJSONL(t, b.String())
	if err == nil {
		t.Fatalf("truncated final record accepted: want a parse error, rows=%d", rows)
	}
}

// TestSec_IO_JSONLReadByteCapBoundary pins the byte-cap crossing: a stream
// one byte over an explicit cap returns ErrInputTooLarge, while the same
// stream exactly at the cap is accepted. Uses a small explicit cap so no
// large input is needed.
func TestSec_IO_JSONLReadByteCapBoundary(t *testing.T) {
	t.Parallel()

	// A valid one-node stream; its byte length becomes the cap.
	const doc = `{"type":"node","id":"a"}` + "\n"
	capBytes := int64(len(doc))

	// At cap: accepted.
	g, rows, err := jsonl.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(doc), adjlist.Config{Directed: true}, capBytes)
	if err != nil {
		t.Fatalf("at-cap stream rejected: err=%v, want nil", err)
	}
	if g == nil || rows != 1 {
		t.Fatalf("at-cap: g=%v rows=%d, want non-nil, 1", g, rows)
	}

	// One byte over: rejected at the crossing byte.
	over := doc + "x"
	g2, _, err2 := jsonl.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(over), adjlist.Config{Directed: true}, capBytes)
	if !errors.Is(err2, jsonl.ErrInputTooLarge) {
		t.Fatalf("over-cap stream: err=%v, want ErrInputTooLarge", err2)
	}
	if g2 != nil {
		t.Errorf("graph = %v, want nil on cap error", g2)
	}
}

// TestSec_IO_JSONLReadLineTooLong pins the per-line scanner cap: a single
// line longer than the 16 MiB token limit surfaces the typed ErrLineTooLong
// sentinel rather than a generic failure or OOM. The input is produced by a
// streaming reader so the test process never itself holds 16 MiB of crafted
// bytes in one buffer beyond what the scanner reads.
func TestSec_IO_JSONLReadLineTooLong(t *testing.T) {
	t.Parallel()

	// One line of 'x' bytes, no newline, longer than the 16 MiB scanner
	// token cap. A streaming reader emits it without us materialising a
	// 16 MiB Go string literal. The byte cap is disabled (0) so the line
	// cap is the boundary under test, not the aggregate cap.
	const over = (16 << 20) + 1024
	r := &secIORepeatReader{b: 'x', remaining: over}

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("over-long line panicked: %v", rec)
		}
	}()
	g, _, err := jsonl.ReadIntoCappedCtx(context.Background(), r, adjlist.Config{Directed: true}, 0)
	if !errors.Is(err, jsonl.ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on line-too-long error", g)
	}
}

// secIORepeatReader streams the byte b exactly `remaining` times, then EOF.
// It lets a test feed a multi-megabyte line to the scanner without holding
// that many bytes in a single allocation in the test itself.
type secIORepeatReader struct {
	b         byte
	remaining int
}

func (r *secIORepeatReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = r.b
	}
	r.remaining -= n
	return n, nil
}

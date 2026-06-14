package jsonl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// FuzzSec_IO_JSONLReadWithProps fuzzes the typed-property JSON-Lines read
// path (ReadWithProps), including its recursive list-property decoder,
// which was previously unfuzzed. The corpus seeds one valid property record
// per kind — string / int64 / float64 / bool / time / base64-bytes / list,
// plus a nested list — so the fuzzer mutates the kind tags, the encoded
// value strings, and the nested-list structure.
//
// The body asserts the contract every mutation must uphold: the reader
// never panics, and on failure it returns a non-nil error with a nil graph
// (all-or-nothing). It runs as an ordinary unit test over the seed corpus
// under `go test`; pass -fuzz to explore further.
func FuzzSec_IO_JSONLReadWithProps(f *testing.F) {
	seeds := []string{
		// node + one property of each kind, one per line.
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"s","kind":"string","value":"hello"}`,
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"i","kind":"int64","value":"42"}`,
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"f","kind":"float64","value":"3.14"}`,
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"f","kind":"float64","value":"NaN"}`,
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"b","kind":"bool","value":"true"}`,
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"t","kind":"time","value":"2024-01-15T12:00:00Z"}`,
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"blob","kind":"bytes","value":"3q2+7w=="}`,
		// list with a nested list — exercises the recursive decoder.
		`{"type":"node","id":"n0"}` + "\n" +
			`{"type":"property","id":"n0","key":"l","kind":"list","value":"[[\"int64\",\"1\"],[\"list\",\"[[\\\"int64\\\",\\\"2\\\"]]\"]]"}`,
		// node + edge + property together.
		`{"type":"node","id":"a"}` + "\n" +
			`{"type":"node","id":"b"}` + "\n" +
			`{"type":"edge","src":"a","dst":"b","weight":5}` + "\n" +
			`{"type":"property","id":"a","key":"i","kind":"int64","value":"7"}`,
		// unknown record type — must be a typed ErrUnknownType, never a panic.
		`{"type":"wat","id":"x"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Cap the input so a pathological mutation cannot stall the worker;
		// the reader must still be fail-closed below the cap.
		const maxFuzzBytes = 64 << 10
		if len(data) > maxFuzzBytes {
			data = data[:maxFuzzBytes]
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReadWithProps panicked on input %q: %v", data, r)
			}
		}()

		g, n, err := jsonl.ReadWithPropsCappedCtx(
			context.Background(), strings.NewReader(string(data)),
			adjlist.Config{Directed: true}, maxFuzzBytes)
		if err != nil {
			// Contract: on any error the graph is nil and the row count is
			// not negative.
			if g != nil {
				t.Fatalf("error returned but graph is non-nil: err=%v", err)
			}
			if n < 0 {
				t.Fatalf("error returned but row count = %d (< 0)", n)
			}
			// Matching a sentinel must not panic on whatever error escaped.
			_ = errors.Is(err, jsonl.ErrUnknownType)
			_ = errors.Is(err, jsonl.ErrInputTooLarge)
			return
		}
		if g == nil {
			t.Fatalf("nil error but nil graph")
		}
		if n < 0 {
			t.Fatalf("negative row count %d", n)
		}
	})
}

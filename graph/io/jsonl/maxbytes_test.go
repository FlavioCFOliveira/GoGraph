package jsonl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	jsonl "github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// hugeLine builds a single JSONL line whose "id" value is n bytes long,
// with no trailing newline so the scanner must buffer it as one token.
func hugeLine(n int) string {
	return `{"type":"node","id":"` + strings.Repeat("x", n) + `"}`
}

// TestReadIntoCappedCtx_Exceeded feeds a single line far larger than a
// low cap and asserts ErrInputTooLarge from the capped variant.
func TestReadIntoCappedCtx_Exceeded(t *testing.T) {
	t.Parallel()

	const capBytes = 1024
	a, n, err := jsonl.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(hugeLine(capBytes*8)), adjlist.Config{Directed: true}, capBytes)
	if !errors.Is(err, jsonl.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on cap error", a)
	}
	_ = n
}

// TestReadWithPropsCappedCtx_Exceeded is the property-graph analogue.
func TestReadWithPropsCappedCtx_Exceeded(t *testing.T) {
	t.Parallel()

	const capBytes = 1024
	g, _, err := jsonl.ReadWithPropsCappedCtx(context.Background(),
		strings.NewReader(hugeLine(capBytes*8)), adjlist.Config{Directed: true}, capBytes)
	if !errors.Is(err, jsonl.ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on cap error", g)
	}
}

// TestReadInto_DefaultCapAllowsSmallInput is the control: the default
// entry point applies DefaultMaxBytes yet decodes a small stream fine.
func TestReadInto_DefaultCapAllowsSmallInput(t *testing.T) {
	t.Parallel()

	const doc = `{"type":"node","id":"a"}
{"type":"node","id":"b"}
{"type":"edge","src":"a","dst":"b","weight":3}
`
	a, n, err := jsonl.ReadInto(strings.NewReader(doc), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil || n != 3 {
		t.Fatalf("a=%v rows=%d, want non-nil, 3", a, n)
	}
}

// TestReadWithProps_DefaultCapAllowsSmallInput is the property-graph
// control through the default entry point.
func TestReadWithProps_DefaultCapAllowsSmallInput(t *testing.T) {
	t.Parallel()

	const doc = `{"type":"node","id":"a"}
{"type":"property","id":"a","key":"age","value":"30","kind":"int64"}
`
	g, n, err := jsonl.ReadWithProps(strings.NewReader(doc), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if g == nil || n != 2 {
		t.Fatalf("g=%v rows=%d, want non-nil, 2", g, n)
	}
}

// TestReadIntoCappedCtx_Disabled confirms a non-positive cap opts out.
func TestReadIntoCappedCtx_Disabled(t *testing.T) {
	t.Parallel()

	// A line larger than DefaultMaxBytes is impractical to allocate in a
	// unit test; instead assert that a modest line which would trip a
	// tiny cap passes once the cap is disabled.
	doc := hugeLine(4096)
	a, _, err := jsonl.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(doc), adjlist.Config{Directed: true}, 0)
	if err != nil {
		t.Fatalf("cap disabled but got error: %v", err)
	}
	if a == nil {
		t.Fatal("graph is nil")
	}
}

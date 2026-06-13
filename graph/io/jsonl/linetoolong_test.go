package jsonl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	jsonl "github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// TestReadInto_LineTooLong asserts that a single record exceeding the
// 16 MiB scanner token limit surfaces the typed ErrLineTooLong sentinel
// (errors.Is-able) rather than the raw bufio "token too long" string.
// The byte cap is well above the line, so the line cap trips first (#1442).
func TestReadInto_LineTooLong(t *testing.T) {
	t.Parallel()

	line := hugeLine(17 << 20) // 17 MiB > 16 MiB scanner token cap
	_, _, err := jsonl.ReadInto(strings.NewReader(line), adjlist.Config{Directed: true})
	if !errors.Is(err, jsonl.ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

// TestReadWithProps_LineTooLong is the property-graph analogue.
func TestReadWithProps_LineTooLong(t *testing.T) {
	t.Parallel()

	line := hugeLine(17 << 20)
	_, _, err := jsonl.ReadWithPropsCappedCtx(context.Background(),
		strings.NewReader(line), adjlist.Config{Directed: true}, 0) // cap disabled
	if !errors.Is(err, jsonl.ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

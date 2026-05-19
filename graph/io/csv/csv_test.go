package csv

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadInto_Basic(t *testing.T) {
	t.Parallel()
	in := "a,b,1\nb,c,2\nc,a,3\n"
	a, n, err := ReadInto(strings.NewReader(in), DefaultOptions())
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	if !a.HasEdge("a", "b") || !a.HasEdge("b", "c") || !a.HasEdge("c", "a") {
		t.Fatalf("missing edge")
	}
}

func TestReadInto_HeaderSkip(t *testing.T) {
	t.Parallel()
	in := "src,dst,weight\na,b,1\n"
	opts := DefaultOptions()
	opts.HasHeader = true
	a, n, err := ReadInto(strings.NewReader(in), opts)
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if !a.HasEdge("a", "b") {
		t.Fatalf("missing edge after header skip")
	}
}

func TestReadInto_Comments(t *testing.T) {
	t.Parallel()
	in := "# this is a comment\na,b,1\n# another\nb,c,2\n"
	a, n, err := ReadInto(strings.NewReader(in), DefaultOptions())
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	_ = a
}

func TestReadInto_BadWeight(t *testing.T) {
	t.Parallel()
	in := "a,b,not-a-number\n"
	_, _, err := ReadInto(strings.NewReader(in), DefaultOptions())
	if err == nil {
		t.Fatalf("expected weight parse error")
	}
}

func TestReadInto_ShortRow(t *testing.T) {
	t.Parallel()
	in := "a\n"
	_, _, err := ReadInto(strings.NewReader(in), DefaultOptions())
	if err == nil {
		t.Fatalf("expected error on short row")
	}
}

func TestWrite_Roundtrip(t *testing.T) {
	t.Parallel()
	in := "a,b,1\na,c,2\nb,c,3\n"
	a, _, err := ReadInto(strings.NewReader(in), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := Write(&buf, a, DefaultOptions())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows written = %d, want 3", n)
	}

	// Re-read and check edges survive.
	b, _, err := ReadInto(&buf, DefaultOptions())
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !b.HasEdge("a", "b") || !b.HasEdge("a", "c") || !b.HasEdge("b", "c") {
		t.Fatalf("roundtrip lost an edge")
	}
}

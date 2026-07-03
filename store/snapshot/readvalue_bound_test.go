package snapshot

// readvalue_bound_test.go — correctness contract for readLenPrefixedValue, the
// bounded length-prefixed value reader shared by the properties, mapper, and
// edgehandles snapshot component decoders (#1886).

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestReadLenPrefixedValue_Contract(t *testing.T) {
	t.Parallel()

	// n == 0 returns nil, nil (no allocation, matches the historical guard).
	if got, err := readLenPrefixedValue(bufio.NewReader(bytes.NewReader(nil)), 0); err != nil || got != nil {
		t.Fatalf("n=0: got (%v, %v), want (nil, nil)", got, err)
	}

	// Exact read on the eager path (n <= snapshotEagerReadCap).
	if got, err := readLenPrefixedValue(bufio.NewReader(bytes.NewReader([]byte("hello"))), 5); err != nil || string(got) != "hello" {
		t.Fatalf("eager exact: got (%q, %v), want (\"hello\", nil)", got, err)
	}

	// Short read on the eager path surfaces an EOF-class error.
	if _, err := readLenPrefixedValue(bufio.NewReader(bytes.NewReader([]byte("ab"))), 5); err == nil {
		t.Fatal("eager short read: want error, got nil")
	} else if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("eager short read: err = %v, want EOF class", err)
	}

	// Exact read on the large path (n > snapshotEagerReadCap) returns all bytes.
	big := make([]byte, snapshotEagerReadCap+100)
	for i := range big {
		big[i] = byte(i)
	}
	got, err := readLenPrefixedValue(bufio.NewReader(bytes.NewReader(big)), uint32(len(big)))
	if err != nil {
		t.Fatalf("large exact: unexpected err %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("large exact: got %d bytes, want %d identical", len(got), len(big))
	}

	// Short read on the large path (forged length exceeds available bytes)
	// surfaces an EOF-class error rather than returning a partial value.
	if _, err := readLenPrefixedValue(bufio.NewReader(bytes.NewReader([]byte("only-a-few"))), snapshotEagerReadCap+1); err == nil {
		t.Fatal("large short read: want error, got nil")
	}
}

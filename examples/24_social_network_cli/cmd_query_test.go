package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// TestReadQuery_Positional uses the single positional argument verbatim,
// after trimming whitespace.
func TestReadQuery_Positional(t *testing.T) {
	got, err := readQuery([]string{"  MATCH (n) RETURN n  "}, strings.NewReader(""))
	if err != nil {
		t.Fatalf("readQuery: %v", err)
	}
	if got != "MATCH (n) RETURN n" {
		t.Fatalf("got %q, want %q", got, "MATCH (n) RETURN n")
	}
}

// TestReadQuery_RejectsTrailingArguments pins the fail-stop contract on the
// argument the operator did not get to use.
//
// Go's flag package stops parsing at the first non-flag argument, so a flag
// written after the query text is a trailing POSITIONAL, not a flag. This was
// found during rmp #2377: `query -d dir "MATCH ..." -profile-dir /tmp/p`
// returned exit 0 and wrote no profile, because the two trailing arguments were
// discarded in silence. An unconsumed argument is now named and refused.
func TestReadQuery_RejectsTrailingArguments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		positional []string
	}{
		{"a mis-ordered flag", []string{"MATCH (n) RETURN n", "-profile-dir", "/tmp/p"}},
		{"one stray argument", []string{"MATCH (n) RETURN n", "stray"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readQuery(tc.positional, strings.NewReader(""))
			if err == nil {
				t.Fatal("trailing arguments accepted; want a usage error")
			}
			var ue *usageError
			if !errors.As(err, &ue) {
				t.Fatalf("want *usageError (exit 2), got %T: %v", err, err)
			}
			// The message must name what was dropped, or it does not help.
			if !strings.Contains(err.Error(), tc.positional[1]) {
				t.Errorf("error does not name the unconsumed argument %q: %v",
					tc.positional[1], err)
			}
		})
	}
}

// TestReadQuery_Stdin reads the entire stdin stream when no positional
// argument is supplied.
func TestReadQuery_Stdin(t *testing.T) {
	got, err := readQuery(nil, strings.NewReader("MATCH (n) RETURN n\n"))
	if err != nil {
		t.Fatalf("readQuery: %v", err)
	}
	if got != "MATCH (n) RETURN n" {
		t.Fatalf("got %q, want %q", got, "MATCH (n) RETURN n")
	}
}

// TestReadQuery_EmptyPositional rejects an empty positional argument
// with a usage error so the dispatcher maps the failure to exit code 2.
func TestReadQuery_EmptyPositional(t *testing.T) {
	_, err := readQuery([]string{""}, strings.NewReader(""))
	if err == nil {
		t.Fatalf("want usage error, got nil")
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("want *usageError, got %T: %v", err, err)
	}
}

// TestReadQuery_EmptyStdin rejects an empty stdin stream with a usage
// error (so users see "query required" instead of an opaque parser
// error pointed at byte 0).
func TestReadQuery_EmptyStdin(t *testing.T) {
	_, err := readQuery(nil, strings.NewReader("   \n   "))
	if err == nil {
		t.Fatalf("want usage error, got nil")
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("want *usageError, got %T: %v", err, err)
	}
}

// TestRunQuery_EmptyMatch verifies that a MATCH against a freshly
// initialised data directory emits zero output (and no trailing
// newline) and returns nil error.
func TestRunQuery_EmptyMatch(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	var buf bytes.Buffer
	if err := runQuery(context.Background(), dir, "MATCH (n) RETURN n", &buf); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("want empty output, got %q", buf.String())
	}
}

// TestRunQuery_CreateThenMatch verifies that a CREATE persists across a
// store close+reopen, and that a subsequent MATCH returns the expected
// JSONL row. This is the smallest end-to-end persistence test of the
// CLI; the round-trip in T9 covers the larger fixture.
func TestRunQuery_CreateThenMatch(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}

	// Write-only invocation (no RETURN). The engine emits one
	// synthetic empty row; runQuery skips it so the output is empty.
	var wbuf bytes.Buffer
	createQ := `CREATE (n:User {username: "alice"})`
	if err := runQuery(context.Background(), dir, createQ, &wbuf); err != nil {
		t.Fatalf("runQuery CREATE: %v", err)
	}
	if wbuf.Len() != 0 {
		t.Fatalf("CREATE produced unexpected output: %q", wbuf.String())
	}

	// Reopen and query — verifies WAL round-trip including the
	// property write produced by the CREATE clause.
	var rbuf bytes.Buffer
	matchQ := `MATCH (u:User) RETURN u.username AS username`
	if err := runQuery(context.Background(), dir, matchQ, &rbuf); err != nil {
		t.Fatalf("runQuery MATCH: %v", err)
	}
	want := `{"username":"alice"}` + "\n"
	if rbuf.String() != want {
		t.Fatalf("MATCH output: got %q, want %q", rbuf.String(), want)
	}
}

// TestRunQuery_InvalidCypher verifies that an unparsable query yields a
// non-nil error that does not unwrap to a usageError (so main maps it
// to exit code 1, not 2).
func TestRunQuery_InvalidCypher(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	var buf bytes.Buffer
	err := runQuery(context.Background(), dir, "XYZ", &buf)
	if err == nil {
		t.Fatalf("want error on invalid Cypher, got nil")
	}
	var ue *usageError
	if errors.As(err, &ue) {
		t.Fatalf("invalid Cypher must be exit 1 (not usage error), got *usageError: %v", err)
	}
}

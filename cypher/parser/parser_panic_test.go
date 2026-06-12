package parser

import (
	"sync"
	"testing"
)

// TestParserNoPanicOnIncompleteWithClause verifies that Parse and ParseStrict
// never crash the process on incomplete WITH clauses or pipe-in-arg expressions
// that previously drove ANTLR's DefaultErrorStrategy into an unchecked type
// assertion, causing a direct process panic in antlr4-go v4.13.1.
//
// Each case must return a non-nil error; no case may panic.
func TestParserNoPanicOnIncompleteWithClause(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{"bare WITH", "WITH"},
		{"WITH literal no return", "WITH 1 AS x"},
		{"MATCH WITH no return", "MATCH (n) WITH n"},
		{"WITH float no return", "WITH 0.5"},
		{"UNWIND WITH no return", "UNWIND [1] AS x WITH x"},
		{"WITH return no projection", "WITH 1 AS x RETURN"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"/Parse", func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.query)
			if err == nil {
				t.Errorf("Parse(%q): expected error, got nil", tc.query)
			}
		})
		t.Run(tc.name+"/ParseStrict", func(t *testing.T) {
			t.Parallel()
			_, errs := ParseStrict(tc.query)
			if len(errs) == 0 {
				t.Errorf("ParseStrict(%q): expected errors, got none", tc.query)
			}
		})
	}
}

// TestParserNoPanicConcurrent hammers Parse and ParseStrict concurrently with
// the panic-triggering inputs to verify that the recover() in recoverParseScript
// is goroutine-safe and does not leak state between concurrent calls.
func TestParserNoPanicConcurrent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"WITH",
		"WITH 1 AS x",
		"MATCH (n) WITH n",
		"WITH 0.5",
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines * len(inputs) * 2)

	for _, q := range inputs {
		for range goroutines {
			q := q
			go func() {
				defer wg.Done()
				// Must not panic; error value is irrelevant here.
				_, _ = Parse(q)
			}()
			go func() {
				defer wg.Done()
				_, _ = ParseStrict(q)
			}()
		}
	}

	wg.Wait()
}

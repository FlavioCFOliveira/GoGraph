package parser

// strip_keyword_test.go — the equivalence gate for [classifyClauseKeyword]
// (rmp #2849, sprint 362 parsing campaign).
//
// [StripLiterals] used to classify every identifier it scanned with
//
//	switch strings.ToUpper(tok) { case "MATCH", "WHERE": … }
//
// which allocated one string per identifier token. The switch is now an ASCII
// fold applied during the comparison, and allocates nothing.
//
// The two must agree on EVERY input, not merely on the corpus: a classifier
// that disagreed on one identifier would silently change which clause regions
// hoist, and a query would change meaning without any test failing. This file
// is that proof, on three independent routes:
//
//	route 1  every ASCII identifier of length 1-3 over the exact byte alphabet
//	         isIdentStart/isIdentChar accept — exhaustive, 213 749 tokens
//	route 2  every case permutation of all 23 keywords — 2^len each
//	route 3  each keyword suffixed with S, s, _, 0, es, ing, and truncated by
//	         one byte — the prefix/length boundary the fold must not blur
//
// The classifier is the ONLY thing that changed inside StripLiterals; the rest
// of the function — the fast reject, the clause state machine, the comment and
// backtick handling, the replacement builder — is untouched, and strip_test.go
// already fences its (stripped, params, ok) triple against golden expectations
// (TestStripLiterals_HoistsAndCollapses,
// TestStripLiterals_PreservesEverythingElseByteForByte and their six
// neighbours). Proving the classifier equivalent therefore proves the function
// equivalent.
//
// The control below is the PRE-CHANGE test, expressed as the same classifier so
// the two can be compared directly. It is deliberately a copy: if the
// production switch is ever edited, this control does not follow it, and the
// exhaustive sweep fails.

import (
	"strings"
	"testing"
)

// clauseKeywordsControl is the keyword set the pre-change switch listed, in the
// order it listed them.
var clauseKeywordsControl = []string{
	"MATCH", "WHERE",
	"RETURN", "WITH", "CREATE", "MERGE", "DELETE", "DETACH", "SET",
	"REMOVE", "UNWIND", "CALL", "YIELD", "FOREACH", "OPTIONAL",
	"UNION", "ON", "ORDER", "SKIP", "LIMIT", "FOR", "ADD", "DROP",
}

// classifyClauseKeywordControl is the classifier [StripLiterals] used before
// rmp #2849: `strings.ToUpper` and a string switch.
func classifyClauseKeywordControl(tok string) clauseKind {
	switch strings.ToUpper(tok) {
	case "MATCH", "WHERE":
		return clauseHoistOn
	case "RETURN", "WITH", "CREATE", "MERGE", "DELETE", "DETACH", "SET",
		"REMOVE", "UNWIND", "CALL", "YIELD", "FOREACH", "OPTIONAL",
		"UNION", "ON", "ORDER", "SKIP", "LIMIT", "FOR", "ADD", "DROP":
		return clauseHoistOff
	}
	return clauseOther
}

// identStartBytes is every byte isIdentStart accepts, and identContBytes every
// byte isIdentChar accepts. They are asserted against the predicates themselves
// by TestIdentAlphabetIsASCIIOnly, so the sweep below cannot silently stop
// covering the alphabet the scanner actually produces.
const (
	identStartBytes = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_"
	identContBytes  = identStartBytes + "0123456789"
)

// TestIdentAlphabetIsASCIIOnly is the premise the whole equivalence rests on:
// the scanner's identifier alphabet is ASCII and nothing else, so
// strings.ToUpper's Unicode branch is unreachable from the call site and an
// ASCII fold is not an approximation of it but the same function.
//
// It checks all 256 byte values against both predicates, so a widening of
// either one — to accept a non-ASCII byte — fails here rather than silently
// making classifyClauseKeyword wrong.
func TestIdentAlphabetIsASCIIOnly(t *testing.T) {
	for b := 0; b < 256; b++ {
		c := byte(b)
		wantStart := strings.IndexByte(identStartBytes, c) >= 0
		wantCont := strings.IndexByte(identContBytes, c) >= 0
		if got := isIdentStart(c); got != wantStart {
			t.Errorf("isIdentStart(%#02x)=%v, alphabet says %v", c, got, wantStart)
		}
		if got := isIdentChar(c); got != wantCont {
			t.Errorf("isIdentChar(%#02x)=%v, alphabet says %v", c, got, wantCont)
		}
		if c >= 0x80 && (isIdentStart(c) || isIdentChar(c)) {
			t.Errorf("byte %#02x is non-ASCII and the scanner accepts it in an identifier; "+
				"classifyClauseKeyword's ASCII fold is no longer equivalent", c)
		}
	}
}

// TestClassifyClauseKeywordMatchesToUpper is the differential test: the new
// classifier and the pre-change one must return the same clauseKind for every
// token on all three routes.
func TestClassifyClauseKeywordMatchesToUpper(t *testing.T) {
	check := func(t *testing.T, tok string) {
		t.Helper()
		if got, want := classifyClauseKeyword(tok), classifyClauseKeywordControl(tok); got != want {
			t.Fatalf("classifyClauseKeyword(%q)=%v, strings.ToUpper switch says %v", tok, got, want)
		}
	}

	// Route 1 — exhaustive over the scanner's own alphabet, lengths 1 to 3.
	t.Run("exhaustive_len1to3", func(t *testing.T) {
		checked := 0
		buf := make([]byte, 0, 3)
		for _, a := range []byte(identStartBytes) {
			buf = append(buf[:0], a)
			check(t, string(buf))
			checked++
			for _, b := range []byte(identContBytes) {
				buf = append(buf[:1], b)
				check(t, string(buf))
				checked++
				for _, c := range []byte(identContBytes) {
					buf = append(buf[:2], c)
					check(t, string(buf))
					checked++
				}
			}
		}
		const wantChecked = len(identStartBytes) * (1 + len(identContBytes)*(1+len(identContBytes)))
		if checked != wantChecked {
			t.Fatalf("swept %d identifiers, want %d: the sweep is no longer exhaustive", checked, wantChecked)
		}
	})

	// Route 2 — every case permutation of every keyword.
	t.Run("case_permutations", func(t *testing.T) {
		for _, kw := range clauseKeywordsControl {
			if len(kw) < clauseKeywordMinLen || len(kw) > clauseKeywordMaxLen {
				t.Fatalf("keyword %q lies outside the length gate [%d,%d], which would make it "+
					"unclassifiable", kw, clauseKeywordMinLen, clauseKeywordMaxLen)
			}
			for mask := 0; mask < 1<<len(kw); mask++ {
				p := []byte(kw)
				for i := range p {
					if mask&(1<<i) != 0 {
						p[i] |= asciiCaseBit
					}
				}
				check(t, string(p))
			}
		}
	})

	// Route 3 — the prefix and length boundary: a keyword with one byte
	// appended, and with one byte removed, must not classify as the keyword.
	t.Run("suffixed_and_truncated", func(t *testing.T) {
		for _, kw := range clauseKeywordsControl {
			for _, suf := range []string{"S", "s", "_", "0", "es", "ing"} {
				check(t, kw+suf)
				check(t, strings.ToLower(kw)+suf)
			}
			check(t, kw[:len(kw)-1])
			check(t, strings.ToLower(kw)[:len(kw)-1])
		}
		// The words the campaign named explicitly: a prefix match would
		// reclassify each of these as a clause keyword and change which region
		// hoists.
		for _, w := range []string{"matches", "setting", "ordering", "forall", "sets",
			"onto", "adds", "dropped", "calls", "merged", "withdraw", "wherever"} {
			check(t, w)
			if classifyClauseKeyword(w) != clauseOther {
				t.Fatalf("%q classifies as a clause keyword", w)
			}
		}
	})

	// Every keyword must still be recognised in its canonical spelling, or the
	// three routes above would all agree on a classifier that recognises
	// nothing.
	t.Run("keywords_are_recognised", func(t *testing.T) {
		for _, kw := range clauseKeywordsControl {
			if classifyClauseKeyword(kw) == clauseOther {
				t.Fatalf("%q is not classified as a clause keyword", kw)
			}
		}
		for _, kw := range []string{"MATCH", "WHERE", "match", "Where", "mAtCh"} {
			if got := classifyClauseKeyword(kw); got != clauseHoistOn {
				t.Fatalf("classifyClauseKeyword(%q)=%v, want clauseHoistOn", kw, got)
			}
		}
	})
}

// TestStripLiteralsAllocatesNothingPerIdentifier asserts the property the
// change bought: a statement with no quote byte costs no allocation at all, and
// one that hoists allocates in proportion to what it hoists, not to how many
// identifiers it contains.
//
// The second half is the load-bearing one. Before rmp #2849 the count grew with
// every identifier token, which is what made StripLiterals — a function that
// runs on every execution, cache hit included — the warm path's whole
// allocation cost.
func TestStripLiteralsAllocatesNothingPerIdentifier(t *testing.T) {
	// A quote-free statement: the fast reject, and nothing else.
	quoteFree := stripShapeQuoteFree(32)
	if n := testing.AllocsPerRun(200, func() {
		stripAttrString, stripAttrMap, stripAttrBool = StripLiterals(quoteFree)
	}); n != 0 {
		t.Errorf("quote-free statement allocated %v times, want 0", n)
	}

	// One hoistable literal, with the identifier count varied over a 16x range.
	// The allocation count must not move: it is a function of what is hoisted,
	// not of how long the statement is.
	baseQuery := stripShapeIdents(2)
	base := testing.AllocsPerRun(200, func() {
		stripAttrString, stripAttrMap, stripAttrBool = StripLiterals(baseQuery)
	})
	for _, k := range []int{4, 8, 16, 32} {
		q := stripShapeIdents(k)
		got := testing.AllocsPerRun(200, func() {
			stripAttrString, stripAttrMap, stripAttrBool = StripLiterals(q)
		})
		if got != base {
			t.Errorf("k=%d allocated %v times against %v at k=2: the count still grows with "+
				"the identifier count", k, got, base)
		}
	}
}

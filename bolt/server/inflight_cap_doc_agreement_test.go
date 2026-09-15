package server

// inflight_cap_doc_agreement_test.go — regression gate for rmp #2811, item 1.
//
// Options.MaxInFlightPerConnection's godoc claimed the cap surfaces as
// "Neo.TransientError.Transaction.MaximumTransactionLimitReached — TRANSIENT, so
// a driver retries, and the session stays in READY so it can". All three halves
// were false: the RUN handler emits Neo.ClientError.General.LimitExceeded, a
// ClientError is never retried by neo4j-go-driver's IsRetriableTransient, and
// the same branch calls enterFailed, so the session lands in FAILED.
//
// The documented code is not cosmetic: a driver retries on a TransientError and
// does not on a ClientError, so a caller acting on the godoc writes the wrong
// recovery path.
//
// # Why this gate reads source text
//
// The BEHAVIOUR is already pinned — TestSession_InFlightCursorCap_
// RejectionAbortsDoomedTx asserts both the code and the FAILED state — so a
// behavioural test would have stayed green throughout the defect. What was
// unguarded is the AGREEMENT between the godoc and the code, and the only way to
// fail on a wrong comment is to read it.
//
// The expectation is DERIVED, not written down twice: the code the gate demands
// of the godoc is extracted from session.go's own emission site. Hard-coding the
// literal on both sides would compare a constant with itself and could not fail
// if the emitted code moved.
//
// Layer: short.

import (
	"os"
	"strings"
	"testing"
)

// inFlightCapBranchAnchor is the line that opens the in-flight cursor cap's
// branch in the RUN handler. The Code literal the gate wants is the first one
// after it.
const inFlightCapBranchAnchor = "if n := s.inFlightCount(); n >= s.maxInFlight {"

// emittedInFlightCapCode extracts the failure code session.go actually emits
// when the per-connection in-flight cursor cap is reached.
func emittedInFlightCapCode(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatalf("read session.go: %v", err)
	}
	_, after, ok := strings.Cut(string(src), inFlightCapBranchAnchor)
	if !ok {
		t.Fatalf("session.go: the in-flight cap branch %q is gone; this gate reads "+
			"the emitted code from there and can no longer find it", inFlightCapBranchAnchor)
	}
	_, after, ok = strings.Cut(after, `Code:    "`)
	if !ok {
		t.Fatal("session.go: no Code literal after the in-flight cap branch")
	}
	code, _, ok := strings.Cut(after, `"`)
	if !ok {
		t.Fatal("session.go: unterminated Code literal after the in-flight cap branch")
	}
	return code
}

// maxInFlightGodoc returns the comment block immediately preceding the
// MaxInFlightPerConnection field in serve.go.
func maxInFlightGodoc(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatalf("read serve.go: %v", err)
	}
	before, _, ok := strings.Cut(string(src), "\n\tMaxInFlightPerConnection int\n")
	if !ok {
		t.Fatal("serve.go: the MaxInFlightPerConnection field declaration is gone")
	}
	// Walk back over the contiguous run of comment lines that forms the godoc.
	lines := strings.Split(before, "\n")
	i := len(lines)
	for i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "//") {
		i--
	}
	if i == len(lines) {
		t.Fatal("serve.go: MaxInFlightPerConnection has no godoc at all")
	}
	return strings.Join(lines[i:], "\n")
}

// TestMaxInFlightPerConnectionGodoc_NamesTheCodeTheServerEmits is the gate: the
// field's godoc must name the code session.go emits for that cap, and must not
// name the per-principal quota's transient code, which belongs to a different
// limit (Options.MaxOpenTxPerPrincipal, see txQuotaRefusalCode).
func TestMaxInFlightPerConnectionGodoc_NamesTheCodeTheServerEmits(t *testing.T) {
	t.Parallel()

	emitted := emittedInFlightCapCode(t)
	doc := maxInFlightGodoc(t)

	if !strings.Contains(doc, emitted) {
		t.Errorf("Options.MaxInFlightPerConnection's godoc does not name the code the "+
			"cap actually emits.\n  session.go emits: %q\n  godoc:\n%s", emitted, doc)
	}
	if strings.Contains(doc, txQuotaRefusalCode) {
		t.Errorf("Options.MaxInFlightPerConnection's godoc names %q, which is the "+
			"PER-PRINCIPAL open-transaction quota's code (Options.MaxOpenTxPerPrincipal), "+
			"not this cap's. The two differ in the one way a driver acts on: that one is "+
			"TRANSIENT and retriable, this one is not.\n  godoc:\n%s",
			txQuotaRefusalCode, doc)
	}
	// The emitted code's classification is the second dot-separated part, and it
	// is what neo4j-go-driver's IsRetriableTransient tests. Asserting it here
	// keeps the godoc's retry claim tied to the code rather than to prose.
	if parts := strings.Split(emitted, "."); len(parts) < 2 || parts[1] != "ClientError" {
		t.Errorf("the in-flight cap now emits %q, whose classification is not "+
			"ClientError; the godoc's retry paragraph was written for a "+
			"non-retriable code and must be revisited", emitted)
	}
}

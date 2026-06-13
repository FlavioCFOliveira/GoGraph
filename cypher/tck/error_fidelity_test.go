package tck_test

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

// ─────────────────────────────────────────────────────────────────────────────
// TCK error-type fidelity (#1443)
//
// The TCK result gate (tckExecutionBaseline) proves every scenario produces the
// expected RESULT, including that error scenarios raise AN error in the right
// phase. It does NOT prove the error is of the CORRECT openCypher type: the
// live assertions (assertSyntaxError / assertError) accept any error so the
// 3897/3897 result-pass is preserved while the engine's error-type modelling is
// brought up to TCK fidelity family by family.
//
// This file adds an ADDITIVE fidelity gate. As each error scenario is asserted,
// recordFidelity classifies the engine error and records whether its type
// matches the expected openCypher token. tckFidelityGateCheck then enforces a
// ratchet (tckErrorFidelityBaseline) on the number of EXACT-type matches — it
// only ever rises, driving error-type fidelity up over successive cycles
// without ever regressing the result-pass gate.
//
// Classification is deterministic and conservative: it maps only engine errors
// that already carry an unambiguous discriminator (the sema.ScopeError.Kind
// vocabulary) to their canonical TCK token. Families whose tokens are
// ambiguous to classify from an error alone (the numeric/argument family —
// IntegerOverflow vs NumberOutOfRange vs InvalidArgumentValue, etc.) are
// deliberately left unclassified so the gate never counts a false match. See
// the cypher-expert mapping in the #1443 task notes.
// ─────────────────────────────────────────────────────────────────────────────

// fidelityRecord is one error scenario's fidelity outcome.
type fidelityRecord struct {
	category   string // expected TCK category (e.g. "SyntaxError")
	expected   string // expected TCK type token (e.g. "UndefinedVariable")
	classified string // engine-classified token; "" when not classifiable
	matched    bool   // classified == expected
	diag       string // diagnostic discriminator (Go error shape) for the report
}

var (
	fidelityMu      sync.Mutex
	fidelityRecords []fidelityRecord
)

// resetFidelity clears the collector at the start of an execution run so a
// re-run does not accumulate stale records.
func resetFidelity() {
	fidelityMu.Lock()
	fidelityRecords = nil
	fidelityMu.Unlock()
}

// recordFidelity classifies err against the expected (category, type) and
// appends the outcome. It is called by the error-assertion steps AFTER the live
// assertion has drained any lazy result into w.err, so err is the engine error
// actually raised (or nil if the query unexpectedly succeeded).
func recordFidelity(category, expected string, err error) {
	classified, _ := classifyTCKErrorType(err)
	fidelityMu.Lock()
	fidelityRecords = append(fidelityRecords, fidelityRecord{
		category:   category,
		expected:   expected,
		classified: classified,
		matched:    classified != "" && classified == expected,
		diag:       fidelityDiag(err),
	})
	fidelityMu.Unlock()
}

// scopeKindToTCKType maps the engine's sema.ScopeError.Kind discriminator to
// its canonical openCypher TCK type token. Only unambiguous 1:1 kinds are
// listed (Families 2–4 in the cypher-expert mapping). A kind absent from this
// table is left unclassified rather than guessed.
var scopeKindToTCKType = map[sema.ErrorKind]string{
	sema.KindUndefinedVar:                   "UndefinedVariable",
	sema.KindVariableAlreadyBound:           "VariableAlreadyBound",
	sema.KindColumnNameConflict:             "ColumnNameConflict",
	sema.KindUnknownFunction:                "UnknownFunction",
	sema.KindRelationshipUniqueness:         "RelationshipUniquenessViolation",
	sema.KindNegativeIntegerArgument:        "NegativeIntegerArgument",
	sema.KindInvalidAggregation:             "InvalidAggregation",
	sema.KindAmbiguousAggregationExpression: "AmbiguousAggregationExpression",
	sema.KindNoVariablesInScope:             "NoVariablesInScope",
	sema.KindNoExpressionAlias:              "NoExpressionAlias",

	// DELIBERATELY OMITTED — these engine kinds are ambiguous to map and are
	// left for a later cycle that adds explicit, per-condition error tagging:
	//
	//   - KindInvalidArgumentType is the engine's broad TypeError catch-all:
	//     it fires both for genuine InvalidArgumentType scenarios AND for
	//     several distinct conditions the TCK names separately (CreatingVarLength,
	//     InvalidDelete, NoSingleRelationshipType, NonConstantExpression,
	//     RequiresDirectedRelationship, InvalidParameterUse). Mapping it to
	//     "InvalidArgumentType" would over-count, so it stays out of the
	//     ratcheted baseline (cypher-expert guidance, #1443).
	//   - KindRedeclaration is used for BOTH VariableTypeConflict and
	//     VariableAlreadyBound scenarios, so it cannot be mapped to a single
	//     token without a discriminator.
}

// classifyTCKErrorType returns the openCypher TCK type token the engine error
// corresponds to, and whether it could be classified. It is deterministic and
// conservative: an error it cannot map unambiguously returns ("", false).
func classifyTCKErrorType(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var se *sema.ScopeError
	if errors.As(err, &se) {
		if tok, ok := scopeKindToTCKType[se.Kind]; ok {
			return tok, true
		}
	}
	return "", false
}

// fidelityDiag returns a short discriminator describing the engine error's
// shape, for the report that guides which families to tag next.
func fidelityDiag(err error) string {
	if err == nil {
		return "<no error raised>"
	}
	var se *sema.ScopeError
	if errors.As(err, &se) {
		return "sema.ScopeError:" + string(se.Kind)
	}
	var sm *parser.SemaError
	if errors.As(err, &sm) {
		return "parser.SemaError:" + sm.Rule
	}
	var pe *parser.ParseError
	if errors.As(err, &pe) {
		return "parser.ParseError"
	}
	return fmt.Sprintf("%T", err)
}

// fidelitySummary aggregates the collected records.
type fidelitySummary struct {
	total      int // error scenarios asserted
	classified int // scenarios the classifier returned a token for
	matched    int // scenarios whose classified token == expected token
}

func summariseFidelity() fidelitySummary {
	fidelityMu.Lock()
	defer fidelityMu.Unlock()
	var s fidelitySummary
	for _, r := range fidelityRecords {
		s.total++
		if r.classified != "" {
			s.classified++
		}
		if r.matched {
			s.matched++
		}
	}
	return s
}

// fidelityByExpected returns, per expected token, the matched/total counts and
// the set of diagnostics seen — the evidence used to widen the classifier.
func fidelityByExpected() map[string]struct {
	matched, total int
	diags          map[string]int
} {
	fidelityMu.Lock()
	defer fidelityMu.Unlock()
	out := map[string]struct {
		matched, total int
		diags          map[string]int
	}{}
	for _, r := range fidelityRecords {
		e := out[r.expected]
		if e.diags == nil {
			e.diags = map[string]int{}
		}
		e.total++
		if r.matched {
			e.matched++
		}
		e.diags[r.diag]++
		out[r.expected] = e
	}
	return out
}

// sortedKeys returns the keys of m sorted.
func sortedFidelityKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// tckErrorFidelityBaseline is the minimum number of error scenarios that must
// raise the EXACT expected openCypher error type. It is a RATCHET, parallel to
// tckExecutionBaseline: it only ever rises, as the classifier (and, in later
// cycles, explicit engine error tagging) brings more families up to type
// fidelity. Lowering it is forbidden — a drop means a previously type-faithful
// error scenario regressed to the wrong type.
//
// Measured 2026-06-13 against the deterministic sema.ScopeError.Kind classifier
// over the full TCK error-scenario population (695 error-step assertions, of
// which Scenario-Outline expansion accounts for the multiplicity). The 121
// matches break down as: UndefinedVariable 69, InvalidAggregation 29,
// VariableAlreadyBound 9, AmbiguousAggregationExpression 5, NegativeInteger-
// Argument 4, and ColumnNameConflict / NoExpressionAlias / NoVariablesInScope /
// RelationshipUniquenessViolation / UnknownFunction 1 each. The ambiguous
// InvalidArgumentType (158 same-token raises) and the numeric/argument family
// are intentionally excluded (see classifyTCKErrorType) and await explicit
// engine error tagging in a later cycle, which will ratchet this upward.
const tckErrorFidelityBaseline = 121

// TestTCKFidelityGateCheck_DetectsRegression is the negative test for the
// fidelity gate (mirrors TestTCKGateCheck_DetectsFailedScenarios): it proves
// tckFidelityGateCheck fires Errorf when the matched count drops below the
// baseline, so a real fidelity regression cannot pass silently.
func TestTCKFidelityGateCheck_DetectsRegression(t *testing.T) {
	t.Parallel()

	// One fewer match than the baseline must trip the gate.
	probe := &errProbe{}
	tckFidelityGateCheck(probe, fidelitySummary{total: 695, classified: 120, matched: tckErrorFidelityBaseline - 1})
	if !probe.errored {
		t.Errorf("tckFidelityGateCheck did not fire Errorf for matched=%d < baseline=%d",
			tckErrorFidelityBaseline-1, tckErrorFidelityBaseline)
	}

	// Meeting the baseline exactly must NOT trip the gate.
	probe = &errProbe{}
	tckFidelityGateCheck(probe, fidelitySummary{total: 695, classified: 302, matched: tckErrorFidelityBaseline})
	if probe.errored {
		t.Errorf("tckFidelityGateCheck fired Errorf for matched=%d == baseline=%d (must not)",
			tckErrorFidelityBaseline, tckErrorFidelityBaseline)
	}
}

// TestClassifyTCKErrorType_KnownKinds is a fast unit test of the classifier: it
// confirms each mapped sema kind classifies to its canonical TCK token, and
// that an unmapped/ambiguous kind and a nil error stay unclassified.
func TestClassifyTCKErrorType_KnownKinds(t *testing.T) {
	t.Parallel()
	for kind, want := range scopeKindToTCKType {
		got, ok := classifyTCKErrorType(&sema.ScopeError{Kind: kind})
		if !ok || got != want {
			t.Errorf("classifyTCKErrorType(Kind=%s) = (%q,%v), want (%q,true)", kind, got, ok, want)
		}
		if _, known := canonicalTCKErrorTypes[want]; !known {
			t.Errorf("classifier maps %s to %q which is not in the canonical TCK vocabulary", kind, want)
		}
	}
	// KindRedeclaration is ambiguous and must stay unclassified.
	if got, ok := classifyTCKErrorType(&sema.ScopeError{Kind: sema.KindRedeclaration}); ok {
		t.Errorf("classifyTCKErrorType(REDECLARATION) = (%q,true), want unclassified", got)
	}
	if _, ok := classifyTCKErrorType(nil); ok {
		t.Error("classifyTCKErrorType(nil) reported classified; want false")
	}
}

// logFidelityBreakdown logs, per expected token, the matched/total counts and
// the engine-error diagnostics seen. It is a developer aid (verbose only) for
// deciding which family to bring up to fidelity next.
func logFidelityBreakdown(t tckFidelityGateError) {
	t.Helper()
	by := fidelityByExpected()
	for _, tok := range sortedFidelityKeys(by) {
		e := by[tok]
		t.Logf("  %-34s matched %d/%d  diags=%v", tok, e.matched, e.total, e.diags)
	}
}

// tckFidelityGateError is the minimal interface tckFidelityGateCheck needs, so
// the gate logic is unit-testable independently of *testing.T.
type tckFidelityGateError interface {
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
	Helper()
}

// tckFidelityGateCheck enforces the error-type fidelity ratchet.
func tckFidelityGateCheck(t tckFidelityGateError, s fidelitySummary) {
	t.Helper()
	t.Logf("TCK error-type fidelity: %d/%d error scenarios raised the exact expected type "+
		"(%d classified; baseline=%d)", s.matched, s.total, s.classified, tckErrorFidelityBaseline)
	if s.matched < tckErrorFidelityBaseline {
		t.Errorf("TCK error-type fidelity regression: %d scenarios matched the expected error "+
			"type, baseline=%d. An error scenario stopped raising its expected openCypher type. "+
			"Do NOT lower the baseline — restore the error type.", s.matched, tckErrorFidelityBaseline)
	}
}

// canonicalTCKErrorTypes is the vocabulary of openCypher TCK error-type tokens,
// confirmed against the upstream TCK constants (TCKErrorDetails.scala) by the
// cypher-expert review for #1443. Every error-type token an error scenario can
// expect must be in this set; tckVocabularyGateCheck fails on any unknown
// token, so a newly vendored TCK that introduces (or renames) a token is caught
// rather than silently passing through the generic accept-any handler.
var canonicalTCKErrorTypes = map[string]struct{}{
	"InvalidArgumentType": {}, "VariableAlreadyBound": {}, "UndefinedVariable": {},
	"UnexpectedSyntax": {}, "VariableTypeConflict": {}, "NegativeIntegerArgument": {},
	"InvalidArgumentValue": {}, "IntegerOverflow": {}, "AmbiguousAggregationExpression": {},
	"NoSingleRelationshipType": {}, "InvalidNumberLiteral": {}, "InvalidAggregation": {},
	"NumberOutOfRange": {}, "NonConstantExpression": {}, "InvalidParameterUse": {},
	"InvalidNumberOfArguments": {}, "InvalidClauseComposition": {}, "DeletedEntityAccess": {},
	"RequiresDirectedRelationship": {}, "ProcedureNotFound": {}, "MergeReadOwnWrites": {},
	"MapElementAccessByNonString": {}, "InvalidRelationshipPattern": {}, "InvalidDelete": {},
	"DifferentColumnsInUnion": {}, "CreatingVarLength": {}, "ColumnNameConflict": {},
	"UnknownFunction": {}, "RelationshipUniquenessViolation": {}, "NoVariablesInScope": {},
	"NoExpressionAlias": {}, "NestedAggregation": {}, "MissingParameter": {},
	"InvalidUnicodeLiteral": {}, "InvalidUnicodeCharacter": {}, "InvalidPropertyType": {},
	"InvalidArgumentPassingMode": {}, "FloatingPointOverflow": {}, "DeleteConnectedNode": {},
	// "*" is the TCK wildcard detail meaning "any error type": the scenario
	// asserts only the category, not a specific type. It is never classified
	// for fidelity (no engine kind maps to it) but is a valid vocabulary token.
	"*": {},
}

// tckVocabularyGateCheck fails when an error scenario expects a token not in the
// canonical TCK vocabulary — a sign the vendored TCK changed and the harness
// (and classifier) must be updated.
func tckVocabularyGateCheck(t tckFidelityGateError) {
	t.Helper()
	fidelityMu.Lock()
	seen := map[string]struct{}{}
	for _, r := range fidelityRecords {
		seen[r.expected] = struct{}{}
	}
	fidelityMu.Unlock()
	for _, tok := range sortedFidelityKeys(seen) {
		if _, ok := canonicalTCKErrorTypes[tok]; !ok {
			t.Errorf("TCK error scenario expects unknown error-type token %q — the vendored TCK "+
				"vocabulary changed; update canonicalTCKErrorTypes and the classifier", tok)
		}
	}
}

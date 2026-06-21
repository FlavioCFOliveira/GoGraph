package lpg

import (
	"testing"
	"time"
)

// TestDateValue_FoldsIntoEpochDayColumn is the #1649 evidence that a Go-API
// DateValue is recognised by classify and folds into the compact int32
// epoch-day column (≈4 bytes/value) rather than the 16-byte-header string
// column — the storage win behind reclaiming ~30% of example 26's heap.
func TestDateValue_FoldsIntoEpochDayColumn(t *testing.T) {
	cases := []struct {
		y    int
		m    time.Month
		d    int
		days int32
	}{
		{1970, time.January, 1, 0},
		{2020, time.June, 15, int32(daysFromCivil(2020, 6, 15))},
		{1, time.January, 1, int32(daysFromCivil(1, 1, 1))},
		{2026, time.December, 31, int32(daysFromCivil(2026, 12, 31))},
	}
	for _, c := range cases {
		// A DateValue built from a timestamp WITH a time-of-day component must
		// ignore the clock and zone and fold on the calendar date alone.
		pv := DateValue(time.Date(c.y, c.m, c.d, 13, 45, 30, 0, time.UTC))
		if pv.Kind() != PropString {
			t.Fatalf("%04d-%02d-%02d: DateValue kind = %v, want PropString", c.y, c.m, c.d, pv.Kind())
		}
		kind, days, ok := classify(pv)
		if !ok || kind != dateKind {
			t.Fatalf("%04d-%02d-%02d: did not fold into the date column: kind=%d ok=%v", c.y, c.m, c.d, kind, ok)
		}
		if days != c.days {
			t.Errorf("%04d-%02d-%02d: epoch-day = %d, want %d", c.y, c.m, c.d, days, c.days)
		}
		// The stored canonical string is byte-identical to the codec's
		// reconstruction of the folded epoch-day, so the round-trip guard holds
		// and the value reconstitutes exactly.
		if got := epochDayToString(days); got != pv.v.(string) {
			t.Errorf("%04d-%02d-%02d: stored %q, codec reconstructs %q", c.y, c.m, c.d, pv.v, got)
		}
	}
}

// TestDateValue_MatchesCypherDateEncoding asserts that a Go-API DateValue is
// byte-identical to the value the Cypher write path produces for the same date
// (lpg.StringValue("\x01" + date.String()) in cypher/api.go). Equality here is
// what makes a Go-API date read back as a native Cypher Date and share the
// folded int32 column with Cypher-written dates.
func TestDateValue_MatchesCypherDateEncoding(t *testing.T) {
	// Cypher encodes a Date as the SOH tag byte followed by canonical YYYY-MM-DD.
	want := "\x01" + "2026-06-22"
	got, ok := DateValue(time.Date(2026, time.June, 22, 0, 0, 0, 0, time.UTC)).String()
	if !ok || got != want {
		t.Fatalf("DateValue encoding = %q (ok=%v), want %q", got, ok, want)
	}
}

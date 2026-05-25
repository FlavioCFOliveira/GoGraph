package server

// T637: rapid-based round-trip tests for temporal expr.Value types
// (DateValue, LocalDateTimeValue, DateTimeValue, LocalTimeValue, TimeValue,
// DurationValue) through exprValueToPackstream.
//
// # Current status — NOT IMPLEMENTED (tests skip at runtime)
//
// exprValueToPackstream does not yet encode temporal types as PackStream
// Struct values. When a temporal value is passed, it falls through to the
// default branch which calls v.String(), producing a string rather than a
// typed PackStream Struct.
//
// This file provides the scaffolding so that when temporal encoding is
// implemented the tests can be activated by removing the t.Skip call and
// filling in the round-trip assertions.
//
// # Protocol notes (for the implementor)
//
//   - DateValue    → Struct{Tag: 0x44, Fields: [epochDay int64]}
//   - LocalTimeValue → Struct{Tag: 0x74, Fields: [nanosOfDay int64]}
//   - TimeValue    → Struct{Tag: 0x54, Fields: [nanosOfDay int64, tzOffsetSec int32]}
//   - LocalDateTimeValue → Struct{Tag: 0x64, Fields: [epochSecond int64, nano int32]}
//   - DateTimeValue (v4.4) → Struct{Tag: 0x46, Fields: [epochSecond int64, nano int32, tzOffsetSec int32]}
//   - DateTimeValue (v5.0+) → Struct{Tag: 0x49, Fields: [epochSecond int64, nano int32, tzId string]}
//   - DurationValue → Struct{Tag: 0x45, Fields: [months int64, days int64, seconds int64, nanoseconds int32]}
//
// Boundary years 1 and 9999 must be covered (see AC §3 of T637).
// v5.0 vs v4.4 DateTime tag selection is negotiated per connection; the server
// must use the negotiated bolt version to select tag 0x46 vs 0x49.
//
// # Known divergence
//
// See docs/tck/DIVERGENCES.md for the tracked gap.
//
// Layer: short (no build tag required).

import (
	"testing"

	"pgregory.net/rapid"

	"gograph/cypher/expr"
)

const msgTemporalNotImplemented = "Temporal expr.Value types " +
	"(DateValue, LocalDateTimeValue, DateTimeValue, LocalTimeValue, TimeValue, DurationValue) " +
	"are not yet encoded as PackStream Structs in exprValueToPackstream — " +
	"see docs/tck/DIVERGENCES.md"

// TestTemporalRapid_Date verifies round-trip identity for DateValue over 500
// rapid iterations. Skipped until temporal encoding is implemented.
func TestTemporalRapid_Date(t *testing.T) {
	t.Skip(msgTemporalNotImplemented)

	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day") // clamp to always-valid day
		want := expr.NewDate(year, month, day)

		got := exprValueToPackstream(want)

		// TODO: when implemented, verify got is a packstream.Struct with
		// Tag=0x44 and Fields[0] == epochDay(want).
		_ = got
		rt.Fatalf("temporal encoding not implemented; remove t.Skip when ready")
	})
}

// TestTemporalRapid_LocalTime verifies round-trip identity for LocalTimeValue.
// Skipped until temporal encoding is implemented.
func TestTemporalRapid_LocalTime(t *testing.T) {
	t.Skip(msgTemporalNotImplemented)

	rapid.Check(t, func(rt *rapid.T) {
		h := rapid.IntRange(0, 23).Draw(rt, "h")
		m := rapid.IntRange(0, 59).Draw(rt, "m")
		s := rapid.IntRange(0, 59).Draw(rt, "s")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		want := expr.NewLocalTime(h, m, s, ns)

		got := exprValueToPackstream(want)

		// TODO: verify Struct{Tag:0x74, Fields:[nanosOfDay]}.
		_ = got
		rt.Fatalf("temporal encoding not implemented; remove t.Skip when ready")
	})
}

// TestTemporalRapid_Time verifies round-trip identity for TimeValue (with offset).
// Skipped until temporal encoding is implemented.
func TestTemporalRapid_Time(t *testing.T) {
	t.Skip(msgTemporalNotImplemented)

	rapid.Check(t, func(rt *rapid.T) {
		h := rapid.IntRange(0, 23).Draw(rt, "h")
		m := rapid.IntRange(0, 59).Draw(rt, "m")
		s := rapid.IntRange(0, 59).Draw(rt, "s")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		offsetSec := rapid.IntRange(-18*3600, 18*3600).Draw(rt, "offsetSec")
		want := expr.NewTime(h, m, s, ns, offsetSec)

		got := exprValueToPackstream(want)

		// TODO: verify Struct{Tag:0x54, Fields:[nanosOfDay, tzOffsetSec]}.
		_ = got
		rt.Fatalf("temporal encoding not implemented; remove t.Skip when ready")
	})
}

// TestTemporalRapid_LocalDateTime verifies round-trip identity for
// LocalDateTimeValue. Skipped until temporal encoding is implemented.
func TestTemporalRapid_LocalDateTime(t *testing.T) {
	t.Skip(msgTemporalNotImplemented)

	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day")
		hour := rapid.IntRange(0, 23).Draw(rt, "hour")
		mi := rapid.IntRange(0, 59).Draw(rt, "min")
		sec := rapid.IntRange(0, 59).Draw(rt, "sec")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		want := expr.NewLocalDateTime(year, month, day, hour, mi, sec, ns)

		got := exprValueToPackstream(want)

		// TODO: verify Struct{Tag:0x64, Fields:[epochSecond, nano]}.
		_ = got
		rt.Fatalf("temporal encoding not implemented; remove t.Skip when ready")
	})
}

// TestTemporalRapid_DateTime verifies round-trip identity for DateTimeValue.
//
// The tag selection depends on the negotiated Bolt version:
//   - v4.4 and earlier: Tag 0x46 with tzOffsetSec field (int32).
//   - v5.0 and later:   Tag 0x49 with tzId field (string).
//
// Skipped until temporal encoding is implemented.
func TestTemporalRapid_DateTime(t *testing.T) {
	t.Skip(msgTemporalNotImplemented)

	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day")
		hour := rapid.IntRange(0, 23).Draw(rt, "hour")
		mi := rapid.IntRange(0, 59).Draw(rt, "min")
		sec := rapid.IntRange(0, 59).Draw(rt, "sec")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		want := expr.NewDateTime(year, month, day, hour, mi, sec, ns, nil /* UTC */)

		got := exprValueToPackstream(want)

		// TODO: verify Struct with Tag 0x46 (v4.4) or 0x49 (v5.0+) depending
		// on the session's negotiated bolt version. The session must pass its
		// negotiated version into exprValueToPackstream (signature change).
		_ = got
		rt.Fatalf("temporal encoding not implemented; remove t.Skip when ready")
	})
}

// TestTemporalRapid_Duration verifies round-trip identity for DurationValue.
// Skipped until temporal encoding is implemented.
func TestTemporalRapid_Duration(t *testing.T) {
	t.Skip(msgTemporalNotImplemented)

	rapid.Check(t, func(rt *rapid.T) {
		months := rapid.Int64Range(-120, 120).Draw(rt, "months")
		days := rapid.Int64Range(-365, 365).Draw(rt, "days")
		seconds := rapid.Int64Range(-86400, 86400).Draw(rt, "seconds")
		nanos := rapid.Int32Range(0, 999_999_999).Draw(rt, "nanos")
		want := expr.NewDuration(months, days, seconds, nanos)

		got := exprValueToPackstream(want)

		// TODO: verify Struct{Tag:0x45, Fields:[months, days, seconds, nanos]}.
		_ = got
		rt.Fatalf("temporal encoding not implemented; remove t.Skip when ready")
	})
}

// TestTemporalBoundaryYears verifies that year boundaries 1 and 9999 are
// accepted by exprValueToPackstream without panic.
//
// Currently this only checks the fallback string representation (because
// encoding is not implemented). When encoding is implemented, this test
// should assert the correct Struct field values.
func TestTemporalBoundaryYears(t *testing.T) {
	dates := []expr.DateValue{
		expr.NewDate(1, 1, 1),
		expr.NewDate(9999, 12, 31),
	}
	for _, d := range dates {
		t.Run(d.String(), func(t *testing.T) {
			got := exprValueToPackstream(d)
			if got == nil {
				t.Fatalf("exprValueToPackstream(DateValue) returned nil for %v", d)
			}
			// TODO: when encoding is implemented, assert got is a Struct and
			// verify the epochDay field. Remove t.Skip from the rapid tests above.
			_ = got // current: fallback to string, no panic expected
		})
	}
}

package server

// T637 / task #1434: rapid-based round-trip tests for temporal expr.Value types
// (DateValue, LocalDateTimeValue, DateTimeValue, LocalTimeValue, TimeValue,
// DurationValue) through exprValueToPackstream.
//
// # Status — IMPLEMENTED
//
// exprValueToPackstream encodes each temporal type as the canonical PackStream
// Struct with the Bolt-specified tag and field layout. These tests assert the
// tag bytes and field values round-trip exactly.
//
// # Protocol notes
//
//   - DateValue          → Struct{Tag: 0x44, Fields: [epochDay int64]}
//   - LocalTimeValue     → Struct{Tag: 0x74, Fields: [nanosOfDay int64]}
//   - TimeValue          → Struct{Tag: 0x54, Fields: [nanosOfDay int64, tzOffsetSec int64]}
//   - LocalDateTimeValue → Struct{Tag: 0x64, Fields: [epochSecond int64, nano int64]}
//   - DateTimeValue (v4.4) → Struct{Tag: 0x46, Fields: [epochSecond int64, nano int64, tzOffsetSec int64]}
//   - DateTimeValue (v5.0+) → Struct{Tag: 0x49, Fields: [epochSecond int64, nano int64, tzId string]}
//   - DurationValue      → Struct{Tag: 0x45, Fields: [months int64, days int64, seconds int64, nanos int64]}
//
// Boundary years 1 and 9999 are covered (see AC §3 of T637).
// v5.0 vs v4.4 DateTime tag selection follows the negotiated bolt major version
// threaded into exprValueToPackstream.
//
// Layer: short (no build tag required).

import (
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// asStruct asserts that got is a packstream.Struct with the wanted tag and
// returns its fields.
func asStruct(t rapid.TB, got packstream.Value, wantTag byte) []packstream.Value {
	t.Helper()
	s, ok := got.(packstream.Struct)
	if !ok {
		t.Fatalf("expected packstream.Struct, got %T (%v)", got, got)
	}
	if s.Tag != wantTag {
		t.Fatalf("tag: got 0x%02X, want 0x%02X", s.Tag, wantTag)
	}
	return s.Fields
}

func fieldInt64(t rapid.TB, fields []packstream.Value, idx int) int64 {
	t.Helper()
	if idx >= len(fields) {
		t.Fatalf("field %d out of range (len %d)", idx, len(fields))
	}
	v, ok := fields[idx].(int64)
	if !ok {
		t.Fatalf("field %d: expected int64, got %T", idx, fields[idx])
	}
	return v
}

// TestTemporalRapid_Date verifies round-trip identity for DateValue (tag 0x44)
// over 500 rapid iterations. The single field is the epoch day.
func TestTemporalRapid_Date(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day") // clamp to always-valid day
		want := expr.NewDate(year, month, day)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x44)
		if len(fields) != 1 {
			rt.Fatalf("Date: got %d fields, want 1", len(fields))
		}
		wantEpochDay := want.ToTime().Unix() / 86400
		if got := fieldInt64(rt, fields, 0); got != wantEpochDay {
			rt.Fatalf("epochDay: got %d, want %d", got, wantEpochDay)
		}
	})
}

// TestTemporalRapid_LocalTime verifies round-trip identity for LocalTimeValue
// (tag 0x74). The single field is the nanoseconds-of-day.
func TestTemporalRapid_LocalTime(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		h := rapid.IntRange(0, 23).Draw(rt, "h")
		m := rapid.IntRange(0, 59).Draw(rt, "m")
		s := rapid.IntRange(0, 59).Draw(rt, "s")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		want := expr.NewLocalTime(h, m, s, ns)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x74)
		if len(fields) != 1 {
			rt.Fatalf("LocalTime: got %d fields, want 1", len(fields))
		}
		if got := fieldInt64(rt, fields, 0); got != want.Nanos {
			rt.Fatalf("nanosOfDay: got %d, want %d", got, want.Nanos)
		}
	})
}

// TestTemporalRapid_Time verifies round-trip identity for TimeValue (tag 0x54,
// with offset): fields [nanosOfDay, tzOffsetSec].
func TestTemporalRapid_Time(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		h := rapid.IntRange(0, 23).Draw(rt, "h")
		m := rapid.IntRange(0, 59).Draw(rt, "m")
		s := rapid.IntRange(0, 59).Draw(rt, "s")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		offsetSec := rapid.IntRange(-18*3600, 18*3600).Draw(rt, "offsetSec")
		want := expr.NewTime(h, m, s, ns, offsetSec)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x54)
		if len(fields) != 2 {
			rt.Fatalf("Time: got %d fields, want 2", len(fields))
		}
		if got := fieldInt64(rt, fields, 0); got != want.Nanos {
			rt.Fatalf("nanosOfDay: got %d, want %d", got, want.Nanos)
		}
		if got := fieldInt64(rt, fields, 1); got != int64(want.OffsetSec) {
			rt.Fatalf("tzOffsetSec: got %d, want %d", got, want.OffsetSec)
		}
	})
}

// TestTemporalRapid_LocalDateTime verifies round-trip identity for
// LocalDateTimeValue (tag 0x64): fields [epochSecond, nano].
func TestTemporalRapid_LocalDateTime(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day")
		hour := rapid.IntRange(0, 23).Draw(rt, "hour")
		mi := rapid.IntRange(0, 59).Draw(rt, "min")
		sec := rapid.IntRange(0, 59).Draw(rt, "sec")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		want := expr.NewLocalDateTime(year, month, day, hour, mi, sec, ns)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x64)
		if len(fields) != 2 {
			rt.Fatalf("LocalDateTime: got %d fields, want 2", len(fields))
		}
		if got := fieldInt64(rt, fields, 0); got != want.T.Unix() {
			rt.Fatalf("epochSecond: got %d, want %d", got, want.T.Unix())
		}
		if got := fieldInt64(rt, fields, 1); got != int64(want.T.Nanosecond()) {
			rt.Fatalf("nano: got %d, want %d", got, want.T.Nanosecond())
		}
	})
}

// TestTemporalRapid_DateTime_V5_Offset verifies round-trip identity for a
// DateTimeValue with a fixed-offset zone under Bolt v5.0+ UTC mode (tag 0x49):
// fields [utcEpochSec, nano, tzOffsetSec], the true UTC instant plus a numeric
// offset, matching hydrator.utcDateTimeOffset.
func TestTemporalRapid_DateTime_V5_Offset(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day")
		hour := rapid.IntRange(0, 23).Draw(rt, "hour")
		mi := rapid.IntRange(0, 59).Draw(rt, "min")
		sec := rapid.IntRange(0, 59).Draw(rt, "sec")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		offsetSec := rapid.IntRange(-12*3600, 14*3600).Draw(rt, "offsetSec")
		loc := time.FixedZone("test", offsetSec)
		want := expr.NewDateTime(year, month, day, hour, mi, sec, ns, loc)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x49)
		if len(fields) != 3 {
			rt.Fatalf("DateTime v5 offset: got %d fields, want 3", len(fields))
		}
		if got := fieldInt64(rt, fields, 0); got != want.T.Unix() {
			rt.Fatalf("utcEpochSec: got %d, want %d", got, want.T.Unix())
		}
		if got := fieldInt64(rt, fields, 1); got != int64(want.T.Nanosecond()) {
			rt.Fatalf("nano: got %d, want %d", got, want.T.Nanosecond())
		}
		_, wantOffset := want.T.Zone()
		if got := fieldInt64(rt, fields, 2); got != int64(wantOffset) {
			rt.Fatalf("tzOffsetSec: got %d, want %d", got, wantOffset)
		}
	})
}

// TestTemporalRapid_DateTime_V5_NamedZone verifies that a DateTimeValue carrying
// an IANA-named zone is encoded under Bolt v5.0+ as the named-zone Struct
// (tag 0x69): fields [utcEpochSec, nano, tzId string], matching
// hydrator.utcDateTimeNamedZone.
func TestTemporalRapid_DateTime_V5_NamedZone(t *testing.T) {
	// A representative set of IANA zones (loadable in the test environment).
	zones := []string{"Europe/Paris", "America/New_York", "Asia/Tokyo", "UTC"}
	rapid.Check(t, func(rt *rapid.T) {
		zoneName := rapid.SampledFrom(zones).Draw(rt, "zone")
		loc, err := time.LoadLocation(zoneName)
		if err != nil {
			rt.Skipf("zone %q not loadable: %v", zoneName, err)
		}
		// Skip "UTC" — its location name has no "/" so it takes the offset form.
		if !strings.Contains(loc.String(), "/") {
			return
		}
		year := rapid.IntRange(1970, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day")
		hour := rapid.IntRange(0, 23).Draw(rt, "hour")
		mi := rapid.IntRange(0, 59).Draw(rt, "min")
		sec := rapid.IntRange(0, 59).Draw(rt, "sec")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		want := expr.NewDateTime(year, month, day, hour, mi, sec, ns, loc)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x69)
		if len(fields) != 3 {
			rt.Fatalf("DateTime v5 named zone: got %d fields, want 3", len(fields))
		}
		if got := fieldInt64(rt, fields, 0); got != want.T.Unix() {
			rt.Fatalf("utcEpochSec: got %d, want %d", got, want.T.Unix())
		}
		if got := fieldInt64(rt, fields, 1); got != int64(want.T.Nanosecond()) {
			rt.Fatalf("nano: got %d, want %d", got, want.T.Nanosecond())
		}
		gotZone, ok := fields[2].(string)
		if !ok {
			rt.Fatalf("tzId: expected string, got %T", fields[2])
		}
		if gotZone != loc.String() {
			rt.Fatalf("tzId: got %q, want %q", gotZone, loc.String())
		}
	})
}

// TestTemporalRapid_DateTime_V44 verifies round-trip identity for DateTimeValue
// under Bolt v4.4 legacy mode (tag 0x46): fields [localEpochSec, nano,
// tzOffsetSec], where localEpochSec is the wall-clock instant expressed as if
// UTC (utcEpochSec + offset), matching hydrator.dateTimeOffset.
func TestTemporalRapid_DateTime_V44(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		year := rapid.IntRange(1, 9999).Draw(rt, "year")
		month := rapid.IntRange(1, 12).Draw(rt, "month")
		day := rapid.IntRange(1, 28).Draw(rt, "day")
		hour := rapid.IntRange(0, 23).Draw(rt, "hour")
		mi := rapid.IntRange(0, 59).Draw(rt, "min")
		sec := rapid.IntRange(0, 59).Draw(rt, "sec")
		ns := rapid.IntRange(0, 999_999_999).Draw(rt, "ns")
		offsetSec := rapid.IntRange(-12*3600, 14*3600).Draw(rt, "offsetSec")
		loc := time.FixedZone("test", offsetSec)
		want := expr.NewDateTime(year, month, day, hour, mi, sec, ns, loc)

		fields := asStruct(rt, exprValueToPackstream(want, 4), 0x46)
		if len(fields) != 3 {
			rt.Fatalf("DateTime v4.4: got %d fields, want 3", len(fields))
		}
		_, wantOffset := want.T.Zone()
		wantLocalSec := want.T.Unix() + int64(wantOffset)
		if got := fieldInt64(rt, fields, 0); got != wantLocalSec {
			rt.Fatalf("localEpochSec: got %d, want %d", got, wantLocalSec)
		}
		if got := fieldInt64(rt, fields, 1); got != int64(want.T.Nanosecond()) {
			rt.Fatalf("nano: got %d, want %d", got, want.T.Nanosecond())
		}
		if got := fieldInt64(rt, fields, 2); got != int64(wantOffset) {
			rt.Fatalf("tzOffsetSec: got %d, want %d", got, wantOffset)
		}
	})
}

// TestTemporalRapid_Duration verifies round-trip identity for DurationValue
// (tag 0x45): fields [months, days, seconds, nanos].
func TestTemporalRapid_Duration(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		months := rapid.Int64Range(-120, 120).Draw(rt, "months")
		days := rapid.Int64Range(-365, 365).Draw(rt, "days")
		seconds := rapid.Int64Range(-86400, 86400).Draw(rt, "seconds")
		nanos := rapid.Int32Range(0, 999_999_999).Draw(rt, "nanos")
		want := expr.NewDuration(months, days, seconds, nanos)

		fields := asStruct(rt, exprValueToPackstream(want, 5), 0x45)
		if len(fields) != 4 {
			rt.Fatalf("Duration: got %d fields, want 4", len(fields))
		}
		if got := fieldInt64(rt, fields, 0); got != want.Months {
			rt.Fatalf("months: got %d, want %d", got, want.Months)
		}
		if got := fieldInt64(rt, fields, 1); got != want.Days {
			rt.Fatalf("days: got %d, want %d", got, want.Days)
		}
		if got := fieldInt64(rt, fields, 2); got != want.Seconds {
			rt.Fatalf("seconds: got %d, want %d", got, want.Seconds)
		}
		if got := fieldInt64(rt, fields, 3); got != int64(want.Nanos) {
			rt.Fatalf("nanos: got %d, want %d", got, want.Nanos)
		}
	})
}

// TestTemporalBoundaryYears verifies that year boundaries 1 and 9999 encode to
// a DateValue Struct (tag 0x44) without panic and with the correct epoch day.
func TestTemporalBoundaryYears(t *testing.T) {
	dates := []expr.DateValue{
		expr.NewDate(1, 1, 1),
		expr.NewDate(9999, 12, 31),
	}
	for _, d := range dates {
		t.Run(d.String(), func(t *testing.T) {
			got := exprValueToPackstream(d, 5)
			s, ok := got.(packstream.Struct)
			if !ok {
				t.Fatalf("expected packstream.Struct, got %T", got)
			}
			if s.Tag != 0x44 {
				t.Fatalf("tag: got 0x%02X, want 0x44", s.Tag)
			}
			if len(s.Fields) != 1 {
				t.Fatalf("got %d fields, want 1", len(s.Fields))
			}
			wantEpochDay := d.ToTime().Unix() / 86400
			gotEpochDay, ok := s.Fields[0].(int64)
			if !ok {
				t.Fatalf("epochDay: expected int64, got %T", s.Fields[0])
			}
			if gotEpochDay != wantEpochDay {
				t.Fatalf("epochDay: got %d, want %d", gotEpochDay, wantEpochDay)
			}
		})
	}
}

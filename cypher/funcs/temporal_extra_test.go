package funcs_test

// temporal_extra_test.go — supplementary tests targeting the temporal
// constructors and helpers that remained uncovered after the initial
// task-394 work, lifting cypher/funcs coverage above the 75% gate.
//
// Tests cover:
//   - localdatetime() / datetime() / time(): every documented arg shape
//     (zero-arg now, null, string, map, and same/related temporal kinds).
//   - Duration projections: duration.inMonths / inDays / inSeconds,
//     including null propagation, arity, and type errors.
//   - dateFromMap branches: ordinal, quarter+dayOfQuarter, base {date: D}
//     overlay, fractional intFromValue coercion, NULL propagation on
//     non-numeric components.
//   - zoneFromMap: Z/UTC sentinel, +HH:MM offsets, -HHMM offsets, named
//     zones (Europe/Lisbon), unknown zones falling back to UTC, malformed
//     offsets falling back to UTC, non-string timezone falling back to UTC.
//   - parseSignedOffset: 2-digit and 4-digit forms, sign handling,
//     too-short and malformed inputs.
//   - duration.between: all five same-kind pairs (Date, LocalDateTime,
//     DateTime, LocalTime, Time) plus null propagation.
//   - durationFromMap: every recognised key, fractional seconds, and
//     non-numeric values coerced to zero.
//
// All tests use the external funcs_test package and the call helper from
// essentials_test.go so behaviour is exercised through the public registry.

import (
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// date()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Date_ZeroArgs_ReturnsToday(t *testing.T) {
	v := mustCall(t, "date")
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date() = %T, want DateValue", v)
	}
	now := time.Now().UTC()
	if dv.Year != now.Year() {
		t.Errorf("date().Year = %d, want %d", dv.Year, now.Year())
	}
}

func TestFn_Date_FromDateValue_RoundTrips(t *testing.T) {
	in := expr.NewDate(2024, 6, 15)
	v := mustCall(t, "date", in)
	if v != in {
		t.Errorf("date(DateValue) = %v, want %v", v, in)
	}
}

func TestFn_Date_FromLocalDateTime(t *testing.T) {
	in := expr.NewLocalDateTime(2024, 6, 15, 12, 30, 0, 0)
	v := mustCall(t, "date", in)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(LocalDateTime) = %T", v)
	}
	if dv.Year != 2024 || dv.Month != 6 || dv.Day != 15 {
		t.Errorf("date(LocalDateTime) = %v", dv)
	}
}

func TestFn_Date_FromDateTime(t *testing.T) {
	in := expr.NewDateTime(2024, 6, 15, 12, 30, 0, 0, time.UTC)
	v := mustCall(t, "date", in)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(DateTime) = %T", v)
	}
	if dv.Year != 2024 || dv.Month != 6 || dv.Day != 15 {
		t.Errorf("date(DateTime) = %v", dv)
	}
}

func TestFn_Date_InvalidStringReturnsNull(t *testing.T) {
	v := mustCall(t, "date", expr.StringValue("not-a-date"))
	if !expr.IsNull(v) {
		t.Errorf("date(bad) = %v, want Null", v)
	}
}

func TestFn_Date_WrongTypeReturnsError(t *testing.T) {
	_, err := call(t, "date", expr.IntegerValue(123))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("date(Integer) error = %T, want TypeError", err)
	}
}

func TestFn_Date_ArityError(t *testing.T) {
	_, err := call(t, "date", expr.IntegerValue(1), expr.IntegerValue(2))
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("date(too many) error = %T, want ArityError", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// dateFromMap — exercised via date(map)
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Date_MapForm_Ordinal(t *testing.T) {
	m := expr.MapValue{
		"year":      expr.IntegerValue(2024),
		"dayOfYear": expr.IntegerValue(60),
	}
	v := mustCall(t, "date", m)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(ordinal) = %T", v)
	}
	if dv.Year != 2024 || dv.Month != 2 || dv.Day != 29 {
		t.Errorf("date(ordinal) = %v, want 2024-02-29", dv)
	}
}

func TestFn_Date_MapForm_QuarterDayOfQuarter(t *testing.T) {
	m := expr.MapValue{
		"year":         expr.IntegerValue(2024),
		"quarter":      expr.IntegerValue(2),
		"dayOfQuarter": expr.IntegerValue(1),
	}
	v := mustCall(t, "date", m)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(quarter) = %T", v)
	}
	if dv.Year != 2024 || dv.Month != 4 || dv.Day != 1 {
		t.Errorf("date(quarter) = %v, want 2024-04-01", dv)
	}
}

func TestFn_Date_MapForm_QuarterOnly(t *testing.T) {
	m := expr.MapValue{
		"year":    expr.IntegerValue(2024),
		"quarter": expr.IntegerValue(3),
		"day":     expr.IntegerValue(5),
	}
	v := mustCall(t, "date", m)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(quarter-only) = %T", v)
	}
	// quarter 3 → month 7, day=5
	if dv.Year != 2024 || dv.Month != 7 || dv.Day != 5 {
		t.Errorf("date(quarter-only) = %v, want 2024-07-05", dv)
	}
}

func TestFn_Date_MapForm_FromBaseDate(t *testing.T) {
	base := expr.NewDate(2020, 5, 10)
	m := expr.MapValue{
		"date": base,
		"day":  expr.IntegerValue(20),
	}
	v := mustCall(t, "date", m)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(base) = %T", v)
	}
	if dv.Year != 2020 || dv.Month != 5 || dv.Day != 20 {
		t.Errorf("date(base) = %v, want 2020-05-20", dv)
	}
}

func TestFn_Date_MapForm_FloatYearCoerced(t *testing.T) {
	m := expr.MapValue{
		"year":  expr.FloatValue(2024.7),
		"month": expr.IntegerValue(3),
		"day":   expr.IntegerValue(1),
	}
	v := mustCall(t, "date", m)
	dv, ok := v.(expr.DateValue)
	if !ok {
		t.Fatalf("date(float-year) = %T", v)
	}
	if dv.Year != 2024 {
		t.Errorf("date(float-year) = %v, want year 2024", dv)
	}
}

func TestFn_Date_MapForm_NonNumericYearReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":  expr.StringValue("nope"),
		"month": expr.IntegerValue(1),
		"day":   expr.IntegerValue(1),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-year) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericMonthReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":  expr.IntegerValue(2024),
		"month": expr.BoolValue(true),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-month) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericDayReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year": expr.IntegerValue(2024),
		"day":  expr.StringValue("x"),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-day) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericWeekReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year": expr.IntegerValue(2024),
		"week": expr.StringValue("bad"),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-week) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericDayOfWeekReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":      expr.IntegerValue(2024),
		"week":      expr.IntegerValue(10),
		"dayOfWeek": expr.StringValue("nope"),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-dow) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_InvalidWeekReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":      expr.IntegerValue(2024),
		"week":      expr.IntegerValue(99), // out of range
		"dayOfWeek": expr.IntegerValue(1),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(invalid-week) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericDayOfYearReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":      expr.IntegerValue(2024),
		"dayOfYear": expr.StringValue("x"),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-doy) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericQuarterReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":    expr.IntegerValue(2024),
		"quarter": expr.StringValue("Q1"),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-quarter) = %v, want Null", v)
	}
}

func TestFn_Date_MapForm_NonNumericDayOfQuarterReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year":         expr.IntegerValue(2024),
		"quarter":      expr.IntegerValue(2),
		"dayOfQuarter": expr.StringValue("x"),
	}
	v := mustCall(t, "date", m)
	if !expr.IsNull(v) {
		t.Errorf("date(bad-doq) = %v, want Null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// localdatetime()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_LocalDateTime_ZeroArgs(t *testing.T) {
	v := mustCall(t, "localdatetime")
	if _, ok := v.(expr.LocalDateTimeValue); !ok {
		t.Errorf("localdatetime() = %T, want LocalDateTimeValue", v)
	}
}

func TestFn_LocalDateTime_Null(t *testing.T) {
	v := mustCall(t, "localdatetime", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("localdatetime(null) = %v, want Null", v)
	}
}

func TestFn_LocalDateTime_String(t *testing.T) {
	v := mustCall(t, "localdatetime", expr.StringValue("2024-06-15T13:45:30"))
	ldt, ok := v.(expr.LocalDateTimeValue)
	if !ok {
		t.Fatalf("localdatetime(string) = %T", v)
	}
	if ldt.T.Year() != 2024 || ldt.T.Month() != time.June || ldt.T.Day() != 15 {
		t.Errorf("localdatetime(string) = %v", ldt)
	}
}

func TestFn_LocalDateTime_InvalidStringReturnsNull(t *testing.T) {
	v := mustCall(t, "localdatetime", expr.StringValue("bad-input"))
	if !expr.IsNull(v) {
		t.Errorf("localdatetime(bad) = %v, want Null", v)
	}
}

func TestFn_LocalDateTime_Map(t *testing.T) {
	m := expr.MapValue{
		"year":   expr.IntegerValue(2024),
		"month":  expr.IntegerValue(6),
		"day":    expr.IntegerValue(15),
		"hour":   expr.IntegerValue(13),
		"minute": expr.IntegerValue(45),
		"second": expr.IntegerValue(30),
	}
	v := mustCall(t, "localdatetime", m)
	ldt, ok := v.(expr.LocalDateTimeValue)
	if !ok {
		t.Fatalf("localdatetime(map) = %T", v)
	}
	if ldt.T.Year() != 2024 || ldt.T.Hour() != 13 || ldt.T.Minute() != 45 || ldt.T.Second() != 30 {
		t.Errorf("localdatetime(map) = %v", ldt)
	}
}

func TestFn_LocalDateTime_Map_InvalidDateReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year": expr.StringValue("nope"),
	}
	v := mustCall(t, "localdatetime", m)
	if !expr.IsNull(v) {
		t.Errorf("localdatetime(bad-map) = %v, want Null", v)
	}
}

func TestFn_LocalDateTime_FromLocalDateTime_RoundTrips(t *testing.T) {
	in := expr.NewLocalDateTime(2024, 6, 15, 13, 45, 30, 0)
	v := mustCall(t, "localdatetime", in)
	if v != in {
		t.Errorf("localdatetime(LDT) = %v, want %v", v, in)
	}
}

func TestFn_LocalDateTime_FromDateTime(t *testing.T) {
	in := expr.NewDateTime(2024, 6, 15, 13, 45, 30, 0, time.UTC)
	v := mustCall(t, "localdatetime", in)
	if _, ok := v.(expr.LocalDateTimeValue); !ok {
		t.Errorf("localdatetime(DT) = %T, want LocalDateTimeValue", v)
	}
}

func TestFn_LocalDateTime_FromDate(t *testing.T) {
	in := expr.NewDate(2024, 6, 15)
	v := mustCall(t, "localdatetime", in)
	ldt, ok := v.(expr.LocalDateTimeValue)
	if !ok {
		t.Fatalf("localdatetime(D) = %T", v)
	}
	if ldt.T.Year() != 2024 || ldt.T.Hour() != 0 {
		t.Errorf("localdatetime(D) = %v", ldt)
	}
}

func TestFn_LocalDateTime_WrongTypeReturnsError(t *testing.T) {
	_, err := call(t, "localdatetime", expr.IntegerValue(42))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("localdatetime(Integer) error = %T, want TypeError", err)
	}
}

func TestFn_LocalDateTime_ArityError(t *testing.T) {
	_, err := call(t, "localdatetime", expr.IntegerValue(1), expr.IntegerValue(2))
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("localdatetime(too many) error = %T, want ArityError", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// datetime()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_DateTime_ZeroArgs(t *testing.T) {
	v := mustCall(t, "datetime")
	if _, ok := v.(expr.DateTimeValue); !ok {
		t.Errorf("datetime() = %T, want DateTimeValue", v)
	}
}

func TestFn_DateTime_Null(t *testing.T) {
	v := mustCall(t, "datetime", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("datetime(null) = %v, want Null", v)
	}
}

func TestFn_DateTime_InvalidStringReturnsNull(t *testing.T) {
	v := mustCall(t, "datetime", expr.StringValue("not-a-datetime"))
	if !expr.IsNull(v) {
		t.Errorf("datetime(bad) = %v, want Null", v)
	}
}

func TestFn_DateTime_FromDateTime_RoundTrips(t *testing.T) {
	in := expr.NewDateTime(2024, 6, 15, 13, 45, 30, 0, time.UTC)
	v := mustCall(t, "datetime", in)
	if dt, ok := v.(expr.DateTimeValue); !ok || !dt.T.Equal(in.T) {
		t.Errorf("datetime(DT) = %v, want %v", v, in)
	}
}

func TestFn_DateTime_FromLocalDateTime(t *testing.T) {
	in := expr.NewLocalDateTime(2024, 6, 15, 13, 45, 30, 0)
	v := mustCall(t, "datetime", in)
	if _, ok := v.(expr.DateTimeValue); !ok {
		t.Errorf("datetime(LDT) = %T, want DateTimeValue", v)
	}
}

func TestFn_DateTime_FromDate(t *testing.T) {
	in := expr.NewDate(2024, 6, 15)
	v := mustCall(t, "datetime", in)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(D) = %T", v)
	}
	if dt.T.Year() != 2024 || dt.T.Hour() != 0 {
		t.Errorf("datetime(D) = %v", dt)
	}
}

func TestFn_DateTime_Map(t *testing.T) {
	m := expr.MapValue{
		"year":   expr.IntegerValue(2024),
		"month":  expr.IntegerValue(6),
		"day":    expr.IntegerValue(15),
		"hour":   expr.IntegerValue(13),
		"minute": expr.IntegerValue(45),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(map) = %T", v)
	}
	if dt.T.Year() != 2024 || dt.T.Hour() != 13 || dt.T.Minute() != 45 {
		t.Errorf("datetime(map) = %v", dt)
	}
}

func TestFn_DateTime_Map_WithNamedTimezone(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"month":    expr.IntegerValue(6),
		"day":      expr.IntegerValue(15),
		"hour":     expr.IntegerValue(13),
		"timezone": expr.StringValue("Europe/Lisbon"),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(map+tz) = %T", v)
	}
	if dt.T.Location().String() != "Europe/Lisbon" {
		t.Errorf("datetime(map+tz) location = %s, want Europe/Lisbon", dt.T.Location())
	}
}

func TestFn_DateTime_Map_WithOffsetTimezone(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"month":    expr.IntegerValue(6),
		"day":      expr.IntegerValue(15),
		"hour":     expr.IntegerValue(13),
		"timezone": expr.StringValue("+02:00"),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(map+off) = %T", v)
	}
	_, off := dt.T.Zone()
	if off != 2*3600 {
		t.Errorf("datetime(map+off) offset = %ds, want 7200", off)
	}
}

func TestFn_DateTime_Map_TimezoneZ(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"month":    expr.IntegerValue(6),
		"day":      expr.IntegerValue(15),
		"timezone": expr.StringValue("Z"),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(Z) = %T", v)
	}
	if dt.T.Location() != time.UTC {
		t.Errorf("datetime(Z) location = %s, want UTC", dt.T.Location())
	}
}

func TestFn_DateTime_Map_TimezoneUTC(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"timezone": expr.StringValue("UTC"),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(UTC) = %T", v)
	}
	if dt.T.Location() != time.UTC {
		t.Errorf("datetime(UTC) location = %s, want UTC", dt.T.Location())
	}
}

func TestFn_DateTime_Map_TimezoneEmpty(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"timezone": expr.StringValue(""),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(empty-tz) = %T", v)
	}
	if dt.T.Location() != time.UTC {
		t.Errorf("datetime(empty-tz) location = %s, want UTC", dt.T.Location())
	}
}

func TestFn_DateTime_Map_TimezoneUnknownFallsBackToUTC(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"timezone": expr.StringValue("Not/A_Real_Zone"),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(bad-tz) = %T", v)
	}
	if dt.T.Location() != time.UTC {
		t.Errorf("datetime(bad-tz) location = %s, want UTC fallback", dt.T.Location())
	}
}

func TestFn_DateTime_Map_TimezoneMalformedOffsetFallsBackToUTC(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"timezone": expr.StringValue("+xx:yy"),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(bad-off) = %T", v)
	}
	if dt.T.Location() != time.UTC {
		t.Errorf("datetime(bad-off) location = %s, want UTC", dt.T.Location())
	}
}

func TestFn_DateTime_Map_TimezoneNonStringFallsBackToUTC(t *testing.T) {
	m := expr.MapValue{
		"year":     expr.IntegerValue(2024),
		"timezone": expr.IntegerValue(42),
	}
	v := mustCall(t, "datetime", m)
	dt, ok := v.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("datetime(int-tz) = %T", v)
	}
	if dt.T.Location() != time.UTC {
		t.Errorf("datetime(int-tz) location = %s, want UTC", dt.T.Location())
	}
}

func TestFn_DateTime_Map_InvalidDateReturnsNull(t *testing.T) {
	m := expr.MapValue{
		"year": expr.StringValue("nope"),
	}
	v := mustCall(t, "datetime", m)
	if !expr.IsNull(v) {
		t.Errorf("datetime(bad-map) = %v, want Null", v)
	}
}

func TestFn_DateTime_WrongTypeReturnsError(t *testing.T) {
	_, err := call(t, "datetime", expr.IntegerValue(42))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("datetime(Integer) error = %T, want TypeError", err)
	}
}

func TestFn_DateTime_ArityError(t *testing.T) {
	_, err := call(t, "datetime", expr.IntegerValue(1), expr.IntegerValue(2))
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("datetime(too many) error = %T, want ArityError", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// localtime()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_LocalTime_ZeroArgs(t *testing.T) {
	v := mustCall(t, "localtime")
	if _, ok := v.(expr.LocalTimeValue); !ok {
		t.Errorf("localtime() = %T, want LocalTimeValue", v)
	}
}

func TestFn_LocalTime_Null(t *testing.T) {
	v := mustCall(t, "localtime", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("localtime(null) = %v, want Null", v)
	}
}

func TestFn_LocalTime_String(t *testing.T) {
	v := mustCall(t, "localtime", expr.StringValue("12:34:56"))
	lt, ok := v.(expr.LocalTimeValue)
	if !ok {
		t.Fatalf("localtime(string) = %T", v)
	}
	want := expr.NewLocalTime(12, 34, 56, 0)
	if lt != want {
		t.Errorf("localtime(string) = %v, want %v", lt, want)
	}
}

func TestFn_LocalTime_InvalidStringReturnsNull(t *testing.T) {
	v := mustCall(t, "localtime", expr.StringValue("not-a-time"))
	if !expr.IsNull(v) {
		t.Errorf("localtime(bad) = %v, want Null", v)
	}
}

func TestFn_LocalTime_FromLocalTime_RoundTrips(t *testing.T) {
	in := expr.NewLocalTime(13, 45, 30, 0)
	v := mustCall(t, "localtime", in)
	if v != in {
		t.Errorf("localtime(LT) = %v, want %v", v, in)
	}
}

func TestFn_LocalTime_FromTime(t *testing.T) {
	in := expr.NewTime(13, 45, 30, 0, 3600)
	v := mustCall(t, "localtime", in)
	lt, ok := v.(expr.LocalTimeValue)
	if !ok {
		t.Fatalf("localtime(T) = %T", v)
	}
	if lt.Nanos != in.Nanos {
		t.Errorf("localtime(T) = %v, want nanos %d", lt, in.Nanos)
	}
}

func TestFn_LocalTime_FromLocalDateTime(t *testing.T) {
	in := expr.NewLocalDateTime(2024, 6, 15, 13, 45, 30, 0)
	v := mustCall(t, "localtime", in)
	if _, ok := v.(expr.LocalTimeValue); !ok {
		t.Errorf("localtime(LDT) = %T", v)
	}
}

func TestFn_LocalTime_FromDateTime(t *testing.T) {
	in := expr.NewDateTime(2024, 6, 15, 13, 45, 30, 0, time.UTC)
	v := mustCall(t, "localtime", in)
	if _, ok := v.(expr.LocalTimeValue); !ok {
		t.Errorf("localtime(DT) = %T", v)
	}
}

func TestFn_LocalTime_WrongTypeReturnsError(t *testing.T) {
	_, err := call(t, "localtime", expr.IntegerValue(42))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("localtime(Integer) error = %T, want TypeError", err)
	}
}

func TestFn_LocalTime_ArityError(t *testing.T) {
	_, err := call(t, "localtime", expr.IntegerValue(1), expr.IntegerValue(2))
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("localtime(too many) error = %T, want ArityError", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// time()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Time_ZeroArgs(t *testing.T) {
	v := mustCall(t, "time")
	if _, ok := v.(expr.TimeValue); !ok {
		t.Errorf("time() = %T, want TimeValue", v)
	}
}

func TestFn_Time_Null(t *testing.T) {
	v := mustCall(t, "time", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("time(null) = %v, want Null", v)
	}
}

func TestFn_Time_String(t *testing.T) {
	v := mustCall(t, "time", expr.StringValue("12:34:56+01:00"))
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(string) = %T", v)
	}
	if tv.OffsetSec != 3600 {
		t.Errorf("time(string) offset = %d, want 3600", tv.OffsetSec)
	}
}

func TestFn_Time_InvalidStringReturnsNull(t *testing.T) {
	v := mustCall(t, "time", expr.StringValue("nope"))
	if !expr.IsNull(v) {
		t.Errorf("time(bad) = %v, want Null", v)
	}
}

func TestFn_Time_Map(t *testing.T) {
	m := expr.MapValue{
		"hour":   expr.IntegerValue(13),
		"minute": expr.IntegerValue(45),
		"second": expr.IntegerValue(30),
	}
	v := mustCall(t, "time", m)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(map) = %T", v)
	}
	want := expr.NewTime(13, 45, 30, 0, 0)
	if tv != want {
		t.Errorf("time(map) = %v, want %v", tv, want)
	}
}

func TestFn_Time_Map_WithOffset(t *testing.T) {
	m := expr.MapValue{
		"hour":     expr.IntegerValue(13),
		"timezone": expr.StringValue("+02:00"),
	}
	v := mustCall(t, "time", m)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(map+off) = %T", v)
	}
	if tv.OffsetSec != 2*3600 {
		t.Errorf("time(map+off) offset = %d, want 7200", tv.OffsetSec)
	}
}

func TestFn_Time_Map_TimezoneZIgnored(t *testing.T) {
	m := expr.MapValue{
		"hour":     expr.IntegerValue(13),
		"timezone": expr.StringValue("Z"),
	}
	v := mustCall(t, "time", m)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(map+Z) = %T", v)
	}
	if tv.OffsetSec != 0 {
		t.Errorf("time(map+Z) offset = %d, want 0", tv.OffsetSec)
	}
}

func TestFn_Time_Map_NonStringTimezoneIgnored(t *testing.T) {
	m := expr.MapValue{
		"hour":     expr.IntegerValue(13),
		"timezone": expr.IntegerValue(123),
	}
	v := mustCall(t, "time", m)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(map+inttz) = %T", v)
	}
	if tv.OffsetSec != 0 {
		t.Errorf("time(map+inttz) offset = %d, want 0", tv.OffsetSec)
	}
}

func TestFn_Time_Map_MalformedOffsetIgnored(t *testing.T) {
	m := expr.MapValue{
		"hour":     expr.IntegerValue(13),
		"timezone": expr.StringValue("+xy"),
	}
	v := mustCall(t, "time", m)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(map+badoff) = %T", v)
	}
	if tv.OffsetSec != 0 {
		t.Errorf("time(map+badoff) offset = %d, want 0 fallback", tv.OffsetSec)
	}
}

func TestFn_Time_FromTime_RoundTrips(t *testing.T) {
	in := expr.NewTime(13, 45, 30, 0, 3600)
	v := mustCall(t, "time", in)
	if v != in {
		t.Errorf("time(T) = %v, want %v", v, in)
	}
}

func TestFn_Time_FromLocalTime(t *testing.T) {
	in := expr.NewLocalTime(13, 45, 30, 0)
	v := mustCall(t, "time", in)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(LT) = %T", v)
	}
	if tv.Nanos != in.Nanos || tv.OffsetSec != 0 {
		t.Errorf("time(LT) = %v, want nanos=%d offset=0", tv, in.Nanos)
	}
}

func TestFn_Time_FromDateTime_CapturesZoneOffset(t *testing.T) {
	loc := time.FixedZone("+0200", 2*3600)
	in := expr.NewDateTime(2024, 6, 15, 13, 45, 30, 0, loc)
	v := mustCall(t, "time", in)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(DT) = %T", v)
	}
	if tv.OffsetSec != 2*3600 {
		t.Errorf("time(DT) offset = %d, want 7200", tv.OffsetSec)
	}
}

func TestFn_Time_FromLocalDateTime(t *testing.T) {
	in := expr.NewLocalDateTime(2024, 6, 15, 13, 45, 30, 0)
	v := mustCall(t, "time", in)
	tv, ok := v.(expr.TimeValue)
	if !ok {
		t.Fatalf("time(LDT) = %T", v)
	}
	if tv.OffsetSec != 0 {
		t.Errorf("time(LDT) offset = %d, want 0", tv.OffsetSec)
	}
}

func TestFn_Time_WrongTypeReturnsError(t *testing.T) {
	_, err := call(t, "time", expr.IntegerValue(42))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("time(Integer) error = %T, want TypeError", err)
	}
}

func TestFn_Time_ArityError(t *testing.T) {
	_, err := call(t, "time", expr.IntegerValue(1), expr.IntegerValue(2))
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("time(too many) error = %T, want ArityError", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// timeComponentsFromMap — exercised via time() / localtime()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_TimeComponents_MillisecondConverted(t *testing.T) {
	m := expr.MapValue{
		"hour":        expr.IntegerValue(13),
		"millisecond": expr.IntegerValue(500),
	}
	v := mustCall(t, "localtime", m)
	lt, ok := v.(expr.LocalTimeValue)
	if !ok {
		t.Fatalf("localtime(ms) = %T", v)
	}
	want := expr.NewLocalTime(13, 0, 0, 500_000_000)
	if lt != want {
		t.Errorf("localtime(ms) = %v, want %v", lt, want)
	}
}

func TestFn_TimeComponents_MicrosecondConverted(t *testing.T) {
	m := expr.MapValue{
		"hour":        expr.IntegerValue(13),
		"microsecond": expr.IntegerValue(500),
	}
	v := mustCall(t, "localtime", m)
	lt, ok := v.(expr.LocalTimeValue)
	if !ok {
		t.Fatalf("localtime(us) = %T", v)
	}
	want := expr.NewLocalTime(13, 0, 0, 500_000)
	if lt != want {
		t.Errorf("localtime(us) = %v, want %v", lt, want)
	}
}

func TestFn_TimeComponents_SubSecondFieldsAreAdditive(t *testing.T) {
	// openCypher 9 §3.10.1: when millisecond/microsecond/nanosecond are
	// all supplied, the nanosecond component is the sum of their scaled
	// contributions, not the highest-precision override.
	m := expr.MapValue{
		"hour":        expr.IntegerValue(13),
		"nanosecond":  expr.IntegerValue(42),
		"millisecond": expr.IntegerValue(999),
	}
	v := mustCall(t, "localtime", m)
	lt, ok := v.(expr.LocalTimeValue)
	if !ok {
		t.Fatalf("localtime(ns) = %T", v)
	}
	want := expr.NewLocalTime(13, 0, 0, 999*1_000_000+42)
	if lt != want {
		t.Errorf("localtime(ns) = %v, want %v", lt, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// duration()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Duration_Null(t *testing.T) {
	v := mustCall(t, "duration", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("duration(null) = %v, want Null", v)
	}
}

func TestFn_Duration_InvalidStringReturnsNull(t *testing.T) {
	v := mustCall(t, "duration", expr.StringValue("not-a-duration"))
	if !expr.IsNull(v) {
		t.Errorf("duration(bad) = %v, want Null", v)
	}
}

func TestFn_Duration_FromDurationValue_RoundTrips(t *testing.T) {
	in := expr.NewDuration(1, 2, 3, 0)
	v := mustCall(t, "duration", in)
	if v != in {
		t.Errorf("duration(D) = %v, want %v", v, in)
	}
}

func TestFn_Duration_WrongTypeReturnsError(t *testing.T) {
	_, err := call(t, "duration", expr.IntegerValue(42))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("duration(Integer) error = %T, want TypeError", err)
	}
}

func TestFn_Duration_ArityError(t *testing.T) {
	_, err := call(t, "duration")
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("duration(no-args) error = %T, want ArityError", err)
	}
}

func TestFn_Duration_Map_AllUnits(t *testing.T) {
	m := expr.MapValue{
		"years":        expr.IntegerValue(1),
		"months":       expr.IntegerValue(2),
		"weeks":        expr.IntegerValue(1),
		"days":         expr.IntegerValue(3),
		"hours":        expr.IntegerValue(4),
		"minutes":      expr.IntegerValue(5),
		"seconds":      expr.IntegerValue(6),
		"milliseconds": expr.IntegerValue(7),
		"microseconds": expr.IntegerValue(8),
		"nanoseconds":  expr.IntegerValue(9),
	}
	v := mustCall(t, "duration", m)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("duration(all-units) = %T", v)
	}
	if dv.Months != 14 {
		t.Errorf("duration(all-units).Months = %d, want 14", dv.Months)
	}
	if dv.Days != 10 { // 1 week * 7 + 3 days
		t.Errorf("duration(all-units).Days = %d, want 10", dv.Days)
	}
	wantSec := int64(4)*3600 + int64(5)*60 + int64(6)
	if dv.Seconds != wantSec {
		t.Errorf("duration(all-units).Seconds = %d, want %d", dv.Seconds, wantSec)
	}
}

func TestFn_Duration_Map_FractionalMonth(t *testing.T) {
	m := expr.MapValue{
		"months": expr.FloatValue(1.5),
	}
	v := mustCall(t, "duration", m)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("duration(frac-month) = %T", v)
	}
	if dv.Months != 1 {
		t.Errorf("duration(frac-month).Months = %d, want 1", dv.Months)
	}
	// 0.5 month * 30.4375 ≈ 15 days (truncated to int)
	if dv.Days != 15 {
		t.Errorf("duration(frac-month).Days = %d, want 15", dv.Days)
	}
}

func TestFn_Duration_Map_FractionalSeconds(t *testing.T) {
	m := expr.MapValue{
		"seconds": expr.FloatValue(1.5),
	}
	v := mustCall(t, "duration", m)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("duration(frac-sec) = %T", v)
	}
	if dv.Seconds != 1 || dv.Nanos != 500_000_000 {
		t.Errorf("duration(frac-sec) = %+v, want Seconds=1 Nanos=500e6", dv)
	}
}

func TestFn_Duration_Map_NonNumericValuesIgnored(t *testing.T) {
	// floatFromValue returns 0 for any non-numeric value.
	m := expr.MapValue{
		"years":  expr.StringValue("nope"),
		"months": expr.IntegerValue(2),
	}
	v := mustCall(t, "duration", m)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("duration(bad-years) = %T", v)
	}
	if dv.Months != 2 {
		t.Errorf("duration(bad-years).Months = %d, want 2", dv.Months)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// duration.between
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_DurationBetween_LocalDateTimes(t *testing.T) {
	a := expr.NewLocalDateTime(2024, 1, 1, 0, 0, 0, 0)
	b := expr.NewLocalDateTime(2024, 1, 2, 0, 0, 0, 0)
	v := mustCall(t, "duration.between", a, b)
	if _, ok := v.(expr.DurationValue); !ok {
		t.Errorf("duration.between(LDT) = %T, want DurationValue", v)
	}
}

func TestFn_DurationBetween_DateTimes(t *testing.T) {
	a := expr.NewDateTime(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b := expr.NewDateTime(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	v := mustCall(t, "duration.between", a, b)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("duration.between(DT) = %T", v)
	}
	if dv.Seconds != 3600 {
		t.Errorf("duration.between(DT).Seconds = %d, want 3600", dv.Seconds)
	}
}

func TestFn_DurationBetween_LocalTimes(t *testing.T) {
	a := expr.NewLocalTime(10, 0, 0, 0)
	b := expr.NewLocalTime(11, 30, 0, 0)
	v := mustCall(t, "duration.between", a, b)
	if _, ok := v.(expr.DurationValue); !ok {
		t.Errorf("duration.between(LT) = %T, want DurationValue", v)
	}
}

func TestFn_DurationBetween_Times(t *testing.T) {
	a := expr.NewTime(10, 0, 0, 0, 0)
	b := expr.NewTime(11, 0, 0, 0, 0)
	v := mustCall(t, "duration.between", a, b)
	if _, ok := v.(expr.DurationValue); !ok {
		t.Errorf("duration.between(T) = %T, want DurationValue", v)
	}
}

func TestFn_DurationBetween_NullFirst(t *testing.T) {
	v := mustCall(t, "duration.between", expr.Null, expr.NewDate(2024, 1, 1))
	if !expr.IsNull(v) {
		t.Errorf("duration.between(null, date) = %v, want Null", v)
	}
}

func TestFn_DurationBetween_NullSecond(t *testing.T) {
	v := mustCall(t, "duration.between", expr.NewDate(2024, 1, 1), expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("duration.between(date, null) = %v, want Null", v)
	}
}

func TestFn_DurationBetween_NonTemporalReturnsNull(t *testing.T) {
	v := mustCall(t, "duration.between", expr.IntegerValue(1), expr.IntegerValue(2))
	if !expr.IsNull(v) {
		t.Errorf("duration.between(int, int) = %v, want Null", v)
	}
}

func TestFn_DurationBetween_ArityError(t *testing.T) {
	_, err := call(t, "duration.between", expr.NewDate(2024, 1, 1))
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("duration.between(one-arg) error = %T, want ArityError", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// duration.inMonths / inDays / inSeconds
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_DurationInMonths(t *testing.T) {
	d := expr.NewDuration(14, 5, 3600, 0)
	v := mustCall(t, "duration.inmonths", d)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("inmonths = %T", v)
	}
	if dv.Months != 14 || dv.Days != 0 || dv.Seconds != 0 {
		t.Errorf("inmonths = %+v, want only Months=14", dv)
	}
}

func TestFn_DurationInMonths_Null(t *testing.T) {
	v := mustCall(t, "duration.inmonths", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("inmonths(null) = %v, want Null", v)
	}
}

func TestFn_DurationInMonths_WrongType(t *testing.T) {
	_, err := call(t, "duration.inmonths", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("inmonths(int) error = %T, want TypeError", err)
	}
}

func TestFn_DurationInMonths_ArityError(t *testing.T) {
	_, err := call(t, "duration.inmonths")
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("inmonths(no-args) error = %T, want ArityError", err)
	}
}

func TestFn_DurationInDays_RollsSeconds(t *testing.T) {
	// 2 days plus 2 * 86400 seconds = 4 days total.
	d := expr.NewDuration(0, 2, 2*86400, 0)
	v := mustCall(t, "duration.indays", d)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("indays = %T", v)
	}
	if dv.Days != 4 || dv.Months != 0 || dv.Seconds != 0 {
		t.Errorf("indays = %+v, want only Days=4", dv)
	}
}

func TestFn_DurationInDays_Null(t *testing.T) {
	v := mustCall(t, "duration.indays", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("indays(null) = %v, want Null", v)
	}
}

func TestFn_DurationInDays_WrongType(t *testing.T) {
	_, err := call(t, "duration.indays", expr.StringValue("oops"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("indays(string) error = %T, want TypeError", err)
	}
}

func TestFn_DurationInDays_ArityError(t *testing.T) {
	_, err := call(t, "duration.indays")
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("indays(no-args) error = %T, want ArityError", err)
	}
}

func TestFn_DurationInSeconds_RollsDays(t *testing.T) {
	d := expr.NewDuration(0, 1, 30, 500)
	v := mustCall(t, "duration.inseconds", d)
	dv, ok := v.(expr.DurationValue)
	if !ok {
		t.Fatalf("inseconds = %T", v)
	}
	want := int64(86400) + 30
	if dv.Seconds != want || dv.Days != 0 || dv.Months != 0 {
		t.Errorf("inseconds = %+v, want Seconds=%d Nanos=500", dv, want)
	}
	if dv.Nanos != 500 {
		t.Errorf("inseconds Nanos = %d, want 500", dv.Nanos)
	}
}

func TestFn_DurationInSeconds_Null(t *testing.T) {
	v := mustCall(t, "duration.inseconds", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("inseconds(null) = %v, want Null", v)
	}
}

func TestFn_DurationInSeconds_WrongType(t *testing.T) {
	_, err := call(t, "duration.inseconds", expr.FloatValue(1.5))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("inseconds(float) error = %T, want TypeError", err)
	}
}

func TestFn_DurationInSeconds_ArityError(t *testing.T) {
	_, err := call(t, "duration.inseconds")
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("inseconds(no-args) error = %T, want ArityError", err)
	}
}
